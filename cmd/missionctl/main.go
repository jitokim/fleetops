// missionctl — fleet cockpit for Claude Code loops.
//
// No args: launch the Bubble Tea TUI (the fleet cockpit).
// Subcommands: `hook notify|session-start|session-end` (Claude Code hook
// entry points, see hook.go) and `hooks install|uninstall` (register/remove
// those hooks in ~/.claude/settings.json, see hooks.go).
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
		default:
			fmt.Fprintf(os.Stderr, "missionctl: unknown command %q\n", os.Args[1])
			os.Exit(1)
		}
	}
	runTUI()
}

func runTUI() {
	p := tea.NewProgram(tui.New(), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "missionctl:", err)
		os.Exit(1)
	}
}
