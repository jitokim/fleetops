package control

import (
	"testing"

	"github.com/jitokim/fleetops/internal/sessions"
)

// fakeTitlerCtl is a fakeResolveCtl that ALSO implements TabTitler — the cmux
// shape (a backend that can rename its tabs). Records the last SetTabTitle call
// so a resolver test can confirm the returned titler is really this backend.
type fakeTitlerCtl struct {
	*fakeResolveCtl
	gotTarget Target
	gotTitle  string
}

func (f *fakeTitlerCtl) SetTabTitle(t Target, title string) error {
	f.gotTarget = t
	f.gotTitle = title
	return nil
}

// TestResolveTabTitler_SingleCmuxMatch_ReturnsTitler: exactly one available
// backend locates the loop AND implements TabTitler → resolve it.
func TestResolveTabTitler_SingleCmuxMatch_ReturnsTitler(t *testing.T) {
	titlerBackend := &fakeTitlerCtl{fakeResolveCtl: &fakeResolveCtl{t: t, name: "cmux", available: true, locateClaudeOK: true}}
	withBackends(t, titlerBackend)

	titler, target, ok := ResolveTabTitler(t.TempDir(), "sess-1", "-x-proj")

	if !ok {
		t.Fatal("expected ok=true — one cmux backend locates the loop and can title tabs")
	}
	if target.Backend != "cmux" {
		t.Errorf("target.Backend = %q, want cmux", target.Backend)
	}
	// The returned titler must be the resolved backend — drive it and confirm.
	if err := titler.SetTabTitle(target, "◆GATE x"); err != nil {
		t.Fatalf("SetTabTitle: %v", err)
	}
	if titlerBackend.gotTitle != "◆GATE x" || titlerBackend.gotTarget != target {
		t.Errorf("titler did not forward to the resolved backend: title=%q target=%+v", titlerBackend.gotTitle, titlerBackend.gotTarget)
	}
}

// TestResolveTabTitler_AmbiguousCwd_Refuses: two distinct backends both locate a
// claude surface for the same projectDir → fail closed (never rename a guessed
// tab), exactly like typed actuation's cross-backend ambiguity refusal.
func TestResolveTabTitler_AmbiguousCwd_Refuses(t *testing.T) {
	a := &fakeTitlerCtl{fakeResolveCtl: &fakeResolveCtl{t: t, name: "cmux", available: true, locateClaudeOK: true}}
	b := &fakeTitlerCtl{fakeResolveCtl: &fakeResolveCtl{t: t, name: "cmux2", available: true, locateClaudeOK: true}}
	withBackends(t, a, b)

	_, _, ok := ResolveTabTitler(t.TempDir(), "sess-1", "-x-proj")
	if ok {
		t.Error("expected ok=false — two backends match the same cwd (ambiguous, must refuse)")
	}
}

// TestResolveTabTitler_ResolvedBackendNotATitler_NoOp: the loop resolves to a
// backend that does NOT implement TabTitler (tmux/orca shape) → no-op (ok=false),
// so the caller skips rather than crashing on the failed type assertion.
func TestResolveTabTitler_ResolvedBackendNotATitler_NoOp(t *testing.T) {
	plain := &fakeResolveCtl{t: t, name: "tmux", available: true, locateClaudeOK: true}
	withBackends(t, plain)

	_, _, ok := ResolveTabTitler(t.TempDir(), "sess-1", "-x-proj")
	if ok {
		t.Error("expected ok=false — resolved backend does not implement TabTitler")
	}
}

// fakeTitlerTTYCtl implements TTYLocator (Tier 1a) AND TabTitler — the cmux
// shape (a per-terminal tty is reachable and its tab can be renamed).
type fakeTitlerTTYCtl struct {
	*fakeResolveTTYCtl
}

func (fakeTitlerTTYCtl) SetTabTitle(Target, string) error { return nil }

// TestResolveTabTitler_TierOneA_TTYMatch_ReturnsTitler pins the tty (Tier 1a)
// path specifically: a validated registry binding (entry tty == live pid's tty)
// resolves via LocateByTTY, then narrows to TabTitler — the high-confidence
// mapping the pivot calls out (tty-exact ⇒ no cwd ambiguity at all).
func TestResolveTabTitler_TierOneA_TTYMatch_ReturnsTitler(t *testing.T) {
	dir := t.TempDir()
	if err := sessions.WriteSession(dir, "sess-1", sessions.SessionEntry{PID: 42, TTY: "ttys012"}); err != nil {
		t.Fatalf("WriteSession: %v", err)
	}
	backend := fakeTitlerTTYCtl{&fakeResolveTTYCtl{
		fakeResolveCtl: &fakeResolveCtl{t: t, name: "cmux", available: true},
		locateByTTYOK:  true,
	}}
	withBackends(t, backend)

	origPidTTY := pidTTYFn
	t.Cleanup(func() { pidTTYFn = origPidTTY })
	pidTTYFn = func(int) string { return "ttys012" } // binding confirmed

	titler, target, ok := ResolveTabTitler(dir, "sess-1", "-x-proj")
	if !ok {
		t.Fatal("expected ok=true via Tier 1a tty match")
	}
	if target.Backend != "cmux" {
		t.Errorf("target.Backend = %q, want cmux", target.Backend)
	}
	if titler == nil {
		t.Error("expected a non-nil titler from the tty-resolved backend")
	}
}

// TestResolveTabTitler_NoMatch_Refuses: no backend locates the loop → not found.
func TestResolveTabTitler_NoMatch_Refuses(t *testing.T) {
	miss := &fakeTitlerCtl{fakeResolveCtl: &fakeResolveCtl{t: t, name: "cmux", available: true, locateClaudeOK: false}}
	withBackends(t, miss)

	_, _, ok := ResolveTabTitler(t.TempDir(), "sess-1", "-x-proj")
	if ok {
		t.Error("expected ok=false — no backend can locate the loop's surface")
	}
}
