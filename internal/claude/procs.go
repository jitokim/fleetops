package claude

import (
	"context"
	"os/exec"
	"path/filepath"
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
// prefix match: "claude-helper" and similar must stay excluded (see
// TestParsePsClaudePids_ExcludesClaudeHelper).
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
