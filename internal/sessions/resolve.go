package sessions

import (
	"context"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// This tty/ancestry resolution lives in internal/sessions rather than
// extending internal/claude/procs.go on purpose. cmd/fleetops does not
// otherwise import internal/claude (it imports gate and now sessions), and
// procs.go solves a different problem — enumerating ALL live claude cwds via
// `ps axo`+`lsof`. What the SessionStart hook needs is the inverse: walk UP
// from one known pid to find the claude process, then read that one pid's
// tty. Colocating it with the registry that consumes it keeps the new
// dependency edge cmd/fleetops→sessions instead of adding cmd→claude, and
// keeps the two `ps`-shelling concerns from bleeding into one package. The
// string-parsing pieces (parsePsPpidComm/parsePsTTY) and the walk itself
// (walkToClaudePID) are pure and unit-tested with fixture strings; only the
// exec.Command calls are the untestable seam, kept thin — same shape as
// procs.go's parsePsClaudePids/parseLsofCwds.

// procTimeout bounds each ps probe so a wedged process table can't hang the
// hook (which claude runs synchronously on every session start/end).
const procTimeout = 2 * time.Second

// maxAncestryHops bounds the parent walk so a pathological or cyclic process
// tree can never loop forever. Both research spikes found the DIRECT parent
// (os.Getppid()) already resolves to `claude` — a single-command sh -c
// wrapper exec-optimizes away — so the walk is cheap defense-in-depth for a
// non-exec-optimizing wrapper, not the common path. A handful of hops is
// plenty.
const maxAncestryHops = 8

// claudeComm is the process name we walk the ancestry looking for. Matched on
// the base name (filepath.Base) so both a bare "claude" and a full path like
// "/usr/local/bin/claude" match — same convention as procs.go.
const claudeComm = "claude"

// noTTY is `ps -o tty=`'s sentinel for a process with no controlling terminal
// (a piped/headless `-p` session). We store that as an empty TTY, not an
// error.
const noTTY = "??"

// ResolveClaudeTTY walks the process ancestry from startPID (typically the
// hook's os.Getppid()) up to maxAncestryHops looking for the `claude`
// process, and returns that pid together with its controlling tty. If no
// claude process is found within the hop budget (a pathological tree), it
// falls back to startPID — which in every observed real case IS claude
// anyway. The returned tty is "" when the process has no controlling
// terminal ("??"), which is expected for headless sessions and not an error.
func ResolveClaudeTTY(startPID int) (pid int, tty string) {
	resolved, found := walkToClaudePID(startPID, ancestryStep)
	if !found {
		resolved = startPID
	}
	return resolved, resolveTTY(resolved)
}

// ancestryStepFunc reports a pid's parent pid and comm. Injected into
// walkToClaudePID so the walk logic is testable without shelling out.
type ancestryStepFunc func(pid int) (ppid int, comm string, ok bool)

// walkToClaudePID climbs the parent chain from startPID until it finds a
// process whose comm base name is "claude", bounded by maxAncestryHops. It
// stops (found=false) at pid <= 1 (init/launchd), on a step failure, or on a
// self-referential ppid — none of which should loop forever.
func walkToClaudePID(startPID int, step ancestryStepFunc) (pid int, found bool) {
	pid = startPID
	for hop := 0; hop < maxAncestryHops; hop++ {
		if pid <= 1 {
			return 0, false
		}
		ppid, comm, ok := step(pid)
		if !ok {
			return 0, false
		}
		if filepath.Base(comm) == claudeComm {
			return pid, true
		}
		if ppid == pid {
			return 0, false // self-loop guard
		}
		pid = ppid
	}
	return 0, false
}

// ancestryStep shells out `ps -o ppid=,comm= -p <pid>` (the exec seam) and
// hands the raw output to parsePsPpidComm.
func ancestryStep(pid int) (ppid int, comm string, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), procTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", "ppid=,comm=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, "", false
	}
	return parsePsPpidComm(string(out))
}

// parsePsPpidComm parses one line of `ps -o ppid=,comm= -p <pid>` output
// (empty column headers, so no header row): a right-justified ppid, then the
// comm, which may itself contain whitespace (a path with spaces) and is kept
// verbatim. An empty/unparseable line (e.g. the pid no longer exists) yields
// ok=false rather than a bogus zero pid.
func parsePsPpidComm(out string) (ppid int, comm string, ok bool) {
	line := strings.TrimSpace(out)
	if line == "" {
		return 0, "", false
	}
	// ps may emit multiple lines if given multiple pids; we only asked for
	// one, so take the first non-empty line.
	if nl := strings.IndexByte(line, '\n'); nl >= 0 {
		line = strings.TrimSpace(line[:nl])
	}
	idx := strings.IndexFunc(line, unicode.IsSpace)
	if idx < 0 {
		return 0, "", false // no comm field
	}
	ppid, err := strconv.Atoi(line[:idx])
	if err != nil {
		return 0, "", false
	}
	comm = strings.TrimSpace(line[idx:])
	if comm == "" {
		return 0, "", false
	}
	return ppid, comm, true
}

// resolveTTY shells out `ps -o tty= -p <pid>` (the exec seam) and normalizes
// via parsePsTTY. A probe failure yields "" (absent tty), same as a headless
// session — the caller treats an empty tty as "not actuatable in place," and
// a transient ps failure degrades to that safely.
func resolveTTY(pid int) string {
	ctx, cancel := context.WithTimeout(context.Background(), procTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", "tty=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return parsePsTTY(string(out))
}

// parsePsTTY normalizes `ps -o tty= -p <pid>` output: the raw device name
// (e.g. "ttys002") for a session with a controlling terminal, or "" for one
// without ("??", the ps sentinel for a piped/headless session).
func parsePsTTY(out string) string {
	tty := strings.TrimSpace(out)
	if tty == noTTY {
		return ""
	}
	return tty
}
