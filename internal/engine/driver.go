// driver.go is LoopEngine MVP Slice 1 (docs/design-loop-engine-mvp.md's
// "Slice 0 — pure decision core" in the design doc's own numbering; the
// seed spec at docs/specs/seed-loop-engine-mvp-2026-07-17.md locks the
// slice ACs under the coordinator's 1-based "Slice 1" label — same content,
// different number, noted here so the two docs don't read as disagreeing).
//
// This file is the engine's entire decision core for this slice: two pure
// functions, zero I/O, zero TUI/scan wiring. `ShouldDrive` decides WHETHER
// to fire the next cycle; `NextWorkPrompt` composes WHAT to send. Both are
// consumed by later slices (`triggerDrives`/`driveCmd` in `internal/tui`,
// per the design's §5 tea.Cmd flow) but reused verbatim, not reimplemented
// — this is deliberately the one place the drive logic lives.
package engine

import (
	"fmt"
	"strings"

	"github.com/jitokim/missionctl/internal/domain"
	"github.com/jitokim/missionctl/internal/registry"
)

// ShouldDrive is the engine's fail-closed drive predicate (design doc §6 /
// §2's "Deciding" row; seed spec AC "ShouldDrive/NextWorkPrompt unit-tested
// incl. every fail-closed edge"). Returns true ONLY when EVERY one of these
// holds — each computed as its own named value below so the fail-closed
// heart of this function reads as a documented checklist, not an opaque
// boolean expression:
//
//   - driven: this session is engine-owned right now. Taken as an EXPLICIT
//     parameter rather than read off l.Driven — at this slice, Loop.Driven
//     exists (see its doc) but nothing populates it yet. Keeping ShouldDrive
//     agnostic to WHERE "driven" comes from means the later slice that
//     wires the real source (registry.Record.Driven, copied onto Loop the
//     same way BoundAt already is) never has to touch this function. This
//     clause is ALSO the captain-mandated attach-preservation AC's pause,
//     for free: a human attaching to an engine-driven loop take-overs by
//     clearing Driven to false, and ShouldDrive already refuses to drive
//     whenever driven==false — no additional code needed in the later
//     slice that implements take-over for "a human took the wheel, so the
//     engine must stop driving."
//   - !inFlight: no Redrive is already running for this session — the SAME
//     m.actuating[sessionID] interlock a manual r/i keypress and the 429
//     auto-redrive policy already share (design §5). Without this, a slow
//     in-flight cycle could be double-fired across 3s scan ticks.
//   - idle (l.State == StateIdle): the fail-closed heart. A cleanly
//     finished turn is the ONLY state the engine may drive from. This
//     single check structurally rules out StateRunning (a turn is still in
//     flight — driving here would pile up concurrent turns), StateStalled
//     (surfaced to the human, no auto-recovery here — that stays 429
//     auto-redrive's separate opt-in), and — critically — StateGate: a
//     live permission prompt / AskUserQuestion is NEVER StateIdle, so the
//     engine structurally cannot drive past one. It halts and waits for a
//     human `a` (approve). The engine has NO approve path, by construction.
//   - notTerminal (!l.State.Terminal(), i.e. not Done/Failed/Killed): kept
//     as an EXPLICIT clause even though `idle` already excludes every
//     terminal value by construction (LoopState is one enum — a Loop is
//     never simultaneously StateIdle and StateDone). The fail-closed design
//     calls for saying "and never drive a finished loop" out loud in the
//     code, not relying on a reader noticing the implication.
//   - notGated (l.State != StateGate): same explicit-redundancy reasoning —
//     already implied by `idle`, stated separately because "a gate never
//     drives" is the single most safety-critical clause in this function
//     and must be readable on its own, not inferred from a different check.
//   - governorOK (engine.Check(l).Action == Continue): the governor's
//     existing hard ceilings (budget / max-cycles / no-improve —
//     DESIGN.md §3, unchanged, called as-is). Escalate (budget exhausted /
//     max cycles reached) does NOT drive either — it surfaces as a gate for
//     a human, same posture as an actual StateGate; only a clean Continue
//     authorizes the next cycle. Stop (no-improve ceiling hit) obviously
//     doesn't drive — the scanner will promote StateFailed on the next
//     scan pass, which the `idle`/`notTerminal` clauses above then cover.
//   - verdictFresh (l.Last != nil && l.Last.AtCycle == l.Cycle): design
//     doc §6's exact pseudocode clause, ADDED in feat/engine-cycle (Slice
//     2 shipped ShouldDrive without it, reasoning it was "structurally
//     subsumed by idle" — that reasoning covered the OutcomeDone case
//     correctly, per the note below, but missed a real race: the FIRST
//     scan tick after a cycle finishes, the loop is StateIdle with an
//     UNJUDGED or STALE verdict (Last.AtCycle < Cycle) — enrichFromRegistry
//     has nothing fresh to promote State from yet, so `idle` alone does
//     NOT exclude this case. Without this clause, triggerDrives could fire
//     cycle N+1 on the SAME tick triggerJudgments dispatches cycle N's
//     judgment — racing ahead of the judge, exactly what design §6 says
//     the engine must never do ("it never races ahead of the judge").
//     This clause closes that gap: no verdict yet, or a verdict from an
//     OLDER cycle, both mean wait.
//
// Still not checked here (deliberately — see the seed spec's open-question
// note, now narrowed to this alone): Last.Outcome != OutcomeDone, present
// in the design doc's §6 pseudocode as a SEPARATE clause. This one IS
// structurally subsumed by `idle`, given the wiring the later slices
// commit to: enrichFromRegistry promotes State to StateDone the SAME scan
// pass a FRESH done-verdict lands (internal/claude/scan.go), strictly
// BEFORE triggerDrives ever runs (design §5: triggerDrives fires inside
// Update(loopsMsg), the same handler that already ran that scan's
// enrichFromRegistry) — so a converged loop is never observed as StateIdle
// by the time ShouldDrive is called; `idle` alone already excludes it.
// This depends on triggerDrives being invoked exactly once per scan tick,
// after enrichFromRegistry, same as triggerJudgments already is. If a
// later slice's wiring ever violates that ordering, this reasoning — and
// this function's contract — needs revisiting.
func ShouldDrive(l domain.Loop, driven bool, inFlight bool) bool {
	inflightOK := !inFlight
	idle := l.State == domain.StateIdle
	notTerminal := !l.State.Terminal()
	notGated := l.State != domain.StateGate
	governorOK := Check(l).Action == Continue
	verdictFresh := l.Last != nil && l.Last.AtCycle == l.Cycle

	return driven && inflightOK && idle && notTerminal && notGated && governorOK && verdictFresh
}

// NextWorkPrompt composes the work prompt for l's NEXT cycle from contract
// (goal / doneCondition / rubric — the exact contract fields
// internal/tui's buildSpawnPrompt already uses to compose cycle 1's prompt,
// same defaults for an empty doneCondition/rubric) plus l's current cycle
// number, and — when available — the most recent oracle verdict's reason
// fed back as a progress note.
//
// feat/engine-cycle correction (was misleading before this comment fix): the
// reason-feedback line below can ONLY ever carry a *progress* verdict's
// note, never a rejection correction. ShouldDrive only calls this from
// l.State == StateIdle, and a REJECTED verdict is promoted to StateDrift by
// the scanner before the next scan's triggerDrives ever runs (§2's
// "Deciding" row in the design doc) — so a rejected loop is never StateIdle
// when NextWorkPrompt is composed for it; it halts at DRIFT for a human
// instead. The superficial resemblance to the manual DRIFT re-drive's
// composeDriftPrompt pattern (internal/tui/model.go: "<original> \n\n
// [operator correction] <hint>") is that: a resemblance, not the same
// mechanism — composeDriftPrompt fires on a human's re-drive OUT of DRIFT,
// which this function is never involved in. Autonomous drive-through-
// rejection (the engine re-driving a DRIFT loop itself) is explicitly
// deferred (design doc §8) pending its own fail-closed review.
//
// feat/panel-info (precise rename): the contract field and this function's
// composed prompt line are both "rubric" now, not "oracle" — "oracle"
// means exclusively the judge/verdict from here on (see domain.Goal's
// doc). The "[oracle, last cycle]"/"independent oracle" lines below are
// UNCHANGED — those really are about the judge, not the criteria.
//
// Pure string composition — no I/O, no registry/exec access. contract is
// passed in explicitly (not looked up inside this function) so it stays
// independently unit-testable with a hand-built registry.Record, matching
// engine.Check's own shape (a Loop value in, a decision out, nothing else).
func NextWorkPrompt(l domain.Loop, contract registry.Record) string {
	done := contract.DoneCondition
	if done == "" {
		done = "you judge the goal fully achieved"
	}
	rubricLine := contract.Rubric
	if rubricLine == "" {
		rubricLine = "an independent LLM judge verifies against the complete condition"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "goal: %s\n", contract.Goal)
	fmt.Fprintf(&b, "complete condition: %s\n", done)
	fmt.Fprintf(&b, "rubric: %s\n", rubricLine)
	fmt.Fprintf(&b, "cycle: %d\n", l.Cycle)
	b.WriteString("\nContinue working toward the goal. Report progress concretely this cycle.\n")
	if l.Last != nil && l.Last.Reason != "" {
		// Always a PROGRESS verdict's note here — see the doc comment above:
		// a rejected verdict never reaches this function from StateIdle.
		fmt.Fprintf(&b, "\n[oracle, last cycle] %s\n", l.Last.Reason)
	}
	b.WriteString("Declare DONE only when the complete condition is met — state the evidence.\n")
	b.WriteString("An independent oracle will verify your claim against this contract; a bare \"done\" claim with no fresh evidence will be rejected.")
	return b.String()
}
