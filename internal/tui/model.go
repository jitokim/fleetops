// Package tui is the fleet cockpit (Bubble Tea): aggregate list + right-pane
// detail + one-key action, refreshed from the Claude Code logs (seed spec §UX).
package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/jitokim/missionctl/internal/claude"
	"github.com/jitokim/missionctl/internal/domain"
)

type loopsMsg []domain.Loop
type tickMsg time.Time

const refreshEvery = 3 * time.Second

// scan is a tea.Cmd: rediscover the fleet from the logs.
func scan() tea.Msg {
	loops, _ := claude.DiscoverLoops(time.Now(), claude.ActiveWindow)
	return loopsMsg(loops)
}

func tick() tea.Cmd {
	return tea.Tick(refreshEvery, func(t time.Time) tea.Msg { return tickMsg(t) })
}

type Model struct {
	loops    []domain.Loop
	cursor   int
	w, h     int
	status   string
	lastScan time.Time
}

func New() Model { return Model{status: "watching ~/.claude/projects"} }

func (m Model) Init() tea.Cmd { return tea.Batch(scan, tick()) }

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
	case loopsMsg:
		m.loops = []domain.Loop(msg)
		if m.cursor >= len(m.loops) {
			m.cursor = maxInt(0, len(m.loops)-1)
		}
		m.lastScan = time.Now()
	case tickMsg:
		return m, tea.Batch(scan, tick())
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.loops)-1 {
				m.cursor++
			}
		case "g":
			m.cursor = 0
		case "G":
			m.cursor = maxInt(0, len(m.loops)-1)
		case "r":
			m.status = m.resumeHint()
		case "enter":
			m.status = "attach/open — TODO (needs cmux integration)"
		}
	}
	return m, nil
}

func (m Model) resumeHint() string {
	if sel, ok := m.selected(); ok && sel.State == domain.StateStalled {
		return fmt.Sprintf("resume %s → re-send prompt to its cmux pane (TODO: wire cmux send-keys)", sel.Project)
	}
	return "select a stalled loop to resume"
}

func (m Model) selected() (domain.Loop, bool) {
	if m.cursor >= 0 && m.cursor < len(m.loops) {
		return m.loops[m.cursor], true
	}
	return domain.Loop{}, false
}

func (m Model) View() string {
	var b strings.Builder

	// header
	stalled := 0
	for _, l := range m.loops {
		if l.State == domain.StateStalled {
			stalled++
		}
	}
	b.WriteString(stTitle.Render("◎ MISSIONCTL"))
	b.WriteString(stFaint.Render("  fleet cockpit"))
	b.WriteString("\n")
	summary := fmt.Sprintf("loops %d · %s %d stalled · window %s",
		len(m.loops), "⚠", stalled, claude.ActiveWindow)
	b.WriteString(stDim.Render(summary))
	b.WriteString("\n\n")

	// table
	b.WriteString(renderHeader())
	b.WriteString("\n")
	if len(m.loops) == 0 {
		b.WriteString(stFaint.Render("  no active Claude Code loops in the window.\n"))
	}
	for i, l := range m.loops {
		b.WriteString(renderRow(l, i == m.cursor))
		b.WriteString("\n")
	}

	// detail
	if sel, ok := m.selected(); ok {
		b.WriteString(renderDetail(sel))
	}

	// keybar
	b.WriteString("\n")
	b.WriteString(renderKeybar(m.status))
	return b.String()
}

// ── row rendering ──────────────────────────────────────────

const (
	wMarker = 2
	wName   = 20
	wState  = 12
	wLast   = 14
	wNote   = 30
)

func renderHeader() string {
	cells := []string{
		stHeader.Width(wMarker).Render(""),
		stHeader.Width(wName).Render("PROJECT"),
		stHeader.Width(wState).Render("STATE"),
		stHeader.Width(wLast).Render("LAST ACTIVITY"),
		stHeader.Width(wNote).Render("NOTE"),
	}
	return "  " + lipgloss.JoinHorizontal(lipgloss.Top, cells...)
}

func renderRow(l domain.Loop, sel bool) string {
	marker := " "
	if sel {
		marker = "▸"
	}
	st := lipgloss.NewStyle().Foreground(stateColor(l))
	note := ""
	if l.Stall != domain.StallNone {
		note = "⚠ " + string(l.Stall)
	}
	cells := []string{
		lipgloss.NewStyle().Foreground(cAccent).Width(wMarker).Render(marker),
		stInk.Width(wName).Render(trunc(l.Project, wName-1)),
		st.Width(wState).Render(stateLabel(l)),
		stDim.Width(wLast).Render(rel(time.Since(l.LastActivity))),
		st.Width(wNote).Render(trunc(note, wNote-1)),
	}
	row := lipgloss.JoinHorizontal(lipgloss.Top, cells...)
	if sel {
		row = stSelRow.Render(row)
	}
	return "  " + row
}

func renderDetail(l domain.Loop) string {
	var d strings.Builder
	d.WriteString(stTitle.Render("▸ " + l.Project))
	d.WriteString(stFaint.Render("  " + l.SessionID))
	d.WriteString("\n")
	d.WriteString(stFaint.Render("STATE   ") + lipgloss.NewStyle().Foreground(stateColor(l)).Render(stateLabel(l)))
	d.WriteString("\n")
	d.WriteString(stFaint.Render("LAST    ") + stInk.Render(rel(time.Since(l.LastActivity))+"  ("+l.LastActivity.Format("15:04:05")+")"))
	d.WriteString("\n")
	if l.Stall != domain.StallNone {
		d.WriteString(stFaint.Render("WHY     ") + lipgloss.NewStyle().Foreground(stateColor(l)).Render(string(l.Stall)))
		d.WriteString("\n")
		d.WriteString(stFaint.Render("        ") + stDim.Render("press r to resume (re-send prompt)"))
		d.WriteString("\n")
	}
	d.WriteString(stFaint.Render("LOG     ") + stDim.Render(l.Path))
	return stDetail.Render(d.String())
}

func renderKeybar(status string) string {
	keys := []string{
		stKey.Render("↑↓") + stDim.Render(" select"),
		stKey.Render("r") + stDim.Render(" resume"),
		stKey.Render("↵") + stDim.Render(" open"),
		stKey.Render("q") + stDim.Render(" quit"),
	}
	bar := strings.Join(keys, stFaint.Render("  ·  "))
	if status != "" {
		bar += "    " + stFaint.Render(status)
	}
	return stKeybar.Render(bar)
}

// ── helpers ────────────────────────────────────────────────

func rel(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func trunc(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
