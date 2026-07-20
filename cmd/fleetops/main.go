// fleetops — fleet cockpit for Claude Code loops.
//
// No args: launch the Bubble Tea TUI (the fleet cockpit).
// `--demo`: launch the same TUI seeded with a fixed synthetic fleet instead
// of scanning ~/.claude/projects — no real data read, nothing written to
// ~/.fleetops (see internal/tui.NewDemo).
// Subcommands: `hook notify|session-start|session-end` (Claude Code hook
// entry points, see hook.go), `hooks install|uninstall` (register/remove
// those hooks in ~/.claude/settings.json, see hooks.go), and
// `report --since 24h` (a plain-text summary of the append-only event
// history, internal/events — see report.go).
package main

import (
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jitokim/fleetops/internal/control"
	"github.com/jitokim/fleetops/internal/tui"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "hook":
			runHookCmd(os.Args[2:])
			return
		case "hooks":
			runHooksCmd(os.Args[2:])
			return
		case "report":
			runReportCmd(os.Args[2:])
			return
		case "--demo":
			runTUI(true)
			return
		case "help", "--help", "-h":
			fmt.Print(helpText())
			return
		default:
			fmt.Fprintf(os.Stderr, "fleetops: unknown command %q\n", os.Args[1])
			os.Exit(1)
		}
	}
	runTUI(false)
}

// helpText is `fleetops --help`/`-h`/`help`'s full output: a one-line
// description, every subcommand, and the TUI's keymap.
//
// fix/exit-gate-ux (UX judge item 6, "cheap credibility fix"): `--help`
// used to fall straight through to the unknown-command branch and exit 1
// — anyone's very first reflex with an unfamiliar CLI reads that as a
// broken/abandoned tool, regardless of how solid the rest of it is.
func helpText() string {
	return `fleetops — fleet cockpit for Claude Code loops: observes running Claude
Code sessions and lets you approve/resume/inject/kill them from one TUI.

Usage:
  fleetops                     launch the fleet cockpit (TUI)
  fleetops --demo              launch the TUI with a synthetic fleet — no real data, no disk writes
  fleetops report [--since D]  plain-text summary of the event history (default 24h)
  fleetops hooks install       register fleetops's Claude Code hooks (gate/idle detection)
  fleetops hooks uninstall     remove them
  fleetops hooks status        report whether the hooks are installed and healthy
  fleetops hook <event>        Claude Code hook entry point (notify|session-start|session-end)
                                  — invoked BY Claude Code itself, not typically run by hand
  fleetops help | --help | -h  show this help

TUI keymap:
  ↑/↓/g/G     move selection
  /           filter the fleet list
  ↵           attach to the selected loop's terminal
  a           approve a GATE
  r           resume a STALLED loop / re-drive a DRIFT loop with a hint
  i           inject an arbitrary prompt
  p           stop (interrupt) a running/gated loop
  k           kill (press twice within 3s to confirm)
  n           spawn a new loop (contract wizard)
  o           view the selected loop's raw log (pager)
  d           hide the selected loop (persists across restart)
  x           hide + remove registry entry (press twice within 3s to confirm)
  q           quit
`
}

// newModel picks tui.NewDemo() (a fixed synthetic fleet — no
// ~/.claude/projects scan, no ~/.fleetops writes) or tui.New() (the
// normal cockpit) — pulled out of runTUI so the --demo routing decision is
// directly testable without starting a real Bubble Tea program (Run()
// takes over the terminal and blocks on input, unsafe to invoke in a
// test).
func newModel(demo bool) tea.Model {
	if demo {
		return tui.NewDemo()
	}
	return tui.New()
}

func runTUI(demo bool) {
	if demo {
		// --demo ignores ~/.fleetops/settings.json entirely and always spawns
		// with the built-in ["claude"]. Demo mode's contract is "nothing real",
		// which has to include the person's own configuration: a demo that
		// picked up their spawn.command would behave differently on every
		// machine and would leak their local setup into any screenshot or
		// recording. The TUI already refuses every spawning key in demo mode
		// (isDemoBlockedKey), so this is defence in depth — it makes the
		// guarantee a property of the mechanism rather than of the current
		// keymap.
		control.UseDefaultSpawnCommand()
	}
	p := tea.NewProgram(newModel(demo), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "fleetops:", err)
		os.Exit(1)
	}
}
