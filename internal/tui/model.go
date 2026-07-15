// Package tui is the fleet cockpit (Bubble Tea): aggregate list + right-pane
// detail + one-key action, refreshed from the Claude Code logs (seed spec §UX).
// Visual language matches the approved mockup (html-artifacts/mission-control-tui.html).
package tui

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/jitokim/missionctl/internal/claude"
	"github.com/jitokim/missionctl/internal/control"
	"github.com/jitokim/missionctl/internal/domain"
)

type loopsMsg []domain.Loop
type tickMsg time.Time

// resumeResultMsg reports the outcome of a resume (r key) attempt, computed
// off the event loop by resumeCmd so the TUI never blocks on exec.
type resumeResultMsg struct {
	ok   bool
	text string
}

// attachResultMsg reports the outcome of an attach (enter key) attempt,
// computed off the event loop by attachCmd, mirroring resumeResultMsg.
type attachResultMsg struct {
	ok   bool
	text string
}

// logClosedMsg reports that the pager opened by the "o" key has exited and
// control has returned to the TUI (tea.ExecProcess suspends the program
// while the pager runs).
type logClosedMsg struct{ err error }

const refreshEvery = 3 * time.Second

// scan is a tea.Cmd: rediscover the fleet from the logs.
func scan() tea.Msg {
	loops, _ := claude.DiscoverLoops(time.Now(), claude.ActiveWindow)
	return loopsMsg(loops)
}

func tick() tea.Cmd {
	return tea.Tick(refreshEvery, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// statusKind colors the status/result line above the keybar (resume
// successes read green, failures red — anything else is neutral/dim).
type statusKind int

const (
	statusNeutral statusKind = iota
	statusOK
	statusErr
)

type Model struct {
	loops      []domain.Loop
	cursor     int
	w, h       int
	status     string
	statusKind statusKind
	lastScan   time.Time
	start      time.Time // for the header's uptime clock
	hostname   string
}

func New() Model {
	host, err := os.Hostname()
	if err != nil {
		host = "localhost"
	}
	return Model{
		status:   "watching ~/.claude/projects",
		start:    time.Now(),
		hostname: host,
	}
}

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
			sel, ok := m.selected()
			if !ok || sel.State != domain.StateStalled {
				m.status, m.statusKind = "select a stalled loop to resume", statusNeutral
				return m, nil
			}
			m.status, m.statusKind = fmt.Sprintf("resuming %s...", sel.Project), statusNeutral
			return m, resumeCmd(sel)
		case "enter":
			sel, ok := m.selected()
			if !ok {
				m.status, m.statusKind = "select a loop to attach", statusNeutral
				return m, nil
			}
			m.status, m.statusKind = fmt.Sprintf("attaching %s...", sel.Project), statusNeutral
			return m, attachCmd(sel)
		case "o":
			sel, ok := m.selected()
			if !ok {
				m.status, m.statusKind = "select a loop to view its log", statusNeutral
				return m, nil
			}
			pager := exec.Command("less", "-R", "+G", sel.Path)
			return m, tea.ExecProcess(pager, func(err error) tea.Msg {
				return logClosedMsg{err}
			})
		}
	case resumeResultMsg:
		m.status = msg.text
		if msg.ok {
			m.statusKind = statusOK
		} else {
			m.statusKind = statusErr
		}
	case attachResultMsg:
		m.status = msg.text
		if msg.ok {
			m.statusKind = statusOK
		} else {
			m.statusKind = statusErr
		}
	case logClosedMsg:
		if msg.err != nil {
			m.status, m.statusKind = fmt.Sprintf("open log failed: %v", msg.err), statusErr
		} else {
			m.status, m.statusKind = "closed log", statusNeutral
		}
	}
	return m, nil
}

// resumeCmd re-sends a stalled loop's last prompt to the terminal surface
// hosting it, via whichever multiplexer backend (orca/cmux/tmux) is
// available. Runs off the event loop — exec calls belong in a tea.Cmd, never
// in Update.
func resumeCmd(l domain.Loop) tea.Cmd {
	return func() tea.Msg {
		ctrl, ok := control.Resolve()
		if !ok {
			return resumeResultMsg{false, "no orca/tmux/cmux — resume manually: " + manualResumeHint(l.SessionID)}
		}
		target, ok := ctrl.Locate(l.ProjectDir)
		if !ok {
			return resumeResultMsg{false, "surface not found — resume manually: " + manualResumeHint(l.SessionID)}
		}
		prompt, ok := claude.LastUserPrompt(l.Path)
		note := ""
		if !ok {
			note = " (no prior prompt found — sent Enter only)"
		}
		if err := ctrl.Resume(target, prompt); err != nil {
			return resumeResultMsg{false, fmt.Sprintf("resume %s failed: %v", l.Project, err)}
		}
		return resumeResultMsg{true, fmt.Sprintf("resumed %s via %s%s", l.Project, ctrl.Name(), note)}
	}
}

// manualResumeHint is the copy-pasteable fallback for bare terminals (no
// orca/cmux/tmux to actuate into) — observation still works everywhere, but
// actuation degrades to "tell the human what to type".
func manualResumeHint(sessionID string) string {
	return "claude --resume " + sessionID
}

// attachCmd brings the terminal surface hosting l to the front, via
// whichever multiplexer backend is available. Works for any loop state (not
// just stalled) — "jump to it" is useful for a running loop too. Runs off
// the event loop, same non-blocking pattern as resumeCmd.
func attachCmd(l domain.Loop) tea.Cmd {
	return func() tea.Msg {
		ctrl, ok := control.Resolve()
		if !ok {
			return attachResultMsg{false, "no orca/tmux/cmux — attach manually: " + manualAttachHint(l.Cwd)}
		}
		target, ok := ctrl.Locate(l.ProjectDir)
		if !ok {
			return attachResultMsg{false, "surface not found — attach manually: " + manualAttachHint(l.Cwd)}
		}
		if err := ctrl.Focus(target); err != nil {
			return attachResultMsg{false, fmt.Sprintf("attach %s failed: %v", l.Project, err)}
		}
		return attachResultMsg{true, fmt.Sprintf("attached %s via %s", l.Project, ctrl.Name())}
	}
}

// manualAttachHint is the copy-pasteable fallback for bare terminals (no
// orca/cmux/tmux to focus) — at least point the human at where the loop lives.
func manualAttachHint(cwd string) string {
	return "cd " + cwd
}

func (m Model) selected() (domain.Loop, bool) {
	if m.cursor >= 0 && m.cursor < len(m.loops) {
		return m.loops[m.cursor], true
	}
	return domain.Loop{}, false
}

// termWidth is the usable render width, guarding against 0 before the first
// tea.WindowSizeMsg arrives.
func (m Model) termWidth() int {
	if m.w <= 0 {
		return 80
	}
	return m.w
}

// counts tallies loop states for the summary band and keybar.
func (m Model) counts() (total, running, stalled, idle int) {
	total = len(m.loops)
	for _, l := range m.loops {
		switch l.State {
		case domain.StateRunning:
			running++
		case domain.StateStalled:
			stalled++
		case domain.StateIdle:
			idle++
		}
	}
	return
}

func (m Model) View() string {
	width := m.termWidth()
	var b strings.Builder

	b.WriteString(renderHeaderRow(m, width))
	b.WriteString("\n")
	b.WriteString(renderRule(width))
	b.WriteString("\n")

	total, running, stalled, idle := m.counts()
	b.WriteString(renderSummaryBand(total, running, stalled, idle, width))
	b.WriteString("\n\n")

	b.WriteString(stFaint.Render("LOOPS"))
	b.WriteString("\n")

	wName, wNote := columnWidths(width)
	b.WriteString(renderTableHeader(wName, wNote))
	b.WriteString("\n")
	if len(m.loops) == 0 {
		b.WriteString(stFaint.Render("  no active Claude Code loops in the window.\n"))
	}
	dupLabels := duplicateLabels(m.loops)
	for i, l := range m.loops {
		b.WriteString(renderRow(l, i == m.cursor, dupLabels[l.Project], wName, wNote, width))
		b.WriteString("\n")
	}

	// detail
	if sel, ok := m.selected(); ok {
		b.WriteString(renderDetail(sel, width))
	}

	// status line (its own line, above the keybar) + keybar
	b.WriteString("\n")
	if line := renderStatusLine(m.status, m.statusKind); line != "" {
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString(renderKeybar(len(m.loops), width))
	return b.String()
}

// ── header / band / rule ────────────────────────────────────

// renderHeaderRow: left "◎ MISSIONCTL  fleet cockpit", right-aligned
// "● LIVE · <uptime> up · <hostname>".
func renderHeaderRow(m Model, width int) string {
	left := stTitle.Render("◎ MISSIONCTL") + stFaint.Render("  fleet cockpit")
	right := stLive.Render("●") + stDim.Render(" LIVE · ") +
		stDim.Bold(true).Render(formatUptime(time.Since(m.start))) +
		stDim.Render(" up · "+m.hostname)
	return padBetween(left, right, width)
}

func renderRule(width int) string {
	return lipgloss.NewStyle().Foreground(cLine).Render(strings.Repeat("─", width))
}

// renderSummaryBand: "fleet N · x run · y stalled · z idle" (zero segments
// omitted, fleet always shown) with a right-aligned amber "▲ N STALLED NEED
// YOU" badge when anything is stalled — the mockup's gate badge, repurposed
// honestly for stalls (the observation MVP has no oracle/gate data yet).
func renderSummaryBand(total, running, stalled, idle int, width int) string {
	parts := []string{stDim.Render(fmt.Sprintf("fleet %d", total))}
	if running > 0 {
		parts = append(parts, lipgloss.NewStyle().Foreground(cBlue).Render(fmt.Sprintf("%d run", running)))
	}
	if stalled > 0 {
		parts = append(parts, lipgloss.NewStyle().Foreground(cAmber).Render(fmt.Sprintf("%d stalled", stalled)))
	}
	if idle > 0 {
		parts = append(parts, stDim.Render(fmt.Sprintf("%d idle", idle)))
	}
	left := strings.Join(parts, stFaint.Render(" · "))

	right := ""
	if stalled > 0 {
		right = stBadgeStalled.Render(fmt.Sprintf("▲ %d STALLED NEED YOU", stalled))
	}
	return padBetween(left, right, width)
}

// ── row rendering ──────────────────────────────────────────

const (
	wMarker = 2
	wState  = 12
	wLast   = 14
)

// minWidthForNote: below this terminal width the NOTE column is dropped
// entirely (not just truncated) so NAME/STATE stay legible.
const minWidthForNote = 70

// columnWidths sizes NAME/NOTE from the remaining width after the fixed
// columns (marker/state/last) and inter-column gaps; NOTE is dropped below
// minWidthForNote, and NAME always keeps a usable minimum.
func columnWidths(width int) (wName, wNote int) {
	const gaps = 4 // spacing lipgloss.JoinHorizontal leaves negligible, but the
	// leading "  " indent plus cell boundaries need a little slack.
	fixed := wMarker + wState + wLast + gaps
	remaining := width - fixed
	if width >= minWidthForNote {
		wNote = 24
		remaining -= wNote
	}
	wName = remaining
	if wName < 10 {
		wName = 10
	}
	// Cap NAME so wide terminals don't stretch it into a chasm between
	// columns (mockup keeps the table compact); spare width goes to NOTE.
	if wName > 28 {
		if wNote > 0 {
			wNote += wName - 28
		}
		wName = 28
	}
	return wName, wNote
}

func renderTableHeader(wName, wNote int) string {
	cells := []string{
		stHeader.Width(wMarker).Render(""),
		stHeader.Width(wName).Render("NAME"),
		stHeader.Width(wState).Render("STATE"),
		stHeader.Width(wLast).Render("LAST"),
	}
	if wNote > 0 {
		cells = append(cells, stHeader.Width(wNote).Render("NOTE"))
	}
	return "  " + lipgloss.JoinHorizontal(lipgloss.Top, cells...)
}

// duplicateLabels reports, for each project label shared by 2+ loops in the
// current fleet, whether renderRow must disambiguate it with a session-id
// suffix (many loops sharing "sessions"/"IdeaProjects" are otherwise
// indistinguishable in the table).
func duplicateLabels(loops []domain.Loop) map[string]bool {
	counts := make(map[string]int, len(loops))
	for _, l := range loops {
		counts[l.Project]++
	}
	dup := make(map[string]bool, len(counts))
	for label, n := range counts {
		dup[label] = n > 1
	}
	return dup
}

func renderRow(l domain.Loop, sel bool, dup bool, wName, wNote, totalWidth int) string {
	marker := " "
	markerStyle := lipgloss.NewStyle().Foreground(cFaint)
	if sel {
		marker = "▸"
		markerStyle = lipgloss.NewStyle().Foreground(cAccent)
	}
	st := stateStyle(l)
	note := ""
	if l.Stall != domain.StallNone {
		note = "⚠ " + string(l.Stall)
	}
	label := l.Project
	if dup {
		label += "·" + shortID(l.SessionID)
	}
	cells := []string{
		markerStyle.Width(wMarker).Render(marker),
		stInk.Width(wName).Render(trunc(label, wName-1)),
		st.Width(wState).Render(stateLabel(l)),
		stDim.Width(wLast).Render(rel(time.Since(l.LastActivity))),
	}
	if wNote > 0 {
		cells = append(cells, st.Width(wNote).Render(trunc(note, wNote-1)))
	}
	row := "  " + lipgloss.JoinHorizontal(lipgloss.Top, cells...)
	if sel {
		// pad to the full table width first so the selection background
		// spans the whole row, like the mockup's .tr.sel.
		row = stSelRow.Render(padToWidth(row, totalWidth))
	}
	return row
}

// shortID is the first 4 chars of a session id, for disambiguating rows
// that share a project label (e.g. "sessions·1110").
func shortID(id string) string {
	if len(id) <= 4 {
		return id
	}
	return id[:4]
}

// ── detail pane ──────────────────────────────────────────────

func renderDetail(l domain.Loop, width int) string {
	// leave room for the ~8-col key + its gap before truncating long values
	// (paths) so nothing overflows the terminal width.
	valueWidth := width - 10
	if valueWidth < 10 {
		valueWidth = 10
	}

	var d strings.Builder
	d.WriteString(stTitle.Render("▸ " + l.Project))
	d.WriteString("  " + stFaint.Render(l.SessionID))
	d.WriteString("\n")
	d.WriteString(detailRow("STATE", stateStyle(l).Render(stateLabel(l))))
	d.WriteString(detailRow("LAST", stInk.Render(rel(time.Since(l.LastActivity))+"  ("+l.LastActivity.Format("15:04:05")+")")))
	d.WriteString(detailRow("CWD", stDim.Render(trunc(l.Cwd, valueWidth))))
	d.WriteString(detailRow("LOG", stDim.Render(trunc(l.Path, valueWidth))))
	if l.LastText != "" {
		d.WriteString(detailRow("TAIL", stDim.Render(trunc(l.LastText, valueWidth))))
	}

	if l.State == domain.StateStalled {
		d.WriteString(renderResumeCallout(l, width))
	}
	return stDetail.Width(width).Render(strings.TrimRight(d.String(), "\n"))
}

// detailRow is one KEY  value line in the mockup's key-value grid (faint
// uppercase key, ~8 cols wide).
func detailRow(key, value string) string {
	return stFaint.Width(8).Render(key) + value + "\n"
}

// renderResumeCallout is the mockup's amber gate-line, repurposed for a
// stall: "RESUME ▸ <why>   r re-send prompt   manual: claude --resume <id>".
// A 429 gets the red accent instead of amber (the turn didn't complete, it
// was rejected — a sharper signal than a generic stall).
func renderResumeCallout(l domain.Loop, width int) string {
	box, accent, chip := stCalloutAmber, cAmber, stKeyChipAmber
	if l.Stall == domain.StallRateLimit {
		box, accent, chip = stCalloutRed, cRed, stKeyChipRed
	}
	// border(1) + padding(1) on each side.
	contentWidth := width - 4
	if contentWidth < 20 {
		contentWidth = 20
	}
	line := lipgloss.NewStyle().Foreground(accent).Bold(true).Render("RESUME ▸") +
		" " + stInk.Render(string(l.Stall)) +
		"   " + chip.Render("r") + stDim.Render(" re-send prompt") +
		"   " + stDim.Render("manual: "+manualResumeHint(l.SessionID))
	return "\n" + box.Width(contentWidth).Render(line)
}

// ── status line / keybar ─────────────────────────────────────

// renderStatusLine shows the last resume result on its own line above the
// keybar: green on success, red on failure, dim otherwise.
func renderStatusLine(status string, kind statusKind) string {
	if status == "" {
		return ""
	}
	style := stDim
	switch kind {
	case statusOK:
		style = lipgloss.NewStyle().Foreground(cGreen)
	case statusErr:
		style = lipgloss.NewStyle().Foreground(cRed)
	}
	return style.Render(status)
}

// renderKeybar: only keys that actually do something today — no
// approve/pause/etc, those arrive with the engine.
func renderKeybar(loopCount int, width int) string {
	keys := []string{
		stKey.Render("↑↓") + stDim.Render(" select"),
		stKey.Render("↵") + stDim.Render(" attach"),
		stKey.Render("r") + stDim.Render(" resume"),
		stKey.Render("o") + stDim.Render(" log"),
		stKey.Render("q") + stDim.Render(" quit"),
	}
	left := strings.Join(keys, stFaint.Render("  ·  "))
	right := stFaint.Render(fmt.Sprintf("missionctl v0.1 · %d loops · ⧗ %s", loopCount, refreshEvery))
	return stKeybar.Width(width).Render(padBetween(left, right, width))
}

// ── layout helpers ────────────────────────────────────────────

// padBetween left-aligns left and right-aligns right within width, joined by
// spaces. If right is empty, left is returned as-is (no trailing padding).
func padBetween(left, right string, width int) string {
	if right == "" {
		return left
	}
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

// padToWidth right-pads s with spaces until it reaches width (visible
// width, ANSI-aware via lipgloss.Width), so a background fill spans evenly.
func padToWidth(s string, width int) string {
	w := lipgloss.Width(s)
	if w >= width {
		return s
	}
	return s + strings.Repeat(" ", width-w)
}

// formatUptime: mm:ss under an hour, hh:mm from an hour on.
func formatUptime(d time.Duration) string {
	if d < time.Hour {
		return fmt.Sprintf("%02d:%02d", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%02d:%02d", int(d.Hours()), int(d.Minutes())%60)
}

// ── misc helpers ────────────────────────────────────────────

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
