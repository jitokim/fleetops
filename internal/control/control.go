// Package control re-drives a stalled loop by re-sending its prompt to the
// terminal surface that hosts it — abstracted over the multiplexer
// (orca/cmux/tmux) so a fleet board on any terminal can resume a loop
// (DESIGN.md: pluggable ports). Observation works everywhere; actuation
// degrades gracefully when no backend is available (see internal/tui's
// manual resume hint).
package control

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// actuationTimeout bounds a single typed-action exec call (Resume/Approve/
// Focus/Interrupt) so a wedged multiplexer CLI never hangs the TUI — Spawn
// already has its own, longer per-step timeouts (see orca.go/tmux.go), so it
// doesn't use this.
const actuationTimeout = 5 * time.Second

// runWithTimeout runs argv[0] with argv[1:] bounded by actuationTimeout —
// the shared exec path for every backend's Resume/Approve/Focus/Interrupt,
// matching the same never-hang discipline availabilityTimeout already
// enforces on Locate/Available (see cmux.go).
func runWithTimeout(argv []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), actuationTimeout)
	defer cancel()
	return exec.CommandContext(ctx, argv[0], argv[1:]...).Run()
}

// encodeCwd applies Claude Code's own project-dir encoding to a real
// (unencoded) absolute path — both "/" AND "." become "-" (verified:
// "/Users/imac/.claude-mem/observer-sessions" →
// "-Users-imac--claude-mem-observer-sessions"). Deliberately duplicated
// (not imported) from internal/claude.encodeCwd — keeps this package's
// dependency footprint at zero internal packages (it's a pure actuation
// layer, DESIGN.md's pluggable-ports boundary), for a stable, already-tested
// 2-line function. Keep both copies in sync if the encoding scheme ever
// changes. Using the exact same scheme here (rather than the old bare
// "/"→"-" replace) is what lets a dot-containing project path (e.g.
// "~/.claude-mem/...") actuate via a matched surface instead of always
// degrading to a manual hint.
func encodeCwd(realPath string) string {
	encoded := strings.ReplaceAll(realPath, "/", "-")
	return strings.ReplaceAll(encoded, ".", "-")
}

// Target is a controllable terminal surface hosting a loop.
type Target struct {
	Backend string // "orca" | "cmux" | "tmux"
	ID      string // orca terminal handle ("term_abc123") / cmux surface ref ("surface:2") / tmux pane id ("%3")
	Cwd     string
}

// Controller locates and re-drives loops on one multiplexer backend.
type Controller interface {
	Name() string
	Available() bool                         // backend usable right now
	Locate(projectDir string) (Target, bool) // match ANY surface by encoded cwd — for attach/Focus, where a bare shell is a fine target
	// LocateClaude is like Locate, but returns ONLY a surface confirmed to be
	// running `claude` (never a bare shell tab that merely shares the
	// directory). Required before any typed/destructive actuation
	// (Resume/Approve/Interrupt, and the TUI's kill) — see DESIGN.md and the
	// hardening-slice P0-3 rationale: Locate's permissive multi-tier
	// fallback exists for attach, and using it for typed actions can drive
	// keystrokes into the wrong terminal.
	//
	// ok is also false when MORE THAN ONE claude surface matches projectDir
	// (e.g. two claude panes/terminals open in the same directory) — this is
	// the authoritative backstop for the same wrong-terminal hazard the
	// TUI's keypress-time fleet-ambiguity guard exists to catch (see
	// Model.refuseIfAmbiguous): the TUI check is the fast/friendly path with
	// a good error message, but LocateClaude must refuse on its own too,
	// since it's the last thing standing between a keystroke and a real
	// terminal.
	LocateClaude(projectDir string) (Target, bool)
	Resume(t Target, prompt string) error // re-send prompt + submit
	Focus(t Target) error                 // bring the surface to the front (attach)
	Approve(t Target) error               // accept the default option at a gate (bare Enter)
	Spawn(cwd, goal string) error         // start a brand new claude loop in cwd
	Interrupt(t Target) error             // stop the current turn (Esc) without killing the process
}

// Resolve returns the first available controller: orca preferred (the
// captain's own environment), cmux then tmux as fallbacks; ok is false if
// none of the three backends is available.
func Resolve() (Controller, bool) {
	for _, c := range []Controller{orcaController{}, cmuxController{}, tmuxController{}} {
		if c.Available() {
			return c, true
		}
	}
	return nil, false
}
