# missionctl

A fleet cockpit for Claude Code loops, in your terminal.

You run several `claude` sessions across projects — some grinding on a task, some
waiting on a permission prompt, some silently dead. missionctl watches
`~/.claude/projects/`'s own session logs, works out what's *actually* happening
in each one, and lets you act on it with one key — without switching terminals
to find out.

## What it does

- **Observes** every Claude Code session by reading its JSONL transcript and file
  mtime. No screen-scraping, no polling `tmux`/`orca` for content.
- **Detects real state**, not just "wrote recently": `run` (mid-turn), `idle`
  (finished a turn, waiting on you), `◆ GATE` (a permission prompt fired), `◆
  STALLED` / `✗ 429` (stuck or rate-limited), `✗ gone` (the terminal closed or
  the process died), and for goal-bound loops, `✓ DONE` / `✗ DRIFT` (see
  Oracle below).
- **Acts in one key** — attach, resume, approve a gate, stop a turn, kill, or
  spawn a brand new loop — across whichever terminal multiplexer hosts the
  session (orca, tmux, cmux). On a bare terminal with no multiplexer, it still
  degrades to a copy-pasteable manual command instead of doing nothing.

## Quick start

```bash
go build -o missionctl ./cmd/missionctl   # or: go install ./cmd/missionctl
./missionctl                              # launch the fleet cockpit
./missionctl hooks install                # wire up gate detection (see below)
```

`hooks install` registers a Claude Code `Notification` hook
(`missionctl hook notify`) in `~/.claude/settings.json`, backing up the existing
file first. `hooks uninstall` removes only the entry missionctl added.

## Keymap

| Key | Action |
|---|---|
| `↑` `↓` | move selection |
| `/` | filter the fleet (live, by project/session/state/stall — `esc` clears, `enter` applies) |
| `↵` | attach: bring the loop's terminal to the front |
| `a` | approve a gate (bare Enter — accepts claude's default option) |
| `r` | resume a stalled/drifted loop (re-sends its last prompt) |
| `p` | stop the current turn (Esc) without killing the process |
| `k` | kill — press twice within 3s to confirm (sends `/exit`) |
| `n` | spawn a new loop: prompts for a goal, starts `claude` in the selected loop's directory (or cwd) |
| `o` | open the session log in `less` |
| `q` | quit |

## How it works

**Observation.** `internal/claude` globs `~/.claude/projects/*/*.jsonl`, uses each
file's mtime as "last activity", and tails the last ~24KB to classify the loop:
a finished turn (`stop_reason: end_turn`) is idle regardless of how long ago
that was; mid-turn is `run` if recent, `stalled` once past a threshold (`429`
detected from the tail text). A `ps`/`lsof` cross-check (`internal/claude`'s
liveness pass) catches the case a session looks idle but its `claude` process
is actually gone — that's `✗ gone`, not idle.

**Gates.** `missionctl hook notify` writes a small marker file
(`~/.missionctl/gates/`) every time Claude Code's `Notification` hook fires.
Only markers whose `notification_type` means "blocked on a human"
(`permission_prompt`, `elicitation_dialog`, `agent_needs_input`) become a real
`◆ GATE` — the same hook also fires for the 60s "waiting for your input" idle
nudge, which is not a gate. A marker goes stale (and is deleted) once the
transcript shows new activity after it fired — the human already answered.

**Oracle.** Loops spawned via `n` are goal-bound: missionctl knows what they're
trying to achieve, so it can judge them. Once idle, `internal/oracle` asks a
cheap model (`claude -p --model haiku --output-format json`) to verdict the
loop's last report against its goal — never trusting the agent's own "done"
claim. `done` → `✓ DONE`; a false "done" claim → `✗ DRIFT` (re-drive with `r`,
or kill); real work with no claim either way → `progress`, state unchanged. A
no-improve counter (`N/I`) tracks consecutive rejections. Observed sessions
that weren't spawned by missionctl have no goal, so no verdict — shown as `—`.

**Budget.** The BUDGET column/bar tracks `output_tokens` only (summed across
the session) against a 2,000,000-token default cap per loop —
`input_tokens`/cache tokens re-bill the whole conversation context on every
single call, so summing those would wildly overstate spend rather than
reflect actual work done.

## Backend matrix

| Backend | Attach/Resume/Approve/Stop | Spawn | Status |
|---|---|---|---|
| **orca** | ✅ | ✅ | Verified live against the real CLI; preferred when available. |
| **tmux** | ✅ | ✅ | Verified against tmux's documented command contract. |
| **cmux** | ✅ (send/focus/approve/interrupt) | ❌ | Adapter present, but the `tree --json` shape it parses is **unverified** — no cmux CLI was available to test against. Marked with `TODO` in the code. |
| **bare terminal** (none of the above) | manual hint only | manual hint only | Observation still works fully; actions print a copy-pasteable command (`claude --resume <id>`, `cd <dir>`, etc.) instead of silently failing. |

`internal/control.Resolve()` picks the first available backend in that order
(orca → cmux → tmux).

## Limitations

- **No goal, no oracle.** A session missionctl didn't spawn has no recorded
  goal, so it can never show `DONE`/`DRIFT`/`N-I` — only the tail-based
  state (`run`/`idle`/`stalled`/etc).
- **cmux is unverified.** Its surface-tree JSON parser is intentionally
  tolerant (walks unknown shapes rather than failing), but was never tested
  against a real cmux install. Treat it as best-effort.
- **macOS-first.** Process liveness (`internal/claude`'s liveness pass) shells
  out to `ps axo pid,comm` and `lsof`; it hasn't been adapted for Linux's
  `/proc` or other platforms.
- **No engine yet.** missionctl is pure observation + actuation today — there
  is no autonomous loop *runner* here (see [`DESIGN.md`](./DESIGN.md) for the
  longer-term engine/governor design this project is named for).
