# ADR: authored state vs. observed state in the loop model

- status: **accepted** (2026-07-19) — accepted as the *representation* decision
  and as the staging/gating plan. Acceptance does NOT pre-approve stages 4–5:
  those remain gated on §3 stage 0's measurements, which have not been run.
  Stage 1 is behaviour-only and is being implemented independently.
- context: this design was derived by reading `main` @ `90ec3c9` against a real
  failure observed 2026-07-19 on `544c27c`. See §4 for what was verified from
  code vs. what remains inferred and unmeasured.

**Scope discipline, read this first:** this ADR is about **how loop state is
represented**, not about adding capability. It proposes no new backend, no new
actuation tier that is not gated on measurement, and no extensibility
framework. Its central claim is that one Go enum is currently holding two
vocabularies with different producers, and that three separately-reported
defects are one consequence of that. Where the honest conclusion is "this is a
message-authoring problem, not a model problem," this ADR says so (§1.3) rather
than growing the model to cover it.

## 1. Problem

`domain.LoopState` is one enum with nine values, produced by two different
mechanisms that never overlap:

- **Inferred** — `running`, `idle`, `gate`, `stalled(kind)`. Produced by
  `claude.classifyLoop` from exactly two inputs: a 24KB tail of the session
  JSONL and `now - mtime`. No contract required; applies to every session on the
  machine.
- **Asserted** — `drift`, `done`, `failed`, `killed`, `paused`. Produced by
  `claude.enrichFromRegistry` and `claude.applyGovernor` from an oracle verdict,
  a governor ceiling, or a replay of the human's own actuation history. Each of
  these **returns early when the session has no `registry.Record`**, so an
  externally-managed session structurally cannot carry them.

The vocabularies are already segregated by producer. They are not segregated by
type, and three faults follow directly.

### 1.1 A non-final interpretation outranks an observed fact

Because both vocabularies inhabit one enum, `applyLiveness`'s presumed-dead
switch enumerates them together and writes:

```go
case domain.StateDone, domain.StateDrift:
    // oracle-judged and settled; leave as-is.
```

`done` is terminal (`LoopState.Terminal()`); `drift` is not. `drift` means the
oracle rejected the claim and the loop should be re-driven — which a dead
process cannot be. Observed live: a loop whose process had exited 40 minutes
earlier displayed `✗ DRIFT`, and the human acted on it.

Worse, the losing value was not merely out-ranked, it was **destroyed one pass
earlier**: `enrichFromRegistry` overwrites the scanner's inferred `State` with
the verdict, so by the time liveness runs there is nothing to fall back to. The
single enum is why the projection had to be lossy.

The correct invariant is *not* "fact beats interpretation" — that would regress
`done`/`failed`/`killed` into `gone`. It is **final beats non-final; among
non-final, observed beats inferred.**

### 1.2 Provenance is not recorded, so owned loops take the guessing path

`Loop.Driven` is asked to answer "did we spawn this?" but it means three things
at once: fleetops spawned it **and** the engine may drive it **and** no human has
taken over. It is opt-in at the spawn wizard (`[m]` yields a fleetops-spawned,
contract-bound loop with `Driven == false`) and is cleared by both take-over and
kill.

So an ordinary human action makes fleetops **forget it created the loop**, after
which `k` falls to locating a terminal by directory and refuses on collision —
for a surface fleetops opened itself. It opened it with a precise handle in
hand: tmux returns a `pane_id` and orca a terminal handle inside `Spawn`, both
discarded because `Controller.Spawn` returns only `error`, and
`registry.WritePending` records cwd and contract and no surface identifier at
all.

### 1.3 Failure messages assert remedies they never checked

The ambiguity refusal is one `fmt.Sprintf` whose remedy clause is a compile-time
constant — emitted identically for a live loop and a dead one, for a spawned
loop and an observed one, whether hooks are installed or not:

> `ambiguous: 3 loops share fleetops's directory, none has a session-registry tty — attach (↵) or run 'fleetops hooks install' so injects can target by session`

Both halves were inapplicable: `hooks install` cannot retroactively register an
already-running session, and attach cannot help a dead one. **This one is
primarily a message-authoring problem, not a model problem** — it is fixable
today against facts the function already holds. The model contribution is only
that nothing currently tells a message author which facts are decidable.

### 1.4 An owned loop's ending has nowhere to live

Retention is effectively "a transcript exists within `ActiveWindow` ⇒ show it."
That is sound *observed-session* policy — fleetops cannot assert an ending it did
not cause — and it is being applied to loops fleetops spawned, drove, judged and
watched hit a ceiling. Kill records no durable ending, only a prose event that is
re-parsed each scan by `strings.HasPrefix(detail, "kill ")` — which also matches
`"kill <tier> failed: …"`, so **a kill that demonstrably did not land marks the
loop killed** and the `k` key then refuses it.

## 2. Decision

Keep **one `Loop` type**, and decompose the single enum into orthogonal facets
with an explicit projection to display state.

### 2.1 Facets

| facet | values | kind | who owns it | the decision it enables |
|---|---|---|---|---|
| **origin** | `owned \| observed` | fact, immutable | the `n` wizard, once (derivable today from "a `Record` exists") | actuation path; whether asserted vocabulary is admissible; retention policy |
| **liveness** | `live \| dead \| unknown` | OS fact | `ps`/`lsof` | whether in-place actuation is possible; ladder position |
| **activity** | `running \| idle \| gate \| stalled(kind)` | inference | `classifyLoop`, sole owner | whether the engine may drive; whether `a` applies |
| **surface** | `bound \| registry \| discoverable \| none` | mixed — and it says which | `sessions` / spawn / backends | which key is offered; **what a message may claim** |
| **contract standing** | verdict × governor × authority × lifecycle | assertion | oracle / engine / human | ORACLE column; ceilings; drive permission; the ending |
| **visibility** | `visible \| hidden` | human intent | `internal/hidden`, unchanged | is the row rendered |

`contract standing` exists **only when `origin == owned`**, as a nil-able
pointer. That is what makes the asserted vocabulary structurally unreachable for
an observed session rather than merely conventionally absent — the specific
guarantee §1.1 needed.

`Driven` splits into immutable `origin` and mutable `authority`. After the
split, `authority` has exactly one reader (`engine.ShouldDrive`), which is how
you can tell it was never the same fact as origin.

### 2.2 The precedence ladder

Display state becomes a **pure function**, evaluated top-down, first match wins.
It is never stored.

| # | condition | display | principle |
|---|---|---|---|
| 0 | `hidden` | *not rendered* | human intent is absolute |
| 1 | `lifecycle == ended(cause)` | `done`/`failed`/`killed` | a settled ending is final |
| 2 | `liveness == unknown` | *unchanged* | a failed probe is not evidence |
| 3 | `liveness == dead` | **`gone`** + annotation | **observed death outranks any non-final interpretation** ← §1.1 |
| 4 | `activity == gate` | `gate` | a decision pending *now* outranks a stored judgment |
| 5 | `governor == stopped` | `failed` | a hard ceiling is a conclusion |
| 6 | `governor == escalated(r)` | **`over(r)`** | new — today `Escalate` writes only a `Note` and has no state |
| 7 | fresh verdict at cycle | `done`/`drift` | the oracle on the current cycle |
| 8 | otherwise | `activity` | the observation, unmodified |

Rung 3 **keeps the loser** as annotation — `gone · drift ×11 · 16/4`. The drift
verdict was never wrong, only less actionable; the facets are what make showing
both possible.

### 2.3 Actions follow capability, not display state

> A key is **offered** exactly when its capability predicate holds. A key that
> is offered must never refuse for a reason discoverable before the keypress.

```
canActInPlace = liveness == live ∧ surface resolves unambiguously
canRedrive    = transcript exists ∧ ¬ended          // Tier 2 needs no surface, no live process
canBookkeep   = always
```

Fail-closed refusals survive — refusing beats acting on the wrong loop — but they
partition: pre-discoverable causes suppress the key; actuation-time causes (a
binding that failed re-validation, a backend that vanished) may still refuse, and
say which.

Consequences worth stating: `r`/`i` are **available on a dead loop** (Tier 2),
which the code already knows as a `StallGone` special case and which becomes the
rule. `a`/`p` are **not offered** on a dead loop — nothing is waiting, nothing to
interrupt. And `k` **never refuses**, because on a dead process it is a lifecycle
transition, not a keystroke: stop the process *if one is running*, revoke engine
authority, record `ended(killed)`. The last two are file writes that cannot fail
for any reason the human could have avoided.

### 2.4 Owned-loop surface binding — Tier 1s, gated

Record `{backend, ref}` at spawn (tmux `pane_id`, orca handle) onto the pending
record; `BindPending` copies it to the `Record` as it already does for `Driven`.

Insert as **Tier 1s, between 1h and 1b — not at the top.** Provenance is not
currency: 1a's strength is that the tty↔pid binding is re-proved against the
live OS *at actuation time*, and a spawn binding has no equivalent proof unless
the backend can validate its own ref. Placing 1s above 1a would substitute
"we created it" for "it is still there" — the same error as trusting a stale
registry record, which `ResolveActuationTarget` was written to prevent. At 1s it
still does the job that matters: **owned loops leave the cwd-guessing path.**

1s's main argument is not precision but independence: it is written
synchronously by the process that just created the surface, so it is immune to
whatever is breaking `SessionStart` for most sessions (#49).

### 2.5 Lifecycle and retention

An owned loop has a definite end, reachable exactly four ways: contract met,
ceiling hit, human killed, process gone with no authority and nothing pending.
Each writes durable `ended(cause, at)` on the `Record`.

Three distinct things, deliberately kept apart:

- **ended** — asserted by the system, immediately, automatic.
- **retired** — the row leaves FLEET after a grace period following `ended`, so
  a human who was not watching still sees the outcome.
- **hidden / deleted** (`d`/`x`) — human intent, permanent, unchanged, applies to
  both kinds.

Retirement **must not write to `hidden.json`**: an automatic system decision must
stay distinguishable from a human's permanent one, especially given the TUI has
no unhide affordance.

**Observed sessions gain no ending.** They keep `ActiveWindow` + idle-drop + `d`.
They simply stop sharing a retention rule with owned loops that need a different
one. `k` implying end implying retire is the whole of the "killed it, restarted,
it came back" complaint.

### 2.6 Rejected: two types

`OwnedLoop` and `ObservedSession` as distinct types is the option a
sum-type-having language would pick, and it would make "an observed session can
never be DRIFT" a compile-time guarantee rather than a discipline — a discipline
this codebase has already demonstrably failed once. Rejected anyway, on four
grounds:

1. The observation substrate is genuinely one thing and is the **bulk of the
   code** — ~600 lines of tail-parsing, gate detection, liveness cross-check,
   cwd healing and collision guarding apply identically to both. Splitting the
   type splits or generifies all of it.
2. The difference is **presence of facts, not kind of thing** — exactly what an
   optional sub-struct expresses. The two sort together, render in one list,
   share a cursor, share `d`/`x`, share attach.
3. **Conversions already exist**: take-over and kill mutate authority today, and
   "adopt an observed session into a contract" is an obvious future feature.
   Under two types those are type conversions with identity problems.
4. The `*Contract` pointer buys **the specific guarantee that was actually
   violated**, at the only seams that matter.

**Tripwire:** revisit if a second owned-only facet turns out to be load-bearing
(at two, a type is trying to be born), or if measurement M4 shows the fleet is
heavily skewed toward one kind.

## 3. Migration (phased, non-breaking)

1. **Stage 0 — measure.** M1, M2, M4, M5 (§4). Instrumentation only, at sites
   that already exist. **Gates everything below.**
2. **Stage 1 — behaviour fixes, no model change.** Remove `StateDrift` from the
   presumed-dead exemption arm; require `" ok"` in `mostRecentActuationIsKill`;
   branch the ambiguity message on liveness/origin/registry presence. A few
   lines each, with tests. Resolves §1.1 and most of §1.3. **May land
   independently of this ADR** and is the correct assignment for work already in
   flight.
3. **Stage 2 — split `Driven`; name `origin`; extract the projection.** Additive
   by construction: `origin` is derivable from "a Record exists," so no on-disk
   migration; `authority` keeps the existing field and JSON key. The projection
   reproduces current behaviour exactly. **No user-visible change** — this is the
   tidy-first commit, best done the next time someone touches `scan.go`.
4. **Stage 3 — capability-derived key availability + honest messages.** The
   legend becomes per-selection; hand-written state conditionals become
   capability predicates. **Observable:** M1's refusal counter falls to near zero
   for pre-discoverable causes while actuation-time refusals continue.
5. **Stage 4 — spawn-time surface binding (Tier 1s).** **Gated on M1 and M3.**
   The largest change and the one most likely to be unnecessary if #49's root
   cause is fixable in the hook. Deferrable indefinitely.
6. **Stage 5 — lifecycle and retention.** Durable `ended`; grace period from M6.
   Partially pre-empted by stage 1's kill fix, which is fine.

**Deferred, possibly forever:** splitting `Loop` into two types (§2.6).

**What's user-visible:** after stage 1, a dead loop reads `gone` rather than a
stale verdict and failure messages stop advising inapplicable remedies. After
stage 3, keys that cannot work are not offered, and `r`/`i` become available on
dead loops. After stage 5, killed and completed loops retire on their own. No
stage removes a capability; no stage requires a data migration.

## 4. Confirmed-live vs inferred (an honesty ledger)

**Confirmed from code** (full citations live in the author's local working
notes, `.notes/design-loop-state-model.md` §0 — `.notes/` is gitignored working
memory, so the findings that matter are restated here rather than only linked):

- `applyLiveness` exempts `StateDrift` by name alongside `StateDone`;
  `enrichFromRegistry` overwrites inferred state with the verdict.
- `killCmd` and the `k` handler **already** have `Driven` fast paths that skip
  terminal resolution and the ambiguity guard — and both are present in
  `544c27c`. The reported incident therefore ran on a loop whose `Driven` was
  **false**, which is what §1.2 is about.
- `BindSpec.Driven` is wizard opt-in; take-over and kill both clear it.
- `Controller.Spawn` returns only `error`; tmux's `pane_id` and orca's handle are
  obtained and discarded; `WritePending` stores no surface identifier.
- The key legend is a static 12-entry list rendered identically every frame.
- `logActuationEvent` writes `"kill <tier> failed: …"`, which
  `mostRecentActuationIsKill`'s `HasPrefix("kill ")` matches.
- Governor `Escalate` sets only `Note` and has no `LoopState`.

**Inferred / not measured — verify before building on:**

- Whether the incident loop's `Driven` was false because of `[m]` at the wizard
  or because a take-over cleared it. Both are the same conflation; a postmortem
  should still settle it.
- Whether tmux `pane_id` / orca handles stay valid for a session's whole life
  (M7) — §2.4 depends on it.
- Everything about *why* `SessionStart` misses most sessions. #49 is explicitly
  the measurement, not the diagnosis; this ADR inherits that ignorance.

**Measurements that gate implementation:**

| id | question | gates |
|---|---|---|
| **M1** | how often does the cwd-ambiguity refusal actually fire, per key, per day? | **stage 4 entirely.** #49 measured registry *coverage*; nobody has measured refusal *incidence* |
| **M2** | of #49's ~60% empty-tty entries, what fraction are headless `-p` sessions (expected per the actuation ADR §2.1)? | whether empty-tty is a hook bug or a legitimate `surface: none` |
| **M3** | #49's unresolved root cause | stage 4; **do not proceed past stage 2 without it** |
| **M4** | owned/observed split in a typical FLEET list | §2.6's ontology choice |
| **M5** | how often does a dead loop display a non-terminal interpreted state? | urgency of §1.1 |

Non-gating: **M6** time-from-outcome-to-acknowledgement (sizes the grace
period); **M7** surface-ref stability; **M8** frequency of the failed-kill
mislabel.

**Recorded risk — one bad night is one data point.** Every fault above was
reasoned from a single screen. "Real" is not "frequent," and this ADR proposes
restructuring the core domain type on evidence that has not been counted. If M1
and M5 both return "rare," **the correct response is to ship stage 1 and stop** —
that is a success of the gate, not a failure of the design.

**Recorded risk — the cost/benefit is lopsided toward the cheap half.** Stage 1
(hours) resolves §1.1 and most of §1.3. Stage 2 alone fixes §1.2's root cause by
giving actuation an origin fact to consult, with no backend change. Stage 4 —
the expensive one — may never be needed.

**Recorded risk — the pid-kill option is a behaviour change wearing a model
change's clothes.** The session registry stores `PID`; signalling it needs no
multiplexer, no tty and no cwd guess, and is strictly more reliable than typing
`/exit` into a directory-located surface. It is also a *different act*: the agent
gets no chance to finish or flush. Deliberately left as the captain's call and
**not** smuggled in under §2.3's "kill is a lifecycle transition."

## 5. Alternatives considered and rejected

- **Two types (`OwnedLoop` / `ObservedSession`).** See §2.6 — rejected on
  substrate duplication, with an explicit tripwire for revisiting.
- **Store the display state.** Rejected: it becomes a fourth thing that can
  drift from the facts, and the current defect is precisely a stored state
  overwriting the evidence that contradicted it. The projection stays derived
  every scan.
- **Fix the three defects tactically and change no model.** Genuinely tempting,
  and it is what stage 1 does. Rejected as the *whole* answer because the
  `case StateDone, StateDrift:` arm is not a typo — it is what the single enum
  invites, and the next arm will be written the same way. But this alternative
  is close enough that §4's gates exist to let it win if the numbers say so.
- **Make `origin` a stored field rather than a derived accessor.** Deferred. "A
  `Record` exists ⇒ owned" is already true and already free; a field is only
  needed once a feature makes the derivation false (e.g. adopting an observed
  session into a contract). **Prefer the accessor until that feature exists.**
- **A generalized state-machine / plugin framework for loop kinds.** Rejected
  outright, in the same spirit as the actuation ADR's curated-backend-list
  discipline. There are two kinds of loop and there is no third on the horizon.

## 6. Prior art

- **This repo's own `docs/adr-vendor-independent-actuation.md`** is the direct
  precedent and the reason several things here are *not* proposed. Its tier
  model already establishes that identity (registry) and liveness (`ps`) are
  different authorities with different owners — §2.1's facets are the same
  separation applied to state rather than to actuation, and §2.4's refusal to
  place spawn-binding above Tier 1a is its "re-validate against live `ps`"
  discipline held to consistently.
- **`engine.Check` / `engine.ShouldDrive`** are the in-repo precedent for §2.2's
  projection: pure functions of a `Loop` value, zero I/O, directly unit-testable,
  with the fail-closed reasoning written out as a documented checklist rather
  than an opaque boolean. The projection should be built to look like them.
- **The `hidden` package** is the precedent for §2.5's insistence that
  retirement not write to the hide-set: it is already carefully scoped to human
  intent, fail-open on read, and non-destructive, and merging an automatic
  system decision into it would break all three properties at once.
