package engine

import (
	"testing"

	"github.com/jitokim/missionctl/internal/domain"
)

func TestCheck_NoCeilingsExceeded_Continue(t *testing.T) {
	l := domain.Loop{
		Goal:        domain.Goal{BudgetTokens: 1000, MaxCycles: 10, NoImproveLimit: 3},
		TokensSpent: 100,
		Cycle:       2,
		NoImprove:   0,
	}
	d := Check(l)
	if d.Action != Continue {
		t.Errorf("Action = %v, want Continue", d.Action)
	}
}

func TestCheck_BudgetExhausted_Escalates(t *testing.T) {
	l := domain.Loop{
		Goal:        domain.Goal{BudgetTokens: 1000, MaxCycles: 10, NoImproveLimit: 3},
		TokensSpent: 1000,
		Cycle:       2,
	}
	d := Check(l)
	if d.Action != Escalate {
		t.Fatalf("Action = %v, want Escalate", d.Action)
	}
	if d.Reason != "budget exhausted" {
		t.Errorf("Reason = %q, want %q", d.Reason, "budget exhausted")
	}
}

func TestCheck_BudgetExceeded_StillEscalates(t *testing.T) {
	// >= is the ceiling check, not ==.
	l := domain.Loop{Goal: domain.Goal{BudgetTokens: 1000}, TokensSpent: 1500}
	if d := Check(l); d.Action != Escalate {
		t.Errorf("Action = %v, want Escalate for TokensSpent past BudgetTokens", d.Action)
	}
}

func TestCheck_MaxCyclesReached_Escalates(t *testing.T) {
	l := domain.Loop{
		Goal:  domain.Goal{MaxCycles: 12, NoImproveLimit: 3},
		Cycle: 12,
	}
	d := Check(l)
	if d.Action != Escalate {
		t.Fatalf("Action = %v, want Escalate", d.Action)
	}
	if d.Reason != "max cycles reached" {
		t.Errorf("Reason = %q, want %q", d.Reason, "max cycles reached")
	}
}

func TestCheck_NoImproveAtLimit_Stops(t *testing.T) {
	l := domain.Loop{
		Goal:      domain.Goal{NoImproveLimit: 3},
		NoImprove: 3,
	}
	d := Check(l)
	if d.Action != Stop {
		t.Fatalf("Action = %v, want Stop", d.Action)
	}
	if d.Reason != "no progress for repeated cycles" {
		t.Errorf("Reason = %q, want %q", d.Reason, "no progress for repeated cycles")
	}
}

func TestCheck_NoImprovePastLimit_StillStops(t *testing.T) {
	l := domain.Loop{Goal: domain.Goal{NoImproveLimit: 3}, NoImprove: 5}
	if d := Check(l); d.Action != Stop {
		t.Errorf("Action = %v, want Stop for NoImprove past the limit", d.Action)
	}
}

func TestCheck_BudgetTakesPriorityOverMaxCyclesAndNoImprove(t *testing.T) {
	// all three ceilings breached at once — Escalate (budget) must win,
	// matching Check's documented "Escalate before Stop" ordering, and
	// budget is checked before max cycles within Escalate.
	l := domain.Loop{
		Goal:        domain.Goal{BudgetTokens: 1000, MaxCycles: 5, NoImproveLimit: 3},
		TokensSpent: 1000,
		Cycle:       10,
		NoImprove:   5,
	}
	d := Check(l)
	if d.Action != Escalate || d.Reason != "budget exhausted" {
		t.Errorf("got %+v, want Escalate/budget exhausted (highest priority)", d)
	}
}

func TestCheck_MaxCyclesTakesPriorityOverNoImprove(t *testing.T) {
	l := domain.Loop{
		Goal:      domain.Goal{MaxCycles: 5, NoImproveLimit: 3},
		Cycle:     10,
		NoImprove: 5,
	}
	d := Check(l)
	if d.Action != Escalate || d.Reason != "max cycles reached" {
		t.Errorf("got %+v, want Escalate/max cycles reached (priority over Stop)", d)
	}
}

func TestCheck_ZeroCeilings_NeverTrigger(t *testing.T) {
	// a zero/negative ceiling means "no ceiling set" — must never fire,
	// regardless of how large the corresponding counter is.
	l := domain.Loop{
		Goal:        domain.Goal{BudgetTokens: 0, MaxCycles: 0, NoImproveLimit: 0},
		TokensSpent: 999999,
		Cycle:       999999,
		NoImprove:   999999,
	}
	if d := Check(l); d.Action != Continue {
		t.Errorf("got %+v, want Continue (no ceilings configured)", d)
	}
}
