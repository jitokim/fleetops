package tui

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// TestDumpDemoView writes one full-screen frame of the --demo fleet to a file
// as raw ANSI, so a marketing screenshot can be produced with `freeze`. It is
// NOT a normal test: it is SKIPPED unless FLEETOPS_DUMP_DEMO is set, so `go
// test ./...` (and CI) never run it and it adds no user-facing surface — the
// preferred "test-only, env-gated dump" path over a hidden --demo flag.
//
// It renders exactly what a real `fleetops --demo` session shows: NewDemo()'s
// synthetic fleet, at a fixed terminal size, with the cursor parked on the
// v0.9.0 delegating loop (flock-audit) so the DETAIL panel's TAIL row carries
// the "delegating: code-reviewer — …" string the feature produces.
//
// Knobs (all optional, env-var):
//   - FLEETOPS_DUMP_DEMO  — any non-empty value ENABLES the dump (else skipped)
//   - FLEETOPS_DUMP_OUT   — output path (default: demo-frame.ansi in the cwd)
//   - FLEETOPS_DUMP_W / _H — terminal width/height (default: 120x40)
//
// The global color profile is forced to TrueColor for the duration of this
// dump so the frame carries real ANSI color escapes (state colors, badges) —
// under `go test` stdout is not a TTY, so lipgloss would otherwise strip them
// to plain text and the screenshot would be monochrome. Restored on exit so no
// other test in the package is affected.
func TestDumpDemoView(t *testing.T) {
	if os.Getenv("FLEETOPS_DUMP_DEMO") == "" {
		t.Skip("set FLEETOPS_DUMP_DEMO=1 to write the --demo frame for a freeze screenshot")
	}

	prevProfile := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prevProfile)

	m := NewDemo()
	m.w = envInt("FLEETOPS_DUMP_W", 120)
	m.h = envInt("FLEETOPS_DUMP_H", 40)

	// Park the cursor on the delegating loop so the DETAIL panel shows its
	// "delegating: …" TAIL row — the whole point of the v0.9.0 screenshot.
	for i := range m.loops {
		if m.loops[i].Project == "flock-audit" {
			m.cursor = i
			break
		}
	}

	out := m.View()

	path := os.Getenv("FLEETOPS_DUMP_OUT")
	if path == "" {
		path = "demo-frame.ansi"
	}
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		t.Fatalf("writing demo frame to %s: %v", path, err)
	}
	t.Logf("wrote %dx%d demo frame (%d bytes) to %s", m.w, m.h, len(out), path)
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}
