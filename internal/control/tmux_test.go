package control

import "testing"

func TestParseTmuxPanes(t *testing.T) {
	out := "%3\t/Users/imac/IdeaProjects/aboard\n%7\t/Users/imac/IdeaProjects/other\n"
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
	// a line missing the tab separator (no cwd) must be skipped, not panic.
	out := "%3\t/Users/imac/IdeaProjects/aboard\nnotabline\n%9\t/tmp\n"
	targets := parseTmuxPanes(out)
	if len(targets) != 2 {
		t.Fatalf("got %d targets, want 2 (malformed line skipped): %+v", len(targets), targets)
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
