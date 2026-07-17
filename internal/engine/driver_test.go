package engine

import (
	"strings"
	"testing"

	"github.com/jitokim/missionctl/internal/domain"
	"github.com/jitokim/missionctl/internal/registry"
)

// ── ShouldDrive: the fail-closed truth table ────────────────────────────

func TestShouldDrive_TruthTable(t *testing.T) {
	baseGoal := domain.Goal{BudgetTokens: 1000, MaxCycles: 10, NoImproveLimit: 3}

	cases := []struct {
		name     string
		l        domain.Loop
		driven   bool
		inFlight bool
		want     bool
	}{
		{
			name:   "idle + driven + governor continue → drive",
			l:      domain.Loop{State: domain.StateIdle, Goal: baseGoal, TokensSpent: 100, Cycle: 2, NoImprove: 0},
			driven: true,
			want:   true,
		},
		{
			name:   "not driven → never drive, even if otherwise eligible",
			l:      domain.Loop{State: domain.StateIdle, Goal: baseGoal, TokensSpent: 100, Cycle: 2},
			driven: false,
			want:   false,
		},
		{
			name:     "in flight → never drive (interlock)",
			l:        domain.Loop{State: domain.StateIdle, Goal: baseGoal, TokensSpent: 100, Cycle: 2},
			driven:   true,
			inFlight: true,
			want:     false,
		},
		{
			name:   "StateGate → never drive (the fail-closed heart: no approve path)",
			l:      domain.Loop{State: domain.StateGate, Goal: baseGoal, TokensSpent: 100, Cycle: 2},
			driven: true,
			want:   false,
		},
		{
			name:   "StateRunning → never drive (turn already in flight)",
			l:      domain.Loop{State: domain.StateRunning, Goal: baseGoal, TokensSpent: 100, Cycle: 2},
			driven: true,
			want:   false,
		},
		{
			name:   "StateStalled → never drive (surfaced to human, no auto-recovery here)",
			l:      domain.Loop{State: domain.StateStalled, Stall: domain.StallNoOutput, Goal: baseGoal, TokensSpent: 100, Cycle: 2},
			driven: true,
			want:   false,
		},
		{
			name:   "StateDone (terminal) → never drive",
			l:      domain.Loop{State: domain.StateDone, Goal: baseGoal, TokensSpent: 100, Cycle: 2},
			driven: true,
			want:   false,
		},
		{
			name:   "StateFailed (terminal) → never drive",
			l:      domain.Loop{State: domain.StateFailed, Goal: baseGoal, TokensSpent: 100, Cycle: 2},
			driven: true,
			want:   false,
		},
		{
			name:   "StateKilled (terminal) → never drive",
			l:      domain.Loop{State: domain.StateKilled, Goal: baseGoal, TokensSpent: 100, Cycle: 2},
			driven: true,
			want:   false,
		},
		{
			name:   "StateDrift → never drive (idle only; DRIFT needs a human's guided re-drive, not the engine)",
			l:      domain.Loop{State: domain.StateDrift, Goal: baseGoal, TokensSpent: 100, Cycle: 2},
			driven: true,
			want:   false,
		},
		{
			name:   "governor Stop (no-improve at limit) → never drive",
			l:      domain.Loop{State: domain.StateIdle, Goal: baseGoal, NoImprove: 3},
			driven: true,
			want:   false,
		},
		{
			name:   "governor Escalate (budget exhausted) → never drive",
			l:      domain.Loop{State: domain.StateIdle, Goal: baseGoal, TokensSpent: 1000},
			driven: true,
			want:   false,
		},
		{
			name:   "governor Escalate (max cycles reached) → never drive",
			l:      domain.Loop{State: domain.StateIdle, Goal: baseGoal, Cycle: 10},
			driven: true,
			want:   false,
		},
		{
			name:   "zero ceilings (unbound-style Goal) → governor never blocks; idle+driven drives",
			l:      domain.Loop{State: domain.StateIdle, Goal: domain.Goal{}, TokensSpent: 999999, Cycle: 999999},
			driven: true,
			want:   true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ShouldDrive(c.l, c.driven, c.inFlight); got != c.want {
				t.Errorf("ShouldDrive(...) = %v, want %v", got, c.want)
			}
		})
	}
}

// TestShouldDrive_InFlightWinsOverEverythingElse: even a loop that would
// otherwise clearly drive (idle, driven, governor continue) must not, once
// a Redrive is already running for it — the interlock is checked
// independently of every other clause, not just as a late tiebreaker.
func TestShouldDrive_InFlightWinsOverEverythingElse(t *testing.T) {
	l := domain.Loop{
		State: domain.StateIdle,
		Goal:  domain.Goal{BudgetTokens: 1000, MaxCycles: 10, NoImproveLimit: 3},
	}
	if ShouldDrive(l, true, true) {
		t.Error("expected false — a Redrive already in flight must block driving regardless of every other clause")
	}
}

// ── NextWorkPrompt: composition ─────────────────────────────────────────

func TestNextWorkPrompt_ComposesGoalDoneWhenRubricAndCycle(t *testing.T) {
	l := domain.Loop{Cycle: 4}
	contract := registry.Record{
		Goal:          "fix the flaky tests",
		DoneCondition: "a fresh test run passes with zero failures",
		Rubric:        "rerun from scratch and show the fresh output",
	}
	got := NextWorkPrompt(l, contract)

	for _, want := range []string{
		"goal: fix the flaky tests",
		"complete condition: a fresh test run passes with zero failures",
		"rubric: rerun from scratch and show the fresh output",
		"cycle: 4",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q, got:\n%s", want, got)
		}
	}
}

// TestNextWorkPrompt_EmptyDoneCondition_UsesDefault mirrors
// buildSpawnPrompt's own default ("you judge the goal fully achieved") —
// same fallback wording, so a loop bound before/without a doneCondition
// gets the identical default whether it's cycle 1 (buildSpawnPrompt) or a
// later engine cycle (NextWorkPrompt).
func TestNextWorkPrompt_EmptyDoneCondition_UsesDefault(t *testing.T) {
	l := domain.Loop{Cycle: 2}
	contract := registry.Record{Goal: "ship it"}
	got := NextWorkPrompt(l, contract)
	if !strings.Contains(got, "complete condition: you judge the goal fully achieved") {
		t.Errorf("got %q, want the default doneCondition wording", got)
	}
}

// TestNextWorkPrompt_EmptyRubric_UsesDefault mirrors buildSpawnPrompt's own
// default rubric wording.
func TestNextWorkPrompt_EmptyRubric_UsesDefault(t *testing.T) {
	l := domain.Loop{Cycle: 2}
	contract := registry.Record{Goal: "ship it", DoneCondition: "tests pass"}
	got := NextWorkPrompt(l, contract)
	if !strings.Contains(got, "rubric: an independent LLM judge verifies against the complete condition") {
		t.Errorf("got %q, want the default rubric wording", got)
	}
}

// TestNextWorkPrompt_NoPriorVerdict_NoOracleFeedbackSection: cycle 1's
// continuation (or any cycle with no verdict recorded yet) must not
// fabricate an "[oracle, last cycle]" section out of a nil Last.
func TestNextWorkPrompt_NoPriorVerdict_NoOracleFeedbackSection(t *testing.T) {
	l := domain.Loop{Cycle: 1, Last: nil}
	contract := registry.Record{Goal: "ship it"}
	got := NextWorkPrompt(l, contract)
	if strings.Contains(got, "[oracle, last cycle]") {
		t.Errorf("got %q, want no oracle-feedback section with no prior verdict", got)
	}
}

// TestNextWorkPrompt_PriorRejectedVerdict_FeedsReasonBack is the corrective-
// signal path the design doc calls for: a rejected/progress verdict's
// Reason is fed back into the next cycle's prompt, mirroring the manual
// DRIFT re-drive's composeDriftPrompt pattern generalized to the oracle's
// own words (the engine has no human operator to type a hint).
func TestNextWorkPrompt_PriorRejectedVerdict_FeedsReasonBack(t *testing.T) {
	l := domain.Loop{
		Cycle: 3,
		Last:  &domain.Verdict{Outcome: domain.OutcomeRejected, Reason: "no evidence of a fresh test run", AtCycle: 2},
	}
	contract := registry.Record{Goal: "fix the flaky tests"}
	got := NextWorkPrompt(l, contract)
	if !strings.Contains(got, "[oracle, last cycle] no evidence of a fresh test run") {
		t.Errorf("got %q, want the prior verdict's reason fed back verbatim", got)
	}
}

// TestNextWorkPrompt_InstructsDoneOnlyWithEvidence: the composed prompt
// must always tell the agent DONE requires evidence and will be
// independently re-judged — the same discipline buildSpawnPrompt's cycle-1
// prompt already establishes, carried through every later cycle too.
func TestNextWorkPrompt_InstructsDoneOnlyWithEvidence(t *testing.T) {
	l := domain.Loop{Cycle: 5}
	contract := registry.Record{Goal: "ship it"}
	got := NextWorkPrompt(l, contract)
	if !strings.Contains(strings.ToLower(got), "done only when") {
		t.Errorf("got %q, want an explicit 'DONE only when the complete condition is met' instruction", got)
	}
	if !strings.Contains(strings.ToLower(got), "independent oracle") {
		t.Errorf("got %q, want a reminder that an independent oracle re-judges the claim", got)
	}
}
