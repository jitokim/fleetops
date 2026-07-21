package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jitokim/fleetops/internal/accountstatus"
	"github.com/jitokim/fleetops/internal/gate"
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

// ── multi-account Phase B: ConfigDir capture + best-effort account status ──

// pinAccountStatus replaces the `claude auth status --json` seam for one
// test, so it never spawns a real `claude` binary. Also asserts the ctx it
// was called with actually has a deadline — resolveAccountLabel's whole
// non-fatal contract depends on the probe being BOUNDED.
func pinAccountStatus(t *testing.T, fn func(ctx context.Context, configDir string) (accountstatus.Status, bool)) {
	t.Helper()
	orig := accountStatusFn
	t.Cleanup(func() { accountStatusFn = orig })
	accountStatusFn = fn
}

// TestSessionStartHook_CapturesConfigDir_EvenWhenAccountProbeIsSkipped is the
// load-bearing guarantee: ConfigDir must be recorded from the environment
// regardless of whether the (best-effort, possibly-skipped) account-status
// probe runs at all. A default-account session (no CLAUDE_CONFIG_DIR set)
// skips the probe entirely (see resolveAccountLabel's doc) yet still writes
// ConfigDir="" correctly — proven separately from the probe's own behavior.
func TestSessionStartHook_CapturesConfigDir_EvenWhenAccountProbeIsSkipped(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	probed := false
	pinAccountStatus(t, func(ctx context.Context, configDir string) (accountstatus.Status, bool) {
		probed = true
		return accountstatus.Status{}, false
	})

	withStdin(t, `{"session_id":"default-acct","cwd":"/tmp/proj","source":"startup"}`, sessionStartHook)

	entry, err := sessions.ReadSession(sessions.SessionsDir(), "default-acct")
	if err != nil {
		t.Fatalf("expected an entry: %v", err)
	}
	if entry.ConfigDir != "" {
		t.Errorf("ConfigDir = %q, want empty (no CLAUDE_CONFIG_DIR set)", entry.ConfigDir)
	}
	if entry.AccountEmail != "" || entry.AccountPlan != "" {
		t.Errorf("AccountEmail/AccountPlan = %q/%q, want both empty for the default account", entry.AccountEmail, entry.AccountPlan)
	}
	if probed {
		t.Error("account-status probe ran for the default account (ConfigDir==\"\") — it must be skipped entirely, see resolveAccountLabel's doc")
	}
}

// TestSessionStartHook_NonDefaultConfigDir_RecordsAccountFromStatus confirms
// the full wiring for a bound (non-default) CLAUDE_CONFIG_DIR: ConfigDir is
// captured from the env, the status probe runs WITH that same config dir, and
// a loggedIn:true reply populates AccountEmail/AccountPlan.
func TestSessionStartHook_NonDefaultConfigDir_RecordsAccountFromStatus(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "/home/user/.claude-work")
	var gotConfigDir string
	pinAccountStatus(t, func(ctx context.Context, configDir string) (accountstatus.Status, bool) {
		gotConfigDir = configDir
		return accountstatus.Status{LoggedIn: true, Email: "jito@company.com", Plan: "team"}, true
	})

	withStdin(t, `{"session_id":"work-acct","cwd":"/tmp/proj","source":"startup"}`, sessionStartHook)

	entry, err := sessions.ReadSession(sessions.SessionsDir(), "work-acct")
	if err != nil {
		t.Fatalf("expected an entry: %v", err)
	}
	if entry.ConfigDir != "/home/user/.claude-work" {
		t.Errorf("ConfigDir = %q, want /home/user/.claude-work", entry.ConfigDir)
	}
	if gotConfigDir != "/home/user/.claude-work" {
		t.Errorf("status probe called with configDir=%q, want the session's own /home/user/.claude-work", gotConfigDir)
	}
	if entry.AccountEmail != "jito@company.com" {
		t.Errorf("AccountEmail = %q, want jito@company.com", entry.AccountEmail)
	}
	if entry.AccountPlan != "team" {
		t.Errorf("AccountPlan = %q, want team", entry.AccountPlan)
	}
}

// TestSessionStartHook_AccountProbe_LoggedInFalse_LeavesLabelsEmpty proves a
// configured-but-not-logged-in account degrades to empty labels rather than
// showing stale/wrong identity — never a token, never a guess.
func TestSessionStartHook_AccountProbe_LoggedInFalse_LeavesLabelsEmpty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "/home/user/.claude-stale")
	pinAccountStatus(t, func(ctx context.Context, configDir string) (accountstatus.Status, bool) {
		return accountstatus.Status{LoggedIn: false}, true
	})

	withStdin(t, `{"session_id":"stale-acct","cwd":"/tmp/proj","source":"startup"}`, sessionStartHook)

	entry, err := sessions.ReadSession(sessions.SessionsDir(), "stale-acct")
	if err != nil {
		t.Fatalf("expected an entry: %v", err)
	}
	if entry.ConfigDir != "/home/user/.claude-stale" {
		t.Errorf("ConfigDir = %q, want /home/user/.claude-stale — captured regardless of login status", entry.ConfigDir)
	}
	if entry.AccountEmail != "" || entry.AccountPlan != "" {
		t.Errorf("AccountEmail/AccountPlan = %q/%q, want both empty when loggedIn:false", entry.AccountEmail, entry.AccountPlan)
	}
}

// TestSessionStartHook_AccountProbe_Fails_DegradesSilently proves the hook
// still writes the entry (ConfigDir intact) and exits normally when the
// status probe itself errors — this must never be fatal to SessionStart.
func TestSessionStartHook_AccountProbe_Fails_DegradesSilently(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "/home/user/.claude-broken")
	pinAccountStatus(t, func(ctx context.Context, configDir string) (accountstatus.Status, bool) {
		return accountstatus.Status{}, false
	})

	withStdin(t, `{"session_id":"broken-acct","cwd":"/tmp/proj","source":"startup"}`, sessionStartHook)

	entry, err := sessions.ReadSession(sessions.SessionsDir(), "broken-acct")
	if err != nil {
		t.Fatalf("expected an entry even though the account probe failed: %v", err)
	}
	if entry.ConfigDir != "/home/user/.claude-broken" {
		t.Errorf("ConfigDir = %q, want /home/user/.claude-broken", entry.ConfigDir)
	}
	if entry.AccountEmail != "" || entry.AccountPlan != "" {
		t.Errorf("AccountEmail/AccountPlan should stay empty on probe failure, got %q/%q", entry.AccountEmail, entry.AccountPlan)
	}
}

// TestResolveAccountLabel_PassesBoundedContext confirms the ctx handed to the
// status-probe seam actually carries a deadline — the hook's "must never
// delay session start unacceptably" contract depends on this, not merely on
// queryAccountStatus's production implementation choosing to respect one.
func TestResolveAccountLabel_PassesBoundedContext(t *testing.T) {
	var sawDeadline bool
	pinAccountStatus(t, func(ctx context.Context, configDir string) (accountstatus.Status, bool) {
		_, sawDeadline = ctx.Deadline()
		return accountstatus.Status{}, false
	})

	resolveAccountLabel("/home/user/.claude-work")

	if !sawDeadline {
		t.Error("resolveAccountLabel's ctx has no deadline — the probe is not bounded")
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

func TestSummarizeToolInput(t *testing.T) {
	// tool_input's shape is per-tool and open-ended. What is pinned here is
	// that an unreadable or unfamiliar payload degrades to "" — the tool name
	// alone is still strictly more than the generic notification carried, so a
	// novel tool must never cost us the marker.
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"Bash command", `{"command":"go test ./...","description":"run tests"}`, "go test ./..."},
		{"Write file_path", `{"file_path":"/tmp/x.go","content":"..."}`, "/tmp/x.go"},
		{"WebFetch url", `{"url":"https://example.com","prompt":"summarize"}`, "https://example.com"},
		{"field precedence: command wins over file_path", `{"file_path":"/tmp/x","command":"rm /tmp/x"}`, "rm /tmp/x"},
		{"unknown tool shape", `{"somethingNew":{"nested":1}}`, ""},
		{"empty object", `{}`, ""},
		{"absent", ``, ""},
		{"malformed json", `{not json`, ""},
		{"not an object", `["a"]`, ""},
		{"empty string value is skipped", `{"command":"","file_path":"/tmp/x"}`, "/tmp/x"},
		{"non-string value is skipped", `{"command":42,"file_path":"/tmp/x"}`, "/tmp/x"},
		{"whitespace collapsed", "{\"command\":\"go   test\\n  ./...\"}", "go test ./..."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := summarizeToolInput([]byte(c.raw)); got != c.want {
				t.Errorf("summarizeToolInput(%s) = %q, want %q", c.raw, got, c.want)
			}
		})
	}
}

func TestSummarizeToolInput_LongCommandBounded(t *testing.T) {
	// A gate callout is one line in a cockpit showing a whole fleet. Bounded
	// at write time so the marker file cannot grow without limit either.
	raw, err := json.Marshal(map[string]string{"command": strings.Repeat("x", toolDetailCap*3)})
	if err != nil {
		t.Fatal(err)
	}
	got := summarizeToolInput(raw)
	if n := utf8.RuneCountInString(got); n > toolDetailCap+1 { // +1 for the ellipsis
		t.Errorf("detail rune length = %d, want <= %d", n, toolDetailCap+1)
	}
}

func TestPermissionHook_WritesToolDetail(t *testing.T) {
	// End-to-end through the hook's own stdin path: the payload shape here is
	// a verbatim reduction of a real PermissionRequest payload measured
	// 2026-07-20.
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	const session = "sess-perm"

	payload := `{"hook_event_name":"PermissionRequest","session_id":"` + session + `",` +
		`"prompt_id":"77e62224-b63c-4744-ae73-38eb3764e406","tool_name":"Bash",` +
		`"tool_input":{"command":"echo PERMREQ","description":"Echo PERMREQ"},"permission_mode":"default"}`

	withStdin(t, payload, permissionHook)

	got := gate.Pending(gate.GatesDir())[session]
	if got.Tool != "Bash" {
		t.Errorf("Tool = %q, want %q", got.Tool, "Bash")
	}
	if got.ToolDetail != "echo PERMREQ" {
		t.Errorf("ToolDetail = %q, want %q", got.ToolDetail, "echo PERMREQ")
	}
	if got.PromptID == "" {
		t.Error("PromptID not recorded — the merge rules have nothing to correlate on")
	}
	if want := "Bash: echo PERMREQ"; got.Detail() != want {
		t.Errorf("Detail() = %q, want %q", got.Detail(), want)
	}
}

func TestPermissionHook_IsASensorOnly(t *testing.T) {
	// A PermissionRequest hook MAY return a permissionDecision and thereby
	// grant or deny the permission itself. fleetops must not: a decision made
	// here leaves no event, no actor, and nothing to attribute or brake. This
	// test guards the boundary that keeps acting auditable — if it ever fails,
	// read permissionHook's contract before "fixing" it.
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	payload := `{"session_id":"s1","tool_name":"Bash","tool_input":{"command":"rm -rf /"}}`

	out := captureStdout(t, func() { withStdin(t, payload, permissionHook) })

	if strings.TrimSpace(out) != "" {
		t.Errorf("permission hook wrote to stdout: %q — anything here can be read as a permission decision", out)
	}
}

func TestPermissionHook_MissingSessionID_NoWrite(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	withStdin(t, `{"tool_name":"Bash"}`, permissionHook)
	if len(gate.Pending(gate.GatesDir())) != 0 {
		t.Error("expected no marker without a session id")
	}
}

func TestPermissionHook_MalformedPayload_NoPanic(t *testing.T) {
	// Claude Code runs this on every permission prompt. It must never fail
	// loudly — a bug here must not be able to break the user's real session.
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	for _, payload := range []string{``, `{`, `null`, `[]`, `{"session_id":123}`} {
		withStdin(t, payload, permissionHook)
	}
}

// captureStdout collects everything fn writes to os.Stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	fn()
	os.Stdout = orig
	_ = w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}
