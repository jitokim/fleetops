# Design — LoopEngine MVP (the "cockpit → runner" graduation)

> Seed spec. Status: draft for PR-flow implementation. Grounded in the code as of
> 2026-07. Companion to `VISION.md` §Phase 0 (single-process MVP) and `DESIGN.md`
> §0–§4. **This does not change the observation pipeline — it adds one automated
> presser of the same button a human already presses (`r`/`i` → `control.Redrive`).**

## 0. Thesis (what the MVP proves, and its one hard constraint)

Today missionctl *observes* real `claude` sessions and lets a human drive one cycle
per keystroke. The engine graduates exactly one thing: **missionctl itself fires the
next cycle**, under the governance layer that already exists (oracle → governor →
gate). The MVP is the smallest slice that proves "missionctl owns a governed cycle."

The load-bearing reuse insight: **a cycle is what the `r` key already does.**
`control.Redrive(sessionID, prompt)` = `claude --resume <id> -p "<prompt>"` = one
headless turn against the same transcript (verified, `actuation.go`). The engine is
"auto-redrive-429, generalized from *on a 429* to *on idle-and-not-yet-done*." So the
engine adds a **policy over the existing scan output**, not a parallel runtime.

**Hard constraint — fail closed.** The engine drives *only* from `StateIdle` (a
cleanly finished turn, `stop_reason:end_turn`). Every non-idle state — `StateGate`,
`StateRunning`, `StateStalled` — blocks driving *by construction*. A permission gate
mid-turn is never `StateIdle`, therefore the engine structurally cannot drive past it;
it halts and waits for a human `a` (approve). The engine never auto-approves.

## 1. Where engine state lives (#1)

**The scanner (`claude.DiscoverLoops`) stays the SOLE owner of `domain.Loop.State`.**
The engine owns no State field and computes no parallel state machine. There is one
State, derived one way every 3s from (transcript tail + registry + governor) — so
"reconcile observed vs engine-driven" is trivial *by construction*: the engine is a
**consumer** of State, not a second producer. Ownership splits cleanly:

| Concern | Owner | Location | Lifetime |
|---|---|---|---|
| `Loop.State` (running/idle/gate/done/failed…) | scanner | derived in `DiscoverLoops` | rebuilt each scan |
| Contract (goal/doneWhen/oracle/maxCycles) | registry | `~/.missionctl/loops/<sid>.json` | durable |
| Verdict + NoImprove counter | registry | same record | durable |
| **`Driven bool`** — "this session is engine-owned" | registry | **new field** on `Record` | durable |
| **drive-in-flight guard** | Model | reuse existing `m.actuating[sid]` | in-memory |

The per-cycle FSM (§2) is **not stored anywhere** — it is derived each scan from
(State × verdict × governor decision × `Driven` × `m.actuating`). This is the same
stateless-rebuild philosophy `DESIGN.md §2` already relies on; a missionctl restart
loses nothing (the contract + `Driven` flag are on disk; State re-derives).

## 2. The per-cycle state machine (#2)

The "engine's view" is a lens over scanner State, not a stored enum. Transitions fire
on two message types only: **`loopsMsg`** (a scan landed) and **`resumeResultMsg`** (a
Redrive returned — reused, see §4). Everything else already happens in the pipeline.

| From (engine lens) | Trigger / condition | Existing fn that fires | To |
|---|---|---|---|
| — | operator `N` (engine-drive spawn) confirms wizard | `bootstrapDrivenCmd`: `claude -p --output-format json` → parse `session_id` → `registry.Bind` + set `Driven` | Driving (cycle 1) |
| Driving | `resumeResultMsg{ok}` | (clears `m.actuating`) | Observing |
| Driving | `resumeResultMsg{err}` | log `TriggerEngine` event, set `Note` | Observing (retry next tick) |
| Observing | scan: `StateRunning` | — (turn in progress) | Observing |
| Observing | scan: **`StateGate`** | — (**HALT — never drive**) | **Gated** |
| Observing | scan: `StateStalled` | — (surface to human; no auto-recover) | Stalled (human owns) |
| Observing | scan: `StateIdle`, verdict stale | `triggerJudgments`→`judgeCmd` (**unchanged**) | Judging |
| Judging | `verdictMsg` | `registry.SaveVerdict` (**unchanged**, updates NoImprove) | Deciding |
| Deciding | `verdict==done` | scanner promotes `StateDone` (§3 precedence) | **Done** (terminal) |
| Deciding | `governor.Check==Stop` (NoImprove≥limit) | scanner promotes `StateFailed` | **Failed** (terminal) |
| Deciding | `governor.Check==Escalate` (budget/maxCycles) | scanner sets amber `Note` | **Gated** (escalation) |
| Deciding | `Continue` & verdict==progress | `triggerDrives`→`driveCmd`→`Redrive` (**new, thin**) | Driving (cycle+1) |
| Deciding | verdict==rejected | scanner promotes `StateDrift` (unchanged, `DESIGN.md` §3) | **Drift** (halt — human owns; the engine does not auto-drive a loop the oracle just rejected) |
| Gated | human `a` (`approveCmd`, unchanged) | — | Observing |
| Gated(escalation) | human: `k` kill / new contract | — | terminal |

**Fail-closed gate design.** No new gate mechanism is added. The scanner already
classifies a Claude Code permission prompt / `AskUserQuestion` as `StateGate`, and
`StateGate` outranks governor/verdict/tail (`DESIGN.md §3` precedence
`kill>gate>gone>verdict>governor>tail`). The engine's drive predicate is simply
`State == StateIdle`; `StateGate != StateIdle`, so a gate halts the loop with zero
extra code. Resume requires a human `a`; only after the next scan shows the turn
advanced can the engine consider driving again. The engine has **no approve path.**

## 3. Bootstrap: headless `claude -p`, not orca-spawn (#4 decision)

`claude -p "<contract>" --output-format json` emits `session_id` in its envelope (same
JSON shape `oracle.parseVerdict` already unwraps; `session_id` documented in the ADR's
hook payload). So bootstrap = run the contract as turn 1, **read `session_id` back
synchronously**, `Bind`+mark `Driven`. Chosen over the `spawnCmd`+`BindPending` dance:

- **Deterministic binding** — no cwd/timestamp matching race (`BindPending` is a
  best-effort proxy); we get the exact `session_id` from turn 1's output.
- **True single-process ownership** — no orca window, works on a bare host, matches
  VISION Phase 0. Provenance is unambiguous: an engine loop has no terminal surface.
- **Reuses `buildSpawnPrompt`** verbatim for the contract text (one document, told to
  the agent and judged by the oracle — the invariant `buildSpawnPrompt`'s doc protects).

**Cost of this choice (must be paid): the dormancy exception.** Between cycles a
headless loop has *no live process* — and `applyLiveness` currently drops `StateIdle` +
gone as "ended cleanly" (`scan.go:243`). Without a fix the loop *vanishes from the
fleet between cycles.* Required scanner change (~6 lines): **a `Driven` loop that is
`StateIdle`+gone is held as Idle (dormant, awaiting next drive), not dropped** — bounded
by a staleness guard: if a `Driven` loop's `Cycle` hasn't advanced within
`drivenDormantStale` (e.g. 15m) and no process is live, fall through to
`StateStalled`/`StallGone` so a truly dead engine loop still surfaces.

Alternative (deferred): orca-spawn bootstrap keeps an interactive process alive as a
liveness anchor and gives an attachable window — but it needs a terminal backend, and
the anchor process doesn't reflect headless turns. Offer it later as an opt-in.

## 4. Reuse / adapter / new (#4)

**Called as-is (zero change):** `engine.Check`, `oracle.Judge`, `control.Redrive`,
`registry.SaveVerdict`/`Load`, `claude.LastAssistantTextFull`/`SessionMetrics`
(cycles auto-increment: each Redrive appends a user turn → `SessionMetrics` counts it),
`triggerJudgments`+`judgeCmd`, `approveCmd`, gate classification, `DESIGN §3` precedence.

**Thin adapter:**
- `driveCmd(l, prompt)` — wraps `control.Redrive`, emits a `TriggerEngine`/`ActorAuto`
  event, **reuses `resumeResultMsg` + the existing `m.actuating` interlock** (exactly as
  `autoRedrive429Cmd` already does). No new message type, no new guard map.
- `killCmd` — for a `Driven` loop, skip the Tier-1 `/exit` (no terminal exists);
  instead clear `Driven` + emit a kill event. Killing = "stop scheduling the next drive."
- `registry.Bind` — set the new `Driven` field.

**Genuinely new (target < 400 lines net):**

| Unit | Where | ~LOC | Pure? |
|---|---|---|---|
| `NextWorkPrompt(loop) string` (compose next cycle's prompt from contract + last verdict reason) | `engine/driver.go` | 40 | ✅ |
| `ShouldDrive(loop, inFlight bool) (bool, reason)` (the Deciding predicate) | `engine/driver.go` | 40 | ✅ |
| `Driven` field + `MarkDriven` | `registry` | 25 | — |
| `TriggerEngine` const | `events` | 2 | — |
| `bootstrapDrivenCmd` (`claude -p` + parse session_id + Bind) | `tui` | 70 | — |
| `triggerDrives()` + `driveCmd` (mirror `triggerJudgments`) | `tui` | 90 | — |
| dormancy exception + staleness guard | `claude/scan.go` | 20 | — |
| `N` key wiring (reuse wizard state) + DRIVEN badge/DETAIL row + kill adapter | `tui` | 70 | — |

**~355 lines net + tests.** The two `engine/driver.go` pure functions carry the real
logic and are 100%-unit-testable with no exec/TUI.

## 5. The tea.Cmd flow (#3 concurrency)

A cycle is off-loop, exactly like `judgeCmd`/`resumeCmd` — never a blocking goroutine:

```
tickMsg → scan (tea.Cmd) → loopsMsg
  └─ Update(loopsMsg):  m.triggerJudgments()   // unchanged: idle+stale-verdict → judgeCmd
                        m.triggerDrives()        // NEW: for each Driven loop, ShouldDrive?
                            └─ ShouldDrive == true  → setActuating(sid); driveCmd(sid, NextWorkPrompt)
driveCmd (tea.Cmd, off-loop):  Redrive(sid, prompt)  → resumeResultMsg{sid,ok}
  └─ Update(resumeResultMsg):  delete(m.actuating,sid)   // existing handler, reused verbatim
verdictMsg / gitStatsMsg / detailCacheMsg … unchanged
```

**Concurrency seams — the guard is already built.** Everything runs on Bubble Tea's
single Update goroutine; only `tea.Cmd`s touch exec. The collision the prompt names —
an engine cycle and a manual `r` firing two concurrent `claude --resume` turns on the
same session — is **already interlocked**: `m.actuating[sid]` is set at the `r`/`i`
dispatch and (per the existing `autoRedriveScheduledMsg` handler) auto-drivers *check it
before firing and skip if set*. The engine **joins the same interlock** — so a manual
`r` in flight blocks the engine's drive and vice-versa, with the code that already
ships. `triggerDrives` also skips any loop with `m.actuating[sid]` set, so a slow
10-minute Redrive can't pile up across 3s ticks (same shape as `m.judging`).

## 6. `ShouldDrive` — the one new decision (pure)

```
ShouldDrive(l, inFlight) → drive when ALL hold:
  l.Goal.Driven                                  // engine-owned
  && !inFlight (m.actuating[l.SessionID])        // no Redrive already running
  && l.State == StateIdle                        // fail-closed: gate/running/stalled block
  && l.Last != nil && l.Last.AtCycle == l.Cycle  // THIS cycle is judged (verdict fresh)
  && l.Last.Outcome != OutcomeDone               // not converged
  && engine.Check(l).Action == Continue           // governor: budget/maxCycles/no-improve OK
```

Note the guard `AtCycle == Cycle`: the engine waits for the *current* cycle's oracle
verdict before driving the next — it never races ahead of the judge. A **rejected**
verdict never reaches this predicate as `StateIdle` in the first place — the scanner
promotes it to `StateDrift` first (§2's `Deciding` row), so `l.State == StateIdle`
alone already excludes it; `l.Last.Outcome != OutcomeDone` is listed for readability
but is structurally redundant given that wiring (see `engine/driver.go`'s doc for the
one case — `OutcomeDone` — where this redundancy is load-bearing to spell out).
`NextWorkPrompt` feeds the last verdict's `Reason` back to the agent ("oracle noted:
<reason> — address it and continue"), but since only a **progress** verdict ever
reaches a drive, that feedback is always a progress note, never a rejection
correction — mirroring the manual `composeDriftPrompt` pattern is deferred to the
"autonomous drive-through-rejection" follow-up (§8), not part of this MVP.

## 7. Demo acceptance (#5): flaky-tests-until-green

Contract entered via the `N` wizard:

- **goal:** `Fix the flaky tests in <pkg> so the suite passes reliably.`
- **doneWhen:** `A fresh run of \`<test cmd> -count=3\` passes with zero failures.`
- **oracle:** `The agent must RERUN the tests from scratch and show the fresh output; DONE only if that rerun is green. Do not accept a bare "fixed" claim.`
- **maxCycles:** 8 · NoImproveLimit: 3 (default).

**Termination — the AUTONOMOUS engine (no human re-drives) terminates in exactly one
of DONE / GATE(escalation) / GATE(permission) / DRIFT(halt-for-human) — never
autonomous FAILED, and never runs unbounded:**
- **DONE** — oracle `Outcome==done` (transcript shows a fresh green rerun) → `StateDone`, engine stops. ✅ pass.
- **DRIFT (halt for human)** — oracle `Outcome==rejected` (agent claims done without a
  fresh rerun, or the oracle otherwise pushes back) → `StateDrift` → the engine does
  NOT auto-drive it (§2's `Deciding` row; §8 non-goal). This is the conservative,
  fail-closed choice for the MVP: an agent lying about "done" is exactly when a human
  should own the call, not when the engine should auto-re-drive it up to 3×.
- **ESCALATE→GATE** — `Cycle≥8` → `governor.Escalate` → amber Note, engine halts for a human.
- **GATE** — the agent hits a permission prompt (e.g. a command needing approval) → `StateGate` → engine halts; human `a`.

**FAILED is semi-assisted, not autonomous.** `NoImprove≥3` → `governor.Stop` →
`StateFailed` is still real (unchanged governor logic, DESIGN.md §3) — the classic
flaky/lying signature — but the engine itself never drives toward it: it only fires
from `StateIdle`, and a rejected verdict never returns a loop to `StateIdle` without a
human's `r`/`i` re-drive out of `DRIFT`. So `StateFailed` is only reachable if a human
keeps manually re-driving a drifted loop and the oracle keeps rejecting it — the
autonomous demo run by itself cannot land here.

**Honest scope of "independent":** MVP's independence is the **oracle judging the
transcript evidence of a fresh rerun** — the oracle (haiku) does not itself execute the
tests. A challenger that *runs the rerun in a clean checkout* is the strictly stronger
guarantee and is explicitly deferred (§8).

## 8. Non-goals (#6 — explicitly deferred)

1. **Challenger execution** — stored (`Goal.Challenger`) but never run; no clean-checkout re-execution of the oracle's pass.
2. **The headless daemon** — engine runs in-process in the TUI (VISION Phase 0). No detached runner, no cross-process engine coordination.
3. **Multi-turn context compaction** — each cycle is one `--resume` turn; no summarization/window management when the transcript grows.
4. **Per-loop model / permission config** — cycles use the session's default model & permissions; no per-loop `--model`/`--permission-mode`.
5. **Stall auto-recovery** — a `Driven` loop that goes `StateStalled` (429 / no-output) surfaces to the human; the engine does not auto-redrive stalls (429 auto-redrive stays the separate opt-in it is).
6. **Worktree isolation for engine loops** — headless bootstrap spawns in the target cwd; worktree-isolated engine spawn is later.
7. **Engine self-chaining** — the MVP drives via the 3s scan policy (a few ticks' latency per cycle); a direct drive→judge→drive chain to cut latency is a later optimization.
8. **Autonomous drive-through-rejection** — the engine re-driving a `DRIFT`/rejected loop with the oracle's corrective feedback, up to `NoImprove`→`FAILED`, is DEFERRED to a follow-up slice with its own dedicated fail-closed review. Auto-driving a loop the oracle just rejected is the "lying agent" runaway path (an agent that repeatedly claims done and gets overruled) and needs its own safety analysis before it's in scope — the MVP's answer for now is: a rejection halts at `DRIFT` for a human to own.

## 9. Risks & failure modes (#7)

| Risk | Mitigation (mostly already in place) |
|---|---|
| Runaway token burn | `governor.Check` `BudgetTokens` ceiling (`TokensSpent` from `SessionMetrics`) + `MaxCycles`; engine drives only on `Continue`. |
| A turn that never ends | Engine drives only from `StateIdle`, so a stuck `StateRunning` never triggers a drive (no pile-up); `Redrive` itself is bounded by a 10-min ctx. A genuinely hung turn needs human `p` (interrupt) — surfaced, not auto-handled. |
| Oracle cost per cycle | One haiku `claude -p` per idle cycle (existing `judgeCmd` cost); bounded by `MaxCycles`. Called out as a real per-cycle spend. |
| Engine outliving a killed session | `k` clears `Driven`; `ShouldDrive` requires `Driven`; `StateKilled` is terminal. No persistent process to orphan (cycles are transient subprocesses, ctx-bounded). |
| Dormancy exception masks a dead loop | `drivenDormantStale` (15m) staleness guard falls a non-advancing dormant loop through to `StateStalled`/`StallGone`. |
| Double-drive across 3s ticks | `m.actuating[sid]` interlock (shared with manual `r`/`i` and 429 auto-redrive) — a 10-min Redrive can't be double-fired. |
| Bootstrap can't capture `session_id` | If the `-p` envelope lacks `session_id` (version skew), fall back to the `BindPending` cwd-match path; log and surface, don't silently lose the loop. |

## 10. Build order (each slice independently mergeable; Slice 0 is first)

- **Slice 0 — pure decision core.** `engine/driver.go`: `NextWorkPrompt` + `ShouldDrive` + table-driven tests. No wiring. Proves the FSM logic in isolation. **← first mergeable PR.**
- **Slice 1 — durable ownership.** `registry.Driven` field + `MarkDriven` + `Bind` sets it; `events.TriggerEngine` const. Tests for round-trip.
- **Slice 2 — bootstrap.** `bootstrapDrivenCmd` + `N` key (reuses the existing wizard state). Creates an engine loop that runs cycle 1 then rests. Manually verifiable.
- **Slice 3 — the cycle.** `triggerDrives` in the `loopsMsg` handler + `driveCmd` + the scanner dormancy exception. Now it loops end-to-end. Demo (§7) runs here.
- **Slice 4 — provenance + kill.** DRIVEN badge / DETAIL row (observed vs engine-driven) + `killCmd` driven-loop adapter.

## 11. Acceptance criteria

- [ ] `ShouldDrive`/`NextWorkPrompt` unit-tested incl. every fail-closed edge (gate/running/stalled/unjudged/done/budget/no-improve all return `drive=false`).
- [ ] A gate mid-cycle halts the engine; no drive fires until a human `a`. (never auto-approves.)
- [ ] `flaky-tests` demo terminates in exactly one of DONE / GATE(escalation) / GATE(permission) / DRIFT(halt-for-human) autonomously — never runs unbounded. (`FAILED` is semi-assisted only, see §7 — not reachable by the autonomous demo run by itself.)
- [ ] A manual `r` and an engine cycle never produce two concurrent `--resume` turns on one session (interlock test).
- [ ] A driven loop stays visible in the fleet between cycles (dormancy), and a dead one surfaces within `drivenDormantStale`.
- [ ] Every cycle writes a `TriggerEngine`/`ActorAuto` event; the loop renders with the DRIVEN provenance marker.
- [ ] Net new code < 400 lines; no change to the observation pipeline's State ownership.
</content>
</invoke>
