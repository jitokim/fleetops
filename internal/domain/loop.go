// Package domain holds the language-agnostic core: loop lifecycle, the value
// objects that cross the seams, and the (future) ports. See DESIGN.md.
package domain

import "time"

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
// currently has to tab over to discover (the core pain missionctl surfaces).
type StallKind string

const (
	StallNone      StallKind = ""
	StallTokenOut  StallKind = "token budget exhausted"
	StallRateLimit StallKind = "rate limited (429)" // one-key re-send resumes
	StallNoOutput  StallKind = "no output"          // hung / waiting on nothing
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

// Goal is what a loop pursues, plus its hard ceilings.
type Goal struct {
	Text           string
	MaxCycles      int
	BudgetTokens   int
	NoImproveLimit int
}

// Verdict is the oracle's independent judgment of a cycle.
type Verdict struct {
	Outcome Outcome
	Reason  string
}

// Loop is one autonomous loop's renderable state. For the observation MVP a loop
// maps to one Claude Code session (its JSONL log); Project/SessionID/Path locate it,
// LastActivity/Stall come from tailing the log (DESIGN.md, seed spec §Observe).
type Loop struct {
	ID           string
	Name         string
	Goal         Goal
	State        LoopState
	Cycle        int
	TokensSpent  int
	NoImprove    int
	Last         *Verdict
	GatePrompt   string
	Project      string    // decoded project label (e.g. "aboard")
	ProjectDir   string    // raw encoded project dir name, e.g. "-Users-imac-IdeaProjects-aboard"
	Cwd          string    // best-effort decoded absolute cwd, for display only
	SessionID    string    // Claude Code session id
	Path         string    // path to the session JSONL
	LastActivity time.Time // last log write
	Stall        StallKind // why it went silent, if it did
	LastText     string    // last assistant text (tail), for the detail pane's TAIL row
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
