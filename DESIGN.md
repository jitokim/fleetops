# fleetops — design

> This is the architecture doc for what's actually **built and running** today
> (100% Go: observation + one-key actuation, plus a minimal opt-in LoopEngine —
> see §0.1). For the longer-term, aspirational engine/governor architecture
> (Python/Textual/asyncio, a headless `LoopEngine`, a persistence `Store`) — a
> future direction well beyond today's engine, not current behavior — see
> [`VISION.md`](./VISION.md). For user-facing behavior, the keymap, and known
> limitations, see [`README.md`](./README.md). For the actuation backend
> design specifically (session identity, capability tiers, per-backend
> verification notes), see
> [`docs/adr-vendor-independent-actuation.md`](./docs/adr-vendor-independent-actuation.md).
> For why `LoopState`'s values are really two vocabularies with different
> producers — inferred (`running`/`idle`/`gate`/`stalled`) vs asserted
> (`done`/`drift`/`failed`/`killed`/`paused`, structurally reachable only for a
> contract-bound loop) — and for the staged plan that corrects §3's precedence,
> see [`docs/adr-loop-state-model.md`](./docs/adr-loop-state-model.md).

---

## §0. The governance layer (what this project is actually for)

The novel part of fleetops isn't running a loop — it's the layer around
it that keeps a human in charge of the decisions that matter, without
requiring them to babysit the work:

- **oracle** (`internal/oracle`) — never trusts a goal-bound loop's own
  "done" claim; independently judges its latest report against its goal.
  `done` → `StateDone`; a false "done" claim → `StateDrift`; real work with
  no claim either way → `progress` (state unchanged).
- **challenger** — an adversarial second pass over an oracle "pass", to
  catch a lenient verdict before it's trusted. Not implemented yet (see
  `VISION.md` §2's `Challenger` protocol) — the ORACLE row's RUBRIC field
  intentionally doesn't show a challenger phase today because there's
  nothing to surface progress against.
- **governor** (`internal/engine.Check`) — pure budget / max-cycles /
  no-improve ceilings. A loop cannot silently exceed them: it escalates (a
  human-visible note, loop keeps running) or fails closed (`StateFailed`).
  See §3.
- **gate** (`internal/gate`, classified into `domain.StateGate` by
  `internal/claude`) — the loop blocks and a human decides, at exactly the
  points only a human should own: a Claude Code permission prompt, an
  `AskUserQuestion`, or (for goal-bound loops) an oracle-verified "done".

"The loop" is a real `claude` CLI session fleetops observes and, for a
goal-bound loop, can re-drive — either a human pressing `r`/`i`, an opt-in
429 auto-redrive (see the README's "Automation" section), or (§0.1) the
opt-in LoopEngine. There is no separate agent runtime anywhere in this
codebase: every cycle, human- or engine-driven, is the same `claude` CLI
call.

### §0.1 The engine (opt-in, minimal)

`internal/engine` adds one governance decision the pipeline above didn't
otherwise make on its own: whether to automatically fire a bound loop's
*next* cycle, instead of waiting for a human keypress. It is reachable only
behind two gates — the `FLEETOPS_ENGINE=1` environment variable, and the
per-loop `Driven` flag (`registry.Record.Driven`, copied onto `domain.Loop`
by `enrichFromRegistry` the same way every other registry-sourced field is)
— both required, off by default.

- `engine.ShouldDrive(loop, driven, inFlight) bool` is the entire fail-closed
  predicate: `Driven` must be true, no actuation already in flight for the
  session, `State == StateIdle` (which alone excludes a live gate, a running
  turn, a stall, and every terminal state), the CURRENT cycle's verdict must
  already be in and fresh (`Last.AtCycle == Cycle` — never race ahead of the
  oracle), and the governor (§3) must say `Continue`. A rejected verdict
  promotes the loop to `StateDrift` before the engine ever looks at it again
  — `StateDrift != StateIdle`, so the engine structurally cannot auto-drive
  a loop the oracle just rejected; that halts for a human, by construction,
  not by a separate check.
- `internal/tui`'s `triggerDrives`/`driveCmd` do the firing, once per ~3s
  scan tick — the same cadence the oracle judgment trigger already runs on.
  There is no direct drive→judge→drive chain; the loop back through a scan
  tick is deliberate (bounds the automation's own tick latency without
  needing a lock or a separate scheduler).
- A `Driven` loop's headless cycles are `claude -p "<contract>" --output-format json`
  calls — no terminal, no daemon. Between cycles it has no live process,
  which would otherwise make the fleet's liveness pass (§2 step 2) drop it
  as "ended cleanly"; a small dormancy exception holds a `Driven`+idle loop
  visible (bounded by a staleness timeout, so a genuinely dead engine loop
  still surfaces).
- Attaching to a `Driven` loop (`↵`) is a take-over, not a plain attach:
  it opens `claude --resume <id>` in a fresh terminal (there's no existing
  one to focus) and clears `Driven`, so the engine stops scheduling further
  cycles the moment a human takes the wheel. Killing one (`k`) clears
  `Driven` instead of sending `/exit` — there's no terminal to send it to.

## §1. Package boundaries & dependency direction

Dependencies point one way — inward, toward `domain` — with `internal/tui`
as the single composition root that wires everything together. No package
below imports `tui`.

```
domain            — the seam: Loop/Goal/Verdict/LoopState/StallKind value
                     objects that cross every other boundary, and the
                     ports the sections below describe. Zero internal deps.
events, gate,      — low-level, dependency-free infrastructure primitives:
notify, settings,    the append-only event log, gate marker files, desktop
fsatomic, worktree   notifications, ~/.fleetops/settings.json (spawn.command
                     — what `n` actually runs), the temp-file+rename atomic
                     write the on-disk registries share, and git-worktree
                     creation with plain `git` (deliberately NOT in control:
                     making a checkout drives no terminal). Zero internal
                     deps each.
sessions           — the session-identity registry: the hook-recorded
                     session→tty map that lets actuation target one exact
                     session rather than guessing by cwd. → fsatomic.
hidden             — the persisted hide-set: `d`/`x` tombstones the TUI
                     filters every scan through, so a hidden loop stays
                     hidden across restarts. Never touches ~/.claude and
                     never removes a registry record. → fsatomic.
engine             — governor.Check (a pure function, domain.Loop in,
                     Decision out) plus the LoopEngine drive predicate
                     (ShouldDrive/NextWorkPrompt, §0.1) — also pure.
                     → domain, registry.
oracle             — independent verdict judging via a cheap model call.
                     → domain only.
registry           — goal-bound loop persistence (spawn contracts, verdicts,
                     no-improve counters). → domain, events.
claude             — OBSERVATION: globs ~/.claude/projects, classifies
                     state from the tail, cross-checks liveness, applies the
                     governor, enriches from the registry. → domain, engine,
                     events, gate, registry.
control            — ACTUATION: locate/resume/approve/interrupt/spawn across
                     pluggable terminal backends (orca/cmux/tmux), plus a
                     host-send adapter for terminal emulators that are not
                     multiplexers (iTerm2, Tier 1h). → domain, sessions,
                     settings.
tui                — composition root: the Bubble Tea Model. Polls claude's
                     DiscoverLoops, renders the fleet, dispatches control's
                     actuations on a keypress, judges via oracle, persists
                     via registry. → claude, control, domain, engine, events,
                     gate, hidden, notify, oracle, registry, sessions,
                     worktree.
```

Two deliberate exceptions to "no duplication across packages," both
documented at their point of duplication rather than hidden: `control`
re-implements `claude`'s `encodeCwd`/`isClaudeComm` (2-line functions) so
`control` — the actuation layer, meant to stay a stable, independently
testable "pluggable ports" boundary — carries **zero** dependency on
`claude`, the much larger and faster-changing observation layer.

## §2. The observation → classification → actuation pipeline

Every ~3s scan tick (`internal/tui`'s `tea.Tick`), the fleet is rebuilt from
scratch — there is no incremental/diffed state, which is what makes the
whole pipeline resilient to a fleetops restart losing nothing durable
(everything a restart needs to reconstruct is either on disk already, or
cheap to re-derive):

1. **Observe** (`claude.DiscoverLoops`) — glob every
   `~/.claude/projects/*/*.jsonl`, keep only files active within the
   window. For each: `loopFromLog` reads the file's mtime (last activity)
   and tails the last ~24KB to classify state (`classifyLoop`) — a
   finished turn (`stop_reason: end_turn`) is `StateIdle` regardless of
   recency; otherwise `StateRunning` if recent, else `StateStalled`
   (`StallRateLimit` only on a genuine synthesized API error marker, never
   a bare "429"/"rate limit" substring — see `internal/claude.
   hasRateLimitMarker`'s doc for why that distinction is load-bearing). A
   pending gate marker (`gate.Pending`) or a live `AskUserQuestion`
   override the tail classification into `StateGate` — see §3's
   precedence order.
2. **Cross-check liveness** (`claude.applyLiveness`) — a `ps`/`lsof` probe
   catches the case a session merely *looks* idle but its process is
   actually gone (`✗ gone` / `StallGone`), which the JSONL tail alone can't
   distinguish from "waiting for a human." Also heals each loop's `Cwd`
   from a lossy directory-name decode to the confirmed-real `lsof` path —
   *unless* two distinct real directories collide under Claude Code's own
   `/`/`.`-both-become-`-` project-dir encoding, in which case healing (and
   the live-process *count* driving drop/demote) both refuse to trust the
   ambiguous data rather than risk attributing it to the wrong directory.
3. **Enrich + govern** (`claude.enrichFromRegistry` / `applyGovernor`) — a
   goal-bound loop (spawned via the TUI's `n` wizard) gets its
   goal/verdict/no-improve state from `registry`, promotes to
   `StateDone`/`StateDrift` on a fresh same-cycle verdict, and is run
   through the governor (§0) for its hard ceilings. An observed
   (non-spawned) session has none of this — it's "unbound": no goal, no
   oracle verdict, no governor.
4. **Render + act** (`internal/tui`) — the fleet is rendered (FLEET list +
   DETAIL panel); a keypress dispatches an actuation (§4) as an async
   `tea.Cmd`, never inline on the render path — see `gitStatsCmd`'s and
   `detailCacheCmd`'s doc for why: real disk I/O / subprocess calls belong
   off the Update/View goroutine, always.
5. **Judge** (`internal/oracle`, dispatched from `tui`) — once a goal-bound
   loop goes idle, an async judgment call renders a verdict, persisted back
   through `registry` for the next scan's enrichment step to pick up.

## §3. State precedence & the governor's ceilings

A loop's `State` is decided by layering overrides in this exact priority
order — each layer either leaves the previous layer's answer alone or
replaces it outright, never merges:

```
kill  >  gate  >  gone  >  verdict  >  governor  >  tail
```

- **kill** (highest) — a human's own kill decision (`mostRecentActuationIsKill`)
  always wins, even over an otherwise-terminal `StateDone`/`StateDrift` — a
  human decision is definitive and must never be silently overridden by a
  later re-examination (`fix/killed-state`'s whole reason for existing).
- **gate** — a live Claude Code permission prompt / `AskUserQuestion` /
  (for a bound loop) a fresh oracle verdict due for gating. Blocks
  everything below it: a human decision pending *right now* outranks a
  stale verdict, a governor note, or the tail's own guess.
- **gone** — `applyLiveness`'s process-death cross-check. `StateIdle` +
  gone → dropped from the fleet (ended cleanly); `StateDone`/`StateDrift` +
  gone → left alone (a settled judgment, not an incident); anything else +
  gone → `StateStalled`/`StallGone` (a mid-work death IS an incident).

  **Correction (2026-07-19) — `StateDrift` does not belong in that exemption,
  and this describes today's behavior, not the intended one.** `done` is
  terminal (`LoopState.Terminal()`); `drift` is not. `drift` means the oracle
  rejected the claim and the loop should be re-driven — which a dead process
  cannot be, so exempting it leaves a loop whose process exited displaying
  `✗ DRIFT` indefinitely (observed live 2026-07-19). The governing rule is
  **final beats non-final; among non-final, observed beats inferred** — so a
  dead non-final loop reads `gone`, keeping the drift verdict as annotation
  rather than as its state. `StateDone`'s exemption is unaffected. The fix is
  stage 1 of [`docs/adr-loop-state-model.md`](./docs/adr-loop-state-model.md)
  (§1.1, §2.2), landing separately; this bullet is corrected here in advance so
  the spec does not keep asserting the exemption as designed behavior.
- **verdict** — a same-cycle oracle verdict promotes `StateDone`/`StateDrift`
  (see §2 step 3). An earlier-cycle verdict is still shown (the ORACLE
  row/column) but does not override the current State.
- **governor** — `engine.Check`'s hard ceilings, applied after the verdict
  mapping so it sees this cycle's final state: `Escalate` (budget
  exhausted / max cycles reached) leaves State alone and sets an amber
  `Note`; `Stop` (no improvement for repeated cycles) promotes to
  `StateFailed`, unrecoverable by design — the loop cannot silently exceed
  its ceilings, it must fail closed or surface to a human.
- **tail** (lowest / fallback) — `classifyLoop`'s raw JSONL-tail heuristic:
  `StateRunning` / `StateIdle` / `StateStalled` (`StallRateLimit` /
  `StallNoOutput` / `StallTokenOut`), the baseline every higher layer above
  can override.

`LoopState.Terminal()` is `StateDone | StateFailed | StateKilled` — once a
loop reaches one of these, nothing in this pipeline re-examines it further
(short of a human's own kill decision, which — per the precedence order
above — is the one thing that can still land on TOP of an already-terminal
state).

## §4. Actuation tier & safety model

Every typed action (resume / approve / stop / kill / inject) resolves
through capability tiers, falling through automatically — see
`docs/adr-vendor-independent-actuation.md` for the full design and live
verification notes; summarized here:

1. **Tier 1a — registry tty (tmux/cmux).** A live tty recorded at
   `SessionStart` (via `fleetops hooks install`), re-validated against a
   live `ps` at actuation time — session-unique, so two loops sharing a
   directory are never ambiguous to each other. The binding is validated
   once, backend-independently, then **every** available backend
   implementing `control.TTYLocator` is probed and the first hit wins (no
   ambiguity guard needed — tty is session-unique). Today that is tmux and
   cmux; orca's CLI exposes no per-terminal tty, so it participates in 1b
   only.
2. **Tier 1h — host-terminal send (iTerm2).** The host terminal writes into
   the session in place, keyed by the registry entry's `host_app` +
   `window_id` (`control.SendAdapter`), reusing Tier 1a's already-computed
   pid↔tty binding. Ordered **between 1a and 1b deliberately**: after 1a so a
   multiplexer running *inside* an iTerm2 window is still addressed by its
   precise pane rather than by the enclosing window, and before 1b because 1h
   is session-exact where cwd is many-to-one. It is also resolved **above the
   "is any multiplexer available?" gate**, so an iTerm2-hosted loop gets
   `p`/`k`/`a` on a machine with no orca/tmux/cmux installed at all — meaning
   `backendAvailable=false` denotes "no multiplexer **and** no host send". An
   unknown or empty `host_app` resolves nothing and falls through to 1b, which
   keeps this tier a pure superset for existing multiplexer users. Uniquely
   among the tiers, 1h re-verifies its target binding a second time inside the
   actuation itself (the host reports the session's own tty in the same
   `osascript` round trip), so a resolved 1h actuator can still honestly refuse
   at send time.
3. **Tier 1b — cwd-based match (orca/cmux/tmux).** `control.Controller`'s
   `Locate`/`LocateClaude` — the latter refuses (rather than guess) when
   more than one `claude` surface shares a directory, the same
   wrong-terminal hazard the TUI's own keypress-time ambiguity guard exists
   to catch. Probed across every available backend, and because cwd is
   many-to-one this tier **counts** matches rather than stopping at the
   first: two or more distinct backends matching the same directory is
   cross-backend ambiguity and refuses outright.
4. **Tier 2 — headless re-drive (every backend, every host).**
   `claude --resume <id> -p "<prompt>"` continues the same session as a
   background turn — no terminal surface needed at all. This is what makes
   a loop whose terminal died resumable, and the only path available with
   no multiplexer backend at all.

**Ports** (the pluggable seams): `control.Controller` is the actuation
port — `Name`/`Available`/`Locate`/`LocateClaude`/typed actions/`Spawn` —
implemented once per backend (orca, cmux, tmux). `control.Resolve()` picks
one by install preference order, but its remit is **creation/capability**
(spawn, terminal-open, capability checks) — not actuation: the tiers above
probe all available backends and select by who can actually reach the
surface, so a loop hosted in a non-preferred backend is still reachable.
`control.SendAdapter` (Tier 1h) is a second, narrower actuation port for
hosts that are terminal emulators rather than pty-owning multiplexers — it
deliberately implements no `Locate`/`LocateClaude`/`Spawn` and does not join
the backend list. `oracle`'s judge call is a third,
narrower port (swap the model/prompt without touching the pipeline that
calls it). Both are Go functions/interfaces today, not the full
`Protocol`-per-concern seam set `VISION.md` §2 sketches (`Agent`/`Oracle`/
`Challenger`/`GatePolicy`/`LoopStore`) — today's engine (§0.1) is plain Go
functions operating on `domain.Loop` directly, not a pluggable-ports
architecture; that remains aspirational.

The **one** automated (non-human-triggered) actuation, 429 auto-redrive,
deliberately uses Tier 2 only — see the README's "Automation" section for
why an unattended action can never be allowed near Tier 1's surface-based
"wrong terminal" hazard at all.
