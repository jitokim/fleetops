// Registry-keyed actuation resolution (ADR Phase 2, §2.2/§3 step 2):
// session identity (internal/sessions, written by the SessionStart hook)
// replaces cwd-guessing as the PRIMARY way to find where a typed action
// should land. tty is session-unique — two sessions sharing a project
// directory stop being ambiguous — so this tier needs no ambiguity guard;
// the existing cwd-based Controller.Resolve()+LocateClaude chain stays as
// the fallback for sessions with no (or a stale) registry entry, and it
// keeps its own ambiguity refusal unchanged.
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
//   - Tier 1a — the session registry's tty (internal/sessions), dispatched
//     to WHICHEVER backend Resolve() picks, if (and only if) that backend
//     implements TTYLocator (see control.go's doc — verified per-backend,
//     e.g. cmux's tree carries tty directly; orca's terminal list/show
//     schema carries none, confirmed live). Tried only when the registry has
//     an entry, it carries a tty, AND a live `ps` confirms the recorded pid
//     CURRENTLY controls that SAME tty right now (pidTTYFn) — never trust a
//     possibly-stale registry record on its own. Proving the pid merely
//     exists is NOT enough: ttys are OS-recycled, so a SIGKILL'd session can
//     leak a registry entry whose tty gets reassigned to a completely
//     different, unrelated live claude pane, and/or whose pid gets reused by
//     any other process — pid-existence alone would pass in both cases and
//     misroute an action onto the wrong session. Re-validating the BINDING
//     (this exact pid ↔ this exact tty, right now) is what ADR §3 step 2
//     means by "re-validate tty↔pid against live ps at actuation time."
//     Session-unique once the binding checks out: no ambiguity guard applies
//     on this path. NOTE: this ties Tier 1a to Resolve()'s single preferred
//     backend (orca→cmux→tmux) — a session actually hosted in a NON-preferred
//     backend's surface (e.g. a tmux pane on a machine where orca is also
//     installed and preferred) won't be found via Tier 1a even if that other
//     backend implements TTYLocator; it falls to Tier 1b/2 instead. Accepted
//     trade for a single, predictable "whichever backend is resolved" rule
//     rather than probing every installed backend on every actuation.
//   - Tier 1b — the existing cwd-based Resolve()+LocateClaude chain,
//     unchanged, including its own internal ">1 match" ambiguity refusal.
//
// backendAvailable=false means no backend resolved AT ALL (caller's message:
// "no orca/tmux/cmux"). backendAvailable=true with found=false means a
// backend resolved but couldn't locate/disambiguate a surface (caller's
// message: "no unambiguous claude surface"). Callers only use ctrl/target
// when found=true.
func ResolveActuationTarget(sessionsDir, sessionID, projectDir string) (ctrl Controller, target Target, backendAvailable, found bool) {
	resolved, resolvedOK := Resolve()

	if entry, err := sessions.ReadSession(sessionsDir, sessionID); err == nil && entry.TTY != "" && pidTTYFn(entry.PID) == normalizeTTY(entry.TTY) {
		if resolvedOK {
			if t, ok := tierOneA(resolved, entry.TTY); ok {
				return resolved, t, true, true
			}
		}
	}

	if !resolvedOK {
		return nil, Target{}, false, false
	}
	t, locateOK := resolved.LocateClaude(projectDir)
	return resolved, t, true, locateOK
}

// tierOneA type-asserts resolved as a TTYLocator and, if it implements the
// interface, tries to locate a surface by tty — pulled out as its own pure
// function (same reasoning as every other type-assert-then-call seam in this
// package) so "dispatch Tier 1a to whichever resolved backend implements
// TTYLocator" is directly unit-testable against a fake Controller, without
// needing a real orca/cmux/tmux binary on the test machine.
func tierOneA(resolved Controller, tty string) (Target, bool) {
	locator, ok := resolved.(TTYLocator)
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
