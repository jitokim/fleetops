---
type: Codebase Overview
title: missionctl — Agent Wiki
description: A Go/Bubble Tea terminal fleet cockpit that observes Claude Code session logs and lets an operator act on stuck loops with one key.
timestamp: 2026-07-16T17:59:00+09:00
okf_version: "0.1"
generated_by: openwiki-claude-code
authoritative: false
---

missionctl is a single-binary Go CLI (`cmd/missionctl`) that renders a Bubble Tea
TUI (the "fleet cockpit") over every Claude Code session log under
`~/.claude/projects/*/*.jsonl`. It classifies each session's live state (running,
idle, gated, stalled, etc.) purely by tailing the JSONL transcript and cross-checking
the OS process table — no screen-scraping — and lets the operator attach, resume,
approve a permission gate, stop, kill, or spawn a brand new loop from the same
terminal, across whichever multiplexer (orca/cmux/tmux) hosts the session. A
lightweight opt-in layer (`internal/registry`, `internal/oracle`, `internal/engine`)
lets loops spawned via the TUI carry a goal contract that an independent LLM judge
verifies each idle cycle, with hard ceilings enforced by a governor. Today the
project is pure observation + actuation; there is no autonomous loop runner (see
`quickstart.md` for how this differs from `VISION.md`'s longer-term vision).

- [Quickstart](quickstart.md) — what the binary does, how to build/run it, and the mental model tying its packages together, including the VISION.md-vs-code gap.
- [Observation Subsystem](observation.md) — `internal/claude`: discovering loops from JSONL logs, state classification, liveness cross-checks, token/cycle metrics.
- [Actuation Subsystem](actuation.md) — `internal/control`: the `Controller` interface and its orca/cmux/tmux backends that resume, approve, focus, interrupt, and spawn loops.
- [Goal & Governance Subsystem](goal-governance.md) — `internal/domain`, `internal/registry`, `internal/oracle`, `internal/engine`, `internal/gate`: the optional goal-bound contract, independent oracle verdicts, governor ceilings, and permission-gate detection.
- [TUI & CLI Subsystem](tui-cli.md) — `cmd/missionctl`, `internal/tui`: the entry point, the Notification-hook subcommands, and the fleet cockpit's model/keymap/rendering.

> AS-IS and non-authoritative. Generated from code by `openwiki-claude-code`. The source of truth for this repo is the actual Go source (README.md/VISION.md for intent) — re-run `owcc update` after code changes.
