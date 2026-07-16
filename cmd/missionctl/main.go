// missionctl — fleet cockpit for Claude Code loops.
//
// No args: launch the Bubble Tea TUI (the fleet cockpit).
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
	"github.com/jitokim/missionctl/internal/tui"
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
		case "help", "--help", "-h":
			fmt.Print(helpText())
			return
		default:
			fmt.Fprintf(os.Stderr, "missionctl: unknown command %q\n", os.Args[1])
			os.Exit(1)
		}
	}
	runTUI()
}

// helpText is `missionctl --help`/`-h`/`help`'s full output: a one-line
// description, every subcommand, and the TUI's keymap.
//
// fix/exit-gate-ux (UX judge item 6, "cheap credibility fix"): `--help`
// used to fall straight through to the unknown-command branch and exit 1
// — anyone's very first reflex with an unfamiliar CLI reads that as a
// broken/abandoned tool, regardless of how solid the rest of it is.
func helpText() string {
	return `missionctl — fleet cockpit for Claude Code loops: observes running Claude
Code sessions and lets you approve/resume/inject/kill them from one TUI.

Usage:
  missionctl                     launch the fleet cockpit (TUI)
  missionctl report [--since D]  plain-text summary of the event history (default 24h)
  missionctl hooks install       register missionctl's Claude Code hooks (gate/idle detection)
  missionctl hooks uninstall     remove them
  missionctl hook <event>        Claude Code hook entry point (notify|session-start|session-end)
                                  — invoked BY Claude Code itself, not typically run by hand
  missionctl help | --help | -h  show this help

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
  q           quit
`
}

func runTUI() {
	p := tea.NewProgram(tui.New(), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "missionctl:", err)
		os.Exit(1)
	}
}
