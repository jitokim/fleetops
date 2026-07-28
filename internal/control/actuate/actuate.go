// Package actuate holds the send/redrive POLICY — the honesty/guard decision
// that picks delivered vs. degraded-to-Tier-2 vs. delivery-unknown-do-not-
// redrive vs. refused-terminal-state — lifted out of the TUI so a headless
// agent-operator can reuse the exact same rules. It covers the send-shaped
// actuations (resume / inject / drive / 429-redrive) that share the
// tier1→tier2 degrade machine, NOT the tier1-only verbs (approve/kill/
// interrupt); ambiguity is a caller precondition adjudicated upstream, not part
// of this policy (see the ADR).
//
// See docs/adr-actuation-policy-extraction.md. The seam is deliberately narrow:
// the policy composes the internal/control primitives (the six send sentinels,
// IsHostSendTier, the resolve/redrive adapters) with the terminal-state guard,
// and returns a STRUCTURED Verdict — never English prose. The caller (TUI
// renderer, or a future agent) branches on the Verdict and the sentinel-typed
// Err; it never string-parses a status line.
//
// Dependency direction is one-way: actuate → control → domain. The policy takes
// its side effects (resolve, redrive, event-log) as injected function seams so
// it imports neither bubbletea nor the event/sessions infrastructure, and so
// tests can drive every branch with fakes.
package actuate

import (
	"errors"

	"github.com/jitokim/fleetops/internal/control"
	"github.com/jitokim/fleetops/internal/domain"
)

// Verdict is the structured outcome of an actuation attempt — one value per
// honesty outcome the TUI used to encode only inside its status string. An
// agent-operator switches on it; the TUI maps it back to the same prose.
type Verdict int

const (
	// Delivered: a Tier 1 surface (multiplexer or in-place host send) accepted
	// the prompt.
	Delivered Verdict = iota
	// DeliveredTier2: the prompt landed via the headless Tier 2 re-drive —
	// either a StallGone loop's normal path, or a degrade after Tier 1 could
	// not target the on-screen session.
	DeliveredTier2
	// RefusedTerminalState: the loop is StateFailed (governor-stopped) or
	// StateKilled — a policy stop, not a capability limit. Nothing dispatched.
	RefusedTerminalState
	// RefusedNoSurface: a dispatch was attempted but delivery failed with a
	// definite (observed) failure — either a resolved non-host Tier 1 surface
	// refused, or the Tier 2 re-drive itself failed. Err carries the cause.
	RefusedNoSurface
	// DeliveryUnknown: a Tier 1h host send timed out (ErrSendDeliveryUnknown).
	// It may or may not have landed, so it is deliberately NOT re-driven — the
	// one outcome an agent must never auto-retry.
	DeliveryUnknown
)

// String renders a Verdict for logs and test failure messages.
func (v Verdict) String() string {
	switch v {
	case Delivered:
		return "Delivered"
	case DeliveredTier2:
		return "DeliveredTier2"
	case RefusedTerminalState:
		return "RefusedTerminalState"
	case RefusedNoSurface:
		return "RefusedNoSurface"
	case DeliveryUnknown:
		return "DeliveryUnknown"
	default:
		return "Verdict(unknown)"
	}
}

// ActuationRequest is everything the policy needs to decide. Every input comes
// from the loop or the caller — no bubbletea Model, no global state.
//
// Action is the actuation-event label ("resume" / "inject"): a shared
// vocabulary token, passed straight to the event log AND reused by the caller's
// renderer (e.g. the DeliveryUnknown status line prefixes it). It is not the
// full display verb (that stays with the caller as successVerb), but it is not
// purely internal either.
type ActuationRequest struct {
	Loop   domain.Loop
	Prompt string
	Action string
}

// ActuationResult is the structured verdict. Err is always one of the
// internal/control send sentinels or nil. Tier ("tier1" / "tier1h" / "tier2")
// and Backend ("orca" / "tmux" / "iterm2" / …) carry the two facts the TUI
// renderer needs to reconstruct its status line without re-deciding anything.
type ActuationResult struct {
	Verdict   Verdict
	Tier      string
	Backend   string
	Err       error
	SessionID string
}

// ResolveTargetFunc resolves the in-place actuation surface for a loop. It
// mirrors control.ResolveActuationTarget with the sessions dir already bound by
// the caller, so the policy stays free of the sessions package.
type ResolveTargetFunc func(sessionID, projectDir string) (act control.Actuator, backendAvailable, found bool)

// RedriveFunc performs the Tier 2 headless re-drive (control.Redrive).
type RedriveFunc func(cwd, sessionID, prompt, configDir string) error

// LogEventFunc best-effort records an actuation event. Injected so the policy
// does not import the events/history infrastructure; the caller wires in the
// real logger. Called only where a tier was actually dispatched — never for the
// terminal-state refusal, which takes nothing to log.
type LogEventFunc func(l domain.Loop, action, tier string, err error)

// Policy is the actuation decision, parameterised by its side-effect seams.
type Policy struct {
	ResolveTarget ResolveTargetFunc
	Redrive       RedriveFunc
	LogEvent      LogEventFunc
}

// Actuate runs the honesty/guard decision for one send. It is a faithful
// re-typing of the TUI's former sendPromptCmd body: the same terminal-state
// guard and the same tier1→tier2 order, now returning a Verdict instead of a
// formatted string.
func (p Policy) Actuate(req ActuationRequest) ActuationResult {
	l := req.Loop

	// Terminal-state guards: policy, not capability. StateFailed (governor
	// stopped, no improvement) and StateKilled are deliberately terminal —
	// Tier 2's headless re-drive is technically capable of reviving either, so
	// this must be blocked here, not left to accidentally succeed. Nothing is
	// dispatched, so nothing is logged.
	if l.State == domain.StateFailed || l.State == domain.StateKilled {
		return ActuationResult{Verdict: RefusedTerminalState, SessionID: l.SessionID}
	}

	// Tier 1: in-place send, skipped entirely for a StallGone loop (its
	// on-screen process is gone; go straight to the headless re-drive of the
	// same session).
	if l.Stall != domain.StallGone {
		act, backendAvailable, found := p.ResolveTarget(l.SessionID, l.ProjectDir)
		if backendAvailable && found {
			err := act.Resume(req.Prompt)
			if err == nil {
				p.LogEvent(l, req.Action, act.Tier(), nil)
				return ActuationResult{Verdict: Delivered, Tier: act.Tier(), Backend: act.Backend(), SessionID: l.SessionID}
			}
			p.LogEvent(l, req.Action, act.Tier(), err)
			// A resolved NON-host (multiplexer) send that failed is terminal
			// here: only Tier 1h degrades to Tier 2 (see control.IsHostSendTier).
			if !control.IsHostSendTier(act) {
				return ActuationResult{Verdict: RefusedNoSurface, Tier: act.Tier(), Err: err, SessionID: l.SessionID}
			}
			// The one Tier 1h failure that MAY already have delivered: a
			// deadline kill. Stop and say the outcome is unknown rather than
			// risk a double-send by degrading.
			if errors.Is(err, control.ErrSendDeliveryUnknown) {
				return ActuationResult{Verdict: DeliveryUnknown, Tier: act.Tier(), Err: err, SessionID: l.SessionID}
			}
			// Any other Tier 1h failure degrades: fall through to Tier 2.
		}
	}

	// Tier 2: vendor-independent headless re-drive. Works on every host,
	// including a StallGone bare shell or an ambiguous/no-backend Tier 1 miss.
	if err := p.Redrive(l.Cwd, l.SessionID, req.Prompt, l.Account.ConfigDir); err != nil {
		p.LogEvent(l, req.Action, "tier2", err)
		return ActuationResult{Verdict: RefusedNoSurface, Tier: "tier2", Err: err, SessionID: l.SessionID}
	}
	p.LogEvent(l, req.Action, "tier2", nil)
	// Both Tier 2 successes are DeliveredTier2; the renderer distinguishes the
	// StallGone "re-drove headlessly" wording from the degrade "background turn"
	// wording on l.Stall, exactly as the former sendPromptCmd did.
	return ActuationResult{Verdict: DeliveredTier2, Tier: "tier2", SessionID: l.SessionID}
}
