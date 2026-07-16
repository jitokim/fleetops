package sessions

import "testing"

func TestParsePsPpidComm(t *testing.T) {
	cases := []struct {
		name     string
		out      string
		wantPP   int
		wantComm string
		wantOK   bool
	}{
		{"bare comm", " 1234 claude\n", 1234, "claude", true},
		{"full path comm", "  501 /usr/local/bin/claude\n", 501, "/usr/local/bin/claude", true},
		{"path with spaces kept verbatim", "  77 /opt/My Tools/claude\n", 77, "/opt/My Tools/claude", true},
		{"leading/trailing whitespace", "\t  42   node  \n", 42, "node", true},
		{"empty output (pid gone)", "", 0, "", false},
		{"whitespace only", "   \n  ", 0, "", false},
		{"no comm field", "1234\n", 0, "", false},
		{"non-numeric ppid", "PPID COMM\n", 0, "", false},
		{"first of multiple lines wins", " 10 claude\n 20 bash\n", 10, "claude", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pp, comm, ok := parsePsPpidComm(c.out)
			if ok != c.wantOK || pp != c.wantPP || comm != c.wantComm {
				t.Errorf("parsePsPpidComm(%q) = (%d, %q, %v), want (%d, %q, %v)",
					c.out, pp, comm, ok, c.wantPP, c.wantComm, c.wantOK)
			}
		})
	}
}

func TestParsePsTTY(t *testing.T) {
	cases := []struct {
		out  string
		want string
	}{
		{"ttys002\n", "ttys002"},
		{"  ttys014  \n", "ttys014"},
		{"??\n", ""},           // no controlling terminal → empty, not an error
		{"??", ""},             // no trailing newline
		{"", ""},               // probe returned nothing
		{"   \n", ""},          // whitespace only
		{"ttys001", "ttys001"}, // no trailing newline
	}
	for _, c := range cases {
		if got := parsePsTTY(c.out); got != c.want {
			t.Errorf("parsePsTTY(%q) = %q, want %q", c.out, got, c.want)
		}
	}
}

// fakeTree builds an ancestryStepFunc from a pid→(ppid,comm) map. A pid
// absent from the map reports ok=false (as if `ps` found no such process).
func fakeTree(tree map[int]struct {
	ppid int
	comm string
}) ancestryStepFunc {
	return func(pid int) (int, string, bool) {
		n, ok := tree[pid]
		if !ok {
			return 0, "", false
		}
		return n.ppid, n.comm, true
	}
}

func TestWalkToClaudePID(t *testing.T) {
	type node = struct {
		ppid int
		comm string
	}

	t.Run("direct parent is claude (the common case)", func(t *testing.T) {
		step := fakeTree(map[int]node{
			500: {ppid: 1, comm: "/usr/local/bin/claude"},
		})
		pid, found := walkToClaudePID(500, step)
		if !found || pid != 500 {
			t.Errorf("got (%d, %v), want (500, true)", pid, found)
		}
	})

	t.Run("one hop up through a wrapper shell", func(t *testing.T) {
		step := fakeTree(map[int]node{
			600: {ppid: 500, comm: "sh"},
			500: {ppid: 1, comm: "claude"},
		})
		pid, found := walkToClaudePID(600, step)
		if !found || pid != 500 {
			t.Errorf("got (%d, %v), want (500, true)", pid, found)
		}
	})

	t.Run("several hops up", func(t *testing.T) {
		step := fakeTree(map[int]node{
			700: {ppid: 690, comm: "env"},
			690: {ppid: 680, comm: "sh"},
			680: {ppid: 500, comm: "bash"},
			500: {ppid: 1, comm: "claude"},
		})
		pid, found := walkToClaudePID(700, step)
		if !found || pid != 500 {
			t.Errorf("got (%d, %v), want (500, true)", pid, found)
		}
	})

	t.Run("no claude in ancestry → not found", func(t *testing.T) {
		step := fakeTree(map[int]node{
			800: {ppid: 700, comm: "sh"},
			700: {ppid: 1, comm: "launchd"},
		})
		pid, found := walkToClaudePID(800, step)
		if found || pid != 0 {
			t.Errorf("got (%d, %v), want (0, false)", pid, found)
		}
	})

	t.Run("stops at pid<=1 (init/launchd)", func(t *testing.T) {
		step := fakeTree(map[int]node{1: {ppid: 0, comm: "launchd"}})
		if pid, found := walkToClaudePID(1, step); found || pid != 0 {
			t.Errorf("got (%d, %v), want (0, false)", pid, found)
		}
	})

	t.Run("step failure (pid vanished mid-walk) → not found", func(t *testing.T) {
		step := fakeTree(map[int]node{
			900: {ppid: 850, comm: "sh"}, // 850 not in tree → step returns ok=false
		})
		if pid, found := walkToClaudePID(900, step); found || pid != 0 {
			t.Errorf("got (%d, %v), want (0, false)", pid, found)
		}
	})

	t.Run("self-referential ppid can't loop forever", func(t *testing.T) {
		step := fakeTree(map[int]node{
			42: {ppid: 42, comm: "weird"}, // points at itself
		})
		if pid, found := walkToClaudePID(42, step); found || pid != 0 {
			t.Errorf("got (%d, %v), want (0, false)", pid, found)
		}
	})

	t.Run("claude deeper than the hop budget → not found", func(t *testing.T) {
		// build a chain longer than maxAncestryHops before reaching claude.
		tree := map[int]node{}
		const start = 1000
		pid := start
		for i := 0; i < maxAncestryHops+3; i++ {
			tree[pid] = node{ppid: pid + 1, comm: "sh"}
			pid++
		}
		tree[pid] = node{ppid: 1, comm: "claude"} // beyond the budget
		if got, found := walkToClaudePID(start, fakeTree(tree)); found {
			t.Errorf("got (%d, true), want not found (claude past hop budget)", got)
		}
	})

	t.Run("claude exactly at the hop budget edge is still found", func(t *testing.T) {
		// claude reachable in exactly maxAncestryHops steps.
		tree := map[int]node{}
		const start = 2000
		pid := start
		for i := 0; i < maxAncestryHops-1; i++ {
			tree[pid] = node{ppid: pid + 1, comm: "sh"}
			pid++
		}
		tree[pid] = node{ppid: 1, comm: "claude"}
		if got, found := walkToClaudePID(start, fakeTree(tree)); !found || got != pid {
			t.Errorf("got (%d, %v), want (%d, true)", got, found, pid)
		}
	})
}

// TestResolveClaudeTTY_FallsBackToStartPID is a light integration check: with
// a startPID whose ancestry contains no `claude` (the test binary's own
// parent), ResolveClaudeTTY must still return that startPID rather than 0, and
// must not panic when shelling out to the real ps.
func TestResolveClaudeTTY_FallsBackToStartPID(t *testing.T) {
	const startPID = 1 // launchd/init: definitely not claude, definitely exists
	pid, _ := ResolveClaudeTTY(startPID)
	if pid != startPID {
		t.Errorf("ResolveClaudeTTY(%d) pid = %d, want fallback %d", startPID, pid, startPID)
	}
}
