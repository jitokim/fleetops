// Registry-keyed actuation resolution (ADR Phase 2, §2.2/§3 step 2):
// session identity (internal/sessions, written by the SessionStart hook)
// replaces cwd-guessing as the PRIMARY way to find where a typed action
// should land. tty is session-unique — two sessions sharing a project
// directory stop being ambiguous — so this tier needs no ambiguity guard;
// the cwd-based LocateClaude chain stays as the fallback for sessions with no
// (or a stale) registry entry. That fallback now probes every available
// backend (not just Resolve()'s single pick) and refuses on cross-backend
// ambiguity — see ResolveActuationTarget's Tier 1b doc.
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
//   - Tier 1b — the cwd-based LocateClaude probe, now run across EVERY
//     available backend (not just Resolve()'s single pick). Because cwd is
//     many-to-one this CANNOT stop at the first hit: it must count matches and
//     REFUSE (found=false) when two or more DISTINCT backends each return a
//     claude surface for the same projectDir — the cross-backend analogue of
//     LocateClaude's own single-backend ">1 match" refusal. Exactly one
//     matching backend → use it; zero → not found.
//
// backendAvailable=false means NO backend is available AT ALL (caller's
// message: "no orca/tmux/cmux"). backendAvailable=true with found=false means
// backends were available but none could locate/disambiguate a claude surface
// — including the cross-backend ambiguity refusal above (caller's message:
// "no unambiguous claude surface"). Callers only use ctrl/target when
// found=true.
func ResolveActuationTarget(sessionsDir, sessionID, projectDir string) (ctrl Controller, target Target, backendAvailable, found bool) {
	// Availability is probed ONCE, up front, and both tiers iterate the result.
	// Available() is a live subprocess (LookPath + a bounded liveness probe per
	// backend), so re-asking per tier cost up to 3 spawns per backend on every
	// actuation keypress. Snapshotting is also the more honest semantics: a
	// backend that dies mid-resolution previously produced an arbitrary
	// tier-dependent split (visible to Tier 1a, gone by Tier 1b) that nothing
	// relied on.
	avail := availableBackends()
	if len(avail) == 0 {
		return nil, Target{}, false, false
	}

	// Tier 1a — session-unique tty. Validate the registry binding once, then
	// probe every available TTYLocator backend; first hit wins (no ambiguity).
	if entry, err := sessions.ReadSession(sessionsDir, sessionID); err == nil && entry.TTY != "" && pidTTYFn(entry.PID) == normalizeTTY(entry.TTY) {
		for _, c := range avail {
			if t, ok := tierOneA(c, entry.TTY); ok {
				return c, t, true, true
			}
		}
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
		return matchedCtrl, matchedTarget, true, true
	}
	return nil, Target{}, true, false
}

// availableBackends returns the backends usable right now, in the shared
// install-preference order. Empty means NO backend is available at all — the
// single "is actuation possible?" gate behind ResolveActuationTarget's
// backendAvailable=false contract. Returning the slice (rather than a bare
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
func Redrive(sessionID, prompt string) error {
	ctx, cancel := context.WithTimeout(context.Background(), redriveTimeout)
	defer cancel()
	argv := redriveArgv(sessionID, prompt)
	if err := exec.CommandContext(ctx, argv[0], argv[1:]...).Run(); err != nil {
		return fmt.Errorf("claude --resume: %w", err)
	}
	return nil
}

// redriveArgv builds Redrive's argv — pulled out as its own pure function
// so the exact command shape is directly unit-testable, same pattern as
// orcaResumeCmd/tmuxResumeCmds.
func redriveArgv(sessionID, prompt string) []string {
	return []string{"claude", "--resume", sessionID, "-p", prompt, "--output-format", "json"}
}
