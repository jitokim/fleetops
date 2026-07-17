package claude

import "testing"

func TestParsePsClaudePids(t *testing.T) {
	// header line, a plain name, a full-path comm (pgrep -x misses this
	// one), a claude.exe comm (observed in the wild: some installs report
	// the process as claude.exe, origin of the binary name TBD — see
	// matchesClaudeComm's doc), noise that must NOT match ("node",
	// "claude-helper" — prefix match on "claude" would wrongly include it).
	out := "  PID COMM\n" +
		" 6796 claude\n" +
		" 9195 /usr/local/bin/claude\n" +
		"72343 claude\n" +
		"12345 /whatever/claude.exe\n" +
		"  111 node\n" +
		"  222 claude-helper\n"

	pids := parsePsClaudePids(out)
	want := []int{6796, 9195, 72343, 12345}
	if len(pids) != len(want) {
		t.Fatalf("got %v, want %v", pids, want)
	}
	for i, w := range want {
		if pids[i] != w {
			t.Errorf("pids[%d] = %d, want %d", i, pids[i], w)
		}
	}
}

func TestMatchesClaudeComm(t *testing.T) {
	cases := []struct {
		comm string
		want bool
	}{
		{"claude", true},
		{"/usr/local/bin/claude", true},
		{"/whatever/claude.exe", true},
		{"claude.exe", true},
		{"claude-helper", false},
		{"node", false},
		{"", false},
	}
	for _, c := range cases {
		if got := matchesClaudeComm(c.comm); got != c.want {
			t.Errorf("matchesClaudeComm(%q) = %v, want %v", c.comm, got, c.want)
		}
	}
}

func TestParsePsClaudePids_Empty(t *testing.T) {
	if pids := parsePsClaudePids(""); len(pids) != 0 {
		t.Errorf("got %v, want empty", pids)
	}
}

func TestParsePsClaudePids_HeaderOnly(t *testing.T) {
	if pids := parsePsClaudePids("  PID COMM\n"); len(pids) != 0 {
		t.Errorf("got %v, want empty (header line must not parse as a pid)", pids)
	}
}

func TestParseLsofCwds(t *testing.T) {
	// real captured shape: p<pid>/fcwd/n<path> repeating per live process.
	out := "p6796\nfcwd\nn/home/user/dotfiles\n" +
		"p9195\nfcwd\nn/home/user/orca/projects/asre\n" +
		"p72343\nfcwd\nn/home/user/.someplugin/agent-sessions\n"

	counts := parseLsofCwds(out)
	if counts["/home/user/dotfiles"] != 1 {
		t.Errorf("dotfiles count = %d, want 1", counts["/home/user/dotfiles"])
	}
	if counts["/home/user/orca/projects/asre"] != 1 {
		t.Errorf("asre count = %d, want 1", counts["/home/user/orca/projects/asre"])
	}
	if counts["/home/user/.someplugin/agent-sessions"] != 1 {
		t.Errorf("observer-sessions count = %d, want 1", counts["/home/user/.someplugin/agent-sessions"])
	}
	if len(counts) != 3 {
		t.Errorf("got %d distinct cwds, want 3: %+v", len(counts), counts)
	}
}

func TestParseLsofCwds_MultipleProcsSameCwd(t *testing.T) {
	out := "p1\nfcwd\nn/home/user/myproject\n" +
		"p2\nfcwd\nn/home/user/myproject\n"

	counts := parseLsofCwds(out)
	if counts["/home/user/myproject"] != 2 {
		t.Errorf("myproject count = %d, want 2", counts["/home/user/myproject"])
	}
}

func TestParseLsofCwds_Empty(t *testing.T) {
	if counts := parseLsofCwds(""); len(counts) != 0 {
		t.Errorf("got %d cwds, want 0: %+v", len(counts), counts)
	}
}

func TestParseLsofCwds_IgnoresNonNLines(t *testing.T) {
	// only "n..." lines carry the cwd path; "p..."/"f..." lines and blanks
	// must be skipped, not misparsed.
	out := "p42\nfcwd\n\nn/tmp/x\n"
	counts := parseLsofCwds(out)
	if len(counts) != 1 || counts["/tmp/x"] != 1 {
		t.Errorf("got %+v, want {/tmp/x: 1}", counts)
	}
}
