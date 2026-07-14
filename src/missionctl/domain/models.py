"""Value objects passed across the ports (see DESIGN.md §2). Pure data — no I/O."""

from __future__ import annotations

from dataclasses import dataclass, field, replace

from missionctl.domain.states import LoopState, Outcome


@dataclass(frozen=True, slots=True)
class Goal:
    """What a loop is trying to achieve, and its hard ceilings."""

    text: str
    max_cycles: int = 12
    budget_tokens: int = 150_000
    no_improve_limit: int = 3            # consecutive non-progress cycles before stop
    gate_points: frozenset[str] = frozenset({"on_done"})


@dataclass(frozen=True, slots=True)
class LoopContext:
    """What the agent sees at the start of a cycle."""

    cycle: int
    goal: Goal
    last_verdict: "Verdict | None" = None
    hint: str | None = None              # a human redirect, if any


@dataclass(frozen=True, slots=True)
class AgentStep:
    """What the agent produced this cycle — including its (untrusted) self-report."""

    summary: str
    claims_done: bool                    # the agent's belief — an input, never the decision
    tokens: int = 0
    artifacts: tuple[str, ...] = ()


@dataclass(frozen=True, slots=True)
class Verdict:
    """The oracle's independent judgment. The only authority on 'done'."""

    outcome: Outcome
    reason: str
    tokens: int = 0

    @property
    def passed(self) -> bool:
        return self.outcome in (Outcome.PROGRESS, Outcome.DONE)

    @property
    def done(self) -> bool:
        return self.outcome is Outcome.DONE


@dataclass(frozen=True, slots=True)
class Refutation:
    holds: bool          # True = the challenger found the "pass" is actually wrong
    reason: str = ""


@dataclass(frozen=True, slots=True)
class CycleRecord:
    cycle: int
    step: AgentStep
    verdict: Verdict


@dataclass(frozen=True, slots=True)
class GateRequest:
    """A loop asking a human to decide."""

    loop_id: str
    point: str           # e.g. "on_done", "before_pr", "escalate:no_improve"
    prompt: str


@dataclass(frozen=True, slots=True)
class LoopSnapshot:
    """The persisted, renderable state of one loop."""

    id: str
    name: str
    goal: Goal
    state: LoopState = LoopState.RUNNING
    cycle: int = 0
    tokens_spent: int = 0
    no_improve: int = 0
    last_verdict: Verdict | None = None
    gate: GateRequest | None = None
    history: tuple[CycleRecord, ...] = field(default_factory=tuple)

    @property
    def budget_frac(self) -> float:
        cap = self.goal.budget_tokens or 1
        return min(1.0, self.tokens_spent / cap)

    def with_(self, **changes) -> "LoopSnapshot":
        return replace(self, **changes)
