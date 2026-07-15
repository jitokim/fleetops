// missionctl — fleet cockpit for Claude Code loops. Launches the Bubble Tea TUI.
package main

import (
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jitokim/missionctl/internal/tui"
)

func main() {
	p := tea.NewProgram(tui.New(), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "missionctl:", err)
		os.Exit(1)
	}
}
