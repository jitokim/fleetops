# ADR: vendor-independent session discovery via Claude Code hooks

- status: proposed (research complete, not yet implemented)
- date: 2026-07-16
- context: two independent design reviews (opus + fable), both with live
  verification on the captain's machine — see "Verification" below

## 1. Problem

`internal/control`'s three backends (orca, cmux, tmux) each work the same
way: shell out to that vendor's CLI, ask it to enumerate its terminal
surfaces, and pattern-match a surface whose `cwd` equals the loop's project
directory. Two structural faults fall out of this directly:

- **Discovery is outsourced to undocumented vendor internals.** cmux's
  `tree --json` carries no cwd at all — missionctl already reverse-resolves
  it via `ps`+`lsof` keyed on tty. Both of today's cmux P0s (dead
  `Locate`/`LocateClaude`, then broken cross-workspace `--window`
  addressing) lived entirely in this reverse-engineered layer. There is no
  upstream contract; a vendor version bump can silently break it again.
- **The join key (cwd) is wrong.** Directory is many-to-one — two sessions
  in the same repo are indistinguishable by cwd, which is exactly why
  `refuseIfAmbiguous`/`LocateClaude`'s "&gt;1 match" refusal exists as a
  safety backstop. The wrong-terminal hazard those guards defend against is
  a direct consequence of matching on the wrong key.
- **Coverage is a hardcoded enum, not a capability.** `Resolve()` picks
  orca→cmux→tmux. A fourth terminal host (e.g. IntelliJ's built-in
  terminal) isn't degraded, it's invisible — none of the three vendors
  enumerate it, and JetBrains exposes no "send text to terminal tab N" API
  to build a fourth integration against in the first place. Reproduced live
  today: `i`-key inject into an IntelliJ-hosted session fails with
  `no unambiguous claude surface`.

## 2. Decision

Adopt a **hook-based session registry** as the primary discovery mechanism,
replacing per-vendor surface enumeration. Keep the existing vendor CLIs
(orca/cmux/tmux) only for the narrow thing they uniquely provide — sending
input into a pty they own — not for discovery.

### 2.1 The registry

Claude Code already runs hooks as direct children of the `claude` process
itself, regardless of which terminal hosts it (`cmd/missionctl/hook.go`'s
`hookPayload` already parses `session_id`/`cwd` from the `Notification`
hook). Installing an additional `SessionStart` (+`SessionEnd`) hook gives
missionctl, for every session in every terminal:

```
session_id (hook stdin) → os.Getppid() → claude's pid → ps -o tty= → tty
```

Verified live (both independent reviews, macOS 15.7.4 / claude 2.1.x,
~10-11 real sessions observed and never disturbed):

- `SessionStart` fires for interactive **and** `-p` sessions; payload
  includes `session_id`, `cwd`, **`transcript_path`** (a direct link to the
  JSONL — no globbing needed), `source`, `model`.
- The hook's parent pid resolves directly to the `claude` process (a
  single-command `sh -c` wrapper exec-optimizes away). This holds even
  though claude spawns hooks via `setsid` (new session, no controlling
  terminal of its own) — `setsid` changes the session, not the ppid, so the
  `getppid()` chain survives it.
- `ps -o tty= -p &lt;pid&gt;` reliably resolves a real tty for interactive
  sessions. Piped/headless sessions (`-p`, or a stream-json daemon) have no
  controlling tty (`??`) — expected, and fine: those aren't human-driven
  anyway, so they were never actuation targets.
- **External validation for free:** cmux itself is already built on this
  exact primitive — its own live `--settings` registers `SessionStart` et
  al., and it hosts claude on a real tty via a wrapper. The vendor we're
  trying to stop depending on already depends on the same primitive we're
  proposing.

This is the same on-disk-marker idiom `internal/gate` already uses
(`gate.WriteMarker` → `~/.missionctl/gates/`), same "swallow errors, exit 0,
never break the user's session" discipline as `notifyHook`. The registry
lives at `~/.missionctl/sessions/&lt;session_id&gt;.json`.

**What this deletes/shrinks:** cmux's `tree --json` parsing and
`liveResolveCmuxTTYs`; the cwd-encode-and-heal heuristics in
`internal/claude/scan.go`'s `applyLiveness`; the cwd-based matching tier in
all three backends' `Locate`/`LocateClaude`. `refuseIfAmbiguous`'s
"&gt;1 match" refusal becomes largely dead weight — tty is session-unique
where cwd was many-to-one, so two sessions sharing a project directory stop
being ambiguous. Keep a thin version as defense-in-depth, not the primary
guard.

### 2.2 Actuation: three tiers, not one hardcoded `Resolve()`

Discovery being solved does **not** solve actuation (injecting keystrokes
into a tty missionctl doesn't own). Both reviews independently tested the
one primitive that would have made this vendor-independent too, and both
killed it:

**TIOCSTI is dead. Do not pursue.** `ioctl(fd, TIOCSTI, &char)` pushes a
character into a tty's input queue as if typed, independent of which
terminal emulator owns it — in theory the actual vendor-independent
actuation primitive. Verified live on macOS: injection into a
**cross-process** tty (i.e. any tty missionctl itself didn't open as its
own controlling terminal) fails with **EPERM**, same uid, no root
available. One review also confirmed same-tty/self-owned injection *does*
work non-root — but that's not the missionctl use case, which is always
reaching into another process's terminal. Linux independently disables this
by default since kernel 6.2 (`CONFIG_LEGACY_TIOCSTI`), and required
`CAP_SYS_ADMIN` even before that. Dead on both of README's target
platforms. Closed, not a future option.

Given that, actuation is reclassified into capability tiers, selected per
session from the registry entry rather than one global `Resolve()`:

- **Tier 0 — universal (every host, including IntelliJ/Terminal.app/iTerm2/
  VSCode).** Observation, gates, oracle, budget/cycle tracking, and the
  existing copy-pasteable manual-hint fallback. Powered entirely by the
  registry, zero vendor dependency. IntelliJ lands here — that's the
  correct outcome, not a gap still to close: there is no reliable OS-level
  mechanism to reach a pty missionctl doesn't own, on a terminal with no
  automation API. This is an OS boundary, not a missing integration.
- **Tier 1 — pty-owning multiplexers (tmux/orca/cmux), demoted not
  dropped.** These keep their `send`/`send-key`/`focus-panel`/`resume`
  calls — a small, `--help`-checkable, stable verb surface — now dispatched
  by tty (from the registry) instead of by cwd-guessing. The maintenance
  burden that produced both P0s lived in *discovery*, not these calls;
  removing discovery removes the burden without needing to drop working
  actuation. tmux is the most-trusted member of this tier (documented,
  stable contract, no reverse-engineering ever required); orca and cmux
  stay first-class but explicitly best-effort against undocumented CLIs.
- **Tier 2 — vendor-independent re-drive via `claude --resume &lt;id&gt; -p
  "&lt;prompt&gt;"`.** Rather than typing into the human's on-screen TUI,
  continue the session as a fresh headless turn against the same persisted
  transcript. This works for **every** host, including IntelliJ, with zero
  tty injection and zero multiplexer dependency — it's the actual
  vendor-independent actuation path, just not "in-place." Best fit for
  "resume a stalled/idle/drifted loop," which is most of what the `r`/`i`
  keys are for today. **Flagged as verified-from-docs, not live-exercised
  in either research spike — confirm the end-to-end behavior (does it
  correctly append to the same transcript the TUI is tailing, does the
  running human-facing session see the result) before committing to it.**
  missionctl-spawned (engine-owned, future) loops get guaranteed control
  for free via their own pty ownership — folds naturally into VISION.md's
  LoopEngine rather than needing separate design work now.

Rejected: AppleScript/Accessibility-API keystroke simulation (requires a
focused window + an Accessibility grant, fragile, and wrong shape for
background fleet actuation across many terminals at once).

## 3. Migration (phased, non-breaking)

1. **Additive.** Extend `hooks install` (today wires only `Notification`)
   to also register `SessionStart`/`SessionEnd`. New `missionctl hook
   session-start` subcommand mirrors `hook notify`'s contract exactly
   (read stdin, resolve tty, write registry entry, always exit 0). Nothing
   removed yet; IntelliJ/bare-terminal sessions become observable
   immediately. Users re-run `missionctl hooks install` (existing
   backup-safe pattern) to pick it up.
2. **Rewire actuation onto the registry.** Strip cwd-matching/discovery out
   of `orca.go`/`cmux.go`/`tmux.go`; backends shrink to pure send adapters
   keyed by a `Target` sourced from the registry. Re-validate `tty↔pid`
   against live `ps` at actuation time, not a possibly-stale registry
   record (ttys are OS-recycled) — the wrong-terminal safety property must
   survive the migration unchanged.
3. **Bridge for pre-existing sessions.** Sessions already running when a
   user upgrades have no registry entry until restarted. Keep a reduced
   `ps`+`lsof` backfill scan (the one piece of today's `internal/claude/
   procs.go` worth retaining) so already-live sessions aren't regressed to
   invisible during the transition.
4. **Prune dead entries.** `SIGKILL` skips `SessionEnd`, so entries can
   leak. Validate each registry entry against `ps -p &lt;pid&gt;` at read
   time — registry is authoritative for *identity*, `ps` stays authoritative
   for *liveness*.

**What's user-visible:** IntelliJ/bare/iTerm sessions gain full observation
+ gate detection + Tier-2 re-drive immediately (net new capability, not a
regression anywhere). `hooks install`/`uninstall` now manage three hook
entries instead of one. Users who skip reinstalling hooks fall back to
today's `ps`/`lsof`-only behavior, not to blindness. README's backend
matrix needs reframing from "which vendors are integrated" to "which
capability tier does this session get."

## 4. Confirmed-live vs inferred (both reviews' honesty ledger, merged)

**Confirmed live, independently, by both reviews:**
- `SessionStart` fires with `session_id`/`cwd`/`transcript_path`; hook
  parent pid resolves to the real `claude` process; `ps -o tty=` resolves a
  real tty for interactive sessions.
- TIOCSTI fails cross-process (EPERM) on macOS, non-root.
- cmux's own hook config proves the registry primitive is already
  load-bearing in production, just not for our benefit yet.

**Inferred / not live-exercised — verify before building on:**
- `claude --resume &lt;id&gt; -p "&lt;prompt&gt;"` end-to-end behavior against a
  session the TUI is actively tailing (Tier 2's core mechanism).
- Linux `TIOCSTI`/`CONFIG_LEGACY_TIOCSTI` state (no Linux box in either
  spike; from public kernel history).
- Whether orca's CLI exposes a per-terminal tty directly (would let orca
  join the clean tty-based path instead of staying cwd/title-matched
  best-effort).

## 5. Alternatives considered and rejected

- **Add a 4th (IntelliJ) vendor integration.** Rejected: IntelliJ's terminal
  API is **plugin-only** — no externally-callable automation surface exists
  (confirmed against the JetBrains Platform SDK; same story for VS Code's
  `vscode.Terminal.sendText()`, which is extension-host-only per Microsoft's
  own maintainers, [vscode#115162](https://github.com/microsoft/vscode/issues/115162)).
  A real integration would mean writing and shipping our own IDE plugin —
  repeats the cmux reverse-engineering cost for strictly worse ROI, and
  still wouldn't cover the 5th, 6th terminal.
- **Drop orca/cmux actuation entirely, tmux-only.** Rejected as premature:
  the P0-prone part was discovery, which the registry removes. Their
  remaining actuation verb surface is small and stable; dropping working
  code isn't justified once the fragile part is gone. (Also: this is the
  ecosystem's default convention — see §6 — but missionctl's own preferred
  environment is orca, so full tmux-only isn't actually on the table here.)
- **missionctl-owned pty for all sessions (Direction C, generalized).**
  Rejected as the near-term fix: doesn't help a session a human already
  started in IntelliJ. Correct as the engine's long-term model for
  loops missionctl itself spawns (VISION.md), not a substitute for
  discovery+Tier 0/2 on human-started sessions.
- **A 4th actuation tier via terminal-emulator-native remote-control APIs**
  (kitty's documented RPC protocol, WezTerm's `cli send-text`, iTerm2's
  Python API — all real, externally-callable, no TIOCSTI-style hackery
  needed). Deferred, not rejected: technically sound and would extend
  in-place actuation to users of those terminals, but today's missionctl
  users are on orca/cmux/tmux, not kitty/WezTerm/iTerm2. Gate this behind
  actual user demand rather than building speculatively (simplicity-first).
  Alacritty/Ghostty/Windows Terminal/Terminal.app either have no comparable
  API or only ad-hoc AppleScript-level text injection, not worth chasing.

## 6. Prior art

- **[Fleet](https://github.com/nicknisi/fleet)** (a tmux dashboard shipped
  as a Claude Code plugin) independently converged on the same core idiom
  this ADR proposes: install Claude Code hooks (`Notification`,
  `PreToolUse`, `Stop`, `SubagentStop`, `SessionEnd`) that fire on every
  event and write identity+state to a status file, discovered passively
  rather than requiring sessions to be launched through the tool itself.
  This is real external validation — "hook as ground-truth identity
  source, filesystem as registry" is a sound idiom arrived at
  independently, not a missionctl-specific idea. Fleet's registry key is
  the **tmux pane id**, though, not pid/tty — it solves discovery *within*
  one tmux server, not across heterogeneous terminal hosts, which is
  exactly the generalization §2.1's `ppid→pid→tty` chain is for. Fleet also
  keeps a `ps`-table fallback mapping known agent binaries back to tmux
  panes for zero-install discovery — independent confirmation that this
  ADR's §3 "bridge for pre-existing sessions" `ps`+`lsof` backfill is a
  sound pattern, not a hack.
- **[ccmanager](https://github.com/kbwo/ccmanager)** sidesteps discovery
  entirely by owning its own pty end to end ("no tmux dependency") — but
  only works because it IS the thing that spawns every session; it can't
  see a session a human started organically elsewhere. This is exactly
  §5's rejected "Direction C, generalized," independently arrived at and
  independently hitting the same limitation.
- **The ecosystem convention is "pick tmux, full stop."** claude-squad,
  agent-of-empires, amux, dmux, muxtree, Tmux-Orchestrator,
  tmux-agent-status — all tmux-only, and (except Fleet) all require
  sessions be spawned through the tool's own wrapper rather than
  discovering pre-existing ones. Outside the AI-agent niche,
  overmind/tmuxinator/teamocil show the identical convention. **No other
  project attempts an N-way multiplexer abstraction like missionctl's
  orca/cmux/tmux layer** — most don't even reach for it. This ADR's Tier
  0/1/2 design is more ambitious than the field, not merely consistent
  with it; worth staying honest with ourselves about that added surface
  area every time a vendor's CLI shifts under us again.
