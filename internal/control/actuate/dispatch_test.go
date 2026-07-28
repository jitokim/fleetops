package actuate

import (
	"errors"
	"testing"

	"github.com/jitokim/fleetops/internal/control"
	"github.com/jitokim/fleetops/internal/domain"
)

// dispatchFake is a control.Actuator seam for the tier1-only verbs. Unlike the
// send-path fakeActuator it lets EACH method (Approve / Interrupt / Resume,
// which carries kill's "/exit") return its own error, and records which one the
// verb→method selector actually fired.
type dispatchFake struct {
	backend string
	tier    string

	approveErr   error
	interruptErr error
	resumeErr    error

	approveCalled    bool
	interruptCalled  bool
	resumeCalled     bool
	lastResumePrompt string
}

func (f *dispatchFake) Resume(prompt string) error {
	f.resumeCalled = true
	f.lastResumePrompt = prompt
	return f.resumeErr
}
func (f *dispatchFake) Approve() error   { f.approveCalled = true; return f.approveErr }
func (f *dispatchFake) Interrupt() error { f.interruptCalled = true; return f.interruptErr }
func (f *dispatchFake) Backend() string  { return f.backend }
func (f *dispatchFake) Tier() string     { return f.tier }

// dispatchHarness wires a Policy to fakes and records what it did — including
// whether Redrive was ever touched (it must NEVER be, for a tier1-only verb).
type dispatchHarness struct {
	act             *dispatchFake
	backendResolves bool
	targetFound     bool
	resolveCalled   bool

	redriveCalled bool

	events []loggedEvent
}

func (h *dispatchHarness) policy() Policy {
	return Policy{
		ResolveTarget: func(sessionID, projectDir string) (control.Actuator, bool, bool) {
			h.resolveCalled = true
			return h.act, h.backendResolves, h.targetFound
		},
		Redrive: func(cwd, sessionID, prompt, configDir string) error {
			h.redriveCalled = true
			return nil
		},
		LogEvent: func(l domain.Loop, action, tier string, err error) {
			h.events = append(h.events, loggedEvent{action: action, tier: tier, err: err})
		},
	}
}

// TestDispatch_Verdicts drives one row per DispatchVerdict, failure/refusal
// cases first (test-failure-first). The decisive property for every row: the
// tier1-only seam has NO Tier 2, so Redrive must never be called.
func TestDispatch_Verdicts(t *testing.T) {
	timedOut := control.ErrSendDeliveryUnknown
	boom := control.ErrSendTTYMismatch

	tests := []struct {
		name string

		action          string
		backendResolves bool
		targetFound     bool
		tier            string
		backend         string
		methodErr       error // error the selected actuator method returns

		wantVerdict DispatchVerdict
		wantTier    string
		wantBackend string
		wantErr     error // errors.Is target; nil means "want nil"

		// wantResolved / wantDispatched assert the seam reached (or did not
		// reach) the resolve and act stages.
		wantResolved   bool
		wantEventTiers []string
	}{
		// --- failure / refusal cases first ---
		{
			name:            "unrecognized action refuses without resolving or acting",
			action:          "bogus",
			backendResolves: true,
			targetFound:     true,
			wantVerdict:     DispatchFailed,
			wantErr:         errUnknownDispatchAction,
			wantResolved:    false,
			wantEventTiers:  nil,
		},
		{
			name:            "no backend available refuses (no Tier 2 fall-through)",
			action:          "interrupt",
			backendResolves: false,
			targetFound:     false,
			wantVerdict:     RefusedBackendUnavailable,
			wantResolved:    true,
			wantEventTiers:  nil,
		},
		{
			name:            "backend present but target not found refuses",
			action:          "approve",
			backendResolves: true,
			targetFound:     false,
			wantVerdict:     RefusedNotFound,
			wantResolved:    true,
			wantEventTiers:  nil,
		},
		{
			name:            "delivery-unknown carries ErrSendDeliveryUnknown and does NOT redrive",
			action:          "kill",
			backendResolves: true,
			targetFound:     true,
			tier:            "tier1h",
			backend:         "iterm2",
			methodErr:       timedOut,
			wantVerdict:     DispatchDeliveryUnknown,
			wantTier:        "tier1h",
			wantBackend:     "iterm2",
			wantErr:         control.ErrSendDeliveryUnknown,
			wantResolved:    true,
			wantEventTiers:  []string{"tier1h"},
		},
		{
			name:            "ordinary failure reports DispatchFailed with the cause",
			action:          "approve",
			backendResolves: true,
			targetFound:     true,
			tier:            "tier1h",
			backend:         "iterm2",
			methodErr:       boom,
			wantVerdict:     DispatchFailed,
			wantTier:        "tier1h",
			wantBackend:     "iterm2",
			wantErr:         control.ErrSendTTYMismatch,
			wantResolved:    true,
			wantEventTiers:  []string{"tier1h"},
		},
		// --- success cases ---
		{
			name:            "approve delivers via tier1 multiplexer",
			action:          "approve",
			backendResolves: true,
			targetFound:     true,
			tier:            "tier1",
			backend:         "orca",
			wantVerdict:     DispatchDelivered,
			wantTier:        "tier1",
			wantBackend:     "orca",
			wantResolved:    true,
			wantEventTiers:  []string{"tier1"},
		},
		{
			name:            "interrupt delivers via tier1h host send",
			action:          "interrupt",
			backendResolves: true,
			targetFound:     true,
			tier:            "tier1h",
			backend:         "iterm2",
			wantVerdict:     DispatchDelivered,
			wantTier:        "tier1h",
			wantBackend:     "iterm2",
			wantResolved:    true,
			wantEventTiers:  []string{"tier1h"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := &dispatchHarness{
				act: &dispatchFake{
					backend:      tc.backend,
					tier:         tc.tier,
					approveErr:   tc.methodErr,
					interruptErr: tc.methodErr,
					resumeErr:    tc.methodErr,
				},
				backendResolves: tc.backendResolves,
				targetFound:     tc.targetFound,
			}
			l := baseLoop()

			res := h.policy().Dispatch(DispatchRequest{Loop: l, Action: tc.action})

			if res.Verdict != tc.wantVerdict {
				t.Errorf("Verdict = %s, want %s", res.Verdict, tc.wantVerdict)
			}
			if res.Tier != tc.wantTier {
				t.Errorf("Tier = %q, want %q", res.Tier, tc.wantTier)
			}
			if res.Backend != tc.wantBackend {
				t.Errorf("Backend = %q, want %q", res.Backend, tc.wantBackend)
			}
			if tc.wantErr == nil {
				if res.Err != nil {
					t.Errorf("Err = %v, want nil", res.Err)
				}
			} else if !errors.Is(res.Err, tc.wantErr) {
				t.Errorf("Err = %v, want errors.Is %v", res.Err, tc.wantErr)
			}
			if res.SessionID != l.SessionID {
				t.Errorf("SessionID = %q, want %q", res.SessionID, l.SessionID)
			}
			if h.resolveCalled != tc.wantResolved {
				t.Errorf("resolveCalled = %v, want %v", h.resolveCalled, tc.wantResolved)
			}
			// The invariant that defines a tier1-only verb: never a Tier 2
			// re-drive, on ANY row (miss, refusal, unknown, failure, success).
			if h.redriveCalled {
				t.Error("Redrive was called on a tier1-only Dispatch — a kill/approve/interrupt must NEVER fall through to Tier 2")
			}

			if len(h.events) != len(tc.wantEventTiers) {
				t.Fatalf("logged %d events %v, want %d %v", len(h.events), h.events, len(tc.wantEventTiers), tc.wantEventTiers)
			}
			for i, want := range tc.wantEventTiers {
				if h.events[i].tier != want {
					t.Errorf("event[%d].tier = %q, want %q", i, h.events[i].tier, want)
				}
				if h.events[i].action != tc.action {
					t.Errorf("event[%d].action = %q, want %q", i, h.events[i].action, tc.action)
				}
			}
		})
	}
}

// TestDispatch_SelectsCorrectActuatorMethod proves Action maps to the right
// actuator method — approve→Approve, interrupt→Interrupt, kill→Resume("/exit").
// A wrong mapping would render matching text while dispatching the wrong verb.
func TestDispatch_SelectsCorrectActuatorMethod(t *testing.T) {
	tests := []struct {
		action    string
		wantCall  func(f *dispatchFake) bool
		wantOther func(f *dispatchFake) bool
	}{
		{
			action:    "approve",
			wantCall:  func(f *dispatchFake) bool { return f.approveCalled },
			wantOther: func(f *dispatchFake) bool { return f.interruptCalled || f.resumeCalled },
		},
		{
			action:    "interrupt",
			wantCall:  func(f *dispatchFake) bool { return f.interruptCalled },
			wantOther: func(f *dispatchFake) bool { return f.approveCalled || f.resumeCalled },
		},
		{
			action:    "kill",
			wantCall:  func(f *dispatchFake) bool { return f.resumeCalled && f.lastResumePrompt == "/exit" },
			wantOther: func(f *dispatchFake) bool { return f.approveCalled || f.interruptCalled },
		},
	}

	for _, tc := range tests {
		t.Run(tc.action, func(t *testing.T) {
			h := &dispatchHarness{
				act:             &dispatchFake{tier: "tier1", backend: "orca"},
				backendResolves: true,
				targetFound:     true,
			}

			h.policy().Dispatch(DispatchRequest{Loop: baseLoop(), Action: tc.action})

			if !tc.wantCall(h.act) {
				t.Errorf("%q did not fire its actuator method", tc.action)
			}
			if tc.wantOther(h.act) {
				t.Errorf("%q fired the wrong actuator method", tc.action)
			}
		})
	}
}

// TestDispatch_StateKilled_RefusesTerminallyBeforeResolve is the design-review
// WARNING fix: the terminal-state guard lives INSIDE Dispatch (mirroring
// Actuate), so the destructive kill verb is not structurally weaker than the
// send path. A StateKilled loop is refused before ANY resolve, dispatch, or
// event log — a headless Dispatch{Action:"kill"} can no longer silently
// re-send "/exit" into a killed session. Driven across all three tier1-only
// verbs, since the guard is verb-independent.
func TestDispatch_StateKilled_RefusesTerminallyBeforeResolve(t *testing.T) {
	for _, action := range []string{"approve", "kill", "interrupt"} {
		t.Run(action, func(t *testing.T) {
			h := &dispatchHarness{
				act:             &dispatchFake{tier: "tier1", backend: "orca"},
				backendResolves: true,
				targetFound:     true,
			}
			l := baseLoop()
			l.State = domain.StateKilled

			res := h.policy().Dispatch(DispatchRequest{Loop: l, Action: action})

			if res.Verdict != DispatchRefusedTerminalState {
				t.Errorf("Verdict = %s, want DispatchRefusedTerminalState", res.Verdict)
			}
			if res.SessionID != l.SessionID {
				t.Errorf("SessionID = %q, want %q", res.SessionID, l.SessionID)
			}
			if h.resolveCalled {
				t.Error("resolve was called — a terminal-state refusal must not resolve a surface")
			}
			if h.act.approveCalled || h.act.interruptCalled || h.act.resumeCalled {
				t.Error("an actuator method fired — a terminal-state refusal must dispatch nothing")
			}
			if h.redriveCalled {
				t.Error("Redrive fired on a terminal-state refusal")
			}
			if len(h.events) != 0 {
				t.Errorf("logged %v, want no events for a terminal-state refusal", h.events)
			}
		})
	}
}

// TestDispatch_StateFailed_StillDispatches pins the deliberate carve-out: the
// terminal-state guard covers StateKilled ONLY, not StateFailed. Killing a
// governor-stopped (StateFailed) loop is a supported, reachable path (the TUI's
// inject guard directs the operator to do exactly that), so Dispatch must still
// resolve and dispatch it — folding StateFailed into the guard would be a
// behavior change, not a hardening.
func TestDispatch_StateFailed_StillDispatches(t *testing.T) {
	h := &dispatchHarness{
		act:             &dispatchFake{tier: "tier1", backend: "orca"},
		backendResolves: true,
		targetFound:     true,
	}
	l := baseLoop()
	l.State = domain.StateFailed

	res := h.policy().Dispatch(DispatchRequest{Loop: l, Action: "kill"})

	if res.Verdict != DispatchDelivered {
		t.Errorf("Verdict = %s, want DispatchDelivered — StateFailed must still dispatch", res.Verdict)
	}
	if !h.resolveCalled {
		t.Error("resolve was NOT called — a StateFailed kill must reach the surface, not be refused")
	}
	if !(h.act.resumeCalled && h.act.lastResumePrompt == "/exit") {
		t.Error("kill did not dispatch /exit for a StateFailed loop")
	}
}

// TestDispatch_NoTierTwoOnTierOneMiss is the headline safety invariant, called
// out on its own: a resolve miss (no backend, or no unambiguous target) refuses
// and never touches Redrive — the property that keeps a kill from headlessly
// re-sending "/exit".
func TestDispatch_NoTierTwoOnTierOneMiss(t *testing.T) {
	for _, tc := range []struct {
		name            string
		backendResolves bool
		targetFound     bool
		wantVerdict     DispatchVerdict
	}{
		{"no backend", false, false, RefusedBackendUnavailable},
		{"ambiguous target", true, false, RefusedNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &dispatchHarness{
				act:             &dispatchFake{},
				backendResolves: tc.backendResolves,
				targetFound:     tc.targetFound,
			}

			res := h.policy().Dispatch(DispatchRequest{Loop: baseLoop(), Action: "kill"})

			if res.Verdict != tc.wantVerdict {
				t.Errorf("Verdict = %s, want %s", res.Verdict, tc.wantVerdict)
			}
			if h.redriveCalled {
				t.Error("Redrive fired on a Tier-1 miss — a tier1-only verb must never fall through to Tier 2")
			}
			if h.act.resumeCalled {
				t.Error("actuator was called despite a resolve miss")
			}
			if len(h.events) != 0 {
				t.Errorf("logged %v, want no events for a resolve refusal", h.events)
			}
		})
	}
}
