// Package domain holds the language-agnostic core: loop lifecycle, the value
// objects that cross the seams, and the (future) ports. See DESIGN.md.
package domain

import (
	"strings"
	"time"
)

// LoopState is where a loop sits in its lifecycle (DESIGN.md §3).
type LoopState string

const (
	StateRunning LoopState = "running" // a cycle is (or will be) executing
	StateGate    LoopState = "gate"    // blocked, waiting on a human decision
	StateStalled LoopState = "stalled" // silently stuck: token budget out / 429 / no output — recoverable
	StateIdle    LoopState = "idle"    // turn complete, waiting on human — not an incident
	StateDrift   LoopState = "drift"   // oracle rejected the agent's "done"
	StateDone    LoopState = "done"    // oracle-verified converged
	StateFailed  LoopState = "failed"  // governor stopped it, unrecoverable
	StatePaused  LoopState = "paused"  // human paused
	StateKilled  LoopState = "killed"  // human killed
)

// StallKind classifies why a loop went silent — the "why did it stop?" a human
// currently has to tab over to discover (the core pain fleetops surfaces).
type StallKind string

const (
	StallNone      StallKind = ""
	StallTokenOut  StallKind = "token budget exhausted"
	StallRateLimit StallKind = "rate limited (429)" // one-key re-send resumes
	StallNoOutput  StallKind = "no output"          // hung / waiting on nothing
	StallGone      StallKind = "process gone"       // terminal closed/crashed mid-work — an incident, not a resume (restart instead)
)

// Terminal reports whether no further work will happen for this loop.
func (s LoopState) Terminal() bool {
	switch s {
	case StateDone, StateFailed, StateKilled:
		return true
	}
	return false
}

// Outcome is the oracle's conclusion about a cycle — the only authority on "done".
type Outcome string

const (
	OutcomeProgress   Outcome = "progress"
	OutcomeDone       Outcome = "done"
	OutcomeRejected   Outcome = "rejected" // agent claimed done but it isn't (drift)
	OutcomeNeedsHuman Outcome = "needs_human"
)

// Goal is what a loop pursues, plus its hard ceilings. DoneWhen/Rubric/
// Challenger are the rest of the wizard's loop contract (see the tui's "n"
// key and internal/registry.BindSpec) — DoneWhen and Rubric together are
// fed to the ORACLE as its judging rubric (internal/oracle.Judge);
// Challenger is stored only, not executed (no challenger phase exists yet).
//
// feat/panel-info (precise rename): this field used to be named Oracle,
// which conflated two different things — the CRITERIA a human hands the
// judge ("how do I verify this is done") and the JUDGE ITSELF (the
// internal/oracle package, the ORACLE row's rendered verdict, the header's
// "oracle NN%" pass-rate band). Renamed to Rubric so "oracle" means
// exclusively the judge/verdict from here on. The ONE place this rename
// deliberately does NOT reach is the on-disk JSON key
// (registry.recordFile/pendingFile keep `json:"oracle"` — see registry.go's
// doc — so an already-persisted record loads unchanged).
type Goal struct {
	Text           string
	DoneWhen       string // completion condition — what evidence makes it DONE; "" = oracle judges against Text alone
	Rubric         string // verification rubric (free text); "" = default "independent LLM judge against the complete condition"
	Challenger     string // adversarial probe description — STORED ONLY, never executed
	MaxCycles      int
	BudgetTokens   int
	NoImproveLimit int
}

// Verdict is the oracle's independent judgment of a cycle. AtCycle is the
// Loop.Cycle this verdict was rendered against — lets the scanner and the
// TUI's judge trigger-policy tell "already judged this cycle" (AtCycle ==
// Cycle) from "cycle advanced since" (Cycle > AtCycle).
type Verdict struct {
	Outcome Outcome
	Reason  string
	AtCycle int
}

// Loop is one autonomous loop's renderable state. For the observation MVP a loop
// maps to one Claude Code session (its JSONL log); Project/SessionID/Path locate it,
// LastActivity/Stall come from tailing the log (DESIGN.md, seed spec §Observe).
type Loop struct {
	ID           string
	Name         string // explicit human-given display name (wizard's "name" step, via registry.Record.Name); "" when none — see DisplayLabel
	Goal         Goal
	State        LoopState
	Cycle        int
	TokensSpent  int
	NoImprove    int
	Last         *Verdict
	GatePrompt   string
	Project      string    // decoded project label (e.g. "myproject")
	ProjectDir   string    // raw encoded project dir name, e.g. "-home-user-myproject"
	Cwd          string    // best-effort decoded absolute cwd, for display only — see CwdVerified
	CwdVerified  bool      // true once Cwd was confirmed against a live process's real lsof path (not a lossy decode); gates spawn-into-this-dir (see tui's "n" key)
	SessionID    string    // Claude Code session id
	Path         string    // path to the session JSONL
	LastActivity time.Time // last log write
	Stall        StallKind // why it went silent, if it did
	LastText     string    // last assistant text (tail), for the detail pane's TAIL row
	GateTS       int64     // unix NANOSECONDS of the gate marker this loop's StateGate was derived from, if any — lets approveCmd compare-and-swap delete only the marker it actually decided on (see gate.DeleteMarkerIfTS; nanosecond precision is what lets the CAS distinguish two markers landing in the same second)
	Note         string    // governor-set annotation (internal/engine.Check via the scanner's applyGovernor) — the tui's NOTE column prefers this over stall/drift text when set; "" leaves NOTE's existing stall/drift behavior untouched
	BoundAt      time.Time // when this loop was bound (internal/registry.Record.BoundAt, copied in by enrichFromRegistry) — zero value for an unbound loop; feat/detail-panel-v2's STAGE row prefers this for "elapsed", falling back to the event log's first entry when zero

	// Driven is the durable "this session is engine-owned" flag. The
	// scanner stays the SOLE owner of State; Driven is a SEPARATE,
	// registry-persisted fact copied onto Loop the same way BoundAt already
	// is, not a second state machine. It's set true at engine-bootstrap,
	// copied from registry.Record.Driven onto this field via
	// enrichFromRegistry (mirroring BoundAt's own copy-in), and read by
	// engine.ShouldDrive as an explicit bool parameter rather than being
	// read directly off this field (see ShouldDrive's own doc for why).
	// Also doubles as the take-over pause switch: attaching to a Driven
	// loop clears this to false so a human interactively driving the
	// session (via `claude --resume <id>` in a real terminal) can never
	// race the engine into firing a concurrent cycle — ShouldDrive's
	// driven==false clause already covers that pause with no extra code
	// (see its doc).
	Driven bool
}

// DisplayLabel is the loop's human-facing list label, in falling priority:
// the explicit display name given at creation (Name), else the goal's first
// line (a bound loop is best identified by WHAT it's doing), else the
// project label (all an observed, never-bound session has). Truncation to a
// column width stays with the renderer — this only picks WHICH text
// identifies the loop.
func (l Loop) DisplayLabel() string {
	if l.Name != "" {
		return l.Name
	}
	if line := firstLine(l.Goal.Text); line != "" {
		return line
	}
	return l.Project
}

// firstLine is DisplayLabel's goal-text normalizer: the first non-empty
// line, whitespace-trimmed — a multi-line goal's opening line is its best
// one-row summary, and embedded newlines would corrupt a single-row layout.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

// BudgetFrac is the fraction of the token budget consumed (0..1).
func (l Loop) BudgetFrac() float64 {
	cap := l.Goal.BudgetTokens
	if cap <= 0 {
		return 0
	}
	f := float64(l.TokensSpent) / float64(cap)
	if f > 1 {
		return 1
	}
	return f
}

// StateString encodes a loop's classified state as the string internal/tui
// and internal/registry persist to the append-only event history
// (internal/events.Event's FromState/ToState) — State alone is not enough:
// a StallKind change (e.g. StallNoOutput → StallGone) with State staying
// StateStalled throughout is a real, notify- and report-worthy incident
// (loop-gone), but a plain string(State) comparison can't see it (both
// sides would just read "stalled"). Every OTHER state is unaffected — only
// StateStalled gets the stall kind appended, since State and StallKind are
// otherwise orthogonal-in-practice (no other state carries a meaningful
// StallKind).
func (l Loop) StateString() string {
	return StateString(l.State, l.Stall)
}

// StateString is StateString's free-function core, for callers building an
// event record from state/stall values that aren't packaged into a Loop
// (e.g. internal/claude's governor recording a PRE-transition state it
// captured separately from the Loop it's mutating in place).
func StateString(state LoopState, stall StallKind) string {
	if state != StateStalled {
		return string(state)
	}
	return string(state) + ":" + stallSlug(stall)
}

// stallSlug is StallKind's short, machine-stable, hyphenated form for
// StateString — the StallKind constants themselves are full human sentences
// ("process gone", "rate limited (429)") meant for the TUI's prose, not for
// concatenating into a colon-delimited state key.
func stallSlug(s StallKind) string {
	switch s {
	case StallTokenOut:
		return "token-out"
	case StallRateLimit:
		return "rate-limit"
	case StallNoOutput:
		return "no-output"
	case StallGone:
		return "gone"
	default:
		return "unknown"
	}
}
