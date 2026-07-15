// Package claude turns Claude Code's own session logs into fleet state — the
// observation core (seed spec §Observe). Each session is a JSONL file under
// ~/.claude/projects/<proj>/<session>.jsonl; we read file mtime (last activity)
// and tail the last few KB for stall markers. No screen scraping.
package claude

import (
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
		loops = append(loops, loopFromLog(path, fi, now))
	}
	sort.Slice(loops, func(i, j int) bool {
		return loops[i].LastActivity.After(loops[j].LastActivity)
	})
	return loops, nil
}

func loopFromLog(path string, fi os.FileInfo, now time.Time) domain.Loop {
	proj := projectLabel(filepath.Base(filepath.Dir(path)))
	session := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	last := fi.ModTime()
	idle := now.Sub(last) >= IdleThreshold

	l := domain.Loop{
		ID:           session,
		Name:         proj,
		Project:      proj,
		SessionID:    session,
		Path:         path,
		LastActivity: last,
		State:        domain.StateRunning,
	}

	if idle {
		l.State = domain.StateStalled
		l.Stall = domain.StallNoOutput
		if tailHasRateLimit(path) {
			l.Stall = domain.StallRateLimit
		}
	}
	return l
}

// tailHasRateLimit reads the last few KB and looks for a recent rate-limit marker.
func tailHasRateLimit(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	start := int64(0)
	if fi.Size() > tailBytes {
		start = fi.Size() - tailBytes
	}
	buf := make([]byte, fi.Size()-start)
	if _, err := f.ReadAt(buf, start); err != nil {
		return false
	}
	s := strings.ToLower(string(buf))
	return strings.Contains(s, "rate limit") ||
		strings.Contains(s, "rate-limit") ||
		strings.Contains(s, "\"status\":429") ||
		strings.Contains(s, "429 ") ||
		strings.Contains(s, "usage limit")
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
