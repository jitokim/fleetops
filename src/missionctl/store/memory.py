"""In-memory LoopStore for the MVP/tests. A JSON-file store (restart-safe) and,
later, SQLite implement the same LoopStore port (DESIGN.md §1, §6)."""

from __future__ import annotations

from missionctl.domain.models import LoopSnapshot


class InMemoryLoopStore:
    def __init__(self) -> None:
        self._loops: dict[str, LoopSnapshot] = {}

    def save(self, snap: LoopSnapshot) -> None:
        self._loops[snap.id] = snap

    def load(self, loop_id: str) -> LoopSnapshot | None:
        return self._loops.get(loop_id)

    def list(self) -> list[LoopSnapshot]:
        return list(self._loops.values())
