package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── splitFlagsAndArgs: flags may follow the positional alias ────────────────

// The exact bug live verification caught: `add work --no-login` (flag AFTER the
// alias, as the documented signature implies) must still yield alias="work" and
// the flag, not treat --no-login as a second operand.
func TestSplitFlagsAndArgs_FlagAfterPositional(t *testing.T) {
	flags, pos := splitFlagsAndArgs([]string{"work", "--no-login"}, nil)
	if len(pos) != 1 || pos[0] != "work" {
		t.Fatalf("positional = %v, want [work]", pos)
	}
	if len(flags) != 1 || flags[0] != "--no-login" {
		t.Fatalf("flags = %v, want [--no-login]", flags)
	}
}

// A value flag consumes the following token even when it trails the positional:
// `add work --dir /abs/d` keeps /abs/d with --dir, not as a second operand.
func TestSplitFlagsAndArgs_ValueFlagConsumesNext(t *testing.T) {
	flags, pos := splitFlagsAndArgs([]string{"work", "--dir", "/abs/d"}, map[string]bool{"dir": true})
	if len(pos) != 1 || pos[0] != "work" {
		t.Fatalf("positional = %v, want [work]", pos)
	}
	if len(flags) != 2 || flags[0] != "--dir" || flags[1] != "/abs/d" {
		t.Fatalf("flags = %v, want [--dir /abs/d]", flags)
	}
}

// ── formatAccountsList: the "did it work" surface ──────────────────────────

// A logged-in account shows its email and plan — and NOTHING token-like: the
// formatter is handed only the safe fields, and this pins that it emits only
// those.
func TestFormatAccountsList_LoggedInShowsEmailAndPlan(t *testing.T) {
	out := formatAccountsList([]accountRow{{
		alias: "work", configDir: "/abs/work",
		probeOK: true, loggedIn: true, email: "you@work.com", plan: "pro",
		hooksOK: true, bindings: []string{"/abs/repo"},
	}})
	for _, want := range []string{"work", "/abs/work", "you@work.com", "(pro)", "installed", "/abs/repo"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// A not-logged-in account points the user at the exact fix command, and flags
// missing hooks (a loop there would record nothing).
func TestFormatAccountsList_LoggedOutAndMissingHooksAreActionable(t *testing.T) {
	out := formatAccountsList([]accountRow{{
		alias: "personal", configDir: "/abs/personal",
		probeOK: true, loggedIn: false, hooksOK: false,
	}})
	if !strings.Contains(out, "fleetops accounts login personal") {
		t.Errorf("logged-out row lacks the login hint:\n%s", out)
	}
	if !strings.Contains(out, "fleetops hooks install") {
		t.Errorf("missing-hooks row lacks the install hint:\n%s", out)
	}
	if !strings.Contains(out, "(none)") {
		t.Errorf("no-binding row should say (none):\n%s", out)
	}
}

// When the probe could not run, the row says so honestly instead of claiming
// logged-out.
func TestFormatAccountsList_ProbeUnavailableIsUnknownNotLoggedOut(t *testing.T) {
	out := formatAccountsList([]accountRow{{alias: "work", configDir: "/abs/work", probeOK: false}})
	if !strings.Contains(out, "unknown") {
		t.Errorf("unavailable probe should read 'unknown':\n%s", out)
	}
	if strings.Contains(out, "not logged in") {
		t.Errorf("unavailable probe must not claim 'not logged in':\n%s", out)
	}
}

// Token-safety guard: even if a token-shaped value somehow reached the display
// struct, the formatter has no field to print it — assert the rendered text of
// a fully-populated row contains none of the fields we refuse to surface.
func TestFormatAccountsList_NeverPrintsSecrets(t *testing.T) {
	out := formatAccountsList([]accountRow{{
		alias: "work", configDir: "/abs/work",
		probeOK: true, loggedIn: true, email: "you@work.com", plan: "pro", hooksOK: true,
	}})
	for _, forbidden := range []string{"token", "Bearer", "sk-", "orgId", "secret"} {
		if strings.Contains(strings.ToLower(out), strings.ToLower(forbidden)) {
			t.Errorf("formatter leaked a forbidden field %q:\n%s", forbidden, out)
		}
	}
}

// ── resolveAddDir ──────────────────────────────────────────────────────────

// An explicit --dir must be absolute — a relative one would spawn an
// unauthenticated session.
func TestResolveAddDir_RejectsRelativeDirFlag(t *testing.T) {
	if _, err := resolveAddDir("work", "relative/dir"); err == nil {
		t.Fatal("relative --dir accepted; want an error")
	}
}

// The default config dir is ~/.fleetops/accounts/<alias>.
func TestResolveAddDir_DefaultsUnderFleetopsAccounts(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	got, err := resolveAddDir("work", "")
	if err != nil {
		t.Fatalf("resolveAddDir: %v", err)
	}
	want := filepath.Join(home, ".fleetops", "accounts", "work")
	if got != want {
		t.Fatalf("default dir = %q, want %q", got, want)
	}
}

// ── launchLogin: the CLI triggers the login for the RIGHT config dir ────────

// The whole point of `accounts login` is testable without a real claude: the
// loginRunner seam records which config dir the flow would authenticate.
func TestLaunchLogin_RunsForResolvedConfigDir(t *testing.T) {
	var gotDir string
	restore := loginRunner
	loginRunner = func(configDir string) error { gotDir = configDir; return nil }
	defer func() { loginRunner = restore }()

	launchLogin("work", "/abs/.claude-work")
	if gotDir != "/abs/.claude-work" {
		t.Fatalf("loginRunner got %q, want /abs/.claude-work", gotDir)
	}
}
