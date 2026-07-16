---
type: Subsystem
title: TUI & CLI — cmd/missionctl, internal/tui
description: The binary's entry point, its Notification-hook subcommands, and the fleet cockpit's Bubble Tea model, keymap, and rendering.
timestamp: 2026-07-16T17:59:00+09:00
okf_version: "0.1"
generated_by: openwiki-claude-code
authoritative: false
---

## `cmd/missionctl` — the binary's three faces

`main.go` dispatches on `os.Args[1]`: `hook` → `hook.go`, `hooks` → `hooks.go`,
anything else / no args → `runTUI()` (`tea.NewProgram(tui.New(), tea.WithAltScreen())`).

- **`hook notify`** (`hook.go`) is *not* meant for a human to type — Claude
  Code's own `Notification` hook invokes it on every notification. It reads
  the hook's JSON payload from stdin (`session_id`, `message`, `cwd`,
  `notification_type`) and writes a gate marker via
  `gate.WriteMarker`. It must never fail loudly: any error is swallowed and
  the process always exits 0, because a bug here must not be able to break
  the user's real `claude` session.
- **`hooks install`/`hooks uninstall`** (`hooks.go`) idempotently edit
  `~/.claude/settings.json`'s `hooks.Notification` array (decoded into
  `map[string]any`, not a struct, so unrelated fields round-trip untouched).
  `install` no-ops if the exact command line is already present; `uninstall`
  only removes entries recognizably missionctl's own
  (`isMissionctlNotifyCommand`: contains `"missionctl"` and ends with
  `"hook notify"`), never another tool's hook. Both back up the file to
  `settings.json.bak-missionctl` first.

## `internal/tui` — the fleet cockpit

A single Bubble Tea `Model` (`internal/tui/model.go`), refreshed every
`refreshEvery` = 3s via a `tea.Tick` that re-issues both `scan` (rediscover the
fleet, `claude.DiscoverLoops`) and itself. `Init()` = `tea.Batch(scan, tick())`.

### Modes

`mode` (`modeNormal | modePrompting | modeFiltering | modeInjecting`) routes
keystrokes: normal fleet navigation vs. the `n` wizard's free-text steps vs.
the `/` filter query vs. the `i` arbitrary-prompt injection box. Only one
`textinput.Model` field (`m.input`) is reused across all three text-entry
modes.

### Keymap (from `Update`'s `tea.KeyMsg` switch, `model.go:243`)

| Key | Guard | Action |
|---|---|---|
| `↑`/`↓`/`j`, `g`/`G` | — | move selection / jump to top or bottom of the **visible** (filtered) list |
| `/` | — | enter `modeFiltering`; live-filters as you type (`matchesFilter`) |
| `↵` (enter) | a loop selected | `attachCmd` — `Locate` (not `LocateClaude`) + `Focus`, permissive since attach is non-destructive |
| `a` | selected loop is `StateGate` | `approveCmd` — `LocateClaude` + `Approve` (bare Enter) |
| `r` | selected loop is `StateStalled`/`StateDrift` | `resumeCmd` — re-sends the session's last user prompt via `sendPromptCmd` |
| `i` | any loop except `StallGone`/`StateFailed` | `modeInjecting` — free-text prompt sent via `sendPromptCmd`, same guards as `r` but not restricted to stalled/drifted loops |
| `p` | selected loop is `StateRunning`/`StateGate` | `interruptCmd` — Esc, stops the turn without killing the process |
| `k` | — | first press arms a 3s confirm window (`killConfirmWindow`); a second `k` within it sends `/exit` via `killCmd` |
| `n` | — | starts the spawn wizard (`wizardStep` sequence, see below) |
| `o` | a loop selected | opens `less -R +G` on the session's JSONL via `tea.ExecProcess` (suspends the TUI) |
| `q` / `ctrl+c` | — | quit |
| `esc` | — | cancels the active mode/wizard step, or clears an active filter |

Every destructive/typed action (`r`, `a`, `i`, `p`, `k`) calls
`m.refuseIfAmbiguous(sel)` before dispatching — this mirrors, at keypress time,
the same ambiguity refusal `Controller.LocateClaude` itself enforces (see
`actuation.md`), so the human gets immediate feedback instead of a silent wrong
keystroke.

### The `n` key spawn wizard

Steps in order: `wizardGoal` (required; empty cancels) → `wizardDoneWhen`
(optional) → `wizardOracle` (optional) → `wizardChallenger` (optional, stored
only) → `wizardMaxCycles` (optional, `parseMaxCycles` defaults to
`registry.DefaultMaxCycles` = 12 on empty input) → `wizardWhere` (single-key
`w`/`d`/`enter`, shown **only** if the resolved backend implements
`control.WorktreeSpawner`, i.e. orca). `checkWorktreeEligibilityCmd` resolves
this off the event loop at `n` keypress time so the result is ready well before
a human finishes typing through the earlier steps.

`submitSpawnWizard` → `spawnCmd(cwd, spec, useWorktree)`
(`internal/registry.BindSpec`) → composes the full contract via
`buildSpawnPrompt` and dispatches `Controller.Spawn` or `SpawnWorktree`, then
`registry.WritePending` so the next scan's `BindPending` can attach the
contract once the new session starts writing its JSONL.

### Oracle trigger policy (`triggerJudgments`, `model.go:1063`)

Fires once per scan, per loop, only when: the loop has a goal (`Goal.Text != ""`),
its state is `StateIdle`, and it hasn't already been judged at this exact cycle
(`Last == nil || Cycle > Last.AtCycle`). An in-flight guard (`m.judging` map)
prevents re-firing while a previous `judgeCmd` for that session is still
running. `judgeCmd` calls `oracle.Judge` with the loop's **full** last report
(`claude.LastAssistantTextFull`, uncapped — unlike the TUI's own 120-char TAIL
row) and saves the verdict via `registry.SaveVerdict`.

### Rendering

`View()` composes: header row → summary band (counts + budget) → table
(name/doing/cycle/oracle/budget/no-improve/note columns, widths computed by
`columnWidths`/`flexNameDoing` to fit terminal width) → detail pane for the
selected row (`renderDetail`, with resume/gate/drift callouts) → status line →
keybar. Visual language is meant to match `html-artifacts/mission-control-tui.html`
(package doc, `model.go:3`) — that file is not part of this Go module.

> AS-IS and non-authoritative. Generated from code by `openwiki-claude-code`. The source of truth for this repo is the actual Go source (README.md/VISION.md for intent) — re-run `owcc update` after code changes.
