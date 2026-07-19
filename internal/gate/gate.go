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

	// PromptID is Claude Code's prompt_id, the key that identifies WHICH
	// prompt a marker describes. Measured 2026-07-20: both the
	// PermissionRequest and Notification hooks carry it and their values
	// match for the same gate. That is what makes merging possible at all —
	// without it the two writers cannot tell "same prompt, worse payload"
	// from "a genuinely new prompt". Empty on claude versions that omit it.
	PromptID string

	// Tool and ToolDetail describe what the session is asking permission to
	// DO — only PermissionRequest supplies them. The Notification hook's
	// message for the same gate is the useless generic "Claude needs your
	// permission", which is precisely why these exist.
	Tool       string
	ToolDetail string
}

// Detail returns the most informative description available for this gate.
//
// Falls back deliberately rather than composing: a marker written by the
// Notification hook alone has only the generic message, and dressing that up
// as though it named a tool would be a claim the payload does not support.
func (i Info) Detail() string {
	switch {
	case i.Tool != "" && i.ToolDetail != "":
		return i.Tool + ": " + i.ToolDetail
	case i.Tool != "":
		return i.Tool
	default:
		return i.Message
	}
}

// isRicherThan reports whether i carries strictly more about the gate than
// other does. Only the tool identity counts: it is the field the two hooks
// actually differ on, and the one whose loss the merge rule exists to
// prevent.
func (i Info) isRicherThan(other Info) bool {
	return i.Tool != "" && other.Tool == ""
}

// markerFile is Info's on-disk JSON shape. TS is unix NANOSECONDS (see
// normalizeTSNanos for backward compat with markers written before this
// migration, when TS was unix seconds) — nanosecond precision is what lets
// DeleteMarkerIfTS's compare-and-swap actually distinguish two markers that
// happen to land within the same second (a gate answered and immediately
// replaced by a fresh one), which whole-second TS could not.
type markerFile struct {
	Message    string `json:"message"`
	Type       string `json:"type"`
	TS         int64  `json:"ts"`
	PromptID   string `json:"prompt_id,omitempty"`
	Tool       string `json:"tool,omitempty"`
	ToolDetail string `json:"tool_detail,omitempty"`
}

// WriteMarker records that sessionID is waiting on a human decision. Called
// from the hook path on every notification and permission request — must be
// fast, and its error is only ever used by the hook to decide what to log;
// the hook itself always exits 0 regardless (see cmd/fleetops's hook
// subcommand). Info.Type is Claude Code's notification_type field verbatim
// (may be empty on older claude versions) — see Pending's caller (the
// scanner) for how it disambiguates a real gate from the 60s idle nudge.
//
// TWO hooks write here for a SINGLE gate. Measured 2026-07-20:
//
//	+0.00s  PermissionRequest  → tool_name + tool_input (what is being asked)
//	+6.01s  Notification       → "Claude needs your permission" (generic)
//
// Markers are keyed by session id, so a plain write would let the generic
// payload land second and erase the useful one — the feature would work for
// six seconds and then silently degrade. mergeMarker prevents that; see its
// doc for the rules and for what happens when the correlation key is absent.
func WriteMarker(dir, sessionID string, in Info) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, sessionID+".json")
	in.TS = time.Now()

	merged, write := mergeMarker(readMarker(path), in)
	if !write {
		return nil
	}
	data, err := json.Marshal(markerFile{
		Message:    merged.Message,
		Type:       merged.Type,
		TS:         merged.TS.UnixNano(),
		PromptID:   merged.PromptID,
		Tool:       merged.Tool,
		ToolDetail: merged.ToolDetail,
	})
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// mergeMarker decides what a new payload does to the marker already on disk.
// Returns the marker to persist and whether a write is needed at all.
//
// Rules, in order:
//
//  1. No existing marker → take the new one.
//  2. Different prompt_id → a genuinely NEW gate. Replace wholesale, new TS.
//  3. Same prompt_id, existing is richer → keep existing, DO NOT WRITE. This
//     is the measured case: the generic Notification arriving 6s after the
//     detailed PermissionRequest.
//  4. Same prompt_id, new is richer → upgrade the detail but KEEP THE
//     EXISTING TS.
//
// Rules 3 and 4 both preserving the original TS is load-bearing, not tidiness.
// TS means "when this gate was first observed", and DeleteMarkerIfTS uses it
// as a compare-and-swap token so a caller cannot delete a marker that changed
// under it. If a merge bumped the TS, a decision made moments earlier would
// hold a token that no longer matches and the approve path would silently
// stop being able to clear the gate it just answered. Holding TS still makes
// the merge invisible to the CAS — the two writers look like one.
//
// When prompt_id is absent on either side (older claude), correlation is
// impossible and the newer payload wins, which is the pre-merge behavior.
// That can still lose detail; it is preferred over the alternative, where a
// stale detailed marker outlives its gate forever because nothing can prove
// it is stale.
func mergeMarker(existing Info, in Info) (Info, bool) {
	if existing.TS.IsZero() {
		return in, true
	}
	if existing.PromptID == "" || in.PromptID == "" || existing.PromptID != in.PromptID {
		return in, true
	}
	if existing.isRicherThan(in) {
		return existing, false
	}
	in.TS = existing.TS
	return in, true
}

// readMarker returns the marker at path, or a zero Info if it is missing or
// unreadable. Missing is the common case (first gate of a session), and a
// corrupt file must not be able to block a new gate from being recorded — so
// both degrade to "nothing was there", which mergeMarker treats as rule 1.
func readMarker(path string) Info {
	data, err := os.ReadFile(path)
	if err != nil {
		return Info{}
	}
	var m markerFile
	if err := json.Unmarshal(data, &m); err != nil {
		return Info{}
	}
	return infoFrom(m)
}

// infoFrom converts the on-disk shape to Info, applying the legacy-TS
// upgrade. Shared by readMarker and Pending so the two cannot drift.
func infoFrom(m markerFile) Info {
	return Info{
		Message:    m.Message,
		Type:       m.Type,
		TS:         time.Unix(0, normalizeTSNanos(m.TS)),
		PromptID:   m.PromptID,
		Tool:       m.Tool,
		ToolDetail: m.ToolDetail,
	}
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
		pending[sessionID] = infoFrom(m)
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
