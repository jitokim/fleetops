# missionctl — design

> **Mission control for a fleet of autonomous agent loops.**
> You watch the *gates*, not the *work*. Loops run themselves; an independent
> **oracle** verifies that "done" is actually done; **governance** stops runaway
> loops; a **human gate** blocks at the decisions only you should make.

- status: `0.1` design draft
- audience: engineers running long-lived autonomous agent loops (the "I kicked off
  5 loops overnight and only want to approve merges" workflow)
- stack: Python 3.11+ · Textual (TUI) · asyncio (engine) · LLM calls via a
  pluggable gateway (route through Bifrost to dogfood it)

---

## 0. Why

Everyone is rolling their own agent-loop harness (the "auth in 2010" moment). The
hard part isn't running an agent — it's running *many* long loops **without them
lying, drifting, or running away**, while keeping a human on the decisions that
matter. missionctl packages exactly that: **oracle + challenger + governance +
human-gate**, with a terminal fleet console.

The novel 1% is not the loop. It is the **governance layer** around the loop:
- **oracle** — never trust the agent's self-declared "done"; verify independently.
- **challenger** — adversarially try to refute a "pass" before accepting it.
- **governor** — budget / max-cycles / no-improve → stop a runaway.
- **gate** — the loop blocks and asks a human at the points only a human should own.

## 1. Architecture — three layers, engine is headless

```
┌─ TUI (view · Textual) ─────────────────────────────┐  thin client. renders fleet
│   fleet table · detail pane · keybar               │  state, sends commands.
│   subscribes to engine events; holds no logic.     │  NO business logic here.
├─ LoopEngine (headless · asyncio) ──────────────────┤  ★ runs the loops.
│   per-loop convergent state machine (§3)           │  survives the TUI closing
│   agent · oracle · challenger · governor · gate    │  (overnight fleet).
├─ Store (persistence · JSON→SQLite) ────────────────┤  loop state + cycle history
│   restart-safe, reattachable                       │  + pending gates.
└─────────────────────────────────────────────────────┘
```

**Load-bearing:** the engine is independent of the TUI. Loops keep running when the
TUI is closed; the TUI (or Slack, later) just attaches to observe and to answer
gates. MVP runs both in one process; Phase 1 splits the engine into a daemon.

## 2. Pluggable ports (the seams)

Everything that varies is a Protocol, injected at the composition root — so
"which agent", "which oracle", "which gateway" are swappable, not baked in.

```python
class Agent(Protocol):
    # the worker. Claude Code CLI, a raw LLM call, a shell command — anything.
    async def step(self, goal: Goal, ctx: LoopContext) -> AgentStep: ...

class Oracle(Protocol):
    # independent verifier. MUST NOT have write access to the loop's world.
    # judges the agent's output against reality, not against the agent's claim.
    async def verify(self, goal: Goal, step: AgentStep, world: WorldState) -> Verdict: ...

class Challenger(Protocol):        # optional adversary (defense in depth)
    async def refute(self, goal: Goal, verdict: Verdict, world: WorldState) -> Refutation: ...

class GatePolicy(Protocol):
    def gate_points(self) -> frozenset[str]   # e.g. {"on_done", "before_pr"}

class LoopStore(Protocol):
    def save(self, snap: LoopSnapshot) -> None: ...
    def load(self, loop_id: str) -> LoopSnapshot | None: ...
    def list(self) -> list[LoopSnapshot]: ...
```

`Governor` is a pure function, not a port: `check(loop) -> Continue | Stop(reason) | Escalate`.

## 3. The loop state machine (per cycle)

```
states: RUNNING → GATE ⇄ RUNNING → DONE
                    ↘ DRIFT (oracle rejected) → RUNNING/ESCALATE
                    ↘ FAILED{reason}          (governor stop)
                    ↘ PAUSED / KILLED         (human)

async def run_cycle(loop):
    if loop.gate_pending:              # block until a human decides
        await loop.await_decision()    # approve → continue · redirect → new hint · kill
    world  = store.load_world(loop)
    step   = await agent.step(goal, ctx)          # the agent works (may claim "done ✓")
    verdict= await oracle.verify(goal, step, world)   # ← independent. self-report NOT trusted
    if verdict.passed and challenger:
        if challenger.refute(...).holds: verdict = REJECT    # adversarial confirm
    loop.record(step, verdict)                     # cycle history + tokens + budget
    match governor.check(loop):                    # budget / max-cycle / no-improve
        case Stop(r):     -> FAILED{r} (or ESCALATE = raise a gate)
        case Escalate:    -> raise gate
        case Continue:
            if verdict.done:
                -> gate("on_done") if policy else DONE   # LLM never self-declares LIVE/DONE
            elif verdict.needs_human:
                -> raise gate
            else:
                -> next cycle
```

Invariants:
- **the oracle is the only authority on "done".** The agent's belief is an input, never the decision.
- **budget & cycles are hard ceilings.** A loop cannot exceed them; it escalates or fails closed.
- **gates block.** A loop at a gate does no further work until a human answers.

## 4. TUI (the mockup)

`missionctl` → fleet console (`html-artifacts/mission-control-tui.html` is the target look):
- fleet summary band (loops · states · oracle pass-rate · budget · **gates-waiting**).
- loops table (name · state · cycle · oracle · budget · no-improve), selected row.
- detail pane for the selected loop (oracle / challenger / stage / gate actions).
- keybar: `↑↓` select · `↵` details · `a` approve · `r` redirect · `k` kill · `p` pause · `n` new · `/` filter · `q` quit.
- auto-refresh from engine state (~2s / event-driven).

## 5. Build order

- **Phase 0 (MVP, single process):** engine + `Governor` (real) + one `Agent` + one
  `Oracle` (fakes are fine to start — enough to demo *oracle catches a false "done"*)
  + Textual TUI = the mockup + gate-approve via keypress. Demo task: "fix the flaky
  tests until CI is green — but the oracle verifies green on an independent rerun."
- **Phase 1:** engine → daemon; TUI attaches; **gate delivery to Slack/phone** ("watch
  gates from your phone"). LLM agent/oracle routed through **Bifrost**.
- **Phase 2:** more agent adapters (Claude Code / Cursor headless), challenger, session
  replay (drill-down), loop config files, richer governance (circuit-break a flapping loop).

## 6. Open questions

- engine transport: in-process (MVP) → daemon over a unix socket / local HTTP?
- oracle cost: every cycle pays for an independent verify — cache / sample / cheap-model tier?
- gate delivery: TUI-only (MVP) → Slack / push / web. Same event, many sinks.
- state store: JSON files (MVP) → SQLite (fleet at scale).
