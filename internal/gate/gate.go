// Package gate stores and reads the "human needs to decide" markers written
// by Claude Code's Notification hook (missionctl hook notify) — the
// reliable signal behind the mockup's ◆ GATE state. Screen-scraping an
// alt-screen terminal app (orca) to detect a permission prompt is not
// viable (verified live against the real orca CLI), so the hook is the
// source of truth instead.
package gate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// staleSlack absorbs the small gap between the hook firing and the
// transcript write that follows it (ordering between the two isn't
// perfectly atomic) — see IsGateActive.
const staleSlack = 2 * time.Second

// GatesDir is ~/.missionctl/gates (override for tests by passing an
// explicit dir to the funcs below instead of calling this).
func GatesDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".missionctl", "gates")
}

// Info is one pending gate marker.
type Info struct {
	Message string
	Type    string // Claude Code's notification_type, e.g. "permission_prompt"; "" on older claude versions that don't send it
	TS      time.Time
}

// markerFile is Info's on-disk JSON shape.
type markerFile struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	TS      int64  `json:"ts"`
}

// WriteMarker records that sessionID is waiting on a human decision. Called
// from the Notification hook on every notification — must be fast, and its
// error is only ever used by the hook to decide what to log; the hook
// itself always exits 0 regardless (see cmd/missionctl's hook subcommand).
// notificationType is Claude Code's notification_type field verbatim (may
// be empty on older claude versions) — see Pending's caller (the scanner)
// for how it disambiguates a real gate from the 60s idle nudge.
func WriteMarker(dir, sessionID, message, notificationType string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(markerFile{Message: message, Type: notificationType, TS: time.Now().Unix()})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, sessionID+".json"), data, 0o644)
}

// Pending reads every marker file in dir into sessionID → Info. Unreadable
// or malformed files are skipped, not an error — this is a best-effort
// signal, not a critical path.
func Pending(dir string) map[string]Info {
	pending := make(map[string]Info)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return pending
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var m markerFile
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		sessionID := strings.TrimSuffix(e.Name(), ".json")
		pending[sessionID] = Info{Message: m.Message, Type: m.Type, TS: time.Unix(m.TS, 0)}
	}
	return pending
}

// DeleteMarker best-effort removes a marker (stale, or answered via
// approveCmd) — errors are swallowed, a leftover file is harmless (it'll be
// judged stale and cleaned up on the next scan anyway).
func DeleteMarker(dir, sessionID string) {
	_ = os.Remove(filepath.Join(dir, sessionID+".json"))
}

// IsGateActive reports whether a gate marker is still live relative to the
// session log's last write. The gate fired at markerTS; if the log's mtime
// is more than staleSlack after that, new transcript entries were written
// AFTER the gate fired — the human must have already answered (claude
// resumed writing), so the marker is stale.
func IsGateActive(markerTS, logMtime time.Time) bool {
	return !logMtime.After(markerTS.Add(staleSlack))
}
