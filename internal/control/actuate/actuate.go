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

// DispatchVerdict is the structured outcome of a TIER1-ONLY actuation
// (approve/kill/interrupt) — a verb with NO headless Tier 2 to degrade to. It
// is a SEPARATE enum from Actuate's Verdict (not a merged one) for two concrete
// reasons, not because the outcome spaces never overlap — they do, e.g. both
// enums carry a terminal-state refusal and a delivery-unknown case:
//  1. Switch totality. A merged enum would force every renderer switch — on
//     both methods — to handle values that can never occur for its method
//     (Actuate's degrade verdicts for Dispatch, Dispatch's resolve-split for
//     Actuate), the partial-enum smell two focused enums avoid.
//  2. The resolve-failure refinement. A resolve failure splits into
//     backend-unavailable vs not-found; Actuate's RefusedNoSurface structurally
//     cannot carry that split (it collapses both into a Tier 2 fall-through),
//     which is exactly the wrong move for a verb that must never headlessly
//     re-drive (a kill must not re-send "/exit").
//
// See docs/adr-actuation-policy-extraction.md §7.
//
// Names carry a Dispatch prefix where they would otherwise collide with the
// Verdict constants in this package (Delivered / DeliveryUnknown); the two
// enums live side by side, so the identifiers must differ.
type DispatchVerdict int

const (
	// DispatchDelivered: the selected actuator method (Approve / Resume("/exit")
	// / Interrupt) returned nil — a Tier 1 surface accepted the keystroke.
	DispatchDelivered DispatchVerdict = iota
	// DispatchRefusedTerminalState: the loop is StateKilled — a policy stop, not
	// a capability limit. Refused inside the seam BEFORE any resolve, so the
	// destructive kill verb is guarded exactly as strongly as Actuate guards its
	// send path (a headless Dispatch{Action:"kill"} on a killed loop is refused,
	// not silently re-dispatched). Nothing is resolved, dispatched, or logged.
	// StateFailed is deliberately NOT guarded here: killing a governor-stopped
	// (StateFailed) loop is a supported, reachable path (the inject guard tells
	// the operator to do exactly that), so folding it in would change behavior —
	// see docs/adr-actuation-policy-extraction.md §7. The "Dispatch" prefix
	// distinguishes it from Verdict's RefusedTerminalState, with which it shares
	// the same terminal-state honesty rule.
	DispatchRefusedTerminalState
	// RefusedBackendUnavailable: resolve reported no reachable surface at all
	// (backendAvailable == false). Nothing was dispatched, nothing logged.
	RefusedBackendUnavailable
	// RefusedNotFound: a backend exists but resolve could not bind an
	// unambiguous target (found == false). Nothing dispatched, nothing logged.
	RefusedNotFound
	// DispatchDeliveryUnknown: the actuator method returned
	// ErrSendDeliveryUnknown (a Tier 1h host send timed out). It may or may not
	// have landed, so it is deliberately NOT retried — the one outcome an agent
	// must never auto-repeat, and the case that motivated the whole carve-out
	// for the destructive kill verb.
	DispatchDeliveryUnknown
	// DispatchFailed: the actuator method returned any other error; Err carries
	// the definite (observed) cause.
	DispatchFailed
)

// String renders a DispatchVerdict for logs and test failure messages.
func (v DispatchVerdict) String() string {
	switch v {
	case DispatchDelivered:
		return "DispatchDelivered"
	case DispatchRefusedTerminalState:
		return "DispatchRefusedTerminalState"
	case RefusedBackendUnavailable:
		return "RefusedBackendUnavailable"
	case RefusedNotFound:
		return "RefusedNotFound"
	case DispatchDeliveryUnknown:
		return "DispatchDeliveryUnknown"
	case DispatchFailed:
		return "DispatchFailed"
	default:
		return "DispatchVerdict(unknown)"
	}
}

// DispatchRequest is everything the tier1-only dispatch needs. Action selects
// BOTH the actuator method AND the event-log label, so the verb→method mapping
// lives in exactly one place (anti-drift). Action is one of "approve", "kill",
// "interrupt". Unlike ActuationRequest it carries no Prompt: no tier1-only verb
// sends operator text (kill's "/exit" is a fixed internal payload of the verb),
// so an always-empty Prompt would be a latent trap.
type DispatchRequest struct {
	Loop   domain.Loop
	Action string
}

// DispatchResult is the structured verdict for a tier1-only actuation. Err is
// a control send sentinel or nil, same discipline as ActuationResult. Tier and
// Backend carry the display facts the renderer must not re-derive.
type DispatchResult struct {
	Verdict   DispatchVerdict
	Tier      string
	Backend   string
	Err       error
	SessionID string
}

// killExitPayload is the literal typed to end a session — killing is
// Resume("/exit") in this codebase (see control.Actuator's doc), not a control
// character. Naming it keeps the kill verb→payload binding in one spot.
const killExitPayload = "/exit"

// dispatchMethod maps an Action to the actuator method it fires. Keeping the
// verb→method map here (not scattered across per-verb callers) is itself the
// anti-drift point of the Dispatch seam. The bool reports a recognised verb;
// an unrecognised one is refused without touching any surface.
func dispatchMethod(action string) (send func(control.Actuator) error, ok bool) {
	switch action {
	case "approve":
		return func(a control.Actuator) error { return a.Approve() }, true
	case "kill":
		return func(a control.Actuator) error { return a.Resume(killExitPayload) }, true
	case "interrupt":
		return func(a control.Actuator) error { return a.Interrupt() }, true
	default:
		return nil, false
	}
}

// Dispatch runs the tier1-only actuation for approve/kill/interrupt. It is a
// faithful consolidation of the identical resolve → {backend|found split} →
// act → {delivery-unknown|other err|ok} → log skeleton the three verbs used to
// repeat inline. Crucially it has NO Tier 2: a resolve miss refuses (it never
// falls through to a headless re-drive), which is the invariant that keeps a
// kill from ever headlessly re-sending "/exit".
//
// Preconditions (StateKilled; kill's StallGone no-op and Driven engine-kill)
// and approve's success side-effect (gate.DeleteMarkerIfTS) stay with the
// caller — they emit verb-specific strings and even different ok-values, so
// they are not a shared verdict, exactly as ambiguity stays a caller
// precondition for Actuate (ADR §7.2).
func (p Policy) Dispatch(req DispatchRequest) DispatchResult {
	l := req.Loop

	// Terminal-state guard: policy, not capability — the same rule Actuate
	// applies inside its seam, so the destructive tier1-only verbs are not
	// structurally weaker than the send path. StateKilled is a completed human
	// decision; re-driving a keystroke into it (a headless kill re-sending
	// "/exit") is exactly the drift the ADR prevents. Refused before resolve, so
	// nothing is resolved, dispatched, or logged. StateFailed is intentionally
	// excluded (killing a governor-stopped loop is a supported path — ADR §7);
	// each caller's per-verb string is reproduced by the renderer's
	// DispatchRefusedTerminalState arm.
	if l.State == domain.StateKilled {
		return DispatchResult{Verdict: DispatchRefusedTerminalState, SessionID: l.SessionID}
	}

	send, ok := dispatchMethod(req.Action)
	if !ok {
		// Unreachable via the three literal callers; a defensive refusal that
		// dispatches nothing rather than guessing a method.
		return DispatchResult{Verdict: DispatchFailed, Err: errUnknownDispatchAction, SessionID: l.SessionID}
	}

	act, backendAvailable, found := p.ResolveTarget(l.SessionID, l.ProjectDir)
	if !backendAvailable {
		return DispatchResult{Verdict: RefusedBackendUnavailable, SessionID: l.SessionID}
	}
	if !found {
		return DispatchResult{Verdict: RefusedNotFound, SessionID: l.SessionID}
	}

	if err := send(act); err != nil {
		p.LogEvent(l, req.Action, act.Tier(), err)
		// Delivery-unknown is not a failure: the host send timed out and may
		// already have landed, so the caller must NOT retry (see
		// ErrSendDeliveryUnknown). This is the double-send guard the kill verb
		// most depends on.
		if errors.Is(err, control.ErrSendDeliveryUnknown) {
			return DispatchResult{Verdict: DispatchDeliveryUnknown, Tier: act.Tier(), Backend: act.Backend(), Err: err, SessionID: l.SessionID}
		}
		return DispatchResult{Verdict: DispatchFailed, Tier: act.Tier(), Backend: act.Backend(), Err: err, SessionID: l.SessionID}
	}
	p.LogEvent(l, req.Action, act.Tier(), nil)
	return DispatchResult{Verdict: DispatchDelivered, Tier: act.Tier(), Backend: act.Backend(), SessionID: l.SessionID}
}

// errUnknownDispatchAction is the defensive-default cause for a DispatchRequest
// carrying an Action outside {"approve","kill","interrupt"}. It is unreachable
// through the three literal callers and exists only so an unknown verb refuses
// without firing any actuator method.
var errUnknownDispatchAction = errors.New("actuate: unrecognized dispatch action")
