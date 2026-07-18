// Package hidden persists the TUI's hide-set — the session IDs a human has
// hidden ("d") or deleted ("x") from the FLEET list. Unlike the in-memory
// dismiss it replaced, this survives a fleetops restart: because loops are
// re-derived by scanning ~/.claude session logs on every launch, an in-memory
// hide brings every hidden (even gone/dead) loop back the moment fleetops
// restarts. A persisted tombstone keeps it filtered instead.
//
// Non-destructive: this package NEVER touches ~/.claude and NEVER removes a
// registry record — a hidden session is only a tombstone the TUI filters each
// scan's result through. Delete ("x") additionally removes the session-registry
// tty registration (internal/sessions.DeleteSession) and tombstones the same
// session here, but the conversation log (the jsonl) is always preserved.
//
// Same ~/.fleetops home and best-effort/fail-open discipline as
// internal/sessions and internal/registry: a missing OR corrupt hidden.json
// loads as an empty set (show every loop) rather than crashing — the safe
// default is always to reveal a loop, never to lose one behind an unreadable
// tombstone file. Writes are atomic (temp-file + rename) so a crash mid-write
// can never leave a half-written file that would then fail-open and resurrect
// every tombstone at once.
package hidden

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

// HiddenFile is ~/.fleetops/hidden.json — a single file alongside the loops/
// sessions/pending registries under ~/.fleetops (override for tests by passing
// an explicit path to the funcs below, same pattern as registry.LoopsDir /
// sessions.SessionsDir). Returns "" if the home dir can't be resolved, which
// Load treats as an empty set and Add reports as an error.
func HiddenFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".fleetops", "hidden.json")
}

// hiddenFile is the on-disk JSON shape: a sorted list of hidden session IDs.
// A list (not a map) so the file reads cleanly by hand and diffs stably.
type hiddenFile struct {
	Hidden []string `json:"hidden"`
}

// Load reads path into a sessionID -> true set. A missing, unreadable, or
// malformed file yields an EMPTY set (never an error): fail-open, show every
// loop. The returned map is always non-nil, so callers can index it directly.
func Load(path string) map[string]bool {
	set, _ := load(path)
	return set
}

// load is Load plus the one bit Load throws away: whether the empty set came
// from a file that EXISTS but could not be parsed. Reading is fail-open either
// way, but Add must tell the two apart — see corruptedFileSuffix.
func load(path string) (set map[string]bool, corrupt bool) {
	out := make(map[string]bool)
	data, err := os.ReadFile(path)
	if err != nil {
		return out, false // missing/unreadable → empty (fail-open, show every loop)
	}
	var hf hiddenFile
	if err := json.Unmarshal(data, &hf); err != nil {
		return out, true // corrupt → empty (fail-open), never crash on garbage
	}
	for _, id := range hf.Hidden {
		if id == "" {
			continue
		}
		out[id] = true
	}
	return out, false
}

// Add tombstones sessionID: it re-reads the current set from disk (so a
// concurrent add elsewhere isn't clobbered), inserts sessionID, and atomically
// rewrites path. Returns the resulting set so the caller can adopt it as its
// in-memory copy, keeping memory and disk in lockstep. A no-op sessionID ("")
// is rejected so an empty tombstone can never be written.
// A corrupt file gets preserved alongside as <path>.bad first: fail-open on
// READ is right (never hide a loop behind an unreadable file), but fail-open
// then OVERWRITE is data loss — Load returns the empty set for garbage, so a
// plain rewrite would replace every prior tombstone with just this one id.
// Renaming keeps the bytes recoverable while still letting the hide succeed.
func Add(path, sessionID string) (map[string]bool, error) {
	if path == "" {
		return nil, &os.PathError{Op: "hidden.Add", Path: path, Err: os.ErrInvalid}
	}
	set, corrupt := load(path)
	if corrupt {
		if err := os.Rename(path, path+corruptedFileSuffix); err != nil {
			return nil, err // couldn't preserve it → refuse rather than destroy
		}
	}
	if sessionID != "" {
		set[sessionID] = true
	}
	if err := write(path, set); err != nil {
		return nil, err
	}
	return set, nil
}

// corruptedFileSuffix names the copy Add sets aside before it overwrites a
// hidden.json it could not parse, so the tombstones inside a damaged file stay
// recoverable by hand instead of being silently replaced by a single id.
const corruptedFileSuffix = ".bad"

// write persists set to path atomically: marshal to a sorted list, write a
// sibling temp file, fsync-free rename over path (rename is atomic on the same
// filesystem, which the sibling temp guarantees). On any error before the
// rename the temp file is removed so no ".tmp" litter survives a failure.
func write(path string, set map[string]bool) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids) // deterministic on-disk order → stable diffs
	data, err := json.Marshal(hiddenFile{Hidden: ids})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".hidden-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
