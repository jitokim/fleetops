package control

import (
	"context"
	"errors"
	"fmt"
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
// the entry carried no usable window id, or its window id / tty failed its
// whitelist. An entry with no recorded tty AT ALL is a different fact with a
// different operator fix and gets ErrSendNoRecordedTTY instead (see there).
// Like ErrNoFocusSurface this is a fail-closed degrade signal, not a report
// that the write was attempted and lost.
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

// ErrSendUnrecognizedVerdict reports that the host ran the script, exited 0,
// and returned something this package does not recognize — not "ok", not
// "miss", not "ttymismatch". It fails closed exactly like a miss, but it is a
// DIFFERENT fact and gets its own message: folding it into ErrSendNoSession
// told the operator "the tab was closed", which is a claim nothing here
// supports and which sends them looking at the wrong thing. What it actually
// indicates is a broken script/host contract (an iTerm2 version whose
// AppleScript dialect changed, an osascript that printed a warning on stdout),
// and that is worth diagnosing rather than mistaking for a closed tab.
var ErrSendUnrecognizedVerdict = errors.New("control: the host returned an unrecognized verdict — the send did NOT happen; iTerm2/osascript may have changed")

// ErrSendNoRecordedTTY reports that the registry entry has no tty at all
// ("tty":"" — observed in the wild for sessions started without a controlling
// terminal). Tier 1h's whole safety argument is comparing the host's reported
// tty against a recorded one, so with nothing to compare there is nothing to
// verify and the send is REFUSED before any exec. Its own sentinel rather than
// a generic refusal: an absent record and a moved tab need different words, and
// the operator's fix is different (restart the loop so the hook records a tty,
// versus re-attach).
var ErrSendNoRecordedTTY = errors.New("control: this loop has no recorded tty — nothing to verify the host session against; attach (↵) and act manually")

// ErrSendDeliveryUnknown reports that osascript was KILLED at the
// actuationTimeout deadline, so whether the `write` reached the pty is
// genuinely unknowable from here.
//
// It is deliberately neither a success nor a plain failure, because this repo's
// rule is that fleetops never claims more than it knows. Every other exec
// failure — a denied Automation grant (-1743), iTerm2 not running, a launch
// failure — happens BEFORE the script can write, so those provably delivered
// nothing and are safe to retry on another tier. A deadline kill is the one
// case that can land mid-`write`: the process was making progress and we
// stopped it. Retrying that on Tier 2 would risk delivering the same prompt
// TWICE (a duplicate injection, a duplicate re-send — wasted turns and a
// transcript that reads as if the operator pressed the key twice), and
// reporting it as a clean failure would be the lie that makes the retry
// automatic. So it fails LOUD instead: the operator is told the outcome is
// unknown and to go look before pressing anything again.
var ErrSendDeliveryUnknown = errors.New("control: the host send timed out — delivery is UNKNOWN, it may or may not have landed; attach (↵) and check before retrying")

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
	return string(out), classifySendExecError(ctx.Err(), err)
}

// classifySendExecError separates the ONE exec failure whose delivery is
// unknowable from all the ones that provably delivered nothing.
//
// It reads ctx.Err() rather than inspecting err, because CommandContext reports
// a deadline kill as an ordinary *exec.ExitError ("signal: killed") — from the
// error alone a timeout is indistinguishable from a script that exited on its
// own. The context is the only witness to WHY the process died.
//
// A pure function taking the context's error rather than the context itself, so
// the classification is directly unit-testable without racing a real 5s
// deadline (same seam discipline as withCommandStderr, which still handles the
// provably-undelivered side).
func classifySendExecError(ctxErr, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(ctxErr, context.DeadlineExceeded) {
		return fmt.Errorf("%w (%v)", ErrSendDeliveryUnknown, err)
	}
	return withCommandStderr(err)
}

// withCommandStderr folds a failed command's stderr into its error.
//
// exec.ExitError stringifies to bare "exit status 1" while carrying the only
// text that says WHY in an ignored field. On this path that text is the whole
// diagnosis: a denied Automation grant reports
// "Not authorized to send Apple events to iTerm2. (-1743)" on stderr, and
// dropping it left the operator with "exit status 1" and no way to learn they
// need to re-grant in System Settings → Privacy → Automation. (.Output()
// populates Stderr precisely so this is possible; design §5.3's honest-failure
// claim depends on it.)
//
// Non-ExitError failures (LookPath and friends) already carry their own text
// and pass through untouched. The deadline case never reaches here — see
// classifySendExecError.
func withCommandStderr(err error) error {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return err
	}
	stderr := strings.TrimSpace(string(exitErr.Stderr))
	if stderr == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, stderr)
}

// The verdicts the osascript returns on stdout. osascript exits 0 whether it
// found the session, refused on a tty mismatch, or matched nothing at all, so
// the verdict is the ONLY thing that can distinguish them — exit status cannot.
const (
	iterm2SendHit         = "ok"
	iterm2SendMiss        = "miss"
	iterm2SendTTYMismatch = "ttymismatch"
)

// sessionGUID is a session id that HAS BEEN CHECKED against itermGUIDPattern.
//
// It exists because the GUID is the one untrusted value this file interpolates
// into AppleScript source, and on a surface with a prior Critical injection
// finding "the caller is supposed to validate first" is not a control — it is a
// convention, and conventions are what the next refactor deletes. A plain
// `guid string` parameter meant iterm2SendScript / iterm2SendArgvPrefix /
// iterm2SendTextArgv would each happily bake an arbitrary caller-supplied
// string into script text; the type makes that unrepresentable, because
// newSessionGUID is the only way to obtain a value of it and newSessionGUID
// cannot return one that failed the pattern.
//
// The zero value carries "" — the one other inhabitant, and injection-inert by
// construction. So the type's invariant reads: every sessionGUID is either
// empty or pattern-clean, and neither can close a string literal.
type sessionGUID struct{ checked string }

// newSessionGUID extracts the GUID from an untrusted registry WindowID and
// validates it. ok=false means REFUSE — never sanitize, never interpolate.
// Deriving and checking in the same place is the point: it is not possible to
// hold a sessionGUID whose value was not the value that passed the whitelist.
func newSessionGUID(windowID string) (sessionGUID, bool) {
	guid := iterm2SessionGUID(windowID)
	if !itermGUIDPattern.MatchString(guid) {
		return sessionGUID{}, false
	}
	return sessionGUID{checked: guid}, true
}

// String returns the checked GUID — the ONLY way to read one, and the only
// value iterm2SendScript is allowed to interpolate.
func (g sessionGUID) String() string { return g.checked }

// itermTTYPattern is the WHITELIST the registry-recorded tty must match before
// it may be used to address a session: bare device names only ("ttys006"), as
// normalizeTTY produces.
//
// LOWERCASE only, and that is a correctness constraint rather than tidiness.
// The script compares `tty of aSession` against this value with AppleScript's
// `is`, which is case-INSENSITIVE by default — so a pattern admitting [A-Za-z]
// would let a recorded "TTYS006" satisfy the binding guard against a real
// "ttys006". Restricting the pattern is the smaller of the two available fixes
// (the other being `considering case` in the script) and it cannot cause a
// false REFUSAL: macOS device names are lowercase, so nothing legitimate is
// excluded. With only one case admissible, a case-insensitive comparison and a
// case-sensitive one agree on every value that can reach it.
//
// The tty is passed to osascript as ARGV, not interpolated (see
// iterm2SendScript), so this whitelist is defense in depth rather than the
// primary control — but it is not decoration. TTY reaches us from the same
// untrusted ~/.fleetops/sessions/<id>.json that carries WindowID, so it gets
// the same treatment on principle: a value that cannot be a device name cannot
// address a real session either, and refusing early beats sending a
// nonsense comparison to the host.
var itermTTYPattern = regexp.MustCompile(`^[a-z0-9]+$`)

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
// Refusal is total and happens BEFORE any exec, with zero subprocesses
// spawned: an empty WindowID or either value failing its whitelist returns
// ErrNoSendSurface, and an empty TTY returns ErrSendNoRecordedTTY (a different
// fact, see there). Refusing beats sanitizing — the worst case is that Tier 1h
// declines and the loop degrades to an already-shipped tier.
func iterm2SendTarget(entry sessions.SessionEntry) (guid sessionGUID, tty string, err error) {
	guid, ok := newSessionGUID(entry.WindowID)
	if !ok {
		return sessionGUID{}, "", ErrNoSendSurface
	}
	tty = normalizeTTY(entry.TTY)
	// An EMPTY recorded tty is its own case, checked before the whitelist,
	// because it is a different fact with a different fix. Entries with
	// "tty":"" exist in the wild (a session with no controlling terminal at
	// SessionStart time), and they are not a malformed/hostile value — there is
	// simply nothing to bind against. Folded into the generic refusal it would
	// eventually reach the operator as a tty MISMATCH, blaming a moved tab for
	// a record that never had one, forever.
	if tty == "" {
		return sessionGUID{}, "", ErrSendNoRecordedTTY
	}
	if !itermTTYPattern.MatchString(tty) {
		return sessionGUID{}, "", ErrNoSendSurface
	}
	return guid, tty, nil
}

// iterm2SendVerdict execs argv and maps the script's stdout verdict to an
// error. The default case is the important one: ANY unrecognized output —
// silence, whitespace, a garbled string — is treated as a failure, never as
// success. This is the false-success guard the repo's attach P0 exists to
// enforce, and it must fail closed on outputs nobody anticipated.
//
// Failing closed is not a licence to invent a diagnosis, though: unrecognized
// output gets ErrSendUnrecognizedVerdict rather than being reported as a
// closed tab. Every verdict maps to the error that states what was actually
// observed.
func iterm2SendVerdict(argv []string) error {
	out, err := iterm2SendFn(argv)
	if err != nil {
		// A real exec failure: a revoked Automation grant (-1743), iTerm2 not
		// running, or the actuationTimeout deadline (already separated out as
		// ErrSendDeliveryUnknown by classifySendExecError, because that one
		// alone may have delivered). Surfaced as-is, never swallowed —
		// deciding what is safe to do next is the caller's job.
		return err
	}
	switch strings.TrimSpace(out) {
	case iterm2SendHit:
		return nil
	case iterm2SendTTYMismatch:
		return ErrSendTTYMismatch
	case iterm2SendMiss:
		return ErrSendNoSession
	default:
		// Anything else fails closed too — but as its own fact, not as a
		// fabricated "the tab was closed."
		return ErrSendUnrecognizedVerdict
	}
}

// iterm2SendTextArgv builds the argv for a text send. Layout:
//
//	osascript -e <script> -- /dev/<tty> <text>
//
// item 1 of argv is the expected tty, item 2 is the payload. Both are DATA and
// both travel in argument slots. Go's os/exec calls execve directly — there is
// no shell, so there is no second quoting layer to get wrong either.
func iterm2SendTextArgv(guid sessionGUID, tty, text string) []string {
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
func iterm2SendArgvPrefix(guid sessionGUID, writeStmt, tty string) []string {
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
//   - class A, script-shaped — may be interpolated. That is ONLY: guid (a
//     sessionGUID, i.e. a value that CANNOT EXIST unless it passed
//     itermGUIDPattern — the check is in the type, not in a caller's manners),
//     writeStmt (a compile-time constant from this package's own source, never
//     derived from input), and the verdict literals.
//   - class B, data-shaped — argv ONLY. That is the expected tty and the
//     prompt text. They are handed to osascript as process arguments, arrive as
//     already-typed AppleScript strings in `argv`, and are never tokenized,
//     compiled, or re-parsed. Same structural argument as SQL bind parameters:
//     the payload travels in a parameter slot, not a syntax slot.
//
// The consequence, and what TestITerm2SendScript_ByteIdenticalAcrossPayloads
// (hostsend_test.go) asserts structurally: the script text is BYTE-IDENTICAL
// for every payload. If that assertion ever fails, someone has reintroduced
// interpolation.
//
// The script neither selects nor activates anything — it must not raise the
// window. Delivery to a background, non-frontmost iTerm2 window is verified
// (design §8b/E1) and is the property that makes this the right shape for
// background fleet actuation.
//
// It walks windows → tabs → sessions read-only. It never mutates the
// collection it iterates (closing a session mid-iteration invalidates the
// index), and it returns from inside the loop on the first GUID match.
func iterm2SendScript(guid sessionGUID, writeStmt string) string {
	return "on run argv\n" +
		"\ttell application \"iTerm2\"\n" +
		"\t\trepeat with aWindow in windows\n" +
		"\t\t\trepeat with aTab in tabs of aWindow\n" +
		"\t\t\t\trepeat with aSession in sessions of aTab\n" +
		"\t\t\t\t\tif id of aSession is \"" + guid.String() + "\" then\n" +
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
