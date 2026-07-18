package control

import (
	"context"
	"errors"
	"os/exec"
	"regexp"
	"strings"

	"github.com/jitokim/fleetops/internal/sessions"
)

// FocusAdapter raises the on-screen surface hosting a session to the front —
// the attach primitive, resolved from the registry entry's HostApp
// ($TERM_PROGRAM). Same optional-capability idiom as TTYLocator/TerminalOpener
// (a narrow, focus-only seam, not a Controller method): a host terminal that
// knows how to reveal one of its windows/tabs implements this; nothing else
// changes. Raise takes the whole SessionEntry because different hosts key on
// different fields (iTerm2 on WindowID/$ITERM_SESSION_ID); returning an error
// lets attach report a real failure, while ErrNoFocusSurface signals "nothing
// to raise — degrade to the manual hint" rather than a hard failure.
//
// Multiplexer focus (orca/cmux/tmux) is the OTHER adapter family: it keys on
// the located surface, not on a host window id, so it stays behind
// ResolveForLocate+Focus (attach's step 2, unchanged). Keeping the
// multiplexers OUT of the host_app registry below is what makes adding iTerm2
// a pure superset — a recognized multiplexer host_app never diverts a loop off
// today's proven ResolveForLocate path. See attachCmd's 3-step resolution.
type FocusAdapter interface {
	Raise(entry sessions.SessionEntry) error
}

// ErrNoFocusSurface reports that an adapter had nothing to raise (e.g. an empty
// WindowID) — a graceful-degrade signal, NOT a real failure. Callers treat it
// like "no backend available" and fall through to the manual attach hint,
// exactly as attach does when no multiplexer can locate a surface.
var ErrNoFocusSurface = errors.New("control: no focus surface for this session")

// iterm2HostApp is the $TERM_PROGRAM value iTerm2 exports. It keys iTerm2's
// FocusAdapter in hostAppFocusAdapters below.
const iterm2HostApp = "iTerm.app"

// hostAppFocusAdapters maps a $TERM_PROGRAM marker to its (non-multiplexer)
// FocusAdapter. Keyed by host_app, as design §4 specifies. Only genuinely new,
// window-id-addressable hosts belong here; multiplexers deliberately do not
// (see FocusAdapter's own doc for why that keeps attach a pure superset).
var hostAppFocusAdapters = map[string]FocusAdapter{
	iterm2HostApp: iterm2FocusAdapter{},
}

// ResolveFocusAdapter returns the FocusAdapter registered for hostApp, or
// (nil, false) when none is — an unknown or empty host_app degrades gracefully
// (attach falls through to its cwd-based resolver, then the manual hint).
func ResolveFocusAdapter(hostApp string) (FocusAdapter, bool) {
	if hostApp == "" {
		return nil, false
	}
	adapter, ok := hostAppFocusAdapters[hostApp]
	return adapter, ok
}

// iterm2FocusFn runs the osascript that raises an iTerm2 session. An injectable
// package var (same seam discipline as pidTTYFn/redriveFn) so the adapter is
// unit-testable against a fake without a real iTerm2 or osascript on the
// machine — the whole reason iterm2FocusArgv is pulled out as a pure function
// too. Bounded by actuationTimeout so a wedged osascript never hangs the caller.
var iterm2FocusFn = func(argv []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), actuationTimeout)
	defer cancel()
	return exec.CommandContext(ctx, argv[0], argv[1:]...).Run()
}

// iterm2FocusAdapter raises the iTerm2 window/tab/session identified by the
// entry's WindowID ($ITERM_SESSION_ID). Focus-only, no Controller surface —
// the first non-multiplexer FocusAdapter (design §4).
type iterm2FocusAdapter struct{}

// itermGUIDPattern is the WHITELIST every session GUID must match before it may
// be interpolated into the osascript below: ASCII alphanumerics and hyphens
// only, which is exactly the shape iTerm2 emits (a UUID). A whitelist, not
// escaping — AppleScript string quoting is its own DSL with its own escape
// rules, and getting it subtly wrong on untrusted input is how `do shell script`
// turns a focus keypress into arbitrary local code execution. WindowID reaches
// us from $ITERM_SESSION_ID via ~/.fleetops/sessions/<id>.json, so anything able
// to write that file or set that env var controls this string: it is untrusted.
var itermGUIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

// Raise selects the iTerm2 session named by entry.WindowID. Two degrade paths,
// both ErrNoFocusSurface with NO exec so attach falls through to its next step:
// an empty WindowID (nothing to raise), and a GUID that fails
// itermGUIDPattern — we never build a script out of an untrusted id, and never
// exec one. Refusing beats sanitizing: the worst case is a degraded attach.
func (iterm2FocusAdapter) Raise(entry sessions.SessionEntry) error {
	if entry.WindowID == "" {
		return ErrNoFocusSurface
	}
	if !itermGUIDPattern.MatchString(iterm2SessionGUID(entry.WindowID)) {
		return ErrNoFocusSurface
	}
	return iterm2FocusFn(iterm2FocusArgv(entry.WindowID))
}

// iterm2FocusArgv builds the osascript argv that reveals the iTerm2 session
// whose AppleScript `id` matches windowID's session GUID — pulled out as a pure
// function so the exact script is directly unit-testable (same pattern as
// redriveArgv/orcaResumeCmd). $ITERM_SESSION_ID is shaped "w<W>t<T>p<P>:<GUID>";
// the session's scriptable `id` is that trailing GUID, so we match on it and
// select the enclosing window + tab + session, then activate iTerm2 to bring it
// forward. Best-effort by design: if nothing matches (the session was closed),
// the script simply activates iTerm2 and returns — attach never hard-fails on a
// focus miss.
func iterm2FocusArgv(windowID string) []string {
	guid := iterm2SessionGUID(windowID)
	script := "tell application \"iTerm2\"\n" +
		"\tactivate\n" +
		"\trepeat with aWindow in windows\n" +
		"\t\trepeat with aTab in tabs of aWindow\n" +
		"\t\t\trepeat with aSession in sessions of aTab\n" +
		"\t\t\t\tif id of aSession is \"" + guid + "\" then\n" +
		"\t\t\t\t\tselect aWindow\n" +
		"\t\t\t\t\tselect aTab\n" +
		"\t\t\t\t\tselect aSession\n" +
		"\t\t\t\t\treturn\n" +
		"\t\t\t\tend if\n" +
		"\t\t\tend repeat\n" +
		"\t\tend repeat\n" +
		"\tend repeat\n" +
		"end tell"
	return []string{"osascript", "-e", script}
}

// iterm2SessionGUID extracts the session GUID from a $ITERM_SESSION_ID value:
// the portion after the last ':' ("w0t1p0:ABC-123" → "ABC-123"), which is what
// iTerm2's AppleScript exposes as a session's `id`. A value with no ':' is
// returned unchanged (already a bare GUID, or an unexpected shape we pass
// through rather than mangle).
func iterm2SessionGUID(windowID string) string {
	if idx := strings.LastIndex(windowID, ":"); idx >= 0 {
		return windowID[idx+1:]
	}
	return windowID
}

// Compile-time assurance the adapter satisfies the interface.
var _ FocusAdapter = iterm2FocusAdapter{}
