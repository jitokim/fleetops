"""Loop lifecycle states and verdict outcomes (see DESIGN.md §3)."""

from __future__ import annotations

from enum import Enum


class LoopState(str, Enum):
    RUNNING = "running"   # a cycle is (or will be) executing
    GATE = "gate"         # blocked, waiting on a human decision
    DRIFT = "drift"       # oracle rejected the agent's "done"; not converged
    DONE = "done"         # oracle-verified converged
    FAILED = "failed"     # governor stopped it (budget/cycles/no-improve) unrecoverably
    PAUSED = "paused"     # human paused
    KILLED = "killed"     # human killed

    @property
    def is_terminal(self) -> bool:
        return self in (LoopState.DONE, LoopState.FAILED, LoopState.KILLED)


class Outcome(str, Enum):
    """What the oracle concluded about a cycle's result."""

    PROGRESS = "progress"      # moved forward, not done
    DONE = "done"              # goal met, independently verified
    REJECTED = "rejected"      # agent claimed done but it isn't (drift)
    NEEDS_HUMAN = "needs_human"  # ambiguous / a decision only a human should make
