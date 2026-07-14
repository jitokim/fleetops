"""LoopEngine — the convergent state machine (see DESIGN.md §3).

Headless and view-agnostic: it runs loops and emits snapshots. The TUI (or any
other sink) subscribes. The oracle is the only authority on 'done'; the agent's
self-report is an input, never the decision.
"""

from __future__ import annotations

import asyncio
from typing import Awaitable, Callable

from missionctl.domain.models import (
    CycleRecord,
    GateRequest,
    LoopContext,
    LoopSnapshot,
)
from missionctl.domain.ports import Agent, GatePolicy, GateSink, LoopStore, Oracle
from missionctl.domain.states import LoopState, Outcome
from missionctl.engine import governor

OnUpdate = Callable[[LoopSnapshot], None]


class LoopRunner:
    """Drives one loop to a terminal state. Injected with its collaborators."""

    def __init__(
        self,
        *,
        agent: Agent,
        oracle: Oracle,
        gate_policy: GatePolicy,
        gate_sink: GateSink,
        store: LoopStore,
        on_update: OnUpdate | None = None,
    ) -> None:
        self._agent = agent
        self._oracle = oracle
        self._policy = gate_policy
        self._gate = gate_sink
        self._store = store
        self._on_update = on_update or (lambda s: None)

    def _emit(self, snap: LoopSnapshot) -> LoopSnapshot:
        self._store.save(snap)
        self._on_update(snap)
        return snap

    async def run(self, snap: LoopSnapshot) -> LoopSnapshot:
        while not snap.state.is_terminal:
            snap = await self._one_cycle(snap)
        return snap

    async def _one_cycle(self, snap: LoopSnapshot) -> LoopSnapshot:
        goal = snap.goal

        # 1) agent works (may claim done)
        snap = self._emit(snap.with_(state=LoopState.RUNNING))
        ctx = LoopContext(cycle=snap.cycle + 1, goal=goal, last_verdict=snap.last_verdict)
        step = await self._agent.step(goal, ctx)

        # 2) oracle verifies INDEPENDENTLY — self-report not trusted
        world = self._store  # placeholder world handle; real adapters read the FS/tests
        verdict = await self._oracle.verify(goal, step, world)

        # 3) record cycle, budget, no-improve
        progressed = verdict.outcome in (Outcome.PROGRESS, Outcome.DONE)
        snap = snap.with_(
            cycle=snap.cycle + 1,
            tokens_spent=snap.tokens_spent + step.tokens + verdict.tokens,
            no_improve=0 if progressed else snap.no_improve + 1,
            last_verdict=verdict,
            history=snap.history + (CycleRecord(snap.cycle + 1, step, verdict),),
        )

        # 4) drift: agent claimed done but oracle rejected
        if step.claims_done and verdict.outcome is Outcome.REJECTED:
            snap = self._emit(snap.with_(state=LoopState.DRIFT))

        # 5) governance — hard ceilings
        action = governor.check(snap)
        if isinstance(action, governor.Stop):
            # TODO: add failure_reason to LoopSnapshot to carry action.reason
            return self._emit(snap.with_(state=LoopState.FAILED, gate=None))
        if isinstance(action, governor.Escalate):
            return await self._raise_gate(snap, f"escalate:{action.reason}", action.reason)

        # 6) done / needs-human / continue
        if verdict.outcome is Outcome.DONE:
            if "on_done" in self._policy.gate_points():
                return await self._raise_gate(snap, "on_done",
                                              f"oracle-verified done — {verdict.reason}. accept?")
            return self._emit(snap.with_(state=LoopState.DONE))
        if verdict.outcome is Outcome.NEEDS_HUMAN:
            return await self._raise_gate(snap, "needs_human", verdict.reason)

        return self._emit(snap.with_(state=LoopState.RUNNING, gate=None))

    async def _raise_gate(self, snap: LoopSnapshot, point: str, prompt: str) -> LoopSnapshot:
        req = GateRequest(loop_id=snap.id, point=point, prompt=prompt)
        snap = self._emit(snap.with_(state=LoopState.GATE, gate=req))
        decision = await self._gate.ask(req)          # blocks until a human answers
        if decision == "kill":
            return self._emit(snap.with_(state=LoopState.KILLED, gate=None))
        if decision == "approve":
            final = LoopState.DONE if point == "on_done" else LoopState.RUNNING
            return self._emit(snap.with_(state=final, gate=None))
        if decision.startswith("redirect:"):
            hint = decision.split(":", 1)[1]
            return self._emit(snap.with_(state=LoopState.RUNNING, gate=None,
                                         goal=snap.goal))  # hint threaded via ctx (TODO: persist)
        return self._emit(snap.with_(state=LoopState.RUNNING, gate=None))
