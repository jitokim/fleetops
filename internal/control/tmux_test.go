package control

import (
	"testing"
)

func TestParseTmuxPanes(t *testing.T) {
	out := "%3\t/home/user/myproject\tclaude\n%7\t/home/user/other\tzsh\n"
	targets := parseTmuxPanes(out)

	if len(targets) != 2 {
		t.Fatalf("got %d targets, want 2: %+v", len(targets), targets)
	}
	if targets[0].ID != "%3" || targets[0].Cwd != "/home/user/myproject" {
		t.Errorf("targets[0] = %+v, want ID %%3 cwd myproject", targets[0])
	}
	if targets[1].ID != "%7" || targets[1].Cwd != "/home/user/other" {
		t.Errorf("targets[1] = %+v, want ID %%7 cwd other", targets[1])
	}
	for _, tg := range targets {
		if tg.Backend != "tmux" {
			t.Errorf("target %+v backend = %q, want tmux", tg, tg.Backend)
		}
	}
}

func TestParseTmuxPanes_Empty(t *testing.T) {
	if targets := parseTmuxPanes(""); len(targets) != 0 {
		t.Errorf("empty input: got %d targets, want 0", len(targets))
	}
}

func TestParseTmuxPanes_MalformedLineSkipped(t *testing.T) {
	// a line missing a field (no command) must be skipped, not panic.
	out := "%3\t/home/user/myproject\tclaude\nnotabline\n%9\t/tmp\tzsh\n"
	targets := parseTmuxPanes(out)
	if len(targets) != 2 {
		t.Fatalf("got %d targets, want 2 (malformed line skipped): %+v", len(targets), targets)
	}
}

func TestParseTmuxPanes_IncludesNonClaudePanes(t *testing.T) {
	// Locate/Focus (attach) must be able to reach a bare shell pane — unlike
	// parseTmuxClaudePanes, parseTmuxPanes does not filter by command.
	out := "%3\t/home/user/myproject\tzsh\n"
	targets := parseTmuxPanes(out)
	if len(targets) != 1 || targets[0].ID != "%3" {
		t.Errorf("got %+v, want the shell pane included", targets)
	}
}

func TestParseTmuxClaudePanes_FiltersToClaudeOnly(t *testing.T) {
	// P0-3: a shell pane first, a claude pane second, sharing no particular
	// order — only the claude pane must come back.
	out := "%3\t/home/user/myproject\tzsh\n%7\t/home/user/myproject\tclaude\n"
	targets := parseTmuxClaudePanes(out)
	if len(targets) != 1 {
		t.Fatalf("got %d targets, want 1 (only the claude pane): %+v", len(targets), targets)
	}
	if targets[0].ID != "%7" {
		t.Errorf("got %+v, want the claude pane (%%7)", targets[0])
	}
}

func TestParseTmuxClaudePanes_Empty(t *testing.T) {
	if targets := parseTmuxClaudePanes(""); len(targets) != 0 {
		t.Errorf("empty input: got %d targets, want 0", len(targets))
	}
}

func TestParseTmuxClaudePanes_NoClaudePanes(t *testing.T) {
	out := "%3\t/home/user/myproject\tzsh\n%7\t/home/user/myproject\tvim\n"
	if targets := parseTmuxClaudePanes(out); len(targets) != 0 {
		t.Errorf("got %d targets, want 0 (no claude panes)", len(targets))
	}
}

// TestParseTmuxClaudePanes_ClaudeExeMatches is the review fix's regression:
// a pane_current_command of "claude.exe" (live 2026-07-17, see
// isClaudeComm's doc) must still be recognized as a claude pane.
func TestParseTmuxClaudePanes_ClaudeExeMatches(t *testing.T) {
	out := "%7\t/home/user/myproject\tclaude.exe\n"
	targets := parseTmuxClaudePanes(out)
	if len(targets) != 1 {
		t.Fatalf("got %d targets, want 1 (claude.exe must match): %+v", len(targets), targets)
	}
}

func TestIsClaudeComm(t *testing.T) {
	cases := []struct {
		comm string
		want bool
	}{
		{"claude", true},
		{"/usr/local/bin/claude", true},
		{"claude.exe", true},
		{"/whatever/claude.exe", true},
		{"claude-helper", false},
		{"zsh", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isClaudeComm(c.comm); got != c.want {
			t.Errorf("isClaudeComm(%q) = %v, want %v", c.comm, got, c.want)
		}
	}
}

// TestLocateVsLocateClaude_ShellPaneFirstClaudePaneSecond exercises the
// exact P0-3 scenario: two panes share a directory, a bare shell pane
// listed first and a claude pane listed second. Locate (attach, permissive)
// and LocateClaude (typed actions, claude-only) share the same "first
// match wins" selection over parseTmuxPanes/parseTmuxClaudePanes
// respectively — this proves they diverge exactly as required: Locate must
// not skip past the shell pane to find claude (attach should reach
// whichever surface is actually at that directory), while LocateClaude must
// never return the shell pane at all.
func TestLocateVsLocateClaude_ShellPaneFirstClaudePaneSecond(t *testing.T) {
	out := "%3\t/home/user/myproject\tzsh\n%7\t/home/user/myproject\tclaude\n"
	projectDir := "-home-user-myproject"

	var locateTarget Target
	for _, tg := range parseTmuxPanes(out) {
		if encodeCwd(tg.Cwd) == projectDir {
			locateTarget = tg
			break
		}
	}
	if locateTarget.ID != "%3" {
		t.Errorf("Locate's selection = %+v, want the shell pane (%%3, first match)", locateTarget)
	}

	locateClaudeTarget, ok := selectClaudeTmuxPane(parseTmuxClaudePanes(out), projectDir)
	if !ok {
		t.Fatal("expected ok=true — exactly one claude pane matches")
	}
	if locateClaudeTarget.ID != "%7" {
		t.Errorf("LocateClaude's selection = %+v, want the claude pane (%%7)", locateClaudeTarget)
	}
}

func TestSelectClaudeTmuxPane_TwoClaudePanesSameCwd_Refuses(t *testing.T) {
	// Residual #1: two claude panes at the same directory — no way to tell
	// which one was meant, so the backstop must refuse rather than pick
	// either (whichever tmux happens to list first).
	out := "%3\t/home/user/myproject\tclaude\n%7\t/home/user/myproject\tclaude\n"
	if _, ok := selectClaudeTmuxPane(parseTmuxClaudePanes(out), "-home-user-myproject"); ok {
		t.Error("expected ok=false — two claude panes share this directory, ambiguous")
	}
}

func TestSelectClaudeTmuxPane_OneClaudePane_Found(t *testing.T) {
	out := "%7\t/home/user/myproject\tclaude\n"
	target, ok := selectClaudeTmuxPane(parseTmuxClaudePanes(out), "-home-user-myproject")
	if !ok {
		t.Fatal("expected ok=true — exactly one claude pane")
	}
	if target.ID != "%7" {
		t.Errorf("got ID %q, want %%7", target.ID)
	}
}

func TestSelectClaudeTmuxPane_DotContainingCwd_Matches(t *testing.T) {
	// Residual #4: encodeCwd (both "/" and "." -> "-") lets a dot-containing
	// pane cwd actuate instead of always degrading.
	out := "%7\t/x/foo.bar\tclaude\n"
	target, ok := selectClaudeTmuxPane(parseTmuxClaudePanes(out), "-x-foo-bar")
	if !ok {
		t.Fatal("expected ok=true — encodeCwd must match the dot-containing cwd")
	}
	if target.ID != "%7" {
		t.Errorf("got ID %q, want %%7", target.ID)
	}
}

func TestTmuxResumeCmds(t *testing.T) {
	cmds := tmuxResumeCmds("%3", "hello world")
	want := [][]string{
		{"tmux", "send-keys", "-t", "%3", "-l", "--", "hello world"},
		{"tmux", "send-keys", "-t", "%3", "Enter"},
	}
	if len(cmds) != len(want) {
		t.Fatalf("got %d cmds, want %d", len(cmds), len(want))
	}
	for i := range want {
		if len(cmds[i]) != len(want[i]) {
			t.Fatalf("cmd[%d] = %v, want %v", i, cmds[i], want[i])
		}
		for j := range want[i] {
			if cmds[i][j] != want[i][j] {
				t.Errorf("cmd[%d][%d] = %q, want %q", i, j, cmds[i][j], want[i][j])
			}
		}
	}
}

func TestTmuxApproveCmd(t *testing.T) {
	got := tmuxApproveCmd("%3")
	want := []string{"tmux", "send-keys", "-t", "%3", "Enter"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestTmuxFocusCmds(t *testing.T) {
	cmds := tmuxFocusCmds("%3")
	want := [][]string{
		{"tmux", "select-pane", "-t", "%3"},
		{"tmux", "switch-client", "-t", "%3"},
	}
	if len(cmds) != len(want) {
		t.Fatalf("got %d cmds, want %d", len(cmds), len(want))
	}
	for i := range want {
		if len(cmds[i]) != len(want[i]) {
			t.Fatalf("cmd[%d] = %v, want %v", i, cmds[i], want[i])
		}
		for j := range want[i] {
			if cmds[i][j] != want[i][j] {
				t.Errorf("cmd[%d][%d] = %q, want %q", i, j, cmds[i][j], want[i][j])
			}
		}
	}
}

func TestTmuxInterruptCmd(t *testing.T) {
	got := tmuxInterruptCmd("%3")
	want := []string{"tmux", "send-keys", "-t", "%3", "Escape"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestTmuxNewWindowCmd(t *testing.T) {
	got := tmuxNewWindowCmd("/home/user/myproject")
	want := []string{"tmux", "new-window", "-c", "/home/user/myproject", "-P", "-F", "#{pane_id}", "claude"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// feat/engine-provenance: OpenTerminal's argv — generalized from
// tmuxNewWindowCmd's hardcoded "claude" to an arbitrary command (take-over's
// "claude --resume <id>"), and unlike tmuxNewWindowCmd, no -P -F pane-id
// capture (see OpenTerminal's doc — no follow-up send needed).
func TestTmuxOpenTerminalCmd(t *testing.T) {
	got := tmuxOpenTerminalCmd("/home/user/myproject", "claude --resume sess-1")
	want := []string{"tmux", "new-window", "-c", "/home/user/myproject", "claude --resume sess-1"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// ── tty-keyed dispatch (ADR Phase 2 §2.2/§3 step 2) ──────────────────

func TestNormalizeTTY_StripsDevPrefix(t *testing.T) {
	if got := normalizeTTY("/dev/ttys012"); got != "ttys012" {
		t.Errorf("got %q, want %q", got, "ttys012")
	}
}

func TestNormalizeTTY_BareFormAlreadyNormalized(t *testing.T) {
	if got := normalizeTTY("ttys012"); got != "ttys012" {
		t.Errorf("got %q, want %q", got, "ttys012")
	}
}

func TestNormalizeTTY_BothFormsCompareEqual(t *testing.T) {
	// the registry stores the bare form ("ttys012", from `ps -o tty=`);
	// tmux's #{pane_tty} reports the full device path ("/dev/ttys012") —
	// normalizeTTY must make these compare equal, which is the whole point
	// of tty-keyed dispatch working at all.
	if normalizeTTY("ttys012") != normalizeTTY("/dev/ttys012") {
		t.Errorf("normalizeTTY(%q) != normalizeTTY(%q), want equal", "ttys012", "/dev/ttys012")
	}
}

func TestParseTmuxTTYPaneLines(t *testing.T) {
	out := "/dev/ttys012\t%3\tclaude\n/dev/ttys013\t%7\tzsh\n"
	lines := parseTmuxTTYPaneLines(out)

	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %+v", len(lines), lines)
	}
	if lines[0] != (tmuxTTYPaneLine{TTY: "/dev/ttys012", ID: "%3", Command: "claude"}) {
		t.Errorf("lines[0] = %+v, want tty=/dev/ttys012 id=%%3 command=claude", lines[0])
	}
	if lines[1] != (tmuxTTYPaneLine{TTY: "/dev/ttys013", ID: "%7", Command: "zsh"}) {
		t.Errorf("lines[1] = %+v, want tty=/dev/ttys013 id=%%7 command=zsh", lines[1])
	}
}

func TestParseTmuxTTYPaneLines_Empty(t *testing.T) {
	if lines := parseTmuxTTYPaneLines(""); len(lines) != 0 {
		t.Errorf("got %d lines, want 0", len(lines))
	}
}

func TestParseTmuxTTYPaneLines_MalformedLineSkipped(t *testing.T) {
	out := "/dev/ttys012\t%3\tclaude\nnotabline\n/dev/ttys099\t%9\tzsh\n"
	lines := parseTmuxTTYPaneLines(out)
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2 (malformed line skipped): %+v", len(lines), lines)
	}
}

func TestSelectTTYPane_ShellPaneFirstClaudePaneSecond_DifferentTTYs(t *testing.T) {
	// shell pane and claude pane at DIFFERENT ttys (tty is what
	// disambiguates here, not directory, unlike the cwd path) —
	// selectTTYPane must pick the claude pane at the requested tty,
	// ignoring the shell pane entirely regardless of listing order.
	lines := parseTmuxTTYPaneLines("/dev/ttys012\t%3\tzsh\n/dev/ttys013\t%7\tclaude\n")

	target, ok := selectTTYPane(lines, "ttys013")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if target.ID != "%7" {
		t.Errorf("got ID %q, want %%7 (the claude pane at ttys013)", target.ID)
	}
	if target.Backend != "tmux" {
		t.Errorf("Backend = %q, want tmux", target.Backend)
	}
}

func TestSelectTTYPane_RegistryFormMatchesDevPrefixedPaneTTY(t *testing.T) {
	// the registry stores the bare form ("ttys012"); tmux reports
	// "#{pane_tty}" as the full device path ("/dev/ttys012") — this is the
	// exact normalization the whole feature depends on.
	lines := parseTmuxTTYPaneLines("/dev/ttys012\t%3\tclaude\n")

	target, ok := selectTTYPane(lines, "ttys012")
	if !ok {
		t.Fatal("expected ok=true — normalizeTTY must make these compare equal")
	}
	if target.ID != "%3" {
		t.Errorf("got ID %q, want %%3", target.ID)
	}
}

func TestSelectTTYPane_NoMatchingTTY(t *testing.T) {
	lines := parseTmuxTTYPaneLines("/dev/ttys012\t%3\tclaude\n")
	if _, ok := selectTTYPane(lines, "ttys099"); ok {
		t.Error("expected ok=false for a tty that isn't present in the fixture")
	}
}

func TestSelectTTYPane_MatchingTTYButNotClaude_NoMatch(t *testing.T) {
	lines := parseTmuxTTYPaneLines("/dev/ttys012\t%3\tvim\n")
	if _, ok := selectTTYPane(lines, "ttys012"); ok {
		t.Error("expected ok=false — the tty matches but the foreground command isn't claude")
	}
}

// TestSelectTTYPane_ClaudeExeMatches is the review fix's regression for the
// tty-dispatch path specifically (see isClaudeComm's doc).
func TestSelectTTYPane_ClaudeExeMatches(t *testing.T) {
	lines := parseTmuxTTYPaneLines("/dev/ttys012\t%3\tclaude.exe\n")
	target, ok := selectTTYPane(lines, "ttys012")
	if !ok {
		t.Fatal("expected ok=true — claude.exe must match the same as claude")
	}
	if target.ID != "%3" {
		t.Errorf("got ID %q, want %%3", target.ID)
	}
}

func TestSelectTTYPane_EmptyLines(t *testing.T) {
	if _, ok := selectTTYPane(nil, "ttys012"); ok {
		t.Error("expected ok=false for no panes at all")
	}
}
