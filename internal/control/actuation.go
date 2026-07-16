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
	"time"

	"github.com/jitokim/missionctl/internal/sessions"
)

// pidAliveTimeout bounds the `ps -p <pid>` liveness probe.
const pidAliveTimeout = 2 * time.Second

// pidAliveFn reports whether pid is a live process (`ps -p <pid>` exit 0).
// A var, not a plain func, so tests can fake liveness without a real
// process table — same injectable-seam pattern as internal/sessions'
// ancestryStepFunc.
var pidAliveFn = func(pid int) bool {
	ctx, cancel := context.WithTimeout(context.Background(), pidAliveTimeout)
	defer cancel()
	return exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid)).Run() == nil
}

// ResolveActuationTarget resolves where a typed/destructive action
// (resume/approve/interrupt/kill/inject) should land, per the ADR's tier
// policy:
//
//   - Tier 1a — the session registry's tty (internal/sessions, tmux-only
//     this slice: orca/cmux don't expose a per-terminal tty). Tried only
//     when the registry has an entry, it carries a tty, AND the recorded
//     pid is confirmed alive RIGHT NOW via a live `ps` (never trust a
//     possibly-stale registry record — ttys are OS-recycled). Session-unique:
//     no ambiguity guard applies on this path.
//   - Tier 1b — the existing cwd-based Resolve()+LocateClaude chain,
//     unchanged, including its own internal ">1 match" ambiguity refusal.
//
// backendAvailable=false means no backend resolved AT ALL (caller's message:
// "no orca/tmux/cmux"). backendAvailable=true with found=false means a
// backend resolved but couldn't locate/disambiguate a surface (caller's
// message: "no unambiguous claude surface"). Callers only use ctrl/target
// when found=true.
func ResolveActuationTarget(sessionsDir, sessionID, projectDir string) (ctrl Controller, target Target, backendAvailable, found bool) {
	if entry, err := sessions.ReadSession(sessionsDir, sessionID); err == nil && entry.TTY != "" && pidAliveFn(entry.PID) {
		tmux := tmuxController{}
		if t, ok := tmux.LocateByTTY(entry.TTY); ok {
			return tmux, t, true, true
		}
	}
	resolved, resolvedOK := Resolve()
	if !resolvedOK {
		return nil, Target{}, false, false
	}
	t, locateOK := resolved.LocateClaude(projectDir)
	return resolved, t, true, locateOK
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
	if err := exec.CommandContext(ctx, "claude", "--resume", sessionID, "-p", prompt, "--output-format", "json").Run(); err != nil {
		return fmt.Errorf("claude --resume: %w", err)
	}
	return nil
}
