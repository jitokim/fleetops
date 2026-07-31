package claude

import (
	"context"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// procTimeout bounds the ps/lsof probes so a wedged process table never
// hangs the TUI's 3s refresh.
const procTimeout = 2 * time.Second

// LiveClaudeCwds returns real (unencoded) cwd → count of live `claude` CLI
// processes there, used to cross-check the JSONL-only signal (a session
// that stopped writing could be "waiting for human" or "the process is
// gone" — the log alone can't tell them apart; see applyLiveness in
// scan.go, which also does the path encoding needed to match these real
// paths against a loop's ProjectDir).
//
// ok=false means the probe itself failed (ps/lsof error or timeout) — the
// caller MUST NOT treat that as "confirmed dead": an empty-but-successful
// probe (ok=true, empty map) genuinely means zero live claude processes,
// which is real information, not a failure.
func LiveClaudeCwds() (map[string]int, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), procTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "axo", "pid,comm").Output()
	if err != nil {
		return map[string]int{}, false
	}
	pids := parsePsClaudePids(string(out))
	if len(pids) == 0 {
		return map[string]int{}, true // ps succeeded; genuinely zero live claude processes
	}
	pidStrs := make([]string, len(pids))
	for i, p := range pids {
		pidStrs[i] = strconv.Itoa(p)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), procTimeout)
	defer cancel2()
	lsofOut, err := exec.CommandContext(ctx2, "lsof", "-a", "-p", strings.Join(pidStrs, ","), "-d", "cwd", "-Fn").Output()
	if err != nil {
		return map[string]int{}, false
	}
	return parseLsofCwds(string(lsofOut)), true
}

// parsePsClaudePids parses `ps axo pid,comm` output into the pids whose comm
// is `claude` — matched on the base name so both a bare "claude" and a full
// path like "/usr/local/bin/claude" match (pgrep -x claude misses the
// latter, and can miss live processes outright — see LiveClaudeCwds' commit
// history). Each line is split on the first run of whitespace into pid +
// comm (comm may itself contain further whitespace, e.g. a path with
// spaces — kept as-is via filepath.Base). The header line ("PID COMM") and
// any unparseable line are skipped, not treated as errors.
//
// A trailing ".exe" is stripped before comparing (matchesClaudeComm) — a
// session has been observed showing up as "/whatever/claude.exe"
// (lsof-confirmed: `cclaude.exe / fcwd / n~/myproject`), origin of the
// binary name TBD (possibly a native-build install), and the strict
// "claude" comparison made that process invisible to this scan — wrong
// live counts, risking a false gone/drop demotion for its sibling loops.
// Deliberately NOT loosened to a
// prefix match: "claude-helper" and similar must stay excluded (pinned by
// TestMatchesClaudeComm's {"claude-helper", false} case).
func parsePsClaudePids(out string) []int {
	var pids []int
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		idx := strings.IndexFunc(line, unicode.IsSpace)
		if idx < 0 {
			continue // no comm field on this line
		}
		pid, err := strconv.Atoi(line[:idx])
		if err != nil {
			continue // e.g. the "PID COMM" header line
		}
		comm := strings.TrimSpace(line[idx:])
		if !matchesClaudeComm(comm) {
			continue
		}
		pids = append(pids, pid)
	}
	return pids
}

// matchesClaudeComm reports whether comm (a ps/tmux "current command" field)
// names a `claude` process — its base name is exactly "claude", or exactly
// "claude" once a trailing ".exe" is stripped (see parsePsClaudePids' doc).
// Never a prefix match: "claude-helper" must stay excluded.
func matchesClaudeComm(comm string) bool {
	name := strings.TrimSuffix(filepath.Base(comm), ".exe")
	return name == "claude"
}

// ListeningPortsByCwd returns real (unencoded) cwd → sorted distinct TCP
// ports with a listening socket held by a process working there — the
// captain's `make local PORT=xxxx` e2e server running as a sibling shell in
// a worktree, found the same way LiveClaudeCwds finds claude processes: two
// bounded lsof probes (system-wide listeners, then those pids' cwds), each
// under procTimeout so a wedged process table never hangs the TUI's refresh.
//
// ok=false means a probe itself failed — the caller MUST NOT treat that as
// "no servers": a racing lsof is not evidence a server died, so on failure
// nothing may be attached OR cleared based on this result. (lsof exits
// non-zero both on real failure and when zero sockets match the filter;
// they're indistinguishable here, and both correctly attach nothing.)
func ListeningPortsByCwd() (map[string][]int, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), procTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "lsof", "-nP", "-iTCP", "-sTCP:LISTEN", "-Fpn").Output()
	if err != nil {
		return map[string][]int{}, false
	}
	portsByPid := parseLsofListenPorts(string(out))
	if len(portsByPid) == 0 {
		return map[string][]int{}, true // probe succeeded; genuinely zero listeners
	}
	pidStrs := make([]string, 0, len(portsByPid))
	for pid := range portsByPid {
		pidStrs = append(pidStrs, strconv.Itoa(pid))
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), procTimeout)
	defer cancel2()
	cwdOut, err := exec.CommandContext(ctx2, "lsof", "-a", "-p", strings.Join(pidStrs, ","), "-d", "cwd", "-Fn").Output()
	if err != nil {
		return map[string][]int{}, false
	}
	return joinPortsByCwd(portsByPid, parseLsofPidCwds(string(cwdOut))), true
}

// parseLsofListenPorts parses `lsof -nP -iTCP -sTCP:LISTEN -Fpn` output —
// interleaved "p<pid>"/"f<fd>"/"n<addr>" lines, one "n" per listening
// socket — into pid → sorted distinct ports. One server commonly listens
// twice per port (IPv4 "*:3000" + IPv6 "[::]:3000"), so ports dedup per
// pid. An addr whose port doesn't parse (e.g. "*:*") attaches nothing —
// never a guessed port — and an unparseable "p" line orphans the "n" lines
// under it rather than crediting them to the previous pid.
func parseLsofListenPorts(out string) map[int][]int {
	ports := make(map[int][]int)
	pid := -1
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 2 {
			continue
		}
		switch line[0] {
		case 'p':
			id, err := strconv.Atoi(line[1:])
			if err != nil {
				pid = -1
				continue
			}
			pid = id
		case 'n':
			if pid < 0 {
				continue
			}
			port, ok := listenAddrPort(line[1:])
			if !ok {
				continue
			}
			if !slices.Contains(ports[pid], port) {
				ports[pid] = append(ports[pid], port)
			}
		}
	}
	for _, ps := range ports {
		sort.Ints(ps)
	}
	return ports
}

// listenAddrPort extracts the port from an lsof listen-socket name field —
// "*:3000", "127.0.0.1:3000", "[::1]:3000" — the text after the LAST colon
// (IPv6 addrs contain colons of their own). ok=false for anything that
// isn't a valid port number.
func listenAddrPort(addr string) (int, bool) {
	idx := strings.LastIndexByte(addr, ':')
	if idx < 0 {
		return 0, false
	}
	port, err := strconv.Atoi(addr[idx+1:])
	if err != nil || port < 1 || port > 65535 {
		return 0, false
	}
	return port, true
}

// parseLsofPidCwds parses `lsof -a -p <pids> -d cwd -Fn` output into
// pid → cwd path — the same record shape parseLsofCwds reads, but keyed by
// pid instead of counted, because the ports join needs to know WHICH
// process's cwd each listener has. An unparseable "p" line orphans the "n"
// under it (never credited to the previous pid), same as
// parseLsofListenPorts.
func parseLsofPidCwds(out string) map[int]string {
	cwds := make(map[int]string)
	pid := -1
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 2 {
			continue
		}
		switch line[0] {
		case 'p':
			id, err := strconv.Atoi(line[1:])
			if err != nil {
				pid = -1
				continue
			}
			pid = id
		case 'n':
			if pid < 0 {
				continue
			}
			cwds[pid] = line[1:]
		}
	}
	return cwds
}

// joinPortsByCwd joins the two probes: listener pids' ports, keyed by those
// pids' real cwd paths. A listener whose pid has no cwd record (process
// exited between the two probes, or lsof couldn't resolve it) contributes
// nothing — an unattributable port is never attached anywhere. Ports dedup
// per cwd (two servers in one dir can share a port across restarts' TIME_WAIT
// races, and a dir can host several pids listening on the same port family).
func joinPortsByCwd(portsByPid map[int][]int, cwdByPid map[int]string) map[string][]int {
	byCwd := make(map[string][]int)
	for pid, ports := range portsByPid {
		cwd, ok := cwdByPid[pid]
		if !ok || cwd == "" {
			continue
		}
		for _, p := range ports {
			if !slices.Contains(byCwd[cwd], p) {
				byCwd[cwd] = append(byCwd[cwd], p)
			}
		}
	}
	for _, ps := range byCwd {
		sort.Ints(ps)
	}
	return byCwd
}

// parseLsofCwds parses `lsof -a -p <pids> -d cwd -Fn` output: interleaved
// "p<pid>"/"f<fdtype>"/"n<path>" lines, one "n<path>" per live process (the
// path of its "cwd" fd) — counting how many processes share each cwd.
// Unknown/empty lines (including the "f..." fd-type lines) are skipped, not
// misparsed as a cwd.
func parseLsofCwds(out string) map[string]int {
	counts := make(map[string]int)
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 2 || line[0] != 'n' {
			continue
		}
		cwd := line[1:]
		if cwd == "" {
			continue
		}
		counts[cwd]++
	}
	return counts
}
