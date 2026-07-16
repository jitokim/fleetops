// Package sessions persists a session-identity registry — one record per
// live Claude Code session keyed by its session_id, written by the
// SessionStart hook (missionctl hook session-start) and removed by the
// SessionEnd hook (missionctl hook session-end). Each record answers "which
// real process/tty/cwd is this session_id," so a later actuation pass can
// dispatch by tty (session-unique) instead of by cwd (many-to-one). See
// docs/adr-vendor-independent-actuation.md §2.1.
//
// This is deliberately NOT internal/registry: that package persists
// goal-bound loop CONTRACTS from the "n"-key spawn wizard (why a loop
// exists, what "done" means). This one persists session IDENTITY (where a
// session physically lives). Same on-disk-marker idiom as internal/gate,
// same "best-effort, swallow errors, never break the user's session"
// discipline — nothing here is on a critical path.
package sessions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SessionsDir is ~/.missionctl/sessions (override for tests by passing an
// explicit dir to the funcs below instead of calling this — same pattern as
// gate.GatesDir / registry.LoopsDir).
func SessionsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".missionctl", "sessions")
}

// SessionEntry is one session's identity record. TTY is empty for a session
// with no controlling terminal (a piped/headless `-p` run — expected, not an
// error). PID is the resolved `claude` process pid; a later liveness check
// (Phase 2) re-validates tty↔pid against live ps at actuation time rather
// than trusting this possibly-stale record, since ttys are OS-recycled.
type SessionEntry struct {
	PID            int       `json:"pid"`
	TTY            string    `json:"tty"`
	Cwd            string    `json:"cwd"`
	TranscriptPath string    `json:"transcript_path"`
	Source         string    `json:"source"`
	StartedAt      time.Time `json:"started_at"`
}

// WriteSession records sessionID's identity entry. Called from the
// SessionStart hook — must be fast; its error is only ever used by the hook
// to decide what to log, and the hook itself always exits 0 regardless (see
// cmd/missionctl's hook subcommand).
func WriteSession(dir, sessionID string, entry SessionEntry) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, sessionID+".json"), data, 0o644)
}

// ReadSession loads sessionID's entry. A missing or malformed file is an
// error (unlike ListSessions, which skips them) — callers that want
// best-effort iteration should use ListSessions instead.
func ReadSession(dir, sessionID string) (SessionEntry, error) {
	var entry SessionEntry
	data, err := os.ReadFile(filepath.Join(dir, sessionID+".json"))
	if err != nil {
		return SessionEntry{}, err
	}
	if err := json.Unmarshal(data, &entry); err != nil {
		return SessionEntry{}, err
	}
	return entry, nil
}

// ListSessions reads every entry in dir into sessionID → SessionEntry.
// Unreadable or malformed files are skipped, not an error — this is a
// best-effort registry, not a critical path (mirrors gate.Pending).
func ListSessions(dir string) map[string]SessionEntry {
	out := make(map[string]SessionEntry)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var entry SessionEntry
		if err := json.Unmarshal(data, &entry); err != nil {
			continue
		}
		sessionID := strings.TrimSuffix(e.Name(), ".json")
		out[sessionID] = entry
	}
	return out
}

// DeleteSession best-effort removes sessionID's entry, called from the
// SessionEnd hook. A missing entry is a no-op, NOT an error — SessionEnd may
// fire for a session that never got a SessionStart record, or after a stale
// entry was already pruned (matches gate's CAS-delete tolerance). Any other
// os.Remove error is returned so the hook can log it (the hook still exits 0).
func DeleteSession(dir, sessionID string) error {
	err := os.Remove(filepath.Join(dir, sessionID+".json"))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
