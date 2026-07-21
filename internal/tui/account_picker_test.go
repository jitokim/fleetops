package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jitokim/fleetops/internal/accounts"
	"github.com/jitokim/fleetops/internal/accountstatus"
)

// driveToWhere runs a fresh "n" wizard through its six free-text steps and
// stops on wizardWhere, the step just before the account decision.
func driveToWhere(t *testing.T, m Model) Model {
	t.Helper()
	m, _ = updateModel(t, m, runeKey('n'))
	m, _ = typeAndEnter(t, m, "ship the thing") // goal
	m, _ = typeAndEnter(t, m, "")               // name
	m, _ = typeAndEnter(t, m, "")               // done-when
	m, _ = typeAndEnter(t, m, "")               // rubric
	m, _ = typeAndEnter(t, m, "")               // challenger
	m, _ = typeAndEnter(t, m, "")               // max_iteration
	if m.spawnStep != wizardWhere {
		t.Fatalf("precondition: spawnStep = %v, want wizardWhere", m.spawnStep)
	}
	return m
}

// pinAccounts overrides the three account seams for one test and restores them.
func pinAccounts(t *testing.T, cfg accounts.Config, mainRepo func(string) (string, bool), probe func(context.Context, string) (accountstatus.Status, bool)) {
	t.Helper()
	oload, ogit, oprobe := loadAccountsFn, gitMainRepoDirFn, accountStatusProbeFn
	t.Cleanup(func() { loadAccountsFn, gitMainRepoDirFn, accountStatusProbeFn = oload, ogit, oprobe })
	loadAccountsFn = func() (accounts.Config, error) { return cfg, nil }
	gitMainRepoDirFn = mainRepo
	accountStatusProbeFn = probe
}

// runCmd executes a tea.Cmd and feeds its message back through Update — the
// off-loop resolveAccountCmd → accountDecisionMsg round-trip.
func runCmd(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a non-nil tea.Cmd")
	}
	return updateModelResult(t, m, cmd())
}

func updateModelResult(t *testing.T, m Model, msg tea.Msg) Model {
	t.Helper()
	mm, _ := updateModel(t, m, msg)
	return mm
}

// ── the zero-config guarantee: no aliases → no account step, identical spawn ──

// The single most important test: a machine with NO aliases configured must
// spawn exactly as before — wizardWhere's enter submits straight through, never
// entering wizardAccount, with spawnConfigDir "" (no override).
func TestProceedFromWhere_ZeroConfig_NoAccountStep(t *testing.T) {
	// TestMain's default loadAccountsFn already returns the empty config; be
	// explicit so the intent is local and immune to a future default change.
	pinAccounts(t, accounts.Config{}, func(string) (string, bool) { return "", false },
		func(context.Context, string) (accountstatus.Status, bool) {
			t.Error("the login probe must NEVER run for a zero-config machine")
			return accountstatus.Status{}, false
		})

	m := driveToWhere(t, New())
	m, cmd := updateModel(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	if m.spawnStep == wizardAccount {
		t.Fatal("zero-config machine entered the account step — it must not exist for a user with no aliases")
	}
	if m.mode != modeNormal {
		t.Errorf("mode = %v, want modeNormal (the spawn should have submitted)", m.mode)
	}
	if m.spawnConfigDir != "" {
		t.Errorf("spawnConfigDir = %q, want \"\" (no account override for zero-config)", m.spawnConfigDir)
	}
	if cmd == nil {
		t.Error("expected the spawn tea.Cmd — the wizard should have submitted directly")
	}
}

// A malformed accounts.json must NOT wedge the wizard: it degrades to the
// default account (submit, no step) with a one-line warning.
func TestProceedFromWhere_MalformedConfig_SubmitsDefaultWithWarning(t *testing.T) {
	oload := loadAccountsFn
	t.Cleanup(func() { loadAccountsFn = oload })
	loadAccountsFn = func() (accounts.Config, error) {
		return accounts.Config{}, errContext("accounts: parsing …: bad json")
	}

	m := driveToWhere(t, New())
	m, cmd := updateModel(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	if m.spawnStep == wizardAccount {
		t.Fatal("a malformed config entered the account step — it must degrade to the default account instead")
	}
	if m.spawnConfigDir != "" {
		t.Errorf("spawnConfigDir = %q, want \"\" on a malformed config", m.spawnConfigDir)
	}
	if !strings.Contains(m.status, "accounts.json invalid") {
		t.Errorf("status = %q, want an 'accounts.json invalid' warning", m.status)
	}
	if cmd == nil {
		t.Error("expected the spawn to still submit despite the malformed config")
	}
}

type errString string

func (e errString) Error() string { return string(e) }
func errContext(s string) error   { return errString(s) }

// ── unbound + aliases: the picker ───────────────────────────────────────────

func twoAliasConfig() accounts.Config {
	return accounts.Config{Aliases: map[string]string{
		"company":  "/abs/.claude-work",
		"personal": "/abs/.claude-personal",
	}}
}

// An UNBOUND spawn dir with aliases configured must enter the picker, probe
// each alias's login status ONCE, and offer a genuine choice.
func TestProceedFromWhere_Unbound_EntersPicker_ProbesEachAliasOnce(t *testing.T) {
	probeCount := map[string]int{}
	pinAccounts(t, twoAliasConfig(),
		func(string) (string, bool) { return "", false }, // unbound
		func(_ context.Context, dir string) (accountstatus.Status, bool) {
			probeCount[dir]++
			if dir == "/abs/.claude-work" {
				return accountstatus.Status{LoggedIn: true, Email: "jito@company.com", Plan: "team"}, true
			}
			return accountstatus.Status{LoggedIn: false}, true
		})

	m := driveToWhere(t, New())
	m, cmd := updateModel(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.spawnStep != wizardAccount {
		t.Fatalf("spawnStep = %v, want wizardAccount", m.spawnStep)
	}
	if !m.accountResolving {
		t.Error("expected accountResolving=true while the probe is in flight")
	}
	m = runCmd(t, m, cmd)

	if m.accountResolving {
		t.Error("accountResolving should be cleared once the decision arrives")
	}
	if m.accountFixed {
		t.Error("an unbound dir must NOT be fixed — it is a real choice")
	}
	if len(m.accountAliasNames) != 2 || m.accountAliasNames[0] != "company" || m.accountAliasNames[1] != "personal" {
		t.Fatalf("accountAliasNames = %v, want sorted [company personal]", m.accountAliasNames)
	}
	for dir, n := range probeCount {
		if n != 1 {
			t.Errorf("probe for %s ran %d times, want exactly 1 (cached within the wizard)", dir, n)
		}
	}
}

// Pressing the digit for an alias selects it: spawnConfigDir is pinned to THAT
// alias's config dir and the spawn submits.
func TestPickerKey_DigitSelectsAlias_PinsConfigDir(t *testing.T) {
	pinAccounts(t, twoAliasConfig(), func(string) (string, bool) { return "", false },
		func(context.Context, string) (accountstatus.Status, bool) { return accountstatus.Status{}, false })

	m := driveToWhere(t, New())
	m, cmd := updateModel(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	m = runCmd(t, m, cmd)

	// [1] == company (sorted first).
	m, cmd = updateModel(t, m, runeKey('1'))

	if m.spawnConfigDir != "/abs/.claude-work" {
		t.Errorf("spawnConfigDir = %q, want company's /abs/.claude-work", m.spawnConfigDir)
	}
	if m.mode != modeNormal {
		t.Errorf("mode = %v, want modeNormal (selection submits)", m.mode)
	}
	if cmd == nil {
		t.Error("expected the spawn tea.Cmd after selecting an alias")
	}
}

// [enter] in the picker takes the DEFAULT account — no override, no config dir.
func TestPickerKey_EnterSelectsDefault_NoOverride(t *testing.T) {
	pinAccounts(t, twoAliasConfig(), func(string) (string, bool) { return "", false },
		func(context.Context, string) (accountstatus.Status, bool) { return accountstatus.Status{}, false })

	m := driveToWhere(t, New())
	m, cmd := updateModel(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	m = runCmd(t, m, cmd)

	m, cmd = updateModel(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // default

	if m.spawnConfigDir != "" {
		t.Errorf("spawnConfigDir = %q, want \"\" for the default choice", m.spawnConfigDir)
	}
	if cmd == nil {
		t.Error("expected the spawn tea.Cmd after choosing default")
	}
}

// An out-of-range digit or a stray key must be ignored — never submit under a
// nonexistent alias.
func TestPickerKey_OutOfRangeDigit_Ignored(t *testing.T) {
	pinAccounts(t, twoAliasConfig(), func(string) (string, bool) { return "", false },
		func(context.Context, string) (accountstatus.Status, bool) { return accountstatus.Status{}, false })

	m := driveToWhere(t, New())
	m, cmd := updateModel(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	m = runCmd(t, m, cmd)

	m, cmd = updateModel(t, m, runeKey('9')) // only 2 aliases

	if m.mode != modePrompting || m.spawnStep != wizardAccount {
		t.Error("an out-of-range digit must keep the picker open, not submit")
	}
	if cmd != nil {
		t.Error("an out-of-range digit must dispatch no command")
	}
}

// ── fixed (bound) account ───────────────────────────────────────────────────

func boundConfig() accounts.Config {
	return accounts.Config{
		Aliases:  map[string]string{"company": "/abs/.claude-work"},
		Bindings: []accounts.Binding{{Path: "/abs/work", Alias: "company"}},
	}
}

// A cwd BOUND to an alias yields a fixed account: shown, not changeable, and
// [enter] submits under exactly the bound config dir.
func TestProceedFromWhere_BoundCwd_FixedAccount_EnterSubmitsBoundDir(t *testing.T) {
	pinAccounts(t, boundConfig(),
		func(string) (string, bool) { return "/abs/work", true }, // cwd resolves to the bound repo
		func(context.Context, string) (accountstatus.Status, bool) {
			return accountstatus.Status{LoggedIn: true, Email: "jito@company.com"}, true
		})

	m := driveToWhere(t, New())
	m, cmd := updateModel(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	m = runCmd(t, m, cmd)

	if !m.accountFixed {
		t.Fatal("a bound cwd must produce a FIXED account")
	}
	if m.accountFixedAlias != "company" || m.accountFixedDir != "/abs/.claude-work" {
		t.Errorf("fixed = (%q, %q), want (company, /abs/.claude-work)", m.accountFixedAlias, m.accountFixedDir)
	}
	// A digit must NOT change a fixed account.
	before := m.spawnConfigDir
	m, _ = updateModel(t, m, runeKey('1'))
	if m.spawnConfigDir != before || m.mode == modeNormal {
		t.Error("a fixed account must ignore alias digits — it cannot be changed")
	}
	// [enter] submits under the bound dir.
	m, cmd = updateModel(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.spawnConfigDir != "/abs/.claude-work" {
		t.Errorf("spawnConfigDir = %q, want the bound /abs/.claude-work", m.spawnConfigDir)
	}
	if cmd == nil {
		t.Error("expected the spawn tea.Cmd after confirming the fixed account")
	}
}

// A fixed account that is NOT logged in must warn prominently in the label.
func TestAccountStepLabel_FixedNotLoggedIn_WarnsProminently(t *testing.T) {
	m := New()
	m.spawnStep = wizardAccount
	m.accountFixed = true
	m.accountFixedAlias = "company"
	m.accountFixedDir = "/abs/.claude-work"
	m.accountStatuses = map[string]accountStatusResult{
		"/abs/.claude-work": {ok: true, st: accountstatus.Status{LoggedIn: false}},
	}

	label := m.accountStepLabel()
	if !strings.Contains(label, "NOT LOGGED IN") {
		t.Errorf("label = %q, want a prominent NOT LOGGED IN warning for a fixed unauthenticated account", label)
	}
}

// ── D1: the status tag rendering ────────────────────────────────────────────

func TestAccountStatusTag(t *testing.T) {
	m := New()
	m.accountStatuses = map[string]accountStatusResult{
		"/in":      {ok: true, st: accountstatus.Status{LoggedIn: true, Email: "jito@company.com"}},
		"/out":     {ok: true, st: accountstatus.Status{LoggedIn: false}},
		"/unknown": {ok: false},
	}
	cases := map[string]string{
		"/in":      "✓ jito@company.com",
		"/out":     "⚠ NOT LOGGED IN",
		"/unknown": "(? login unknown)",
		"/missing": "(? login unknown)", // never probed
	}
	for dir, want := range cases {
		if got := m.accountStatusTag(dir); got != want {
			t.Errorf("accountStatusTag(%q) = %q, want %q", dir, got, want)
		}
	}
}

// ── aliasForDigit ───────────────────────────────────────────────────────────

func TestAliasForDigit(t *testing.T) {
	m := New()
	m.accountAliasNames = []string{"company", "personal"}
	if name, ok := m.aliasForDigit("1"); !ok || name != "company" {
		t.Errorf(`aliasForDigit("1") = (%q, %v), want (company, true)`, name, ok)
	}
	if name, ok := m.aliasForDigit("2"); !ok || name != "personal" {
		t.Errorf(`aliasForDigit("2") = (%q, %v), want (personal, true)`, name, ok)
	}
	if _, ok := m.aliasForDigit("3"); ok {
		t.Error(`aliasForDigit("3") ok=true, want false (out of range)`)
	}
	if _, ok := m.aliasForDigit("0"); ok {
		t.Error(`aliasForDigit("0") ok=true, want false (1-indexed)`)
	}
	if _, ok := m.aliasForDigit("x"); ok {
		t.Error(`aliasForDigit("x") ok=true, want false (non-digit)`)
	}
}

// ── D2: login launch ────────────────────────────────────────────────────────

// The picker's [l] then a digit launches `claude login` for THAT alias's config
// dir, through the TerminalOpener seam, with the correct env-prefixed command.
func TestPickerLoginLaunch_OpensTerminalWithLoginCommand(t *testing.T) {
	pinAccounts(t, twoAliasConfig(), func(string) (string, bool) { return "", false },
		func(context.Context, string) (accountstatus.Status, bool) {
			return accountstatus.Status{LoggedIn: false}, true
		})
	opener := &fakeTerminalOpenerController{fakeController: &fakeController{name: "orca"}}
	withFakeControlResolve(t, opener, true)

	m := driveToWhere(t, New())
	m, cmd := updateModel(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	m = runCmd(t, m, cmd)

	// [l] arms the login sub-prompt; it must NOT yet spawn or submit.
	m, cmd = updateModel(t, m, runeKey('l'))
	if !m.accountLoginPrompt {
		t.Fatal("[l] must arm the login sub-prompt")
	}
	if m.mode != modePrompting {
		t.Error("[l] must not submit the wizard")
	}
	// [2] == personal → launch its login.
	m, cmd = updateModel(t, m, runeKey('2'))
	if cmd == nil {
		t.Fatal("expected the login-launch tea.Cmd")
	}
	_ = runCmd(t, m, cmd)

	if !opener.openTerminalCalled {
		t.Fatal("expected OpenTerminal to be called for the login launch")
	}
	want := "env CLAUDE_CONFIG_DIR=/abs/.claude-personal claude login"
	if opener.openTerminalCommand != want {
		t.Errorf("login command = %q, want %q", opener.openTerminalCommand, want)
	}
	if m.mode == modeNormal {
		t.Error("launching a login must NOT close the picker — the human still has to pick after authenticating")
	}
}

// loginLaunchCmd on a backend with no TerminalOpener reports the manual command
// rather than failing silently, and never closes the picker (attachResultMsg).
func TestLoginLaunchCmd_NoOpener_ReportsManualCommand(t *testing.T) {
	withFakeControlResolve(t, &fakeController{name: "cmux"}, true) // no OpenTerminal

	msg := loginLaunchCmd("/repo", "/abs/.claude-work", "company")()

	res, ok := msg.(attachResultMsg)
	if !ok {
		t.Fatalf("loginLaunchCmd returned %T, want attachResultMsg", msg)
	}
	if res.ok {
		t.Error("expected ok=false when no terminal opener is available")
	}
	if !strings.Contains(res.text, "env CLAUDE_CONFIG_DIR=/abs/.claude-work claude login") {
		t.Errorf("text = %q, want it to include the manual login command", res.text)
	}
}
