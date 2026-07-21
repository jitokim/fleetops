// Package hooks owns the canonical definition of what a "fleetops Claude Code
// hook" is — the set of managed events, the command-matching rule that decides
// whether a settings.json entry is recognizably ours, and a pure Health check
// that reports whether those hooks are actually installed AND wired to a
// binary that still exists.
//
// The point of the package is that install/uninstall (cmd/fleetops/hooks.go,
// the audited settings.json WRITE path) and detection (this package, read-only)
// share the SAME matcher and the SAME event list, so they can never disagree
// about what "installed" means. This package never writes settings.json — it
// only reads and reasons.
//
// Why StalePath is a first-class state: a hook entry that LOOKS installed but
// whose command points at a binary that no longer exists is strictly worse
// than a missing one. Session registration silently no-ops with no error and
// no signal — exactly the missionctl-dead-path regression this repo already
// lived through. "Looks installed" must therefore never read as healthy.
package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jitokim/fleetops/internal/accounts"
)

// Spec pairs a Claude Code hook event name with the `fleetops hook <sub>`
// suffix that identifies our own entry under it. Declared here — not in
// cmd/fleetops — so the install/uninstall driver and the Health check draw the
// managed-event list from ONE source and cannot drift out of sync.
type Spec struct {
	EventName        string
	SubcommandSuffix string
}

// Specs is the full set of hook events fleetops manages. The suffix is what
// installHooks appends to the executable path (exe + " " + suffix), and what
// IsFleetopsHookCommand keys on to recognize an entry as ours.
func Specs() []Spec {
	return []Spec{
		{EventName: "Notification", SubcommandSuffix: "hook notify"},
		{EventName: "SessionStart", SubcommandSuffix: "hook session-start"},
		{EventName: "SessionEnd", SubcommandSuffix: "hook session-end"},
		// PermissionRequest is the only source that names the tool a gate is
		// asking about — registered as a sensor. See permissionHook's contract.
		{EventName: "PermissionRequest", SubcommandSuffix: "hook permission"},
	}
}

// IsFleetopsHookCommand identifies our own hook command line for a given
// subcommand suffix (e.g. "hook notify"), so callers only ever act on entries
// we installed — never another tool's, and never one of our OTHER events (each
// event keys on its own distinct suffix). This is the single matcher shared by
// cmd/fleetops's uninstall and this package's Health.
func IsFleetopsHookCommand(cmd, subcommandSuffix string) bool {
	return strings.Contains(cmd, "fleetops") && strings.HasSuffix(strings.TrimSpace(cmd), subcommandSuffix)
}

// EventState classifies one managed hook event's health.
type EventState int

const (
	// StateOK: an entry recognizably ours is present AND the binary its
	// command points at exists on disk.
	StateOK EventState = iota
	// StateMissing: no entry recognizably ours is present for this event.
	StateMissing
	// StateStalePath: an entry recognizably ours IS present, but the binary
	// its command points at does not exist. Worse than StateMissing — it
	// "looks installed" yet silently no-ops. The missionctl-dead-path case.
	StateStalePath
)

// String renders an EventState for status output and messaging.
func (s EventState) String() string {
	switch s {
	case StateOK:
		return "ok"
	case StateMissing:
		return "missing"
	case StateStalePath:
		return "stale-path"
	default:
		return "unknown"
	}
}

// EventStatus is one managed event's per-event health finding.
type EventStatus struct {
	Event string
	State EventState
	// Binary is the executable path the event's command points at — populated
	// whenever an entry recognizably ours is present (StateOK or StateStalePath),
	// so a stale-path message can name the dead binary. Empty for StateMissing.
	Binary string
}

// Report is the overall hook-health picture across every managed event.
type Report struct {
	Events []EventStatus
	// OK is true iff EVERY managed event is StateOK. The zero-value Report
	// (no events) is deliberately not OK — an honest "cannot confirm installed".
	OK bool
}

// HasStalePath reports whether any managed event is StateStalePath — the cue
// for the scarier "hooks point at a missing binary" message, distinct from a
// plain "not installed".
func (r Report) HasStalePath() bool {
	for _, e := range r.Events {
		if e.State == StateStalePath {
			return true
		}
	}
	return false
}

// Missing returns the names of events with no entry of ours at all.
func (r Report) Missing() []string {
	return r.eventsInState(StateMissing)
}

// StalePaths returns the names of events whose entry points at a dead binary.
func (r Report) StalePaths() []string {
	return r.eventsInState(StateStalePath)
}

func (r Report) eventsInState(state EventState) []string {
	var names []string
	for _, e := range r.Events {
		if e.State == state {
			names = append(names, e.Event)
		}
	}
	return names
}

// Health computes per-event hook health from already-parsed settings.json
// contents and an injectable binary-existence probe. Pure: it does no I/O of
// its own, so it is exhaustively table-testable. A nil/absent settings map
// (missing or malformed file) yields every event Missing and OK=false — an
// honest "cannot confirm installed", never a panic.
func Health(settings map[string]any, exists func(string) bool) Report {
	specs := Specs()
	report := Report{Events: make([]EventStatus, 0, len(specs)), OK: true}
	for _, spec := range specs {
		status := eventHealth(settings, spec, exists)
		report.Events = append(report.Events, status)
		if status.State != StateOK {
			report.OK = false
		}
	}
	return report
}

// eventHealth resolves a single event to Missing (no entry of ours), StalePath
// (our entry present but its binary is gone) or OK (present and the binary
// exists).
func eventHealth(settings map[string]any, spec Spec, exists func(string) bool) EventStatus {
	cmd, found := findFleetopsCommand(settings, spec)
	if !found {
		return EventStatus{Event: spec.EventName, State: StateMissing}
	}
	binary := binaryFromCommand(cmd, spec.SubcommandSuffix)
	if !exists(binary) {
		return EventStatus{Event: spec.EventName, State: StateStalePath, Binary: binary}
	}
	return EventStatus{Event: spec.EventName, State: StateOK, Binary: binary}
}

// findFleetopsCommand returns the first command string under
// settings.hooks[event] that IsFleetopsHookCommand recognizes as ours for this
// event's suffix. Every lookup is comma-ok so malformed or foreign shapes are
// skipped, never panicked on — the same defensive decoding installHookEntry
// uses, which is why settings is decoded into map[string]any rather than a
// struct.
func findFleetopsCommand(settings map[string]any, spec Spec) (string, bool) {
	hooks, _ := settings["hooks"].(map[string]any)
	event, _ := hooks[spec.EventName].([]any)
	for _, entryAny := range event {
		entry, ok := entryAny.(map[string]any)
		if !ok {
			continue
		}
		list, _ := entry["hooks"].([]any)
		for _, hAny := range list {
			h, ok := hAny.(map[string]any)
			if !ok {
				continue
			}
			cmd, _ := h["command"].(string)
			if IsFleetopsHookCommand(cmd, spec.SubcommandSuffix) {
				return cmd, true
			}
		}
	}
	return "", false
}

// binaryFromCommand extracts the executable path from a hook command line by
// stripping the known `hook <sub>` suffix, e.g.
// "/usr/local/bin/fleetops hook notify" minus "hook notify" →
// "/usr/local/bin/fleetops". Only ever called after IsFleetopsHookCommand has
// confirmed the suffix is present, so TrimSuffix is guaranteed to bite.
func binaryFromCommand(cmd, subcommandSuffix string) string {
	trimmed := strings.TrimSpace(cmd)
	return strings.TrimSpace(strings.TrimSuffix(trimmed, subcommandSuffix))
}

// BinaryExists reports whether path names an existing regular file — the
// production probe behind Health's injectable existence check. A hook whose
// command points at a path that no longer exists is StalePath, not healthy.
func BinaryExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Mode().IsRegular()
}

// DefaultSettingsPath is ~/.claude/settings.json — the same file the audited
// install/uninstall path writes. Shared so detection and mutation can never
// read/write different files.
func DefaultSettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

// HealthAt reads and parses the settings file at path, then computes Health
// against the given probe. Fail-open to a degraded (all-Missing, not-OK)
// report on any read/parse trouble: a missing file, an unreadable file, and
// malformed JSON all mean the same thing operationally — we cannot confirm
// hooks are installed — so it says so honestly rather than crash or lie
// healthy.
func HealthAt(path string, exists func(string) bool) Report {
	settings, err := loadSettings(path)
	if err != nil {
		settings = nil // degraded: Health then reports every event Missing
	}
	return Health(settings, exists)
}

// loadSettings decodes the settings file into map[string]any (never a struct)
// so unknown fields are irrelevant to detection. A missing file is not an
// error (returns an empty map); a malformed one is. This is a READ helper only
// — the audited WRITE path lives in cmd/fleetops and is intentionally not
// shared here.
func loadSettings(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, err
	}
	return settings, nil
}

// ConfigDirLocation names one Claude config dir whose settings.json fleetops
// manages hooks in: the default account, or a named alias. Label is for human
// output ("default"/"company"); Path is the settings.json to read or write.
type ConfigDirLocation struct {
	Label string
	Path  string
}

// DefaultLabel is the Label used for the default account's ~/.claude config dir
// — the account with no CLAUDE_CONFIG_DIR override.
const DefaultLabel = "default"

// SettingsLocations enumerates every settings.json fleetops installs hooks
// into: the default ~/.claude/settings.json FIRST, then one per alias config
// dir declared in accountsPath ("<configDir>/settings.json"), ordered by alias
// name and deduped by cleaned path so an alias that names the default dir does
// not double it.
//
// This is the SINGLE source of "which config dirs" shared by install/uninstall
// (cmd/fleetops, the write path) and health (this package, read-only), so they
// can never disagree about where hooks belong — the same discipline Specs()
// enforces for WHICH events. A missing/malformed accounts.json yields JUST the
// default location, so zero-config install/status behave byte-identically.
func SettingsLocations(defaultSettingsPath, accountsPath string) []ConfigDirLocation {
	locs := make([]ConfigDirLocation, 0, 1)
	seen := map[string]bool{}
	add := func(label, path string) {
		if path == "" {
			return
		}
		clean := filepath.Clean(path)
		if seen[clean] {
			return
		}
		seen[clean] = true
		locs = append(locs, ConfigDirLocation{Label: label, Path: clean})
	}
	add(DefaultLabel, defaultSettingsPath)

	cfg, err := accounts.Load(accountsPath)
	if err != nil {
		return locs // a typo in accounts.json must not drop the default location
	}
	names := make([]string, 0, len(cfg.Aliases))
	for name := range cfg.Aliases {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if dir := cfg.Aliases[name]; dir != "" {
			add(name, filepath.Join(dir, "settings.json"))
		}
	}
	return locs
}

// ConfigDirHealth pairs one config dir's location with its hook-health report,
// so status and the launch self-verify can say "installed in default, MISSING
// in company" per dir instead of one blurred verdict that hides a non-default
// account whose loops record nothing.
type ConfigDirHealth struct {
	Location ConfigDirLocation
	Report   Report
}

// HealthAllAt computes per-config-dir health across SettingsLocations. Each
// dir's settings.json is read and probed independently (a missing one degrades
// to that dir's own all-Missing report, never affecting the others). Injectable
// exists probe, so it is testable without touching a real filesystem beyond the
// fixture paths a test supplies.
func HealthAllAt(defaultSettingsPath, accountsPath string, exists func(string) bool) []ConfigDirHealth {
	locs := SettingsLocations(defaultSettingsPath, accountsPath)
	out := make([]ConfigDirHealth, 0, len(locs))
	for _, loc := range locs {
		out = append(out, ConfigDirHealth{Location: loc, Report: HealthAt(loc.Path, exists)})
	}
	return out
}

// Merge folds per-config-dir reports into ONE Report for the launch banner,
// which only needs "is everything OK, and if not is any of it the scarier
// stale-path kind". OK is the AND across all dirs; Events are concatenated so
// HasStalePath/Missing still answer across every dir. A dir not fully installed
// (e.g. "company") thus flips the merged report not-OK — making a silent
// per-alias gap visible, the whole point of CRITICAL-1's hooks half.
func Merge(healths []ConfigDirHealth) Report {
	merged := Report{OK: true}
	for _, h := range healths {
		merged.Events = append(merged.Events, h.Report.Events...)
		if !h.Report.OK {
			merged.OK = false
		}
	}
	return merged
}

// DefaultHealth reads the real ~/.claude/settings.json AND every alias config
// dir's settings.json (from ~/.fleetops/accounts.json), probes the filesystem,
// and MERGES them — the production entry point the TUI's startup check calls,
// so the banner fires when hooks are missing in ANY account, not only the
// default one. A home-directory lookup failure degrades to a not-OK zero Report
// rather than crashing the launch. Zero-config (no accounts.json) merges a
// single default-dir report, byte-identical to the pre-multi-account check.
func DefaultHealth() Report {
	path, err := DefaultSettingsPath()
	if err != nil {
		return Report{}
	}
	return Merge(HealthAllAt(path, accounts.DefaultPath(), BinaryExists))
}
