---
type: Subsystem
title: Observation — internal/claude
description: How missionctl discovers Claude Code sessions and classifies each one's live state from its JSONL transcript and the OS process table.
timestamp: 2026-07-16T17:59:00+09:00
okf_version: "0.1"
generated_by: openwiki-claude-code
authoritative: false
---

Package `internal/claude` is the observation core: it turns Claude Code's own
session logs into the `[]domain.Loop` slice the TUI renders every scan. Nothing
in this package screen-scrapes a terminal.

## Entry point: `DiscoverLoops`

`DiscoverLoops(now, within)` (`internal/claude/scan.go:53`) is called once per
refresh tick (every `refreshEvery` = 3s, from `internal/tui/model.go`'s `scan`
`tea.Cmd`). Its pipeline:

1. Glob `~/.claude/projects/*/*.jsonl` (`ProjectsDir()`).
2. Drop empty files and anything not written within `within` (`ActiveWindow` =
   24h) — old finished sessions age out of "the fleet".
3. Drop hidden-project-dir sessions (`IncludeHidden` = false by default) —
   Claude Code encodes both `/` and `.` as `-`, so a dot-prefixed path segment
   (e.g. `~/.claude-mem/...`) doubles up a dash (`--`); these are headless
   agent-tooling sessions, not a human's loop.
4. For each remaining file, `loopFromLog` builds a `domain.Loop` (see below).
5. Sort by `LastActivity` descending.
6. `registry.BindPending` + `enrichFromRegistry` attach goal-contract metadata
   for loops spawned via the TUI (see `goal-governance.md`).
7. `applyLiveness` cross-checks against live `claude` OS processes.
8. `pruneMetricsCache` bounds the per-session token/cycle cache to files still
   present in this scan.

## State classification (`loopFromLog` / `classifyLoop`)

Each loop's `domain.LoopState` (`internal/domain/loop.go`) comes from tailing
the last 24KB of its JSONL (`tailBytes`, `scan.go:26`) and applying, in order:

1. **Turn-completion check** (`lastTurnEnded`): if the last parseable
   user/assistant entry is an assistant message with `stop_reason: "end_turn"`,
   the loop is `StateIdle` — *regardless of how long ago that was*. A finished
   turn waiting on a human is not an incident.
2. **Recency check**: otherwise (mid-turn), `StateRunning` if the file was
   written within `IdleThreshold` (4 minutes), else `StateStalled`.
3. **Stall reason**: a stalled loop gets `StallRateLimit` if the tail contains
   a `429`/"rate limit"/"usage limit" marker (`hasRateLimitMarker`), else
   `StallNoOutput`.
4. **Gate override**: a pending Notification-hook marker
   (see `internal/gate`) beats all of the above *only if* its
   `notification_type` means "blocked on a human" (`permission_prompt`,
   `elicitation_dialog`, `agent_needs_input` — `gateNotificationTypes`,
   `scan.go:363`). The same hook also fires for Claude Code's 60s "waiting for
   your input" idle nudge, which is explicitly excluded. Older claude versions
   that omit `notification_type` fall back to a message-text heuristic
   (`isGateNotification`).

## Liveness cross-check (`applyLiveness`, `internal/claude/procs.go`)

The JSONL alone can't tell "waiting for human" (idle) from "the terminal
closed / process died". `LiveClaudeCwds()` runs `ps axo pid,comm` then a
batched `lsof -d cwd` for matching pids, returning real (unencoded) cwd → live
`claude`-process count. `applyLiveness` (`scan.go:226`) then, per `ProjectDir`:

- Heals `Cwd`/`CwdVerified` to the confirmed-real lsof path whenever any live
  process backs that `ProjectDir` (skipped if two distinct real paths collide
  on the same encoded `ProjectDir` — ambiguous, so no healing).
- For loops beyond the live-process count in that dir: `StateIdle` → dropped
  from the fleet entirely (clean exit); `StateDone`/`StateDrift` → left alone
  (a settled oracle judgment, not an incident); anything else → reclassified
  `StateStalled` / `StallGone` (a mid-work death).

`ok=false` from the probe (ps/lsof itself failed) must never be treated as
"zero live processes" — `applyLiveness` short-circuits to "leave the fleet
exactly as classified" in that case.

## Cwd encoding (shared with `internal/control`)

`domain.EncodeCwd` (`internal/domain/encode.go`) is the single source of truth
for Claude Code's project-dir encoding (`/` and `.` → `-`). Matching a live OS
path against a `ProjectDir` must always go through this lossless direction;
`decodeCwd` (display-only, in `scan.go`) is lossy and never used for matching.

## Metrics (`internal/claude/metrics.go`)

`SessionMetrics(path)` returns `(cycles, tokensSpent)` for the BUDGET/CYCLE
columns, cached by file size+mtime (`metricsCache`, a `sync.Map`) so a full
re-scan of a session only happens when it actually changed:

- `cycles` = count of `"type":"user"` entries with real text content
  (tool-result-only entries don't count).
- `tokensSpent` = sum of `message.usage.output_tokens` across assistant
  entries — **`input_tokens`/cache tokens are deliberately excluded**, because
  they re-bill the whole conversation context on every call and would wildly
  overstate spend (43M observed on a session that hadn't done that much work).
  `DefaultBudgetTokens` = 2,000,000, applied when a loop has no per-loop budget
  configured yet.

## Gotchas for anyone extending this package

- `IdleThreshold` (4 min) and `ActiveWindow` (24h) are package vars, not
  constants — currently only ever overridden in tests.
- Tail-based classification always runs, not just once a loop looks idle, so
  the same tail buffer also serves `LastText` (the detail pane's TAIL row)
  without a second file read.
- `LastAssistantTextFull` (uncapped) exists separately from
  `LastAssistantText` (120-char cap for the TUI row) because the oracle needs
  the full report — a summary would discard exactly the evidence it judges.

> AS-IS and non-authoritative. Generated from code by `openwiki-claude-code`. The source of truth for this repo is the actual Go source (README.md/VISION.md for intent) — re-run `owcc update` after code changes.
