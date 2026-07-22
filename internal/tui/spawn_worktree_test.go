package tui

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jitokim/fleetops/internal/control"
	"github.com/jitokim/fleetops/internal/registry"
	"github.com/jitokim/fleetops/internal/worktree"
)

// ── fixtures ─────────────────────────────────────────────────────────────

// isolateFleetopsHome points ~/.fleetops at a temp dir for the duration of a
// test, so spawnCmd's registry.WritePending call cannot write into the
// developer's REAL ~/.fleetops/pending — a stray pending record there would be
// picked up by their next real fleetops run and bound to an unrelated session.
func isolateFleetopsHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

// stubWorktreeCreate replaces the worktree seam with a fixed outcome and
// records the directory it was asked to branch from.
func stubWorktreeCreate(t *testing.T, result worktree.Result, err error) *string {
	t.Helper()
	original := worktreeCreateFn
	t.Cleanup(func() { worktreeCreateFn = original })
	var gotRepoDir string
	worktreeCreateFn = func(repoDir string) (worktree.Result, error) {
		gotRepoDir = repoDir
		return result, err
	}
	return &gotRepoDir
}

// stubSpawner pins spawnCmd's resolver seam. It takes a control.Spawner (not a
// Controller) because spawn now resolves over the wider seam — which is what
// lets a host terminal with no multiplexer spawn at all.
func stubSpawner(t *testing.T, spawner control.Spawner, ok bool) {
	t.Helper()
	original := resolveSpawnerFn
	t.Cleanup(func() { resolveSpawnerFn = original })
	resolveSpawnerFn = func() (control.Spawner, bool) { return spawner, ok }
}

// fakeWorktreeSpawnerController is a controller that ALSO implements
// control.WorktreeSpawner — i.e. orca. Embedding rather than extending
// fakeController itself keeps a plain *fakeController correctly NOT
// implementing the interface (same shape as fakeTerminalOpenerController).
type fakeWorktreeSpawnerController struct {
	*fakeController
	spawnWorktreeCalled bool
	worktreePath        string
	spawnWorktreeErr    error
}

func (f *fakeWorktreeSpawnerController) SpawnWorktree(repoCwd, name, prompt string) (string, error) {
	f.spawnWorktreeCalled = true
	return f.worktreePath, f.spawnWorktreeErr
}

func testBindSpec() registry.BindSpec {
	return registry.BindSpec{Name: "n", Goal: "마케팅 전략 수립", MaxCycles: 5}
}

// runSpawn dispatches spawnCmd and returns its message as a spawnResultMsg.
func runSpawn(t *testing.T, cwd string, useWorktree bool) spawnResultMsg {
	t.Helper()
	// "" configDir = the default account (no explicit override) — the
	// zero-config path these worktree tests exercise (multi-account Phase C
	// added the parameter; account-specific spawn is covered separately).
	msg := spawnCmd(cwd, testBindSpec(), useWorktree, "")()
	result, ok := msg.(spawnResultMsg)
	if !ok {
		t.Fatalf("spawnCmd returned %T, want spawnResultMsg", msg)
	}
	return result
}

func okResult(path string) worktree.Result {
	return worktree.Result{Path: path, Branch: "wt-20260719-011612", Base: "origin/main"}
}

// ── success ──────────────────────────────────────────────────────────────

// The whole point of stage 1: a backend that does NOT implement
// control.WorktreeSpawner (tmux, iTerm2 — everything but orca) still gets a
// real isolated worktree, and the loop starts INSIDE it.
func TestSpawnCmd_Worktree_NonOrcaBackend_SpawnsIntoTheNewWorktree(t *testing.T) {
	isolateFleetopsHome(t)
	ctrl := &fakeController{name: "tmux"}
	stubSpawner(t, ctrl, true)
	wtPath := filepath.Join(t.TempDir(), "repo-wt-20260719-011612")
	gotRepoDir := stubWorktreeCreate(t, okResult(wtPath), nil)

	result := runSpawn(t, "/repo", true)

	if !result.ok {
		t.Fatalf("spawn failed: %s", result.text)
	}
	if *gotRepoDir != "/repo" {
		t.Fatalf("worktree branched from %q, want /repo", *gotRepoDir)
	}
	if !ctrl.spawnCalled {
		t.Fatal("Controller.Spawn was never called")
	}
	if ctrl.spawnCwd != wtPath {
		t.Fatalf("spawned in %q, want the new worktree %q", ctrl.spawnCwd, wtPath)
	}
}

// The explicit base is this feature's whole guarantee, so the human has to be
// able to SEE which base the branch was cut from.
func TestSpawnCmd_Worktree_StatusNamesBranchAndBase(t *testing.T) {
	isolateFleetopsHome(t)
	stubSpawner(t, &fakeController{name: "tmux"}, true)
	stubWorktreeCreate(t, okResult(filepath.Join(t.TempDir(), "repo-wt-20260719-011612")), nil)

	result := runSpawn(t, "/repo", true)

	for _, want := range []string{"wt-20260719-011612", "origin/main"} {
		if !strings.Contains(result.text, want) {
			t.Fatalf("status %q does not mention %q", result.text, want)
		}
	}
}

// The pending record must be keyed by the WORKTREE path, not the repo — that
// is where the new session's transcript cwd will be, and BindPending matches
// on it. Keyed by the repo, the loop would never bind.
func TestSpawnCmd_Worktree_PendingRecordIsKeyedByWorktreePath(t *testing.T) {
	home := isolateFleetopsHome(t)
	stubSpawner(t, &fakeController{name: "tmux"}, true)
	wtPath := filepath.Join(t.TempDir(), "repo-wt-20260719-011612")
	stubWorktreeCreate(t, okResult(wtPath), nil)

	if result := runSpawn(t, "/repo", true); !result.ok {
		t.Fatalf("spawn failed: %s", result.text)
	}

	pending := readAllPending(t, filepath.Join(home, ".fleetops", "pending"))
	if len(pending) != 1 {
		t.Fatalf("wrote %d pending records, want 1", len(pending))
	}
	if !strings.Contains(pending[0], wtPath) {
		t.Fatalf("pending record %q is not keyed by the worktree path %q", pending[0], wtPath)
	}
}

func readAllPending(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read pending: %v", err)
		}
		out = append(out, string(data))
	}
	return out
}

// orca keeps its own one-shot `worktree create --agent` route — "keep, not
// remove". fleetops's git path must not quietly take it over.
func TestSpawnCmd_Worktree_OrcaStillUsesItsOwnSpawnWorktree(t *testing.T) {
	isolateFleetopsHome(t)
	orca := &fakeWorktreeSpawnerController{
		fakeController: &fakeController{name: "orca"},
		worktreePath:   "/repo-orca-wt",
	}
	stubSpawner(t, orca, true)
	gotRepoDir := stubWorktreeCreate(t, okResult("/should-not-be-used"), nil)

	if result := runSpawn(t, "/repo", true); !result.ok {
		t.Fatalf("spawn failed: %s", result.text)
	}

	if !orca.spawnWorktreeCalled {
		t.Fatal("orca's SpawnWorktree was not used — its own worktree route was taken over")
	}
	if *gotRepoDir != "" {
		t.Fatalf("fleetops's git worktree path ran for orca (branched from %q)", *gotRepoDir)
	}
}

// The plain (non-worktree) spawn is untouched: cwd is used directly and no
// worktree is created.
func TestSpawnCmd_NoWorktree_UsesCwdDirectlyAndCreatesNoWorktree(t *testing.T) {
	isolateFleetopsHome(t)
	ctrl := &fakeController{name: "tmux"}
	stubSpawner(t, ctrl, true)
	gotRepoDir := stubWorktreeCreate(t, okResult("/should-not-be-used"), nil)

	if result := runSpawn(t, "/repo", false); !result.ok {
		t.Fatalf("spawn failed: %s", result.text)
	}

	if ctrl.spawnCwd != "/repo" {
		t.Fatalf("spawned in %q, want /repo", ctrl.spawnCwd)
	}
	if *gotRepoDir != "" {
		t.Fatalf("a worktree was created for a non-worktree spawn (branched from %q)", *gotRepoDir)
	}
}

// ── failure ──────────────────────────────────────────────────────────────

// A non-repo target must fail with a message the human can act on, and must
// NOT start a loop anywhere.
func TestSpawnCmd_Worktree_NotARepo_FailsAndNeverSpawns(t *testing.T) {
	isolateFleetopsHome(t)
	ctrl := &fakeController{name: "tmux"}
	stubSpawner(t, ctrl, true)
	stubWorktreeCreate(t, worktree.Result{}, worktree.ErrNotARepo)

	result := runSpawn(t, "/not-a-repo", true)

	if result.ok {
		t.Fatalf("spawn reported success despite worktree creation failing: %s", result.text)
	}
	if ctrl.spawnCalled {
		t.Fatal("a loop was spawned even though the worktree was never created")
	}
	if !strings.Contains(result.text, "not inside a git repository") {
		t.Fatalf("status %q does not explain why it failed", result.text)
	}
}

func TestSpawnCmd_Worktree_NoOriginRemote_Fails(t *testing.T) {
	isolateFleetopsHome(t)
	ctrl := &fakeController{name: "tmux"}
	stubSpawner(t, ctrl, true)
	stubWorktreeCreate(t, worktree.Result{}, worktree.ErrNoRemote)

	result := runSpawn(t, "/repo", true)

	if result.ok {
		t.Fatalf("spawn reported success with no origin remote: %s", result.text)
	}
	if ctrl.spawnCalled {
		t.Fatal("a loop was spawned even though the worktree was never created")
	}
}

func TestSpawnCmd_Worktree_PathCollision_Fails(t *testing.T) {
	isolateFleetopsHome(t)
	ctrl := &fakeController{name: "tmux"}
	stubSpawner(t, ctrl, true)
	stubWorktreeCreate(t, worktree.Result{}, worktree.ErrPathExists)

	if result := runSpawn(t, "/repo", true); result.ok {
		t.Fatalf("spawn reported success on a colliding worktree path: %s", result.text)
	}
	if ctrl.spawnCalled {
		t.Fatal("a loop was spawned into a colliding path")
	}
}

// The failure that must NEVER become a silent success: worktree creation
// fails, so fleetops falls back to spawning in the repo the human was
// explicitly trying to keep clean.
func TestSpawnCmd_Worktree_FailureNeverFallsBackToTheRepoDir(t *testing.T) {
	isolateFleetopsHome(t)
	ctrl := &fakeController{name: "tmux"}
	stubSpawner(t, ctrl, true)
	stubWorktreeCreate(t, worktree.Result{}, worktree.ErrNoRemote)

	runSpawn(t, "/repo", true)

	if ctrl.spawnCwd == "/repo" {
		t.Fatal("worktree spawn silently degraded to a plain spawn in the repo directory")
	}
}

// The checkout exists but nothing runs in it — the human owns an orphan
// directory and must be told where it is.
func TestSpawnCmd_Worktree_SpawnFailsAfterCreate_NamesTheOrphanCheckout(t *testing.T) {
	isolateFleetopsHome(t)
	wtPath := filepath.Join(t.TempDir(), "repo-wt-20260719-011612")
	ctrl := &fakeController{name: "tmux", spawnErr: errors.New("tmux: no server running")}
	stubSpawner(t, ctrl, true)
	stubWorktreeCreate(t, okResult(wtPath), nil)

	result := runSpawn(t, "/repo", true)

	if result.ok {
		t.Fatalf("spawn reported success though Controller.Spawn failed: %s", result.text)
	}
	if !strings.Contains(result.text, wtPath) {
		t.Fatalf("status %q does not name the orphaned checkout %q", result.text, wtPath)
	}
	if !strings.Contains(result.text, "no server running") {
		t.Fatalf("status %q does not carry the backend's own error", result.text)
	}
}

func TestSpawnCmd_Worktree_NoBackend_FailsBeforeCreatingAnything(t *testing.T) {
	isolateFleetopsHome(t)
	stubSpawner(t, nil, false)
	gotRepoDir := stubWorktreeCreate(t, okResult("/x"), nil)

	result := runSpawn(t, "/repo", true)

	if result.ok {
		t.Fatalf("spawn reported success with no backend: %s", result.text)
	}
	if *gotRepoDir != "" {
		t.Fatal("a worktree was created even though no backend could spawn into it")
	}
}

// ── iTerm2-only machine (no multiplexer) ─────────────────────────────────

// hostOnlySpawner implements ONLY control.Spawner — no Locate, no Focus, no
// pane addressing. It stands in for the iTerm2 host spawner, and the point of
// using it here is structural: if spawnCmd ever required a full Controller
// again, these tests stop compiling.
type hostOnlySpawner struct {
	spawnCalled bool
	spawnCwd    string
	spawnErr    error
}

func (hostOnlySpawner) Name() string    { return "iterm2" }
func (hostOnlySpawner) Available() bool { return true }
func (h *hostOnlySpawner) Spawn(cwd, prompt string) error {
	h.spawnCalled, h.spawnCwd = true, cwd
	return h.spawnErr
}

// The headline of stage 3: pressing "n" on a machine with no multiplexer now
// starts a loop instead of refusing.
func TestSpawnCmd_HostOnlySpawner_SpawnsWithNoMultiplexer(t *testing.T) {
	isolateFleetopsHome(t)
	host := &hostOnlySpawner{}
	stubSpawner(t, host, true)

	result := runSpawn(t, "/repo", false)

	if !result.ok {
		t.Fatalf("spawn failed on an iTerm2-only machine: %s", result.text)
	}
	if !host.spawnCalled || host.spawnCwd != "/repo" {
		t.Fatalf("host spawner was not used correctly (called=%v cwd=%q)", host.spawnCalled, host.spawnCwd)
	}
	if !strings.Contains(result.text, "iterm2") {
		t.Fatalf("status %q does not name the backend that spawned it", result.text)
	}
}

// Worktree isolation must work on the host spawner too — a Spawner that is not
// a WorktreeSpawner takes fleetops's own git path.
func TestSpawnCmd_HostOnlySpawner_StillGetsAGitWorktree(t *testing.T) {
	isolateFleetopsHome(t)
	host := &hostOnlySpawner{}
	stubSpawner(t, host, true)
	wtPath := filepath.Join(t.TempDir(), "repo-wt-20260719-011612")
	stubWorktreeCreate(t, okResult(wtPath), nil)

	result := runSpawn(t, "/repo", true)

	if !result.ok {
		t.Fatalf("worktree spawn failed on an iTerm2-only machine: %s", result.text)
	}
	if host.spawnCwd != wtPath {
		t.Fatalf("spawned in %q, want the new worktree %q", host.spawnCwd, wtPath)
	}
}

// With nothing at all available the message must mention iTerm2 too, or an
// iTerm2 user reads "no orca/tmux/cmux" and concludes fleetops cannot help
// them when in fact their host is supported.
func TestSpawnCmd_NoSpawnerAtAll_MessageMentionsITerm2(t *testing.T) {
	isolateFleetopsHome(t)
	stubSpawner(t, nil, false)

	result := runSpawn(t, "/repo", false)

	if result.ok {
		t.Fatal("spawn reported success with no spawner available")
	}
	if !strings.Contains(result.text, "iTerm2") {
		t.Fatalf("status %q does not mention iTerm2 among the supported hosts", result.text)
	}
}

// The stale-base caveat has to reach the human — it is appended to a SUCCESS
// line, so it is the only thing standing between them and a silently stale
// branch.
func TestSpawnCmd_Worktree_StaleBaseIsSurfacedOnSuccess(t *testing.T) {
	isolateFleetopsHome(t)
	stubSpawner(t, &fakeController{name: "tmux"}, true)
	stale := okResult(filepath.Join(t.TempDir(), "repo-wt-20260719-011612"))
	stale.StaleBase = true
	stale.StaleReason = "could not read from remote repository"
	stubWorktreeCreate(t, stale, nil)

	result := runSpawn(t, "/repo", true)

	if !result.ok {
		t.Fatalf("a stale base must not fail the spawn: %s", result.text)
	}
	if !strings.Contains(result.text, "STALE") {
		t.Fatalf("status %q does not warn that the base may be stale", result.text)
	}
	if !strings.Contains(result.text, "could not read from remote repository") {
		t.Fatalf("status %q does not carry the fetch failure reason", result.text)
	}
}

func TestSpawnCmd_Worktree_FreshBaseAddsNoWarning(t *testing.T) {
	isolateFleetopsHome(t)
	stubSpawner(t, &fakeController{name: "tmux"}, true)
	stubWorktreeCreate(t, okResult(filepath.Join(t.TempDir(), "repo-wt-20260719-011612")), nil)

	result := runSpawn(t, "/repo", true)

	if strings.Contains(result.text, "STALE") {
		t.Fatalf("status %q warns about staleness after a clean fetch", result.text)
	}
}

// ── eligibility ──────────────────────────────────────────────────────────

// [w] must now be offered on ANY backend that can spawn — gating it on
// control.WorktreeSpawner would hide the new capability from exactly the
// tmux/iTerm2 users it was added for.
func TestCheckWorktreeEligibility_OfferedOnNonWorktreeSpawnerBackend(t *testing.T) {
	stubSpawner(t, &fakeController{name: "tmux"}, true)

	msg := checkWorktreeEligibilityCmd()()
	if eligible, ok := msg.(worktreeEligibilityMsg); !ok || !bool(eligible) {
		t.Fatalf("eligibility = %v, want true for a plain (non-WorktreeSpawner) backend", msg)
	}
}

func TestCheckWorktreeEligibility_NotOfferedWithNoBackend(t *testing.T) {
	stubSpawner(t, nil, false)

	msg := checkWorktreeEligibilityCmd()()
	if eligible, ok := msg.(worktreeEligibilityMsg); !ok || bool(eligible) {
		t.Fatalf("eligibility = %v, want false when no backend resolves", msg)
	}
}
