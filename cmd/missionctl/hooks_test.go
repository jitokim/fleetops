package main

import (
	"encoding/json"
	"reflect"
	"testing"
)

const testCmd = "/usr/local/bin/missionctl hook notify"

func TestInstallNotifyHook_EmptySettings(t *testing.T) {
	settings := map[string]any{}

	if changed := installNotifyHook(settings, testCmd); !changed {
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

func TestInstallNotifyHook_PreservesOtherHookTypes(t *testing.T) {
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

	if changed := installNotifyHook(settings, testCmd); !changed {
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

func TestInstallNotifyHook_PreservesUnknownTopLevelFields(t *testing.T) {
	settings := map[string]any{
		"someFutureSetting": "keep me",
		"model":             "opus",
	}

	installNotifyHook(settings, testCmd)

	if settings["someFutureSetting"] != "keep me" {
		t.Error("unknown field someFutureSetting was dropped")
	}
	if settings["model"] != "opus" {
		t.Error("unrelated field model was dropped")
	}
}

func TestInstallNotifyHook_AlreadyInstalled_NoChange(t *testing.T) {
	settings := map[string]any{
		"hooks": map[string]any{
			"Notification": []any{
				map[string]any{"matcher": "", "hooks": []any{map[string]any{"type": "command", "command": testCmd}}},
			},
		},
	}
	before := deepCopyJSON(t, settings)

	if changed := installNotifyHook(settings, testCmd); changed {
		t.Error("expected changed=false when already installed")
	}
	if !reflect.DeepEqual(settings, before) {
		t.Errorf("settings mutated despite no-op: got %+v, want unchanged %+v", settings, before)
	}
}

func TestUninstallNotifyHook_RemovesOnlyOurs(t *testing.T) {
	settings := map[string]any{
		"hooks": map[string]any{
			"Notification": []any{
				map[string]any{"matcher": "", "hooks": []any{map[string]any{"type": "command", "command": testCmd}}},
				map[string]any{"matcher": "", "hooks": []any{map[string]any{"type": "command", "command": "some-other-tool notify"}}},
			},
		},
	}

	if changed := uninstallNotifyHook(settings); !changed {
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

func TestUninstallNotifyHook_PreservesOtherHookTypes(t *testing.T) {
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

	if changed := uninstallNotifyHook(settings); !changed {
		t.Fatal("expected changed=true")
	}
	hooks := settings["hooks"].(map[string]any)
	if _, ok := hooks["PreToolUse"]; !ok {
		t.Error("PreToolUse was dropped by uninstall, want preserved")
	}
}

func TestUninstallNotifyHook_NotInstalled_NoChange(t *testing.T) {
	settings := map[string]any{}
	if changed := uninstallNotifyHook(settings); changed {
		t.Error("expected changed=false when not installed")
	}
}

func TestUninstallNotifyHook_EmptySettings_NoChange(t *testing.T) {
	if changed := uninstallNotifyHook(map[string]any{}); changed {
		t.Error("expected changed=false for empty settings")
	}
}

func TestIsMissionctlNotifyCommand(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		{"/usr/local/bin/missionctl hook notify", true},
		{"missionctl hook notify", true},
		{"some-other-tool notify", false},
		{"missionctl hook resume", false}, // doesn't end in "hook notify"
		{"/opt/other/missionctl-lookalike hook notify", true},
	}
	for _, c := range cases {
		if got := isMissionctlNotifyCommand(c.cmd); got != c.want {
			t.Errorf("isMissionctlNotifyCommand(%q) = %v, want %v", c.cmd, got, c.want)
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
