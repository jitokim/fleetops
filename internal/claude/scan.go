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
		loops = append(loops, loopFromLog(path, fi, now))
	}
	sort.Slice(loops, func(i, j int) bool {
		return loops[i].LastActivity.After(loops[j].LastActivity)
	})
	return loops, nil
}

// isHiddenProjectDir reports whether an encoded project dir contains a
// dot-prefixed path segment (see IncludeHidden): "/" and "." both encode to
// "-", so a hidden dir shows up as a double dash.
func isHiddenProjectDir(dir string) bool {
	return strings.Contains(dir, "--")
}

func loopFromLog(path string, fi os.FileInfo, now time.Time) domain.Loop {
	projectDir := filepath.Base(filepath.Dir(path))
	proj := projectLabel(projectDir)
	session := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	last := fi.ModTime()
	idle := now.Sub(last) >= IdleThreshold

	l := domain.Loop{
		ID:           session,
		Name:         proj,
		Project:      proj,
		ProjectDir:   projectDir,
		Cwd:          decodeCwd(projectDir),
		SessionID:    session,
		Path:         path,
		LastActivity: last,
		State:        domain.StateRunning,
	}

	if idle {
		l.State, l.Stall = tailState(path)
	}
	return l
}

// tailState reads the tail of the session log ONCE and classifies why an
// idle loop went quiet:
//   - a rate-limit marker anywhere in the tail ⇒ StateStalled/StallRateLimit
//     (a 429 means the turn did NOT complete, so this takes precedence over
//     end_turn below even if both somehow appear in the tail)
//   - otherwise, if the last user/assistant entry is an assistant message
//     that finished its turn (stop_reason "end_turn") ⇒ StateIdle: the agent
//     is done and waiting on a human, not stuck — not an incident
//   - otherwise (mid-work: last entry is user/tool_result, or an assistant
//     message that hasn't finished, e.g. tool_use) ⇒ StateStalled/StallNoOutput
func tailState(path string) (domain.LoopState, domain.StallKind) {
	buf, ok := readTail(path, tailBytes)
	if !ok {
		return domain.StateStalled, domain.StallNoOutput
	}
	if hasRateLimitMarker(buf) {
		return domain.StateStalled, domain.StallRateLimit
	}
	if lastTurnEnded(buf) {
		return domain.StateIdle, domain.StallNone
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
