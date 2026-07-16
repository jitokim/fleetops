package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// runHooksCmd dispatches `missionctl hooks <sub>`.
func runHooksCmd(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "missionctl hooks: expected install|uninstall")
		os.Exit(1)
	}
	switch args[0] {
	case "install":
		installHooks()
	case "uninstall":
		uninstallHooks()
	default:
		fmt.Fprintf(os.Stderr, "missionctl hooks: unknown subcommand %q\n", args[0])
		os.Exit(1)
	}
}

// hookSpec pairs a Claude Code hook event name with the `missionctl hook
// <sub>` subcommand suffix that identifies our own entry under it — so
// install/uninstall can run the identical logic once per event instead of
// triplicating it. The suffix is what isMissionctlHookCommand keys on when
// deciding whether an entry is recognizably ours to remove.
type hookSpec struct {
	eventName        string
	subcommandSuffix string
}

// missionctlHookSpecs is the full set of hook events missionctl manages. The
// suffix must match what installHooks appends to the executable path (exe +
// " " + suffix) so uninstall recognizes exactly what install wrote.
func missionctlHookSpecs() []hookSpec {
	return []hookSpec{
		{eventName: "Notification", subcommandSuffix: "hook notify"},
		{eventName: "SessionStart", subcommandSuffix: "hook session-start"},
		{eventName: "SessionEnd", subcommandSuffix: "hook session-end"},
	}
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
// recognizably ours (isMissionctlHookCommand with this event's suffix) — it
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
			if isMissionctlHookCommand(cmd, subcommandSuffix) {
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

// isMissionctlHookCommand identifies our own hook command line for a given
// subcommand suffix (e.g. "hook notify"), so uninstall only ever removes
// entries we installed — never another tool's, and never one of our OTHER
// events (each event keys on its own distinct suffix).
func isMissionctlHookCommand(cmd, subcommandSuffix string) bool {
	return strings.Contains(cmd, "missionctl") && strings.HasSuffix(strings.TrimSpace(cmd), subcommandSuffix)
}

// asSlice normalizes a decoded JSON field that's expected to be an array;
// anything else (missing, wrong type) becomes an empty slice.
func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// ── driver funcs (file IO — not unit tested directly; the pure funcs above are) ──

func settingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

// loadSettings decodes into map[string]any (not a struct) so unknown fields
// round-trip untouched — we only ever want to touch the specific
// hooks.<event> arrays in missionctlHookSpecs.
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

// backupSettings copies the current settings file to settings.json.bak-missionctl
// before we touch it. A missing file needs no backup.
func backupSettings(path string) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return os.WriteFile(path+".bak-missionctl", data, 0o644)
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

func installHooks() {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "missionctl hooks install:", err)
		os.Exit(1)
	}

	path, err := settingsPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, "missionctl hooks install:", err)
		os.Exit(1)
	}
	settings, err := loadSettings(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "missionctl hooks install:", err)
		os.Exit(1)
	}

	// Idempotent per entry: install each event that isn't already present,
	// skip the ones that are. Nothing new means nothing to write.
	var installed []string
	for _, spec := range missionctlHookSpecs() {
		cmd := exe + " " + spec.subcommandSuffix
		if installHookEntry(settings, spec.eventName, cmd) {
			installed = append(installed, spec.eventName)
		}
	}
	if len(installed) == 0 {
		fmt.Println("already installed")
		return
	}

	if err := backupSettings(path); err != nil {
		fmt.Fprintln(os.Stderr, "missionctl hooks install: backup failed:", err)
		os.Exit(1)
	}
	if err := writeSettings(path, settings); err != nil {
		fmt.Fprintln(os.Stderr, "missionctl hooks install:", err)
		os.Exit(1)
	}
	fmt.Printf("installed hooks: %s\n(backup: %s.bak-missionctl)\n", strings.Join(installed, ", "), path)
}

func uninstallHooks() {
	path, err := settingsPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, "missionctl hooks uninstall:", err)
		os.Exit(1)
	}
	settings, err := loadSettings(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "missionctl hooks uninstall:", err)
		os.Exit(1)
	}

	// Remove each event we recognizably installed; leave everything else
	// (other tools' hooks, unrelated settings) untouched.
	changed := false
	for _, spec := range missionctlHookSpecs() {
		if uninstallHookEntry(settings, spec.eventName, spec.subcommandSuffix) {
			changed = true
		}
	}
	if !changed {
		fmt.Println("not installed")
		return
	}

	if err := backupSettings(path); err != nil {
		fmt.Fprintln(os.Stderr, "missionctl hooks uninstall: backup failed:", err)
		os.Exit(1)
	}
	if err := writeSettings(path, settings); err != nil {
		fmt.Fprintln(os.Stderr, "missionctl hooks uninstall:", err)
		os.Exit(1)
	}
	fmt.Printf("uninstalled missionctl's hooks\n(backup: %s.bak-missionctl)\n", path)
}
