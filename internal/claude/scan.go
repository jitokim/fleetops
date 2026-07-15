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
	return applyLiveness(loops, LiveClaudeCwds()), nil
}

// applyLiveness cross-checks each loop against live `claude` CLI processes
// in its cwd — the JSONL alone can't tell "waiting for human" (idle) from
// "process dead" (terminal closed/crashed): both just stop writing. loops
// must already be sorted by LastActivity desc (as DiscoverLoops does), so
// within any cwd the earliest-indexed entries are the most recently active
// ones — no extra sort needed here.
//
// Per cwd, the `live` count of most-recently-active loops are left
// untouched (there's a real process behind them). The rest are presumed
// dead:
//   - StateIdle (finished its turn, then the process went away) → dropped
//     entirely: the loop ended cleanly, it's not part of the fleet anymore.
//   - anything else (StateStalled, or StateRunning past the live count —
//     e.g. a process that just died mid-turn) → kept, reclassified
//     StateStalled/StallGone: a mid-work death IS an incident.
func applyLiveness(loops []domain.Loop, live map[string]int) []domain.Loop {
	byCwd := make(map[string][]int)
	for i, l := range loops {
		byCwd[l.Cwd] = append(byCwd[l.Cwd], i)
	}

	drop := make(map[int]bool, len(loops))
	for cwd, idxs := range byCwd {
		k := live[cwd]
		if k >= len(idxs) {
			continue // enough live processes for every loop sharing this cwd
		}
		for _, i := range idxs[k:] {
			if loops[i].State == domain.StateIdle {
				drop[i] = true
				continue
			}
			loops[i].State = domain.StateStalled
			loops[i].Stall = domain.StallGone
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
	// the human is being asked something RIGHT NOW, which is a stronger,
	// more direct signal than anything inferred from the transcript.
	if info, ok := pending[session]; ok {
		if gate.IsGateActive(info.TS, last) {
			l.State = domain.StateGate
			l.Stall = domain.StallNone
			l.GatePrompt = info.Message
		} else {
			gate.DeleteMarker(gatesDir, session) // best-effort: already answered
		}
	}
	return l
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

// lastAssistantTextFromTail is LastAssistantText's buffer-only core.
func lastAssistantTextFromTail(buf []byte) (string, bool) {
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
	return summarizeTailText(last, 120), true
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
