// Package gate stores and reads the "human needs to decide" markers written
// by Claude Code's Notification hook (fleetops hook notify) — the
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

// legacySecondsThreshold distinguishes a legacy on-disk TS (unix SECONDS,
// written before markers moved to nanosecond precision) from a current
// nanosecond-scale one: any real UnixNano timestamp for a remotely current
// date is many orders of magnitude larger than this, while a legacy
// unix-seconds TS is far below it. Lets old marker files on disk keep
// working across the migration instead of being silently misinterpreted.
const legacySecondsThreshold = 1_000_000_000_000 // 1e12

// normalizeTSNanos upgrades a possibly-legacy on-disk TS to unix
// nanoseconds — see legacySecondsThreshold.
func normalizeTSNanos(ts int64) int64 {
	if ts != 0 && ts < legacySecondsThreshold {
		return ts * int64(time.Second)
	}
	return ts
}

// GatesDir is ~/.fleetops/gates (override for tests by passing an
// explicit dir to the funcs below instead of calling this).
func GatesDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".fleetops", "gates")
}

// Info is one pending gate marker.
type Info struct {
	Message string
	Type    string // Claude Code's notification_type, e.g. "permission_prompt"; "" on older claude versions that don't send it
	TS      time.Time
}

// markerFile is Info's on-disk JSON shape. TS is unix NANOSECONDS (see
// normalizeTSNanos for backward compat with markers written before this
// migration, when TS was unix seconds) — nanosecond precision is what lets
// DeleteMarkerIfTS's compare-and-swap actually distinguish two markers that
// happen to land within the same second (a gate answered and immediately
// replaced by a fresh one), which whole-second TS could not.
type markerFile struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	TS      int64  `json:"ts"`
}

// WriteMarker records that sessionID is waiting on a human decision. Called
// from the Notification hook on every notification — must be fast, and its
// error is only ever used by the hook to decide what to log; the hook
// itself always exits 0 regardless (see cmd/fleetops's hook subcommand).
// notificationType is Claude Code's notification_type field verbatim (may
// be empty on older claude versions) — see Pending's caller (the scanner)
// for how it disambiguates a real gate from the 60s idle nudge.
func WriteMarker(dir, sessionID, message, notificationType string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(markerFile{Message: message, Type: notificationType, TS: time.Now().UnixNano()})
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
		pending[sessionID] = Info{Message: m.Message, Type: m.Type, TS: time.Unix(0, normalizeTSNanos(m.TS))}
	}
	return pending
}

// DeleteMarker best-effort removes a marker (stale, or answered via
// approveCmd) — errors are swallowed, a leftover file is harmless (it'll be
// judged stale and cleaned up on the next scan anyway).
func DeleteMarker(dir, sessionID string) {
	_ = os.Remove(filepath.Join(dir, sessionID+".json"))
}

// DeleteMarkerIfTS deletes sessionID's marker ONLY if its current on-disk TS
// (unix NANOSECONDS — pass Info.TS.UnixNano() / domain.Loop.GateTS, both of
// which are nanosecond-scale) still equals ts — a compare-and-swap guard.
// Without it, a caller that decided "this marker is stale/answered" based on
// a snapshot taken moments ago could delete a BRAND NEW marker that arrived
// in the meantime (a fresh permission prompt right after the old one was
// answered) — the human would lose that gate notification with no sign
// anything was wrong. Nanosecond precision (rather than the old whole-second
// TS) is what actually closes that window: two markers landing within the
// same second used to be indistinguishable by TS, so a stale-delete could
// still destroy a fresh one that arrived in the same second as the old one
// — see the "same-second, different-nanosecond" test. Returns true if a
// matching marker was found and deleted.
func DeleteMarkerIfTS(dir, sessionID string, ts int64) bool {
	path := filepath.Join(dir, sessionID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var m markerFile
	if err := json.Unmarshal(data, &m); err != nil {
		return false
	}
	if normalizeTSNanos(m.TS) != ts {
		return false
	}
	return os.Remove(path) == nil
}

// IsGateActive reports whether a gate marker is still live relative to the
// session log's last write. The gate fired at markerTS; if the log's mtime
// is more than staleSlack after that, new transcript entries were written
// AFTER the gate fired — the human must have already answered (claude
// resumed writing), so the marker is stale. markerTS now carries real
// nanosecond resolution (see markerFile's doc) rather than being truncated
// to whole seconds; staleSlack's 2s window is compared via ordinary
// time.Duration arithmetic, which is nanosecond-precision throughout, so no
// change was needed here beyond the more precise input.
func IsGateActive(markerTS, logMtime time.Time) bool {
	return !logMtime.After(markerTS.Add(staleSlack))
}
