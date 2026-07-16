---
type: Subsystem
title: Actuation — internal/control
description: The Controller interface and its orca/cmux/tmux backends that resume, approve, focus, interrupt, and spawn Claude Code loops.
timestamp: 2026-07-16T17:59:00+09:00
okf_version: "0.1"
generated_by: openwiki-claude-code
authoritative: false
---

Package `internal/control` re-drives a stalled/gated loop by re-sending
keystrokes to whichever terminal multiplexer hosts it. It is a deliberately
**zero-internal-package** layer (only `internal/domain` is imported, for
`EncodeCwd`) — a pure actuation boundary matching `VISION.md`'s "pluggable
ports" idea, even though no formal port abstraction exists elsewhere yet.

## The `Controller` interface (`internal/control/control.go`)

```go
type Controller interface {
    Name() string
    Available() bool
    Locate(projectDir string) (Target, bool)
    LocateClaude(projectDir string) (Target, bool)
    Resume(t Target, prompt string) error
    Focus(t Target) error
    Approve(t Target) error
    Spawn(cwd, goal string) error
    Interrupt(t Target) error
}
```

`Target` identifies a controllable surface: `Backend`, `ID` (backend-specific
handle/ref/pane-id), `Cwd`, and `Window` (cmux-only — see below).

**`Locate` vs `LocateClaude`** is the load-bearing distinction across all three
backends: `Locate` is permissive (a bare shell in the right directory is a fine
attach target); `LocateClaude` must return a surface *confirmed* to be running
`claude`, and **refuses (ok=false) on genuine ambiguity** — e.g. two claude
panes open in the same directory. Every typed/destructive action
(Resume/Approve/Interrupt, and the TUI's kill) goes through `LocateClaude` only;
`Locate` backs attach (`enter` key) alone. This is the authoritative backstop
behind the TUI's own keypress-time ambiguity guard
(`Model.refuseIfAmbiguous`, `internal/tui/model.go:1134`).

`Resolve()` picks the first available backend in order **orca → cmux → tmux**
(`control.go:110`); `nil, false` if none are on `PATH`/responsive.

`WorktreeSpawner` is an **optional** capability interface
(`SpawnWorktree(repoCwd, name, prompt) (worktreePath string, err error)`),
implemented only by orca. Callers type-assert (`ctrl.(control.WorktreeSpawner)`)
rather than widening `Controller` for one backend's capability.

## Backend implementations

### orca (`internal/control/orca.go`) — preferred backend

Drives Orca via its CLI (`orca terminal ...`, `orca worktree ...`), all
`--json`. This is the most thoroughly *runtime-verified* backend (per its own
comments): terminal listing/creation, resume (`terminal send --text ... --enter`),
approve (same call with empty text), focus (`terminal switch`), interrupt
(`terminal send --interrupt`), and the one-shot worktree-isolated spawn
(`worktree create --repo ... --agent claude --prompt ...`).

Notable behaviors:
- `Spawn` creates a terminal, waits for TUI boot (`terminal wait --for tui-idle`),
  then **re-locates** the terminal by cwd+title rather than trusting the
  create-time handle, because that handle can go stale once Orca's UI adopts
  the pane.
- `selectClaudeOrcaTerminal`/`selectOrcaTerminal` tier terminals by
  connected+writable+Claude-Code-titled (title prefix `✳`) before falling back
  — `LocateClaude` only ever uses tier 1, refusing on >1 match.
- `SpawnWorktree`'s **shared-workspace caveat**: for a path-registered
  ("folder") repo, Orca does not create an isolated checkout — the returned
  path equals `repoCwd`. The spawn still works; the TUI's status line tells
  the human it landed in a shared directory, not a fresh one.

### cmux (`internal/control/cmux.go`) — verified against cmux 0.64.15 only

Parses `cmux tree --json` tolerantly (`parseCmuxTree`/`walkCmuxNode`) into
`cmuxSurface{ref, tty, window}`. cmux's tree carries **no cwd anywhere**
(verified against a full dump), so a surface's directory is resolved
out-of-band by cross-referencing its tty against the OS process table
(`liveResolveCmuxTTYs`, the same `ps`→`lsof` pattern as
`internal/claude/procs.go`, deliberately duplicated rather than imported).

- `locateCmux` (permissive) vs `locateCmuxClaude` (confirmed-claude-only,
  refuses on >1 **distinct tty** match — note ambiguity is counted by tty, not
  by surface ref, since cmux can list the same terminal as more than one
  surface ref sharing one tty, which is not a wrong-terminal hazard).
- **Cross-workspace addressing**: a cmux `--surface`/`--panel` ref resolves
  within a *window* context that defaults to the caller's own workspace
  (`$CMUX_WORKSPACE_ID`) when `--window` is omitted — a target in another
  workspace then fails ("Surface not found"). `Target.Window` (captured while
  walking the tree) is appended as `--window <ref>` to every cmux actuation
  call (`appendCmuxWindow`) to fix this; reproduced and verified live on cmux
  0.64.15 for `send`, `send-key`, and `focus-panel`.
- `Approve` (`send-key ... enter`) and `Interrupt` (`send-key ... escape`) are
  contract-level, not fully runtime-tested: the `enter` key token is confirmed
  from `cmux send-key --help`, but `escape` is assumed by convention (not shown
  in the CLI's own examples), and no live claude-in-cmux gate was available to
  confirm the semantic effect end-to-end.
- `Spawn` is **not implemented** — returns an explicit error; no verified
  create-surface command exists yet.

### tmux (`internal/control/tmux.go`)

The simplest backend: `tmux list-panes -a -F '#{pane_id}\t#{pane_current_path}\t#{pane_current_command}'`.
`LocateClaude` filters to panes whose foreground command is literally `claude`
(`selectClaudeTmuxPane`), refusing on >1 match — same ambiguity contract as the
other two backends.

## Shared mechanics (`control.go`)

- `actuationTimeout` (5s) bounds every typed action's exec call so a wedged
  multiplexer CLI never hangs the TUI; `availabilityTimeout` (2s, in `cmux.go`)
  bounds liveness/listing probes shared by all backends.
- `encodeCwd` in this package is a **deliberate duplicate** of
  `domain.EncodeCwd` — kept local rather than imported, to preserve the
  zero-internal-package boundary. Must be kept in sync if the encoding scheme
  ever changes.

## Backend feature matrix (also in the repo README)

| Backend | Attach/Resume/Approve/Stop | Spawn | Notes |
|---|---|---|---|
| orca | Yes | Yes | Preferred; most thoroughly verified. |
| tmux | Yes | Yes | Verified against tmux's documented contract. |
| cmux | Yes (locate/send/focus/approve/interrupt) | No | Verified only on cmux 0.64.15; cross-workspace `--window` fix included. |
| none | manual hint only | manual hint only | Observation still works; actions print a copy-pasteable command instead of failing silently. |

> AS-IS and non-authoritative. Generated from code by `openwiki-claude-code`. The source of truth for this repo is the actual Go source (README.md/VISION.md for intent) — re-run `owcc update` after code changes.
