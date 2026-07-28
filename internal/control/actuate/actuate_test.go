package actuate

import (
	"errors"
	"testing"

	"github.com/jitokim/fleetops/internal/control"
	"github.com/jitokim/fleetops/internal/domain"
)

// fakeActuator is a control.Actuator seam for driving each dispatch branch
// without a real terminal. tier selects "tier1" (multiplexer) vs "tier1h"
// (in-place host send), the axis IsHostSendTier keys the degrade decision on.
type fakeActuator struct {
	backend   string
	tier      string
	resumeErr error

	resumeCalled     bool
	lastResumePrompt string
}

func (f *fakeActuator) Resume(prompt string) error {
	f.resumeCalled = true
	f.lastResumePrompt = prompt
	return f.resumeErr
}
func (f *fakeActuator) Approve() error   { return nil }
func (f *fakeActuator) Interrupt() error { return nil }
func (f *fakeActuator) Backend() string  { return f.backend }
func (f *fakeActuator) Tier() string     { return f.tier }

// loggedEvent captures a LogEvent call so tests can assert the tier logged and
// that the terminal-state refusal logs nothing.
type loggedEvent struct {
	action string
	tier   string
	err    error
}

// policyHarness wires a Policy to fakes and records what it did.
type policyHarness struct {
	act             *fakeActuator
	backendResolves bool // ResolveTarget returns backendAvailable
	targetFound     bool // ResolveTarget returns found
	resolveCalled   bool

	redriveErr    error
	redriveCalled bool

	events []loggedEvent
}

func (h *policyHarness) policy() Policy {
	return Policy{
		ResolveTarget: func(sessionID, projectDir string) (control.Actuator, bool, bool) {
			h.resolveCalled = true
			return h.act, h.backendResolves, h.targetFound
		},
		Redrive: func(cwd, sessionID, prompt, configDir string) error {
			h.redriveCalled = true
			return h.redriveErr
		},
		LogEvent: func(l domain.Loop, action, tier string, err error) {
			h.events = append(h.events, loggedEvent{action: action, tier: tier, err: err})
		},
	}
}

func baseLoop() domain.Loop {
	return domain.Loop{
		SessionID:  "sess-abc",
		Project:    "myproj",
		ProjectDir: "-home-user-myproj",
		Cwd:        "/home/user/myproj",
		State:      domain.StateStalled,
		Stall:      domain.StallNone,
	}
}

// TestActuate_Verdicts drives one row per Verdict, failure cases first
// (test-failure-first): the guards and the honest-refusal branches are the ones
// a regression would silently break.
func TestActuate_Verdicts(t *testing.T) {
	tests := []struct {
		name string

		state domain.LoopState
		stall domain.StallKind

		backendResolves bool
		targetFound     bool
		tier            string
		backend         string
		resumeErr       error
		redriveErr      error

		wantVerdict Verdict
		wantTier    string
		wantBackend string
		wantErr     error // errors.Is target; nil means "want nil"

		// wantEventTiers is the ordered list of tiers LogEvent was called with.
		wantEventTiers []string
	}{
		// --- failure / refusal cases first ---
		{
			name:           "StateFailed refuses terminally, dispatches nothing",
			state:          domain.StateFailed,
			wantVerdict:    RefusedTerminalState,
			wantEventTiers: nil,
		},
		{
			name:           "StateKilled refuses terminally, dispatches nothing",
			state:          domain.StateKilled,
			wantVerdict:    RefusedTerminalState,
			wantEventTiers: nil,
		},
		{
			name:            "resolved multiplexer send failure is terminal (no degrade)",
			state:           domain.StateStalled,
			backendResolves: true,
			targetFound:     true,
			tier:            "tier1",
			resumeErr:       control.ErrSendNoSession,
			wantVerdict:     RefusedNoSurface,
			wantTier:        "tier1",
			wantErr:         control.ErrSendNoSession,
			wantEventTiers:  []string{"tier1"},
		},
		{
			name:            "tier1h delivery-unknown carries ErrSendDeliveryUnknown and does NOT redrive",
			state:           domain.StateStalled,
			backendResolves: true,
			targetFound:     true,
			tier:            "tier1h",
			resumeErr:       control.ErrSendDeliveryUnknown,
			wantVerdict:     DeliveryUnknown,
			wantTier:        "tier1h",
			wantErr:         control.ErrSendDeliveryUnknown,
			wantEventTiers:  []string{"tier1h"},
		},
		{
			name:           "tier2 redrive failure refuses with the redrive error",
			state:          domain.StateStalled,
			stall:          domain.StallGone, // skip Tier 1, go straight to Tier 2
			redriveErr:     control.ErrSendNoSession,
			wantVerdict:    RefusedNoSurface,
			wantTier:       "tier2",
			wantErr:        control.ErrSendNoSession,
			wantEventTiers: []string{"tier2"},
		},
		// --- success cases ---
		{
			name:            "tier1 multiplexer send delivers",
			state:           domain.StateStalled,
			backendResolves: true,
			targetFound:     true,
			tier:            "tier1",
			backend:         "orca",
			wantVerdict:     Delivered,
			wantTier:        "tier1",
			wantBackend:     "orca",
			wantEventTiers:  []string{"tier1"},
		},
		{
			name:            "tier1h host send delivers",
			state:           domain.StateStalled,
			backendResolves: true,
			targetFound:     true,
			tier:            "tier1h",
			backend:         "iterm2",
			wantVerdict:     Delivered,
			wantTier:        "tier1h",
			wantBackend:     "iterm2",
			wantEventTiers:  []string{"tier1h"},
		},
		{
			name:            "tier1h refusal degrades to a successful tier2 redrive",
			state:           domain.StateStalled,
			backendResolves: true,
			targetFound:     true,
			tier:            "tier1h",
			resumeErr:       control.ErrSendNoSession, // NOT delivery-unknown, so it degrades
			wantVerdict:     DeliveredTier2,
			wantTier:        "tier2",
			wantEventTiers:  []string{"tier1h", "tier2"},
		},
		{
			name:           "StallGone delivers via tier2 without touching tier1",
			state:          domain.StateStalled,
			stall:          domain.StallGone,
			wantVerdict:    DeliveredTier2,
			wantTier:       "tier2",
			wantEventTiers: []string{"tier2"},
		},
		{
			name:            "no backend available falls through to tier2",
			state:           domain.StateStalled,
			backendResolves: false,
			targetFound:     false,
			wantVerdict:     DeliveredTier2,
			wantTier:        "tier2",
			wantEventTiers:  []string{"tier2"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := &policyHarness{
				act:             &fakeActuator{backend: tc.backend, tier: tc.tier, resumeErr: tc.resumeErr},
				backendResolves: tc.backendResolves,
				targetFound:     tc.targetFound,
				redriveErr:      tc.redriveErr,
			}
			l := baseLoop()
			l.State = tc.state
			l.Stall = tc.stall

			res := h.policy().Actuate(ActuationRequest{
				Loop:   l,
				Prompt: "do the thing",
				Action: "resume",
			})

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

			if len(h.events) != len(tc.wantEventTiers) {
				t.Fatalf("logged %d events %v, want %d %v", len(h.events), h.events, len(tc.wantEventTiers), tc.wantEventTiers)
			}
			for i, want := range tc.wantEventTiers {
				if h.events[i].tier != want {
					t.Errorf("event[%d].tier = %q, want %q", i, h.events[i].tier, want)
				}
			}
		})
	}
}

// TestActuate_TerminalStateSkipsAllDispatch proves the terminal guards short-
// circuit before any seam is touched — the "policy, not capability" invariant:
// Tier 2 could technically revive a killed loop, so nothing must be called.
func TestActuate_TerminalStateSkipsAllDispatch(t *testing.T) {
	for _, state := range []domain.LoopState{domain.StateFailed, domain.StateKilled} {
		t.Run(string(state), func(t *testing.T) {
			h := &policyHarness{act: &fakeActuator{}}
			l := baseLoop()
			l.State = state

			res := h.policy().Actuate(ActuationRequest{Loop: l, Prompt: "p", Action: "resume"})

			if res.Verdict != RefusedTerminalState {
				t.Fatalf("Verdict = %s, want RefusedTerminalState", res.Verdict)
			}
			if h.resolveCalled || h.act.resumeCalled || h.redriveCalled {
				t.Errorf("dispatched something on a terminal loop: resolve=%v resume=%v redrive=%v",
					h.resolveCalled, h.act.resumeCalled, h.redriveCalled)
			}
			if len(h.events) != 0 {
				t.Errorf("logged %v, want no events for a terminal-state refusal", h.events)
			}
		})
	}
}

// TestActuate_DeliveryUnknownDoesNotRedrive is the carve-out that most matters
// for a headless agent: a timed-out host send must NOT be auto-retried.
func TestActuate_DeliveryUnknownDoesNotRedrive(t *testing.T) {
	h := &policyHarness{
		act:             &fakeActuator{tier: "tier1h", resumeErr: control.ErrSendDeliveryUnknown},
		backendResolves: true,
		targetFound:     true,
	}
	l := baseLoop()

	res := h.policy().Actuate(ActuationRequest{Loop: l, Prompt: "p", Action: "resume"})

	if res.Verdict != DeliveryUnknown {
		t.Fatalf("Verdict = %s, want DeliveryUnknown", res.Verdict)
	}
	if !errors.Is(res.Err, control.ErrSendDeliveryUnknown) {
		t.Errorf("Err = %v, want errors.Is ErrSendDeliveryUnknown", res.Err)
	}
	if h.redriveCalled {
		t.Error("redrive was called on a delivery-unknown host send — must never auto-retry")
	}
}
