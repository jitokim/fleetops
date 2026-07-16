// Package events is the fleet's append-only history log — the keystone of
// the event-log-and-notify slice. Every emitter (the scanner's state-
// transition detector, the TUI's actuation commands, the oracle's verdicts,
// the governor's promotions) writes here, one JSONL file per loop session
// under ~/.missionctl/history/<session_id>.jsonl — same one-file-per-entity,
// dir-override-for-tests idiom as internal/gate/internal/registry/
// internal/sessions.
//
// This is purely additive, diagnostic-only history — never a source of
// truth for fleet state (that's still the live scan, internal/claude), and
// never on a critical path: every Append call site in this codebase
// swallows its error (a full disk or a permissions problem must not
// interrupt the fleet loop it's merely recording).
package events

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// HistoryDir is ~/.missionctl/history (override for tests by passing an
// explicit dir to Append/ReadAll below instead of calling this — same
// pattern as gate.GatesDir/registry.LoopsDir/sessions.SessionsDir).
func HistoryDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".missionctl", "history")
}

// Trigger is what caused an event to be recorded.
type Trigger string

const (
	TriggerScan      Trigger = "scan"      // the scanner's edge-triggered state-transition detector
	TriggerHook      Trigger = "hook"      // a Claude Code hook firing (reserved — not yet wired to any emitter)
	TriggerActuation Trigger = "actuation" // a TUI-driven action: resume/inject/approve/interrupt/kill/spawn
	TriggerOracle    Trigger = "oracle"    // a judgeCmd verdict
	TriggerGovernor  Trigger = "governor"  // the governor (internal/engine.Check) stopping a loop
)

// Actor is who/what caused the event.
type Actor string

const (
	ActorHuman  Actor = "human"  // a TUI keypress (actuation events)
	ActorAuto   Actor = "auto"   // an automated judgment (oracle verdicts)
	ActorSystem Actor = "system" // mechanical classification/enforcement (scan transitions, governor)
)

// Event is one append-only history record. FromState/ToState are always
// domain.LoopState string values; for emitters that don't represent a state
// TRANSITION per se (oracle verdicts, actuation attempts) both fields carry
// the loop's state AT THE TIME of the event — a descriptive snapshot, not a
// claim that the action itself changed state (the next scan is what would
// reclassify it, and that reclassification gets its own scan-triggered
// event if it happens). Detail is a short, emitter-specific free-text
// string (a stall kind, a verdict outcome + cycle, an actuation's tier and
// outcome) — optional, "" when there's nothing more to say than from/to.
type Event struct {
	TS        int64   `json:"ts"` // unix nanoseconds
	SessionID string  `json:"session_id"`
	FromState string  `json:"from_state"`
	ToState   string  `json:"to_state"`
	Trigger   Trigger `json:"trigger"`
	Detail    string  `json:"detail,omitempty"`
	Actor     Actor   `json:"actor"`
}

// maxFileSize: once a session's history file would exceed this, Append
// rotates it first (rename to ".1", start the live file fresh again) — a
// SINGLE rotation, deliberately dumb (no generation chain: a second rotation
// just overwrites ".1"). A loop churning through 5MB of history within one
// missionctl run is already far outside normal usage; keeping one backup
// generation is enough to not silently lose the immediately-prior history
// without the bookkeeping of a real log-rotation scheme.
//
// A var, not a const, purely so a test can shrink it to force many
// rotations under concurrent writers (see
// TestAppend_ConcurrentWriters_ForcedRotations_NoDataLoss) — production
// code never overrides it.
var maxFileSize int64 = 5 * 1024 * 1024

// Append records ev to sessionID's history file under dir (HistoryDir(), or
// a test override). Best-effort by design: every caller in this codebase
// swallows the returned error (see package doc) — it's returned here purely
// so a caller COULD choose to log it, not because any caller is expected to
// treat it as fatal.
//
// Review fix (P1): Append is called concurrently from several goroutines
// within one missionctl process (the scanner's transition detector, every
// actuation cmd, judgeCmd, applyGovernor, registry.BindPending) — a bare
// stat-then-rename rotation with no lock could race two concurrent Appends
// for the SAME session into rotating at once, clobbering the ".1" backup
// (silent audit-data loss, the exact failure mode a history log must not
// have). appendMu serializes the whole rotate-then-write span per process.
// Cross-PROCESS writers (e.g. two missionctl cockpits pointed at the same
// ~/.missionctl/history) remain unserialized — out of scope for this fix
// (would need a real file lock, e.g. flock, not just an in-process mutex).
var appendMu sync.Mutex

func Append(dir string, ev Event) error {
	if !validSessionID(ev.SessionID) {
		return &invalidSessionIDError{ev.SessionID}
	}
	appendMu.Lock()
	defer appendMu.Unlock()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, ev.SessionID+".jsonl")
	if err := rotateIfOversize(path); err != nil {
		return err
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(data, '\n'))
	return err
}

// invalidSessionIDError names its offending input, matching this codebase's
// custom-exception convention (see sessions.validSessionID's fmt.Errorf
// callers) — a named type here (rather than fmt.Errorf) since it's checked
// nowhere else by type, just kept consistent/greppable with the rest of the
// package's small, single-purpose error values.
type invalidSessionIDError struct{ sessionID string }

func (e *invalidSessionIDError) Error() string {
	return "events: invalid session id " + quote(e.sessionID)
}

func quote(s string) string { return `"` + s + `"` }

// validSessionID rejects anything that isn't a plain, single-component
// filename — mirrors sessions.validSessionID exactly (same hazard: a
// session_id containing a path separator, joined directly into a
// filesystem path, could otherwise escape dir).
func validSessionID(id string) bool {
	return id != "" && filepath.Base(id) == id
}

// rotateIfOversize renames path to path+".1" (clobbering any previous
// ".1") if it's grown past maxFileSize. A missing file is not an error —
// the common case (a session's very first event).
func rotateIfOversize(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if fi.Size() < maxFileSize {
		return nil
	}
	return os.Rename(path, path+".1")
}

// ReadAll reads every session's history under dir — both the live
// "<id>.jsonl" and, if present, the rotated "<id>.jsonl.1" backup — merged
// and sorted oldest-first per session. Used by `missionctl report`. Tolerant
// throughout: a missing dir yields an empty result (not an error, mirrors
// sessions.ListSessions), and an unreadable file or a malformed line (e.g. a
// torn write from a crash mid-append) is skipped rather than failing the
// whole read — history is diagnostic, not authoritative, so a report should
// degrade gracefully rather than refuse to run over one bad file.
func ReadAll(dir string) (map[string][]Event, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string][]Event{}, nil
		}
		return nil, err
	}
	out := make(map[string][]Event)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		sessionID, ok := sessionIDFromFilename(e.Name())
		if !ok {
			continue
		}
		evs, err := readEventFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue // best-effort — see doc
		}
		out[sessionID] = append(out[sessionID], evs...)
	}
	for id := range out {
		sort.Slice(out[id], func(i, j int) bool { return out[id][i].TS < out[id][j].TS })
	}
	return out, nil
}

// Read returns sessionID's own history from dir — its live ".jsonl" file
// merged with a rotated ".1" backup, if any — oldest first. Unlike
// ReadAll, this reads ONLY the two files that could possibly belong to
// sessionID, not every file in dir — the efficient path for a caller (e.g.
// internal/claude's killed-loop derivation) that only needs one specific
// session's history out of a fleet-wide scan, not a report over all of
// them. Same tolerance as ReadAll: a missing file is a no-op (not an
// error), and a malformed line is skipped rather than failing the whole
// read.
func Read(dir, sessionID string) ([]Event, error) {
	if !validSessionID(sessionID) {
		return nil, &invalidSessionIDError{sessionID}
	}
	var out []Event
	for _, suffix := range [...]string{".jsonl.1", ".jsonl"} {
		evs, err := readEventFile(filepath.Join(dir, sessionID+suffix))
		if err != nil {
			continue // missing or unreadable — best-effort, see doc
		}
		out = append(out, evs...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TS < out[j].TS })
	return out, nil
}

// sessionIDFromFilename strips the ".jsonl" or ".jsonl.1" suffix, reporting
// ok=false for anything else in the directory (defensive — the dir is
// expected to hold only history files, but a stray unrelated file should be
// skipped, not misparsed).
func sessionIDFromFilename(name string) (string, bool) {
	switch {
	case strings.HasSuffix(name, ".jsonl.1"):
		return strings.TrimSuffix(name, ".jsonl.1"), true
	case strings.HasSuffix(name, ".jsonl"):
		return strings.TrimSuffix(name, ".jsonl"), true
	default:
		return "", false
	}
}

// readEventFile parses one history file, skipping any line that fails to
// unmarshal (see ReadAll's doc on tolerance).
func readEventFile(path string) ([]Event, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []Event
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		out = append(out, ev)
	}
	return out, nil
}

// ParseOracleDetail splits a TriggerOracle event's Detail field —
// "<outcome> at cycle <n>: <reason>" (see internal/tui's judgeCmd, the sole
// emitter) — into its outcome and verbatim reason. feat/detail-panel-v2's
// VERDICTS block needs the reason text verbatim (council hard rule: never
// paraphrased); cmd/missionctl's report command only ever needed the
// outcome (its own local, unchanged parser). Tolerant of an unexpected
// shape: outcome falls back to the whole string, reason to "".
func ParseOracleDetail(detail string) (outcome, reason string) {
	i := strings.Index(detail, " at cycle")
	if i < 0 {
		return detail, ""
	}
	outcome = detail[:i]
	if j := strings.Index(detail, ": "); j >= 0 {
		reason = detail[j+2:]
	}
	return outcome, reason
}
