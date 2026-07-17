package domain

import "testing"

func TestStateString_NonStalledStates_PlainString(t *testing.T) {
	for _, s := range []LoopState{StateRunning, StateGate, StateIdle, StateDrift, StateDone, StateFailed, StatePaused, StateKilled} {
		if got := StateString(s, StallNone); got != string(s) {
			t.Errorf("StateString(%v, StallNone) = %q, want %q", s, got, s)
		}
	}
}

func TestStateString_Stalled_AppendsStallSlug(t *testing.T) {
	cases := []struct {
		stall StallKind
		want  string
	}{
		{StallTokenOut, "stalled:token-out"},
		{StallRateLimit, "stalled:rate-limit"},
		{StallNoOutput, "stalled:no-output"},
		{StallGone, "stalled:gone"},
	}
	for _, c := range cases {
		if got := StateString(StateStalled, c.stall); got != c.want {
			t.Errorf("StateString(StateStalled, %v) = %q, want %q", c.stall, got, c.want)
		}
	}
}

// TestStateString_NoOutputVersusGone_ProduceDifferentStrings is the core
// regression this encoding exists for: a stall-KIND-only change (both sides
// StateStalled) must produce two DIFFERENT persisted state strings, so a
// FromState != ToState comparison (internal/tui's edge-trigger,
// cmd/fleetops's report transition count) can see it as a real change —
// a plain string(State) comparison could not (both would read "stalled").
func TestStateString_NoOutputVersusGone_ProduceDifferentStrings(t *testing.T) {
	noOutput := StateString(StateStalled, StallNoOutput)
	gone := StateString(StateStalled, StallGone)
	if noOutput == gone {
		t.Fatalf("StateString(no-output) == StateString(gone) == %q, want them to differ", noOutput)
	}
}

func TestLoop_StateString_DelegatesToFreeFunction(t *testing.T) {
	l := Loop{State: StateStalled, Stall: StallGone}
	if got, want := l.StateString(), StateString(StateStalled, StallGone); got != want {
		t.Errorf("Loop.StateString() = %q, want %q (matching the free function)", got, want)
	}
}

func TestStateString_UnknownStallKind_DoesNotPanic(t *testing.T) {
	// a StallKind this package doesn't recognize (future addition, or a
	// zero-value StallKind combined with StateStalled some caller
	// constructed unusually) must degrade to a stable label, never panic.
	got := StateString(StateStalled, StallKind("some future kind"))
	if got == "" {
		t.Error("expected a non-empty fallback string for an unrecognized StallKind")
	}
}
