// Package claude turns Claude Code's own session logs into fleet state — the
// observation core (seed spec §Observe). Each session is a JSONL file under
// ~/.claude/projects/<proj>/<session>.jsonl; we read file mtime (last activity)
// and tail the last few KB for stall markers. No screen scraping.
package claude

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jitokim/missionctl/internal/domain"
	"github.com/jitokim/missionctl/internal/gate"
	"github.com/jitokim/missionctl/internal/registry"
)

// IdleThreshold: no log write for this long ⇒ the loop is considered stuck.
var IdleThreshold = 4 * time.Minute

const tailBytes = 24 * 1024

// ProjectsDir is ~/.claude/projects (override for tests).
func ProjectsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

// ActiveWindow: only sessions written within this window are part of "the fleet".
// Long-running loops keep writing (so they stay in); old finished sessions fall out.
var ActiveWindow = 24 * time.Hour

// IncludeHidden: when false (default), sessions whose project dir encodes a
// hidden (dot-prefixed) path segment are filtered out. Claude Code encodes
// both "/" and "." as "-", so a dot-dir doubles up a dash, e.g.
// "/Users/imac/.claude-mem/observer/sessions" → "-Users-imac--claude-mem-observer-sessions".
// Those are headless/infra sessions (agent tooling like claude-mem's
// observer), not a human's loop, and otherwise drown out the real fleet.
// A future flag can flip this to see them.
var IncludeHidden = false

// DiscoverLoops scans session logs and derives current fleet state, keeping only
// sessions active within `within` (0 = keep all). Seed spec AC-1 + filter decision:
// "recent activity + not cleanly ended" — the window drops days-old noise.
func DiscoverLoops(now time.Time, within time.Duration) ([]domain.Loop, error) {
	root := ProjectsDir()
	matches, err := filepath.Glob(filepath.Join(root, "*", "*.jsonl"))
	if err != nil {
		return nil, err
	}
	gatesDir := gate.GatesDir()
	pending := gate.Pending(gatesDir)
	loops := make([]domain.Loop, 0, len(matches))
	for _, path := range matches {
		fi, err := os.Stat(path)
		if err != nil || fi.Size() == 0 {
			continue
		}
		if within > 0 && now.Sub(fi.ModTime()) > within {
			continue
		}
		if !IncludeHidden && isHiddenProjectDir(filepath.Base(filepath.Dir(path))) {
			continue
		}
		loops = append(loops, loopFromLog(path, fi, now, gatesDir, pending))
	}
	sort.Slice(loops, func(i, j int) bool {
		return loops[i].LastActivity.After(loops[j].LastActivity)
	})

	loopsDir := registry.LoopsDir()
	registry.BindPending(loopsDir, registry.PendingDir(), loops, now)
	loops = enrichFromRegistry(loops, loopsDir)

	live, liveOK := LiveClaudeCwds()
	loops = applyLiveness(loops, live, liveOK)

	// Keep metricsCache bounded to sessions actually present in this scan —
	// otherwise it grows forever as old sessions age out of the window or
	// get deleted, over a long-running missionctl process.
	keep := make(map[string]bool, len(matches))
	for _, m := range matches {
		keep[m] = true
	}
	pruneMetricsCache(keep)

	return loops, nil
}

// enrichFromRegistry attaches goal-bound metadata (Goal.Text/MaxCycles/
// NoImproveLimit, Last verdict, NoImprove) from the registry to each loop
// that has a record — observed (non-spawned) sessions have none and are
// left untouched (Goal.Text stays "", which the TUI treats as "unbound").
//
// A bound loop whose latest verdict was rendered AT this exact cycle
// (Verdict.AtCycle == Cycle — i.e. nothing has happened since it was
// judged) gets its State promoted to the oracle's conclusion: done →
// StateDone, rejected → StateDrift. "progress" leaves State as already
// classified (idle/running) — real work is happening, there's no terminal
// call to make yet. A verdict from an EARLIER cycle (AtCycle < Cycle) is
// still shown (Last stays populated for the ORACLE column) but does not
// override State — the loop has moved on since that judgment, and it's due
// to be judged again (see the TUI's judge trigger policy).
func enrichFromRegistry(loops []domain.Loop, loopsDir string) []domain.Loop {
	for i := range loops {
		rec, ok := registry.Load(loopsDir, loops[i].SessionID)
		if !ok {
			continue
		}
		loops[i].Goal.Text = rec.Goal
		loops[i].Goal.MaxCycles = rec.MaxCycles
		loops[i].Goal.NoImproveLimit = rec.NoImproveLimit
		loops[i].NoImprove = rec.NoImprove
		loops[i].Last = rec.Verdict

		// A live gate always wins over a stale verdict: the loop is blocked
		// on a human decision RIGHT NOW, which is more urgent and more
		// current than a judgment rendered against an earlier cycle's
		// output. Without this guard, a bound loop that hit a fresh
		// permission prompt after being judged done/rejected would show
		// DONE/DRIFT instead of the ◆ GATE it's actually sitting in.
		if rec.Verdict != nil && rec.Verdict.AtCycle == loops[i].Cycle && loops[i].State != domain.StateGate {
			switch rec.Verdict.Outcome {
			case domain.OutcomeDone:
				loops[i].State = domain.StateDone
				loops[i].Stall = domain.StallNone
			case domain.OutcomeRejected:
				loops[i].State = domain.StateDrift
				loops[i].Stall = domain.StallNone
			}
		}
	}
	return loops
}

// applyLiveness cross-checks each loop against live `claude` CLI processes
// in its cwd — the JSONL alone can't tell "waiting for human" (idle) from
// "process dead" (terminal closed/crashed): both just stop writing. loops
// must already be sorted by LastActivity desc (as DiscoverLoops does), so
// within any cwd the earliest-indexed entries are the most recently active
// ones — no extra sort needed here.
//
// live is keyed by REAL (unencoded) lsof cwd paths (see LiveClaudeCwds), not
// by a loop's lossily-decoded Cwd — decodeCwd can't tell a "-" that was
// originally "/" from one that was originally in the directory name itself
// (e.g. "my-app"), so matching against it would silently miss real
// directories. Instead each live real path is re-encoded with encodeCwd
// (Claude Code's own "/" and "." → "-" scheme) and matched against the
// loop's ProjectDir, which IS that raw encoded string — lossless in this
// direction. ok=false (the ps/lsof probe itself failed) short-circuits to
// "leave the fleet exactly as classified" — see LiveClaudeCwds: a probe
// failure is not evidence of anything, and must never be treated as "0 live
// processes", which would wrongly mark the entire fleet StallGone/dropped.
//
// Per ProjectDir, the live count of most-recently-active loops are left
// untouched (there's a real process behind them). The rest are presumed
// dead:
//   - StateIdle (finished its turn, then the process went away) → dropped
//     entirely: the loop ended cleanly, it's not part of the fleet anymore.
//   - StateDone / StateDrift (the oracle already rendered a verdict this
//     cycle — see enrichFromRegistry) → left alone, dropped or demoted by
//     neither rule: that's the terminal record of a judgment, not an
//     incident, regardless of whether the terminal later closed.
//   - anything else (StateStalled, or StateRunning past the live count —
//     e.g. a process that just died mid-turn) → kept, reclassified
//     StateStalled/StallGone: a mid-work death IS an incident.
//
// Bonus: whenever a ProjectDir has ANY live process backing it (regardless
// of which specific loop in the group that process belongs to), every loop
// sharing that ProjectDir gets its Cwd healed to the confirmed-real lsof
// path (overwriting the lossy decode) and CwdVerified set — the directory
// itself is confirmed real, independent of which loop instance is live.
//
// Collision guard: encodeCwd is many-to-one (both "/" and "." collapse to
// "-"), so two DISTINCT real directories — e.g. /x/foo-bar and /x/foo.bar —
// can map to the SAME ProjectDir. When that happens for a given ProjectDir,
// which real path is "the" real one is genuinely ambiguous, so healing is
// skipped entirely for it: Cwd stays the lossy decode and CwdVerified stays
// false, rather than risk silently healing to the WRONG one of the two.
func applyLiveness(loops []domain.Loop, live map[string]int, ok bool) []domain.Loop {
	if !ok {
		return loops // probe failed — do not reclassify the fleet on no data (P1-2)
	}

	liveCountByProjectDir := make(map[string]int)
	realPathByProjectDir := make(map[string]string)
	collidedProjectDir := make(map[string]bool) // ProjectDir reached from >1 distinct real path
	for realPath, count := range live {
		pd := encodeCwd(realPath)
		liveCountByProjectDir[pd] += count
		if existing, seen := realPathByProjectDir[pd]; seen && existing != realPath {
			collidedProjectDir[pd] = true
		}
		realPathByProjectDir[pd] = realPath
	}

	byProjectDir := make(map[string][]int)
	for i, l := range loops {
		byProjectDir[l.ProjectDir] = append(byProjectDir[l.ProjectDir], i)
	}

	drop := make(map[int]bool, len(loops))
	for pd, idxs := range byProjectDir {
		if realPath, matched := realPathByProjectDir[pd]; matched && !collidedProjectDir[pd] {
			for _, i := range idxs {
				loops[i].Cwd = realPath
				loops[i].CwdVerified = true
			}
		}

		k := liveCountByProjectDir[pd]
		if k >= len(idxs) {
			continue // enough live processes for every loop sharing this dir
		}
		for _, i := range idxs[k:] {
			switch loops[i].State {
			case domain.StateIdle:
				drop[i] = true
			case domain.StateDone, domain.StateDrift:
				// oracle-judged and settled; leave as-is.
			default:
				loops[i].State = domain.StateStalled
				loops[i].Stall = domain.StallGone
			}
		}
	}
	if len(drop) == 0 {
		return loops
	}

	out := make([]domain.Loop, 0, len(loops)-len(drop))
	for i, l := range loops {
		if !drop[i] {
			out = append(out, l)
		}
	}
	return out
}

// isHiddenProjectDir reports whether an encoded project dir contains a
// dot-prefixed path segment (see IncludeHidden): "/" and "." both encode to
// "-", so a hidden dir shows up as a double dash.
func isHiddenProjectDir(dir string) bool {
	return strings.Contains(dir, "--")
}

func loopFromLog(path string, fi os.FileInfo, now time.Time, gatesDir string, pending map[string]gate.Info) domain.Loop {
	projectDir := filepath.Base(filepath.Dir(path))
	proj := projectLabel(projectDir)
	session := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	last := fi.ModTime()
	idleFor := now.Sub(last)

	l := domain.Loop{
		ID:           session,
		Name:         proj,
		Project:      proj,
		ProjectDir:   projectDir,
		Cwd:          decodeCwd(projectDir),
		SessionID:    session,
		Path:         path,
		LastActivity: last,
		State:        domain.StateRunning, // fallback if the tail can't be read at all
	}

	l.Cycle, l.TokensSpent = SessionMetrics(path)
	if l.Goal.BudgetTokens == 0 {
		l.Goal.BudgetTokens = DefaultBudgetTokens // v0 default until per-loop budgets exist
	}

	// One shared tail read serves both classification and the detail pane's
	// TAIL row (LastText) — avoid reading the file twice. Classification
	// always runs (not just once "idle"): "running" means a turn is
	// genuinely in flight, not merely "wrote recently" (see classifyLoop) —
	// a loop that finished its turn a second ago is idle, not running.
	if buf, ok := readTail(path, tailBytes); ok {
		if text, ok := lastAssistantTextFromTail(buf); ok {
			l.LastText = text
		}
		l.State, l.Stall = classifyLoop(buf, idleFor)
	} else if idleFor >= IdleThreshold {
		l.State, l.Stall = domain.StateStalled, domain.StallNoOutput
	}

	// A pending Notification-hook marker beats any tail heuristic above —
	// but only when it's actually asking for a decision. Claude Code fires
	// the SAME hook for the 60s "Claude is waiting for your input" idle
	// notification, which is NOT a gate (verified live). The official
	// notification_type field is the authoritative signal
	// (permission_prompt/elicitation_dialog/agent_needs_input all mean
	// "blocked on a human"; idle_prompt and anything else don't); older
	// claude versions that omit it (Type == "") fall back to the
	// message-contains-"permission" heuristic. Anything that isn't a gate
	// falls through to the normal tail classification above (→ Idle) and
	// the marker is best-effort deleted so it doesn't linger.
	if info, ok := pending[session]; ok {
		if gate.IsGateActive(info.TS, last) && isGateNotification(info) {
			l.State = domain.StateGate
			l.Stall = domain.StallNone
			l.GatePrompt = info.Message
			l.GateTS = info.TS.UnixNano() // lets approveCmd compare-and-swap delete only the marker this decision was based on
		} else {
			// Compare-and-swap: only delete the marker this scan actually
			// judged stale/non-gate. A plain delete-by-name could destroy a
			// BRAND NEW marker that landed between the Pending() snapshot
			// above and this delete (e.g. the human answered, then a fresh
			// permission prompt fired moments later) — see gate.DeleteMarkerIfTS.
			gate.DeleteMarkerIfTS(gatesDir, session, info.TS.UnixNano())
		}
	}
	return l
}

// gateNotificationTypes are Claude Code's notification_type values that mean
// "blocked on a human decision" — the rest (idle_prompt, auth_success, etc.)
// are informational, not a gate.
var gateNotificationTypes = map[string]bool{
	"permission_prompt":  true,
	"elicitation_dialog": true,
	"agent_needs_input":  true,
}

// isGateNotification decides whether a marker represents a real gate.
// Type is authoritative when present; when empty (older claude versions
// that predate notification_type), falls back to a message-text heuristic.
func isGateNotification(info gate.Info) bool {
	if info.Type != "" {
		return gateNotificationTypes[info.Type]
	}
	return strings.Contains(strings.ToLower(info.Message), "permission")
}

// tailState reads the tail of the session log and classifies it given how
// long it's been since the last write (see classifyLoop). Exposed for
// tests; loopFromLog itself calls classifyLoop directly since it already
// holds the tail buffer from the LastText read (avoids a second file read).
func tailState(path string, idleFor time.Duration) (domain.LoopState, domain.StallKind) {
	buf, ok := readTail(path, tailBytes)
	if !ok {
		return domain.StateStalled, domain.StallNoOutput
	}
	return classifyLoop(buf, idleFor)
}

// classifyLoop is tailState's buffer-only core. "Running" means "a turn is
// in flight", not just "the log was touched recently", so a finished turn
// is idle regardless of how long ago that was:
//   - the last meaningful (user/assistant) entry is an assistant message
//     whose turn finished (stop_reason "end_turn") ⇒ StateIdle: waiting on
//     a human, not stuck — not an incident, no matter the recency.
//   - otherwise (mid-turn: last entry is user/tool_result, or an assistant
//     message that hasn't finished, e.g. tool_use):
//   - idleFor < IdleThreshold ⇒ StateRunning: genuinely still working.
//   - idleFor >= IdleThreshold ⇒ StateStalled (a rate-limit marker
//     anywhere in the tail ⇒ StallRateLimit, else StallNoOutput).
func classifyLoop(buf []byte, idleFor time.Duration) (domain.LoopState, domain.StallKind) {
	if lastTurnEnded(buf) {
		return domain.StateIdle, domain.StallNone
	}
	if idleFor < IdleThreshold {
		return domain.StateRunning, domain.StallNone
	}
	if hasRateLimitMarker(buf) {
		return domain.StateStalled, domain.StallRateLimit
	}
	return domain.StateStalled, domain.StallNoOutput
}

// readTail reads the last n bytes of path (or the whole file if smaller).
func readTail(path string, n int64) ([]byte, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, false
	}
	start := int64(0)
	if fi.Size() > n {
		start = fi.Size() - n
	}
	buf := make([]byte, fi.Size()-start)
	if _, err := f.ReadAt(buf, start); err != nil {
		return nil, false
	}
	return buf, true
}

// hasRateLimitMarker looks for a recent rate-limit marker in the tail.
func hasRateLimitMarker(buf []byte) bool {
	s := strings.ToLower(string(buf))
	return strings.Contains(s, "rate limit") ||
		strings.Contains(s, "rate-limit") ||
		strings.Contains(s, "\"status\":429") ||
		strings.Contains(s, "429 ") ||
		strings.Contains(s, "usage limit")
}

// lastTurnEnded reports whether the last parseable user/assistant entry in
// the tail is an assistant message whose turn finished (stop_reason
// "end_turn"). A possibly-truncated first line in the tail buffer simply
// fails to parse and is skipped, same tolerance as LastUserPrompt.
func lastTurnEnded(buf []byte) bool {
	var last map[string]any
	for _, line := range strings.Split(string(buf), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if t, _ := entry["type"].(string); t == "user" || t == "assistant" {
			last = entry
		}
	}
	if last == nil || last["type"] != "assistant" {
		return false
	}
	msg, ok := last["message"].(map[string]any)
	if !ok {
		return false
	}
	stopReason, _ := msg["stop_reason"].(string)
	return stopReason == "end_turn"
}

// projectLabel turns "-Users-imac-IdeaProjects-aboard" into "aboard".
func projectLabel(dir string) string {
	parts := strings.Split(strings.Trim(dir, "-"), "-")
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i] != "" {
			return parts[i]
		}
	}
	return dir
}

// decodeCwd best-effort reverses the "/" → "-" project-dir encoding, for
// display only. Lossy when a path segment itself contains "-"; ProjectDir
// (the raw encoded string) is the source of truth for matching, see
// internal/control.
func decodeCwd(dir string) string {
	return "/" + strings.ReplaceAll(strings.TrimPrefix(dir, "-"), "-", "/")
}

// encodeCwd applies Claude Code's own project-dir encoding to a real
// (unencoded) absolute path — both "/" AND "." become "-" (verified:
// "/Users/imac/.claude-mem/observer-sessions" →
// "-Users-imac--claude-mem-observer-sessions"). This is the lossless
// direction (unlike decodeCwd): encoding a known-real path can be compared
// exactly against a loop's ProjectDir, which is why applyLiveness uses this
// instead of decoding ProjectDir and fuzzy-matching against a live path.
func encodeCwd(realPath string) string {
	encoded := strings.ReplaceAll(realPath, "/", "-")
	return strings.ReplaceAll(encoded, ".", "-")
}

// LastUserPrompt returns the text of the last user message in a Claude Code
// session log, for re-sending on resume (DESIGN.md: resume re-drives the
// loop rather than restarting it). ok is false if the file has no user
// message (or can't be read).
func LastUserPrompt(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)

	last := ""
	found := false
	for scanner.Scan() {
		var entry map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if entry["type"] != "user" {
			continue
		}
		if text, ok := userMessageText(entry); ok && text != "" {
			last = text
			found = true
		}
	}
	return last, found
}

// userMessageText extracts the text of a user transcript entry's
// message.content, which is either a plain string or an array of content
// blocks (text blocks have "type":"text").
func userMessageText(entry map[string]any) (string, bool) {
	msg, ok := entry["message"].(map[string]any)
	if !ok {
		return "", false
	}
	switch content := msg["content"].(type) {
	case string:
		return content, content != ""
	case []any:
		for _, block := range content {
			b, ok := block.(map[string]any)
			if !ok {
				continue
			}
			if b["type"] != "text" {
				continue
			}
			if text, ok := b["text"].(string); ok && text != "" {
				return text, true
			}
		}
	}
	return "", false
}

// LastAssistantText returns the last assistant message's text (first 120
// chars, newlines collapsed to spaces) from the tail of the session log —
// "what was it last doing", shown in the detail pane's TAIL row. ok is false
// if the tail has no assistant text. Thin path-based wrapper around
// lastAssistantTextFromTail, which loopFromLog calls directly against a tail
// buffer it already read (see readTail).
func LastAssistantText(path string) (string, bool) {
	buf, ok := readTail(path, tailBytes)
	if !ok {
		return "", false
	}
	return lastAssistantTextFromTail(buf)
}

// lastAssistantTextFromTail is LastAssistantText's buffer-only core: finds
// the raw text, then caps it to 120 chars for the TAIL row.
func lastAssistantTextFromTail(buf []byte) (string, bool) {
	text, ok := lastAssistantTextRawFromTail(buf)
	if !ok {
		return "", false
	}
	return summarizeTailText(text, 120), true
}

// LastAssistantTextFull returns the last assistant message's RAW text from
// the tail of the session log — uncapped, unlike LastAssistantText (which
// caps at 120 chars for the TUI's TAIL row). The oracle (internal/oracle)
// needs the full report to judge accurately; a 120-char summary would
// throw away exactly the evidence it's supposed to check.
func LastAssistantTextFull(path string) (string, bool) {
	buf, ok := readTail(path, tailBytes)
	if !ok {
		return "", false
	}
	return lastAssistantTextRawFromTail(buf)
}

// lastAssistantTextRawFromTail is the shared, uncapped core of both
// lastAssistantTextFromTail and LastAssistantTextFull.
func lastAssistantTextRawFromTail(buf []byte) (string, bool) {
	last := ""
	found := false
	for _, line := range strings.Split(string(buf), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if t, _ := entry["type"].(string); t != "assistant" {
			continue
		}
		if text, ok := assistantMessageText(entry); ok && text != "" {
			last = text
			found = true
		}
	}
	if !found {
		return "", false
	}
	return last, true
}

// assistantMessageText mirrors userMessageText for an assistant entry:
// message.content is either a plain string or an array of blocks (text
// blocks have "type":"text"; tool_use blocks are skipped — not useful as a
// one-line summary of "what it was doing").
func assistantMessageText(entry map[string]any) (string, bool) {
	msg, ok := entry["message"].(map[string]any)
	if !ok {
		return "", false
	}
	switch content := msg["content"].(type) {
	case string:
		return content, content != ""
	case []any:
		for _, block := range content {
			b, ok := block.(map[string]any)
			if !ok {
				continue
			}
			if b["type"] != "text" {
				continue
			}
			if text, ok := b["text"].(string); ok && text != "" {
				return text, true
			}
		}
	}
	return "", false
}

// summarizeTailText collapses newlines to spaces and caps length for a
// one-line TAIL row.
func summarizeTailText(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
