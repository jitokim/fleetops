---
type: Codebase Overview
title: missionctl — Quickstart
description: What missionctl is, its entry points, how to build/run it, and the mental model tying its packages together.
timestamp: 2026-07-16T17:59:00+09:00
okf_version: "0.1"
generated_by: openwiki-claude-code
authoritative: false
---

## What it is

`missionctl` is a Go module (`github.com/jitokim/missionctl`, `go.mod`, Go 1.25)
that builds one binary from `cmd/missionctl`. Its UI is
[Bubble Tea](https://github.com/charmbracelet/bubbletea) +
[Lipgloss](https://github.com/charmbracelet/lipgloss) +
[bubbles](https://github.com/charmbracelet/bubbles) — no other runtime
dependency beyond those and their transitive libs (`go.mod`).

Run with no args and it launches the fleet cockpit TUI. Two subcommand families
exist for wiring up gate detection:

```
go build -o missionctl ./cmd/missionctl
./missionctl                 # launch the fleet cockpit (default, no args)
./missionctl hooks install    # register the Notification hook in ~/.claude/settings.json
./missionctl hooks uninstall  # remove only the entry missionctl added
./missionctl hook notify      # (internal) the hook's own entry point; Claude Code invokes this, not a human
```

See `cmd/missionctl/main.go:17` for the top-level dispatch.

## Entry points

| Entry point | File | Purpose |
|---|---|---|
| `main()` | `cmd/missionctl/main.go` | Dispatches `hook`/`hooks` subcommands, else launches the TUI (`tui.New()` + `tea.NewProgram(..., tea.WithAltScreen())`). |
| `runHookCmd` / `notifyHook` | `cmd/missionctl/hook.go` | `missionctl hook notify` — reads Claude Code's `Notification` hook JSON from stdin, writes a gate marker file. Always exits 0; never fails loudly (a bug here must not break the user's real `claude` session). |
| `runHooksCmd` / `installHooks` / `uninstallHooks` | `cmd/missionctl/hooks.go` | `missionctl hooks install\|uninstall` — idempotently edits `~/.claude/settings.json`'s `hooks.Notification` array, backing up to `settings.json.bak-missionctl` first. |
| `tui.New()` / `Model.Update`/`View` | `internal/tui/model.go` | The fleet cockpit itself (Bubble Tea Model). |

## The mental model

missionctl's core idea, per its own README and code comments, is: **read the
evidence Claude Code already writes to disk, never re-derive it by scraping a
terminal.** Two independent, cheap signals are cross-referenced every ~3s
(`refreshEvery` in `internal/tui/model.go:102`):

1. **The session's own JSONL transcript** (`~/.claude/projects/<proj>/<session>.jsonl`)
   — file mtime = last activity; tailing the last 24KB (`tailBytes`,
   `internal/claude/scan.go:26`) reveals whether the last turn ended
   (`stop_reason: end_turn`) or is still in flight, and whether a rate-limit
   marker appears in the text.
2. **The OS process table** (`ps`/`lsof`, `internal/claude/procs.go`) — because
   the JSONL alone can't distinguish "waiting for a human" from "the process
   died"; both just stop writing.

A **third** signal, a small marker file dropped by a Claude Code `Notification`
hook (`internal/gate`), is the only reliable way to detect a live permission
prompt — screen-scraping an alt-screen terminal app was tried and rejected (see
`internal/gate/gate.go`'s package doc).

On top of this observation core sits an **optional** governance layer that only
applies to loops spawned via the TUI's `n` key: a goal contract
(`internal/registry`), an independent LLM verdict each idle cycle
(`internal/oracle`), and hard ceilings (`internal/engine`). Sessions missionctl
didn't spawn have no contract and are shown unbound (`—` in the ORACLE/N-I
columns) — they still get full state observation, just no verdict.

Actuation (attach/resume/approve/stop/kill/spawn) is abstracted behind the
`control.Controller` interface (`internal/control/control.go`), implemented for
orca, cmux, and tmux, with graceful degradation to a copy-pasteable manual hint
when no multiplexer is available.

## `VISION.md` vs the actual code — read this before trusting `VISION.md`

`VISION.md` at the repo root describes a **different, larger, aspirational**
system: a Python 3.11+/Textual/asyncio engine with a headless `LoopEngine` that
keeps loops running independently of the TUI, a `Challenger` adversarial-refute
phase, and a JSON→SQLite store — none of which exists in the current Go
codebase. `VISION.md` itself calls this "status: 0.1 design draft" and frames it
as a longer-term target ("the engine/governor design this project is named
for" — README's own Limitations section).

What actually exists today, verified by reading the Go source:

- **No headless engine.** There is no process that runs loops independently of
  a human's `claude` terminal session and the TUI. `internal/engine/governor.go`
  is a single pure function (`Check`) that classifies a loop's ceiling state —
  it does not execute cycles.
- **No `Challenger`.** `domain.Goal.Challenger` / `registry.Record.Challenger`
  are stored fields only, threaded into the spawn prompt and the registry, but
  never executed against anything (see the field comments in
  `internal/domain/loop.go` and `internal/registry/registry.go`).
  `buildSpawnPrompt` in `internal/tui/model.go` deliberately omits the
  challenger line when empty, "since there's no challenger phase yet."
- **Oracle exists, but is simpler than the design doc's `Protocol`.** `internal/oracle.Judge`
  shells out to `claude -p --model haiku --output-format json` once per idle
  cycle of a bound loop and parses a `{"outcome","reason"}` verdict — there is
  no pluggable `Oracle` interface, no challenger-then-oracle pipeline.
  Runtime language, stack, and store are all Go/JSON files on disk (see
  `internal/registry`, `internal/gate`), not Python/SQLite.

Treat `VISION.md` as a vision document, not a description of current behavior —
this AS-IS wiki (and reading the code directly) is the accurate reference for
what the binary does today.

## Where the "real" contract for this feature area is defined

`docs/specs/seed-missionctl-fleet-2026-07-16.md` is the locked seed spec this
codebase was actually built against (Go/Bubble Tea, observation + resume, the
oracle/governance layer explicitly deprioritized as "not the felt pain" — its
own "Consciously excluded" section). It matches the current code far more
closely than `VISION.md` does, though it too predates features that now exist
(spawn, gates, oracle, governor were all added after the seed spec's AC list).

> AS-IS and non-authoritative. Generated from code by `openwiki-claude-code`. The source of truth for this repo is the actual Go source (README.md/VISION.md for intent) — re-run `owcc update` after code changes.
