package control

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/jitokim/fleetops/internal/sessions"
)

// iterm2Spawner starts a brand new claude loop in a fresh iTerm2 window, with
// NO multiplexer involved — the spawn half of the iTerm2 backend, sibling to
// the already-shipped Tier 1h in-place send (hostsend.go) and focus
// (focus.go) adapters.
//
// It exists because pressing "n" on a machine with only iTerm2 installed
// failed outright: every spawn path went through a Controller, and
// control.Resolve only knows orca/cmux/tmux. A user moving to iTerm2-only had
// a cockpit that could observe and actuate loops but could not create one.
//
// # Why this is NOT a Controller in `backends`
//
// Adding it to the `backends` slice would have been the obvious move and is
// the wrong one: that slice feeds ResolveForLocate and ResolveActuationTarget,
// so a new entry silently joins Tier 1b's cwd-based LocateClaude probe and its
// cross-backend ambiguity counting — i.e. it would change actuation dispatch,
// which this slice must not touch. Spawning is the only capability being
// added, so it is exposed through the narrow Spawner seam (see control.go) and
// resolved AFTER every multiplexer. Existing orca/cmux/tmux users reach an
// identical code path to before; only a machine where no multiplexer is
// available can ever land here.
type iterm2Spawner struct{}

func (iterm2Spawner) Name() string { return "iterm2" }

// iterm2HostDetectFn reports whether this process is itself running inside
// iTerm2 on macOS. A package var so the whole spawner is testable on any
// machine, in the same seam style as iterm2SendFn/iterm2FocusFn.
var iterm2HostDetectFn = func() bool {
	return runtime.GOOS == "darwin" && os.Getenv("TERM_PROGRAM") == iterm2HostApp
}

// Available reports whether an iTerm2 spawn can be attempted.
//
// The test is "$TERM_PROGRAM says we are running INSIDE iTerm2", not "iTerm2
// is installed somewhere". That is deliberately narrow, and it is the honest
// test rather than the generous one:
//
//   - It needs no subprocess, no Apple Events, and no Automation permission
//     prompt, so it costs nothing on the availability path — which
//     ResolveSpawner calls on every spawn.
//   - It cannot LAUNCH iTerm2 as a side effect of asking. An availability
//     probe that opens an application is not a probe.
//   - If the cockpit is running in iTerm2, iTerm2 is provably running right
//     now — the strongest evidence obtainable this cheaply.
//
// The cost is a false negative: running fleetops from Terminal.app or Ghostty
// on a machine that also has iTerm2 reports unavailable, and spawn falls back
// to the multiplexer path or the manual hint. That is the right way round —
// claiming a spawn surface we cannot prove is how false success starts, and
// the iTerm2-only dogfooding case this was built for always runs the cockpit
// inside iTerm2.
func (iterm2Spawner) Available() bool { return iterm2HostDetectFn() }

// iterm2SpawnFn runs the window-creating osascript and returns its stdout.
// Injectable seam, same discipline and the same timeout classification as
// iterm2SendFn — a deadline kill is separated out as ErrSendDeliveryUnknown,
// because a killed osascript may have created a window we can no longer see.
var iterm2SpawnFn = func(argv []string) (string, error) {
	return iterm2SendFn(argv)
}

// iterm2BootWaitFn pauses for claude's TUI to finish booting inside the new
// window before the goal is typed into it. iTerm2 has no equivalent of orca's
// `wait --for tui-idle`, so this is a flat sleep — the same pragmatic choice
// tmux's Spawn already makes, reusing its constant rather than inventing a
// second one. A var so tests need not actually wait.
var iterm2BootWaitFn = func() { time.Sleep(spawnBootWait) }

// ErrITerm2SpawnNoSession reports that osascript exited 0 but returned nothing
// this package can recognize as a session — no GUID, no tty, or values failing
// their whitelists.
//
// Its own sentinel, and it fails CLOSED: osascript exits 0 in several
// situations where no usable window exists, so treating unrecognized output as
// success would report a spawned loop that is not there. This repo has a P0
// history of exactly that (see focus.go's verdict protocol), and a spawn is
// the worst place to repeat it — the human walks away believing work has
// started.
var ErrITerm2SpawnNoSession = errors.New("control: iTerm2 did not report a usable new session — the window may not have been created")

// iterm2Session is a just-created iTerm2 session, identified by the two values
// every subsequent operation needs. The GUID is a sessionGUID, so it CANNOT
// exist unless it passed itermGUIDPattern — the validation lives in the type,
// not in a caller's manners (see hostsend.go's sessionGUID).
type iterm2Session struct {
	guid sessionGUID
	tty  string // normalized, no "/dev/" prefix
}

// Spawn creates a fresh iTerm2 window running the configured spawn command in
// cwd, waits for claude's TUI to boot, then types the goal and submits it.
//
// # How this avoids reporting a false success
//
// Three checks, none of which trusts an exit status:
//
//  1. The script must RETURN a GUID and a tty, both of which must pass their
//     whitelists (parseITerm2SpawnResult). osascript exits 0 even when it
//     produced nothing useful, so its own report is the only evidence.
//  2. The goal delivery goes through the existing, already-hardened
//     iterm2SendAdapter, which re-finds the session BY GUID and refuses unless
//     the session's own tty still matches the one we just recorded. A window
//     that died between creation and delivery returns "miss" and fails.
//  3. The launch line ends with `|| exit 1` (see iterm2LaunchLine), so a
//     failed cd or a missing claude binary CLOSES the window rather than
//     leaving a bare shell behind. Without that, step 2 would happily type the
//     goal into a shell prompt and report success — the exact false-success
//     shape this project treats as a defect.
//
// So the goal delivery doubles as the liveness check: nothing here claims a
// loop started until something is provably still alive in that session and
// bound to the tty we created.
func (iterm2Spawner) Spawn(cwd, goal string) error {
	session, err := iterm2CreateSession(cwd, spawnCommandFn())
	if err != nil {
		return err
	}
	iterm2BootWaitFn()
	// Re-uses the Tier 1h send path verbatim rather than writing a second
	// osascript: the goal is untrusted free text, and hostsend.go is where the
	// argv-only discipline for that is already established and tested.
	entry := sessions.SessionEntry{HostApp: iterm2HostApp, WindowID: session.guid.String(), TTY: session.tty}
	if err := (iterm2SendAdapter{}).SendText(entry, goal); err != nil {
		return fmt.Errorf("iterm2: created a window on %s but could not deliver the goal there: %w", session.tty, err)
	}
	return nil
}

// iterm2CreateSession runs the window-creating script and validates what came
// back.
func iterm2CreateSession(cwd string, spawnArgv []string) (iterm2Session, error) {
	out, err := iterm2SpawnFn(iterm2SpawnArgv(iterm2LaunchLine(cwd, spawnArgv)))
	if err != nil {
		// Includes the deadline case, already classified as
		// ErrSendDeliveryUnknown: a killed osascript may have left a real
		// window behind, so this must not be reported as "nothing happened."
		return iterm2Session{}, fmt.Errorf("iterm2: creating a window: %w", err)
	}
	session, ok := parseITerm2SpawnResult(out)
	if !ok {
		return iterm2Session{}, fmt.Errorf("%w (osascript returned %q)", ErrITerm2SpawnNoSession, strings.TrimSpace(out))
	}
	return session, nil
}

// iterm2SpawnResultSeparator is the delimiter the script joins its two return
// values with — AppleScript's `tab` constant. A tab, not a space, because a
// tty path cannot contain one, so the split is unambiguous.
const iterm2SpawnResultSeparator = "\t"

// parseITerm2SpawnResult validates the script's "GUID<tab>/dev/ttysNNN" reply.
//
// ok=false on anything unexpected — the wrong field count, a GUID failing
// itermGUIDPattern, an empty or non-device-shaped tty. It never repairs or
// guesses: a value that cannot address a real session is not made safe by
// being tidied up, and the caller's honest failure is a far better outcome
// than a spawn reported against a session that does not exist.
func parseITerm2SpawnResult(out string) (iterm2Session, bool) {
	fields := strings.Split(strings.TrimSpace(out), iterm2SpawnResultSeparator)
	if len(fields) != 2 {
		return iterm2Session{}, false
	}
	guid, ok := newSessionGUID(strings.TrimSpace(fields[0]))
	if !ok {
		return iterm2Session{}, false
	}
	tty := normalizeTTY(strings.TrimSpace(fields[1]))
	if !itermTTYPattern.MatchString(tty) {
		return iterm2Session{}, false
	}
	return iterm2Session{guid: guid, tty: tty}, true
}

// iterm2LaunchLine builds the shell line typed into the new window's shell:
//
//	cd '<cwd>' && exec <spawn argv> || exit 1
//
// A shell line is unavoidable here — `create window with default profile`
// starts the user's login shell, and setting a working directory is a shell
// operation. So both interpolated values are SHELL-QUOTED (shellQuoteJoin),
// which is the same treatment orca's --command already gets and the reason
// that helper exists. The finished line then crosses into AppleScript as
// osascript ARGV, never as script source — see iterm2SpawnScript.
//
// `exec` replaces the shell with claude, so the session's process IS claude:
// closing the loop closes the window, matching what `tmux new-window claude`
// already does.
//
// `|| exit 1` is a false-success guard, not tidiness. Without it, a cd that
// fails or a claude binary that is missing leaves a live bare shell sitting at
// a prompt — and Spawn's goal delivery would then succeed in typing the goal
// into that shell and report a loop that never started. Closing the window
// instead turns the same situation into an honest delivery failure. The cost
// is that the human loses the shell's error text; that is the right trade
// against silently claiming work has begun.
func iterm2LaunchLine(cwd string, spawnArgv []string) string {
	return "cd " + shellQuote(cwd) + " && exec " + shellQuoteJoin(spawnArgv) + " || exit 1"
}

// iterm2SpawnArgv assembles the osascript invocation:
//
//	osascript -e <fixed script> -- <launch line>
//
// The launch line travels in an ARGUMENT slot. Go's os/exec calls execve
// directly, so there is no shell between here and osascript and no second
// quoting layer to get wrong.
func iterm2SpawnArgv(launchLine string) []string {
	return []string{"osascript", "-e", iterm2SpawnScript, "--", launchLine}
}

// iterm2SpawnScript is the FIXED AppleScript source that creates the window.
//
// # INJECTION SAFETY
//
// It is a compile-time CONSTANT. Not a function returning a built string — a
// constant, containing no interpolation of any kind. That is a stronger
// guarantee than iterm2SendScript's (which must interpolate a validated
// sessionGUID because it has to FIND an existing session), and it is
// achievable here because a brand-new window needs no identifier to locate:
// the script holds a direct reference to the window it just made.
//
// The only untrusted value, the launch line, arrives as `item 1 of argv` — an
// already-typed AppleScript string that is never tokenized, compiled or
// re-parsed. AppleScript reaches `do shell script`, so a value able to close a
// string literal and append statements would be arbitrary local code
// execution; a prior review found exactly that Critical defect on this
// surface. TestITerm2SpawnScript_IsAConstantWithNoInterpolation and the
// byte-identity test enforce that it stays this way.
//
// It also never TRAVERSES the windows→tabs→sessions collection that
// focus.go/hostsend.go walk, so the iterate-and-mutate hazard those documents
// does not arise: `create window` hands back the new window directly.
//
// It returns the session's id and tty joined by tab. Returning both in the
// SAME round trip is what makes the result trustworthy — asking for the tty
// afterwards would be a second call against a session that could already have
// changed.
const iterm2SpawnScript = "on run argv\n" +
	"\ttell application \"iTerm2\"\n" +
	"\t\tset newWindow to (create window with default profile)\n" +
	"\t\tset newSession to current session of newWindow\n" +
	"\t\twrite newSession text (item 1 of argv) newline yes\n" +
	"\t\treturn ((id of newSession) & tab & (tty of newSession))\n" +
	"\tend tell\n" +
	"end run"

// Compile-time assurance the spawner satisfies the narrow spawn seam.
var _ Spawner = iterm2Spawner{}
