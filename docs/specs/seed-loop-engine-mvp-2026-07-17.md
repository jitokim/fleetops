---
status: locked
created: 2026-07-17
source: docs/design-loop-engine-mvp.md (design doc, status: draft for PR-flow implementation)
---

# Seed Spec: LoopEngine MVP (the "cockpit → runner" graduation)

> This is the review baseline for every engine slice (Slice 1–5, per the design doc's
> build order — see "Build order" below for the numbering note). Crystallizes the
> design doc's ACs into a locked, verifiable checklist. **Faithful to the design
> doc — this spec does not redesign anything the design doc decided.** Where this spec
> adds something the design doc didn't already state, it's called out explicitly as a
> **captain-mandated addition** (see "Attach preservation," below) or an **open
> question for reviewers** (see the note at the end) — never silently folded into the
> design doc's own ACs as if it had always been there.

## Addendum (2026-07-17, post-lock): opt-in spike — standing discipline for every slice

**Captain decision, issued after this spec's initial lock, during the durable-ownership
slice.** This section is itself locked and is now a review gate for EVERY remaining
engine slice — reproduce it (or link back here) in each slice's PR body:

1. **The observation cockpit stays first-class.** The engine must never degrade
   observed-loop behavior — the attach-preservation AC (above) is the concrete instance
   of this, but the principle is general: nothing about shipping the engine may make
   the existing cockpit worse, slower, or less trustworthy for a loop the engine
   doesn't own.
2. **The engine is reachable ONLY behind an explicit opt-in — BOTH of:**
   - an env gate, `MISSIONCTL_ENGINE=1` (`engineEnabledFn`, `internal/tui/model.go` —
     shipped in the durable-ownership slice as a seam, not yet called by anything); AND
   - the `n` wizard's engine-drive choice, per-loop (`registry.BindSpec.Driven` →
     `Record.Driven` → `domain.Loop.Driven`, shipped across Slice 0/1).

   **No engine cycle EVER fires unless a loop was explicitly created engine-driven —
   both gates must be true.** Off by default in every dimension: the env var, the
   per-loop choice, and (implicitly) every loop that existed before this feature.
3. **No "better runner" features.** Challenger execution, context compaction, per-loop
   model/permission config, worktree isolation, fancier prompts — all STAY non-goals
   (see "Non-goals" below, unchanged by this addendum). **The engine is a governance
   harness, not an agent runtime.** If a future slice starts adding runner polish on
   top of the governed-cycle mechanism, that slice's author (or reviewer) must stop and
   flag it — this is a standing tripwire, not a one-time checklist item.
4. **Kill-switch.** `MISSIONCTL_ENGINE` unset (or a future global disable) must make
   EVERY driven loop inert: it keeps rendering and being observed normally (State
   ownership is untouched either way — see "Where engine state lives" above), but the
   engine never fires a drive for it. This is the env-gate half of the two-gate opt-in
   in point 2 — the SAME mechanism, restated here as the specific failure-mode
   guarantee it must uphold.

## Goal

missionctl graduates from "observes a fleet and lets a human drive one cycle per
keystroke" to "missionctl itself fires the next cycle" — under the SAME governance
layer that already exists (oracle → governor → gate). The load-bearing reuse insight:
**a cycle is what the `r` key already does** (`control.Redrive` = `claude --resume <id>
-p "<prompt>"`, one headless turn against the same transcript). The engine is "auto-
redrive-429, generalized from *on a 429* to *on idle-and-not-yet-done*" — a policy over
the existing scan output, not a parallel runtime.

**Hard constraint (fail closed), locked verbatim from the design doc's Thesis (§0):**
the engine drives *only* from `StateIdle`. Every non-idle state (`StateGate`,
`StateRunning`, `StateStalled`) blocks driving *by construction*. A permission gate
mid-turn is never `StateIdle`; the engine structurally cannot drive past it — it halts
and waits for a human `a` (approve). **The engine never auto-approves.**

## Scope

- **In:** an in-process (single-binary, VISION Phase 0) engine that fires governed
  cycles for goal-bound loops the operator explicitly hands to it (via the `N` engine-
  drive spawn, a later slice). Bootstrap via headless `claude -p --output-format json`
  (session_id captured synchronously — **live-verified** on the captain's machine:
  counter 1→2→done, same session_id, transcript grows across `claude --resume <id> -p`
  calls). Reuses `engine.Check`, `oracle.Judge`, `control.Redrive`,
  `registry.SaveVerdict`/`Load`, gate classification, and `DESIGN.md §3`'s state
  precedence (`kill>gate>gone>verdict>governor>tail`) as-is.
- **Out:** see "Non-goals," below — pulled verbatim from the design doc §8.

## Where engine state lives (locked, design doc §1)

The scanner (`claude.DiscoverLoops`) stays the **sole owner** of `domain.Loop.State`.
The engine owns no State field and computes no parallel state machine — it is a
**consumer** of State, never a second producer.

| Concern | Owner | Location | Lifetime |
|---|---|---|---|
| `Loop.State` | scanner | derived in `DiscoverLoops` | rebuilt each scan |
| Contract (goal/doneWhen/oracle/maxCycles) | registry | `~/.missionctl/loops/<sid>.json` | durable |
| Verdict + NoImprove counter | registry | same record | durable |
| `Driven bool` — "this session is engine-owned" | registry (→ copied onto `Loop`, mirroring `BoundAt`) | new field | durable |
| drive-in-flight guard | Model | reuse existing `m.actuating[sid]` | in-memory |

The per-cycle FSM (design doc §2) is **not stored anywhere** — derived each scan from
`(State × verdict × governor decision × Driven × m.actuating)`. A missionctl restart
loses nothing: the contract + `Driven` flag are on disk, State re-derives.

## Acceptance Criteria

Crystallized from the design doc §11, kept faithful (same substance, made independently
verifiable per slice):

- [ ] **AC-1 (fail-closed drive predicate).** `ShouldDrive` and `NextWorkPrompt` are
      unit-tested including every fail-closed edge: gate, running, stalled, drift,
      terminal (done/failed/killed), not-driven, in-flight, and every governor outcome
      (continue/escalate/stop) all correctly gate driving. **Locked in Slice 1.**
- [ ] **AC-2 (gate halts, never auto-approved).** A permission gate mid-cycle halts the
      engine; no drive fires until a human presses `a`. The engine has no approve path,
      by construction (verified structurally in Slice 1 via `ShouldDrive`'s `StateGate`
      clause; verified end-to-end once wired in Slice 3).
- [ ] **AC-3 (bounded termination).** The `flaky-tests-until-green` demo (design doc
      §7), run autonomously (no human re-drives), terminates in exactly one of DONE /
      GATE(escalation) / GATE(permission) / DRIFT(halt-for-human) — never runs
      unbounded. **Autonomous `FAILED` is out of scope for this MVP** (design doc §7/§8):
      a rejected verdict halts at `DRIFT` for a human, not an auto-redrive; `FAILED` is
      reachable only semi-assisted, if a human keeps re-driving a drifted loop and the
      oracle keeps rejecting it (`NoImprove≥limit` → `governor.Stop`). Verified once
      Slice 3 wires the cycle end-to-end.
- [ ] **AC-4 (no concurrent turns on one session).** A manual `r`/`i` actuation and an
      engine cycle never produce two concurrent `--resume` turns against the same
      session — both join the existing `m.actuating[sessionID]` interlock (the SAME
      guard 429 auto-redrive already shares with manual actuation). Verified once
      Slice 3 wires `triggerDrives`; Slice 1 documents (but cannot yet test end-to-end)
      that `ShouldDrive`'s `inFlight` clause is the mechanism.
- [ ] **AC-5 (dormancy, not disappearance).** A driven loop stays visible in the fleet
      between cycles (a headless bootstrap loop has no live process between turns) —
      the scanner's dormancy exception, bounded by a `drivenDormantStale` staleness
      guard so a genuinely dead engine loop still surfaces as `StateStalled`/`StallGone`.
      Verified once the scanner change lands (design doc §3 — not part of Slice 1).
- [ ] **AC-6 (provenance).** Every engine-fired cycle writes a `TriggerEngine`/
      `ActorAuto` event; a driven loop renders with a DRIVEN provenance marker
      distinguishing it from an observed (human-run) loop. Verified once the TUI
      wiring lands (Slice 4).
- [ ] **AC-7 (budget).** Net new code across the whole MVP stays under ~400 lines
      (design doc §4's per-unit LOC table); no change to the observation pipeline's
      State ownership (the scanner stays the sole writer — reaffirms "Where engine
      state lives," above).

## Attach preservation (captain-mandated, top-level AC — review gate for EVERY slice)

**This AC did not originate in the design doc.** It was added by explicit captain
mandate after the design doc was written, and is locked here as a top-level AC that
every engine slice (1 through 4, and any future slice) must be reviewed against —
not just the slice that eventually implements the take-over action.

1. **Observed loops: `↵` attach must NEVER regress.** It continues to work EXACTLY as
   today — `control.Resolve()` → `Locate` (never the stricter `LocateClaude`) → `Focus`,
   jumping to the orca/tmux/cmux surface hosting the session. **No engine slice may
   touch `attachCmd`'s observed-loop behavior.** A regression test asserting this
   (`Locate` is called, never `LocateClaude`) is REQUIRED and is added in Slice 1
   (`TestAttachCmd_ObservedLoop_UsesLocateNotLocateClaude`) even though Slice 1 is
   otherwise pure-logic-only — this AC is important enough to pin immediately rather
   than wait for a slice that happens to touch `attachCmd` for engine reasons.
2. **Engine-driven loops (headless `claude -p`, no terminal surface): attach means
   TAKE-OVER.** Since there is no terminal surface to focus, `↵` on a `Driven` loop
   must:
   - Open `claude --resume <sessionID>` in a REAL terminal via the active backend
     (orca `terminal create --command "claude --resume <id>"` / tmux new-window), so
     the human takes the wheel interactively — never fail with "surface not found".
   - Clear the loop's `Driven` flag to `false` — the human now owns this session; the
     engine must not fire a cycle into a session a human is interactively driving.
   - Emit an event (`actor: human`, detail `"take-over"`).
   - Fall back to the manual hint (`claude --resume <id>`) exactly as attach does
     today, if no backend is available.
   - **This is a Slice-4-ish concern** (it needs the `Driven` flag wired, which Slice 2
     adds) — not implemented in Slice 1. It is locked here NOW so it is a review gate
     for every intervening slice's design choices.

**Confirmed for Slice 1 (no extra code needed for the pause half of this AC):**
`ShouldDrive`'s `driven` clause already makes "a human took over → the engine stops
driving" fall out for free — once a later slice's take-over action clears `Driven` to
`false`, `ShouldDrive(l, false, inFlight)` returns `false` unconditionally, with zero
additional logic. Only the take-over ACTION itself (opening a real terminal, clearing
the flag, emitting the event) is new work, deferred to the slice that implements it.
`ShouldDrive`'s doc comment (`internal/engine/driver.go`) states this explicitly so the
connection isn't lost between now and that slice.

## The one new decision: `ShouldDrive` (locked shape for Slice 1)

Per the captain's exact Slice 1 instruction (which takes priority over the design
doc's own §6 pseudocode where the two differ — see the "Open question for reviewers"
note at the end of this spec):

```
ShouldDrive(l domain.Loop, driven bool, inFlight bool) bool

drive when ALL hold:
  driven                                    // engine-owned right now (explicit param, not l.Driven — see driver.go)
  && !inFlight                              // no Redrive already running (m.actuating interlock)
  && l.State == StateIdle                   // fail-closed: gate/running/stalled/drift all block
  && !l.State.Terminal()                    // explicit, even though StateIdle already implies it
  && l.State != StateGate                   // explicit, even though StateIdle already implies it
  && engine.Check(l).Action == Continue      // governor: budget/maxCycles/no-improve all clear
```

`NextWorkPrompt(l domain.Loop, contract registry.Record) string` composes the next
cycle's prompt from the contract (goal/doneCondition/oracle — same fields
`buildSpawnPrompt` already uses for cycle 1, same defaults) plus the current cycle
number, feeding the prior verdict's `Reason` back as corrective signal when one exists
(mirrors the manual DRIFT re-drive's `composeDriftPrompt` pattern, generalized from an
operator's typed hint to the oracle's own words).

## Bootstrap decision (locked, design doc §3)

Headless `claude -p "<contract>" --output-format json`, reading `session_id` back
synchronously, over `spawnCmd`+`BindPending`'s cwd/timestamp matching — **live-verified**
by the captain: `session_id` returned synchronously, `claude --resume <id> -p` continues
the SAME session with context preserved across cycles. Cost that must be paid: the
scanner's dormancy exception (design doc §3) — a `Driven` loop that's `StateIdle`+gone
is held as dormant (not dropped), bounded by `drivenDormantStale` (~15m) so a truly dead
engine loop still surfaces. Not part of Slice 1.

## Non-goals (locked verbatim from design doc §8 — explicitly deferred, not forgotten)

1. **Challenger execution** — stored (`Goal.Challenger`) but never run; no clean-checkout
   re-execution of the oracle's pass.
2. **The headless daemon** — engine runs in-process in the TUI (VISION Phase 0). No
   detached runner, no cross-process engine coordination.
3. **Multi-turn context compaction** — each cycle is one `--resume` turn; no
   summarization/window management when the transcript grows.
4. **Per-loop model / permission config** — cycles use the session's default model &
   permissions; no per-loop `--model`/`--permission-mode`.
5. **Stall auto-recovery** — a `Driven` loop that goes `StateStalled` (429/no-output)
   surfaces to the human; the engine does not auto-redrive stalls (429 auto-redrive
   stays the separate opt-in it already is).
6. **Worktree isolation for engine loops** — headless bootstrap spawns in the target
   cwd; worktree-isolated engine spawn is later.
7. **Engine self-chaining** — the MVP drives via the 3s scan policy (a few ticks'
   latency per cycle); a direct drive→judge→drive chain to cut latency is a later
   optimization.

## Build order (design doc §10 — slice numbering note)

The design doc numbers its slices **0 through 4** ("Slice 0 — pure decision core" is
the design doc's first mergeable slice). The captain's task messages refer to this
same first slice as **"Slice 1."** Same content, different number — this spec uses the
captain's 1-based numbering going forward (Slice 1–4) and cross-references the design
doc's own 0-based numbering in parentheses where useful, so the two documents don't
read as disagreeing about scope, only about which integer labels it.

- **Slice 1** (design doc's "Slice 0"): pure decision core. `engine/driver.go`:
  `ShouldDrive` + `NextWorkPrompt` + table-driven tests. No wiring. Also locks the
  attach-preservation regression test (captain-mandated addition, see above) even
  though it isn't itself engine logic — cheap to pin now, high cost to regress later.
- **Slice 2**: durable ownership. `registry.Driven` field + `MarkDriven` + `Bind` sets
  it; `events.TriggerEngine` const. Tests for round-trip.
- **Slice 3**: bootstrap. `bootstrapDrivenCmd` + `N` key (reuses the existing wizard
  state). Creates an engine loop that runs cycle 1 then rests. Manually verifiable.
- **Slice 4**: the cycle. `triggerDrives` in the `loopsMsg` handler + `driveCmd` + the
  scanner dormancy exception. Now it loops end-to-end. The `flaky-tests` demo (design
  doc §7) runs here. AC-3/AC-4/AC-5 become verifiable here.
- **Slice 5**: provenance + kill + take-over. DRIVEN badge/DETAIL row + `killCmd`
  driven-loop adapter + the attach take-over action (part 2 of the attach-preservation
  AC). AC-6 and attach-preservation part 2 become verifiable here.

(The design doc's own §10 stops at its "Slice 4"; this spec adds one more slice
boundary — its "Slice 5" — purely to give the take-over action, which the design doc
didn't originally scope as its own slice, an explicit landing slot. This is the one
place this spec extends the design doc's structure rather than only crystallizing it;
flagged here rather than silently inserted.)

## Open question for reviewers (not a design decision — flagging, not resolving)

The design doc's §6 `ShouldDrive` pseudocode includes two clauses this spec's locked
Slice 1 shape (above, per the captain's exact instruction) does not:
`l.Last != nil && l.Last.AtCycle == l.Cycle` (verdict freshness) and
`l.Last.Outcome != OutcomeDone` (not converged).

`internal/engine/driver.go`'s doc comment argues both are structurally subsumed by
`l.State == StateIdle`, GIVEN that `triggerDrives` (Slice 4) is invoked exactly once
per scan tick, strictly after `enrichFromRegistry` promotes State to `StateDone` on a
fresh converged verdict — the same ordering `triggerJudgments` already relies on. If a
future slice's wiring ever calls `ShouldDrive` from anywhere else (a different message
handler, a manual trigger, etc.), that assumption needs re-verifying before trusting
this shape. Recorded here so it's checked at Slice 4 review time, not lost between now
and then.
