package control

import (
	"context"
	"errors"
	"os/exec"
	"regexp"
	"strings"

	"github.com/jitokim/fleetops/internal/sessions"
)

// SendAdapter delivers a typed action IN PLACE to the terminal surface a
// session physically lives in — the actuation sibling of FocusAdapter, keyed
// the same way (the registry entry's HostApp, i.e. $TERM_PROGRAM, which the
// SessionStart hook already observed, so the lookup needs no probing and no
// guessing).
//
// Deliberately NOT folded into FocusAdapter. Raising a window is idempotent
// and harmless; writing to a session is a destructive keystroke with different
// failure modes and a hard injection-safety requirement. focus.go states the
// boundary directly ("raising a window is the ONLY capability it has — it
// cannot send keys"), and keeping the two interfaces separate keeps that true
// and lets a future host implement one without the other.
//
// Methods per MECHANISM, not per verb: the whole verb table reduces to "type
// text, then submit" and "deliver a raw Esc." Every text-shaped verb is
// SendText with a different payload — resume/inject send the prompt, kill sends
// the literal "/exit" (kill is NOT a control character in this codebase, see
// killCmd), and approve sends "" for a bare submit.
type SendAdapter interface {
	// Name reports the mechanism for human-facing status text ("iterm2").
	Name() string
	// SendText types text into the session and submits it. An empty text is a
	// meaningful call: a bare submit (approve).
	SendText(entry sessions.SessionEntry, text string) error
	// Interrupt delivers a raw Esc without submitting — stop the current turn,
	// leave the process alive. The one control-character path; kept a separate
	// method (and a separate file) rather than a SendText payload so it stays
	// individually reviewable.
	Interrupt(entry sessions.SessionEntry) error
}

// ErrNoSendSurface reports that an adapter REFUSED before executing anything:
// the entry carried no usable window id / tty, or one of them failed its
// whitelist. Like ErrNoFocusSurface this is a fail-closed degrade signal, not a
// report that the write was attempted and lost.
var ErrNoSendSurface = errors.New("control: no send surface for this session")

// ErrSendNoSession reports that the host ran the script fine but found no
// session with that id — the tab was closed. Distinct from an exec failure
// because osascript exits 0 either way (the same P0 lesson that produced
// focus.go's ok/miss verdict protocol: a script that cannot report a miss makes
// every send look like a success).
var ErrSendNoSession = errors.New("control: the host has no session with this id — it was closed")

// ErrSendTTYMismatch reports the session was found but its tty no longer
// matches the one the registry recorded for this loop, so the write was
// REFUSED. Kept distinct from ErrSendNoSession rather than collapsed into a
// generic miss: this is a genuine wrong-terminal refusal (the
// tmux-inside-iTerm2 hazard, and tab recycling), and the operator needs to know
// which of the two happened. Its message is what the TUI surfaces verbatim.
var ErrSendTTYMismatch = errors.New("control: session found but its tty no longer matches this loop — attach (↵) and act manually")

// hostAppSendAdapters maps a $TERM_PROGRAM marker to its SendAdapter, mirroring
// hostAppFocusAdapters exactly. Multiplexers deliberately do NOT appear here —
// they are addressed by Tier 1a/1b through Controller, and adding one here
// would divert a loop off the proven path (same reasoning as FocusAdapter's).
var hostAppSendAdapters = map[string]SendAdapter{
	iterm2HostApp: iterm2SendAdapter{},
}

// ResolveSendAdapter returns the SendAdapter registered for hostApp, or
// (nil, false) when none is. An unknown or empty host_app degrades silently:
// Tier 1h is skipped and dispatch continues to Tier 1b exactly as it does
// today, which is what keeps this a pure superset for existing
// orca/cmux/tmux users.
func ResolveSendAdapter(hostApp string) (SendAdapter, bool) {
	if hostApp == "" {
		return nil, false
	}
	adapter, ok := hostAppSendAdapters[hostApp]
	return adapter, ok
}

// boundSendAdapter binds a SendAdapter to the registry entry it will act on,
// making it an Actuator — the host-send mirror of boundController. The verb
// mapping lives HERE, in one place, rather than in each adapter: kill's "/exit"
// arrives through Resume (see killCmd), and Approve is a bare submit, so both
// ride SendText.
type boundSendAdapter struct {
	adapter SendAdapter
	entry   sessions.SessionEntry
}

func (b boundSendAdapter) Resume(prompt string) error { return b.adapter.SendText(b.entry, prompt) }
func (b boundSendAdapter) Approve() error             { return b.adapter.SendText(b.entry, "") }
func (b boundSendAdapter) Interrupt() error           { return b.adapter.Interrupt(b.entry) }
func (b boundSendAdapter) Backend() string            { return b.adapter.Name() }
func (b boundSendAdapter) Tier() string               { return actuationTierHostSend }

// iterm2SendFn runs the osascript that writes to an iTerm2 session and returns
// its stdout — the script's verdict (see iterm2Send*). An injectable package
// var, same seam discipline as iterm2FocusFn/pidTTYFn/redriveFn, so the whole
// adapter is unit-testable against a fake with no iTerm2, no osascript and no
// macOS. Bounded by actuationTimeout so a wedged osascript never hangs the TUI.
var iterm2SendFn = func(argv []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), actuationTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).Output()
	return string(out), err
}

// The verdicts the osascript returns on stdout. osascript exits 0 whether it
// found the session, refused on a tty mismatch, or matched nothing at all, so
// the verdict is the ONLY thing that can distinguish them — exit status cannot.
const (
	iterm2SendHit         = "ok"
	iterm2SendMiss        = "miss"
	iterm2SendTTYMismatch = "ttymismatch"
)

// itermTTYPattern is the WHITELIST the registry-recorded tty must match before
// it may be used to address a session: bare device names only ("ttys006"), as
// normalizeTTY produces.
//
// The tty is passed to osascript as ARGV, not interpolated (see
// iterm2SendScript), so this whitelist is defense in depth rather than the
// primary control — but it is not decoration. TTY reaches us from the same
// untrusted ~/.fleetops/sessions/<id>.json that carries WindowID, so it gets
// the same treatment on principle: a value that cannot be a device name cannot
// address a real session either, and refusing early beats sending a
// nonsense comparison to the host.
var itermTTYPattern = regexp.MustCompile(`^[A-Za-z0-9]+$`)

// iterm2SendAdapter writes to the iTerm2 session identified by the entry's
// WindowID ($ITERM_SESSION_ID), after verifying that session's own tty still
// matches the entry's. It is the ADR's Tier 1h mechanism.
type iterm2SendAdapter struct{}

func (iterm2SendAdapter) Name() string { return "iterm2" }

// SendText types text into the session and submits it (write … newline yes,
// which iTerm2 delivers as a CR — a real Enter, measured at the byte level with
// `stty raw`; see .notes/design-iterm2-tier1.md §8b/E2).
//
// text is UNTRUSTED free-form human input (the inject verb's whole purpose) and
// is passed as osascript ARGV — never interpolated into the script. See
// iterm2SendScript for the full argument.
func (a iterm2SendAdapter) SendText(entry sessions.SessionEntry, text string) error {
	guid, tty, err := iterm2SendTarget(entry)
	if err != nil {
		return err
	}
	return iterm2SendVerdict(iterm2SendTextArgv(guid, tty, text))
}

// iterm2SendTarget validates and returns the two identifiers a send needs,
// deriving each EXACTLY ONCE so the value that was checked is the value that is
// used — deriving separately per site is precisely how a whitelist and its
// consumer drift apart (focus.go's Raise documents the same hazard).
//
// Refusal is total and happens BEFORE any exec: an empty WindowID, an empty
// TTY, or either failing its whitelist all return ErrNoSendSurface with zero
// subprocesses spawned. Refusing beats sanitizing — the worst case is that
// Tier 1h declines and the loop degrades to an already-shipped tier.
func iterm2SendTarget(entry sessions.SessionEntry) (guid, tty string, err error) {
	guid = iterm2SessionGUID(entry.WindowID)
	if !itermGUIDPattern.MatchString(guid) {
		return "", "", ErrNoSendSurface
	}
	tty = normalizeTTY(entry.TTY)
	if !itermTTYPattern.MatchString(tty) {
		return "", "", ErrNoSendSurface
	}
	return guid, tty, nil
}

// iterm2SendVerdict execs argv and maps the script's stdout verdict to an
// error. The default case is the important one: ANY unrecognized output —
// silence, whitespace, a garbled string — is treated as a failure, never as
// success. This is the false-success guard the repo's attach P0 exists to
// enforce, and it must fail closed on outputs nobody anticipated.
func iterm2SendVerdict(argv []string) error {
	out, err := iterm2SendFn(argv)
	if err != nil {
		// A real exec failure: a revoked Automation grant (-1743), iTerm2 not
		// running, or the actuationTimeout deadline. Surfaced as-is, never
		// swallowed — deciding it is non-fatal is the caller's job.
		return err
	}
	switch strings.TrimSpace(out) {
	case iterm2SendHit:
		return nil
	case iterm2SendTTYMismatch:
		return ErrSendTTYMismatch
	default:
		return ErrSendNoSession
	}
}

// iterm2SendTextArgv builds the argv for a text send. Layout:
//
//	osascript -e <script> -- /dev/<tty> <text>
//
// item 1 of argv is the expected tty, item 2 is the payload. Both are DATA and
// both travel in argument slots. Go's os/exec calls execve directly — there is
// no shell, so there is no second quoting layer to get wrong either.
func iterm2SendTextArgv(guid, tty, text string) []string {
	return append(iterm2SendArgvPrefix(guid, iterm2WriteTextStmt, tty), text)
}

// iterm2WriteTextStmt types argv item 2 and submits it. `newline yes` is
// iTerm2's own submit: measured at the byte level it delivers CR (0x0d), the
// same byte a physical Enter sends (design §8b/E2 — a canonical-mode capture
// reports LF because the tty line discipline's icrnl rewrites it, so any
// re-verification must use `stty raw`).
const iterm2WriteTextStmt = "write aSession text (item 2 of argv) newline yes"

// iterm2SendArgvPrefix assembles everything but the trailing payload:
// the fixed script (parameterized ONLY by class-A values — see below) plus the
// "--" end-of-options marker and the expected tty as the first argument.
func iterm2SendArgvPrefix(guid, writeStmt, tty string) []string {
	return []string{"osascript", "-e", iterm2SendScript(guid, writeStmt), "--", "/dev/" + tty}
}

// iterm2SendScript builds the FIXED osascript source for a send.
//
// # INJECTION SAFETY — the load-bearing invariant of this whole file
//
// No untrusted value is ever concatenated into AppleScript source. Untrusted
// values cross the boundary only as osascript argv. AppleScript reaches
// `do shell script`, so a payload able to close a string literal and append
// statements is arbitrary local code execution; a prior review found exactly
// that Critical defect on this surface.
//
// Every value here is therefore classified as exactly one of:
//
//   - class A, script-shaped — may be interpolated. That is ONLY: guid (gated
//     on itermGUIDPattern by iterm2SendTarget before this is ever called),
//     writeStmt (a compile-time constant from this package's own source, never
//     derived from input), and the verdict literals.
//   - class B, data-shaped — argv ONLY. That is the expected tty and the
//     prompt text. They are handed to osascript as process arguments, arrive as
//     already-typed AppleScript strings in `argv`, and are never tokenized,
//     compiled, or re-parsed. Same structural argument as SQL bind parameters:
//     the payload travels in a parameter slot, not a syntax slot.
//
// The consequence, and what iterm2_send_test.go asserts structurally: the
// script text is BYTE-IDENTICAL for every payload. If that assertion ever
// fails, someone has reintroduced interpolation.
//
// The script neither selects nor activates anything — it must not raise the
// window. Delivery to a background, non-frontmost iTerm2 window is verified
// (design §8b/E1) and is the property that makes this the right shape for
// background fleet actuation.
//
// It walks windows → tabs → sessions read-only. It never mutates the
// collection it iterates (closing a session mid-iteration invalidates the
// index), and it returns from inside the loop on the first GUID match.
func iterm2SendScript(guid, writeStmt string) string {
	return "on run argv\n" +
		"\ttell application \"iTerm2\"\n" +
		"\t\trepeat with aWindow in windows\n" +
		"\t\t\trepeat with aTab in tabs of aWindow\n" +
		"\t\t\t\trepeat with aSession in sessions of aTab\n" +
		"\t\t\t\t\tif id of aSession is \"" + guid + "\" then\n" +
		// The binding check, INSIDE the same round trip: iTerm2's session
		// exposes a read-only tty, so the GUID→tty binding is verified against
		// the registry's recorded tty before a single byte is written. This has
		// no multiplexer analogue and it is what closes the tmux-inside-iTerm2
		// case: if claude's controlling tty is a tmux pane's pty rather than
		// this iTerm2 session's, the ttys differ and the script REFUSES instead
		// of typing into whichever pane happens to be active. It also closes
		// tab recycling (a reused GUID with a different tty).
		"\t\t\t\t\t\tif tty of aSession is not (item 1 of argv) then\n" +
		"\t\t\t\t\t\t\treturn \"" + iterm2SendTTYMismatch + "\"\n" +
		"\t\t\t\t\t\tend if\n" +
		"\t\t\t\t\t\t" + writeStmt + "\n" +
		"\t\t\t\t\t\treturn \"" + iterm2SendHit + "\"\n" +
		"\t\t\t\t\tend if\n" +
		"\t\t\t\tend repeat\n" +
		"\t\t\tend repeat\n" +
		"\t\tend repeat\n" +
		"\tend tell\n" +
		"\treturn \"" + iterm2SendMiss + "\"\n" +
		"end run"
}

// Compile-time assurance the adapter and its binding satisfy their interfaces.
var (
	_ SendAdapter = iterm2SendAdapter{}
	_ Actuator    = boundSendAdapter{}
)
