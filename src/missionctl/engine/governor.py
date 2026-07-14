"""Governance — pure functions. Budget / max-cycles / no-improve are hard ceilings;
a loop cannot exceed them, it escalates or fails closed (see DESIGN.md §3)."""

from __future__ import annotations

from dataclasses import dataclass

from missionctl.domain.models import LoopSnapshot


@dataclass(frozen=True, slots=True)
class Continue:
    pass


@dataclass(frozen=True, slots=True)
class Escalate:
    reason: str          # raise a human gate rather than dying silently


@dataclass(frozen=True, slots=True)
class Stop:
    reason: str          # terminal FAILED


GovernorAction = Continue | Escalate | Stop


def check(loop: LoopSnapshot) -> GovernorAction:
    """Decide whether a loop may run another cycle. Escalate (ask a human) before
    Stopping (fail) — a runaway should surface, not vanish."""
    goal = loop.goal
    if loop.tokens_spent >= goal.budget_tokens:
        return Escalate(f"budget exhausted ({loop.tokens_spent}/{goal.budget_tokens} tok)")
    if loop.cycle >= goal.max_cycles:
        return Escalate(f"max cycles reached ({loop.cycle}/{goal.max_cycles})")
    if loop.no_improve >= goal.no_improve_limit:
        return Stop(f"no progress for {loop.no_improve} cycles")
    return Continue()
