package main

import (
	"os"
	"strings"
	"testing"

	"github.com/jitokim/fleetops/internal/sessions"
)

// withStdin swaps os.Stdin for a pipe carrying input, runs fn, then restores
// it — lets us drive the hook handlers (which read os.Stdin) from a test.
func withStdin(t *testing.T, input string, fn func()) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdin
	os.Stdin = r
	defer func() {
		os.Stdin = orig
		_ = r.Close()
	}()
	go func() {
		_, _ = w.Write([]byte(input))
		_ = w.Close()
	}()
	fn()
}

func TestSessionStartHook_WritesEntry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	payload := `{"session_id":"abc-123","cwd":"/tmp/proj","transcript_path":"/tmp/t.jsonl","source":"startup","hook_event_name":"SessionStart"}`

	withStdin(t, payload, sessionStartHook)

	entry, err := sessions.ReadSession(sessions.SessionsDir(), "abc-123")
	if err != nil {
		t.Fatalf("expected an entry to be written: %v", err)
	}
	if entry.Cwd != "/tmp/proj" {
		t.Errorf("Cwd = %q, want /tmp/proj", entry.Cwd)
	}
	if entry.TranscriptPath != "/tmp/t.jsonl" {
		t.Errorf("TranscriptPath = %q, want /tmp/t.jsonl", entry.TranscriptPath)
	}
	if entry.Source != "startup" {
		t.Errorf("Source = %q, want startup", entry.Source)
	}
	// PID falls back to os.Getppid() (the test runner's parent) when no
	// claude ancestor exists — must be a real, positive pid, never 0.
	if entry.PID <= 0 {
		t.Errorf("PID = %d, want > 0", entry.PID)
	}
	if entry.StartedAt.IsZero() {
		t.Error("StartedAt is zero, want set")
	}
}

// TestSessionStartHook_PopulatesHostWindowFromEnv confirms the hook records
// $TERM_PROGRAM as HostApp and that host's OWN window id as WindowID.
func TestSessionStartHook_PopulatesHostWindowFromEnv(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TERM_PROGRAM", "iTerm.app")
	t.Setenv("ITERM_SESSION_ID", "w0t1p0:ABC-123")
	t.Setenv("TMUX_PANE", "%9") // ignored: the host is iTerm2, so its id wins

	withStdin(t, `{"session_id":"host-win","cwd":"/tmp/proj","source":"startup"}`, sessionStartHook)

	entry, err := sessions.ReadSession(sessions.SessionsDir(), "host-win")
	if err != nil {
		t.Fatalf("expected an entry: %v", err)
	}
	if entry.HostApp != "iTerm.app" {
		t.Errorf("HostApp = %q, want iTerm.app", entry.HostApp)
	}
	if entry.WindowID != "w0t1p0:ABC-123" {
		t.Errorf("WindowID = %q, want the iTerm2 host's own marker ($ITERM_SESSION_ID)", entry.WindowID)
	}
	if entry.SocketPath != "" {
		t.Errorf("SocketPath = %q, want empty (out of scope for this slice)", entry.SocketPath)
	}
}

// TestResolveHostWindow_NestedTmuxInITerm2_NoForeignWindowID is the
// mismatched-pair pin and the common nested case: claude runs in tmux inside
// iTerm2, so tmux sets $TERM_PROGRAM=tmux while the OUTER iTerm2's
// $ITERM_SESSION_ID is still inherited in the environment. Resolving the two
// fields independently records HostApp=tmux with an iTerm2 window id — one
// record describing two different terminals. WindowID must come from the
// resolved host, never from whichever marker happens to be set.
func TestResolveHostWindow_NestedTmuxInITerm2_NoForeignWindowID(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "tmux")
	t.Setenv("ITERM_SESSION_ID", "w0t1p0:OUTER-ITERM-GUID") // leaked from the outer iTerm2
	t.Setenv("TMUX_PANE", "%9")

	hostApp, windowID := resolveHostWindow()

	if hostApp != "tmux" {
		t.Errorf("HostApp = %q, want tmux", hostApp)
	}
	if strings.Contains(windowID, "ITERM") {
		t.Fatalf("WindowID = %q — a tmux-hosted session must never carry the outer iTerm2 window id", windowID)
	}
	if windowID != "%9" {
		t.Errorf("WindowID = %q, want tmux's own $TMUX_PANE", windowID)
	}
}

// TestResolveHostWindow_UnknownHost_KeepsHostDropsWindowID: an unrecognized
// $TERM_PROGRAM is still a true fact worth recording, but we don't know which
// env var carries ITS window id — so the host name is kept and the window id is
// dropped rather than borrowing another terminal's id.
func TestResolveHostWindow_UnknownHost_KeepsHostDropsWindowID(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "Apple_Terminal")
	t.Setenv("ITERM_SESSION_ID", "w0t1p0:NOT-OURS")
	t.Setenv("TMUX_PANE", "%9")

	hostApp, windowID := resolveHostWindow()
	if hostApp != "Apple_Terminal" {
		t.Errorf("hostApp = %q, want the real $TERM_PROGRAM %q to be preserved", hostApp, "Apple_Terminal")
	}
	if windowID != "" {
		t.Errorf("windowID = %q, want empty — an unrecognized host must not borrow another terminal's id", windowID)
	}
}

// TestResolveHostWindow_HostWithoutItsOwnMarker_EmptyWindowID: iTerm2 without
// $ITERM_SESSION_ID must NOT fall back to $TMUX_PANE.
func TestResolveHostWindow_HostWithoutItsOwnMarker_EmptyWindowID(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "iTerm.app")
	t.Setenv("ITERM_SESSION_ID", "")
	t.Setenv("TMUX_PANE", "%9")

	hostApp, windowID := resolveHostWindow()

	if hostApp != "iTerm.app" {
		t.Errorf("HostApp = %q, want iTerm.app", hostApp)
	}
	if windowID != "" {
		t.Errorf("WindowID = %q, want empty — $TMUX_PANE is not iTerm2's window id", windowID)
	}
}

// TestSessionStartHook_HostWindowEnvAbsent_StillWritesEntry confirms the hook
// still succeeds (exits 0, writes the entry) when NO host/window env is set —
// HostApp/WindowID simply stay empty and attach later degrades to the hint.
func TestSessionStartHook_HostWindowEnvAbsent_StillWritesEntry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// Force-clear inherited terminal env (the test runner itself may be under
	// iTerm2/tmux) so "absent" is actually absent.
	t.Setenv("TERM_PROGRAM", "")
	t.Setenv("ITERM_SESSION_ID", "")
	t.Setenv("TMUX_PANE", "")

	withStdin(t, `{"session_id":"no-host","cwd":"/tmp/proj","source":"startup"}`, sessionStartHook)

	entry, err := sessions.ReadSession(sessions.SessionsDir(), "no-host")
	if err != nil {
		t.Fatalf("expected an entry even with no host env: %v", err)
	}
	if entry.HostApp != "" || entry.WindowID != "" {
		t.Errorf("want empty host/window with no env, got HostApp=%q WindowID=%q", entry.HostApp, entry.WindowID)
	}
}

func TestSessionStartHook_MissingSessionID_NoWrite(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	withStdin(t, `{"cwd":"/tmp/proj","source":"startup"}`, sessionStartHook)

	if got := sessions.ListSessions(sessions.SessionsDir()); len(got) != 0 {
		t.Errorf("wrote %d entries despite missing session_id, want 0: %+v", len(got), got)
	}
}

func TestSessionEndHook_DeletesEntry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := sessions.SessionsDir()
	if err := sessions.WriteSession(dir, "to-del", sessions.SessionEntry{PID: 1}); err != nil {
		t.Fatal(err)
	}

	withStdin(t, `{"session_id":"to-del"}`, sessionEndHook)

	if _, err := sessions.ReadSession(dir, "to-del"); err == nil {
		t.Error("entry still present after session-end hook, want deleted")
	}
}

// TestSessionEndHook_UnknownSession_NoError exercises the SIGKILL-leak path:
// SessionEnd firing for a session with no registry entry must be a quiet
// no-op (the handler swallows DeleteSession's nil and returns normally).
func TestSessionEndHook_UnknownSession_NoError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// no panic, no crash — that's the whole assertion.
	withStdin(t, `{"session_id":"never-started"}`, sessionEndHook)
}

func TestRunHookCmd_UnknownAndEmpty_NoOp(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// unknown/empty subcommands return before touching stdin — must not panic
	// and must not exit non-zero (they just return).
	runHookCmd(nil)
	runHookCmd([]string{})
	runHookCmd([]string{"bogus-subcommand"})
}

// TestHookHandlers_NeverPanicOnGarbage is the safety-critical property: no
// stdin, however malformed, may crash a hook handler — a panic here would be
// able to break the user's real claude session. Every handler is fed the
// same table of garbage and must simply return.
func TestHookHandlers_NeverPanicOnGarbage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	inputs := []string{
		"",
		"   ",
		"\x00\x01\x02\xff",
		"not json at all",
		"{",
		"}",
		"[]",
		"null",
		"true",
		"12345",
		`{"session_id":`,
		`{"session_id":null}`,
		`{"session_id":123}`,           // wrong type → unmarshal error
		`{"session_id":"unterminated`,  // truncated
		`{"session_id":"ok","cwd":42}`, // wrong type on a string field
		`{"session_id":"ok"}`,          // valid, minimal
		`{"unrelated":"field"}`,        // no session_id
		`{"session_id":""}`,            // empty session_id
		strings.Repeat("a", 200000),    // large non-json blob
	}

	handlers := []struct {
		name string
		fn   func()
	}{
		{"notify", notifyHook},
		{"session-start", sessionStartHook},
		{"session-end", sessionEndHook},
	}

	for _, h := range handlers {
		for _, in := range inputs {
			// If any handler panics on any input, the test fails here.
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("%s panicked on input %q: %v", h.name, truncate(in), r)
					}
				}()
				withStdin(t, in, h.fn)
			}()
		}
	}
}

func truncate(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}
