<p align="center">
  <img src="icon-round.png" alt="FleetOps logo" width="128" />
</p>

<h1 align="center">fleetops</h1>

<p align="center">A fleet cockpit for Claude Code loops, in your terminal.</p>

<p align="center">
  <a href="https://github.com/jitokim/fleetops/releases"><img src="https://img.shields.io/github/v/release/jitokim/fleetops?include_prereleases&amp;label=release&amp;color=blue" alt="Latest release" /></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-blue" alt="MIT license" /></a>
  <a href="go.mod"><img src="https://img.shields.io/badge/go-1.25-00ADD8?logo=go&amp;logoColor=white" alt="Go 1.25" /></a>
  <a href="https://x.com/fleetopsdev"><img src="https://img.shields.io/badge/follow-%40fleetopsdev-000000?logo=x&amp;logoColor=white" alt="Follow @fleetopsdev on X" /></a>
</p>

![fleetops cockpit](screenshot.png)

_(the fleet above is `fleetops --demo` — a synthetic fleet, nothing real)_

> **Status: experimental / 0.6.1-alpha.** This is a young, actively-changing
> project — expect rough edges, and read the "Known rough edges" and
> "Limitations" sections below before trusting it with anything you can't
> afford to have go wrong (it does send real keystrokes and can kill real
> processes; see "How it works").

You run several `claude` sessions across projects — some grinding on a task, some
waiting on a permission prompt, some silently dead. fleetops watches
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
  session (orca, tmux, cmux), plus **iTerm2 directly, with no multiplexer at
  all**. Anywhere else, it still degrades to a copy-pasteable manual command
  instead of doing nothing.

## Install

```bash
go install github.com/jitokim/fleetops/cmd/fleetops@latest
```

This puts a `fleetops` binary on `$GOBIN` (or `$GOPATH/bin` — usually
`~/go/bin`). Make sure that directory is on your `PATH`.

Building from a local clone instead:

```bash
git clone https://github.com/jitokim/fleetops.git
cd fleetops
make install    # go install ./cmd/fleetops — same result as above
# or: make build  → ./fleetops (a local binary, not installed to PATH)
```

## Quick start

```bash
fleetops                 # launch the fleet cockpit
fleetops hooks install   # wire up gate detection (see below)
```

`hooks install` registers four Claude Code hooks in `~/.claude/settings.json`,
backing up the existing file first: `Notification` and `PermissionRequest`
(gate detection), plus `SessionStart`/`SessionEnd` (the session-identity
registry that actuation depends on). `hooks uninstall` removes only the
entries fleetops added.

All four are **sensors**. `PermissionRequest` is the one that could be more
than that — Claude Code lets such a hook return a `permissionDecision` and
grant or deny the permission itself. fleetops writes nothing to stdout and
always exits 0. A decision made inside a hook leaves no event, no actor and
nothing to attribute or brake; decisions belong on the actuation path, which
records them.

> **Install the hooks before starting the sessions you want to act on.** The
> `SessionStart` hook records a session's identity (pid ↔ tty ↔ window) only
> when that session *starts*. A `claude` session that was already running when
> you ran `hooks install` is **observed** (fleetops reads its transcript and
> shows it in the fleet) but was never **registered**, so a typed action
> against it can't resolve an unambiguous target and falls back to a manual
> hint — you'll see something like `no unambiguous claude surface — attach (↵)
> and act manually`. That is fleetops refusing to guess which terminal to type
> into, not a crash. Restart such a session (or just act on it manually) and
> every session you start from then on registers and is actionable. Nothing
> re-registers a session retroactively.

**Try it without any real loops:** `fleetops --demo` launches the cockpit
seeded with a small synthetic fleet instead of scanning `~/.claude/projects` —
no disk reads of real session data, no writes anywhere. Useful for a first
look at the UI, or for screenshotting it without leaking real paths/goals.

## Keymap

| Key | Action |
|---|---|
| `↑` `↓` | move selection |
| `/` | filter the fleet (live, by project/session/state/stall — `esc` clears, `enter` applies) |
| `↵` | attach: bring the loop's terminal to the front |
| `a` | approve a gate (bare Enter — accepts claude's default option) |
| `r` | resume a stalled/drifted loop (re-sends its last prompt) |
| `i` | inject an arbitrary prompt into the selected loop (type it, `enter` sends, `esc` cancels) |
| `p` | stop the current turn (Esc) without killing the process |
| `k` | kill — press twice within 3s to confirm (sends `/exit`) |
| `n` | spawn a new loop: wizard for the goal/contract (plus an optional display name for the FLEET list), then a "where" step that shows the target directory (default: the dir fleetops was launched from) and lets you change it — `[w]` new worktree, `[d]` this dir, `[c]` type a path, `[s]` the selected loop's dir |
| `o` | open the session log in `less` |
| `d` | hide: tombstone the selected loop into `~/.fleetops/hidden.json` — stays hidden across restarts (doesn't touch the session or its registry entry) |
| `x` | delete — press twice within 3s to confirm: hides the loop (same as `d`) and removes its `~/.fleetops/sessions/` registry entry (conversation history untouched) |
| `q` | quit |

## Configuration

Optional. fleetops works with no config file at all — this exists for one
thing: telling it **what to run** when you press `n`.

`~/.fleetops/settings.json` (outside the repo, beside the rest of fleetops's
state, so there is nothing to gitignore and nothing you can commit by accident):

```json
{
  "spawn": {
    "command": ["claude", "--agent", "team", "--dangerously-skip-permissions"]
  }
}
```

**It is an argv array, not a command string.** That is deliberate: a string
would need a shell, and the wrapper most people want to point at is a shell
*function* (`team() { claude --agent team; }`), which only exists inside an
interactive shell — neither tmux nor a fresh iTerm2 window gives you one. An
array is passed straight to `execve`, so there is no shell, no quoting to get
wrong, and no injection surface. Write the flags out; the wrapper's body is
what you want anyway.

**`argv[0]` must be `claude`** (an absolute path to it is fine). fleetops finds
your loops by looking for a process named `claude` on a pane or tty, so
`["mise", "exec", "--", "claude"]` would start a loop it can then never reach —
`a`/`r`/`i`/`p`/`k` would all stop working on it. A config that breaks this is
ignored and the default is used.

Anything unusable — missing file, bad JSON, a `command` that is a string
instead of an array, an empty element — falls back to plain `claude` rather
than failing. A typo in an optional convenience should never stop you starting
a loop.

Two things this setting does **not** touch:

- **`fleetops --demo`** always runs plain `claude`. The demo is synthetic and
  must not depend on your machine.
- **The headless re-drive** (Tier 2, what `r`/`i` fall back to) always runs
  plain `claude`. That path also serves sessions fleetops merely *observes* —
  loops you started yourself, elsewhere — and your spawn preferences have no
  business rewriting how those are driven.

## How it works

**Observation.** `internal/claude` globs `~/.claude/projects/*/*.jsonl`, uses each
file's mtime as "last activity", and tails the last ~24KB to classify the loop:
a finished turn (`stop_reason: end_turn`) is idle regardless of how long ago
that was; mid-turn is `run` if recent, `stalled` once past a threshold (`429`
detected from the tail text). A `ps`/`lsof` cross-check (`internal/claude`'s
liveness pass) catches the case a session looks idle but its `claude` process
is actually gone — that's `✗ gone`, not idle.

**Gates.** `fleetops hook notify` writes a small marker file
(`~/.fleetops/gates/`) every time Claude Code's `Notification` hook fires.
Only markers whose `notification_type` means "blocked on a human"
(`permission_prompt`, `elicitation_dialog`, `agent_needs_input`) become a real
`◆ GATE` — the same hook also fires for the 60s "waiting for your input" idle
nudge, which is not a gate. A marker goes stale (and is deleted) once the
transcript shows new activity after it fired — the human already answered.

The gate callout says **what is being asked**, so you can judge whether a gate
deserves the interrupt without attaching to it:

- **Permission prompts** name the tool and its argument — `Bash: git push
  origin main` rather than "Claude needs your permission". That detail comes
  from the `PermissionRequest` hook, the only source that has it.
- **Questions** (`AskUserQuestion`) list the answer choices under the
  question. The choices are deliberately **not numbered**: fleetops does not
  inject answers into a gate, so a number would advertise a keystroke that
  does not exist. You still answer with `a` or by attaching.

Both hooks fire for a single permission gate, and the generic one arrives
about six seconds after the detailed one (measured). Markers are keyed by
session, so they are merged on `prompt_id` rather than overwritten — otherwise
the late generic payload would erase the tool name.

**Oracle.** Loops spawned via `n` are goal-bound: fleetops knows what they're
trying to achieve, so it can judge them. Once idle, `internal/oracle` asks a
cheap model (`claude -p --model haiku --output-format json`) to verdict the
loop's last report against its goal — never trusting the agent's own "done"
claim. `done` → `✓ DONE`; a false "done" claim → `✗ DRIFT` (re-drive with `r`,
or kill); real work with no claim either way → `progress`, state unchanged. A
no-improve counter (`N/I`) tracks consecutive rejections. Observed sessions
that weren't spawned by fleetops have no goal, so no verdict — shown as `—`.

**Budget.** The BUDGET column/bar tracks `output_tokens` only (summed across
the session) against a 2,000,000-token default cap per loop —
`input_tokens`/cache tokens re-bill the whole conversation context on every
single call, so summing those would wildly overstate spend rather than
reflect actual work done.

## Automation (opt-in, off by default)

fleetops is pure observation + one-key human actuation everywhere else in
this document. There is exactly ONE piece of automation it ships today, and
it is **opt-in and off by default**:

### 429 auto-redrive

Set `FLEETOPS_AUTO_REDRIVE_429=1` in the environment fleetops runs in to
enable it. Unset it (or don't set it at all) to disable — there is no
in-app toggle for this yet, and no per-loop override; it's a single global
switch.

**What it does:** when a loop's state transition lands it in `✗ 429`
(rate-limited) — the same scan edge that would otherwise just sit there
until you notice and press `r` — fleetops schedules ONE automatic
re-drive 5 minutes later, via the exact same headless
`claude --resume <id> -p "<prompt>"` path Tier 2 uses for a manual resume.
Before firing, it re-checks the loop's CURRENT state: if you already
resumed it yourself, or it recovered on its own, the scheduled auto-redrive
silently does nothing.

**Ceilings (both per session, not per day):**
- At most **3 lifetime attempts** — recounted from the append-only event
  log on every fleetops restart, so restarting doesn't reset the counter.
- At most one scheduled at a time — a second 429 edge for the same session
  within 5 minutes of the last schedule doesn't queue another.
- On the 3rd attempt failing, you get a desktop notification
  ("🚀 fleetops · auto-redrive exhausted") and nothing further is
  scheduled for that session.

Every attempt (success or failure) is recorded in the event log
(`actor: auto`, distinct from every human-triggered actuation event, which
is always `actor: human`) — visible in `fleetops report` and the DETAIL
panel's EVENTS block.

**Why this is the only automation, and why it's built this way — the
design rationale:**
- **Tier 2 only.** It never types into a terminal. Every OTHER actuation
  in this codebase (resume/inject/approve/kill) can fall back to typing
  into a live pane, which carries a "wrong surface" hazard if session
  identity is ambiguous. An automated, unattended action can't be allowed
  anywhere near that risk at all — Tier 2's `claude --resume <id>` is
  keyed by session id, not by a terminal pane, so there is no surface to
  get wrong.
- **Idempotent.** Re-sending a resume against a session that already
  recovered is a harmless no-op turn, not a destructive action — this is
  why 429 (a transient, well-understood failure mode) was judged safe to
  automate at all, unlike e.g. an oracle-rejected DRIFT (which needs a
  human's judgment call, see `r` on a DRIFT loop's guided re-drive) or a
  StateFailed loop (the governor already decided to stop it — automation
  overriding that would defeat the governor's whole purpose).
- **Transient, not a judgment call.** A 429 is "the API said slow down,"
  not "something is wrong with this loop's approach" — there's no
  reasoning required to decide whether retrying is appropriate, unlike
  DRIFT or a repeated no-improve stall.
- **Honest about what survives a restart.** The 5-minute delay is a
  `tea.Tick`, not a persisted job — if you quit fleetops before it
  fires, the pending retry is simply lost. No silent double-fire risk, no
  stale on-disk schedule to reconcile on the next launch.

## Engine (opt-in, off by default)

fleetops also ships a minimal **LoopEngine**: given a goal, done-condition,
and verification rubric, it can drive a loop's cycles itself instead of
waiting for you to press `r`/`i` each time. It is a **governance harness, not
an agent runtime** — it doesn't add a smarter agent, model selection, or
context management; it just automates the same "judge, then re-drive"
decision a human would otherwise make by hand, behind two explicit gates:

1. The `FLEETOPS_ENGINE=1` environment variable must be set.
2. The specific loop must have been created with the `n` wizard's
   engine-drive choice (`e`), not the default manual path.

Both are required — unset the env var and every engine-owned loop goes
inert (still observed, never driven). A loop the engine drives shows a `⚙`
marker in the fleet list and a `DRIVE` row in the detail panel.

**What it will and won't do:**
- Drives ONLY on a `progress` verdict from the oracle. A verdict the oracle
  *rejects* halts the loop (`✗ DRIFT`) for a human to look at — the engine
  never auto-re-drives a loop the oracle just rejected. This is deliberate:
  an agent that's wrong about being "done" is exactly the moment a human
  should own the next decision, not the engine.
- Respects the same budget/max-cycles/no-improve governor ceilings as
  everything else in this document — it stops or escalates rather than
  running past them.
- Never approves a gate. A live permission prompt halts it exactly like it
  halts a human-driven loop; only a human's `a` clears it.
- `↵` (attach) on an engine-driven loop is a take-over: it opens
  `claude --resume <id>` in a fresh terminal so you can drive the session
  interactively, and hands ownership back to you (the engine stops
  scheduling further cycles for it). `k` (kill) on one clears engine
  ownership instead of sending `/exit` (there's no terminal to send it to).

See [`DESIGN.md`](./DESIGN.md) for the fail-closed drive predicate and the
full state machine.

## Platform support

**macOS is the only platform the full backend matrix works on.** This isn't
just the "macOS-first" Limitations bullet below being cautious — it's the
actual state of each backend:

- **orca** ([stablyai/orca](https://github.com/stablyai/orca)) is macOS-native.
- **cmux** ([manaflow-ai/cmux](https://github.com/manaflow-ai/cmux)) is
  macOS-native.
- **tmux** is the only backend that's cross-platform by itself, but
  fleetops's own process-liveness check (`internal/claude`'s liveness pass)
  shells out to `ps axo pid,comm` and `lsof` — a BSD/macOS-flavored check, not
  Linux's `/proc`. So even the tmux path is unverified and likely degraded on
  Linux (liveness detection specifically — you may see a session reported as
  live/idle when its process actually died, or vice versa).

Net effect: a **Windows** user has no working backend at all today — you get
the bare-terminal manual-hint fallback only. A **Linux** user gets tmux, but
with unverified liveness detection. Only **macOS** gets the full matrix below.

## Backend matrix

| Backend | Attach/Resume/Approve/Stop | Spawn | Status |
|---|---|---|---|
| **orca** | ✅ | ✅ | Verified live against the real CLI; preferred when available. |
| **tmux** | ✅ | ✅ | Verified against tmux's documented command contract. |
| **cmux** | ✅ (locate/send/focus/approve/interrupt) | ❌ | Verified against real **cmux 0.64.15**: `tree --json` shape, and `send`/`send-key`/`focus-panel` subcommands — including **cross-workspace addressing**: each actuation now passes `--window <ref>` so a surface outside the caller's own workspace is reachable (verified live; without it a cross-workspace target failed). Its tree carries no cwd, so surface→dir matching cross-references the OS by tty (`ps`+`lsof`). Spawn is still unsupported (no verified create-surface command). |
| **iTerm2** (no multiplexer needed) | ✅ | ✅ | The terminal emulator itself, not a multiplexer — reached through iTerm2's AppleScript `write` over `osascript`, keyed by the session GUID the hooks record. Measured live: delivery works to a **background, non-frontmost** window without raising it, `newline yes` emits a real CR, and a raw ESC survives. Before writing, the script re-reads the session's own `tty` and **refuses** if it disagrees with the registry — so a loop inside tmux-inside-iTerm2 is never typed into at the wrong layer; that hazard is measured, not assumed (the enclosing iTerm2 session and the tmux pane inside it really do report different ttys). `a` (approve) is verified end-to-end against a real permission prompt: the bare submit accepts the default option and the turn proceeds. Caveat: iTerm2 marks AppleScript deprecated in favour of its Python API, so if `write` ever disappears `r`/`i` fall back to the headless re-drive while `k`/`p`/`a` have no fallback and go dead. Spawn opens a fresh iTerm2 window and starts the loop there, with `[w]` producing a real git worktree branched from a freshly-fetched `origin/<default>`. Two spawn caveats: creating a window **takes focus** (iTerm2 has no non-activating equivalent of `tmux new-window -d` — measured, and re-selecting the cockpit window afterwards does not bring it back), and spawn is only offered when the cockpit is itself running inside iTerm2. |
| **bare terminal** (none of the above) | manual hint only | manual hint only | Observation still works fully; actions print a copy-pasteable command (`claude --resume <id>`, `cd <dir>`, etc.) instead of silently failing. |

The cmux backend requires a separately-installed CLI
([manaflow-ai/cmux](https://github.com/manaflow-ai/cmux)) that is
**independently licensed AGPL-3.0-or-later as of 2026-02-14** — this doesn't
affect fleetops's own MIT license (the same relationship as any tool that
shells out to `git`), but know what you're installing before you opt into
that backend.

`internal/control.Resolve()` picks the first available backend in that order
(orca → cmux → tmux) — but only for *creating* things (spawning a loop,
opening a terminal). Typed actions don't use it: they probe every available
backend and pick whoever can actually reach your session, so a loop living in
a tmux pane is still reachable on a machine where orca happens to be
installed and preferred.

### Capability tiers (session identity, not just backend)

Since the session-identity registry (`fleetops hooks install`'s
`SessionStart`/`SessionEnd` hooks), which terminal/backend hosts a session
matters less than what fleetops actually *knows* about it. Every typed
action (resume/approve/stop/kill/inject) now resolves in tiers, falling
through automatically:

1. **Tier 1a — registry tty (tmux/cmux).** If the session registry has a
   live tty for the loop (written at `SessionStart`, re-validated against a
   live `ps` at actuation time), fleetops dispatches straight to that pane by
   tty — session-unique, so two loops sharing a project directory are no
   longer ambiguous to each other. Every backend that can address a surface by
   tty is probed, not just the preferred one; today that's tmux and cmux
   (orca's CLI doesn't expose a per-terminal tty, so orca loops land in 1b).
2. **Tier 1h — host-terminal send (iTerm2).** If the loop isn't in a
   multiplexer but *is* hosted in a terminal fleetops can script, the host
   writes into the session in place — for iTerm2, via its AppleScript `write`
   over `osascript`. This is what gives `p` (interrupt), `k` (kill) and `a`
   (approve) a path on iTerm2, where they previously had none: they're
   Tier-1-only verbs with no headless equivalent. It needs **no multiplexer at
   all** — a plain macOS + iTerm2 machine with no orca/tmux/cmux still gets
   these keys. Ordered after 1a on purpose, so a tmux/orca/cmux session running
   *inside* an iTerm2 window is still addressed by its exact pane rather than
   by whatever pane the window happens to be showing.
3. **Tier 1b — cwd-based match (orca/cmux/tmux).** The existing
   `Locate`/`LocateClaude` chain, unchanged — still guarded against
   ambiguity when more than one loop shares a directory. For an **idle or
   stalled** loop that guard is no longer a dead end: `i` (inject) lets you
   type the prompt anyway and routes it via Tier 2's session-unique headless
   re-drive (exact session id — it cannot hit a sibling). A **running**
   (mid-turn) loop still refuses, because a concurrent headless turn against
   a live interactive session is unverified territory — attach (`↵`) and act
   there instead. Because a directory can be matched by more than one backend
   at once, this tier refuses on cross-backend ambiguity too, not just within
   a single backend.
4. **Tier 2 — headless re-drive (every backend, every host).**
   `claude --resume <id> -p "<prompt>"` continues the SAME session as a
   fresh background turn, appending to the SAME transcript the cockpit
   already tails. No terminal surface needed at all — this is what makes a
   loop whose terminal died (`process gone`) resumable again, and it's the
   only path available for a session with no multiplexer backend (a bare
   terminal, an IDE-hosted shell, etc.) once Tier 1 can't find a surface.

See `docs/adr-vendor-independent-actuation.md` for the full design and
verification notes.

## Limitations

- **No goal, no oracle.** A session fleetops didn't spawn has no recorded
  goal, so it can never show `DONE`/`DRIFT`/`N-I` — only the tail-based
  state (`run`/`idle`/`stalled`/etc).
- **cmux: verified shape, partially-verified actuation.** The `tree --json`
  parser was verified against real cmux **0.64.15** (surface identity is the
  `ref` key; terminal surfaces carry `type:"terminal"` + `tty`; browser tabs
  carry `tty:null`), and the parser stays tolerant of unknown shapes. Because
  the tree has no cwd field, a surface's directory is resolved by
  cross-referencing its tty against the OS process table (`ps`+`lsof`), the
  same pattern `internal/claude` uses. `send`/`send-key`/`focus-panel` and the
  `enter` key token are verified from the CLI's own help; the `escape` (Esc /
  interrupt) key token and the end-to-end effect of Resume/Approve on a live
  claude gate are still assumed (no live claude-in-cmux session was safe to
  drive). Verified on this cmux version only, not all cmux releases.
- **cmux: cross-workspace addressing (`--window`).** A `--surface`/`--panel`
  ref is resolved by cmux within a *window* context; with `--window` omitted
  that context defaults to the caller's own workspace (`$CMUX_WORKSPACE_ID`),
  so a surface in **any other workspace** silently fails ("Surface not found" /
  "Surface is not a terminal") — the earlier "verified subcommands" work only
  covered the flags, not this addressing gap. Locate/LocateClaude now capture
  each surface's enclosing `window:<n>` ref while walking the tree
  (`Target.Window`), and every actuation appends `--window <ref>` when it is
  set. This was reproduced and fixed live on cmux 0.64.15 for all three
  subcommands by driving a throwaway cross-workspace surface: each failed
  without `--window` and succeeded with it, correctly targeting the
  other workspace. A **same-workspace** target accepts `--window` as a verified
  no-op, so there is no regression. orca/tmux leave `Target.Window` empty (their
  handles/pane-ids are already globally addressable), so their argv is unchanged.
- **macOS-first.** See "Platform support" above — orca and cmux are
  macOS-native, and even the cross-platform tmux backend's liveness check is
  unverified on Linux and has no working path on Windows.
- **The engine is intentionally minimal.** See "Engine" above — it's a
  governance harness (judge, then re-drive on progress), not an autonomous
  agent runtime. No self-chaining (it drives at most once per scan tick),
  no challenger execution, no per-loop model/permission config. See
  [`VISION.md`](./VISION.md) for the longer-term direction this project is
  named for — that document describes a more expansive future, not current
  behavior.

## Known rough edges

Honest framing, not spin: this codebase's real-world actuation testing is
very fresh and thin, and most of what's caught it so far has been manual
dogfooding, not test coverage or review.

The clearest example: the cmux backend's `Locate`/`LocateClaude` (surface
lookup) and its cross-workspace actuation (the `--window` addressing
described above) were **both completely non-functional against the real cmux
CLI** — not degraded, not flaky, but dead-on-arrival — until two same-day
bugfixes landed. Both bugs shipped, then were caught and fixed the same day
they were dogfooded, not by a test or a review catching them beforehand. If
you're relying on the cmux backend for anything you care about, be aware that
"verified against real cmux X.Y.Z" in this README means "worked when someone
manually drove it that day," not "covered by regression tests" — there isn't
CI or a test harness that exercises the real CLI yet.

More generally: everything under "Limitations" above that says "verified
live" or "reproduced and fixed live" was verified by a human manually driving
the real CLI once, on one version, not by automated tests. Treat those
claims as "known to have worked at some point," not "guaranteed to keep
working."

**`no unambiguous claude surface` on a typed action.** The most common cause
is not a bug: the target session isn't in the identity registry, so fleetops
won't guess which terminal to type into. It happens when the session started
before `hooks install` ran (see "Install the hooks before starting the
sessions you want to act on," above), or when it's a headless `claude -p`
session that never had a terminal surface. A session with a genuinely stale
binding — e.g. an iTerm2 window reopened after a restart, so the recorded
`window_id` no longer matches — can also land here; that case is tracked and
still being hardened. Either way the fallback is honest: it refuses to act
rather than act on the wrong session.
