# Security Policy

## What makes fleetops security-relevant

fleetops is not a passive dashboard. Its actuation layer
(`internal/control` — the orca/tmux/cmux backends) **sends real keystrokes
into, and can kill, real terminal processes** on your machine: attaching to a
session, approving a gate, resuming a stalled loop, or the `k` kill action all
resolve a target session to a live PID or terminal-multiplexer pane and act on
it directly. Observation reads Claude Code's session transcripts under
`~/.claude/projects/` and cross-references OS process state (`ps`, `lsof`) to
tell a live session from a dead one.

That means the realistic vulnerability classes for this project are not the
usual web-app ones. They're things like:

- **Wrong-target actuation**: a bug in surface/session resolution (e.g. tty
  matching, cmux workspace addressing, orca handle lookup) that causes an
  action meant for session A to be sent to session B — potentially killing
  the wrong process or injecting keystrokes into the wrong terminal.
- **Injection via untrusted input**: anything that lets content from a
  session's transcript, a hook payload, or a process listing influence what
  command fleetops runs or what gets sent to a terminal, instead of being
  treated as inert data to observe.
- **Privilege/scope escape**: an action reaching a process or session outside
  what the user selected in the fleet table.
- **Hook installation risk**: `fleetops hooks install` edits
  `~/.claude/settings.json`. A bug here could corrupt that file or wire up a
  hook that behaves unexpectedly (it does back up the existing file first —
  a regression in that backup path is itself worth reporting).

If you find a bug in one of these areas — even one that "just" causes a
wrong-but-harmless action rather than something you can weaponize — please
report it privately rather than filing a public issue. Given the actuation
surface, we'd rather over-index on caution here than have a wrong-target-kill
bug sit in a public tracker while it gets fixed.

## Reporting a vulnerability

Preferred: open a [GitHub private security advisory](https://github.com/jitokim/fleetops/security/advisories/new)
for this repository. This reaches the maintainer without disclosing details
publicly and gives you a private thread to share reproduction steps.

If you can't use GitHub's advisory flow, email the commit author address
listed in `git log` for this repository (`pigberger70@gmail.com`) with a
subject line starting `[fleetops security]`.

Please include:
- What backend/action was involved (orca / tmux / cmux / bare terminal;
  attach / resume / approve / stop / kill / spawn).
- Steps to reproduce, including relevant versions (fleetops commit, OS,
  and the version of whichever backend CLI is involved — this matters a lot
  here, see the README's backend-matrix caveats about version-specific
  behavior).
- Impact as you understand it (e.g. "sent keystrokes to an unintended
  terminal" vs. "crashed fleetops").

## What to expect

This is a small, alpha-stage (`0.1.0-alpha`), single-maintainer open-source
project — there's no formal SLA. As a working target: acknowledgment within a
few days, and a fix or mitigation plan communicated before any public
disclosure. Given the actuation risk described above, reports that involve
wrong-target actuation or process control will be prioritized over
cosmetic/observation-only bugs.

## Scope

In scope: fleetops's own code (`cmd/`, `internal/`) — actuation logic,
session/process resolution, hook installation, transcript parsing.

Out of scope: vulnerabilities in the backend CLIs themselves (`orca`, `tmux`,
`cmux`) or in Claude Code — please report those to their respective
maintainers. If you're unsure which side a bug is on, report it here and
we'll help route it.
