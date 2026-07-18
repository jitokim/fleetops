package control

import (
	"errors"
	"strings"
	"testing"

	"github.com/jitokim/fleetops/internal/sessions"
)

func TestResolveFocusAdapter_ITerm2_Resolves(t *testing.T) {
	adapter, ok := ResolveFocusAdapter(iterm2HostApp)
	if !ok {
		t.Fatalf("ResolveFocusAdapter(%q) ok=false, want an adapter", iterm2HostApp)
	}
	if _, isITerm := adapter.(iterm2FocusAdapter); !isITerm {
		t.Errorf("adapter = %T, want iterm2FocusAdapter", adapter)
	}
}

func TestResolveFocusAdapter_UnknownHostApp_Degrades(t *testing.T) {
	if adapter, ok := ResolveFocusAdapter("Apple_Terminal"); ok || adapter != nil {
		t.Errorf("unknown host_app resolved to (%v,%v), want (nil,false) to degrade", adapter, ok)
	}
}

func TestResolveFocusAdapter_EmptyHostApp_Degrades(t *testing.T) {
	if adapter, ok := ResolveFocusAdapter(""); ok || adapter != nil {
		t.Errorf("empty host_app resolved to (%v,%v), want (nil,false)", adapter, ok)
	}
}

// TestITerm2FocusArgv_BuildsSelectScriptForGUID pins the pure osascript
// builder: given a session GUID it emits `osascript -e <script>` whose script
// matches on that GUID and selects the enclosing window/tab/session. The
// builder takes an already-extracted GUID — extraction itself is pinned by
// TestITerm2SessionGUID, and that Raise extracts before building is pinned by
// TestITerm2Raise_WithWindowID_ExecsBuiltScript.
func TestITerm2FocusArgv_BuildsSelectScriptForGUID(t *testing.T) {
	argv := iterm2FocusArgv("ABC-123-GUID")

	if len(argv) != 3 || argv[0] != "osascript" || argv[1] != "-e" {
		t.Fatalf("argv = %v, want [osascript -e <script>]", argv)
	}
	script := argv[2]
	if !strings.Contains(script, `id of aSession is "ABC-123-GUID"`) {
		t.Errorf("script does not match on the session GUID:\n%s", script)
	}
	for _, want := range []string{`tell application "iTerm2"`, "activate", "select aWindow", "select aTab", "select aSession"} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}
}

// TestITerm2FocusArgv_ReportsHitAndMiss pins the property that makes Raise
// capable of failing at all: osascript exits 0 either way, so the script must
// say which happened, and must NOT activate iTerm2 before it knows. An
// activate-first script yanks the app forward and returns success for a session
// that no longer exists.
func TestITerm2FocusArgv_ReportsHitAndMiss(t *testing.T) {
	script := iterm2FocusArgv("ABC-123")[2]

	if !strings.Contains(script, `return "`+iterm2FocusHit+`"`) {
		t.Errorf("script never reports a hit:\n%s", script)
	}
	if !strings.Contains(script, `return "`+iterm2FocusMiss+`"`) {
		t.Errorf("script falls off the end without reporting a miss:\n%s", script)
	}
	// activate must come AFTER the match (inside the branch), so a miss never
	// brings iTerm2 forward.
	activateAt := strings.Index(script, "activate")
	matchAt := strings.Index(script, "id of aSession is")
	if activateAt < matchAt {
		t.Errorf("activate runs before the id match — a miss would still raise iTerm2:\n%s", script)
	}
}

// TestITerm2Raise_ScriptMiss_DegradesNoHardFailure is the false-success pin:
// the script ran fine (exit 0) but matched no session, which must surface as
// ErrNoFocusSurface so attach degrades to its multiplexer step instead of
// reporting "attached via iTerm.app" for a closed tab.
func TestITerm2Raise_ScriptMiss_DegradesNoHardFailure(t *testing.T) {
	withFakeITerm2Focus(t, func([]string) (string, error) { return iterm2FocusMiss + "\n", nil })

	err := iterm2FocusAdapter{}.Raise(sessions.SessionEntry{HostApp: iterm2HostApp, WindowID: "w0t0p0:GONE-1"})
	if !errors.Is(err, ErrNoFocusSurface) {
		t.Errorf("Raise on a script miss = %v, want ErrNoFocusSurface", err)
	}
}

// TestITerm2Raise_UnexpectedStdout_Degrades: any verdict that is not exactly
// the hit marker is treated as a miss. Silence (an osascript that printed
// nothing) must not read as success.
func TestITerm2Raise_UnexpectedStdout_Degrades(t *testing.T) {
	for name, stdout := range map[string]string{
		"empty":      "",
		"whitespace": "  \n",
		"garbage":    "something else",
	} {
		t.Run(name, func(t *testing.T) {
			withFakeITerm2Focus(t, func([]string) (string, error) { return stdout, nil })

			err := iterm2FocusAdapter{}.Raise(sessions.SessionEntry{HostApp: iterm2HostApp, WindowID: "w0t0p0:GUID-2"})
			if !errors.Is(err, ErrNoFocusSurface) {
				t.Errorf("Raise with stdout %q = %v, want ErrNoFocusSurface", stdout, err)
			}
		})
	}
}

// TestITerm2Raise_ExecError_Propagates: a non-zero osascript (e.g. macOS
// Automation permission denied) is a REAL error, distinct from a miss. The
// adapter reports it as-is; deciding it is non-fatal is attach's job, not the
// adapter's.
func TestITerm2Raise_ExecError_Propagates(t *testing.T) {
	boom := errors.New("exit status 1: not authorized to send Apple events")
	withFakeITerm2Focus(t, func([]string) (string, error) { return "", boom })

	err := iterm2FocusAdapter{}.Raise(sessions.SessionEntry{HostApp: iterm2HostApp, WindowID: "w0t0p0:GUID-3"})
	if !errors.Is(err, boom) {
		t.Errorf("Raise = %v, want the exec error %v", err, boom)
	}
	if errors.Is(err, ErrNoFocusSurface) {
		t.Error("an exec failure must not masquerade as ErrNoFocusSurface")
	}
}

// TestITerm2Raise_ScriptHit_Succeeds is the one success case.
func TestITerm2Raise_ScriptHit_Succeeds(t *testing.T) {
	withFakeITerm2Focus(t, func([]string) (string, error) { return iterm2FocusHit + "\n", nil })

	err := iterm2FocusAdapter{}.Raise(sessions.SessionEntry{HostApp: iterm2HostApp, WindowID: "w0t0p0:GUID-4"})
	if err != nil {
		t.Errorf("Raise on a script hit = %v, want nil", err)
	}
}

func TestITerm2SessionGUID(t *testing.T) {
	cases := map[string]string{
		"w0t1p0:ABC-123": "ABC-123",   // typical $ITERM_SESSION_ID
		"bare-guid":      "bare-guid", // no colon → passthrough
		"a:b:c":          "c",         // last colon wins
	}
	for in, want := range cases {
		if got := iterm2SessionGUID(in); got != want {
			t.Errorf("iterm2SessionGUID(%q) = %q, want %q", in, got, want)
		}
	}
}

// withFakeITerm2Focus swaps the injectable exec seam for a spy and restores it.
func withFakeITerm2Focus(t *testing.T, fn func(argv []string) (string, error)) *[]string {
	t.Helper()
	var captured []string
	orig := iterm2FocusFn
	t.Cleanup(func() { iterm2FocusFn = orig })
	iterm2FocusFn = func(argv []string) (string, error) {
		captured = argv
		return fn(argv)
	}
	return &captured
}

// TestITerm2Raise_WithWindowID_ExecsBuiltScript confirms Raise dispatches the
// pure builder's argv to the exec seam (not a real osascript).
func TestITerm2Raise_WithWindowID_ExecsBuiltScript(t *testing.T) {
	captured := withFakeITerm2Focus(t, func([]string) (string, error) { return iterm2FocusHit, nil })

	err := iterm2FocusAdapter{}.Raise(sessions.SessionEntry{HostApp: iterm2HostApp, WindowID: "w0t0p0:GUID-9"})
	if err != nil {
		t.Fatalf("Raise = %v, want nil", err)
	}
	// Built from the EXTRACTED GUID, not the raw window id — this is what pins
	// that Raise derives the GUID once and hands that same value to the builder.
	if want := iterm2FocusArgv("GUID-9"); !equalArgv(*captured, want) {
		t.Errorf("execed argv = %v, want %v", *captured, want)
	}
}

// TestITerm2Raise_EmptyWindowID_DegradesNoExec is the graceful-degrade pin:
// an empty WindowID must NOT exec anything and must return ErrNoFocusSurface so
// attach falls through rather than hard-failing.
func TestITerm2Raise_EmptyWindowID_DegradesNoExec(t *testing.T) {
	captured := withFakeITerm2Focus(t, func([]string) (string, error) {
		t.Fatal("iterm2FocusFn must not be called for an empty WindowID")
		return "", nil
	})

	err := iterm2FocusAdapter{}.Raise(sessions.SessionEntry{HostApp: iterm2HostApp})
	if !errors.Is(err, ErrNoFocusSurface) {
		t.Errorf("Raise(empty WindowID) = %v, want ErrNoFocusSurface", err)
	}
	if *captured != nil {
		t.Errorf("execed %v, want no exec", *captured)
	}
}

// TestITerm2Raise_UntrustedWindowID_DegradesNoExec is the injection pin. The
// GUID is interpolated into a double-quoted AppleScript literal, so a WindowID
// carrying a quote (or any AppleScript metacharacter) could close the literal
// and append statements — `do shell script` would then be arbitrary local code
// execution. WindowID comes from $ITERM_SESSION_ID via a world-writable-ish
// registry file, so it is untrusted input: anything failing the GUID whitelist
// must degrade exactly like an empty WindowID (ErrNoFocusSurface, NO exec).
func TestITerm2Raise_UntrustedWindowID_DegradesNoExec(t *testing.T) {
	hostile := map[string]string{
		"closes the quoted literal": `w0t0p0:X" & (do shell script "touch /tmp/pwned") & "`,
		"bare quote":                `w0t0p0:a"b`,
		"embedded newline":          "w0t0p0:a\nactivate",
		"backslash escape":          `w0t0p0:a\"b`,
		"whitespace":                "w0t0p0:a b",
		"empty guid after colon":    "w0t0p0:",
		"ampersand concat":          `w0t0p0:a&b`,
		"paren call":                `w0t0p0:a(b)`,
	}
	for name, windowID := range hostile {
		t.Run(name, func(t *testing.T) {
			captured := withFakeITerm2Focus(t, func([]string) (string, error) {
				t.Fatal("iterm2FocusFn must never be called for an untrusted WindowID")
				return "", nil
			})

			err := iterm2FocusAdapter{}.Raise(sessions.SessionEntry{HostApp: iterm2HostApp, WindowID: windowID})
			if !errors.Is(err, ErrNoFocusSurface) {
				t.Errorf("Raise(%q) = %v, want ErrNoFocusSurface", windowID, err)
			}
			if *captured != nil {
				t.Errorf("execed %v, want no exec for %q", *captured, windowID)
			}
		})
	}
}

// TestITerm2GUIDPattern_AcceptsRealGUIDs guards against a whitelist so strict
// it rejects the ids iTerm2 actually emits (which would silently disable focus).
func TestITerm2GUIDPattern_AcceptsRealGUIDs(t *testing.T) {
	for _, guid := range []string{
		"ABC-123-GUID",
		"A0B1C2D3-4E5F-6789-ABCD-EF0123456789",
		"bareguid",
		"0",
	} {
		if !itermGUIDPattern.MatchString(guid) {
			t.Errorf("itermGUIDPattern rejected a legitimate session GUID %q", guid)
		}
	}
}

func equalArgv(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
