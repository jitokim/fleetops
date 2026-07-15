// Package engine runs the loops. Governor is its one pure piece: budget /
// max-cycles / no-improve are hard ceilings — a loop escalates or fails closed,
// it never silently exceeds them (DESIGN.md §3).
package engine

import "github.com/jitokim/missionctl/internal/domain"

type Action int

const (
	Continue Action = iota // may run another cycle
	Escalate               // raise a human gate rather than dying silently
	Stop                   // terminal FAILED
)

type Decision struct {
	Action Action
	Reason string
}

// Check decides whether a loop may run another cycle. Escalate (ask a human)
// comes before Stop (fail) — a runaway should surface, not vanish.
func Check(l domain.Loop) Decision {
	g := l.Goal
	if g.BudgetTokens > 0 && l.TokensSpent >= g.BudgetTokens {
		return Decision{Escalate, "budget exhausted"}
	}
	if g.MaxCycles > 0 && l.Cycle >= g.MaxCycles {
		return Decision{Escalate, "max cycles reached"}
	}
	if g.NoImproveLimit > 0 && l.NoImprove >= g.NoImproveLimit {
		return Decision{Stop, "no progress for repeated cycles"}
	}
	return Decision{Continue, ""}
}
