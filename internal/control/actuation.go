// Registry-keyed actuation resolution (ADR Phase 2, §2.2/§3 step 2):
// session identity (internal/sessions, written by the SessionStart hook)
// replaces cwd-guessing as the PRIMARY way to find where a typed action
// should land. tty is session-unique — two sessions sharing a project
// directory stop being ambiguous — so this tier needs no ambiguity guard;
// the cwd-based LocateClaude chain stays as the fallback for sessions with no
// (or a stale) registry entry. That fallback now probes every available
// backend (not just Resolve()'s single pick) and refuses on cross-backend
// ambiguity — see ResolveActuationTarget's Tier 1b doc.
//
// Between those two sits Tier 1h, an in-place write by the HOST terminal
// itself (see hostsend.go), which needs no multiplexer at all — so this file's
// "is a backend available?" gate deliberately sits below it rather than above.
package control

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/jitokim/fleetops/internal/sessions"
)

// pidTTYTimeout bounds the `ps -o tty= -p <pid>` binding probe.
const pidTTYTimeout = 2 * time.Second

// noControllingTTY is `ps -o tty=`'s sentinel for a process with no
// controlling terminal — same convention internal/sessions.resolveTTY
// already relies on.
const noControllingTTY = "??"

// pidTTYFn reports pid's CURRENT controlling tty (normalized, no "/dev/"
// prefix — see normalizeTTY), or "" if the process is dead OR has no
// controlling terminal. A var, not a plain func, so tests can fake the OS
// process table without a real one — same injectable-seam pattern as
// internal/sessions' ancestryStepFunc.
//
// This is a BINDING check, not a liveness check: it doesn't just prove some
// process holds pid, it proves that process CURRENTLY controls a specific
// tty — see ResolveActuationTarget's doc for why the distinction matters.
var pidTTYFn = func(pid int) string {
	ctx, cancel := context.WithTimeout(context.Background(), pidTTYTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", "tty=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	tty := strings.TrimSpace(string(out))
	if tty == noControllingTTY {
		return ""
	}
	return normalizeTTY(tty)
}

// ResolveActuationTarget resolves where a typed/destructive action
// (resume/approve/interrupt/kill/inject) should land, per the ADR's tier
// policy:
//
//   - Tier 1a — the session registry's tty (internal/sessions), probed across
//     EVERY available backend that implements TTYLocator (see control.go's doc
//     — verified per-backend, e.g. cmux's tree carries tty directly; orca's
//     terminal list/show schema carries none, confirmed live). The registry
//     binding is validated ONCE, backend-independently, before any backend is
//     probed: tried only when the registry has an entry, it carries a tty, AND
//     a live `ps` confirms the recorded pid CURRENTLY controls that SAME tty
//     right now (pidTTYFn) — never trust a possibly-stale registry record on
//     its own. Proving the pid merely exists is NOT enough: ttys are
//     OS-recycled, so a SIGKILL'd session can leak a registry entry whose tty
//     gets reassigned to a completely different, unrelated live claude pane,
//     and/or whose pid gets reused by any other process — pid-existence alone
//     would pass in both cases and misroute an action onto the wrong session.
//     Re-validating the BINDING (this exact pid ↔ this exact tty, right now) is
//     what ADR §3 step 2 means by "re-validate tty↔pid against live ps at
//     actuation time." Once the binding checks out the tty is session-unique,
//     so the FIRST backend whose LocateByTTY hits wins with no ambiguity guard
//     — probing across all backends (not just Resolve()'s single preferred
//     one) is what lets a session hosted in a NON-preferred backend's surface
//     (e.g. a tmux pane on a machine where orca is also installed and
//     preferred) still be found by Tier 1a instead of silently falling to
//     Tier 1b/2.
//   - Tier 1h — the HOST terminal writes to the session in place, keyed by the
//     registry entry's host_app + window_id (see SendAdapter). Reuses Tier 1a's
//     already-computed binding validation, so it costs no extra `ps`. Ordered
//     between 1a and 1b on purpose — see the inline comment at the dispatch
//     site for the wrong-pane safety argument. Unlike every other tier, 1h
//     verifies its target binding a SECOND time inside the actuation itself
//     (the host reports the session's own tty in the same round trip), which is
//     why a resolved 1h actuator can still honestly refuse at send time.
//   - Tier 1b — the cwd-based LocateClaude probe, now run across EVERY
//     available backend (not just Resolve()'s single pick). Because cwd is
//     many-to-one this CANNOT stop at the first hit: it must count matches and
//     REFUSE (found=false) when two or more DISTINCT backends each return a
//     claude surface for the same projectDir — the cross-backend analogue of
//     LocateClaude's own single-backend ">1 match" refusal. Exactly one
//     matching backend → use it; zero → not found.
//
// backendAvailable=false means NO multiplexer backend is available AT ALL and
// Tier 1h did not resolve either (caller's message: "no orca/tmux/cmux").
// Tier 1h is deliberately checked BEFORE that gate — it needs no multiplexer,
// so a host-send-capable session on a multiplexer-less machine reports
// backendAvailable=true and the caller's "no orca/tmux/cmux" hint is correctly
// never shown. backendAvailable=true with found=false means
// backends were available but none could locate/disambiguate a claude surface
// — including the cross-backend ambiguity refusal above (caller's message:
// "no unambiguous claude surface"). Callers only use act when found=true.
//
// It returns a target-BOUND Actuator rather than the (Controller, Target) pair
// it used to: see Actuator's doc for why that pair was one level too wide, and
// why narrowing it is what lets a non-multiplexer host participate at all.
func ResolveActuationTarget(sessionsDir, sessionID, projectDir string) (act Actuator, backendAvailable, found bool) {
	// Availability is probed ONCE, up front, and both tiers iterate the result.
	// Available() is a live subprocess (LookPath + a bounded liveness probe per
	// backend), so re-asking per tier cost up to 3 spawns per backend on every
	// actuation keypress. Snapshotting is also the more honest semantics: a
	// backend that dies mid-resolution previously produced an arbitrary
	// tier-dependent split (visible to Tier 1a, gone by Tier 1b) that nothing
	// relied on.
	avail := availableBackends()

	// Tiers 1a and 1h share ONE registry read and ONE pid↔tty binding probe:
	// both require the same guarantee (this pid controls this tty right now),
	// and re-probing would cost a second `ps` on every actuation keypress.
	if entry, err := sessions.ReadSession(sessionsDir, sessionID); err == nil && entry.TTY != "" && pidTTYFn(entry.PID) == normalizeTTY(entry.TTY) {
		// Tier 1a — session-unique tty. Probe every available TTYLocator
		// backend; first hit wins (no ambiguity guard needed).
		for _, c := range avail {
			if t, ok := tierOneA(c, entry.TTY); ok {
				return boundController{ctrl: c, target: t}, true, true
			}
		}
		// Tier 1h — the host terminal writes to the session in place, keyed by
		// the registry's host_app + window_id.
		//
		// AFTER 1a, deliberately, and this is a SAFETY property rather than a
		// preference. The dangerous case is a multiplexer running INSIDE an
		// iTerm2 window: if the hook recorded HostApp "iTerm.app" for such a
		// session, writing to the iTerm2 session would deliver keystrokes to
		// whichever pane is currently active in that window. Letting 1a win
		// first means a multiplexer that can address the precise pane always
		// does. (The adapter's own tty guard then refuses the residual case;
		// defense in depth is the house style here.)
		//
		// BEFORE 1b, deliberately. 1h is session-EXACT — the window id comes
		// from that very session's own $ITERM_SESSION_ID — whereas 1b is
		// cwd-based and many-to-one. A strictly more precise tier must never be
		// shadowed by a guessing one.
		//
		// No ambiguity guard, for the same reason Tier 1a has none: the
		// identifier is session-unique by construction. Ambiguity is a cwd
		// disease.
		//
		// An unknown or empty host_app resolves nothing and falls straight
		// through to 1b, which is what keeps this a pure superset for existing
		// orca/cmux/tmux users.
		if adapter, ok := ResolveSendAdapter(entry.HostApp); ok {
			return boundSendAdapter{adapter: adapter, entry: entry}, true, true
		}
	}

	// The "no backend at all" gate sits BELOW 1a/1h, not above them. Tier 1h
	// needs no multiplexer — the host terminal writes to its own session — so
	// gating it on availableBackends() made the tier unreachable for a fresh
	// macOS + iTerm2 user with no orca/tmux/cmux installed, i.e. precisely the
	// person it was added for.
	//
	// Widening the gate cannot disturb any existing user: the ONLY sessions
	// whose outcome changes are those whose recorded host_app has a registered
	// SendAdapter AND whose pid↔tty binding validates, and before this feature
	// existed that set was empty by construction. Everything else still reaches
	// the identical `return nil, false, false` and the identical
	// "no orca/tmux/cmux" message.
	//
	// Tier 1a's loop above is a no-op when avail is empty, so ordering costs
	// nothing here.
	if len(avail) == 0 {
		return nil, false, false
	}

	// Tier 1b — cwd is many-to-one, so probe ALL available backends and count
	// matches; >=2 distinct backends matching is cross-backend ambiguity and
	// must refuse, never silently pick one.
	var matchedCtrl Controller
	var matchedTarget Target
	matches := 0
	for _, c := range avail {
		if t, ok := c.LocateClaude(projectDir); ok {
			matchedCtrl, matchedTarget = c, t
			matches++
		}
	}
	if matches == 1 {
		return boundController{ctrl: matchedCtrl, target: matchedTarget}, true, true
	}
	return nil, true, false
}

// availableBackends returns the backends usable right now, in the shared
// install-preference order. Empty means no MULTIPLEXER backend is available.
// That is one input to ResolveActuationTarget's backendAvailable=false
// contract, not the whole of it: Tier 1h resolves ahead of the emptiness gate
// and reports backendAvailable=true with no multiplexer installed at all.
// Returning the slice (rather than a bare
// bool) is what lets both tiers reuse one round of Available() probes instead
// of re-execing per tier; preserving the order keeps each tier's iteration —
// and so Tier 1b's ambiguity counting — identical to probing `backends`
// directly.
func availableBackends() []Controller {
	avail := make([]Controller, 0, len(backends))
	for _, c := range backends {
		if c.Available() {
			avail = append(avail, c)
		}
	}
	return avail
}

// tierOneA type-asserts c as a TTYLocator and, if it implements the interface,
// tries to locate a surface by tty — pulled out as its own pure function (same
// reasoning as every other type-assert-then-call seam in this package) so
// "dispatch Tier 1a to a candidate backend that implements TTYLocator" is
// directly unit-testable against a fake Controller, without needing a real
// orca/cmux/tmux binary on the test machine. Called once per AVAILABLE backend
// (not once for a single pre-resolved pick) — Tier 1a probes them all.
func tierOneA(c Controller, tty string) (Target, bool) {
	locator, ok := c.(TTYLocator)
	if !ok {
		return Target{}, false
	}
	return locator.LocateByTTY(tty)
}

// redriveTimeout bounds the Tier-2 headless re-drive call. LONG — a full
// agent turn can legitimately take minutes; this is not a quick keystroke
// send like the other actuation calls (see actuationTimeout).
const redriveTimeout = 10 * time.Minute

// Redrive continues sessionID as a fresh HEADLESS turn against its existing
// transcript: `claude --resume <sessionID> -p "<prompt>"` recalls context,
// returns the SAME session_id, and appends to the SAME transcript JSONL the
// cockpit already tails — verified live (see
// docs/adr-vendor-independent-actuation.md §2.2 Tier 2). Vendor-independent:
// works on every host (IntelliJ, a bare terminal, anything), zero tty
// injection, zero multiplexer dependency — the actual vendor-independent
// actuation path, just not "in-place."
//
// Deliberately a standalone function, not a Controller method: unlike
// Resume/Approve/Interrupt (which act on a Target — a specific terminal
// surface a Controller located), Redrive doesn't touch any terminal at all,
// so it doesn't belong behind the per-backend Controller abstraction. The
// command's own stdout is discarded and only its exit status matters — the
// point isn't to read the reply here, it's that the turn lands in the
// transcript, which the cockpit picks up on its next scan.
// Deliberately NOT configurable via spawn.command. Tier 2 is the universal
// path — it serves sessions fleetops merely OBSERVES (a human's session started
// in another editor entirely), not just loops it spawned. Letting a setting
// named spawn.command rewrite this invocation meant the operator's choices for
// loops fleetops CREATES silently became the posture for re-driving loops it
// never created: a spawn.command carrying --dangerously-skip-permissions would
// apply that to someone else's session. That crosses the owned/observed line
// docs/adr-loop-state-model.md draws, so the command here stays "claude".
//
// cwd is the loop's OWN working directory and is load-bearing, not cosmetic:
// Claude Code scopes sessions by project (their cwd), so `claude --resume
// <id>` can only find <id>'s transcript when it runs from that session's OWN
// project directory. Run it from anywhere else (e.g. fleetops' own cwd) and
// resume fails with exit status 1 — the whole reason this parameter exists.
// This is exactly why the sibling bootstrapClaudeFn sets cmd.Dir=cwd too, and
// why Redrive was the one actuation path silently broken for loops living in a
// different directory than fleetops: it execs claude but forgot to. Do NOT
// re-drop cmd.Dir — see buildRedriveCmd. An empty cwd is refused up front
// rather than allowed to fall back to the process dir (cmd.Dir=""), which is
// the exact broken behavior this fixes.
//
// # Multi-account (Phase A): deliberately NO CLAUDE_CONFIG_DIR here
//
// Redrive RESUMES an existing session; its account was fixed when the session
// was first started. Prefixing it with the accounts binding's config dir would
// let a re-drive SWITCH accounts out from under a live session, which is never
// what a resume should do — so the account injection that spawncmd.go layers
// onto SPAWN (spawnArgvForCwd) is intentionally absent from this path.
//
// There is a real Phase B nuance to revisit: a session first started under a
// non-default CLAUDE_CONFIG_DIR may need that SAME dir set to be found on
// resume. But the right source for that is the config dir RECORDED for the
// session (Phase B captures it via the SessionStart hook), NOT the accounts
// binding for cwd — the two can disagree, and a resume must honor the session's
// own recorded account, not whatever the directory is currently bound to. Phase
// A leaves Redrive untouched; wiring the recorded config dir is Phase B's job.
func Redrive(cwd, sessionID, prompt string) error {
	if cwd == "" {
		return fmt.Errorf("claude --resume: refusing to re-drive %s with no cwd — sessions are cwd/project-scoped, so resuming from the wrong directory silently fails", sessionID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), redriveTimeout)
	defer cancel()
	if err := buildRedriveCmd(ctx, cwd, sessionID, prompt).Run(); err != nil {
		return fmt.Errorf("claude --resume: %w", err)
	}
	return nil
}

// buildRedriveCmd assembles Tier 2's exec.Cmd with its working directory set to
// the session's own project cwd. Split out from Redrive as a testable seam (the
// same shape bootstrap uses) so a unit test can assert cmd.Dir == cwd and the
// argv is redriveArgv's fixed invocation WITHOUT spawning a real claude — the
// cwd wiring was invisible precisely because nothing exercised it in isolation.
func buildRedriveCmd(ctx context.Context, cwd, sessionID, prompt string) *exec.Cmd {
	argv := redriveArgv(sessionID, prompt)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = cwd
	return cmd
}

// redriveArgv is Tier 2's fixed invocation. Each element is structural:
// --resume names the session to continue, -p makes the turn headless, and
// --output-format json is the shape this path's contract is written against.
// Kept a function rather than inlined so the contract has one greppable home.
func redriveArgv(sessionID, prompt string) []string {
	return []string{"claude", "--resume", sessionID, "-p", prompt, "--output-format", "json"}
}
