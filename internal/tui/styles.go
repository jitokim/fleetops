package tui

import (
	"github.com/charmbracelet/lipgloss"
	"github.com/jitokim/missionctl/internal/domain"
)

// Control-room palette. Cyan accent (telemetry, not the overused acid-green);
// semantic state colors kept separate from the accent.
var (
	cAccent = lipgloss.Color("#4fd6e0")
	cInk    = lipgloss.Color("#c9d4de")
	cDim    = lipgloss.Color("#7a8896")
	cFaint  = lipgloss.Color("#4f5c69")
	cLine   = lipgloss.Color("#20303c")
	cSel    = lipgloss.Color("#132430")
	cRun    = lipgloss.Color("#46d98a")
	cGate   = lipgloss.Color("#ffb236")
	cAmber  = lipgloss.Color("#ffb236")
	cRed    = lipgloss.Color("#ff6b6b")

	stTitle  = lipgloss.NewStyle().Foreground(cAccent).Bold(true)
	stFaint  = lipgloss.NewStyle().Foreground(cFaint)
	stDim    = lipgloss.NewStyle().Foreground(cDim)
	stInk    = lipgloss.NewStyle().Foreground(cInk)
	stHeader = lipgloss.NewStyle().Foreground(cFaint).Bold(true)
	stKeybar = lipgloss.NewStyle().Foreground(cDim).
			BorderTop(true).BorderStyle(lipgloss.NormalBorder()).BorderForeground(cLine).
			PaddingTop(0)
	stKey    = lipgloss.NewStyle().Foreground(cAccent).Bold(true)
	stDetail = lipgloss.NewStyle().Foreground(cInk).
			BorderTop(true).BorderStyle(lipgloss.NormalBorder()).BorderForeground(cLine).
			PaddingTop(1)
	stSelRow = lipgloss.NewStyle().Background(cSel)
)

// stateColor / stateLabel encode loop state as form + text so it reads at a glance.
func stateColor(l domain.Loop) lipgloss.Color {
	switch l.State {
	case domain.StateStalled:
		if l.Stall == domain.StallRateLimit || l.Stall == domain.StallTokenOut {
			return cRed
		}
		return cAmber
	case domain.StateGate:
		return cGate
	case domain.StateRunning:
		return cRun
	default:
		return cDim
	}
}

func stateLabel(l domain.Loop) string {
	switch l.State {
	case domain.StateStalled:
		if l.Stall == domain.StallRateLimit {
			return "✗ 429"
		}
		return "◆ STALLED"
	case domain.StateRunning:
		return "● running"
	case domain.StateGate:
		return "◆ gate"
	case domain.StateIdle:
		return "· idle"
	default:
		return string(l.State)
	}
}
