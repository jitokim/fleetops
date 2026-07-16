package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

// fix/exit-gate-ux (UX judge item 6): `missionctl --help`/`-h`/`help` used
// to fall through to the unknown-command branch and exit 1. These tests
// pin helpText()'s content, and TestMain_HelpAliases_PrintHelpAndReturn
// drives the REAL main() (not a mirror of its switch) for all three
// spellings — main()'s "help"/"--help"/"-h" cases just print and return
// (no os.Exit), so it's safe to invoke directly with os.Args swapped and
// stdout captured, mirroring hook_test.go's withStdin pattern.

func TestHelpText_HasOneLineDescription(t *testing.T) {
	got := helpText()
	if !strings.Contains(got, "fleet cockpit for Claude Code loops") {
		t.Errorf("expected a one-line description mentioning the fleet cockpit, got:\n%s", got)
	}
}

func TestHelpText_ListsAllSubcommands(t *testing.T) {
	got := helpText()
	for _, want := range []string{
		"missionctl report",
		"missionctl hooks install",
		"missionctl hooks uninstall",
		"missionctl hook <event>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected help text to mention %q, got:\n%s", want, got)
		}
	}
}

func TestHelpText_ListsTUIKeymap(t *testing.T) {
	got := helpText()
	if !strings.Contains(got, "TUI keymap") {
		t.Errorf("expected a TUI keymap section, got:\n%s", got)
	}
	for _, key := range []string{"a ", "r ", "i ", "p ", "k ", "n ", "o ", "q "} {
		if !strings.Contains(got, key) {
			t.Errorf("expected the keymap to mention key %q, got:\n%s", key, got)
		}
	}
}

// withStdout swaps os.Stdout for a pipe, runs fn, and returns everything fn
// wrote — the output-capture mirror of hook_test.go's withStdin.
func withStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	fn()
	os.Stdout = orig
	_ = w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// TestMain_HelpAliases_PrintHelpAndReturn drives the REAL main() (not a
// mirror of its switch) for each of the three "--help" spellings —
// os.Args[1] set to the alias under test, stdout captured. main()'s
// help-case path only prints and returns (never os.Exit), so this is safe
// to call in-process, unlike the unknown-command path (which DOES
// os.Exit(1) and would kill the test binary).
func TestMain_HelpAliases_PrintHelpAndReturn(t *testing.T) {
	for _, alias := range []string{"help", "--help", "-h"} {
		t.Run(alias, func(t *testing.T) {
			origArgs := os.Args
			os.Args = []string{"missionctl", alias}
			defer func() { os.Args = origArgs }()

			out := withStdout(t, main)
			if !strings.Contains(out, "fleet cockpit for Claude Code loops") {
				t.Errorf("missionctl %s printed unexpected output:\n%s", alias, out)
			}
			if !strings.Contains(out, "TUI keymap") {
				t.Errorf("missionctl %s did not print the TUI keymap:\n%s", alias, out)
			}
		})
	}
}
