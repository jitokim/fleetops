package tui

import (
	"github.com/charmbracelet/lipgloss"
	"github.com/jitokim/missionctl/internal/domain"
)

// Control-room palette — the design tokens of the approved mockup
// (html-artifacts/mission-control-tui.html). Terminal rendering can't
// replicate the mockup's CSS letter-spacing; bold/caps stand in for that
// visual weight where it mattered (e.g. the ◎ MISSIONCTL logo).
var (
	cChrome = lipgloss.Color("#161d25")
	cLine   = lipgloss.Color("#20303c")
	cInk    = lipgloss.Color("#c9d4de")
	cDim    = lipgloss.Color("#7a8896")
	cFaint  = lipgloss.Color("#4f5c69")
	cAccent = lipgloss.Color("#4fd6e0") // cyan
	cBlue   = lipgloss.Color("#5aa2ff")
	cAmber  = lipgloss.Color("#ffb236")
	cGreen  = lipgloss.Color("#46d98a")
	cRed    = lipgloss.Color("#ff6b6b")
	cSel    = lipgloss.Color("#132430")

	// dark-on-bright text for badges/key-chips sitting on an amber/red fill.
	cAmberInk = lipgloss.Color("#1a1205")
	cRedInk   = lipgloss.Color("#1a0505")

	stTitle  = lipgloss.NewStyle().Foreground(cAccent).Bold(true)
	stFaint  = lipgloss.NewStyle().Foreground(cFaint)
	stDim    = lipgloss.NewStyle().Foreground(cDim)
	stInk    = lipgloss.NewStyle().Foreground(cInk)
	stHeader = lipgloss.NewStyle().Foreground(cFaint).Bold(true)
	stKey    = lipgloss.NewStyle().Foreground(cAccent).Bold(true)
	stSelRow = lipgloss.NewStyle().Background(cSel)
	stLive   = lipgloss.NewStyle().Foreground(cGreen)

	stKeybar = lipgloss.NewStyle().Foreground(cDim).Background(cChrome).
			BorderTop(true).BorderStyle(lipgloss.NormalBorder()).BorderForeground(cLine)
	stDetail = lipgloss.NewStyle().Foreground(cInk).
			BorderTop(true).BorderStyle(lipgloss.NormalBorder()).BorderForeground(cLine).
			PaddingTop(1)

	// the mockup's amber "GATE NEEDS YOU" badge, repurposed honestly for stalls.
	stBadgeStalled = lipgloss.NewStyle().Foreground(cAmberInk).Background(cAmber).Bold(true).Padding(0, 1)

	stKeyChipAmber = lipgloss.NewStyle().Foreground(cAmberInk).Background(cAmber).Bold(true).Padding(0, 1)
	stKeyChipRed   = lipgloss.NewStyle().Foreground(cRedInk).Background(cRed).Bold(true).Padding(0, 1)

	stCalloutAmber = lipgloss.NewStyle().Foreground(cInk).
			Border(lipgloss.RoundedBorder()).BorderForeground(cAmber).Padding(0, 1)
	stCalloutRed = lipgloss.NewStyle().Foreground(cInk).
			Border(lipgloss.RoundedBorder()).BorderForeground(cRed).Padding(0, 1)
)

// stateStyle / stateLabel encode loop state as form + text so it reads at a
// glance. The mockup uses blue for a running loop (not green — green is
// reserved for "done"/"live", which the observation MVP doesn't have yet).
func stateStyle(l domain.Loop) lipgloss.Style {
	s := lipgloss.NewStyle().Foreground(stateColor(l))
	switch l.State {
	case domain.StateStalled, domain.StateGate, domain.StateRunning, domain.StateDone, domain.StateDrift, domain.StateFailed:
		s = s.Bold(true)
	}
	return s
}

func stateColor(l domain.Loop) lipgloss.Color {
	switch l.State {
	case domain.StateStalled:
		if l.Stall == domain.StallRateLimit || l.Stall == domain.StallTokenOut || l.Stall == domain.StallGone {
			return cRed
		}
		return cAmber
	case domain.StateGate:
		return cAmber
	case domain.StateRunning:
		return cBlue
	case domain.StateDone:
		return cGreen
	case domain.StateDrift:
		return cRed
	case domain.StateFailed:
		return cRed
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
		if l.Stall == domain.StallGone {
			return "✗ gone"
		}
		return "◆ STALLED"
	case domain.StateRunning:
		return "● run"
	case domain.StateGate:
		return "◆ GATE"
	case domain.StateIdle:
		return "· idle"
	case domain.StateDone:
		return "✓ DONE"
	case domain.StateDrift:
		return "✗ DRIFT"
	case domain.StateFailed:
		return "✗ FAILED"
	default:
		return string(l.State)
	}
}

// budgetStyle colors the BUDGET cell/row by how close a loop is to its
// token budget: dim normally, amber past 75%, red past 90% — an at-a-glance
// heads-up before a loop hits StallTokenOut.
func budgetStyle(l domain.Loop) lipgloss.Style {
	switch frac := l.BudgetFrac(); {
	case frac >= 0.9:
		return lipgloss.NewStyle().Foreground(cRed)
	case frac >= 0.75:
		return lipgloss.NewStyle().Foreground(cAmber)
	default:
		return stDim
	}
}

// oracleStyle colors the ORACLE cell/row by the loop's latest verdict:
// green for done, red for rejected, dim for progress or unbound/unjudged.
func oracleStyle(l domain.Loop) lipgloss.Style {
	if l.Last == nil {
		return stFaint
	}
	switch l.Last.Outcome {
	case domain.OutcomeDone:
		return lipgloss.NewStyle().Foreground(cGreen)
	case domain.OutcomeRejected:
		return lipgloss.NewStyle().Foreground(cRed)
	default:
		return stDim
	}
}

// noImproveStyle colors the N/I cell/row: dim normally, amber one short of
// the limit, red at (or past) the limit — the same "heads up before the
// hard ceiling" pattern as budgetStyle. Unbound loops (no NoImproveLimit)
// are always dim, since "—" isn't a real number to threshold against.
func noImproveStyle(l domain.Loop) lipgloss.Style {
	if l.Goal.Text == "" || l.Goal.NoImproveLimit <= 0 {
		return stFaint
	}
	switch {
	case l.NoImprove >= l.Goal.NoImproveLimit:
		return lipgloss.NewStyle().Foreground(cRed)
	case l.NoImprove == l.Goal.NoImproveLimit-1:
		return lipgloss.NewStyle().Foreground(cAmber)
	default:
		return stDim
	}
}
