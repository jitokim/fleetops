package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jitokim/fleetops/internal/accounts"
	"github.com/jitokim/fleetops/internal/accountstatus"
	"github.com/jitokim/fleetops/internal/control"
	"github.com/jitokim/fleetops/internal/registry"
	"github.com/jitokim/fleetops/internal/worktree"
)

// ── CRITICAL-2: engine-drive honors the account ──────────────────────────────

// buildBootstrapCmd is where the account env is set — the same pattern
// buildRedriveCmd uses. A bound cwd MUST carry CLAUDE_CONFIG_DIR so cycle 1 and
// every driven cycle run on the resolved account, not silently on the default.
func TestBuildBootstrapCmd_BoundCwd_SetsConfigDirEnv(t *testing.T) {
	cmd := buildBootstrapCmd(context.Background(), "/repo", "the contract", "/abs/.claude-work")

	if cmd.Dir != "/repo" {
		t.Errorf("cmd.Dir = %q, want /repo", cmd.Dir)
	}
	want := claudeConfigDirEnv + "=/abs/.claude-work"
	found := false
	for _, e := range cmd.Env {
		if e == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("cmd.Env is missing %q — a bound engine-drive spawn would run on the DEFAULT account", want)
	}
}

// The zero-config guarantee at the exec layer: an unbound cwd leaves cmd.Env
// nil (inherit the parent env), byte-identical to the pre-account bootstrap.
func TestBuildBootstrapCmd_UnboundCwd_LeavesEnvInherited(t *testing.T) {
	cmd := buildBootstrapCmd(context.Background(), "/repo", "the contract", "")
	if cmd.Env != nil {
		t.Fatalf("cmd.Env = %v, want nil (inherit parent env) for the default account", cmd.Env)
	}
}

// The wiring: bootstrapEngineCmd must RESOLVE the account for the spawn cwd
// (same resolution the manual path uses) and thread it into the claude call.
func TestBootstrapEngineCmd_BoundCwd_ThreadsResolvedConfigDir(t *testing.T) {
	loopsDir, historyDir := t.TempDir(), t.TempDir()
	origRegistryDir, origHistoryDir := registryDirFn, historyDirFn
	t.Cleanup(func() { registryDirFn, historyDirFn = origRegistryDir, origHistoryDir })
	registryDirFn = func() string { return loopsDir }
	historyDirFn = func() string { return historyDir }

	var gotConfigDir string
	withFakeBootstrapClaude(t, func(_ context.Context, _, _, configDir string) ([]byte, error) {
		gotConfigDir = configDir
		return []byte(`{"session_id":"sess-1"}`), nil
	})
	// pinAccounts AFTER withFakeBootstrapClaude: the latter resets loadAccountsFn
	// to empty for hermeticity, so the bound config must be pinned last to win.
	// A config that binds /repo → company, with the git seam a miss so cwd is
	// matched as-is (the same resolution defaultAccountConfigDir performs).
	pinAccounts(t, accounts.Config{
		Aliases:  map[string]string{"company": "/abs/.claude-work"},
		Bindings: []accounts.Binding{{Path: "/repo", Alias: "company"}},
	}, func(string) (string, bool) { return "", false }, nil)

	bootstrapEngineCmd("/repo/sub", registry.BindSpec{Goal: "g"})()

	if gotConfigDir != "/abs/.claude-work" {
		t.Fatalf("engine-drive threaded configDir=%q, want /abs/.claude-work — a bound cwd silently ran the default account", gotConfigDir)
	}
}

// An unbound engine-drive cwd threads "" — the default account, unchanged.
func TestBootstrapEngineCmd_UnboundCwd_ThreadsEmptyConfigDir(t *testing.T) {
	loopsDir, historyDir := t.TempDir(), t.TempDir()
	origRegistryDir, origHistoryDir := registryDirFn, historyDirFn
	t.Cleanup(func() { registryDirFn, historyDirFn = origRegistryDir, origHistoryDir })
	registryDirFn = func() string { return loopsDir }
	historyDirFn = func() string { return historyDir }

	gotConfigDir := "sentinel"
	withFakeBootstrapClaude(t, func(_ context.Context, _, _, configDir string) ([]byte, error) {
		gotConfigDir = configDir
		return []byte(`{"session_id":"sess-1"}`), nil
	})
	// pinAccounts last (see the bound test): /repo is NOT under /elsewhere, so
	// this cwd is unbound → the default account.
	pinAccounts(t, accounts.Config{
		Aliases:  map[string]string{"company": "/abs/.claude-work"},
		Bindings: []accounts.Binding{{Path: "/elsewhere", Alias: "company"}},
	}, func(string) (string, bool) { return "", false }, nil)

	bootstrapEngineCmd("/repo", registry.BindSpec{Goal: "g"})()

	if gotConfigDir != "" {
		t.Fatalf("unbound engine-drive threaded configDir=%q, want \"\" (default account)", gotConfigDir)
	}
}

// ── FINDING #1 (2nd review): the account lands on the DURABLE record ──────────

// The engine path is the exact wedge: bootstrapEngineCmd Binds directly, so it
// must write the resolved account onto the durable record — else cycle 2+
// (driveCmd), which redrives after the transient session entry is gone, runs on
// the default account.
func TestBootstrapEngineCmd_PersistsConfigDirOnRecord(t *testing.T) {
	loopsDir, historyDir := t.TempDir(), t.TempDir()
	origRegistryDir, origHistoryDir := registryDirFn, historyDirFn
	t.Cleanup(func() { registryDirFn, historyDirFn = origRegistryDir, origHistoryDir })
	registryDirFn = func() string { return loopsDir }
	historyDirFn = func() string { return historyDir }

	withFakeBootstrapClaude(t, func(context.Context, string, string, string) ([]byte, error) {
		return []byte(`{"session_id":"sess-1"}`), nil
	})
	pinAccounts(t, accounts.Config{
		Aliases:  map[string]string{"company": "/abs/.claude-work"},
		Bindings: []accounts.Binding{{Path: "/repo", Alias: "company"}},
	}, func(string) (string, bool) { return "", false }, nil)

	bootstrapEngineCmd("/repo", registry.BindSpec{Goal: "g"})()

	rec, ok := registry.Load(loopsDir, "sess-1")
	if !ok {
		t.Fatal("expected sess-1 to be bound")
	}
	if rec.ConfigDir != "/abs/.claude-work" {
		t.Fatalf("durable record ConfigDir = %q, want /abs/.claude-work — cycle 2+ would redrive on the DEFAULT account", rec.ConfigDir)
	}
}

// The manual spawn path threads the account onto the PENDING record (the next
// scan binds it onto the durable record) — without it the same wedge hits after
// the session entry is gone.
func TestSpawnCmd_PersistsConfigDirOnPendingRecord(t *testing.T) {
	home := isolateFleetopsHome(t)
	sp := &fakeAccountWorktreeSpawner{fakeController: &fakeController{name: "tmux"}}
	stubSpawner(t, sp, true)

	if result := spawnCmd("/repo", testBindSpec(), false, "/abs/.claude-work")().(spawnResultMsg); !result.ok {
		t.Fatalf("spawn failed: %s", result.text)
	}

	pendingDir := filepath.Join(home, ".fleetops", "pending")
	entries, err := os.ReadDir(pendingDir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("no pending record written: %v", err)
	}
	found := false
	for _, e := range entries {
		data, _ := os.ReadFile(filepath.Join(pendingDir, e.Name()))
		if strings.Contains(string(data), "/abs/.claude-work") {
			found = true
		}
	}
	if !found {
		t.Fatal("the pending record does not carry the account configDir — the durable record (and every later redrive) would default it")
	}
}

// ── HIGH: a stale accountDecisionMsg must be discarded ────────────────────────

// unboundResolvableProbe is a login probe that always reports logged-in — the
// resolveAccountCmd probes need something to return without a real claude.
func unboundResolvableProbe(context.Context, string) (accountstatus.Status, bool) {
	return accountstatus.Status{LoggedIn: true, Email: "x@y.z"}, true
}

// A decision from a CANCELLED/RE-ENTERED wizard (its generation no longer
// matches) must be DISCARDED, never applied — otherwise a slow probe from the
// first attempt could stamp the wrong account onto the second.
func TestAccountDecision_StaleGeneration_IsDiscarded(t *testing.T) {
	// Unbound + aliases → the picker path, so resolveAccountCmd returns a
	// (non-fixed) decision we can watch land.
	pinAccounts(t, twoAliasConfig(), func(string) (string, bool) { return "", false }, unboundResolvableProbe)

	// First wizard entry: reaches wizardAccount, generation 1, cmd1 in flight.
	m := driveToWhere(t, New())
	m, cmd1 := updateModel(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.spawnStep != wizardAccount {
		t.Fatalf("precondition: spawnStep = %v, want wizardAccount", m.spawnStep)
	}
	if m.spawnGeneration != 1 {
		t.Fatalf("precondition: spawnGeneration = %d, want 1", m.spawnGeneration)
	}

	// The human cancels and restarts the whole wizard. esc bumps the generation
	// (1→2, see TestAccountDecision_EscBumpsGeneration_DiscardsInFlight) and the
	// re-entry bumps again (2→3), so the fresh decision is gen 3.
	m, _ = updateModel(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	m = driveToWhere(t, m)
	m, cmd2 := updateModel(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.spawnGeneration != 3 {
		t.Fatalf("precondition: spawnGeneration = %d, want 3 after esc+re-entry", m.spawnGeneration)
	}

	// The STALE gen-1 decision lands late — it must be ignored, leaving the
	// account still resolving for gen 2.
	m = updateModelResult(t, m, cmd1())
	if !m.accountResolving {
		t.Fatal("a stale gen-1 accountDecisionMsg was APPLIED; the current (gen-2) resolution was overwritten")
	}

	// The fresh gen-2 decision is honored.
	m = updateModelResult(t, m, cmd2())
	if m.accountResolving {
		t.Fatal("the current gen-2 accountDecisionMsg was NOT applied")
	}
}

// NIT #6 (2nd review): esc at the account step must BUMP the generation, so an
// account decision STILL IN FLIGHT (slow git + probes) can't land after the
// cancel and mutate the picker fields — even when the wizard is NOT restarted.
// Without the bump the stale gen still matches (spawnStep stays wizardAccount)
// and applyAccountDecision would run post-cancel.
func TestAccountDecision_EscBumpsGeneration_DiscardsInFlight(t *testing.T) {
	pinAccounts(t, twoAliasConfig(), func(string) (string, bool) { return "", false }, unboundResolvableProbe)

	m := driveToWhere(t, New())
	m, cmd1 := updateModel(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.spawnStep != wizardAccount {
		t.Fatalf("precondition: spawnStep = %v, want wizardAccount", m.spawnStep)
	}
	genBefore := m.spawnGeneration

	// Cancel at the account step (no wizard restart). esc must still invalidate
	// the in-flight generation.
	m, _ = updateModel(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.spawnGeneration == genBefore {
		t.Fatal("esc did not bump spawnGeneration — an in-flight decision could still land post-cancel")
	}

	// The stale decision lands after esc: it must be discarded, picker fields
	// left untouched (still nil, never populated from the aliases).
	m = updateModelResult(t, m, cmd1())
	if m.accountFixed || m.accountConfigDirs != nil || m.accountAliasNames != nil {
		t.Fatal("an accountDecisionMsg landed after esc and mutated picker fields; the race guard is not airtight")
	}
}

// A decision that lands after the account step was already LEFT (e.g. the spawn
// submitted, mode is back to normal) is also discarded — matching step guard.
func TestAccountDecision_WrongStep_IsDiscarded(t *testing.T) {
	m := New()
	m.spawnStep = wizardGoal // anything but wizardAccount
	m.spawnGeneration = 7
	m.accountResolving = true

	m = updateModelResult(t, m, accountDecisionMsg{generation: 7, fixed: true, configDir: "/abs/.claude-work"})

	if !m.accountResolving || m.accountFixed {
		t.Fatal("an accountDecisionMsg was applied while NOT at wizardAccount; want discarded")
	}
}

// ── MEDIUM: orca worktree honors a non-default account ───────────────────────

// fakeAccountWorktreeSpawner is orca's shape for these tests: it implements
// BOTH control.WorktreeSpawner (its native --agent route) AND
// control.AccountSpawner (SpawnWithConfigDir, the account-honoring path).
type fakeAccountWorktreeSpawner struct {
	*fakeController
	spawnWorktreeCalled   bool
	worktreePath          string
	spawnWithConfigCalled bool
	gotConfigDir          string
	gotConfigCwd          string
}

func (f *fakeAccountWorktreeSpawner) SpawnWorktree(repoCwd, name, prompt string) (string, error) {
	f.spawnWorktreeCalled = true
	return f.worktreePath, nil
}

func (f *fakeAccountWorktreeSpawner) SpawnWithConfigDir(cwd, goal, configDir string) error {
	f.spawnWithConfigCalled = true
	f.gotConfigCwd, f.gotConfigDir = cwd, configDir
	return nil
}

// FINDING #3 (2nd review, CONFIRMED LIVE): orca + a non-default account + a
// worktree request cannot all coexist — orca's `terminal create --worktree
// path:<p>` only accepts a worktree orca itself registered, so handing it a
// fleetops-created checkout returns selector_not_found. The OLD fix routed this
// through the git-worktree path (creating a checkout, then SpawnWithConfigDir
// into it) — which strands an ORPHANED worktree orca rejects. The correct
// behavior: create NO fleetops worktree, spawn in the bound repo dir orca DOES
// know with the account preserved via the env prefix, and say so.
func TestSpawnCmd_OrcaWorktree_NonDefaultAccount_SpawnsInRepoNoWorktree(t *testing.T) {
	isolateFleetopsHome(t)
	orca := &fakeAccountWorktreeSpawner{
		fakeController: &fakeController{name: "orca"},
		worktreePath:   "/should-not-be-used",
	}
	stubSpawner(t, orca, true)
	// Stubbed but must NOT be called: creating a worktree here is exactly the
	// orphaned-checkout bug this fix removes.
	gotRepoDir := stubWorktreeCreate(t, worktree.Result{Path: "/repo-wt-should-not-exist", Branch: "b", Base: "origin/main"}, nil)

	msg := spawnCmd("/repo", testBindSpec(), true, "/abs/.claude-work")()
	result, ok := msg.(spawnResultMsg)
	if !ok || !result.ok {
		t.Fatalf("spawn failed: %+v", msg)
	}

	if orca.spawnWorktreeCalled {
		t.Fatal("orca's native SpawnWorktree ran for a non-default account — it silently drops CLAUDE_CONFIG_DIR")
	}
	if *gotRepoDir != "" {
		t.Fatalf("a fleetops worktree WAS created (branched from %q) — orca will reject it (selector_not_found), stranding an orphan", *gotRepoDir)
	}
	if !orca.spawnWithConfigCalled {
		t.Fatal("SpawnWithConfigDir was never called — the account was not honored")
	}
	if orca.gotConfigDir != "/abs/.claude-work" {
		t.Errorf("SpawnWithConfigDir configDir = %q, want /abs/.claude-work", orca.gotConfigDir)
	}
	if orca.gotConfigCwd != "/repo" {
		t.Errorf("SpawnWithConfigDir cwd = %q, want the bound repo dir /repo (NOT a worktree)", orca.gotConfigCwd)
	}
	if !strings.Contains(result.text, "no separate checkout") || !strings.Contains(result.text, "/abs/.claude-work") {
		t.Errorf("status %q must tell the human isolation was forfeited AND name the account", result.text)
	}
}

// The DEFAULT account (configDir == "") keeps orca's native worktree route,
// unchanged — the reroute is scoped strictly to the non-default case.
func TestSpawnCmd_OrcaWorktree_DefaultAccount_KeepsNativeRoute(t *testing.T) {
	isolateFleetopsHome(t)
	orca := &fakeAccountWorktreeSpawner{
		fakeController: &fakeController{name: "orca"},
		worktreePath:   "/repo-orca-wt",
	}
	stubSpawner(t, orca, true)
	gotRepoDir := stubWorktreeCreate(t, worktree.Result{Path: "/should-not-be-used"}, nil)

	if result := spawnCmd("/repo", testBindSpec(), true, "")().(spawnResultMsg); !result.ok {
		t.Fatalf("spawn failed: %s", result.text)
	}

	if !orca.spawnWorktreeCalled {
		t.Fatal("orca's native SpawnWorktree was bypassed for the default account — the reroute leaked outside the non-default case")
	}
	if *gotRepoDir != "" {
		t.Fatalf("the git worktree path ran for a default-account orca spawn (branched from %q)", *gotRepoDir)
	}
	if orca.spawnWithConfigCalled {
		t.Fatal("SpawnWithConfigDir ran for the default account; want the native orca route")
	}
}

var _ control.AccountSpawner = (*fakeAccountWorktreeSpawner)(nil)
var _ control.WorktreeSpawner = (*fakeAccountWorktreeSpawner)(nil)
