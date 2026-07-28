# ADR: extracting the actuation policy from the TUI

- status: **proposed** (2026-07-29) — proposed as the *seam* decision (where the
  policy lives, what it returns) and as the non-breaking migration order. It does
  not pre-approve the headless agent-operator itself (roadmap §1 stage 4), which
  stays gated on the actor/brake model (§2.4 below, roadmap §2.4).
- context: derived from a read-only measurement spike on `main` @ `8a64a36`,
  2026-07-29. Every code claim below was line-verified in that spike; see §4 for
  what was verified vs. what was assumed. This ADR records the same finding the
  roadmap logged inline at §2.1 (2026-07-29), promoted to a decision.

**Scope discipline, read this first:** this ADR is about **moving one policy to a
callable seam**, not about building the agent-operator that will call it. It adds
no new capability, no new actuation tier, and no new backend. Its central claim is
that the actuation *policy* (the honesty/guard decision) and the actuation
*presentation* (the English status line a human reads) are fused at a single
return value, and that this fusion — not the bubbletea event loop — is the only
thing keeping a headless operator from reusing the policy. The fix is a refactor:
the decision code already ignores the TUI. The one genuinely destructive control
(`k`/`x` two-press confirm) is **explicitly left fused** (§2.4), because a headless
operator needs a different brake, not the same one.

## 1. Problem

The end-goal (roadmap §1) is that another **agent** operates the fleet, not only a
human at the TUI. Today the send/redrive policy — the honesty/guard rules that
decide *delivered vs. degraded-to-Tier-2 vs. delivery-unknown-do-not-redrive vs.
refused-terminal-state* — is orchestrated inside `internal/tui/model.go`. (Fleet
ambiguity is decided there too, but as a separate upstream precondition, not part
of this policy — §2.2.) A headless operator cannot attach to a bubbletea program,
so it cannot reach that policy. Two structural faults, both measured:

- **The policy's OUTPUT is fused to presentation.** `sendPromptCmd`
  (`model.go:1860`) returns `resumeResultMsg{sessionID string; ok bool; text
  string}` (`model.go:190`). The entire honesty distinction exists **only** inside
  the `text` string, built by `fmt.Sprintf` at the 8 `resumeResultMsg{…}` return
  sites. There is **no structured verdict** on the returned value. A structured
  outcome *is* computed one layer over — `actuationOutcome` (`model.go:4056`) maps
  the error to `events.Outcome*` — but only for the event log, never handed back to
  the caller. So a headless operator would have to **string-parse English prose**
  to learn whether a send landed. That is the real blocker.
- **The policy is not actually entangled with the Model — the spike disproves the
  worry.** `sendPromptCmd` is a free function that reads and mutates **zero**
  `Model` fields (no cursor, no mode, no selection); every decision input arrives
  through its `domain.Loop` argument. The tier mechanism (`IsHostSendTier`,
  `control/actuator.go:71`) and all six honesty sentinels already live in
  `internal/control/` (`hostsend.go`, `iterm2spawn.go`). bubbletea is **not
  load-bearing** for the decision. This is a refactor, not a redesign.

One decision the spike first read as a policy input turns out **not** to be one:
the fleet-ambiguity check. `refuseIfAmbiguous` (`model.go:4189`, called at 7 sites)
reads `m.loops` to count siblings sharing a project directory — but it also applies
two bypasses (`ttyPathPlausible`, `injectHeadlessFallbackEligible`) and emits a
per-loop `ambiguityRemedy` advice string, all TUI-resident. Rather than pass a bare
count into the policy (which would drop those and refuse cases the TUI lets
through), ambiguity stays a **caller precondition** decided upstream of the seam
(§2.2). The send policy takes no ambiguity input.

## 2. Decision

Lift the actuation policy to a callable seam that returns a **structured verdict**
(a sentinel-typed error, not prose). Both the TUI (as renderer) and a future
headless agent-operator call the same method. The TUI keeps ownership of the prose.

### 2.1 Where the policy lives — a new `internal/control/actuate` package

Introduce the policy type in a **new `internal/control/actuate` package**, not in
`internal/control` directly.

- The six sentinels, `IsHostSendTier`, and the send adapters
  (`hostsend.go`/`iterm2spawn.go`/`actuator.go`) already fill `internal/control`
  with the *primitives*. The policy is the *composition* of those primitives with
  the ambiguity/terminal-state guards — a distinct responsibility (SRP). Keeping it
  in a sub-package keeps `internal/control` as the adapter layer it already is.
- The dependency direction is clean and one-way:
  `actuate → control → domain`; `tui → actuate`; `future agent-operator → actuate`.
  No import cycle, and the TUI stops being on the actuation decision's import path
  for anything but rendering.

If, in migration, the policy turns out to need nothing `internal/control` doesn't
already export to the TUI, collapsing it back into `internal/control` is a cheap
follow-up — but start it separate, because the whole point is to stop mixing
decision with adapter.

### 2.2 The request and the structured verdict

Minimal shape (Simplicity First — this is the seam, not a framework):

```
ActuationRequest{ Loop domain.Loop; Prompt, Action string }

Verdict:  Delivered | DeliveredTier2 | RefusedTerminalState
        | RefusedNoSurface | DeliveryUnknown

ActuationResult{ Verdict; Tier, Backend string; Err error; SessionID string }
```

- **Ambiguity is a caller precondition, NOT part of this policy.** The
  fleet-ambiguity refusal is adjudicated *upstream* of the seam: the TUI decides
  it at "r"/"i" keypress time in `refuseIfAmbiguous` (which owns the `m.loops`
  count), and a future agent-operator would decide it from its own fleet view.
  It is deliberately excluded because the real rule is more than a
  `SiblingCount > 1` check — it carries two deliberate bypasses (`ttyPathPlausible`
  and `injectHeadlessFallbackEligible`) and a per-loop `ambiguityRemedy` advice
  string, all of which live in the TUI. A `SiblingCount`-based copy inside the
  policy would omit those and thus refuse cases the TUI intentionally lets
  through — i.e. it would be the very comment-vs-code drift this ADR exists to
  prevent. So the policy takes no ambiguity input and emits no ambiguity verdict.
- `Err` is always one of the existing `internal/control` sentinels
  (`ErrSendDeliveryUnknown`, `ErrSendNoSession`, `ErrSendTTYMismatch`,
  `ErrSendUnrecognizedVerdict`, `ErrSendNoRecordedTTY`, `ErrITerm2SpawnNoClaude`)
  or nil. The agent does `errors.Is` + a `switch` on `Verdict`; it never parses a
  string. `DeliveryUnknown` carries `ErrSendDeliveryUnknown` and is the machine-
  readable form of the "do not redrive" carve-out — the one case an agent must
  never auto-retry.
- `ActuationResult` is not a pure verdict: alongside `Verdict`/`Err` it carries
  the *display facts* the renderer needs but must not re-derive — `Tier` and
  `Backend` (added in impl, load-bearing for reproducing the "via `<backend>`"
  line). So the "structured verdict, no prose" framing shouldn't be read too
  purely: the policy returns no English, but it does return the tokens the prose
  is built from.
- The enum is deliberately five values, one per honesty outcome already emitted at
  the return sites of the send path, so the refactor is a re-typing of existing
  branches, not a new taxonomy.

### 2.3 The TUI renders the verdict; the sibling helpers stay put

What actually moved: `sendPromptCmd`'s own **inline** status strings became a new
pure formatter, `renderActuationResult`, which maps the returned `Verdict`/`Err`
(plus `Tier`/`Backend`) back to the exact human-facing line. That is the whole
presentation move for the send path.

The three prose helpers — `ambiguityRemedy` (`model.go:4261`), `noSurfaceText`
(`model.go:4288`), `spawnFailureText` (`model.go:3979`) — did **not** move and did
**not** become `Verdict`→`string` formatters. `sendPromptCmd` never called them:
they belong to the *unmigrated* ambiguity/spawn paths (`refuseIfAmbiguous` and the
iterm2-spawn flow), which this refactor deliberately leaves alone (§2.2, §2.4).
Leaving them untouched is correct — pulling them through the send seam would have
been scope creep into paths the send policy does not own.

**Behavior for humans must be preserved.** The rendered strings a human sees should
not change. The refactor re-routes *where* the send path's string is chosen (from
the policy's verdict rather than inline in `sendPromptCmd`), not *what* it says.

### 2.4 Not in this decision: the `k`/`x` destructive-confirm gate

The two-press destructive-confirm gate (`pendingKill*`, `destructiveConfirmWindow`)
stays fused to `Model`. It is genuinely entangled with the Model — unlike
`sendPromptCmd` — and it is deliberately **not** extracted here. Per roadmap §2.4, a
two-press gate assumes human fingers at a keyboard; a headless operator needs a
*different* brake (budget/frequency/scope caps), not the same one. Designing that
brake is the actor/brake work (roadmap §2.4, stage 3), and it is future work, not
this ADR. Extracting the human gate now would be building the wrong brake for the
agent.

## 3. Consequences

**What gets simpler:**
- A headless operator branches on `errors.Is(res.Err, control.ErrSend…)` and a
  `switch res.Verdict`, with no English prose parsing and no bubbletea dependency.
- One policy source. The send-policy honesty rules (degrade-vs-terminal,
  delivery-unknown carve-out, terminal-state refusal) exist once. The TUI and the
  agent cannot drift, because there is nothing to keep in sync — the whole reason
  roadmap §2.1 rejected "reimplement the policy in the agent layer." (Ambiguity is
  not on this list on purpose — it stays a caller precondition, §2.2.)

**Scope of this policy — send/redrive only.** `Policy.Actuate` is the
**send/redrive** policy: `resume` / `inject` / `drive` / 429-redrive, the verbs
that share the tier1→tier2 degrade machine (a Tier 1 miss or Tier 1h non-delivery
falls through to the headless Tier 2 re-drive). It is **not** a universal
actuation policy. `approve` / `kill` / `interrupt` are a different shape and do
**not** belong under this method: they are **tier1-only** (there is no headless
Tier 2 for "press the approve key"), **non-degrading**, and they need a *richer
resolve-failure taxonomy* — `backendUnavailable` vs `notFound` — that this enum
cannot express (it collapses both into "fall through to Tier 2", which is wrong
for a verb with no Tier 2).

**What to watch (honest follow-up):**
- The sibling actuation verbs — `approveCmd` / `interruptCmd` / `killCmd` — were
  **not** line-verified in the spike. They will **share the PRIMITIVES** this
  package already composes (the send sentinels, `LogEventFunc`, `ResolveTargetFunc`,
  the terminal-state guard, the `DeliveryUnknown` handling), but they need a
  **separate tier1-only dispatch method**, not this degrading `Actuate`. Folding
  them into `Actuate` would force the wrong (degrading) machine onto a non-degrading
  verb and lose the `backendUnavailable`/`notFound` distinction. The 429 auto-redrive
  path, by contrast, *is* a send/redrive and does belong on `Actuate`. Confirm the
  exact split at implementation start; the drift this ADR prevents is a second
  caller reconstructing the *send* policy inline, not a legitimately different
  policy for a legitimately different verb.

**What must not change:**
- The strings rendered to a human (§2.3). This is a behavior-preserving refactor;
  the human-visible status line is the invariant.

## 4. Confirmed by the spike vs. not line-verified (an honesty ledger)

**Confirmed live (read-only, `main` @ `8a64a36`, 2026-07-29):**
- `sendPromptCmd` (`model.go:1860`) reads/mutates zero `Model` fields; all inputs
  arrive via its `domain.Loop` argument.
- `resumeResultMsg` (`model.go:190`) is `{sessionID, ok bool, text string}` — no
  verdict field. The honesty distinction lives only in `text`, built at 8
  `resumeResultMsg{…}` return sites.
- A structured outcome *does* exist one layer over (`actuationOutcome`,
  `model.go:4056` → `events.Outcome*`) but is written only to the event log, not
  returned to the caller.
- All six sentinels live in `internal/control/` (`hostsend.go` lines 68/76/89/99/117,
  `iterm2spawn.go:200`); `IsHostSendTier` is `control/actuator.go:71`.
- `refuseIfAmbiguous` (`model.go:4189`) reads `m.loops`; it is called at 7 sites.
- The three prose helpers exist at `model.go:3979`/`4261`/`4288`.

**Not line-verified — verify before building on:**
- `approveCmd` / `interruptCmd` / `killCmd` / auto-redrive-429 were **not** traced
  in the spike. They are assumed to share the same seam; §3 records the drift risk
  if that assumption is wrong. Confirm at implementation start.
- Whether the policy needs anything from `internal/control` that is not already
  exported to the TUI (decides whether §2.1's sub-package can collapse back).

## 5. Alternatives considered and rejected

- **Reimplement the policy in the agent-operator layer.** Rejected — roadmap §2.1's
  central finding. Two copies of the honesty rules guarantee drift; the roadmap
  logged 13 comment-vs-code drifts in a single day, and duplicating the policy would
  make that failure structural rather than incidental.
- **Put the policy in `internal/control` directly.** Not rejected outright, but not
  the starting point (§2.1): it mixes the composition (policy) into the adapter
  package. Deferred as a possible collapse-back if the spike's open question
  resolves that way.
- **Return a richer result (full event, remedy text, structured gate contents).**
  Rejected as over-engineering for this seam (Simplicity First). Five verdicts plus a
  sentinel error is exactly what the agent must branch on; prose stays in the TUI.
- **Extract the `k`/`x` confirm gate at the same time.** Rejected — §2.4. It is a
  human-fingers brake; the agent needs a different one, and that is separate work.

## 6. Prior art

- **This repo's `docs/adr-loop-state-model.md`** is the direct precedent. Its §2.3
  ("actions follow capability, not display state") already separates the *decision*
  from the *rendered state*; this ADR applies the same separation to actuation
  output — a machine-readable verdict underneath, prose on top. Its §1.3 ("failure
  messages assert remedies they never checked") is the same fusion this ADR removes,
  seen from the message-authoring side.
- **`docs/adr-vendor-independent-actuation.md`** established the six sentinels and
  the Tier 1h/Tier 2 distinction this policy composes. Its honesty discipline —
  "don't claim more than you know," which is why `ErrSendDeliveryUnknown` exists as
  a type — is exactly what makes the verdict enum machine-readable for free (roadmap
  §2.2): a distinction built to be honest to a human turns out to be the distinction
  an agent must branch on.
