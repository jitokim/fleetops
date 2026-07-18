package control

// Actuator is a TARGET-BOUND typed-action surface — the whole of what an
// actuation caller needs once resolution has decided WHERE a keypress lands.
// It replaces the (Controller, Target) pair ResolveActuationTarget used to
// return: that pair existed only because Controller is one level too wide for
// this job, so every call site had to carry a Target around purely to hand it
// straight back on the next line.
//
// Narrowing it is what lets a NON-multiplexer host implement actuation at all.
// A Controller must answer Locate/LocateClaude/Spawn — cwd-based surface
// enumeration and loop creation — none of which a terminal EMULATOR like
// iTerm2 has any business implementing (see SendAdapter's doc, and
// docs/adr-vendor-independent-actuation.md §2.1 on why rebuilding cwd
// discovery per host is the layer the ADR exists to delete). Modelling iTerm2
// as a Controller would have meant three permanently-lying methods and an
// "iterm2" Backend string leaking into a Target struct whose doc enumerates
// exactly "orca" | "cmux" | "tmux".
//
// Deliberately NOT given a Kill method: killing is `Resume("/exit")` in this
// codebase (a literal typed into the prompt), not a control character — see
// the TUI's killCmd. Approve is a bare submit and Interrupt is Esc, so three
// methods cover every typed action the fleet board can dispatch.
type Actuator interface {
	Resume(prompt string) error // type text + submit (also carries kill's "/exit")
	Approve() error             // accept the default at a gate (bare submit)
	Interrupt() error           // stop the current turn (Esc), leaving the process alive
	// Backend names the mechanism that will act ("orca"|"cmux"|"tmux"|
	// "iterm2") — for the human-facing "resumed X via <backend>" status text.
	Backend() string
	// Tier is the actuation-event label for this actuator's dispatch tier
	// ("tier1" for a multiplexer, "tier1h" for an in-place host send). The
	// ACTUATOR reports its own tier rather than the TUI mapping backend names
	// to tiers: a stringly-typed `if backend == "iterm2"` at the log site would
	// silently mislabel every future host adapter, and the actuation log is the
	// only post-hoc way to tell an in-place write from a multiplexer send when
	// debugging a misrouted keystroke.
	Tier() string
}

// Actuation tier labels, as they appear in the actuation event log. Distinct
// values are the point: docs/adr-vendor-independent-actuation.md's Tier 1h is
// a different mechanism with different failure modes than Tier 1a/1b, and
// collapsing them would make a misrouted keystroke undiagnosable after the
// fact.
const (
	actuationTierMultiplexer = "tier1"
	actuationTierHostSend    = "tier1h"
)

// boundController adapts a (Controller, Target) pair to Actuator by closing
// over the target — the entire multiplexer side of the Actuator migration.
// Every backend (orca/cmux/tmux) keeps its existing Controller methods
// untouched; only the binding moves, from "every call site threads a Target"
// to "resolution binds it once."
type boundController struct {
	ctrl   Controller
	target Target
}

func (b boundController) Resume(prompt string) error { return b.ctrl.Resume(b.target, prompt) }
func (b boundController) Approve() error             { return b.ctrl.Approve(b.target) }
func (b boundController) Interrupt() error           { return b.ctrl.Interrupt(b.target) }
func (b boundController) Backend() string            { return b.ctrl.Name() }
func (b boundController) Tier() string               { return actuationTierMultiplexer }

// Compile-time assurance the multiplexer binding satisfies the interface.
var _ Actuator = boundController{}
