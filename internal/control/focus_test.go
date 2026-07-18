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
// builder: given a $ITERM_SESSION_ID it emits `osascript -e <script>` whose
// script matches on the session GUID (the part after the colon) and selects
// the enclosing window/tab/session.
func TestITerm2FocusArgv_BuildsSelectScriptForGUID(t *testing.T) {
	argv := iterm2FocusArgv("w0t1p0:ABC-123-GUID")

	if len(argv) != 3 || argv[0] != "osascript" || argv[1] != "-e" {
		t.Fatalf("argv = %v, want [osascript -e <script>]", argv)
	}
	script := argv[2]
	// Matches on the GUID, not the whole window id (the colon-prefixed
	// w/t/p coordinates are NOT the scriptable session id).
	if !strings.Contains(script, `id of aSession is "ABC-123-GUID"`) {
		t.Errorf("script does not match on the session GUID:\n%s", script)
	}
	if strings.Contains(script, "w0t1p0") {
		t.Errorf("script leaked the w/t/p prefix into the id match:\n%s", script)
	}
	for _, want := range []string{`tell application "iTerm2"`, "activate", "select aWindow", "select aTab", "select aSession"} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
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
func withFakeITerm2Focus(t *testing.T, fn func(argv []string) error) *[]string {
	t.Helper()
	var captured []string
	orig := iterm2FocusFn
	t.Cleanup(func() { iterm2FocusFn = orig })
	iterm2FocusFn = func(argv []string) error {
		captured = argv
		return fn(argv)
	}
	return &captured
}

// TestITerm2Raise_WithWindowID_ExecsBuiltScript confirms Raise dispatches the
// pure builder's argv to the exec seam (not a real osascript).
func TestITerm2Raise_WithWindowID_ExecsBuiltScript(t *testing.T) {
	captured := withFakeITerm2Focus(t, func([]string) error { return nil })

	err := iterm2FocusAdapter{}.Raise(sessions.SessionEntry{HostApp: iterm2HostApp, WindowID: "w0t0p0:GUID-9"})
	if err != nil {
		t.Fatalf("Raise = %v, want nil", err)
	}
	if want := iterm2FocusArgv("w0t0p0:GUID-9"); !equalArgv(*captured, want) {
		t.Errorf("execed argv = %v, want %v", *captured, want)
	}
}

// TestITerm2Raise_EmptyWindowID_DegradesNoExec is the graceful-degrade pin:
// an empty WindowID must NOT exec anything and must return ErrNoFocusSurface so
// attach falls through rather than hard-failing.
func TestITerm2Raise_EmptyWindowID_DegradesNoExec(t *testing.T) {
	captured := withFakeITerm2Focus(t, func([]string) error {
		t.Fatal("iterm2FocusFn must not be called for an empty WindowID")
		return nil
	})

	err := iterm2FocusAdapter{}.Raise(sessions.SessionEntry{HostApp: iterm2HostApp})
	if !errors.Is(err, ErrNoFocusSurface) {
		t.Errorf("Raise(empty WindowID) = %v, want ErrNoFocusSurface", err)
	}
	if *captured != nil {
		t.Errorf("execed %v, want no exec", *captured)
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
