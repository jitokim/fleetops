"""The seams (see DESIGN.md §2). Everything that varies is a Protocol, injected
at the composition root. Pure — no concrete impls here."""

from __future__ import annotations

from typing import Protocol, runtime_checkable

from missionctl.domain.models import (
    AgentStep,
    Goal,
    GateRequest,
    LoopContext,
    LoopSnapshot,
    Refutation,
    Verdict,
)


@runtime_checkable
class Agent(Protocol):
    """The worker. A Claude Code CLI run, a raw LLM call, a shell command — anything
    that can take a goal + context and produce a step toward it."""

    async def step(self, goal: Goal, ctx: LoopContext) -> AgentStep: ...


@runtime_checkable
class Oracle(Protocol):
    """The independent verifier. MUST NOT have write access to the loop's world.
    Judges the agent's output against reality — never against the agent's claim.
    This is the only authority on whether a loop is 'done'."""

    async def verify(self, goal: Goal, step: AgentStep, world: object) -> Verdict: ...


@runtime_checkable
class Challenger(Protocol):
    """Optional adversary: given a passing verdict, actively try to refute it
    (defense in depth against a hallucinated 'pass')."""

    async def refute(self, goal: Goal, verdict: Verdict, world: object) -> Refutation: ...


@runtime_checkable
class GatePolicy(Protocol):
    """Which points in a loop require a human decision."""

    def gate_points(self) -> frozenset[str]: ...


@runtime_checkable
class GateSink(Protocol):
    """Where a pending gate is delivered and a decision is awaited (TUI now; Slack/push later)."""

    async def ask(self, request: GateRequest) -> str: ...  # returns "approve" | "redirect:<hint>" | "kill"


@runtime_checkable
class LoopStore(Protocol):
    """Restart-safe persistence of loop state."""

    def save(self, snap: LoopSnapshot) -> None: ...
    def load(self, loop_id: str) -> LoopSnapshot | None: ...
    def list(self) -> list[LoopSnapshot]: ...
