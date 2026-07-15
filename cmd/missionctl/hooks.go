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

// installNotifyHook idempotently ensures settings["hooks"]["Notification"]
// contains an entry whose hooks list includes a {"type":"command","command":
// cmd} step, per Claude Code's settings schema:
//
//	"hooks": {"Notification": [{"matcher": "", "hooks": [{"type": "command", "command": cmd}]}]}
//
// Returns changed=false (no-op) if cmd is already present anywhere under
// Notification. All other keys/hook types in settings are left untouched —
// settings is decoded into map[string]any by the caller specifically so
// unknown fields survive round-tripping.
func installNotifyHook(settings map[string]any, cmd string) bool {
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	notification, _ := hooks["Notification"].([]any)

	for _, entryAny := range notification {
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

	notification = append(notification, map[string]any{
		"matcher": "",
		"hooks": []any{
			map[string]any{"type": "command", "command": cmd},
		},
	})
	hooks["Notification"] = notification
	settings["hooks"] = hooks
	return true
}

// uninstallNotifyHook removes only Notification hook entries whose command
// is recognizably ours (isMissionctlNotifyCommand) — it never touches other
// tools' hooks, including other Notification entries that aren't ours.
// Returns changed=false if nothing matched.
func uninstallNotifyHook(settings map[string]any) bool {
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		return false
	}
	notification, _ := hooks["Notification"].([]any)
	if notification == nil {
		return false
	}

	changed := false
	kept := make([]any, 0, len(notification))
	for _, entryAny := range notification {
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
			if isMissionctlNotifyCommand(cmd) {
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
	hooks["Notification"] = kept
	settings["hooks"] = hooks
	return true
}

// isMissionctlNotifyCommand identifies our own hook command line, so
// uninstall only ever removes entries we installed — never another tool's.
func isMissionctlNotifyCommand(cmd string) bool {
	return strings.Contains(cmd, "missionctl") && strings.HasSuffix(strings.TrimSpace(cmd), "hook notify")
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
// round-trip untouched — we only ever want to touch hooks.Notification.
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
	cmd := exe + " hook notify"

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

	if !installNotifyHook(settings, cmd) {
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
	fmt.Printf("installed Notification hook: %s\n(backup: %s.bak-missionctl)\n", cmd, path)
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

	if !uninstallNotifyHook(settings) {
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
	fmt.Printf("uninstalled missionctl's Notification hook\n(backup: %s.bak-missionctl)\n", path)
}
