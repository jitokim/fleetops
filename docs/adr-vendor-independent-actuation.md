# ADR: vendor-independent session discovery via Claude Code hooks

- status: **landed** — the session-identity hook registry and registry-keyed
  actuation (Tier 2 re-drive) described below are both implemented; see §3's
  migration steps 1–2.
- context: this design was validated with live testing against real
  terminals and multiplexer CLIs — see §4 for what was confirmed live vs.
  inferred from documentation.

**Scope discipline, read this first:** "vendor-independent" in this ADR's
title refers specifically to *discovery* (§2.1's hook registry — this part
genuinely works for any terminal, no exceptions, no curated list needed).
It does **not** mean fleetops aims to actuate into literally any terminal
that exists. Actuation stays a small, explicit, curated backend list —
the same shape as how e.g. opencode maintains a fixed supported-model list
rather than an open-ended "works with any LLM" claim, instead of adding a
provider on spec. v1's list is orca/cmux/tmux (already built). A backend
gets added to the list when a real user asks for it, not speculatively —
see §5's kitty/WezTerm/iTerm2 entry (deferred, not rejected, gated behind
demand) and §6's finding that the wider ecosystem converges on exactly this
"pick a short list, full stop" pattern. Everything outside the list is
Tier 0 (observation + gate detection + manual-hint fallback) **by design**,
not a gap still to close — see §2.2.

## 1. Problem

`internal/control`'s three backends (orca, cmux, tmux) each work the same
way: shell out to that vendor's CLI, ask it to enumerate its terminal
surfaces, and pattern-match a surface whose `cwd` equals the loop's project
directory. Two structural faults fall out of this directly:

- **Discovery is outsourced to undocumented vendor internals.** cmux's
  `tree --json` carries no cwd at all — fleetops already reverse-resolves
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
itself, regardless of which terminal hosts it (`cmd/fleetops/hook.go`'s
`hookPayload` already parses `session_id`/`cwd` from the `Notification`
hook). Installing an additional `SessionStart` (+`SessionEnd`) hook gives
fleetops, for every session in every terminal:

```
session_id (hook stdin) → os.Getppid() → claude's pid → ps -o tty= → tty
```

Verified live (macOS 15.7.4 / claude 2.1.x, ~10-11 real sessions observed
and never disturbed):

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
(`gate.WriteMarker` → `~/.fleetops/gates/`), same "swallow errors, exit 0,
never break the user's session" discipline as `notifyHook`. The registry
lives at `~/.fleetops/sessions/&lt;session_id&gt;.json`.

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
into a tty fleetops doesn't own). The one primitive that would have made
this vendor-independent too was tested and ruled out:

**TIOCSTI is dead. Do not pursue.** `ioctl(fd, TIOCSTI, &char)` pushes a
character into a tty's input queue as if typed, independent of which
terminal emulator owns it — in theory the actual vendor-independent
actuation primitive. Verified live on macOS: injection into a
**cross-process** tty (i.e. any tty fleetops itself didn't open as its
own controlling terminal) fails with **EPERM**, same uid, no root
available. Same-tty/self-owned injection *does*
work non-root — but that's not the fleetops use case, which is always
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
  mechanism to reach a pty fleetops doesn't own, on a terminal with no
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

  **Backend selection is locate-based, not install-order-based (revised).**
  The original Tier-1 dispatch tied each actuation to whichever single backend
  `Resolve()` picked in fixed install order (orca→cmux→tmux). On a machine
  where orca is always installed/available, that made orca always win even for
  a loop physically hosted in a tmux/cmux surface orca cannot Locate — attach
  then failed outright, and Tier 1a silently skipped a session hosted in a
  non-preferred backend. Revised: every actuation resolver now probes across
  **all available backends** and selects by who can actually reach the
  surface, not by install order. Attach (`ResolveForLocate`) takes the first
  backend that Locates and is permissive (first-by-order wins on ties, never
  refuses). The typed/destructive path (`ResolveActuationTarget`) probes every
  available backend's `LocateByTTY` (Tier 1a, first hit wins — tty is
  session-unique) and every available backend's `LocateClaude` (Tier 1b,
  counting matches and **refusing on cross-backend ambiguity** — ≥2 distinct
  backends matching the same cwd). `Resolve()` itself is unchanged and remains
  the creation/capability resolver (spawn, terminal-open, capability checks),
  where install order is the correct tiebreak.
- **Tier 2 — vendor-independent re-drive via `claude --resume &lt;id&gt; -p
  "&lt;prompt&gt;"`.** Rather than typing into the human's on-screen TUI,
  continue the session as a fresh headless turn against the same persisted
  transcript. This works for **every** host, including IntelliJ, with zero
  tty injection and zero multiplexer dependency — it's the actual
  vendor-independent actuation path, just not "in-place." Best fit for
  "resume a stalled/idle/drifted loop," which is most of what the `r`/`i`
  keys are for today; also the sole actuation path for a headless,
  engine-driven loop (see DESIGN.md), which never has a terminal surface
  at all. See §4 for the end-to-end verification of this mechanism.
  fleetops-spawned engine-owned loops get guaranteed control for free via
  their own headless-bootstrap ownership — folds naturally into VISION.md's
  LoopEngine rather than needing separate design work now.
  **(Superseded — see the 2026-07-19 amendment immediately below.)**

**Superseded (amendment, 2026-07-19) — "guaranteed control for free" and "no
separate design work now" were both wrong.** Recorded rather than deleted
because the reasoning that produced them is the reasoning this repo keeps
failing at: it treats "fleetops created it" as a fact the actuation path can
still consult later. It cannot, because nothing durable records it.

- **"For free" was never bought.** The ownership fast paths that skip terminal
  resolution and the ambiguity guard (`killCmd`'s `if l.Driven` arm and the `k`
  handler's) are gated on **one** flag, `domain.Loop.Driven`, copied in from
  `registry.Record.Driven`. That flag is **opt-in at the `n` wizard**
  (`registry.BindSpec.Driven` defaults false, so `[m]` yields a fleetops-spawned,
  contract-bound loop with `Driven == false`) and is **cleared by both take-over
  and kill** (`registry.MarkDriven(..., false)`). An ordinary human take-over of a
  loop fleetops itself spawned therefore drops it back onto the cwd-guessing
  path and, on a directory shared by several sessions, into the ambiguity
  refusal — for a surface fleetops opened. `Driven` is doing three jobs at once
  (origin ∧ engine authority ∧ ¬taken-over), and only the middle one warrants
  being cleared.
- **"For free" also over-reads this tier.** Tier 2 is a *re-drive* path; `k`/`p`/
  `a` have no Tier 2 equivalent at all — a point §4's iTerm2 ledger already makes
  independently. Headless-bootstrap ownership cannot confer "guaranteed control"
  over verbs this tier does not serve.
- **The design work was owed and has now been done.** See
  [`docs/adr-loop-state-model.md`](./adr-loop-state-model.md), which was derived
  from a live incident on `544c27c`: a fleetops-spawned loop hit its ceiling, its
  oracle rejected it 11×, its process exited, the cockpit still read `✗ DRIFT`,
  and `k` refused with a cwd-ambiguity message advising `fleetops hooks install`
  — a remedy that can neither retroactively register a running session nor help a
  dead one. That ADR's §1.2 is this bullet's root cause; its §2.4 (spawn-time
  surface binding, "Tier 1s") is the tier this bullet assumed already existed
  implicitly, deliberately placed **between 1a and 1b** rather than at the top,
  for exactly this ADR's §3 step 2 reason: provenance is not a substitute for
  re-proving the binding against live `ps` at actuation time.

Nothing about Tier 2 itself is withdrawn — the mechanism verified in §4 stands
unchanged. What is withdrawn is the claim that ownership needed no design.

**Rejected: Accessibility-API keystroke simulation** (`System Events`'
`keystroke`/`key code`, or the equivalent CGEvent posting). It synthesizes
*system-wide* key events, so it requires an Accessibility grant
(`kTCCServiceAccessibility`) **and** delivers to whatever window currently
holds key focus — meaning the target must be frontmost. Fragile, and the wrong
shape for background fleet actuation across many terminals at once.

**Clarified (amendment, 2026-07-18) — this rejection is about *keystroke
simulation*, not about AppleScript as a transport.** An earlier revision wrote
"AppleScript/Accessibility-API keystroke simulation," which can be misread as
rejecting AppleScript generically. It does not, and in fact never did in
practice: `internal/control/focus.go`'s shipped iTerm2 attach path already
drives AppleScript via `osascript`, and the ADR was simply never updated to say
so. The material distinction is:

|  | Accessibility keystroke simulation (**rejected**) | App-level scripting command (**permitted, curated**) |
|---|---|---|
| example | `tell application "System Events" to keystroke "x"` | `tell application "iTerm2" … write <session> text …` |
| TCC grant | Accessibility (system-wide input synthesis) | Automation, per target app |
| needs focus? | **yes** — goes to the focused window | **no** — takes an explicit session specifier |
| targeting | implicit (whatever has focus) | explicit, session-unique, and **verifiable** (iTerm2's `session` exposes a read-only `tty`) |
| fit for background fleet actuation | wrong shape | correct shape |

Verified live 2026-07-18: enumerating iTerm2 sessions and reading their
`id`/`tty` over `osascript` raised no Accessibility prompt and did not bring
iTerm2 to the front. Delivery to a background, non-frontmost window was
separately measured and passed (see §4's iTerm2 ledger entry).

An app-level scripting command therefore does not fall under this rejection. It
remains subject to the same curated-list discipline as every other backend:
added when a real user asks, never on spec.

## 3. Migration (phased, non-breaking)

1. **Additive.** Extend `hooks install` (today wires only `Notification`)
   to also register `SessionStart`/`SessionEnd`. New `fleetops hook
   session-start` subcommand mirrors `hook notify`'s contract exactly
   (read stdin, resolve tty, write registry entry, always exit 0). Nothing
   removed yet; IntelliJ/bare-terminal sessions become observable
   immediately. Users re-run `fleetops hooks install` (existing
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

## 4. Confirmed-live vs inferred (an honesty ledger)

**Confirmed live:**
- `SessionStart` fires with `session_id`/`cwd`/`transcript_path`; hook
  parent pid resolves to the real `claude` process; `ps -o tty=` resolves a
  real tty for interactive sessions.
- TIOCSTI fails cross-process (EPERM) on macOS, non-root.
- cmux's own hook config proves the registry primitive is already
  load-bearing in production, just not for our benefit yet.
- `claude --resume &lt;id&gt; -p "&lt;prompt&gt;"` — Tier 2's core mechanism: the
  resumed turn recalls prior context, returns the SAME session_id (no
  fork), and appends to the same JSONL transcript the cockpit tails
  (file observed growing 57.6k→61.9k with the new turn). Remaining
  nuance, still unverified: concurrency when an interactive TUI holds
  that session open on-screen at the same moment — the -p turn lands in
  the transcript, but whether the open TUI re-renders it is unknown.

**Inferred / not live-exercised — verify before building on:**
- Linux `TIOCSTI`/`CONFIG_LEGACY_TIOCSTI` state (no Linux test machine used
  for this ADR; from public kernel history).
- Whether orca's CLI exposes a per-terminal tty directly (would let orca
  join the clean tty-based path instead of staying cwd/title-matched
  best-effort).

**Reversal (revised — see §2.2 "Backend selection is locate-based"):** an
earlier revision of this ADR proposed tying Tier 1a to whichever single
backend `Resolve()` picked in install order, and recorded that as an
"accepted trade for a single, predictable rule rather than probing every
installed backend on every actuation." That trade is **withdrawn.** It was
wrong in practice: on the captain's machine orca is always available, so
`Resolve()` always returned orca even when the loop lived in a tmux/cmux
surface orca cannot Locate — attach failed and Tier 1a skipped legitimately
reachable sessions. Every resolver now probes all available backends and
selects locate-based (accepting up to a 3× `Locate`/`LocateClaude` fan-out
among available backends). The Tier 1b fan-out cost is intentional: it is
what makes the cross-backend ambiguity refusal possible, so it must not be
"optimized" back to a single first-hit probe.

**iTerm2 Tier 1h (amendment, 2026-07-18).**

*Confirmed live — mechanism:* iTerm2's shipped `.sdef` declares `write` taking
a **session specifier** with `text`/`newline`/`contents of file` parameters;
`class session` exposes read-only `id` (guid) and **`tty`**; read-only
enumeration of sessions over `osascript` works, exits 0, returns
`<GUID> tty=/dev/ttysNNN`, requires only the already-granted Automation
permission, and does **not** raise the app; `osascript` argv preserves
arbitrary bytes including quotes, backslashes, unicode, embedded newlines and
raw control characters (`\x1b`, `\x03`) byte-identical.

*Measured live 2026-07-18* (throwaway iTerm2 window; no Claude session driven,
so zero API cost). Method and full evidence: `.notes/design-iterm2-tier1.md`
§8b. Six experiments run, six PASS:

| id | question | verdict |
|---|---|---|
| **E1** | does `write` reach a **background, non-frontmost** window? | ✅ PASS — `frontmost` read `false` before and after; text landed; iTerm2 never came forward |
| **E2** | does `newline yes` emit CR or LF? | ✅ PASS — **CR (`0x0d`)**, the same byte a physical Enter sends |
| **E4** | does a raw ESC survive `write` to the pty? | ✅ PASS — `od` read `033`; the `p`/interrupt verb is viable |
| **E6** (mechanism half only) | does an EMPTY payload still emit CR? | ✅ PASS — `write … text "" newline yes` → `\r`; a bare submit is a real Enter, not a no-op |
| **E8** | does `tty of session` need normalization beyond stripping `/dev/`? | ✅ PASS — iTerm2 reports `/dev/ttysNNN`, the registry stores the bare `ttysNNN`; `"/dev/" + entry.TTY` is the correct join |
| **E11** | does `osascript` consume the `--` end-of-options marker? | ✅ PASS — marker consumed, `item N of argv` indices do not shift, and a payload beginning with `-e` arrives as **data** rather than being eaten as a flag |

*Measurement trap, recorded so it is not re-fallen-into:* the first E2 reading
was `\n` and would have been reported as "LF, may not submit" — **wrong.** That
was an artifact of the tty line discipline (`icrnl` rewrites incoming CR to NL
before the shell sees it), so a canonical-mode capture cannot distinguish the
two. **Any re-verification of E2/E4/E6 must use `stty raw`**; a `cat -v` or
plain `od` check silently reports the post-translation byte.

**Run since (amendment, 2026-07-19) — E5 and E6's TUI half both PASS.** Budget
was approved for the two experiments this ledger flagged as gating, and both
were run live against a real Claude Code session in iTerm2:

- **E6's TUI half — ✅ PASS. `a` is now verified end-to-end, not inferred.**
  A `claude --settings` run with `permissions.ask: ["Bash"]` was driven to a
  real permission prompt (`Do you want to proceed? ❯ 1. Yes / 2. No`), and the
  exact call the `a` key makes — `write <session> text "" newline yes` — cleared
  it: the default option was accepted and the command ran. The same session also
  confirmed the **text** path end-to-end (a prompt written with `newline yes`
  submitted and produced a turn), which upgrades `r`/`i` from byte-level to
  behaviour-level verification. The earlier caution that "`a` must not be
  described as verified" is hereby withdrawn — it may be.
- **E5 — ✅ PASS, and the hazard is real.** An iTerm2 session hosting a tmux
  client reported `tty of aSession` = `/dev/ttys001`, while the pane inside that
  tmux reported `/dev/ttys007`. So a loop running inside tmux inside iTerm2
  genuinely carries a registry tty that differs from the enclosing iTerm2
  session's, and an unguarded Tier 1h write would land in the tmux client's
  shell rather than the claude pane. The tty-mismatch guard is not defensive
  paranoia; it refuses exactly this case, and the 1a-before-1h ordering is
  justified as a safety property rather than a preference.

*Still NOT run — do not read this ledger as more verification than was done:*

- **E3** multi-line payload — one paste or N submits (affects `i` quality, not
  correctness).
- **E7** `osascript`'s exit behavior under a **denied** Automation grant. The
  code now folds `exec.ExitError.Stderr` into the error specifically so the
  -1743 text reaches the operator, but the expectation that osascript exits
  non-zero and writes that text to stderr is still **inferred from Apple's
  documentation, not observed.**
- **E9** Apple Event payload size limits. **E10** concurrency/interleaving of
  two rapid `write`s.

*Recorded risk — deprecated vendor surface:* iTerm2 documents its AppleScript
support as **Deprecated** in favor of the Python API. `write` is present and
functional in shipped 3.x and removal would break a large installed base of
user scripts, so near-term removal is judged unlikely — but this is a
deprecated vendor surface, the same species of dependency §1 blames for both
cmux P0s. It is accepted knowingly, and it is bounded to one adapter behind one
interface.

*What "bounded" actually means, stated per verb rather than as a blanket* (an
earlier draft of this amendment claimed "if `write` disappears, iTerm2 users
degrade to today's Tier 2/0 — a return to the status quo," which was **false as
written**):

- **`r`/`i` — genuinely degrade.** A failed host send falls through to Tier 2's
  headless redrive, so the prompt still lands. This is a real runtime fallback,
  not a hope. (It was *not* true when this amendment was first drafted: the
  dispatch returned the 1h failure as terminal and never reached Tier 2. That
  was a capability regression and has since been fixed.) The **one** exception
  is a `write` that times out: delivery is then unknown, so it is deliberately
  NOT retried, and the operator is told the outcome is unknown rather than
  sold a success or a silent duplicate.
- **`k`/`p`/`a` — do NOT degrade. There is no Tier 2 for them.** If `write`
  disappears these become dead keys on iTerm2, falling back only to Tier 0's
  manual hint ("attach and do it yourself"). Relative to the pre-amendment
  world that is the status quo ante — those verbs had no iTerm2 path at all —
  but it is **not** a graceful runtime degrade, and a fleet that has come to
  rely on them would experience it as a capability loss. That asymmetry is the
  honest shape of this dependency and is exactly why the tier is scoped to one
  adapter.

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
  ecosystem's default convention — see §6 — but fleetops's own preferred
  environment is orca, so full tmux-only isn't actually on the table here.)
- **fleetops-owned pty for all sessions (Direction C, generalized).**
  Rejected as the near-term fix: doesn't help a session a human already
  started in IntelliJ. Correct as the engine's long-term model for
  loops fleetops itself spawns (VISION.md), not a substitute for
  discovery+Tier 0/2 on human-started sessions.
- **A 4th actuation tier via terminal-emulator-native remote-control APIs**
  (kitty's documented RPC protocol, WezTerm's `cli send-text`, iTerm2's
  Python API — all real, externally-callable, no TIOCSTI-style hackery
  needed). Deferred, not rejected: technically sound and would extend
  in-place actuation to users of those terminals, but today's fleetops
  users are on orca/cmux/tmux, not kitty/WezTerm/iTerm2. Gate this behind
  actual user demand rather than building speculatively (simplicity-first).
  Alacritty/Ghostty/Windows Terminal/Terminal.app either have no comparable
  API or only ad-hoc AppleScript-level text injection, not worth chasing.

  **Partially triggered (amendment, 2026-07-18) — iTerm2 only.** The demand
  gate fired: fleetops's primary user moved to iTerm2, where `p` (interrupt),
  `k` (kill), and `a` (approve) have **no actuation path at all** — all three
  are Tier-1-only with no headless Tier-2 equivalent, and no multiplexer can
  reach an iTerm2 session. See `.notes/design-iterm2-tier1.md`.

  **kitty, WezTerm, Ghostty, Alacritty, Windows Terminal and Terminal.app
  remain deferred/rejected exactly as before.** This is one host, not the
  opening of a plugin framework — the short-curated-list principle in this
  ADR's preamble is unchanged and is the reason the iTerm2 work is scoped as a
  single adapter rather than a generalized emulator abstraction.

  **Mechanism, revised from the entry above:** iTerm2's **AppleScript `write`
  command over `osascript`**, *not* its Python API. The Python API would
  require the user to enable it in preferences, plus a Python 3 runtime and the
  `iterm2` package, plus either shipping a Python script alongside a static Go
  binary or reimplementing iTerm2's websocket/protobuf protocol in Go. The
  AppleScript path needs none of that: `osascript` ships with macOS, and
  `focus.go`'s attach already established both the dependency and the TCC
  Automation grant. See §2.2's amended rejection note for why this is not the
  rejected keystroke-simulation approach.

  **Shape:** a new **Tier 1h** — a `host_app`-keyed `SendAdapter`, sibling to
  the existing `FocusAdapter`, dispatched from the session registry. iTerm2
  does **not** become a `Controller` and does **not** join the `backends`
  slice: it is a terminal emulator, not a pty-owning multiplexer, and it has no
  `Locate`/`LocateClaude`/`Spawn` worth implementing (rebuilding cwd-based
  surface enumeration for it would recreate exactly the discovery layer §2.1
  deletes). Ordering is **1a → 1h → 1b**: multiplexers keep first say, so a
  tmux/orca/cmux session running *inside* an iTerm2 window is still addressed
  by its precise pane rather than by the enclosing iTerm2 session.

  **Tier 1h requires no multiplexer.** It is resolved *before* the
  "is any backend available?" gate, so a machine with no orca/tmux/cmux
  installed still gets `p`/`k`/`a` on an iTerm2-hosted loop — which is the
  configuration this tier exists to serve, and an earlier implementation
  revision wrongly excluded. The gate's `backendAvailable=false` (and its
  "no orca/tmux/cmux" operator message) now means "no multiplexer **and** no
  host send," never merely the former.

  **Safety carried over unchanged:** the session GUID stays whitelist-gated
  (`itermGUIDPattern`) with no exec on failure; the recorded pid↔tty binding is
  re-validated against live `ps` exactly as Tier 1a requires (§3 step 2); and
  — new, with no multiplexer analogue — the GUID→tty binding is **verified
  inside the same `osascript` round trip** using iTerm2's read-only `session
  tty` property, refusing on mismatch. Arbitrary prompt text is passed
  **exclusively as `osascript` argv**, never interpolated into script source
  (verified: quotes, backslashes, `$(…)`, newlines, unicode and raw control
  bytes round-trip byte-identical). This is a hard requirement, not a
  preference — a prior review found a Critical AppleScript-injection defect on
  this exact surface.

## 6. Prior art

- **[Fleet](https://github.com/nicknisi/fleet)** (a tmux dashboard shipped
  as a Claude Code plugin) independently converged on the same core idiom
  this ADR proposes: install Claude Code hooks (`Notification`,
  `PreToolUse`, `Stop`, `SubagentStop`, `SessionEnd`) that fire on every
  event and write identity+state to a status file, discovered passively
  rather than requiring sessions to be launched through the tool itself.
  This is real external validation — "hook as ground-truth identity
  source, filesystem as registry" is a sound idiom arrived at
  independently, not a fleetops-specific idea. Fleet's registry key is
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
  project attempts an N-way multiplexer abstraction like fleetops's
  orca/cmux/tmux layer** — most don't even reach for it. This ADR's Tier
  0/1/2 design is more ambitious than the field, not merely consistent
  with it; worth staying honest with ourselves about that added surface
  area every time a vendor's CLI shifts under us again.
