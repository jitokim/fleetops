"""Textual fleet console (the mockup, see html-artifacts/mission-control-tui.html).

Scaffold: renders a sample fleet + wires the keybar. Phase 0 replaces the sample
data with a live subscription to the LoopEngine and makes the gate keys real.
"""

from __future__ import annotations

from rich.text import Text
from textual.app import App, ComposeResult
from textual.binding import Binding
from textual.containers import Vertical
from textual.widgets import DataTable, Footer, Static

from missionctl.domain.models import Goal, LoopSnapshot, Verdict
from missionctl.domain.states import LoopState, Outcome

_STATE_STYLE = {
    LoopState.RUNNING: ("● RUN", "blue"),
    LoopState.GATE: ("◆ GATE", "yellow"),
    LoopState.DRIFT: ("✗ DRIFT", "red"),
    LoopState.DONE: ("✓ DONE", "green"),
    LoopState.FAILED: ("✗ FAIL", "red"),
    LoopState.PAUSED: ("⏸ PAUSE", "grey58"),
    LoopState.KILLED: ("✗ KILL", "grey58"),
}


def _budget_bar(frac: float) -> str:
    filled = round(frac * 7)
    return "█" * filled + "░" * (7 - filled) + f" {round(frac * 100)}%"


def _sample_fleet() -> list[LoopSnapshot]:
    def g(text: str) -> Goal:
        return Goal(text=text)
    return [
        LoopSnapshot("l1", "voc-triage", g("triage alert #4471 → dedup → open PR"),
                     state=LoopState.GATE, cycle=6, tokens_spent=38_000,
                     last_verdict=Verdict(Outcome.PROGRESS, "fix verified on independent rerun")),
        LoopSnapshot("l2", "flaky-test-hunt", g("fix flaky tests until CI green"),
                     state=LoopState.DRIFT, cycle=9, tokens_spent=71_000, no_improve=2,
                     last_verdict=Verdict(Outcome.REJECTED, "3 tests still fail on clean rerun")),
        LoopSnapshot("l3", "aboard·slice-5", g("Bifrost gateway integration"),
                     state=LoopState.RUNNING, cycle=4, tokens_spent=54_000,
                     last_verdict=Verdict(Outcome.PROGRESS, "3/6 acceptance checks green")),
        LoopSnapshot("l4", "spec-0.3-review", Goal("5-round adversarial review", max_cycles=5),
                     state=LoopState.DONE, cycle=5, tokens_spent=96_000,
                     last_verdict=Verdict(Outcome.DONE, "converged — all rounds verified")),
    ]


class MissionControl(App):
    CSS = """
    Screen { background: #0b0f14; }
    #summary { padding: 0 1; color: #8a98a7; height: 1; }
    #detail { padding: 1 1; color: #c9d4de; border-top: solid #20303c; }
    DataTable { height: auto; }
    """
    BINDINGS = [
        Binding("a", "approve", "approve"),
        Binding("r", "redirect", "redirect"),
        Binding("k", "kill", "kill"),
        Binding("p", "pause", "pause"),
        Binding("n", "new_loop", "new"),
        Binding("q", "quit", "quit"),
    ]

    def __init__(self) -> None:
        super().__init__()
        self._fleet = _sample_fleet()

    def compose(self) -> ComposeResult:
        yield Static(id="summary")
        yield Vertical(DataTable(id="loops"), Static(id="detail"))
        yield Footer()

    def on_mount(self) -> None:
        self.title = "◎ missionctl"
        gates = sum(s.state is LoopState.GATE for s in self._fleet)
        summary = (f"fleet {len(self._fleet)} · oracle 86% · budget 418k/1.2M"
                   f"    [b yellow]▲ {gates} GATE NEEDS YOU[/]" if gates else "")
        self.query_one("#summary", Static).update(Text.from_markup(summary))

        table = self.query_one("#loops", DataTable)
        table.cursor_type = "row"
        table.add_columns("NAME", "STATE", "CYCLE", "ORACLE", "BUDGET", "N/I")
        for s in self._fleet:
            label, color = _STATE_STYLE[s.state]
            oracle = s.last_verdict.reason if s.last_verdict else "—"
            omark = {Outcome.REJECTED: "[red]✗[/]", Outcome.DONE: "[green]✓[/]"}.get(
                s.last_verdict.outcome if s.last_verdict else None, "[green]✓[/]")
            table.add_row(
                Text(s.name),
                Text.from_markup(f"[{color}]{label}[/]"),
                f"{s.cycle}/{s.goal.max_cycles}",
                Text.from_markup(f"{omark} {oracle[:26]}"),
                _budget_bar(s.budget_frac),
                "—" if s.state.is_terminal else str(s.no_improve),
                key=s.id,
            )
        self._render_detail(self._fleet[0])

    def _render_detail(self, s: LoopSnapshot) -> None:
        v = s.last_verdict
        lines = [f"[b cyan]▸ {s.name}[/]  [grey58]{s.goal.text}[/]", ""]
        if v:
            mark = "[red]✗[/]" if v.outcome is Outcome.REJECTED else "[green]✓[/]"
            lines.append(f"[grey58]ORACLE[/]  {mark} {v.reason}")
        if s.state is LoopState.GATE:
            lines.append("")
            lines.append(f"[b yellow]GATE ▸[/] {s.gate.prompt if s.gate else 'awaiting decision'}"
                         "   [black on yellow] a [/] approve   [b]r[/] redirect   [b]k[/] kill")
        self.query_one("#detail", Static).update(Text.from_markup("\n".join(lines)))

    def on_data_table_row_highlighted(self, event: DataTable.RowHighlighted) -> None:
        for s in self._fleet:
            if s.id == event.row_key.value:
                self._render_detail(s)
                return

    # gate actions — scaffold no-ops; Phase 0 wires these to the engine's gate sink
    def action_approve(self) -> None:
        self.notify("approved (scaffold — not yet wired to engine)")

    def action_redirect(self) -> None:
        self.notify("redirect… (scaffold)")

    def action_kill(self) -> None:
        self.notify("kill (scaffold)")

    def action_pause(self) -> None:
        self.notify("pause (scaffold)")

    def action_new_loop(self) -> None:
        self.notify("new loop (scaffold)")


def run() -> None:
    MissionControl().run()
