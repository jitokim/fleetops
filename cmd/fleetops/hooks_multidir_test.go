package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jitokim/fleetops/internal/hooks"
)

// installHooksAt into a config dir that does not yet exist must CREATE the dir
// and settings.json and install every managed event — the mechanism that lets a
// non-default account's loops fire our hooks. A second call is a no-op
// (idempotent), proving install can be re-run safely.
func TestInstallHooksAt_CreatesDirAndIsIdempotent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist-yet")
	path := filepath.Join(dir, "settings.json")
	exe := "/usr/local/bin/fleetops"

	installed, err := installHooksAt(path, exe)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(installed) != len(fleetopsHookSpecs()) {
		t.Fatalf("installed %v, want every managed event", installed)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("settings.json was not created at %s: %v", path, err)
	}

	// Idempotent: nothing new the second time.
	again, err := installHooksAt(path, exe)
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("second install reported %v, want nothing (already installed)", again)
	}
}

// uninstallHooksAt fully reverses installHooksAt in an alias dir — so uninstall
// never strands a hook behind in a non-default config dir.
func TestUninstallHooksAt_ReversesInstall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	exe := "/usr/local/bin/fleetops"

	if _, err := installHooksAt(path, exe); err != nil {
		t.Fatalf("install: %v", err)
	}
	changed, err := uninstallHooksAt(path)
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if !changed {
		t.Fatal("uninstall reported no change on a freshly-installed dir")
	}

	// Health of the leftover file must now be all-Missing.
	report := hooks.HealthAt(path, func(string) bool { return true })
	if report.OK {
		t.Fatal("hooks still healthy after uninstall — an entry was stranded in the alias dir")
	}

	// And uninstall again is a no-op.
	if changed, _ := uninstallHooksAt(path); changed {
		t.Fatal("second uninstall reported a change; want none")
	}
}

// ── FINDING #2 (2nd review): a bad dir must not stop the others ───────────────

// installHooksAllAt must CONTINUE past a per-dir failure (a hand-broken
// settings.json in one alias) so every OTHER dir still gets hooks — the "each
// dir independent" contract. The prior os.Exit(1)-on-first-error left every
// alias after the broken one without hooks (recording nothing).
func TestInstallHooksAllAt_OneBadDirDoesNotStopOthers(t *testing.T) {
	exe := "/usr/local/bin/fleetops"
	good1 := filepath.Join(t.TempDir(), "settings.json")
	bad := filepath.Join(t.TempDir(), "settings.json")
	good2 := filepath.Join(t.TempDir(), "settings.json")
	// A hand-broken settings.json in the MIDDLE dir — loadSettings can't parse it.
	if err := os.WriteFile(bad, []byte("{ this is not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	results := installHooksAllAt([]hooks.ConfigDirLocation{
		{Label: "default", Path: good1},
		{Label: "company", Path: bad},
		{Label: "personal", Path: good2},
	}, exe)

	if len(results) != 3 {
		t.Fatalf("got %d results, want 3 — the loop aborted early instead of visiting every dir", len(results))
	}
	if results[0].err != nil || len(results[0].installed) == 0 {
		t.Errorf("first dir should have installed cleanly: %+v", results[0])
	}
	if results[1].err == nil {
		t.Error("the broken middle dir should have reported an error")
	}
	// The load-bearing assertion: the dir AFTER the broken one still installed.
	if results[2].err != nil || len(results[2].installed) == 0 {
		t.Errorf("the dir after the broken one must still install — the loop must not abort on the first error: %+v", results[2])
	}
	if report := hooks.HealthAt(good2, func(string) bool { return true }); !report.OK {
		t.Error("hooks did not actually land in the dir after the broken one")
	}
}

// uninstallHooksAllAt has the same each-dir-independent contract: a broken
// settings.json in one alias must not strand stale hooks in every other alias.
func TestUninstallHooksAllAt_OneBadDirDoesNotStopOthers(t *testing.T) {
	exe := "/usr/local/bin/fleetops"
	good1 := filepath.Join(t.TempDir(), "settings.json")
	bad := filepath.Join(t.TempDir(), "settings.json")
	good2 := filepath.Join(t.TempDir(), "settings.json")
	for _, p := range []string{good1, good2} {
		if _, err := installHooksAt(p, exe); err != nil {
			t.Fatalf("seed install %s: %v", p, err)
		}
	}
	if err := os.WriteFile(bad, []byte("{ broken"), 0o644); err != nil {
		t.Fatal(err)
	}

	results := uninstallHooksAllAt([]hooks.ConfigDirLocation{
		{Label: "default", Path: good1},
		{Label: "company", Path: bad},
		{Label: "personal", Path: good2},
	})

	if len(results) != 3 {
		t.Fatalf("got %d results, want 3 — the loop aborted early", len(results))
	}
	if !results[0].changed {
		t.Error("first dir should have had hooks removed")
	}
	if results[1].err == nil {
		t.Error("the broken middle dir should have reported an error")
	}
	if !results[2].changed || results[2].err != nil {
		t.Errorf("the dir after the broken one must still be uninstalled: %+v", results[2])
	}
}

// formatMultiHookStatus must render one legible section PER config dir, so a
// per-alias gap ("company: missing") is visible rather than silent.
func TestFormatMultiHookStatus_PerDirSections(t *testing.T) {
	healths := []hooks.ConfigDirHealth{
		{Location: hooks.ConfigDirLocation{Label: "default", Path: "/home/user/.claude/settings.json"},
			Report: hooks.Report{OK: true}},
		{Location: hooks.ConfigDirLocation{Label: "company", Path: "/home/user/.claude-work/settings.json"},
			Report: hooks.Report{OK: false, Events: []hooks.EventStatus{{Event: "SessionStart", State: hooks.StateMissing}}}},
	}

	out := formatMultiHookStatus(healths)

	for _, want := range []string{"default", "company", "/home/user/.claude-work/settings.json", "not fully installed"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q:\n%s", want, out)
		}
	}
}
