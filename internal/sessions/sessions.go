// Package sessions persists a session-identity registry — one record per
// live Claude Code session keyed by its session_id, written by the
// SessionStart hook (fleetops hook session-start) and removed by the
// SessionEnd hook (fleetops hook session-end). Each record answers "which
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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jitokim/fleetops/internal/fsatomic"
)

// SessionsDir is ~/.fleetops/sessions (override for tests by passing an
// explicit dir to the funcs below instead of calling this — same pattern as
// gate.GatesDir / registry.LoopsDir).
func SessionsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".fleetops", "sessions")
}

// SessionEntry is one session's identity record. TTY is empty for a session
// with no controlling terminal (a piped/headless `-p` run — expected, not an
// error). PID is the resolved `claude` process pid; a later liveness check
// (Phase 2) re-validates tty↔pid against live ps at actuation time rather
// than trusting this possibly-stale record, since ttys are OS-recycled.
//
// HostApp/WindowID/SocketPath are additive, omitempty extensions (design
// §2). They are BACK-COMPATIBLE by construction: json.Unmarshal leaves them at
// their zero value for a record written by an older binary (no field present),
// and json.Marshal omits an empty one — so a tty-only record still round-trips,
// and a new binary reading it simply sees empty strings (→ no focus adapter,
// attach degrades to the manual hint). No migration, no version field.
type SessionEntry struct {
	PID            int       `json:"pid"`
	TTY            string    `json:"tty"`
	Cwd            string    `json:"cwd"`
	TranscriptPath string    `json:"transcript_path"`
	Source         string    `json:"source"`
	StartedAt      time.Time `json:"started_at"`
	// HostApp is the host terminal application's $TERM_PROGRAM value
	// ("iTerm.app", "tmux", …), inherited by the SessionStart hook from the
	// user's shell. It keys the FocusAdapter that raises this session's
	// window at attach time (internal/control §4); "" ⇒ no adapter ⇒ degrade.
	HostApp string `json:"host_app,omitempty"`
	// WindowID identifies the host's window/tab/pane — the first non-empty of
	// $ITERM_SESSION_ID, $TMUX_PANE, … A FocusAdapter needs it to select the
	// exact surface; "" ⇒ attach falls through to the cwd-based resolver.
	WindowID string `json:"window_id,omitempty"`
	// SocketPath is reserved for the (out-of-scope here) session-agent control
	// channel at ~/.fleetops/sessions/<id>.sock. Declared now for forward
	// compatibility only — this slice never populates or reads it.
	SocketPath string `json:"socket_path,omitempty"`
}

// validSessionID rejects anything that isn't a plain, single-component
// filename — session_id arrives from a Claude Code hook payload (external
// input; a malformed or crafted payload could in principle reach this via
// `hook session-start`/`hook session-end`'s stdin) and is joined directly
// into a filesystem path below. A real session_id is always a UUID, but a
// value containing a path separator (e.g. "../canary") would let
// filepath.Join escape SessionsDir entirely — filepath.Base(id) != id
// catches exactly that (any "/" makes Base return a shorter suffix); a bare
// "." or ".." both pass through as harmless literal filenames once ".json"
// is appended ("..json"/"...json"), so no extra special-casing is needed.
func validSessionID(id string) bool {
	return id != "" && filepath.Base(id) == id
}

// WriteSession records sessionID's identity entry. Called from the
// SessionStart hook — must be fast; its error is only ever used by the hook
// to decide what to log, and the hook itself always exits 0 regardless (see
// cmd/fleetops's hook subcommand).
func WriteSession(dir, sessionID string, entry SessionEntry) error {
	if !validSessionID(sessionID) {
		return fmt.Errorf("sessions: invalid session id %q", sessionID)
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	return fsatomic.WriteFile(filepath.Join(dir, sessionID+".json"), data, sessionTmpPrefix)
}

// sessionTmpPrefix names WriteSession's sibling temp file, so a stray temp
// surviving a hard kill is identifiable as this registry's.
//
// The write goes through fsatomic (temp + rename) rather than a bare
// os.WriteFile because the record carries several fields now and a resume can
// rewrite it in rapid succession — a torn record would cost the loop its
// host_app/window_id and silently degrade attach. internal/hidden shares the
// same writer for the same reason.
const sessionTmpPrefix = ".session-*.tmp"

// ReadSession loads sessionID's entry. A missing or malformed file is an
// error (unlike ListSessions, which skips them) — callers that want
// best-effort iteration should use ListSessions instead.
func ReadSession(dir, sessionID string) (SessionEntry, error) {
	if !validSessionID(sessionID) {
		return SessionEntry{}, fmt.Errorf("sessions: invalid session id %q", sessionID)
	}
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
// An invalid session id (see validSessionID) is likewise a no-op, not an
// error — same tolerant posture as a missing file, and it means a crafted
// session_id can't be used as an arbitrary-file-delete primitive.
func DeleteSession(dir, sessionID string) error {
	if !validSessionID(sessionID) {
		return nil
	}
	err := os.Remove(filepath.Join(dir, sessionID+".json"))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
