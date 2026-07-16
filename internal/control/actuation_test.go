package control

import (
	"testing"

	"github.com/jitokim/missionctl/internal/sessions"
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
	// pidAliveFn never being called (Tier 1a's own gate: TTY != "" is
	// checked BEFORE the pid-alive probe).
	origPidAlive := pidAliveFn
	defer func() { pidAliveFn = origPidAlive }()
	pidAliveCalled := false
	pidAliveFn = func(pid int) bool { pidAliveCalled = true; return true }

	ResolveActuationTarget(dir, "sess-1", "-x-nonexistent-project-dir")

	if pidAliveCalled {
		t.Error("expected pidAliveFn NOT to be called — an empty TTY must skip Tier 1a entirely")
	}
}

func TestResolveActuationTarget_EntryPresentTTYSetButPIDDead_SkipsTierOneA(t *testing.T) {
	dir := t.TempDir()
	if err := sessions.WriteSession(dir, "sess-1", sessions.SessionEntry{PID: 99999999, TTY: "ttys012"}); err != nil {
		t.Fatalf("WriteSession: %v", err)
	}

	origPidAlive := pidAliveFn
	defer func() { pidAliveFn = origPidAlive }()
	pidAliveFn = func(pid int) bool { return false } // simulate a recycled/dead pid — never trust a stale registry record

	_, _, _, found := ResolveActuationTarget(dir, "sess-1", "-x-nonexistent-project-dir")
	if found {
		t.Error("expected found=false — a dead pid must skip the tty path, and no real surface matches the bogus cwd fallback either")
	}
}

func TestResolveActuationTarget_PIDAliveGateCalledWithRegistryPID(t *testing.T) {
	dir := t.TempDir()
	if err := sessions.WriteSession(dir, "sess-1", sessions.SessionEntry{PID: 42, TTY: "ttys012"}); err != nil {
		t.Fatalf("WriteSession: %v", err)
	}

	origPidAlive := pidAliveFn
	defer func() { pidAliveFn = origPidAlive }()
	var gotPID int
	called := false
	pidAliveFn = func(pid int) bool {
		gotPID, called = pid, true
		return false // doesn't matter for this test — just confirm it's invoked with the right pid
	}

	ResolveActuationTarget(dir, "sess-1", "-x-nonexistent-project-dir")

	if !called {
		t.Fatal("expected pidAliveFn to be called")
	}
	if gotPID != 42 {
		t.Errorf("pidAliveFn called with pid %d, want 42 (the registry entry's PID)", gotPID)
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
