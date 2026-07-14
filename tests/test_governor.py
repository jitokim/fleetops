"""Governance ceilings are hard — the scaffold's one real behavior for now."""

from missionctl.domain.models import Goal, LoopSnapshot
from missionctl.domain.states import LoopState
from missionctl.engine import governor


def _loop(**kw) -> LoopSnapshot:
    return LoopSnapshot("t", "t", Goal("x", max_cycles=5, budget_tokens=100, no_improve_limit=3), **kw)


def test_continue_within_limits():
    assert isinstance(governor.check(_loop(cycle=1, tokens_spent=10, no_improve=0)), governor.Continue)


def test_budget_exhausted_escalates():
    action = governor.check(_loop(cycle=1, tokens_spent=100))
    assert isinstance(action, governor.Escalate)


def test_max_cycles_escalates():
    assert isinstance(governor.check(_loop(cycle=5)), governor.Escalate)


def test_no_improve_stops():
    action = governor.check(_loop(cycle=2, no_improve=3))
    assert isinstance(action, governor.Stop)
    assert "no progress" in action.reason


def test_done_is_terminal():
    assert LoopState.DONE.is_terminal
    assert not LoopState.RUNNING.is_terminal
