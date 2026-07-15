// Package control re-drives a stalled loop by re-sending its prompt to the
// terminal surface that hosts it — abstracted over the multiplexer
// (orca/cmux/tmux) so a fleet board on any terminal can resume a loop
// (DESIGN.md: pluggable ports). Observation works everywhere; actuation
// degrades gracefully when no backend is available (see internal/tui's
// manual resume hint).
package control

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
	Locate(projectDir string) (Target, bool) // match surface by encoded cwd
	Resume(t Target, prompt string) error    // re-send prompt + submit
	Focus(t Target) error                    // bring the surface to the front (attach)
	Approve(t Target) error                  // accept the default option at a gate (bare Enter)
	Spawn(cwd, goal string) error            // start a brand new claude loop in cwd
	Interrupt(t Target) error                // stop the current turn (Esc) without killing the process
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
