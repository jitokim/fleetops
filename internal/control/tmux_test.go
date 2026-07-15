package control

import (
	"strings"
	"testing"
)

func TestParseTmuxPanes(t *testing.T) {
	out := "%3\t/Users/imac/IdeaProjects/aboard\tclaude\n%7\t/Users/imac/IdeaProjects/other\tzsh\n"
	targets := parseTmuxPanes(out)

	if len(targets) != 2 {
		t.Fatalf("got %d targets, want 2: %+v", len(targets), targets)
	}
	if targets[0].ID != "%3" || targets[0].Cwd != "/Users/imac/IdeaProjects/aboard" {
		t.Errorf("targets[0] = %+v, want ID %%3 cwd aboard", targets[0])
	}
	if targets[1].ID != "%7" || targets[1].Cwd != "/Users/imac/IdeaProjects/other" {
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
	out := "%3\t/Users/imac/IdeaProjects/aboard\tclaude\nnotabline\n%9\t/tmp\tzsh\n"
	targets := parseTmuxPanes(out)
	if len(targets) != 2 {
		t.Fatalf("got %d targets, want 2 (malformed line skipped): %+v", len(targets), targets)
	}
}

func TestParseTmuxPanes_IncludesNonClaudePanes(t *testing.T) {
	// Locate/Focus (attach) must be able to reach a bare shell pane — unlike
	// parseTmuxClaudePanes, parseTmuxPanes does not filter by command.
	out := "%3\t/Users/imac/IdeaProjects/aboard\tzsh\n"
	targets := parseTmuxPanes(out)
	if len(targets) != 1 || targets[0].ID != "%3" {
		t.Errorf("got %+v, want the shell pane included", targets)
	}
}

func TestParseTmuxClaudePanes_FiltersToClaudeOnly(t *testing.T) {
	// P0-3: a shell pane first, a claude pane second, sharing no particular
	// order — only the claude pane must come back.
	out := "%3\t/Users/imac/IdeaProjects/aboard\tzsh\n%7\t/Users/imac/IdeaProjects/aboard\tclaude\n"
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
	out := "%3\t/Users/imac/IdeaProjects/aboard\tzsh\n%7\t/Users/imac/IdeaProjects/aboard\tvim\n"
	if targets := parseTmuxClaudePanes(out); len(targets) != 0 {
		t.Errorf("got %d targets, want 0 (no claude panes)", len(targets))
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
	out := "%3\t/Users/imac/IdeaProjects/aboard\tzsh\n%7\t/Users/imac/IdeaProjects/aboard\tclaude\n"
	projectDir := "-Users-imac-IdeaProjects-aboard"

	var locateTarget Target
	for _, tg := range parseTmuxPanes(out) {
		if strings.ReplaceAll(tg.Cwd, "/", "-") == projectDir {
			locateTarget = tg
			break
		}
	}
	if locateTarget.ID != "%3" {
		t.Errorf("Locate's selection = %+v, want the shell pane (%%3, first match)", locateTarget)
	}

	var locateClaudeTarget Target
	for _, tg := range parseTmuxClaudePanes(out) {
		if strings.ReplaceAll(tg.Cwd, "/", "-") == projectDir {
			locateClaudeTarget = tg
			break
		}
	}
	if locateClaudeTarget.ID != "%7" {
		t.Errorf("LocateClaude's selection = %+v, want the claude pane (%%7)", locateClaudeTarget)
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
	got := tmuxNewWindowCmd("/Users/imac/IdeaProjects/aboard")
	want := []string{"tmux", "new-window", "-c", "/Users/imac/IdeaProjects/aboard", "-P", "-F", "#{pane_id}", "claude"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
