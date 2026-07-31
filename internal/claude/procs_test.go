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

func TestParseLsofListenPorts(t *testing.T) {
	// real captured shape of `lsof -nP -iTCP -sTCP:LISTEN -Fpn`: p/f/n
	// records, one n per listening socket. pid 100 listens on 3000 twice
	// (IPv4 "*:3000" + IPv6 "[::]:3000" — the common dual-stack server,
	// must dedup to ONE 3000) plus 8080; pid 200 on a loopback addr.
	out := "p100\nf23\nn*:3000\nf24\nn[::]:3000\nf25\nn*:8080\n" +
		"p200\nf10\nn127.0.0.1:5173\n"

	ports := parseLsofListenPorts(out)
	if len(ports) != 2 {
		t.Fatalf("got %d pids, want 2: %+v", len(ports), ports)
	}
	if len(ports[100]) != 2 || ports[100][0] != 3000 || ports[100][1] != 8080 {
		t.Errorf("ports[100] = %v, want [3000 8080] (dual-stack 3000 deduped, sorted)", ports[100])
	}
	if len(ports[200]) != 1 || ports[200][0] != 5173 {
		t.Errorf("ports[200] = %v, want [5173]", ports[200])
	}
}

func TestParseLsofListenPorts_UnparseableAddrAttachesNothing(t *testing.T) {
	// an addr with no valid port ("*:*", garbage) must attach nothing —
	// never a guessed port. Honesty rule: ambiguous → nothing.
	out := "p100\nf23\nn*:*\nf24\nngarbage\n"
	if ports := parseLsofListenPorts(out); len(ports) != 0 {
		t.Errorf("got %+v, want empty — unparseable addrs must not attach", ports)
	}
}

func TestParseLsofListenPorts_BadPidLineOrphansItsRecords(t *testing.T) {
	// an unparseable "p" line must orphan the n-lines under it, NOT credit
	// them to the previous pid.
	out := "p100\nf23\nn*:3000\npNOPE\nf24\nn*:9999\n"
	ports := parseLsofListenPorts(out)
	if len(ports) != 1 || len(ports[100]) != 1 || ports[100][0] != 3000 {
		t.Errorf("got %+v, want {100: [3000]} — 9999 belongs to an unparseable pid", ports)
	}
}

func TestParseLsofListenPorts_Empty(t *testing.T) {
	if ports := parseLsofListenPorts(""); len(ports) != 0 {
		t.Errorf("got %+v, want empty", ports)
	}
}

func TestListenAddrPort(t *testing.T) {
	cases := []struct {
		addr string
		port int
		ok   bool
	}{
		{"*:3000", 3000, true},
		{"127.0.0.1:8080", 8080, true},
		{"[::1]:5173", 5173, true}, // port is after the LAST colon — IPv6 addrs contain their own
		{"[::]:80", 80, true},
		{"*:*", 0, false},
		{"no-colon", 0, false},
		{"*:0", 0, false},     // 0 is not a real listening port
		{"*:99999", 0, false}, // out of range
		{"", 0, false},
	}
	for _, c := range cases {
		port, ok := listenAddrPort(c.addr)
		if port != c.port || ok != c.ok {
			t.Errorf("listenAddrPort(%q) = (%d, %v), want (%d, %v)", c.addr, port, ok, c.port, c.ok)
		}
	}
}

func TestParseLsofPidCwds(t *testing.T) {
	// same record shape parseLsofCwds reads, but keyed by pid for the
	// ports join.
	out := "p100\nfcwd\nn/x/worktree-a\np200\nfcwd\nn/x/worktree-b\n"
	cwds := parseLsofPidCwds(out)
	if len(cwds) != 2 || cwds[100] != "/x/worktree-a" || cwds[200] != "/x/worktree-b" {
		t.Errorf("got %+v, want {100: /x/worktree-a, 200: /x/worktree-b}", cwds)
	}
}

func TestJoinPortsByCwd(t *testing.T) {
	portsByPid := map[int][]int{
		100: {3000},
		200: {8080},
		300: {9999}, // no cwd record (exited between probes) — must attach nowhere
	}
	cwdByPid := map[int]string{
		100: "/x/worktree-a",
		200: "/x/worktree-a", // second server in the same dir — ports merge
	}

	byCwd := joinPortsByCwd(portsByPid, cwdByPid)
	if len(byCwd) != 1 {
		t.Fatalf("got %d cwds, want 1: %+v", len(byCwd), byCwd)
	}
	got := byCwd["/x/worktree-a"]
	if len(got) != 2 || got[0] != 3000 || got[1] != 8080 {
		t.Errorf("got %v, want [3000 8080] (merged across pids, sorted)", got)
	}
}
