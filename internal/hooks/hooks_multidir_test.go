package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// ── SettingsLocations ────────────────────────────────────────────────────────

// Zero-config: no accounts.json ⇒ exactly the default location, so
// install/status behave byte-identically for single-account users.
func TestSettingsLocations_ZeroConfig_DefaultOnly(t *testing.T) {
	def := "/home/user/.claude/settings.json"
	locs := SettingsLocations(def, filepath.Join(t.TempDir(), "missing.json"))

	if len(locs) != 1 {
		t.Fatalf("locations = %+v, want exactly the default", locs)
	}
	if locs[0].Label != DefaultLabel || locs[0].Path != def {
		t.Fatalf("default location = %+v, want {%q, %q}", locs[0], DefaultLabel, def)
	}
}

// With aliases, each alias config dir contributes "<configDir>/settings.json",
// default first, ordered by alias name — the set of files install must touch so
// a non-default account's loops fire our hooks.
func TestSettingsLocations_WithAliases_OnePerConfigDir(t *testing.T) {
	def := "/home/user/.claude/settings.json"
	path := writeJSON(t, `{"aliases":{"company":"/home/user/.claude-work","personal":"/home/user/.claude-personal"}}`)

	locs := SettingsLocations(def, path)

	want := []ConfigDirLocation{
		{Label: DefaultLabel, Path: def},
		{Label: "company", Path: "/home/user/.claude-work/settings.json"},
		{Label: "personal", Path: "/home/user/.claude-personal/settings.json"},
	}
	if len(locs) != len(want) {
		t.Fatalf("locations = %+v, want %+v", locs, want)
	}
	for i := range want {
		if locs[i] != want[i] {
			t.Fatalf("location[%d] = %+v, want %+v", i, locs[i], want[i])
		}
	}
}

// An alias naming the default config dir must not double the default location.
func TestSettingsLocations_AliasOnDefaultDir_Deduped(t *testing.T) {
	def := "/home/user/.claude/settings.json"
	path := writeJSON(t, `{"aliases":{"main":"/home/user/.claude"}}`)

	locs := SettingsLocations(def, path)

	if len(locs) != 1 || locs[0].Path != def {
		t.Fatalf("locations = %+v, want a single deduped default", locs)
	}
}

// A malformed accounts.json must not drop the default location.
func TestSettingsLocations_MalformedConfig_KeepsDefault(t *testing.T) {
	def := "/home/user/.claude/settings.json"
	path := writeJSON(t, `{"aliases":{},"bindings":[{"path":"/x","alias":"missing"}]}`)

	locs := SettingsLocations(def, path)

	if len(locs) != 1 || locs[0].Path != def {
		t.Fatalf("locations = %+v, want just the default when accounts.json is malformed", locs)
	}
}

// ── HealthAllAt ──────────────────────────────────────────────────────────────

// The half of CRITICAL-1 that makes a per-alias gap VISIBLE: hooks installed in
// the default dir but MISSING in "company" must report OK for default and
// not-OK for company — not one blurred verdict that hides the silent account.
func TestHealthAllAt_InstalledInDefaultMissingInAlias_ReportsPerDir(t *testing.T) {
	home := t.TempDir()
	binary := filepath.Join(home, "fleetops")
	mustTouch(t, binary)

	// default settings.json fully installed; company's dir has no settings.json.
	defPath := filepath.Join(home, ".claude", "settings.json")
	writeSettingsFile(t, defPath, allInstalled(binary))
	companyDir := filepath.Join(home, ".claude-work")
	accountsPath := writeJSON(t, `{"aliases":{"company":"`+companyDir+`"}}`)

	healths := HealthAllAt(defPath, accountsPath, existsOnly(binary))

	if len(healths) != 2 {
		t.Fatalf("got %d config-dir reports, want 2 (default + company)", len(healths))
	}
	byLabel := map[string]ConfigDirHealth{}
	for _, h := range healths {
		byLabel[h.Location.Label] = h
	}
	if !byLabel[DefaultLabel].Report.OK {
		t.Errorf("default report not OK, want OK (hooks installed there)")
	}
	if byLabel["company"].Report.OK {
		t.Errorf("company report OK, want NOT OK — its settings.json is missing, so its loops record nothing")
	}
}

// ── Merge ────────────────────────────────────────────────────────────────────

// The launch banner consumes the merged report: OK only if EVERY dir is OK, so
// a missing-in-company state flips the whole thing not-OK and the banner fires.
func TestMerge_NotOKIfAnyDirNotOK(t *testing.T) {
	healths := []ConfigDirHealth{
		{Location: ConfigDirLocation{Label: DefaultLabel}, Report: Report{OK: true}},
		{Location: ConfigDirLocation{Label: "company"}, Report: Report{OK: false,
			Events: []EventStatus{{Event: "SessionStart", State: StateMissing}}}},
	}

	merged := Merge(healths)

	if merged.OK {
		t.Fatal("merged report OK despite company being not-OK; the banner would never fire")
	}
	if len(merged.Missing()) == 0 {
		t.Fatal("merged report lost company's Missing events — HasStalePath/Missing must answer across all dirs")
	}
}

// A stale-path in ANY dir must surface through the merged report so the scarier
// banner message is chosen.
func TestMerge_HasStalePathAcrossDirs(t *testing.T) {
	healths := []ConfigDirHealth{
		{Report: Report{OK: true}},
		{Report: Report{OK: false, Events: []EventStatus{{Event: "SessionStart", State: StateStalePath, Binary: "/gone"}}}},
	}
	if !Merge(healths).HasStalePath() {
		t.Fatal("merged report did not surface a stale path present in a non-default dir")
	}
}

// All dirs OK ⇒ merged OK (zero-config's single-dir case must read healthy).
func TestMerge_AllOK(t *testing.T) {
	merged := Merge([]ConfigDirHealth{{Report: Report{OK: true}}})
	if !merged.OK {
		t.Fatal("a single all-OK dir merged to not-OK")
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func writeJSON(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "accounts.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write accounts.json: %v", err)
	}
	return path
}

func writeSettingsFile(t *testing.T, path string, settings map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	data, err := json.Marshal(settings)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write settings: %v", err)
	}
}

func mustTouch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("touch: %v", err)
	}
}
