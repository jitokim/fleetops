"""Fake Agent / Oracle / GateSink so the engine runs end-to-end without an LLM.

Enough to demo the headline: the agent claims "done", the oracle independently
rejects it (drift), the loop keeps going, then genuinely converges. Real adapters
(LLM-via-Bifrost agent, test-runner oracle) land in Phase 0/1.
"""

from __future__ import annotations

import asyncio

from missionctl.domain.models import (
    AgentStep,
    Goal,
    GateRequest,
    LoopContext,
    Verdict,
)
from missionctl.domain.states import Outcome


class ScriptedAgent:
    """Claims 'done' from `claims_done_from` onward — so we can watch the oracle
    catch a premature claim."""

    def __init__(self, claims_done_from: int = 2, tokens_per_cycle: int = 9_000) -> None:
        self._from = claims_done_from
        self._tok = tokens_per_cycle

    async def step(self, goal: Goal, ctx: LoopContext) -> AgentStep:
        await asyncio.sleep(0)  # yield; real agent does work here
        claims = ctx.cycle >= self._from
        return AgentStep(
            summary=f"cycle {ctx.cycle}: worked on “{goal.text}”" + (" — done ✓" if claims else ""),
            claims_done=claims,
            tokens=self._tok,
        )


class ScriptedOracle:
    """Independently rejects a 'done' claim until `accepts_from`, then verifies it.
    A real oracle reruns tests / re-checks state with NO write access."""

    def __init__(self, accepts_from: int = 4, tokens_per_verify: int = 2_500) -> None:
        self._accepts = accepts_from
        self._tok = tokens_per_verify

    async def verify(self, goal: Goal, step: AgentStep, world: object) -> Verdict:
        await asyncio.sleep(0)
        if step.claims_done:
            if step_cycle(step) >= self._accepts:
                return Verdict(Outcome.DONE, "verified on independent rerun", self._tok)
            return Verdict(Outcome.REJECTED,
                           "agent claimed done — 3 checks still fail on clean rerun", self._tok)
        return Verdict(Outcome.PROGRESS, "moved forward", self._tok)


def step_cycle(step: AgentStep) -> int:
    # cheap: parse "cycle N" out of the summary (scaffold only)
    try:
        return int(step.summary.split("cycle", 1)[1].split(":", 1)[0])
    except Exception:
        return 0


class AutoApproveGate:
    """Dev gate sink: auto-answers so the engine can run unattended in tests/demos.
    The real sink is the TUI (keypress) or Slack."""

    def __init__(self, decision: str = "approve") -> None:
        self._decision = decision

    async def ask(self, request: GateRequest) -> str:
        await asyncio.sleep(0)
        return self._decision
