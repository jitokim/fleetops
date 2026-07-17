package control

import (
	"testing"

	"github.com/jitokim/fleetops/internal/sessions"
)

func TestResolveActuationTarget_NoRegistryEntry_FallsThroughToCwdChain(t *testing.T) {
	// no sessions dir / no entry at all — must fall to Tier 1b (Resolve +
	// LocateClaude). Whether a real backend happens to be installed on the
	// machine running this test varies (this dev box has one), so only
	// found=false is deterministic here: no real claude surface lives at
	// this bogus projectDir either way.
	_, _, _, found := ResolveActuationTarget(t.TempDir(), "sess-1", "-x-nonexistent-project-dir")
	if found {
		t.Error("expected found=false — no real claude surface at this projectDir")
	}
}

func TestResolveActuationTarget_EntryPresentButEmptyTTY_SkipsTierOneA(t *testing.T) {
	dir := t.TempDir()
	if err := sessions.WriteSession(dir, "sess-1", sessions.SessionEntry{PID: 12345, TTY: ""}); err != nil {
		t.Fatalf("WriteSession: %v", err)
	}

	// An empty TTY must skip Tier 1a and fall to the cwd chain — proven by
	// pidTTYFn never being called (Tier 1a's own gate: TTY != "" is checked
	// BEFORE the tty-binding probe).
	origPidTTY := pidTTYFn
	defer func() { pidTTYFn = origPidTTY }()
	pidTTYCalled := false
	pidTTYFn = func(pid int) string { pidTTYCalled = true; return "ttys099" }

	ResolveActuationTarget(dir, "sess-1", "-x-nonexistent-project-dir")

	if pidTTYCalled {
		t.Error("expected pidTTYFn NOT to be called — an empty TTY must skip Tier 1a entirely")
	}
}

func TestResolveActuationTarget_EntryPresentTTYSetButPIDDead_SkipsTierOneA(t *testing.T) {
	dir := t.TempDir()
	if err := sessions.WriteSession(dir, "sess-1", sessions.SessionEntry{PID: 99999999, TTY: "ttys012"}); err != nil {
		t.Fatalf("WriteSession: %v", err)
	}

	origPidTTY := pidTTYFn
	defer func() { pidTTYFn = origPidTTY }()
	pidTTYFn = func(pid int) string { return "" } // simulate a dead pid (no controlling tty at all) — never trust a stale registry record

	_, _, _, found := ResolveActuationTarget(dir, "sess-1", "-x-nonexistent-project-dir")
	if found {
		t.Error("expected found=false — a dead pid must skip the tty path, and no real surface matches the bogus cwd fallback either")
	}
}

// TestResolveActuationTarget_PIDAliveButTTYMismatch_SkipsTierOneA is the P1-1
// hazard this binding check exists to close: a SIGKILL'd session leaks its
// registry entry; the OS recycles BOTH the tty (now controlled by a
// DIFFERENT, unrelated live claude pane) and the pid (reused by some other
// process). A pid-existence-only check would have passed here and misrouted
// an action onto the wrong session — the binding check must catch this by
// comparing the pid's CURRENT tty against the registry's recorded one, not
// just asking "does this pid exist."
func TestResolveActuationTarget_PIDAliveButTTYMismatch_SkipsTierOneA(t *testing.T) {
	dir := t.TempDir()
	if err := sessions.WriteSession(dir, "sess-1", sessions.SessionEntry{PID: 42, TTY: "ttys012"}); err != nil {
		t.Fatalf("WriteSession: %v", err)
	}

	origPidTTY := pidTTYFn
	defer func() { pidTTYFn = origPidTTY }()
	// pid 42 is alive (recycled to an unrelated process), but it now
	// controls a DIFFERENT tty than the one the registry recorded.
	pidTTYFn = func(pid int) string { return "ttys099" }

	_, _, _, found := ResolveActuationTarget(dir, "sess-1", "-x-nonexistent-project-dir")
	if found {
		t.Error("expected found=false — the pid is alive but bound to a different tty, must not take Tier 1a")
	}
}

func TestResolveActuationTarget_PIDBindingConfirmed_TriesTierOneA(t *testing.T) {
	dir := t.TempDir()
	if err := sessions.WriteSession(dir, "sess-1", sessions.SessionEntry{PID: 42, TTY: "ttys012"}); err != nil {
		t.Fatalf("WriteSession: %v", err)
	}

	origPidTTY := pidTTYFn
	defer func() { pidTTYFn = origPidTTY }()
	var gotPID int
	called := false
	pidTTYFn = func(pid int) string {
		gotPID, called = pid, true
		return "ttys012" // matches the registry entry — binding confirmed
	}

	// No real tmux pane exists at this tty in the test environment, so this
	// still falls through to the cwd chain — but the binding probe must
	// have been consulted with the right pid first.
	ResolveActuationTarget(dir, "sess-1", "-x-nonexistent-project-dir")

	if !called {
		t.Fatal("expected pidTTYFn to be called")
	}
	if gotPID != 42 {
		t.Errorf("pidTTYFn called with pid %d, want 42 (the registry entry's PID)", gotPID)
	}
}

func TestResolveActuationTarget_TTYNormalizedBeforeComparison(t *testing.T) {
	// the registry stores the bare form ("ttys012"); pidTTYFn's real
	// implementation normalizes ps's "/dev/ttys012" the same way — this
	// proves the comparison itself normalizes both sides symmetrically
	// rather than requiring an exact string match.
	dir := t.TempDir()
	if err := sessions.WriteSession(dir, "sess-1", sessions.SessionEntry{PID: 42, TTY: "/dev/ttys012"}); err != nil {
		t.Fatalf("WriteSession: %v", err)
	}

	origPidTTY := pidTTYFn
	defer func() { pidTTYFn = origPidTTY }()
	called := false
	pidTTYFn = func(pid int) string {
		called = true
		return normalizeTTY("/dev/ttys012") // real impl always returns the normalized form
	}

	ResolveActuationTarget(dir, "sess-1", "-x-nonexistent-project-dir")

	if !called {
		t.Fatal("expected pidTTYFn to be called — registry TTY was non-empty")
	}
}

func TestResolveActuationTarget_InvalidSessionID_TreatedAsNoEntry(t *testing.T) {
	// sessions.ReadSession errors on a malformed/invalid session id — must
	// degrade to the cwd chain, not panic or propagate the error.
	_, _, _, found := ResolveActuationTarget(t.TempDir(), "../escape", "-x-nonexistent-project-dir")
	if found {
		t.Error("expected found=false")
	}
}

// --- tierOneA: dispatch to whichever resolved backend implements TTYLocator ---
//
// Bug 2 (Option A): ResolveActuationTarget used to hardcode tmuxController{}
// for Tier 1a regardless of which backend Resolve() actually picked. These
// tests pin the generalized "type-assert the RESOLVED backend" behavior
// against fake Controllers, independent of which real multiplexer binaries
// (if any) happen to be installed on the test machine.

// fakeControllerNoTTY implements control.Controller only — no TTYLocator —
// the shape orcaController has today (confirmed live: orca's terminal
// list/show JSON carries no tty/pid field at all).
type fakeControllerNoTTY struct{}

func (fakeControllerNoTTY) Name() string                       { return "fake-no-tty" }
func (fakeControllerNoTTY) Available() bool                    { return true }
func (fakeControllerNoTTY) Locate(string) (Target, bool)       { return Target{}, false }
func (fakeControllerNoTTY) LocateClaude(string) (Target, bool) { return Target{}, false }
func (fakeControllerNoTTY) Resume(Target, string) error        { return nil }
func (fakeControllerNoTTY) Focus(Target) error                 { return nil }
func (fakeControllerNoTTY) Approve(Target) error               { return nil }
func (fakeControllerNoTTY) Spawn(string, string) error         { return nil }
func (fakeControllerNoTTY) Interrupt(Target) error             { return nil }

// fakeControllerWithTTY implements Controller AND TTYLocator — the shape
// cmuxController has (tty is already exposed directly in `tree --json`).
type fakeControllerWithTTY struct {
	fakeControllerNoTTY
	locateByTTYCalled string // last tty passed in
	target            Target
	ok                bool
}

func (f *fakeControllerWithTTY) Name() string { return "fake-with-tty" }
func (f *fakeControllerWithTTY) LocateByTTY(tty string) (Target, bool) {
	f.locateByTTYCalled = tty
	return f.target, f.ok
}

func TestTierOneA_ResolvedImplementsTTYLocator_Dispatches(t *testing.T) {
	want := Target{Backend: "fake-with-tty", ID: "surface:1"}
	f := &fakeControllerWithTTY{target: want, ok: true}

	got, ok := tierOneA(f, "ttys012")

	if !ok {
		t.Fatal("expected ok=true")
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if f.locateByTTYCalled != "ttys012" {
		t.Errorf("LocateByTTY called with %q, want ttys012", f.locateByTTYCalled)
	}
}

func TestTierOneA_ResolvedDoesNotImplementTTYLocator_ReturnsNotFound(t *testing.T) {
	// orca's real shape today: Controller without TTYLocator. Must degrade
	// gracefully (never panic on the failed type assertion) so
	// ResolveActuationTarget falls through to Tier 1b.
	_, ok := tierOneA(fakeControllerNoTTY{}, "ttys012")
	if ok {
		t.Error("expected ok=false — resolved backend doesn't implement TTYLocator")
	}
}

func TestRedriveArgv(t *testing.T) {
	got := redriveArgv("sess-abc123", "do the thing")
	want := []string{"claude", "--resume", "sess-abc123", "-p", "do the thing", "--output-format", "json"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
