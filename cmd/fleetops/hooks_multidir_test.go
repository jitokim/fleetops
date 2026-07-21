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
