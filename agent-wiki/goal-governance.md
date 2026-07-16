---
type: Subsystem
title: Goal & Governance — domain, registry, oracle, engine, gate
description: The optional goal contract for spawned loops, the independent oracle verdict, governor ceilings, and how a live permission gate is detected.
timestamp: 2026-07-16T17:59:00+09:00
okf_version: "0.1"
generated_by: openwiki-claude-code
authoritative: false
---

Four small packages implement the part of missionctl that goes beyond plain
observation: a loop the TUI itself spawned (the `n` key) can carry a goal
contract, get judged by an independent model each idle cycle, and be stopped by
hard ceilings. **This layer is entirely optional** — a session missionctl
didn't spawn has no contract, is never judged, and shows `—` in the
ORACLE/N-I columns.

## `internal/domain` — shared vocabulary

`loop.go` defines the types every other package shares:

- `LoopState`: `running | gate | stalled | idle | drift | done | failed | paused | killed`.
  `Terminal()` is true only for `done`/`failed`/`killed`.
- `StallKind`: `token budget exhausted | rate limited (429) | no output | process gone`.
- `Goal{Text, DoneWhen, Oracle, Challenger, MaxCycles, BudgetTokens, NoImproveLimit}`
  — the wizard-collected contract. **`Challenger` is stored only, never
  executed** (no challenger phase exists — see `quickstart.md`'s VISION.md gap
  section).
- `Verdict{Outcome, Reason, AtCycle}` — the oracle's judgment, tagged with
  which cycle it was rendered against so callers can tell "already judged this
  cycle" (`AtCycle == Cycle`) from "cycle advanced since" (`AtCycle < Cycle`).
- `Loop` — the full renderable state the TUI operates on; notably
  `CwdVerified` (only true once `Cwd` is confirmed against a live process's
  real `lsof` path, not a lossy decode) gates whether spawning "into this
  loop's directory" is safe, and `GateTS` (nanosecond marker timestamp) lets
  approve compare-and-swap delete only the exact marker a decision was based
  on.

`encode.go` holds `EncodeCwd` — the single source of truth for Claude Code's
`/`-and-`.`-both-become-`-` project-dir encoding, used by `internal/claude`,
`internal/control`, and `internal/registry`.

## `internal/registry` — persisted goal contracts

Stores one JSON file per bound session under `~/.missionctl/loops/<sessionID>.json`
(`LoopsDir`), plus not-yet-matched spawns under `~/.missionctl/pending/`
(`PendingDir`).

- `WritePending(dir, cwd, spec)` — called right after a successful `Spawn`/
  `SpawnWorktree`, since `Controller.Spawn` has no way to report the new
  session's id back (it just starts a process).
- `BindPending(loopsDir, pendingDir, loops, now)` — run every scan
  (`internal/claude/scan.go`'s `DiscoverLoops`): matches each pending spawn to
  the newest not-yet-bound session sharing its **encoded** cwd
  (`domain.EncodeCwd(p.Cwd) != l.ProjectDir` — matching must go through the
  lossless real-path→encoded direction, never the lossy decode, or a
  hyphenated worktree dir silently fails to match — "this exact bug shipped
  once"). Pending spawns older than `pendingStaleAfter` (10 min) are dropped
  as presumed-failed.
- `Bind` creates the record (`MaxCycles` defaults to `DefaultMaxCycles` = 12 if
  ≤ 0); `SaveVerdict` updates it after each oracle judgment and maintains the
  no-improve streak: `rejected` increments it, anything else resets it to 0.

## `internal/oracle` — independent verdict

`Judge(goal, cwd, lastAssistantText, doneWhen, oracleRubric)`
(`internal/oracle/oracle.go:36`) shells out to
`claude -p --model haiku --output-format json` with a strict prompt
(`buildPrompt`) instructing the model to verify only against the report's
evidence, never the agent's own claim — explicitly telling it the agent's cwd
so it doesn't reject valid relative paths. `doneWhen`/`oracleRubric` are the
same wizard-collected text shown to the agent at spawn time
(`internal/tui/model.go`'s `buildSpawnPrompt`) — one contract document used for
both instructing and judging. `parseVerdict` unwraps the `claude -p` JSON
envelope, strips a possible code fence, and only accepts
`done`/`progress`/`rejected` as valid outcomes — anything else is an error, never
guessed. `judgeTimeout` = 2 minutes.

## `internal/engine` — governor (pure function)

`Check(loop) Decision` (`internal/engine/governor.go`) is the *only* piece of
what `VISION.md` calls "the engine" that actually exists — a pure classifier,
not a loop runner:

```go
if budget exhausted        -> Escalate("budget exhausted")
if max cycles reached      -> Escalate("max cycles reached")
if no-improve limit hit    -> Stop("no progress for repeated cycles")
else                        -> Continue
```

Escalate is checked before Stop deliberately — "a runaway should surface, not
vanish." Applied by `internal/claude/scan.go`'s `applyGovernor` after the
oracle-verdict state mapping, and skipped entirely for a loop already at a live
gate or already terminal.

## `internal/gate` — permission-prompt detection

Stores one marker file per session under `~/.missionctl/gates/<sessionID>.json`
(`GatesDir`), written by `missionctl hook notify`
(`cmd/missionctl/hook.go`) every time Claude Code's `Notification` hook fires.

- `Info{Message, Type, TS}` — `Type` is Claude Code's `notification_type`
  verbatim; empty on older claude versions.
- **Why a hook and not screen-scraping**: the package doc states plainly that
  screen-scraping an alt-screen terminal app (orca) to detect a permission
  prompt "is not viable (verified live against the real orca CLI)."
- `IsGateActive(markerTS, logMtime)` — a marker is stale (already answered) once
  the transcript's mtime is more than `staleSlack` (2s) after the marker fired;
  new transcript activity after the gate means the human already answered.
- `DeleteMarkerIfTS` is a **compare-and-swap** delete: only removes a marker if
  its on-disk TS still equals the TS the caller decided was stale — otherwise a
  plain delete-by-name could destroy a *brand new* marker that landed between
  the snapshot and the delete (a fresh prompt right after the old one was
  answered). Nanosecond-precision TS (`normalizeTSNanos` upgrades legacy
  whole-second timestamps) is what makes the CAS actually distinguish two
  markers landing in the same second.
- Only three `notification_type` values count as a real gate
  (`gateNotificationTypes`, `internal/claude/scan.go:363`):
  `permission_prompt`, `elicitation_dialog`, `agent_needs_input`. The same hook
  also fires for the 60s "waiting for your input" idle nudge, which is *not* a
  gate — the type field is what tells them apart (message-text fallback for
  older claude versions that omit the field).

## How it all ties together at spawn time (`internal/tui`'s `n` key)

The wizard (`wizardStep` in `internal/tui/model.go`) collects Goal → DoneWhen →
Oracle rubric → Challenger text (stored only) → MaxCycles → (optionally)
worktree-or-current-dir. `buildSpawnPrompt` composes all but the worktree
choice into a single "LOOP CONTRACT" block sent as the new session's first
message; `registry.WritePending` records the same fields for later binding.
Once the session starts writing its own JSONL and gets picked up by
`DiscoverLoops`, `BindPending` matches it to the pending record, and from then
on `enrichFromRegistry` + `applyGovernor` + the TUI's `triggerJudgments`
(fires `oracle.Judge` once per idle cycle not yet judged) drive the ORACLE/N-I
columns and `StateDone`/`StateDrift`/`StateFailed` transitions.

> AS-IS and non-authoritative. Generated from code by `openwiki-claude-code`. The source of truth for this repo is the actual Go source (README.md/VISION.md for intent) — re-run `owcc update` after code changes.
