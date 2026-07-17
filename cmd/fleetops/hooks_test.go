package main

import (
	"encoding/json"
	"reflect"
	"testing"
)

const (
	testCmd     = "/usr/local/bin/fleetops hook notify"
	testStartC  = "/usr/local/bin/fleetops hook session-start"
	testEndCmd  = "/usr/local/bin/fleetops hook session-end"
	notifySuf   = "hook notify"
	startSuffix = "hook session-start"
	endSuffix   = "hook session-end"
)

// ── installHookEntry ──────────────────────────────────────────────────────

func TestInstallHookEntry_EmptySettings(t *testing.T) {
	settings := map[string]any{}

	if changed := installHookEntry(settings, "Notification", testCmd); !changed {
		t.Fatal("expected changed=true on empty settings")
	}

	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("settings[hooks] = %T, want map[string]any", settings["hooks"])
	}
	notif, ok := hooks["Notification"].([]any)
	if !ok || len(notif) != 1 {
		t.Fatalf("hooks[Notification] = %+v, want 1 entry", hooks["Notification"])
	}
	entry := notif[0].(map[string]any)
	hooksList := entry["hooks"].([]any)
	if len(hooksList) != 1 {
		t.Fatalf("entry[hooks] = %+v, want 1 hook", entry["hooks"])
	}
	h := hooksList[0].(map[string]any)
	if h["command"] != testCmd || h["type"] != "command" {
		t.Errorf("got %+v, want command=%q type=command", h, testCmd)
	}
}

// TestInstallHookEntry_AllThreeEvents installs Notification, SessionStart and
// SessionEnd into one settings map and asserts each landed under its own
// event key with its own command — the core of the generalization.
func TestInstallHookEntry_AllThreeEvents(t *testing.T) {
	settings := map[string]any{}

	specs := []struct {
		event string
		cmd   string
	}{
		{"Notification", testCmd},
		{"SessionStart", testStartC},
		{"SessionEnd", testEndCmd},
	}
	for _, s := range specs {
		if changed := installHookEntry(settings, s.event, s.cmd); !changed {
			t.Fatalf("installHookEntry(%s) = false, want true", s.event)
		}
	}

	hooks := settings["hooks"].(map[string]any)
	for _, s := range specs {
		arr, ok := hooks[s.event].([]any)
		if !ok || len(arr) != 1 {
			t.Fatalf("hooks[%s] = %+v, want 1 entry", s.event, hooks[s.event])
		}
		h := arr[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
		if h["command"] != s.cmd {
			t.Errorf("hooks[%s] command = %v, want %q", s.event, h["command"], s.cmd)
		}
	}
}

// TestInstallHookEntry_AppendsAlongsideForeignSameEvent is the real-world
// case verified against a real settings.json: another tool already
// has SessionStart entries, and installing ours must APPEND a new entry while
// leaving every foreign entry under the same event byte-for-byte intact.
func TestInstallHookEntry_AppendsAlongsideForeignSameEvent(t *testing.T) {
	foreignA := map[string]any{"matcher": "", "hooks": []any{map[string]any{"type": "command", "command": "~/.claude/hooks/working-notes.sh"}}}
	foreignB := map[string]any{"matcher": "", "hooks": []any{map[string]any{"type": "command", "command": "~/.claude/hooks/curator.sh"}}}
	settings := map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{foreignA, foreignB},
		},
	}

	if changed := installHookEntry(settings, "SessionStart", testStartC); !changed {
		t.Fatal("expected changed=true appending alongside foreign entries")
	}

	ss := settings["hooks"].(map[string]any)["SessionStart"].([]any)
	if len(ss) != 3 {
		t.Fatalf("SessionStart entries = %d, want 3 (2 foreign + 1 ours)", len(ss))
	}
	// foreign entries stay first, unchanged; ours is appended last.
	if !reflect.DeepEqual(ss[0], foreignA) || !reflect.DeepEqual(ss[1], foreignB) {
		t.Errorf("foreign SessionStart entries were mutated: %+v", ss[:2])
	}
	ours := ss[2].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	if ours["command"] != testStartC {
		t.Errorf("appended entry command = %v, want %q", ours["command"], testStartC)
	}
}

func TestInstallHookEntry_PreservesOtherHookTypes(t *testing.T) {
	settings := map[string]any{
		"hooks": map[string]any{
			"PreToolUse": []any{
				map[string]any{
					"matcher": "Bash",
					"hooks":   []any{map[string]any{"type": "command", "command": "some-other-tool check"}},
				},
			},
		},
	}

	if changed := installHookEntry(settings, "Notification", testCmd); !changed {
		t.Fatal("expected changed=true")
	}

	hooks := settings["hooks"].(map[string]any)
	if _, ok := hooks["PreToolUse"]; !ok {
		t.Error("PreToolUse hooks were dropped, want preserved")
	}
	preToolUse := hooks["PreToolUse"].([]any)
	if len(preToolUse) != 1 {
		t.Errorf("PreToolUse entries = %d, want 1 (untouched)", len(preToolUse))
	}
	if _, ok := hooks["Notification"]; !ok {
		t.Error("Notification hooks were not added")
	}
}

// TestInstallHookEntry_SessionStartLeavesNotificationUntouched proves adding
// one event doesn't disturb an already-present different event — the exact
// idempotence a live re-install against a real settings.json relies on.
func TestInstallHookEntry_SessionStartLeavesNotificationUntouched(t *testing.T) {
	settings := map[string]any{
		"hooks": map[string]any{
			"Notification": []any{
				map[string]any{"matcher": "", "hooks": []any{map[string]any{"type": "command", "command": testCmd}}},
			},
		},
	}

	if changed := installHookEntry(settings, "SessionStart", testStartC); !changed {
		t.Fatal("expected changed=true adding SessionStart")
	}

	hooks := settings["hooks"].(map[string]any)
	notif := hooks["Notification"].([]any)
	if len(notif) != 1 {
		t.Fatalf("Notification entries = %d, want 1 (untouched)", len(notif))
	}
	if h := notif[0].(map[string]any)["hooks"].([]any)[0].(map[string]any); h["command"] != testCmd {
		t.Errorf("existing Notification command mutated: %v", h["command"])
	}
	if _, ok := hooks["SessionStart"]; !ok {
		t.Error("SessionStart was not added")
	}
}

func TestInstallHookEntry_PreservesUnknownTopLevelFields(t *testing.T) {
	settings := map[string]any{
		"someFutureSetting": "keep me",
		"model":             "opus",
	}

	installHookEntry(settings, "SessionStart", testStartC)

	if settings["someFutureSetting"] != "keep me" {
		t.Error("unknown field someFutureSetting was dropped")
	}
	if settings["model"] != "opus" {
		t.Error("unrelated field model was dropped")
	}
}

func TestInstallHookEntry_AlreadyInstalled_NoChange(t *testing.T) {
	settings := map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{
				map[string]any{"matcher": "", "hooks": []any{map[string]any{"type": "command", "command": testStartC}}},
			},
		},
	}
	before := deepCopyJSON(t, settings)

	if changed := installHookEntry(settings, "SessionStart", testStartC); changed {
		t.Error("expected changed=false when already installed")
	}
	if !reflect.DeepEqual(settings, before) {
		t.Errorf("settings mutated despite no-op: got %+v, want unchanged %+v", settings, before)
	}
}

// ── uninstallHookEntry ────────────────────────────────────────────────────

func TestUninstallHookEntry_RemovesOnlyOurs(t *testing.T) {
	settings := map[string]any{
		"hooks": map[string]any{
			"Notification": []any{
				map[string]any{"matcher": "", "hooks": []any{map[string]any{"type": "command", "command": testCmd}}},
				map[string]any{"matcher": "", "hooks": []any{map[string]any{"type": "command", "command": "some-other-tool notify"}}},
			},
		},
	}

	if changed := uninstallHookEntry(settings, "Notification", notifySuf); !changed {
		t.Fatal("expected changed=true")
	}

	hooks := settings["hooks"].(map[string]any)
	notif := hooks["Notification"].([]any)
	if len(notif) != 1 {
		t.Fatalf("got %d Notification entries, want 1 (only ours removed): %+v", len(notif), notif)
	}
	entry := notif[0].(map[string]any)
	h := entry["hooks"].([]any)[0].(map[string]any)
	if h["command"] != "some-other-tool notify" {
		t.Errorf("wrong entry survived: %+v", h)
	}
}

// TestUninstallHookEntry_OnlyMatchingEvent proves uninstalling SessionStart
// leaves our OWN Notification entry alone — each event keys on its own
// distinct suffix, so they can't cross-remove each other.
func TestUninstallHookEntry_OnlyMatchingEvent(t *testing.T) {
	settings := map[string]any{
		"hooks": map[string]any{
			"Notification": []any{
				map[string]any{"matcher": "", "hooks": []any{map[string]any{"type": "command", "command": testCmd}}},
			},
			"SessionStart": []any{
				map[string]any{"matcher": "", "hooks": []any{map[string]any{"type": "command", "command": testStartC}}},
			},
		},
	}

	if changed := uninstallHookEntry(settings, "SessionStart", startSuffix); !changed {
		t.Fatal("expected changed=true removing SessionStart")
	}

	hooks := settings["hooks"].(map[string]any)
	if _, ok := hooks["Notification"]; !ok {
		t.Fatal("Notification event was dropped, want preserved")
	}
	notif := hooks["Notification"].([]any)
	if len(notif) != 1 {
		t.Errorf("Notification entries = %d, want 1 (untouched)", len(notif))
	}
	ss := hooks["SessionStart"].([]any)
	if len(ss) != 0 {
		t.Errorf("SessionStart entries = %d, want 0 (removed)", len(ss))
	}
}

func TestUninstallHookEntry_PreservesOtherHookTypes(t *testing.T) {
	settings := map[string]any{
		"hooks": map[string]any{
			"Notification": []any{
				map[string]any{"matcher": "", "hooks": []any{map[string]any{"type": "command", "command": testCmd}}},
			},
			"PreToolUse": []any{
				map[string]any{"matcher": "Bash", "hooks": []any{map[string]any{"type": "command", "command": "some-other-tool check"}}},
			},
		},
	}

	if changed := uninstallHookEntry(settings, "Notification", notifySuf); !changed {
		t.Fatal("expected changed=true")
	}
	hooks := settings["hooks"].(map[string]any)
	if _, ok := hooks["PreToolUse"]; !ok {
		t.Error("PreToolUse was dropped by uninstall, want preserved")
	}
}

func TestUninstallHookEntry_NotInstalled_NoChange(t *testing.T) {
	settings := map[string]any{}
	if changed := uninstallHookEntry(settings, "Notification", notifySuf); changed {
		t.Error("expected changed=false when not installed")
	}
}

func TestUninstallHookEntry_EmptySettings_NoChange(t *testing.T) {
	if changed := uninstallHookEntry(map[string]any{}, "SessionEnd", endSuffix); changed {
		t.Error("expected changed=false for empty settings")
	}
}

// TestInstallUninstallRoundTrip installs all three events then uninstalls
// them and asserts nothing our-owned survives while an unrelated tool's hook
// does — the install/uninstall symmetry the driver funcs depend on.
func TestInstallUninstallRoundTrip(t *testing.T) {
	settings := map[string]any{
		"hooks": map[string]any{
			"PreToolUse": []any{
				map[string]any{"matcher": "Bash", "hooks": []any{map[string]any{"type": "command", "command": "other-tool check"}}},
			},
		},
	}
	specs := []struct{ event, cmd, suffix string }{
		{"Notification", testCmd, notifySuf},
		{"SessionStart", testStartC, startSuffix},
		{"SessionEnd", testEndCmd, endSuffix},
	}
	for _, s := range specs {
		installHookEntry(settings, s.event, s.cmd)
	}
	for _, s := range specs {
		if !uninstallHookEntry(settings, s.event, s.suffix) {
			t.Fatalf("uninstallHookEntry(%s) = false, want true", s.event)
		}
	}

	hooks := settings["hooks"].(map[string]any)
	for _, s := range specs {
		if arr, _ := hooks[s.event].([]any); len(arr) != 0 {
			t.Errorf("hooks[%s] = %+v after uninstall, want empty", s.event, arr)
		}
	}
	if pre, _ := hooks["PreToolUse"].([]any); len(pre) != 1 {
		t.Errorf("PreToolUse = %+v, want 1 (untouched by round-trip)", hooks["PreToolUse"])
	}
}

// ── isFleetopsHookCommand ───────────────────────────────────────────────

func TestIsFleetopsHookCommand(t *testing.T) {
	cases := []struct {
		cmd    string
		suffix string
		want   bool
	}{
		{"/usr/local/bin/fleetops hook notify", notifySuf, true},
		{"fleetops hook notify", notifySuf, true},
		{"some-other-tool notify", notifySuf, false},
		{"fleetops hook resume", notifySuf, false}, // doesn't end in "hook notify"
		{"/opt/other/fleetops-lookalike hook notify", notifySuf, true},
		{"/usr/local/bin/fleetops hook session-start", startSuffix, true},
		{"/usr/local/bin/fleetops hook session-end", endSuffix, true},
		// wrong suffix for the command: notify must NOT match session-start's
		// suffix, so the events never cross-remove each other.
		{"/usr/local/bin/fleetops hook notify", startSuffix, false},
		{"/usr/local/bin/fleetops hook session-start", notifySuf, false},
	}
	for _, c := range cases {
		if got := isFleetopsHookCommand(c.cmd, c.suffix); got != c.want {
			t.Errorf("isFleetopsHookCommand(%q, %q) = %v, want %v", c.cmd, c.suffix, got, c.want)
		}
	}
}

// TestFleetopsHookSpecs pins the exact set of managed events + suffixes so a
// future edit that drops or renames one is caught here.
func TestFleetopsHookSpecs(t *testing.T) {
	specs := fleetopsHookSpecs()
	want := map[string]string{
		"Notification": "hook notify",
		"SessionStart": "hook session-start",
		"SessionEnd":   "hook session-end",
	}
	if len(specs) != len(want) {
		t.Fatalf("got %d specs, want %d: %+v", len(specs), len(want), specs)
	}
	for _, s := range specs {
		if w, ok := want[s.eventName]; !ok || w != s.subcommandSuffix {
			t.Errorf("spec %+v not in expected set %+v", s, want)
		}
	}
}

func deepCopyJSON(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}
