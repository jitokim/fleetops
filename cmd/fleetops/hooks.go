package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jitokim/fleetops/internal/accounts"
	"github.com/jitokim/fleetops/internal/hooks"
)

// runHooksCmd dispatches `fleetops hooks <sub>`.
func runHooksCmd(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "fleetops hooks: expected install|uninstall|status")
		os.Exit(1)
	}
	switch args[0] {
	case "install":
		installHooks()
	case "uninstall":
		uninstallHooks()
	case "status":
		statusHooks()
	default:
		fmt.Fprintf(os.Stderr, "fleetops hooks: unknown subcommand %q\n", args[0])
		os.Exit(1)
	}
}

// hookSpec pairs a Claude Code hook event name with the `fleetops hook
// <sub>` subcommand suffix that identifies our own entry under it — so
// install/uninstall can run the identical logic once per event instead of
// triplicating it. The suffix is what isFleetopsHookCommand keys on when
// deciding whether an entry is recognizably ours to remove.
type hookSpec struct {
	eventName        string
	subcommandSuffix string
}

// fleetopsHookSpecs is the full set of hook events fleetops manages, derived
// from the canonical internal/hooks.Specs() so the install/uninstall driver
// and the hooks.Health detection can never drift on WHICH events are ours or
// WHAT suffix identifies them. The suffix must match what installHooks appends
// to the executable path (exe + " " + suffix) so uninstall recognizes exactly
// what install wrote.
func fleetopsHookSpecs() []hookSpec {
	specs := hooks.Specs()
	out := make([]hookSpec, len(specs))
	for i, s := range specs {
		out[i] = hookSpec{eventName: s.EventName, subcommandSuffix: s.SubcommandSuffix}
	}
	return out
}

// installHookEntry idempotently ensures settings["hooks"][eventName] contains
// an entry whose hooks list includes a {"type":"command","command":cmd} step,
// per Claude Code's settings schema:
//
//	"hooks": {"<eventName>": [{"matcher": "", "hooks": [{"type": "command", "command": cmd}]}]}
//
// Returns changed=false (no-op) if cmd is already present anywhere under
// eventName. All other keys/hook types in settings are left untouched —
// settings is decoded into map[string]any by the caller specifically so
// unknown fields survive round-tripping.
func installHookEntry(settings map[string]any, eventName, cmd string) bool {
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	event, _ := hooks[eventName].([]any)

	for _, entryAny := range event {
		entry, ok := entryAny.(map[string]any)
		if !ok {
			continue
		}
		for _, hAny := range asSlice(entry["hooks"]) {
			h, ok := hAny.(map[string]any)
			if !ok {
				continue
			}
			if c, _ := h["command"].(string); c == cmd {
				return false // already installed
			}
		}
	}

	event = append(event, map[string]any{
		"matcher": "",
		"hooks": []any{
			map[string]any{"type": "command", "command": cmd},
		},
	})
	hooks[eventName] = event
	settings["hooks"] = hooks
	return true
}

// uninstallHookEntry removes only eventName entries whose command is
// recognizably ours (isFleetopsHookCommand with this event's suffix) — it
// never touches other tools' hooks, including other entries under the same
// event that aren't ours. Returns changed=false if nothing matched.
func uninstallHookEntry(settings map[string]any, eventName, subcommandSuffix string) bool {
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		return false
	}
	event, _ := hooks[eventName].([]any)
	if event == nil {
		return false
	}

	changed := false
	kept := make([]any, 0, len(event))
	for _, entryAny := range event {
		entry, ok := entryAny.(map[string]any)
		if !ok {
			kept = append(kept, entryAny)
			continue
		}
		keptHooks := make([]any, 0)
		for _, hAny := range asSlice(entry["hooks"]) {
			h, ok := hAny.(map[string]any)
			if !ok {
				keptHooks = append(keptHooks, hAny)
				continue
			}
			cmd, _ := h["command"].(string)
			if isFleetopsHookCommand(cmd, subcommandSuffix) {
				changed = true
				continue // drop it
			}
			keptHooks = append(keptHooks, hAny)
		}
		if len(keptHooks) == 0 {
			changed = true
			continue // nothing left in this entry, drop the whole thing
		}
		entry["hooks"] = keptHooks
		kept = append(kept, entry)
	}
	if !changed {
		return false
	}
	hooks[eventName] = kept
	settings["hooks"] = hooks
	return true
}

// isFleetopsHookCommand delegates to the canonical matcher in internal/hooks,
// so uninstall and hooks.Health share ONE definition of "an entry that is
// recognizably ours" and cannot drift. It still removes only entries we
// installed — never another tool's, and never one of our OTHER events (each
// event keys on its own distinct suffix).
func isFleetopsHookCommand(cmd, subcommandSuffix string) bool {
	return hooks.IsFleetopsHookCommand(cmd, subcommandSuffix)
}

// asSlice normalizes a decoded JSON field that's expected to be an array;
// anything else (missing, wrong type) becomes an empty slice.
func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// ── driver funcs (file IO — not unit tested directly; the pure funcs above are) ──

// settingsPath delegates to hooks.DefaultSettingsPath so the audited WRITE
// path and the read-only detection resolve the exact same file.
func settingsPath() (string, error) {
	return hooks.DefaultSettingsPath()
}

// hookLocations enumerates EVERY config dir fleetops manages hooks in — the
// default ~/.claude plus each alias config dir in ~/.fleetops/accounts.json —
// so install/uninstall/status operate on all of them, not just the default.
// This is what stops a loop spawned under a non-default account from firing NO
// fleetops hooks (recording nothing) because its settings.json was never
// touched. Zero-config (no accounts.json) yields just the default location, so
// nothing changes for single-account users.
func hookLocations() ([]hooks.ConfigDirLocation, error) {
	path, err := settingsPath()
	if err != nil {
		return nil, err
	}
	return hooks.SettingsLocations(path, accounts.DefaultPath()), nil
}

// loadSettings decodes into map[string]any (not a struct) so unknown fields
// round-trip untouched — we only ever want to touch the specific
// hooks.<event> arrays in fleetopsHookSpecs.
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
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return settings, nil
}

// backupSettings copies the current settings file to settings.json.bak-fleetops
// before we touch it. A missing file needs no backup.
func backupSettings(path string) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return os.WriteFile(path+".bak-fleetops", data, 0o644)
}

func writeSettings(path string, settings map[string]any) error {
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// installHooksAt installs the fleetops hook entries into ONE settings.json,
// creating the file (and its config dir) if needed — the same backup+merge+
// idempotent logic install has always used for ~/.claude/settings.json, now
// reused per config dir rather than duplicated. Returns the events it newly
// added (empty ⇒ already installed, nothing written).
func installHooksAt(path, exe string) ([]string, error) {
	settings, err := loadSettings(path)
	if err != nil {
		return nil, err
	}
	var installed []string
	for _, spec := range fleetopsHookSpecs() {
		cmd := exe + " " + spec.subcommandSuffix
		if installHookEntry(settings, spec.eventName, cmd) {
			installed = append(installed, spec.eventName)
		}
	}
	if len(installed) == 0 {
		return nil, nil // already installed — nothing to back up or write
	}
	if err := backupSettings(path); err != nil {
		return nil, fmt.Errorf("backup failed: %w", err)
	}
	if err := writeSettings(path, settings); err != nil {
		return nil, err
	}
	return installed, nil
}

func installHooks() {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fleetops hooks install:", err)
		os.Exit(1)
	}
	locations, err := hookLocations()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fleetops hooks install:", err)
		os.Exit(1)
	}

	// Install into every account's config dir (default + each alias), so a loop
	// spawned under a non-default account fires our hooks too. Each dir is
	// independent; a failure in one is reported but does not abort the rest.
	anyInstalled := false
	for _, loc := range locations {
		installed, err := installHooksAt(loc.Path, exe)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fleetops hooks install [%s]: %v\n", loc.Label, err)
			os.Exit(1)
		}
		if len(installed) == 0 {
			fmt.Printf("[%s] already installed (%s)\n", loc.Label, loc.Path)
			continue
		}
		anyInstalled = true
		fmt.Printf("[%s] installed hooks: %s (%s)\n(backup: %s.bak-fleetops)\n",
			loc.Label, strings.Join(installed, ", "), loc.Path, loc.Path)
	}
	if !anyInstalled {
		fmt.Println("all config dirs already installed")
	}
}

// uninstallHooksAt removes the fleetops hook entries from ONE settings.json,
// returning whether anything changed (false ⇒ nothing of ours was present).
func uninstallHooksAt(path string) (bool, error) {
	settings, err := loadSettings(path)
	if err != nil {
		return false, err
	}
	changed := false
	for _, spec := range fleetopsHookSpecs() {
		if uninstallHookEntry(settings, spec.eventName, spec.subcommandSuffix) {
			changed = true
		}
	}
	if !changed {
		return false, nil
	}
	if err := backupSettings(path); err != nil {
		return false, fmt.Errorf("backup failed: %w", err)
	}
	if err := writeSettings(path, settings); err != nil {
		return false, err
	}
	return true, nil
}

func uninstallHooks() {
	locations, err := hookLocations()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fleetops hooks uninstall:", err)
		os.Exit(1)
	}

	// Remove from every account's config dir, so `uninstall` fully reverses
	// `install` and never leaves a stale hook behind in an alias dir.
	anyChanged := false
	for _, loc := range locations {
		changed, err := uninstallHooksAt(loc.Path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fleetops hooks uninstall [%s]: %v\n", loc.Label, err)
			os.Exit(1)
		}
		if !changed {
			fmt.Printf("[%s] not installed (%s)\n", loc.Label, loc.Path)
			continue
		}
		anyChanged = true
		fmt.Printf("[%s] uninstalled fleetops's hooks (%s)\n(backup: %s.bak-fleetops)\n", loc.Label, loc.Path, loc.Path)
	}
	if !anyChanged {
		fmt.Println("not installed in any config dir")
	}
}

// statusHooks prints the current hook health PER config dir without touching
// settings.json — the read-only counterpart to install/uninstall. Reporting per
// dir is what makes a "not installed in company" state VISIBLE rather than
// silent: after an uninstall each dir reports Missing, after a reinstall each
// reports OK, and a dir with no hooks stands out from the healthy ones.
func statusHooks() {
	path, err := settingsPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fleetops hooks status:", err)
		os.Exit(1)
	}
	healths := hooks.HealthAllAt(path, accounts.DefaultPath(), hooks.BinaryExists)
	fmt.Print(formatMultiHookStatus(healths))
}

// formatMultiHookStatus renders one labeled section per config dir, so a
// per-alias gap ("company: missing") is legible at a glance. Pure (no I/O) so
// it is unit-testable — statusHooks only resolves paths and prints this.
func formatMultiHookStatus(healths []hooks.ConfigDirHealth) string {
	var b strings.Builder
	for i, h := range healths {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "── %s (%s) ──\n", h.Location.Label, h.Location.Path)
		b.WriteString(formatHookStatus(h.Report))
	}
	return b.String()
}

// formatHookStatus renders a Report as human-readable status lines plus a
// one-line verdict. Pure (no I/O) so it is unit-testable — statusHooks only
// resolves the path and prints what this returns.
func formatHookStatus(report hooks.Report) string {
	var b strings.Builder
	for _, e := range report.Events {
		line := fmt.Sprintf("%-18s %s", e.Event, e.State)
		if e.State == hooks.StateStalePath {
			line += fmt.Sprintf(" (points at missing binary: %s)", e.Binary)
		}
		b.WriteString(line + "\n")
	}
	if report.OK {
		b.WriteString("\nall fleetops hooks installed and healthy\n")
	} else if report.HasStalePath() {
		b.WriteString("\nhooks point at a missing binary — run: fleetops hooks install\n")
	} else {
		b.WriteString("\nhooks not fully installed — run: fleetops hooks install\n")
	}
	return b.String()
}
