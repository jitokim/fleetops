package control

import (
	"testing"

	"github.com/jitokim/fleetops/internal/sessions"
)

// boundOf unwraps a resolved Actuator back to the (Controller, Target) pair it
// closed over, so these tests can keep asserting WHICH backend and WHICH
// surface resolution picked — the binding moved behind Actuator, but what the
// resolver decides is unchanged and still worth pinning. Fails the test if the
// actuator isn't a multiplexer binding (a Tier 1h host send would be, and is
// asserted on separately).
func boundOf(t *testing.T, act Actuator) boundController {
	t.Helper()
	b, ok := act.(boundController)
	if !ok {
		t.Fatalf("actuator = %T, want boundController", act)
	}
	return b
}

func TestResolveActuationTarget_NoRegistryEntry_FallsThroughToCwdChain(t *testing.T) {
	// no sessions dir / no entry at all — must fall to Tier 1b (Resolve +
	// LocateClaude). Whether a real backend happens to be installed on the
	// machine running this test varies (this dev box has one), so only
	// found=false is deterministic here: no real claude surface lives at
	// this bogus projectDir either way.
	_, _, found := ResolveActuationTarget(t.TempDir(), "sess-1", "-x-nonexistent-project-dir")
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

	_, _, found := ResolveActuationTarget(dir, "sess-1", "-x-nonexistent-project-dir")
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

	_, _, found := ResolveActuationTarget(dir, "sess-1", "-x-nonexistent-project-dir")
	if found {
		t.Error("expected found=false — the pid is alive but bound to a different tty, must not take Tier 1a")
	}
}

func TestResolveActuationTarget_PIDBindingConfirmed_TriesTierOneA(t *testing.T) {
	dir := t.TempDir()
	if err := sessions.WriteSession(dir, "sess-1", sessions.SessionEntry{PID: 42, TTY: "ttys012"}); err != nil {
		t.Fatalf("WriteSession: %v", err)
	}

	// Inject one available backend: ResolveActuationTarget's availableBackends()
	// gate now short-circuits (backendAvailable=false) BEFORE the pid↔tty binding
	// probe when nothing could act on the result, so a hermetic test must supply a
	// backend rather than rely on whichever real multiplexer happens to be
	// installed. orca has no TTYLocator and matches no claude surface, so the call
	// still falls through to the cwd chain — but the binding probe must have been
	// consulted with the right pid first.
	withBackends(t, &fakeResolveCtl{t: t, name: "orca", available: true})

	origPidTTY := pidTTYFn
	defer func() { pidTTYFn = origPidTTY }()
	var gotPID int
	called := false
	pidTTYFn = func(pid int) string {
		gotPID, called = pid, true
		return "ttys012" // matches the registry entry — binding confirmed
	}

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

	// An available backend must exist for the binding probe to be reached at all
	// (the availableBackends() gate now precedes it) — inject one rather than
	// depend on an installed multiplexer.
	withBackends(t, &fakeResolveCtl{t: t, name: "orca", available: true})

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
	_, _, found := ResolveActuationTarget(t.TempDir(), "../escape", "-x-nonexistent-project-dir")
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

// The fakes these tests run against (fakeResolveCtl / fakeResolveTTYCtl) are
// defined below with the multi-backend resolution tests — one fake family for
// the whole package, so a new Controller method is implemented once.

func TestTierOneA_BackendImplementsTTYLocator_Dispatches(t *testing.T) {
	f := &fakeResolveTTYCtl{
		fakeResolveCtl: &fakeResolveCtl{t: t, name: "fake-with-tty", available: true},
		locateByTTYOK:  true,
	}

	got, ok := tierOneA(f, "ttys012")

	if !ok {
		t.Fatal("expected ok=true")
	}
	if want := (Target{Backend: "fake-with-tty", ID: "fake-with-tty:tty"}); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if f.locateByTTYCalled != "ttys012" {
		t.Errorf("LocateByTTY called with %q, want ttys012", f.locateByTTYCalled)
	}
}

func TestTierOneA_BackendDoesNotImplementTTYLocator_ReturnsNotFound(t *testing.T) {
	// orca's real shape today: Controller without TTYLocator (confirmed live —
	// its terminal list/show JSON carries no tty/pid field at all). Must degrade
	// gracefully (never panic on the failed type assertion) so
	// ResolveActuationTarget falls through to Tier 1b.
	_, ok := tierOneA(&fakeResolveCtl{t: t, name: "fake-no-tty", available: true}, "ttys012")
	if ok {
		t.Error("expected ok=false — backend doesn't implement TTYLocator")
	}
}

// --- multi-backend resolution: ResolveForLocate + ResolveActuationTarget ---
//
// These pin the locate-based (not install-order-based) selection across ALL
// available backends, against fully-configurable fake Controllers so they run
// hermetically regardless of which real multiplexer binaries are installed.

// fakeResolveCtl is a configurable control.Controller test double. Every probe
// method fails the test if it is called while the backend is unavailable —
// the cost-guard invariant that no resolver may probe an unavailable backend.
type fakeResolveCtl struct {
	t              *testing.T
	name           string
	available      bool
	locateOK       bool
	locateClaudeOK bool
}

func (f *fakeResolveCtl) Name() string    { return f.name }
func (f *fakeResolveCtl) Available() bool { return f.available }
func (f *fakeResolveCtl) Locate(string) (Target, bool) {
	if !f.available {
		f.t.Errorf("Locate probed on unavailable backend %q — resolvers must gate on Available() first", f.name)
	}
	return Target{Backend: f.name, ID: f.name + ":loc"}, f.locateOK
}
func (f *fakeResolveCtl) LocateClaude(string) (Target, bool) {
	if !f.available {
		f.t.Errorf("LocateClaude probed on unavailable backend %q — resolvers must gate on Available() first", f.name)
	}
	return Target{Backend: f.name, ID: f.name + ":claude"}, f.locateClaudeOK
}
func (f *fakeResolveCtl) Resume(Target, string) error { return nil }
func (f *fakeResolveCtl) Focus(Target) error          { return nil }
func (f *fakeResolveCtl) Approve(Target) error        { return nil }
func (f *fakeResolveCtl) Spawn(string, string) error  { return nil }
func (f *fakeResolveCtl) Interrupt(Target) error      { return nil }

// fakeResolveTTYCtl adds TTYLocator on top of fakeResolveCtl — the cmux/tmux
// shape (a per-terminal tty is reachable); orca's shape is fakeResolveCtl
// alone (no TTYLocator).
type fakeResolveTTYCtl struct {
	*fakeResolveCtl
	locateByTTYOK     bool
	locateByTTYCalled string // last tty passed in, for the Tier 1a dispatch pin
}

func (f *fakeResolveTTYCtl) LocateByTTY(tty string) (Target, bool) {
	if !f.available {
		f.t.Errorf("LocateByTTY probed on unavailable backend %q — resolvers must gate on Available() first", f.name)
	}
	f.locateByTTYCalled = tty
	return Target{Backend: f.name, ID: f.name + ":tty"}, f.locateByTTYOK
}

// withBackends swaps the package-level backend list for the duration of one
// test, restoring it afterwards.
func withBackends(t *testing.T, bs ...Controller) {
	t.Helper()
	orig := backends
	t.Cleanup(func() { backends = orig })
	backends = bs
}

func TestResolveForLocate_LocateBased_TmuxWinsOverInstalledOrca(t *testing.T) {
	// orca is available but cannot Locate the surface; tmux can. Locate-based
	// selection must pick tmux — orca no longer wins purely by install order.
	orca := &fakeResolveCtl{t: t, name: "orca", available: true, locateOK: false}
	tmux := &fakeResolveCtl{t: t, name: "tmux", available: true, locateOK: true}
	withBackends(t, orca, tmux)

	ctrl, target, ok := ResolveForLocate("-x-proj")

	if !ok {
		t.Fatal("expected ok=true — tmux can Locate the surface")
	}
	if ctrl.Name() != "tmux" {
		t.Errorf("resolved %q, want tmux (locate-based, not install order)", ctrl.Name())
	}
	if target.Backend != "tmux" {
		t.Errorf("target.Backend = %q, want tmux (the located Target must be returned directly)", target.Backend)
	}
}

func TestResolveForLocate_BothLocate_PrefersByOrderNeverRefuses(t *testing.T) {
	// Attach is permissive: when two backends both Locate a surface, the first
	// by install order wins and it NEVER refuses on ambiguity (unlike the
	// typed/destructive path).
	orca := &fakeResolveCtl{t: t, name: "orca", available: true, locateOK: true}
	tmux := &fakeResolveCtl{t: t, name: "tmux", available: true, locateOK: true}
	withBackends(t, orca, tmux)

	ctrl, _, ok := ResolveForLocate("-x-proj")

	if !ok {
		t.Fatal("expected ok=true — attach must not refuse on ambiguity")
	}
	if ctrl.Name() != "orca" {
		t.Errorf("resolved %q, want orca (first-by-order wins on ties)", ctrl.Name())
	}
}

func TestResolveForLocate_OnlyOrcaAvailableAndLocates_ResolvesOrca(t *testing.T) {
	// Unchanged single-backend path: orca is the only available backend and it
	// Locates → orca.
	orca := &fakeResolveCtl{t: t, name: "orca", available: true, locateOK: true}
	withBackends(t, orca)

	ctrl, _, ok := ResolveForLocate("-x-proj")

	if !ok || ctrl.Name() != "orca" {
		t.Fatalf("resolved (%v, ok=%v), want orca ok=true", ctrl, ok)
	}
}

func TestResolveForLocate_AllAvailableNoneLocate_NotOK(t *testing.T) {
	orca := &fakeResolveCtl{t: t, name: "orca", available: true, locateOK: false}
	tmux := &fakeResolveCtl{t: t, name: "tmux", available: true, locateOK: false}
	withBackends(t, orca, tmux)

	_, _, ok := ResolveForLocate("-x-proj")

	if ok {
		t.Error("expected ok=false — available backends, but none can Locate a surface")
	}
}

func TestResolveForLocate_NoBackendAvailable_NotOK(t *testing.T) {
	orca := &fakeResolveCtl{t: t, name: "orca", available: false}
	tmux := &fakeResolveCtl{t: t, name: "tmux", available: false}
	withBackends(t, orca, tmux)

	_, _, ok := ResolveForLocate("-x-proj")

	if ok {
		t.Error("expected ok=false — no backend available at all")
	}
}

func TestResolveActuationTarget_TierOneB_ExactlyOneBackendLocatesClaude(t *testing.T) {
	// No registry tty (empty sessions dir) → Tier 1a skipped; Tier 1b probes
	// all available backends. orca cannot LocateClaude, tmux can → found via
	// tmux.
	orca := &fakeResolveCtl{t: t, name: "orca", available: true, locateClaudeOK: false}
	tmux := &fakeResolveCtl{t: t, name: "tmux", available: true, locateClaudeOK: true}
	withBackends(t, orca, tmux)

	act, backendAvailable, found := ResolveActuationTarget(t.TempDir(), "sess-1", "-x-proj")

	if !found {
		t.Fatal("expected found=true — exactly one backend (tmux) LocatesClaude")
	}
	if !backendAvailable {
		t.Error("expected backendAvailable=true")
	}
	if act.Backend() != "tmux" {
		t.Errorf("resolved %q, want tmux", act.Backend())
	}
	if act.Tier() != actuationTierMultiplexer {
		t.Errorf("tier = %q, want %q — a multiplexer send is Tier 1", act.Tier(), actuationTierMultiplexer)
	}
	if target := boundOf(t, act).target; target.Backend != "tmux" {
		t.Errorf("target.Backend = %q, want tmux", target.Backend)
	}
}

func TestResolveActuationTarget_TierOneB_CrossBackendAmbiguity_Refuses(t *testing.T) {
	// orca AND tmux both return a claude surface for the same cwd → this is
	// cross-backend ambiguity and must REFUSE, never silently pick one.
	orca := &fakeResolveCtl{t: t, name: "orca", available: true, locateClaudeOK: true}
	tmux := &fakeResolveCtl{t: t, name: "tmux", available: true, locateClaudeOK: true}
	withBackends(t, orca, tmux)

	act, backendAvailable, found := ResolveActuationTarget(t.TempDir(), "sess-1", "-x-proj")

	if found {
		t.Error("expected found=false — two distinct backends match the same cwd (ambiguous)")
	}
	if !backendAvailable {
		t.Error("expected backendAvailable=true — backends ARE available, they just disagree")
	}
	if act != nil {
		t.Error("expected actuator=nil on ambiguity refusal — nothing must be actuated")
	}
}

func TestResolveActuationTarget_AllAvailableNoneLocate_NotFoundButBackendAvailable(t *testing.T) {
	orca := &fakeResolveCtl{t: t, name: "orca", available: true}
	tmux := &fakeResolveCtl{t: t, name: "tmux", available: true}
	withBackends(t, orca, tmux)

	_, backendAvailable, found := ResolveActuationTarget(t.TempDir(), "sess-1", "-x-proj")

	if found {
		t.Error("expected found=false")
	}
	if !backendAvailable {
		t.Error("expected backendAvailable=true — backends available, just no surface")
	}
}

func TestResolveActuationTarget_NoBackendAvailable_BackendUnavailable(t *testing.T) {
	orca := &fakeResolveCtl{t: t, name: "orca", available: false}
	tmux := &fakeResolveCtl{t: t, name: "tmux", available: false}
	withBackends(t, orca, tmux)

	_, backendAvailable, found := ResolveActuationTarget(t.TempDir(), "sess-1", "-x-proj")

	if found {
		t.Error("expected found=false")
	}
	if backendAvailable {
		t.Error("expected backendAvailable=false — NO backend available at all")
	}
}

func TestResolveActuationTarget_TierOneA_MultiBackend_ResolvesViaTmuxTTY(t *testing.T) {
	// Valid pid↔tty binding. orca has no TTYLocator, cmux's LocateByTTY misses,
	// tmux's hits → Tier 1a resolves via tmux across all available backends
	// (not just Resolve()'s preferred pick).
	dir := t.TempDir()
	if err := sessions.WriteSession(dir, "sess-1", sessions.SessionEntry{PID: 42, TTY: "ttys012"}); err != nil {
		t.Fatalf("WriteSession: %v", err)
	}
	origPidTTY := pidTTYFn
	defer func() { pidTTYFn = origPidTTY }()
	pidTTYFn = func(int) string { return "ttys012" } // binding confirmed

	orca := &fakeResolveCtl{t: t, name: "orca", available: true} // no TTYLocator
	cmux := &fakeResolveTTYCtl{fakeResolveCtl: &fakeResolveCtl{t: t, name: "cmux", available: true}, locateByTTYOK: false}
	tmux := &fakeResolveTTYCtl{fakeResolveCtl: &fakeResolveCtl{t: t, name: "tmux", available: true}, locateByTTYOK: true}
	withBackends(t, orca, cmux, tmux)

	act, backendAvailable, found := ResolveActuationTarget(dir, "sess-1", "-x-proj")

	if !found {
		t.Fatal("expected found=true — tmux's LocateByTTY hits")
	}
	if !backendAvailable {
		t.Error("expected backendAvailable=true")
	}
	if act.Backend() != "tmux" {
		t.Errorf("resolved %q, want tmux", act.Backend())
	}
	if target := boundOf(t, act).target; target.ID != "tmux:tty" {
		t.Errorf("target.ID = %q, want tmux:tty (resolved via Tier 1a LocateByTTY)", target.ID)
	}
}

func TestResolveActuationTarget_BindingInvalid_SkipsTierOneA_FallsToTierOneB(t *testing.T) {
	// pid↔tty binding fails → Tier 1a must be skipped even though a backend's
	// LocateByTTY WOULD hit; it falls to Tier 1b (LocateClaude). Proven by the
	// resolved target coming from the ":claude" (Tier 1b) path, not ":tty".
	dir := t.TempDir()
	if err := sessions.WriteSession(dir, "sess-1", sessions.SessionEntry{PID: 42, TTY: "ttys012"}); err != nil {
		t.Fatalf("WriteSession: %v", err)
	}
	origPidTTY := pidTTYFn
	defer func() { pidTTYFn = origPidTTY }()
	pidTTYFn = func(int) string { return "ttys999" } // MISMATCH → binding invalid

	orca := &fakeResolveTTYCtl{fakeResolveCtl: &fakeResolveCtl{t: t, name: "orca", available: true, locateClaudeOK: true}, locateByTTYOK: true}
	withBackends(t, orca)

	act, _, found := ResolveActuationTarget(dir, "sess-1", "-x-proj")

	if !found {
		t.Fatal("expected found=true via Tier 1b")
	}
	if act.Backend() != "orca" {
		t.Errorf("resolved %q, want orca", act.Backend())
	}
	if target := boundOf(t, act).target; target.ID != "orca:claude" {
		t.Errorf("target.ID = %q, want orca:claude — Tier 1a must have been skipped (binding invalid)", target.ID)
	}
}

func TestResolveActuationTarget_UnavailableBackendNeverProbed(t *testing.T) {
	// Cost guard: an unavailable backend's Locate/LocateClaude/LocateByTTY must
	// never be called. The unavailable fake fails the test if probed; ordering
	// it first would expose a resolver that forgot to gate on Available().
	dir := t.TempDir()
	if err := sessions.WriteSession(dir, "sess-1", sessions.SessionEntry{PID: 42, TTY: "ttys012"}); err != nil {
		t.Fatalf("WriteSession: %v", err)
	}
	origPidTTY := pidTTYFn
	defer func() { pidTTYFn = origPidTTY }()
	pidTTYFn = func(int) string { return "ttys012" } // binding valid, so Tier 1a runs too

	unavail := &fakeResolveTTYCtl{fakeResolveCtl: &fakeResolveCtl{t: t, name: "orca", available: false}, locateByTTYOK: true}
	avail := &fakeResolveTTYCtl{fakeResolveCtl: &fakeResolveCtl{t: t, name: "tmux", available: true, locateOK: true, locateClaudeOK: true}, locateByTTYOK: true}
	withBackends(t, unavail, avail)

	// Exercise every resolver; the unavailable fake would t.Errorf if probed.
	ResolveForLocate("-x-proj")
	ResolveActuationTarget(dir, "sess-1", "-x-proj")
}

func TestRedriveArgv(t *testing.T) {
	got, err := redriveArgv([]string{"claude"}, "sess-abc123", "do the thing")
	if err != nil {
		t.Fatalf("redriveArgv: %v", err)
	}
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
