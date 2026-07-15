---
status: locked
created: 2026-07-16
interview: deep-interview (fleet cockpit)
---

# Seed Spec: missionctl — fleet cockpit for Claude Code loops

## Goal
A single-binary terminal cockpit (Go/Bubble Tea) that aggregates every long-running
**Claude Code loop session** on the local machine into one view, **detects when a loop
went silently stuck** (rate-limited / idle / token-out), and lets the operator **resume
it with one key** — killing the `cmux` tab-hopping + "why did it stop?" babysitting of
1–2 day eval/agent loops. North-star UX = the colleague's Teleport TUI (aggregated list
· arrow-key select · right-pane detail · one-key action).

## Scope
- **In:** local machine only. A "loop" = a **Claude Code session** running a harness with
  a stop condition (`~/.claude/projects/<proj>/<session>.jsonl`, one file per session).
  Observe by **tailing those JSONL logs — NOT screen-scraping** (validated: `429` / `rate
  limit` appear in the logs directly). Aggregate fleet view + stall detection + one-key
  resume/re-send.
- **Out (later):** remote machines / multi-host aggregation; non-Claude-Code loops; full
  create/intervene (redirect, spawn) beyond resume; the oracle/governance layer.

## Technical approach
- **Observe (LOCKED):** watch `~/.claude/projects/*/*.jsonl`. Each active file = a loop.
  Derive per-loop state from the tail: last-activity timestamp (idle = no new line for
  N min), and stall reason by scanning recent lines for `429` / `rate limit` / usage-limit
  markers. No `tmux capture-pane`, no rendered-screen parsing.
- **Control / resume (OPEN — see Assumptions):** `cmux`/`tmux` are not on PATH here, so
  the send-keys path is unconfirmed. Candidate mechanisms: (a) multiplexer `send-keys` to
  the loop's pane, (b) `claude --resume <session-id>` re-invocation. **Blocking for the
  resume feature; the observation cockpit does not depend on it.**
- **Stack:** Go 1.25 + Bubble Tea/Lipgloss, single static binary. Domain/engine already
  scaffolded (`internal/domain`, `internal/engine`).

## Acceptance Criteria
- [ ] AC-1: discover all active Claude Code sessions under `~/.claude/projects/` and list
      them in one Bubble Tea cockpit (name/project · state · last-activity · budget/usage).
- [ ] AC-2: detect **idle** (no new log line for a threshold) and surface it as a state.
- [ ] AC-3: detect **rate-limited (429)** from log content and surface *why it stopped*.
- [ ] AC-4: arrow-key select a loop → right-pane detail (last activity, stall reason).
- [ ] AC-5: **one-key resume/re-send** on a stalled loop (mechanism per the resolved
      control decision).
- [ ] AC-6: notify (desktop/terminal) when a loop enters a stall state (so you don't poll).
- [ ] AC-7: single `missionctl` binary; `go build ./...` clean; runs without a config.

## Confirmed edge cases (to cover)
1. **Distinguish loops from ordinary Claude Code chats** — every session logs here,
   including interactive chats (e.g. *this* session). Default: show sessions with recent
   activity; allow filtering by project/tag. (Assumption A2.)
2. **Session ended vs stalled** — a finished harness (stop condition met) vs a hung one:
   ended = clean last event; stalled = idle/429 with no completion.
3. **Huge JSONL** — 1–2 day loops produce large files → tail from the end, don't re-read.
4. **429 that self-recovered** — a 429 followed by later activity is not a current stall.

## Consciously excluded (with reason)
1. Remote/multi-host aggregation — MVP is one machine (transport/auth later).
2. Non-Claude-Code loops (bash/python harnesses) — the ICP's loops are Claude Code; add an
   adapter later.
3. Full create/intervene (spawn new loops, redirect mid-flight) — start with resume.
4. Oracle/governance layer — de-prioritized; not the felt pain.

## Assumptions (confirmed / to confirm)
- A1 (confirmed): a loop = a Claude Code session (harness + stop condition), 1 JSONL/session.
- A2 (default): monitor sessions with recent activity; filtering/tagging is a follow-up.
- A3 (OPEN): the resume/control mechanism depends on what `cmux` is and how the operator
  re-sends a prompt today — **the one open decision before AC-5.**
