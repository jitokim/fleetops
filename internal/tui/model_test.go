package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/jitokim/missionctl/internal/control"
	"github.com/jitokim/missionctl/internal/domain"
	"github.com/jitokim/missionctl/internal/events"
	"github.com/jitokim/missionctl/internal/registry"
	"github.com/jitokim/missionctl/internal/sessions"
	runewidth "github.com/mattn/go-runewidth"
)

// TestMain is this package's safety net against the real
// ~/.missionctl/history: feat/detail-panel-v2's detailPanelLines reads it
// (via events.Read) on EVERY m.View() call, and this file has many tests
// that call View() without any reason to care about history data at all.
// Defaulting historyDirFn to a deliberately-nonexistent path here means
// every such test is hermetic by default — reads simply find nothing
// (events.Read tolerates a missing file, same as a missing dir) — while
// tests that DO need specific history data still override historyDirFn
// themselves (see withFakeActuationSeams and others), same
// save-then-restore pattern as always.
func TestMain(m *testing.M) {
	historyDirFn = func() string { return filepath.Join(os.TempDir(), "missionctl-tui-tests-unused-history") }
	os.Exit(m.Run())
}

// runeKey builds the tea.KeyMsg bubbletea sends for a single printable
// character keypress (msg.String() == string(r)).
func runeKey(r rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
}

func updateModel(t *testing.T, m Model, msg tea.Msg) (Model, tea.Cmd) {
	t.Helper()
	newModel, cmd := m.Update(msg)
	mm, ok := newModel.(Model)
	if !ok {
		t.Fatalf("Update returned %T, want Model", newModel)
	}
	return mm, cmd
}

func TestManualResumeHint(t *testing.T) {
	got := manualResumeHint("abc-123")
	want := "claude --resume abc-123"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestManualAttachHint(t *testing.T) {
	got := manualAttachHint("/Users/imac/IdeaProjects/aboard")
	want := "cd /Users/imac/IdeaProjects/aboard"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestPagerCmd(t *testing.T) {
	got := pagerCmd("/x/sess.jsonl")
	want := []string{"less", "-R", "+G", "-M", "-PMmissionctl log — q to return (%pB\\%)", "/x/sess.jsonl"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestDuplicateLabels(t *testing.T) {
	loops := []domain.Loop{
		{Project: "sessions", SessionID: "aaa1"},
		{Project: "sessions", SessionID: "bbb2"},
		{Project: "aboard", SessionID: "ccc3"},
	}
	dup := duplicateLabels(loops)
	if !dup["sessions"] {
		t.Error(`dup["sessions"] = false, want true (shared by 2 loops)`)
	}
	if dup["aboard"] {
		t.Error(`dup["aboard"] = true, want false (unique label)`)
	}
}

func TestShortID(t *testing.T) {
	if got := shortID("1110abcdef"); got != "1110" {
		t.Errorf("got %q, want %q", got, "1110")
	}
	if got := shortID("ab"); got != "ab" {
		t.Errorf("got %q, want %q (shorter than 4 returned as-is)", got, "ab")
	}
}

func TestFormatUptime(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{45 * time.Second, "00:45"},
		{3*time.Minute + 41*time.Second, "03:41"},
		{90 * time.Minute, "01:30"},
		{25 * time.Hour, "25:00"},
	}
	for _, c := range cases {
		if got := formatUptime(c.d); got != c.want {
			t.Errorf("formatUptime(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestPadBetween(t *testing.T) {
	got := padBetween("left", "right", 20)
	if len(got) != 20 {
		t.Errorf("got len %d (%q), want 20", len(got), got)
	}
	if !strings.HasPrefix(got, "left") || !strings.HasSuffix(got, "right") {
		t.Errorf("got %q, want to start with left / end with right", got)
	}
}

func TestPadBetween_EmptyRightReturnsLeftUnpadded(t *testing.T) {
	if got := padBetween("left", "", 20); got != "left" {
		t.Errorf("got %q, want %q (no trailing padding when right is empty)", got, "left")
	}
}

// F1: padBetween used to just floor the gap at 1 space and concatenate
// regardless of whether left+right actually fit — which is exactly why the
// header/summary band didn't degrade at narrow widths (a live w=45 render
// measured 65 cols). It now degrades in two steps: drop right first, then
// (if even left alone overflows) ANSI-aware truncate left.

func TestPadBetween_Overflow_DropsRightFirst(t *testing.T) {
	got := padBetween("short left", "right", 10)
	if strings.Contains(got, "right") {
		t.Errorf("got %q, want right dropped entirely — it doesn't fit alongside left", got)
	}
	if got != "short left" {
		t.Errorf("got %q, want left unchanged (it alone fits within width)", got)
	}
}

func TestPadBetween_Overflow_TruncatesLeftWhenEvenLeftAloneOverflows(t *testing.T) {
	got := padBetween("a very long left string", "right", 10)
	if lipgloss.Width(got) > 10 {
		t.Errorf("got %q (width %d), want <= 10", got, lipgloss.Width(got))
	}
	if !strings.Contains(got, "…") {
		t.Errorf("got %q, want a truncation marker since even left alone overflows", got)
	}
}

func TestPadToWidth(t *testing.T) {
	got := padToWidth("abc", 6)
	if got != "abc   " {
		t.Errorf("got %q, want %q", got, "abc   ")
	}
}

func TestPadToWidth_AlreadyAtOrOverWidth(t *testing.T) {
	if got := padToWidth("abcdef", 6); got != "abcdef" {
		t.Errorf("got %q, want unchanged %q", got, "abcdef")
	}
	if got := padToWidth("abcdefgh", 6); got != "abcdefgh" {
		t.Errorf("got %q, want unchanged (already over width) %q", got, "abcdefgh")
	}
}

// ── two-pane layout (feat/two-pane-cockpit) ──────────────────────────────

func TestLayoutModeFor(t *testing.T) {
	cases := []struct {
		width int
		want  layoutMode
	}{
		{49, layoutListOnly},
		{50, layoutStacked},
		{79, layoutStacked},
		{80, layoutWide},
		{200, layoutWide},
	}
	for _, c := range cases {
		if got := layoutModeFor(c.width); got != c.want {
			t.Errorf("layoutModeFor(%d) = %v, want %v", c.width, got, c.want)
		}
	}
}

// TestListRowWidths_NeverOverflows: for every inner width from the narrowest
// this layout ever hands the FLEET panel up to a very wide one, the row's
// fixed columns (marker+NAME+STATE[+LAST]) must sum to <= innerWidth — the
// same "prove it fits, don't just assume the thresholds line up" guarantee
// F1 established for the old columnWidths.
// TestListRowWidths_NeverOverflows sweeps from wMarker+wState (the
// structural floor this row format needs — marker+STATE alone, see
// listRowWidths' doc) up to a very wide panel: the row must never exceed
// innerWidth. Below that floor marker+STATE alone already overflow no
// matter what listRowWidths returns for NAME — an acknowledged edge, not a
// guaranteed one (same spirit as F1's own "not fully guaranteed under ~40
// cols" caveat for the old columnWidths).
func TestListRowWidths_NeverOverflows(t *testing.T) {
	for innerWidth := wMarker + wState; innerWidth <= 200; innerWidth++ {
		wName, showLast := listRowWidths(innerWidth)
		sum := wMarker + wName + wState
		if showLast {
			sum += wLast
		}
		if sum > innerWidth {
			t.Errorf("innerWidth=%d: wMarker+wName(%d)+wState+wLast(shown=%v) = %d, want <= %d", innerWidth, wName, showLast, sum, innerWidth)
		}
	}
}

func TestListRowWidths_DropsLastWhenNoRoom(t *testing.T) {
	_, showLast := listRowWidths(wMarker + wState + listNameFloor - 1)
	if showLast {
		t.Error("showLast = true with no room for it, want false (LAST dropped)")
	}
	_, showLast = listRowWidths(wMarker + wState + wLast + listNameFloor)
	if !showLast {
		t.Error("showLast = false with enough room, want true (LAST kept)")
	}
}

// TestListRowWidths_NameWithinBounds: NAME never exceeds its cap, and never
// goes below listNameFloor once there's actually enough room for it (at
// innerWidth=1 there manifestly isn't — see TestListRowWidths_NeverOverflows'
// doc on the structural floor this row format needs).
func TestListRowWidths_NameWithinBounds(t *testing.T) {
	for _, innerWidth := range []int{wMarker + wState + listNameFloor, 40, 100, 300} {
		wName, _ := listRowWidths(innerWidth)
		if wName < listNameFloor {
			t.Errorf("innerWidth=%d: wName=%d, want >= listNameFloor (%d)", innerWidth, wName, listNameFloor)
		}
		if wName > nameCapWidth {
			t.Errorf("innerWidth=%d: wName=%d, want <= nameCapWidth (%d)", innerWidth, wName, nameCapWidth)
		}
	}
}

func TestVisibleWindow_TotalFitsWithinMaxRows_ReturnsWholeRange(t *testing.T) {
	start, end := visibleWindow(3, 1, 10)
	if start != 0 || end != 3 {
		t.Errorf("got [%d,%d), want [0,3)", start, end)
	}
}

func TestVisibleWindow_ScrollsToKeepCursorVisible(t *testing.T) {
	// 20 items, a 5-row window, cursor near the end: the window must have
	// scrolled so idx=17 actually falls in [start,end).
	start, end := visibleWindow(20, 17, 5)
	if 17 < start || 17 >= end {
		t.Errorf("cursor 17 not in window [%d,%d)", start, end)
	}
	if end-start != 5 {
		t.Errorf("window size = %d, want 5", end-start)
	}
}

func TestVisibleWindow_ClampsAtStart(t *testing.T) {
	start, end := visibleWindow(20, 0, 5)
	if start != 0 || end != 5 {
		t.Errorf("got [%d,%d), want [0,5) (cursor at the very start)", start, end)
	}
}

func TestVisibleWindow_ClampsAtEnd(t *testing.T) {
	start, end := visibleWindow(20, 19, 5)
	if start != 15 || end != 20 {
		t.Errorf("got [%d,%d), want [15,20) (cursor at the very end)", start, end)
	}
}

func TestPadLines_PadsShortSlices(t *testing.T) {
	out := padLines([]string{"a", "b"}, 4)
	if len(out) != 4 {
		t.Fatalf("len = %d, want 4", len(out))
	}
	if out[0] != "a" || out[1] != "b" || out[2] != "" || out[3] != "" {
		t.Errorf("got %#v, want [a b \"\" \"\"]", out)
	}
}

func TestPadLines_TruncatesLongSlices(t *testing.T) {
	out := padLines([]string{"a", "b", "c"}, 2)
	if len(out) != 2 || out[0] != "a" || out[1] != "b" {
		t.Errorf("got %#v, want [a b]", out)
	}
}

// TestRenderPanel_NeverExceedsOuterWidth: the bordered panel (border+title+
// rule+content) must never render a line wider than the outer width it was
// asked for, across a spread of widths and content lengths.
func TestRenderPanel_NeverExceedsOuterWidth(t *testing.T) {
	for _, width := range []int{10, 30, 50, 80, 120} {
		lines := []string{strings.Repeat("x", 500), "short"}
		out := renderPanel("A VERY LONG PANEL TITLE THAT MIGHT OVERFLOW", lines, width)
		for i, line := range strings.Split(out, "\n") {
			if got := lipgloss.Width(line); got > width {
				t.Errorf("width=%d: panel line %d is %d cols wide, want <= %d: %q", width, i, got, width, line)
			}
		}
	}
}

// ── F1 acceptance bar: no rendered line ever exceeds the terminal width ──

// viewRegressionLoops is a representative fleet for the full-frame width
// regression: a running loop with a long name/goal/note, a gate with a
// Korean prompt (the callout path), a drifted loop with a long rejection
// reason, and a plain idle loop — exercising every row/callout/detail-pane
// path renderListRow, renderGateCallout, and renderDetail can take.
func viewRegressionLoops() []domain.Loop {
	now := time.Now()
	return []domain.Loop{
		{
			Project: "IdeaProjects-very-long-label", SessionID: "abcd1234", ProjectDir: "-x-a",
			Cwd: "/Users/imac/IdeaProjects/very-long-label", Path: "/Users/imac/.claude/projects/-x-a/abcd1234.jsonl",
			State: domain.StateRunning, Cycle: 6,
			Goal:         domain.Goal{Text: "add pagination to the search results endpoint and cache it", MaxCycles: 12, BudgetTokens: 200000},
			TokensSpent:  64000,
			LastActivity: now.Add(-30 * time.Second),
			Note:         "⚠ over budget please look",
			LastText:     "이 기능을 추가하고 테스트를 실행했습니다. 모든 테스트가 통과했습니다.",
		},
		{
			Project: "voc-triage", SessionID: "kor00001", ProjectDir: "-x-b",
			Cwd: "/Users/imac/IdeaProjects/voc-triage", Path: "/Users/imac/.claude/projects/-x-b/kor00001.jsonl",
			State: domain.StateGate, GatePrompt: "캡틴, 재설치가 완료되었습니다. 계속 진행할까요?",
			LastActivity: now.Add(-2 * time.Minute),
		},
		{
			Project: "flaky-hunt", SessionID: "drift001", ProjectDir: "-x-c",
			Cwd: "/Users/imac/IdeaProjects/flaky-hunt", Path: "/Users/imac/.claude/projects/-x-c/drift001.jsonl",
			State: domain.StateDrift, Cycle: 3,
			Goal:         domain.Goal{Text: "fix the flaky auth test", MaxCycles: 12},
			Last:         &domain.Verdict{Outcome: domain.OutcomeRejected, Reason: "no evidence of a passing test run shown, claim unsubstantiated"},
			LastActivity: now.Add(-10 * time.Minute),
		},
		{
			Project: "asre", SessionID: "idle0001", ProjectDir: "-x-d",
			Cwd: "/Users/imac/orca/projects/asre", Path: "/Users/imac/.claude/projects/-x-d/idle0001.jsonl",
			State:        domain.StateIdle,
			LastActivity: now.Add(-1 * time.Hour),
		},
		{
			Project: "dotfiles", SessionID: "fail0001", ProjectDir: "-x-e",
			Cwd: "/Users/imac/dotfiles", Path: "/Users/imac/.claude/projects/-x-e/fail0001.jsonl",
			State: domain.StateFailed, Cycle: 12, NoImprove: 3,
			Goal:         domain.Goal{Text: "refactor the dotfiles bootstrap script", MaxCycles: 12, NoImproveLimit: 3},
			Note:         "stopped: no improvement 3/3",
			Stall:        domain.StallGone,
			LastActivity: now.Add(-3 * time.Hour),
		},
	}
}

// TestView_NoLineExceedsTerminalWidth is F1's original acceptance bar,
// extended by feat/two-pane-cockpit with a height-budget check: at every
// (width, height) combination, every line must fit within the width AND the
// total rendered line count must fit within the height — required because
// cmd/missionctl/main.go runs in tea.WithAltScreen() mode, where content
// beyond the terminal height is genuinely invisible, not just visually
// awkward. 18 is the lowest height this checks: it's the exact break-even
// where layoutStacked's two-bordered-panel floor (stackedPanelHeightFloor)
// stops needing to override the requested height — below it the floor
// intentionally wins over the exact budget (same spirit as the pre-existing
// "not fully guaranteed under ~40 cols" width caveat; see panelHeightFloor/
// stackedPanelHeightFloor's docs). feat/top-hint-grid added 70 to the width
// sweep — the hint-grid's own drop-the-whole-grid threshold — so the
// standing regression test also covers the header block's own width
// degradation, not just the panel area's.
func TestView_NoLineExceedsTerminalWidth(t *testing.T) {
	for _, width := range []int{45, 65, 70, 90, 120, 175} {
		for _, height := range []int{18, 24, 40, 60} {
			t.Run(fmt.Sprintf("width=%d/height=%d", width, height), func(t *testing.T) {
				m := New()
				m.w, m.h = width, height
				m.loops = viewRegressionLoops()
				m.cursor = 0

				out := m.View()
				lines := strings.Split(out, "\n")
				for i, line := range lines {
					if got := lipgloss.Width(line); got > width {
						t.Errorf("width=%d: line %d is %d cols wide, want <= %d: %q", width, i, got, width, line)
					}
				}
				if got := len(lines); got > height {
					t.Errorf("height=%d: rendered frame is %d lines, want <= %d", height, got, height)
				}
			})
		}
	}
}

// detailPanelV2RegressionLoop builds a loop whose DETAIL panel actually
// exercises every feat/detail-panel-v2 block — LAST ERROR (a REAL
// transcript file with a long verbatim 429 body, including one long
// UNBROKEN token — no spaces at all — to stress wrapTailText's hard-break
// path, not just its ordinary word-wrap), VERDICTS (oracle events),
// EVENTS (actuation + scan events), and the STALLED callout's flap counter
// (3 stall transitions within the last hour) — none of which
// viewRegressionLoops exercises (its Path/history are fake/absent, so
// those blocks render empty in the standing sweep above). Review fix (P2):
// the width/height regression sweep must ALSO cover these blocks, not just
// the pre-v2 key-value rows.
func detailPanelV2RegressionLoop(t *testing.T, historyDir string) domain.Loop {
	t.Helper()
	now := time.Now()

	transcriptPath := filepath.Join(t.TempDir(), "session.jsonl")
	longToken := strings.Repeat("x", 220) // one unbroken 220-char token — no spaces at all
	errLine := fmt.Sprintf(
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"API Error: 429 Too Many Requests — request id %s retry after 30s"}]},"timestamp":%q}`,
		longToken, now.Format(time.RFC3339))
	if err := os.WriteFile(transcriptPath, []byte(errLine+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	evs := []events.Event{
		{TS: now.Add(-50 * time.Minute).UnixNano(), SessionID: "v2-regress", FromState: "", ToState: "running", Trigger: events.TriggerActuation, Detail: "spawn: fix the flaky auth test", Actor: events.ActorHuman},
		{TS: now.Add(-40 * time.Minute).UnixNano(), SessionID: "v2-regress", FromState: "running", ToState: "stalled:rate-limit", Trigger: events.TriggerScan, Detail: "rate-limit", Actor: events.ActorSystem},
		{TS: now.Add(-39 * time.Minute).UnixNano(), SessionID: "v2-regress", FromState: "stalled:rate-limit", ToState: "running", Trigger: events.TriggerActuation, Detail: "resume tier1 ok", Actor: events.ActorHuman},
		{TS: now.Add(-25 * time.Minute).UnixNano(), SessionID: "v2-regress", FromState: "running", ToState: "stalled:rate-limit", Trigger: events.TriggerScan, Detail: "rate-limit", Actor: events.ActorSystem},
		{TS: now.Add(-24 * time.Minute).UnixNano(), SessionID: "v2-regress", FromState: "stalled:rate-limit", ToState: "running", Trigger: events.TriggerActuation, Detail: "resume tier1 ok", Actor: events.ActorHuman},
		{TS: now.Add(-15 * time.Minute).UnixNano(), SessionID: "v2-regress", FromState: "running", ToState: "idle", Trigger: events.TriggerScan, Actor: events.ActorSystem},
		{TS: now.Add(-14 * time.Minute).UnixNano(), SessionID: "v2-regress", FromState: "idle", ToState: "idle", Trigger: events.TriggerOracle, Detail: "progress at cycle 3: made partial progress, one test still failing intermittently under load", Actor: events.ActorAuto},
		{TS: now.Add(-5 * time.Minute).UnixNano(), SessionID: "v2-regress", FromState: "idle", ToState: "running", Trigger: events.TriggerScan, Actor: events.ActorSystem},
		{TS: now.UnixNano(), SessionID: "v2-regress", FromState: "running", ToState: "stalled:rate-limit", Trigger: events.TriggerScan, Detail: "rate-limit", Actor: events.ActorSystem}, // 3rd stall within the hour — flap counter
	}
	for _, ev := range evs {
		if err := events.Append(historyDir, ev); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	return domain.Loop{
		Project: "v2-regress", SessionID: "v2-regress", ProjectDir: "-x-v2",
		Cwd: "/Users/imac/IdeaProjects/v2-regress", Path: transcriptPath, CwdVerified: true,
		State: domain.StateStalled, Stall: domain.StallRateLimit, Cycle: 3,
		Goal:         domain.Goal{Text: "fix the flaky auth test", MaxCycles: 12, BudgetTokens: 2_000_000},
		TokensSpent:  1_200_000,
		Last:         &domain.Verdict{Outcome: domain.OutcomeProgress, Reason: "made partial progress, one test still failing intermittently under load"},
		LastActivity: now,
		LastText:     "still working on stabilizing the flaky test",
		BoundAt:      now.Add(-50 * time.Minute),
	}
}

// TestView_NoLineExceedsTerminalWidth_DetailPanelV2Blocks is the P2 review
// fix's regression: the standing width/height sweep above must ALSO
// exercise LAST ERROR/VERDICTS/EVENTS/the flap counter, not just render
// them empty — same acceptance bar (every line <= width, total lines <=
// height), same width/height matrix, but with a fixture whose DETAIL panel
// actually populates every feat/detail-panel-v2 block.
func TestView_NoLineExceedsTerminalWidth_DetailPanelV2Blocks(t *testing.T) {
	historyDir := t.TempDir()
	origHistoryDir := historyDirFn
	defer func() { historyDirFn = origHistoryDir }()
	historyDirFn = func() string { return historyDir }

	l := detailPanelV2RegressionLoop(t, historyDir)

	for _, width := range []int{45, 65, 70, 90, 120, 175} {
		for _, height := range []int{18, 24, 40, 60} {
			t.Run(fmt.Sprintf("width=%d/height=%d", width, height), func(t *testing.T) {
				m := New()
				m.w, m.h = width, height
				m.loops = []domain.Loop{l}
				m.cursor = 0

				out := m.View()
				lines := strings.Split(out, "\n")
				for i, line := range lines {
					if got := lipgloss.Width(line); got > width {
						t.Errorf("width=%d: line %d is %d cols wide, want <= %d: %q", width, i, got, width, line)
					}
				}
				if got := len(lines); got > height {
					t.Errorf("height=%d: rendered frame is %d lines, want <= %d", height, got, height)
				}
			})
		}
	}
}

// TestView_NoLineExceedsTerminalWidth_DetailPanelV2Blocks_ActuallyRendered
// is a sanity check for the test above: at a generous width/height, the
// v2 blocks (LAST ERROR/VERDICTS/EVENTS/flap counter) must actually
// APPEAR — otherwise the width/height sweep above would be silently
// testing nothing new (an empty panel trivially satisfies both
// invariants).
func TestView_NoLineExceedsTerminalWidth_DetailPanelV2Blocks_ActuallyRendered(t *testing.T) {
	historyDir := t.TempDir()
	origHistoryDir := historyDirFn
	defer func() { historyDirFn = origHistoryDir }()
	historyDirFn = func() string { return historyDir }

	l := detailPanelV2RegressionLoop(t, historyDir)

	m := New()
	m.w, m.h = 120, 45
	m.loops = []domain.Loop{l}
	m.cursor = 0
	out := m.View()

	for _, want := range []string{"LAST ERROR", "VERDICTS", "EVENTS", "stall in"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected the rendered frame to contain %q (the block this regression test exists to exercise), got:\n%s", want, out)
		}
	}
}

// ── feat/top-hint-grid: header block ─────────────────────────────────────

// TestRenderHeaderBlock_ExactlyThreeLines_AtWideWidths verifies the header
// block itself (not just the full View()) is exactly headerLines (3) rows
// tall, at every width the hint grid renders at (>=headerHintMinWidth).
func TestRenderHeaderBlock_ExactlyThreeLines_AtWideWidths(t *testing.T) {
	m := New()
	for _, width := range []int{headerHintMinWidth, 90, 120, 175, 300} {
		out := renderHeaderBlock(m, width)
		got := len(strings.Split(out, "\n"))
		if got != headerLines {
			t.Errorf("width=%d: header block is %d lines, want exactly %d", width, got, headerLines)
		}
	}
}

// TestRenderHeaderBlock_ExactlyThreeLines_AtNarrowWidths: the same
// exactly-3-lines invariant must hold even once the hint grid is dropped
// entirely (LEFT+MIDDLE only) or squeezed to its narrowest.
func TestRenderHeaderBlock_ExactlyThreeLines_AtNarrowWidths(t *testing.T) {
	m := New()
	for _, width := range []int{1, 20, 45, 65, headerHintMinWidth - 1} {
		out := renderHeaderBlock(m, width)
		got := len(strings.Split(out, "\n"))
		if got != headerLines {
			t.Errorf("width=%d: header block is %d lines, want exactly %d", width, got, headerLines)
		}
	}
}

// TestHeaderHintColumnCount_DropsColumnsAsWidthShrinks pins the column
// count at representative widths — hint columns must drop right-to-left
// (fewer columns at narrower widths), reaching 0 below headerHintMinWidth.
func TestHeaderHintColumnCount_DropsColumnsAsWidthShrinks(t *testing.T) {
	cases := []struct {
		width int
		want  int
	}{
		{headerHintMinWidth - 1, 0}, // one below the threshold — whole grid dropped
		{headerHintMinWidth, 1},
		{175, 4}, // capped at the number of columns headerHintKeys actually needs
		{300, 4}, // still capped — extra width doesn't grow a 5th column
	}
	for _, c := range cases {
		if got := headerHintColumnCount(c.width); got != c.want {
			t.Errorf("headerHintColumnCount(%d) = %d, want %d", c.width, got, c.want)
		}
	}
}

// TestHeaderHintColumnCount_MonotonicallyNonDecreasing: columns must never
// drop as width GROWS (no oscillation) — the width-degradation direction is
// exclusively "narrower → fewer or equal columns".
func TestHeaderHintColumnCount_MonotonicallyNonDecreasing(t *testing.T) {
	prev := -1
	for width := 1; width <= 300; width++ {
		got := headerHintColumnCount(width)
		if got < prev {
			t.Fatalf("width=%d: cols=%d, want >= previous width's %d (must not decrease as width grows)", width, got, prev)
		}
		prev = got
	}
}

// TestRenderHeaderBlock_HintGridAbsentBelowThreshold: below
// headerHintMinWidth, no "<key>" chip renders at all — not even a single
// squeezed column.
func TestRenderHeaderBlock_HintGridAbsentBelowThreshold(t *testing.T) {
	m := New()
	out := renderHeaderBlock(m, headerHintMinWidth-1)
	if strings.Contains(out, "<r>") || strings.Contains(out, "<q>") {
		t.Errorf("header block below the threshold must not show any hint chip:\n%s", out)
	}
}

// TestRenderHeaderBlock_HintGridPresentAtThreshold: AT headerHintMinWidth,
// at least the first (most essential) hint column renders.
func TestRenderHeaderBlock_HintGridPresentAtThreshold(t *testing.T) {
	m := New()
	out := renderHeaderBlock(m, headerHintMinWidth)
	if !strings.Contains(out, "<r>") {
		t.Errorf("header block at the threshold width must show at least the first hint column:\n%s", out)
	}
}

// TestRenderHeaderLeft_HostnameNotTruncatedAtTypicalLength verifies
// headerLeftWidth is generous enough for a realistic hostname (the
// regression this constant's own doc comment cites) — a real bug caught
// empirically while building this feature (an earlier, narrower
// headerLeftWidth truncated "iMacui-iMac.local").
func TestRenderHeaderLeft_HostnameNotTruncatedAtTypicalLength(t *testing.T) {
	m := New()
	m.hostname = "iMacui-iMac.local"
	out := renderHeaderLeft(m, headerLeftWidth)
	if strings.Contains(out, "…") {
		t.Errorf("hostname line got truncated at headerLeftWidth=%d:\n%s", headerLeftWidth, out)
	}
	if !strings.Contains(out, m.hostname) {
		t.Errorf("expected the full hostname to appear untruncated, got:\n%s", out)
	}
}

// TestView_SelectedRowVisibleInFleetPanel: with more loops than fit in the
// FLEET panel's rows, moving the cursor deep into the list must scroll the
// panel (visibleWindow) so the selected loop's row is still on screen — not
// require any NEW keybinding, just keep the existing ↑/↓/g/G-driven cursor
// visible.
func TestView_SelectedRowVisibleInFleetPanel(t *testing.T) {
	for _, width := range []int{45, 65, 90} {
		t.Run(fmt.Sprintf("width=%d", width), func(t *testing.T) {
			m := New()
			m.w, m.h = width, 24
			m.loops = manyLoopsForScrollTest(50)
			m.cursor = 37

			out := m.View()
			want := m.loops[m.cursor].Project
			if !strings.Contains(out, want) {
				t.Errorf("selected loop %q not visible in rendered frame (scrolled off screen):\n%s", want, out)
			}
		})
	}
}

// manyLoopsForScrollTest builds n distinct, uniquely-named loops — enough to
// force the FLEET panel to scroll at any of this layout's widths.
func manyLoopsForScrollTest(n int) []domain.Loop {
	now := time.Now()
	out := make([]domain.Loop, n)
	for i := 0; i < n; i++ {
		out[i] = domain.Loop{
			Project:      fmt.Sprintf("loop-%03d", i),
			SessionID:    fmt.Sprintf("sess%04d", i),
			State:        domain.StateIdle,
			LastActivity: now.Add(-time.Duration(i) * time.Minute),
		}
	}
	return out
}

// TestView_FallbackThresholds_TriggerCorrectLayout is the task's fallback
// acceptance bar: <80 stacked (FLEET above DETAIL), <50 list-only (no
// DETAIL panel at all).
func TestView_FallbackThresholds_TriggerCorrectLayout(t *testing.T) {
	m := New()
	m.h = 30
	m.loops = viewRegressionLoops()
	m.cursor = 0

	t.Run("width<50 is list-only (no DETAIL panel)", func(t *testing.T) {
		m.w = 49
		out := m.View()
		if strings.Contains(out, "DETAIL") {
			t.Errorf("list-only layout (w=49) must not show a DETAIL panel:\n%s", out)
		}
	})

	t.Run("50<=width<80 is stacked (FLEET above DETAIL, not side by side)", func(t *testing.T) {
		m.w = 65
		out := m.View()
		lines := strings.Split(out, "\n")
		fleetLine, detailLine := -1, -1
		for i, l := range lines {
			if strings.Contains(l, "FLEET (") {
				fleetLine = i
			}
			if strings.Contains(l, "DETAIL") && detailLine == -1 {
				detailLine = i
			}
		}
		if fleetLine == -1 || detailLine == -1 {
			t.Fatalf("expected both FLEET and DETAIL panels in stacked layout, got fleetLine=%d detailLine=%d", fleetLine, detailLine)
		}
		if fleetLine >= detailLine {
			t.Errorf("stacked layout: FLEET title (line %d) must come before DETAIL title (line %d)", fleetLine, detailLine)
		}
		if strings.Contains(lines[fleetLine], "DETAIL") {
			t.Errorf("stacked layout: FLEET and DETAIL must not share a line (not side by side): %q", lines[fleetLine])
		}
	})

	t.Run("width>=80 is wide (FLEET and DETAIL side by side)", func(t *testing.T) {
		m.w = 90
		out := m.View()
		found := false
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, "FLEET (") && strings.Contains(l, "DETAIL") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("wide layout (w=90): expected a line containing both FLEET and DETAIL (side by side):\n%s", out)
		}
	})
}

func TestBudgetBar_Boundaries(t *testing.T) {
	cases := []struct {
		frac float64
		want string
	}{
		{0, "░░░░░░░ 0%"},
		{0.32, "██░░░░░ 32%"},
		{1.0, "███████ 100%"},
		{1.5, "███████ 100%"}, // clamps
		{-0.5, "░░░░░░░ 0%"},  // clamps
	}
	for _, c := range cases {
		if got := budgetBar(c.frac, 7); got != c.want {
			t.Errorf("budgetBar(%v, 7) = %q, want %q", c.frac, got, c.want)
		}
	}
}

func TestPrettyTokens(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{950, "950"},
		{12_400, "12k"},
		{1_234_567, "1.2M"},
	}
	for _, c := range cases {
		if got := prettyTokens(c.n); got != c.want {
			t.Errorf("prettyTokens(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

// ── mode state machine ("n" new-loop prompt) ──────────────────────

func modelWithOneLoop() Model {
	m := New()
	m.loops = []domain.Loop{{Project: "aboard", SessionID: "sess-1", ProjectDir: "-x-aboard", Cwd: "/x/aboard", CwdVerified: true, State: domain.StateRunning}}
	m.cursor = 0
	return m
}

func TestUpdate_NKey_EntersPromptingModeWithSelectedCwd(t *testing.T) {
	m := modelWithOneLoop()

	m, _ = updateModel(t, m, runeKey('n'))

	if m.mode != modePrompting {
		t.Fatalf("mode = %v, want modePrompting", m.mode)
	}
	if m.spawnCwd != "/x/aboard" {
		t.Errorf("spawnCwd = %q, want the selected loop's Cwd %q", m.spawnCwd, "/x/aboard")
	}
	if !m.input.Focused() {
		t.Error("expected the text input to be focused after entering prompting mode")
	}
}

func TestUpdate_NKey_NoSelectionFallsBackToGetwd(t *testing.T) {
	m := New() // no loops, nothing selected

	m, _ = updateModel(t, m, runeKey('n'))

	if m.mode != modePrompting {
		t.Fatalf("mode = %v, want modePrompting", m.mode)
	}
	if m.spawnCwd == "" {
		t.Error("expected spawnCwd to fall back to a non-empty cwd (os.Getwd) when nothing is selected")
	}
}

func TestUpdate_NKey_SelectedCwdNotVerified_FallsBackWithNote(t *testing.T) {
	// P1-3: a dead loop's Cwd is at best a lossy decode of ProjectDir — spawn
	// must not trust it unless applyLiveness confirmed it against a live
	// process's real lsof path (CwdVerified).
	m := New()
	m.loops = []domain.Loop{{Project: "aboard", SessionID: "sess-1", Cwd: "/x/aboard", CwdVerified: false, State: domain.StateStalled}}
	m.cursor = 0

	m, _ = updateModel(t, m, runeKey('n'))

	if m.spawnCwd == "/x/aboard" {
		t.Error("expected spawnCwd NOT to use the unverified Cwd")
	}
	if m.spawnCwd == "" {
		t.Error("expected spawnCwd to fall back to a non-empty cwd (os.Getwd)")
	}
	if m.spawnNote == "" {
		t.Error("expected a spawnNote explaining the fallback")
	}
}

// typeAndEnter types s into m's active textinput (rune by rune, as a human
// would) then presses enter — one wizard-step answer.
func typeAndEnter(t *testing.T, m Model, s string) (Model, tea.Cmd) {
	t.Helper()
	for _, r := range s {
		m, _ = updateModel(t, m, runeKey(r))
	}
	return updateModel(t, m, tea.KeyMsg{Type: tea.KeyEnter})
}

func TestUpdate_SpawnNote_SurfacesInStatusOnSubmit(t *testing.T) {
	// the note set at "n" keypress time must actually reach the user —
	// View() replaces the status line with the prompt the instant
	// modePrompting is entered, so the note can only surface later, at the
	// "enter"-submit status message (which fires at the END of the wizard,
	// step 5).
	m := New()
	m.loops = []domain.Loop{{Project: "aboard", SessionID: "sess-1", Cwd: "/x/aboard", CwdVerified: false, State: domain.StateStalled}}
	m.cursor = 0
	m, _ = updateModel(t, m, runeKey('n'))

	m, _ = typeAndEnter(t, m, "goal") // step 1: goal
	m, _ = typeAndEnter(t, m, "")     // step 2: done-when, skipped
	m, _ = typeAndEnter(t, m, "")     // step 3: oracle, skipped
	m, _ = typeAndEnter(t, m, "")     // step 4: challenger, skipped
	m, cmd := typeAndEnter(t, m, "")  // step 5: max_iteration, default

	if cmd == nil {
		t.Fatal("expected a non-nil tea.Cmd (spawnCmd)")
	}
	if !strings.Contains(m.status, "wasn't verified") {
		t.Errorf("status = %q, want it to surface the spawnNote about the unverified cwd", m.status)
	}
	if !strings.Contains(m.status, "no done condition") {
		t.Errorf("status = %q, want it to nudge about the missing done condition", m.status)
	}
}

func TestUpdate_Esc_CancelsPromptingMode(t *testing.T) {
	m := modelWithOneLoop()
	m, _ = updateModel(t, m, runeKey('n'))
	if m.mode != modePrompting {
		t.Fatalf("precondition failed: mode = %v, want modePrompting", m.mode)
	}

	m, cmd := updateModel(t, m, tea.KeyMsg{Type: tea.KeyEsc})

	if m.mode != modeNormal {
		t.Errorf("mode = %v, want modeNormal after esc", m.mode)
	}
	if m.status != "cancelled" {
		t.Errorf("status = %q, want %q", m.status, "cancelled")
	}
	if cmd != nil {
		t.Error("expected no tea.Cmd from cancelling (no spawn should be triggered)")
	}
}

func TestUpdate_EscAtEachWizardStep_Cancels(t *testing.T) {
	// esc must cancel the wizard regardless of which of the 5 steps is
	// currently active.
	steps := []struct {
		name    string
		answers []string // typed+entered before esc
	}{
		{"step1_goal", nil},
		{"step2_doneWhen", []string{"goal"}},
		{"step3_oracle", []string{"goal", ""}},
		{"step4_challenger", []string{"goal", "", ""}},
		{"step5_maxCycles", []string{"goal", "", "", ""}},
	}
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			m := modelWithOneLoop()
			m, _ = updateModel(t, m, runeKey('n'))
			for _, a := range s.answers {
				m, _ = typeAndEnter(t, m, a)
			}
			if m.mode != modePrompting {
				t.Fatalf("precondition failed: mode = %v, want modePrompting before esc", m.mode)
			}

			m, cmd := updateModel(t, m, tea.KeyMsg{Type: tea.KeyEsc})

			if m.mode != modeNormal {
				t.Errorf("mode = %v, want modeNormal after esc", m.mode)
			}
			if cmd != nil {
				t.Error("expected no tea.Cmd from cancelling at any step")
			}
		})
	}
}

func TestUpdate_Enter_EmptyGoal_CancelsWithoutSpawning(t *testing.T) {
	m := modelWithOneLoop()
	m, _ = updateModel(t, m, runeKey('n'))

	m, cmd := updateModel(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	if m.mode != modeNormal {
		t.Errorf("mode = %v, want modeNormal", m.mode)
	}
	if !strings.Contains(m.status, "empty goal") {
		t.Errorf("status = %q, want it to mention the empty goal", m.status)
	}
	if cmd != nil {
		t.Error("expected no tea.Cmd for an empty goal (spawn must not be triggered)")
	}
}

func TestWizard_FullFlow_AllStepsFilled(t *testing.T) {
	m := modelWithOneLoop()
	m, _ = updateModel(t, m, runeKey('n'))

	m, _ = typeAndEnter(t, m, "fix the bug")                // step 1: goal
	m, _ = typeAndEnter(t, m, "tests pass")                 // step 2: done when
	m, _ = typeAndEnter(t, m, "run go test ./...")          // step 3: oracle
	m, _ = typeAndEnter(t, m, "try to break it with -race") // step 4: challenger
	m, cmd := typeAndEnter(t, m, "20")                      // step 5: max cycles

	if m.mode != modeNormal {
		t.Errorf("mode = %v, want modeNormal after the full wizard", m.mode)
	}
	if cmd == nil {
		t.Fatal("expected a non-nil tea.Cmd (spawnCmd)")
	}
	if m.spawnGoal != "fix the bug" {
		t.Errorf("spawnGoal = %q, want %q", m.spawnGoal, "fix the bug")
	}
	if m.spawnDoneWhen != "tests pass" {
		t.Errorf("spawnDoneWhen = %q, want %q", m.spawnDoneWhen, "tests pass")
	}
	if m.spawnOracle != "run go test ./..." {
		t.Errorf("spawnOracle = %q, want %q", m.spawnOracle, "run go test ./...")
	}
	if m.spawnChallenger != "try to break it with -race" {
		t.Errorf("spawnChallenger = %q, want %q", m.spawnChallenger, "try to break it with -race")
	}
	if strings.Contains(m.status, "no done condition") {
		t.Errorf("status = %q, want no missing-done-condition nudge (one was supplied)", m.status)
	}
}

func TestWizard_DefaultsAtOptionalSteps(t *testing.T) {
	m := modelWithOneLoop()
	m, _ = updateModel(t, m, runeKey('n'))

	m, _ = typeAndEnter(t, m, "fix the bug") // step 1: goal (required)
	m, _ = typeAndEnter(t, m, "")            // step 2: done when — skipped
	m, _ = typeAndEnter(t, m, "")            // step 3: oracle — skipped
	m, _ = typeAndEnter(t, m, "")            // step 4: challenger — skipped

	// each of steps 2-4 returns textinput.Blink (a non-nil cmd) to advance
	// to the next question — only the mode/step, not cmd-nilness, indicates
	// whether the wizard has actually submitted yet.
	if m.mode != modePrompting || m.spawnStep != wizardMaxCycles {
		t.Fatalf("expected to be sitting at step 5 (max cycles), got mode=%v step=%v", m.mode, m.spawnStep)
	}
	if m.spawnDoneWhen != "" || m.spawnOracle != "" || m.spawnChallenger != "" {
		t.Errorf("got doneWhen=%q oracle=%q challenger=%q, want all empty (skipped)", m.spawnDoneWhen, m.spawnOracle, m.spawnChallenger)
	}

	m, cmd := typeAndEnter(t, m, "") // step 5: max cycles — default
	if cmd == nil {
		t.Fatal("expected a non-nil tea.Cmd (spawnCmd) once step 5 is answered")
	}
	if m.mode != modeNormal {
		t.Errorf("mode = %v, want modeNormal after the wizard completes", m.mode)
	}
	if !strings.Contains(m.status, "no done condition") {
		t.Errorf("status = %q, want the missing-done-condition nudge", m.status)
	}
}

func TestUpdate_NonNumericMaxCycles_RePromptsSameStep(t *testing.T) {
	m := modelWithOneLoop()
	m, _ = updateModel(t, m, runeKey('n'))
	m, _ = typeAndEnter(t, m, "goal")
	m, _ = typeAndEnter(t, m, "")
	m, _ = typeAndEnter(t, m, "")
	m, _ = typeAndEnter(t, m, "")

	m, cmd := typeAndEnter(t, m, "not-a-number")

	if cmd != nil {
		t.Error("expected no tea.Cmd — invalid max_iteration must not submit")
	}
	if m.mode != modePrompting || m.spawnStep != wizardMaxCycles {
		t.Errorf("got mode=%v step=%v, want to stay in modePrompting at wizardMaxCycles (re-prompt)", m.mode, m.spawnStep)
	}
	if m.statusKind != statusErr {
		t.Errorf("statusKind = %v, want statusErr", m.statusKind)
	}
	if !strings.Contains(m.status, "positive number") {
		t.Errorf("status = %q, want it to explain the max_iteration requirement", m.status)
	}
}

func TestUpdate_ZeroMaxCycles_RePromptsSameStep(t *testing.T) {
	m := modelWithOneLoop()
	m, _ = updateModel(t, m, runeKey('n'))
	m, _ = typeAndEnter(t, m, "goal")
	m, _ = typeAndEnter(t, m, "")
	m, _ = typeAndEnter(t, m, "")
	m, _ = typeAndEnter(t, m, "")

	m, cmd := typeAndEnter(t, m, "0")

	if cmd != nil {
		t.Error("expected no tea.Cmd — zero max_iteration must not submit")
	}
	if m.spawnStep != wizardMaxCycles {
		t.Errorf("spawnStep = %v, want to stay at wizardMaxCycles", m.spawnStep)
	}
}

func TestUpdate_TypeThenEnter_SubmitsSpawn(t *testing.T) {
	m := modelWithOneLoop()
	m, _ = updateModel(t, m, runeKey('n'))

	for _, r := range "fix the bug" {
		m, _ = updateModel(t, m, runeKey(r))
	}
	if m.input.Value() != "fix the bug" {
		t.Fatalf("input value = %q, want %q (typing must reach the textinput while prompting)", m.input.Value(), "fix the bug")
	}

	// step 1 (goal) submitted — advances to step 2 (mode stays modePrompting;
	// the returned cmd is textinput.Blink for the next question, not a spawn).
	m, _ = updateModel(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.mode != modePrompting {
		t.Fatalf("mode = %v, want modePrompting (4 more steps to go)", m.mode)
	}
	if m.spawnStep != wizardDoneWhen {
		t.Fatalf("spawnStep = %v, want wizardDoneWhen", m.spawnStep)
	}

	// steps 2-5 all skipped/defaulted — the LAST enter must submit.
	m, _ = typeAndEnter(t, m, "")
	m, _ = typeAndEnter(t, m, "")
	m, _ = typeAndEnter(t, m, "")
	m, cmd := typeAndEnter(t, m, "")

	if m.mode != modeNormal {
		t.Errorf("mode = %v, want modeNormal after submit", m.mode)
	}
	if cmd == nil {
		t.Fatal("expected a non-nil tea.Cmd (spawnCmd) once the wizard completes")
	}
	if !strings.Contains(m.status, "spawning") {
		t.Errorf("status = %q, want it to mention spawning", m.status)
	}
}

func TestUpdate_ArrowKeysWhilePrompting_RouteToInputNotCursor(t *testing.T) {
	// two loops so cursor movement would be observable if it were (wrongly)
	// still handled by the normal navigation path while prompting.
	m := New()
	m.loops = []domain.Loop{
		{Project: "a", SessionID: "s1", State: domain.StateRunning},
		{Project: "b", SessionID: "s2", State: domain.StateRunning},
	}
	m.cursor = 0
	m, _ = updateModel(t, m, runeKey('n'))

	m, _ = updateModel(t, m, tea.KeyMsg{Type: tea.KeyDown})

	if m.cursor != 0 {
		t.Errorf("cursor = %d, want unchanged at 0 (down arrow must route to the text input while prompting)", m.cursor)
	}
}

// ── "k" kill double-press confirm ─────────────────────────────────

func TestUpdate_FirstK_SetsPendingKillWarning(t *testing.T) {
	m := modelWithOneLoop()

	m, cmd := updateModel(t, m, runeKey('k'))

	if m.pendingKillSession != "sess-1" {
		t.Errorf("pendingKillSession = %q, want %q", m.pendingKillSession, "sess-1")
	}
	if m.statusKind != statusWarn {
		t.Errorf("statusKind = %v, want statusWarn", m.statusKind)
	}
	if !strings.Contains(m.status, "press k again") {
		t.Errorf("status = %q, want it to prompt for a confirming k", m.status)
	}
	if cmd != nil {
		t.Error("expected no tea.Cmd on the first k (not confirmed yet)")
	}
}

func TestUpdate_SecondKWithinWindow_TriggersKill(t *testing.T) {
	m := modelWithOneLoop()
	m, _ = updateModel(t, m, runeKey('k'))

	m, cmd := updateModel(t, m, runeKey('k'))

	if m.pendingKillSession != "" {
		t.Error("expected pendingKillSession to clear once confirmed")
	}
	if cmd == nil {
		t.Fatal("expected a non-nil tea.Cmd (killCmd) on the confirming second k")
	}
	if !strings.Contains(m.status, "killing") {
		t.Errorf("status = %q, want it to mention killing", m.status)
	}
}

func TestUpdate_SecondKAfterWindowExpires_RestartsConfirmCycle(t *testing.T) {
	m := modelWithOneLoop()
	m, _ = updateModel(t, m, runeKey('k'))
	m.pendingKillAt = time.Now().Add(-killConfirmWindow - time.Second) // simulate the window having expired

	m, cmd := updateModel(t, m, runeKey('k'))

	if cmd != nil {
		t.Error("expected no kill to trigger once the confirm window has expired — a fresh cycle should start instead")
	}
	if m.pendingKillSession != "sess-1" {
		t.Error("expected a fresh pending-kill cycle to start for the same loop")
	}
}

// ── fix/killed-state: "k" on an already-dead loop ────────────────────────

func TestUpdate_KKey_AlreadyKilled_RefusesFastWithoutConfirmCycle(t *testing.T) {
	m := modelWithOneLoop()
	m.loops[0].State = domain.StateKilled

	m, cmd := updateModel(t, m, runeKey('k'))

	if cmd != nil {
		t.Error("expected no tea.Cmd — an already-killed loop must refuse before any confirm cycle")
	}
	if m.pendingKillSession != "" {
		t.Error("expected no pending-kill cycle to start for an already-killed loop")
	}
	if !strings.Contains(m.status, "already killed/gone") {
		t.Errorf("status = %q, want the already-killed/gone message", m.status)
	}
}

func TestUpdate_KKey_StallGone_RefusesFastWithoutConfirmCycle(t *testing.T) {
	m := modelWithOneLoop()
	m.loops[0].State = domain.StateStalled
	m.loops[0].Stall = domain.StallGone

	m, cmd := updateModel(t, m, runeKey('k'))

	if cmd != nil {
		t.Error("expected no tea.Cmd — a gone loop must refuse before any confirm cycle")
	}
	if !strings.Contains(m.status, "already killed/gone") {
		t.Errorf("status = %q, want the already-killed/gone message", m.status)
	}
}

func TestKillCmd_AlreadyKilled_ReturnsFriendlyMessage(t *testing.T) {
	l := domain.Loop{SessionID: "sess-1", Project: "aboard", State: domain.StateKilled}
	msg := killCmd(l)()
	km, ok := msg.(killResultMsg)
	if !ok {
		t.Fatalf("got %T, want killResultMsg", msg)
	}
	if !km.ok {
		t.Error("expected ok=true — this is a graceful no-op, not a failure")
	}
	if !strings.Contains(km.text, "already killed/gone") {
		t.Errorf("text = %q, want the already-killed/gone message", km.text)
	}
}

func TestKillCmd_SuccessMessage_DoesNotClaimImmediateStateChange(t *testing.T) {
	withFakeActuationSeams(t,
		func(sessionsDir, sessionID, projectDir string) (control.Controller, control.Target, bool, bool) {
			return &fakeController{name: "tmux"}, control.Target{Backend: "tmux", ID: "%1"}, true, true
		},
		nil,
	)
	l := domain.Loop{SessionID: "sess-1", Project: "aboard", State: domain.StateDrift}
	msg := killCmd(l)()
	km, ok := msg.(killResultMsg)
	if !ok || !km.ok {
		t.Fatalf("got %+v, want a successful killResultMsg", msg)
	}
	if !strings.Contains(km.text, "state updates on next scan") {
		t.Errorf("text = %q, want it to say the state updates on the next scan (not an optimistic local state change)", km.text)
	}
}

func TestKillCmd_Success_RecordsKillActuationEvent(t *testing.T) {
	withFakeActuationSeams(t,
		func(sessionsDir, sessionID, projectDir string) (control.Controller, control.Target, bool, bool) {
			return &fakeController{name: "tmux"}, control.Target{Backend: "tmux", ID: "%1"}, true, true
		},
		nil,
	)
	l := domain.Loop{SessionID: "sess-1", Project: "aboard", State: domain.StateDrift}
	killCmd(l)()

	got, err := events.ReadAll(historyDirFn())
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	evs := got["sess-1"]
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1: %#v", len(evs), evs)
	}
	if !strings.HasPrefix(evs[0].Detail, "kill ") {
		t.Errorf("Detail = %q, want it to start with \"kill \" (mostRecentActuationIsKill's expected format)", evs[0].Detail)
	}
}

func TestUpdate_IKey_KilledLoop_RefusesWithoutEnteringInjectMode(t *testing.T) {
	m := modelWithOneLoop()
	m.loops[0].State = domain.StateKilled

	m, cmd := updateModel(t, m, runeKey('i'))

	if cmd != nil {
		t.Error("expected no tea.Cmd — a killed loop must not enter inject mode")
	}
	if m.mode != modeNormal {
		t.Errorf("mode = %v, want modeNormal", m.mode)
	}
	if !strings.Contains(m.status, "killed") {
		t.Errorf("status = %q, want it to mention the loop was killed", m.status)
	}
}

func TestSendPromptCmd_KilledLoop_RefusesWithoutDispatching(t *testing.T) {
	// Belt-and-suspenders: sendPromptCmd itself must refuse StateKilled too
	// — this is the one shared choke point that matters, since Tier 2's
	// headless redrive is fully capable of reviving a killed session.
	redriveCalled := false
	withFakeActuationSeams(t, nil, func(sessionID, prompt string) error {
		redriveCalled = true
		return nil
	})
	l := domain.Loop{SessionID: "sess-1", Project: "aboard", State: domain.StateKilled}

	msg := sendPromptCmd(l, "do the thing", "inject", "injected into", "")()

	if redriveCalled {
		t.Error("expected redriveFn NOT to be called for a killed loop — Tier 2 could otherwise revive it")
	}
	rm, ok := msg.(resumeResultMsg)
	if !ok || rm.ok {
		t.Fatalf("got %+v, want a refused resumeResultMsg", msg)
	}
	if !strings.Contains(rm.text, "killed") {
		t.Errorf("text = %q, want it to mention the loop was killed", rm.text)
	}
}

// ── fix/killed-state: display + badge exclusion ──────────────────────────

func TestStateLabel_Killed(t *testing.T) {
	if got := stateLabel(domain.Loop{State: domain.StateKilled}); got != "☠ KILLED" {
		t.Errorf("got %q, want %q", got, "☠ KILLED")
	}
}

func TestStateColor_Killed_IsDimNotRed(t *testing.T) {
	got := stateColor(domain.Loop{State: domain.StateKilled})
	if got == cRed {
		t.Error("StateKilled must NOT be red — it's a completed human decision, not an incident")
	}
	if got != cDim {
		t.Errorf("got %v, want cDim", got)
	}
}

func TestCounts_KilledLoop_NotCountedAsStalledOrGated(t *testing.T) {
	m := New()
	m.loops = []domain.Loop{
		{SessionID: "s1", State: domain.StateKilled},
		{SessionID: "s2", State: domain.StateRunning},
	}
	total, running, stalled, idle, gated, _, _, _ := m.counts()
	if total != 2 {
		t.Errorf("total = %d, want 2 (killed loops still count toward fleet total)", total)
	}
	if running != 1 {
		t.Errorf("running = %d, want 1", running)
	}
	if stalled != 0 {
		t.Errorf("stalled = %d, want 0 (killed must not be counted as stalled)", stalled)
	}
	if gated != 0 {
		t.Errorf("gated = %d, want 0", gated)
	}
	if idle != 0 {
		t.Errorf("idle = %d, want 0", idle)
	}
}

func TestUpdate_AnyOtherKey_ClearsPendingKill(t *testing.T) {
	m := modelWithOneLoop()
	m, _ = updateModel(t, m, runeKey('k'))
	if m.pendingKillSession == "" {
		t.Fatal("precondition failed: expected pendingKillSession to be set")
	}

	m, _ = updateModel(t, m, tea.KeyMsg{Type: tea.KeyDown})

	if m.pendingKillSession != "" {
		t.Error("expected pendingKillSession to be cleared by an unrelated keypress")
	}
}

// ── P0-1/P0-2 ambiguity guard ───────────────────────────────────────
//
// Locate/LocateClaude match a terminal surface by ProjectDir (a directory),
// but loops are SESSIONS — when more than one loop in the fleet shares a
// directory, a typed/destructive action could silently land on the wrong
// one. refuseIfAmbiguous must block r/a/p/k (never attach/o) whenever the
// selected loop's ProjectDir is shared by another loop in the fleet.

func modelWithTwoLoopsSharingDir() Model {
	m := New()
	m.loops = []domain.Loop{
		{Project: "aboard", SessionID: "sess-1", ProjectDir: "-x-aboard", Cwd: "/x/aboard", CwdVerified: true, State: domain.StateStalled},
		{Project: "aboard", SessionID: "sess-2", ProjectDir: "-x-aboard", Cwd: "/x/aboard", CwdVerified: true, State: domain.StateStalled},
	}
	m.cursor = 0
	return m
}

func TestUpdate_RKey_AmbiguousSharedDir_Refuses(t *testing.T) {
	m := modelWithTwoLoopsSharingDir()

	m, cmd := updateModel(t, m, runeKey('r'))

	if cmd != nil {
		t.Error("expected no tea.Cmd — ambiguous target must refuse before any actuation")
	}
	if m.statusKind != statusErr {
		t.Errorf("statusKind = %v, want statusErr", m.statusKind)
	}
	if !strings.Contains(m.status, "ambiguous") {
		t.Errorf("status = %q, want it to mention the ambiguity", m.status)
	}
}

func TestUpdate_RKey_SingleLoopInDir_Proceeds(t *testing.T) {
	// the counterpart case: exactly one loop shares this directory, so the
	// guard must NOT refuse.
	m := modelWithOneLoop()
	m.loops[0].State = domain.StateStalled

	m, cmd := updateModel(t, m, runeKey('r'))

	if cmd == nil {
		t.Error("expected a non-nil tea.Cmd (resumeCmd) — only one loop shares this directory")
	}
	if m.statusKind == statusErr {
		t.Errorf("statusKind = %v, want not statusErr (a single loop must not be refused)", m.statusKind)
	}
}

// ── ttyPathPlausible: skip the ambiguity guard when a registry tty could
// resolve this session uniquely (ADR Phase 2, §2.2/§3 step 2) ────────

// withSessionsDir points sessionsDirFn at a fresh temp dir for the duration
// of one test, restoring the original on cleanup.
func withSessionsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	orig := sessionsDirFn
	t.Cleanup(func() { sessionsDirFn = orig })
	sessionsDirFn = func() string { return dir }
	return dir
}

func TestTtyPathPlausible_EntryWithTTY_True(t *testing.T) {
	dir := withSessionsDir(t)
	if err := sessions.WriteSession(dir, "sess-1", sessions.SessionEntry{TTY: "ttys012"}); err != nil {
		t.Fatalf("WriteSession: %v", err)
	}

	m := New()
	if !m.ttyPathPlausible(domain.Loop{SessionID: "sess-1"}) {
		t.Error("expected true — a registry entry with a non-empty tty exists")
	}
}

func TestTtyPathPlausible_EntryWithEmptyTTY_False(t *testing.T) {
	dir := withSessionsDir(t)
	if err := sessions.WriteSession(dir, "sess-1", sessions.SessionEntry{TTY: ""}); err != nil {
		t.Fatalf("WriteSession: %v", err)
	}

	m := New()
	if m.ttyPathPlausible(domain.Loop{SessionID: "sess-1"}) {
		t.Error("expected false — the registry entry has no tty (headless/-p session)")
	}
}

func TestTtyPathPlausible_NoEntry_False(t *testing.T) {
	withSessionsDir(t)

	m := New()
	if m.ttyPathPlausible(domain.Loop{SessionID: "never-registered"}) {
		t.Error("expected false — no registry entry at all")
	}
}

func TestUpdate_RKey_AmbiguousSharedDir_ButTTYPlausible_Proceeds(t *testing.T) {
	// the whole point of Tier 1a: two loops sharing a directory are no
	// longer ambiguous once the selected one has a known tty — the
	// keypress-time guard must not refuse just because ANOTHER loop happens
	// to share the same cwd.
	dir := withSessionsDir(t)
	m := modelWithTwoLoopsSharingDir()
	if err := sessions.WriteSession(dir, m.loops[0].SessionID, sessions.SessionEntry{TTY: "ttys012"}); err != nil {
		t.Fatalf("WriteSession: %v", err)
	}

	m, cmd := updateModel(t, m, runeKey('r'))

	if cmd == nil {
		t.Error("expected a non-nil tea.Cmd (resumeCmd) — the tty path makes this unambiguous")
	}
	if m.statusKind == statusErr {
		t.Errorf("statusKind = %v, want not statusErr", m.statusKind)
	}
}

func TestUpdate_IKey_AmbiguousSharedDir_ButTTYPlausible_EntersInjectingMode(t *testing.T) {
	dir := withSessionsDir(t)
	m := modelWithTwoLoopsSharingDir()
	if err := sessions.WriteSession(dir, m.loops[0].SessionID, sessions.SessionEntry{TTY: "ttys012"}); err != nil {
		t.Fatalf("WriteSession: %v", err)
	}

	m, cmd := updateModel(t, m, runeKey('i'))

	if m.mode != modeInjecting {
		t.Errorf("mode = %v, want modeInjecting (the tty path makes this unambiguous)", m.mode)
	}
	if cmd == nil {
		t.Error("expected a non-nil tea.Cmd (textinput.Blink)")
	}
}

func TestUpdate_RKey_StateFailed_BlockedByKeyGuard(t *testing.T) {
	// the "r" keypress guard only allows StateStalled/StateDrift — a
	// governor-failed loop must never even reach resumeCmd via this path.
	m := modelWithOneLoop()
	m.loops[0].State = domain.StateFailed

	m, cmd := updateModel(t, m, runeKey('r'))

	if cmd != nil {
		t.Error("expected no tea.Cmd — StateFailed is not resumable via the r key")
	}
	if !strings.Contains(m.status, "stalled or drifted") {
		t.Errorf("status = %q, want the r-key guard's usual message", m.status)
	}
}

// ── feat/drift-guided-redrive ─────────────────────────────────────────────

// ── mode transitions ──────────────────────────────────────────────────────

func TestUpdate_RKey_DriftLoop_EntersModeDriftHint(t *testing.T) {
	m := modelWithOneLoop()
	m.loops[0].State = domain.StateDrift

	m, cmd := updateModel(t, m, runeKey('r'))

	if m.mode != modeDriftHint {
		t.Fatalf("mode = %v, want modeDriftHint", m.mode)
	}
	if m.driftHintTarget.SessionID != "sess-1" {
		t.Errorf("driftHintTarget = %+v, want it snapshotted to sess-1", m.driftHintTarget)
	}
	if cmd == nil {
		t.Error("expected a non-nil tea.Cmd (textinput.Blink)")
	}
}

// TestUpdate_RKey_StalledLoop_NonDriftBypass_StillImmediateResume is the
// task's explicit "non-drift bypass": StateStalled must keep the EXISTING
// immediate-resume behavior (no input step at all).
func TestUpdate_RKey_StalledLoop_NonDriftBypass_StillImmediateResume(t *testing.T) {
	m := modelWithOneLoop()
	m.loops[0].State = domain.StateStalled

	m, cmd := updateModel(t, m, runeKey('r'))

	if m.mode == modeDriftHint {
		t.Error("expected StateStalled to bypass the hint-input step entirely")
	}
	if cmd == nil {
		t.Error("expected a non-nil tea.Cmd (resumeCmd, dispatched immediately)")
	}
	if !strings.Contains(m.status, "resuming") {
		t.Errorf("status = %q, want the immediate-resume status text", m.status)
	}
}

func TestUpdate_RKey_DriftLoop_AmbiguousSharedDir_RefusesBeforeEnteringHintMode(t *testing.T) {
	m := modelWithTwoLoopsSharingDir()
	m.loops[0].State = domain.StateDrift
	m.loops[1].State = domain.StateDrift

	m, cmd := updateModel(t, m, runeKey('r'))

	if cmd != nil {
		t.Error("expected no tea.Cmd — ambiguous target must refuse before entering hint mode")
	}
	if m.mode == modeDriftHint {
		t.Error("expected the ambiguity guard to prevent entering modeDriftHint at all")
	}
	if m.statusKind != statusErr || !strings.Contains(m.status, "ambiguous") {
		t.Errorf("status = %q (kind %v), want an ambiguity refusal", m.status, m.statusKind)
	}
}

func TestUpdate_RKey_DriftLoop_AlreadyActuating_Refuses(t *testing.T) {
	m := modelWithOneLoop()
	m.loops[0].State = domain.StateDrift
	m.actuating = map[string]bool{"sess-1": true}

	m, cmd := updateModel(t, m, runeKey('r'))

	if cmd != nil {
		t.Error("expected no tea.Cmd — already actuating")
	}
	if m.mode == modeDriftHint {
		t.Error("expected the in-flight guard to prevent entering modeDriftHint")
	}
}

func TestUpdate_DriftHint_Esc_Cancels(t *testing.T) {
	m := modelWithOneLoop()
	m.loops[0].State = domain.StateDrift
	m, _ = updateModel(t, m, runeKey('r'))
	if m.mode != modeDriftHint {
		t.Fatal("setup: expected modeDriftHint")
	}

	m, cmd := updateModel(t, m, tea.KeyMsg{Type: tea.KeyEsc})

	if m.mode != modeNormal {
		t.Errorf("mode = %v, want modeNormal after esc", m.mode)
	}
	if cmd != nil {
		t.Error("expected no tea.Cmd on cancel")
	}
	if !strings.Contains(m.status, "cancelled") {
		t.Errorf("status = %q, want a cancellation message", m.status)
	}
}

func TestUpdate_DriftHint_EnterWithHint_DispatchesDriftRedrive(t *testing.T) {
	m := modelWithOneLoop()
	m.loops[0].State = domain.StateDrift
	m, _ = updateModel(t, m, runeKey('r'))

	for _, r := range "check the auth header casing" {
		m, _ = updateModel(t, m, runeKey(r))
	}
	m, cmd := updateModel(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	if m.mode != modeNormal {
		t.Errorf("mode = %v, want modeNormal after submit", m.mode)
	}
	if cmd == nil {
		t.Fatal("expected a non-nil tea.Cmd (driftRedriveCmd)")
	}
	if !m.actuating["sess-1"] {
		t.Error("expected sess-1 to be marked actuating after dispatch")
	}
}

// TestUpdate_DriftHint_EnterEmpty_StillDispatches is the task's
// "enter=none" behavior: unlike modeInjecting's empty-prompt-cancels
// convention, an EMPTY hint submission on modeDriftHint is a valid choice
// (re-drive with no hint), not a cancel.
func TestUpdate_DriftHint_EnterEmpty_StillDispatches(t *testing.T) {
	m := modelWithOneLoop()
	m.loops[0].State = domain.StateDrift
	m, _ = updateModel(t, m, runeKey('r'))

	m, cmd := updateModel(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	if m.mode != modeNormal {
		t.Errorf("mode = %v, want modeNormal", m.mode)
	}
	if cmd == nil {
		t.Error("expected a non-nil tea.Cmd even with an empty hint — enter=none still re-drives")
	}
}

func TestUpdate_DriftHint_AlreadyActuatingAtSubmitTime_Refuses(t *testing.T) {
	m := modelWithOneLoop()
	m.loops[0].State = domain.StateDrift
	m, _ = updateModel(t, m, runeKey('r'))
	m.actuating = map[string]bool{"sess-1": true} // simulate a race: became actuating while typing

	m, cmd := updateModel(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	if cmd != nil {
		t.Error("expected no tea.Cmd — already actuating by submit time")
	}
	if !strings.Contains(m.status, "already re-driving") {
		t.Errorf("status = %q, want the already-re-driving message", m.status)
	}
}

func TestUpdate_ArrowKeysWhileDriftHint_RouteToInputNotCursor(t *testing.T) {
	m := modelWithOneLoop()
	m.loops[0].State = domain.StateDrift
	m, _ = updateModel(t, m, runeKey('r'))
	beforeCursor := m.cursor

	m, _ = updateModel(t, m, tea.KeyMsg{Type: tea.KeyDown})

	if m.cursor != beforeCursor {
		t.Errorf("cursor changed from %d to %d — arrow keys must route to the input during modeDriftHint, not move the cursor", beforeCursor, m.cursor)
	}
}

// ── prompt composition (pure function) ────────────────────────────────────

func TestComposeDriftPrompt_WithHint_Appends(t *testing.T) {
	got := composeDriftPrompt("fix the auth test", "check the header casing")
	want := "fix the auth test\n\n[operator correction] check the header casing"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestComposeDriftPrompt_EmptyHint_ReturnsUnchanged(t *testing.T) {
	if got := composeDriftPrompt("fix the auth test", ""); got != "fix the auth test" {
		t.Errorf("got %q, want the prompt unchanged", got)
	}
}

func TestComposeDriftPrompt_WhitespaceOnlyHint_TreatedAsEmpty(t *testing.T) {
	if got := composeDriftPrompt("fix the auth test", "   \t  "); got != "fix the auth test" {
		t.Errorf("got %q, want the prompt unchanged for a whitespace-only hint", got)
	}
}

func TestComposeDriftPrompt_HintIsTrimmed(t *testing.T) {
	got := composeDriftPrompt("fix it", "  add a retry  ")
	want := "fix it\n\n[operator correction] add a retry"
	if got != want {
		t.Errorf("got %q, want the hint trimmed of surrounding whitespace", got)
	}
}

// ── driftRedriveCmd dispatch ───────────────────────────────────────────────

func TestDriftRedriveCmd_ComposesHintIntoSentPrompt(t *testing.T) {
	var gotPrompt string
	fakeCtrl := &fakeController{name: "tmux"}
	withFakeActuationSeams(t,
		func(sessionsDir, sessionID, projectDir string) (control.Controller, control.Target, bool, bool) {
			return fakeCtrl, control.Target{Backend: "tmux", ID: "%3"}, true, true
		},
		nil,
	)
	path := writeTranscriptLastUserPrompt(t, "fix the flaky auth test")
	l := domain.Loop{SessionID: "sess-1", Project: "aboard", State: domain.StateDrift, Path: path}

	msg := driftRedriveCmd(l, "check the header casing")()

	gotPrompt = fakeCtrl.lastResumePrompt
	want := "fix the flaky auth test\n\n[operator correction] check the header casing"
	if gotPrompt != want {
		t.Errorf("sent prompt = %q, want %q", gotPrompt, want)
	}
	rm, ok := msg.(resumeResultMsg)
	if !ok || !rm.ok {
		t.Fatalf("got %+v, want a successful resumeResultMsg", msg)
	}
}

func TestDriftRedriveCmd_EmptyHint_SendsPromptUnchanged(t *testing.T) {
	fakeCtrl := &fakeController{name: "tmux"}
	withFakeActuationSeams(t,
		func(sessionsDir, sessionID, projectDir string) (control.Controller, control.Target, bool, bool) {
			return fakeCtrl, control.Target{Backend: "tmux", ID: "%3"}, true, true
		},
		nil,
	)
	path := writeTranscriptLastUserPrompt(t, "fix the flaky auth test")
	l := domain.Loop{SessionID: "sess-1", Project: "aboard", State: domain.StateDrift, Path: path}

	driftRedriveCmd(l, "")()

	if fakeCtrl.lastResumePrompt != "fix the flaky auth test" {
		t.Errorf("sent prompt = %q, want the original prompt unchanged (enter=none)", fakeCtrl.lastResumePrompt)
	}
}

// writeTranscriptLastUserPrompt writes a minimal transcript JSONL whose
// last user message is prompt, so claude.LastUserPrompt(path) returns it.
func writeTranscriptLastUserPrompt(t *testing.T, prompt string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	line := fmt.Sprintf(`{"type":"user","message":{"role":"user","content":%q}}`, prompt)
	if err := os.WriteFile(path, []byte(line+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// ── DRIFT callout text ─────────────────────────────────────────────────────

func TestRenderDriftCallout_MentionsReDriveWithHint(t *testing.T) {
	l := domain.Loop{Last: &domain.Verdict{Outcome: domain.OutcomeRejected, Reason: "no evidence"}}
	got := renderDriftCallout(l, 80)
	if !strings.Contains(got, "re-drive with hint") {
		t.Errorf("got %q, want the callout to mention \"re-drive with hint\"", got)
	}
}

func TestResumeCmd_StateFailed_RefusesWithGovernorMessage(t *testing.T) {
	// belt-and-suspenders: resumeCmd itself must refuse on StateFailed too,
	// independent of the "r" keypress guard (see resumeCmd's SAFETY comment).
	l := domain.Loop{SessionID: "sess-1", Project: "aboard", State: domain.StateFailed}

	msg := resumeCmd(l)()

	rm, ok := msg.(resumeResultMsg)
	if !ok {
		t.Fatalf("got %T, want resumeResultMsg", msg)
	}
	if rm.ok {
		t.Error("expected ok=false")
	}
	if !strings.Contains(rm.text, "governor stopped this loop") {
		t.Errorf("text = %q, want it to mention the governor stopped the loop", rm.text)
	}
}

func TestUpdate_AKey_AmbiguousSharedDir_Refuses(t *testing.T) {
	m := modelWithTwoLoopsSharingDir()
	m.loops[0].State = domain.StateGate

	m, cmd := updateModel(t, m, runeKey('a'))

	if cmd != nil {
		t.Error("expected no tea.Cmd — ambiguous target must refuse before any actuation")
	}
	if !strings.Contains(m.status, "ambiguous") {
		t.Errorf("status = %q, want it to mention the ambiguity", m.status)
	}
}

func TestUpdate_PKey_AmbiguousSharedDir_Refuses(t *testing.T) {
	m := modelWithTwoLoopsSharingDir()
	m.loops[0].State = domain.StateRunning

	m, cmd := updateModel(t, m, runeKey('p'))

	if cmd != nil {
		t.Error("expected no tea.Cmd — ambiguous target must refuse before any actuation")
	}
	if !strings.Contains(m.status, "ambiguous") {
		t.Errorf("status = %q, want it to mention the ambiguity", m.status)
	}
}

func TestUpdate_KKey_AmbiguousSharedDir_ConfirmingPressRefuses(t *testing.T) {
	m := modelWithTwoLoopsSharingDir()

	m, _ = updateModel(t, m, runeKey('k')) // arm the pending-kill confirm
	if m.pendingKillSession == "" {
		t.Fatal("precondition failed: expected the first k to arm the pending kill")
	}

	m, cmd := updateModel(t, m, runeKey('k')) // confirming press

	if cmd != nil {
		t.Error("expected no tea.Cmd on the confirming k — ambiguous target must refuse")
	}
	if !strings.Contains(m.status, "ambiguous") {
		t.Errorf("status = %q, want it to mention the ambiguity", m.status)
	}
}

// ── "i" inject arbitrary prompt ───────────────────────────────────
//
// The "i" key snapshots the selected loop and drops into modeInjecting so the
// human can type a brand-new prompt to send into it — without attaching
// first. It mirrors the r-key/sendPromptCmd double-guard: a keypress-time
// state gate (StallGone/StateFailed refused early, before typing) PLUS the
// same guard re-checked inside sendPromptCmd (belt-and-suspenders).

func TestUpdate_IKey_EntersInjectingModeWithSelectedLoop(t *testing.T) {
	m := modelWithOneLoop() // one StateRunning loop, unambiguous dir

	m, cmd := updateModel(t, m, runeKey('i'))

	if m.mode != modeInjecting {
		t.Fatalf("mode = %v, want modeInjecting", m.mode)
	}
	if m.injectTarget.SessionID != "sess-1" {
		t.Errorf("injectTarget.SessionID = %q, want the selected loop's %q", m.injectTarget.SessionID, "sess-1")
	}
	if !m.input.Focused() {
		t.Error("expected the text input to be focused after entering inject mode")
	}
	if cmd == nil {
		t.Error("expected a non-nil tea.Cmd (textinput.Blink) on entering inject mode")
	}
}

func TestUpdate_IKey_NoSelection_ShowsStatus(t *testing.T) {
	m := New() // no loops, nothing selected

	m, cmd := updateModel(t, m, runeKey('i'))

	if m.mode != modeNormal {
		t.Errorf("mode = %v, want modeNormal (no selection must not enter inject mode)", m.mode)
	}
	if !strings.Contains(m.status, "select a loop to send a prompt to") {
		t.Errorf("status = %q, want the select-a-loop prompt", m.status)
	}
	if cmd != nil {
		t.Error("expected no tea.Cmd when there's nothing selected")
	}
}

func TestUpdate_IKey_AmbiguousSharedDir_Refuses(t *testing.T) {
	m := modelWithTwoLoopsSharingDir()

	m, cmd := updateModel(t, m, runeKey('i'))

	if cmd != nil {
		t.Error("expected no tea.Cmd — ambiguous target must refuse before entering inject mode")
	}
	if m.mode != modeNormal {
		t.Errorf("mode = %v, want modeNormal (ambiguous target must not enter inject mode)", m.mode)
	}
	if m.statusKind != statusErr {
		t.Errorf("statusKind = %v, want statusErr", m.statusKind)
	}
	if !strings.Contains(m.status, "ambiguous") {
		t.Errorf("status = %q, want it to mention the ambiguity", m.status)
	}
}

func TestUpdate_IKey_StallGone_EntersInjectingMode(t *testing.T) {
	// StallGone no longer refuses at the "i" keypress guard — the ADR Phase
	// 2 Tier 2 redrive path means it's now a perfectly valid inject target
	// (routed headlessly once submitted), so it must reach inject mode like
	// any other loop.
	m := modelWithOneLoop()
	m.loops[0].Stall = domain.StallGone

	m, cmd := updateModel(t, m, runeKey('i'))

	if m.mode != modeInjecting {
		t.Fatalf("mode = %v, want modeInjecting (StallGone is now a valid inject target)", m.mode)
	}
	if cmd == nil {
		t.Error("expected a non-nil tea.Cmd (textinput.Blink)")
	}
}

func TestUpdate_IKey_StateFailed_BlockedByKeyGuard(t *testing.T) {
	// the "i" keypress guard must refuse a governor-failed loop early, so it
	// never reaches inject mode (mirrors resumeCmd's StateFailed guard).
	m := modelWithOneLoop()
	m.loops[0].State = domain.StateFailed

	m, cmd := updateModel(t, m, runeKey('i'))

	if cmd != nil {
		t.Error("expected no tea.Cmd — StateFailed is not injectable via the i key")
	}
	if m.mode != modeNormal {
		t.Errorf("mode = %v, want modeNormal (StateFailed must not enter inject mode)", m.mode)
	}
	if !strings.Contains(m.status, "governor stopped this loop") {
		t.Errorf("status = %q, want the governor-stopped message", m.status)
	}
}

func TestSendPromptCmd_StateFailed_RefusesWithGovernorMessage(t *testing.T) {
	// belt-and-suspenders: sendPromptCmd itself must refuse on StateFailed too,
	// independent of the "i"/"r" keypress guard (see its SAFETY comment).
	l := domain.Loop{SessionID: "sess-1", Project: "aboard", State: domain.StateFailed}

	msg := sendPromptCmd(l, "do the thing", "inject", "injected into", "")()

	rm, ok := msg.(resumeResultMsg)
	if !ok {
		t.Fatalf("got %T, want resumeResultMsg", msg)
	}
	if rm.ok {
		t.Error("expected ok=false")
	}
	if !strings.Contains(rm.text, "governor stopped this loop") {
		t.Errorf("text = %q, want it to mention the governor stopped the loop", rm.text)
	}
}

// ── ADR Phase 2 tier policy (tty → cwd → headless redrive) ───────────
//
// sendPromptCmd/approveCmd/interruptCmd/killCmd all resolve a surface via
// resolveActuationTargetFn (control.ResolveActuationTarget by default,
// overridable here) — Tier 1. sendPromptCmd additionally falls to redriveFn
// (control.Redrive by default) — Tier 2 — when Tier 1 doesn't resolve a
// surface, or immediately (skipping Tier 1 outright) for a StallGone loop.
// These seams let the whole state machine be exercised without touching a
// real ~/.missionctl/sessions or shelling out to tmux/claude.

// withFakeActuationSeams overrides resolveActuationTargetFn/redriveFn for
// the duration of one test, restoring the originals on cleanup.
// withFakeActuationSeams also overrides historyDirFn to a t.TempDir() — any
// test that reaches a real tier dispatch (success or failure) now also
// triggers logActuationEvent's events.Append call, which must never touch
// the real ~/.missionctl/history during `go test`.
func withFakeActuationSeams(t *testing.T, resolve func(sessionsDir, sessionID, projectDir string) (control.Controller, control.Target, bool, bool), redrive func(sessionID, prompt string) error) {
	t.Helper()
	origResolve, origRedrive, origHistoryDir := resolveActuationTargetFn, redriveFn, historyDirFn
	t.Cleanup(func() { resolveActuationTargetFn, redriveFn, historyDirFn = origResolve, origRedrive, origHistoryDir })
	if resolve != nil {
		resolveActuationTargetFn = resolve
	}
	if redrive != nil {
		redriveFn = redrive
	}
	historyDir := t.TempDir()
	historyDirFn = func() string { return historyDir }
}

func TestSendPromptCmd_StallGone_SkipsTierOne_GoesStraightToTierTwo(t *testing.T) {
	tier1Called := false
	var gotSessionID, gotPrompt string
	withFakeActuationSeams(t,
		func(sessionsDir, sessionID, projectDir string) (control.Controller, control.Target, bool, bool) {
			tier1Called = true
			return nil, control.Target{}, true, true // would succeed if tried — must NOT be tried
		},
		func(sessionID, prompt string) error {
			gotSessionID, gotPrompt = sessionID, prompt
			return nil
		},
	)
	l := domain.Loop{SessionID: "sess-1", Project: "aboard", Stall: domain.StallGone}

	msg := sendPromptCmd(l, "do the thing", "inject", "injected into", "")()

	if tier1Called {
		t.Error("expected Tier 1 (resolveActuationTargetFn) NOT to be called for a StallGone loop")
	}
	if gotSessionID != "sess-1" || gotPrompt != "do the thing" {
		t.Errorf("redriveFn called with (%q, %q), want (sess-1, do the thing)", gotSessionID, gotPrompt)
	}
	rm, ok := msg.(resumeResultMsg)
	if !ok {
		t.Fatalf("got %T, want resumeResultMsg", msg)
	}
	if !rm.ok {
		t.Errorf("expected ok=true, got text %q", rm.text)
	}
	if !strings.Contains(rm.text, "headlessly (tier 2)") {
		t.Errorf("text = %q, want it to mention the tier-2 redrive", rm.text)
	}
}

func TestSendPromptCmd_TierOneFound_UsesTierOneNotRedrive(t *testing.T) {
	redriveCalled := false
	fakeCtrl := &fakeController{name: "tmux"}
	withFakeActuationSeams(t,
		func(sessionsDir, sessionID, projectDir string) (control.Controller, control.Target, bool, bool) {
			return fakeCtrl, control.Target{Backend: "tmux", ID: "%3"}, true, true
		},
		func(sessionID, prompt string) error {
			redriveCalled = true
			return nil
		},
	)
	l := domain.Loop{SessionID: "sess-1", Project: "aboard"}

	msg := sendPromptCmd(l, "do the thing", "resume", "resumed", "")()

	if redriveCalled {
		t.Error("expected redriveFn NOT to be called when Tier 1 already found a surface")
	}
	if !fakeCtrl.resumeCalled {
		t.Error("expected ctrl.Resume to be called with the Tier 1 target")
	}
	rm, ok := msg.(resumeResultMsg)
	if !ok || !rm.ok {
		t.Fatalf("got %+v, want a successful resumeResultMsg", msg)
	}
	if !strings.Contains(rm.text, "via tmux") {
		t.Errorf("text = %q, want it to mention the Tier 1 backend", rm.text)
	}
}

func TestSendPromptCmd_TierOneNotFound_FallsToTierTwoRedrive(t *testing.T) {
	redriveCalled := false
	withFakeActuationSeams(t,
		func(sessionsDir, sessionID, projectDir string) (control.Controller, control.Target, bool, bool) {
			return nil, control.Target{}, true, false // backend resolved, but no surface located
		},
		func(sessionID, prompt string) error {
			redriveCalled = true
			return nil
		},
	)
	l := domain.Loop{SessionID: "sess-1", Project: "aboard"}

	msg := sendPromptCmd(l, "do the thing", "resume", "resumed", "")()

	if !redriveCalled {
		t.Error("expected redriveFn to be called once Tier 1 fails to find a surface")
	}
	rm, ok := msg.(resumeResultMsg)
	if !ok || !rm.ok {
		t.Fatalf("got %+v, want a successful resumeResultMsg", msg)
	}
}

func TestSendPromptCmd_TierTwoRedriveFails_ReportsError(t *testing.T) {
	withFakeActuationSeams(t,
		func(sessionsDir, sessionID, projectDir string) (control.Controller, control.Target, bool, bool) {
			return nil, control.Target{}, false, false
		},
		func(sessionID, prompt string) error {
			return errTestJudgeFailed
		},
	)
	l := domain.Loop{SessionID: "sess-1", Project: "aboard"}

	msg := sendPromptCmd(l, "do the thing", "resume", "resumed", "")()

	rm, ok := msg.(resumeResultMsg)
	if !ok {
		t.Fatalf("got %T, want resumeResultMsg", msg)
	}
	if rm.ok {
		t.Error("expected ok=false")
	}
	if !strings.Contains(rm.text, "re-drive") {
		t.Errorf("text = %q, want it to mention the failed re-drive", rm.text)
	}
}

// ── F4: the ambiguity guard's authoritative backstop survives the
// keypress-time ttyPathPlausible skip ──────────────────────────────
//
// ttyPathPlausible only skips refuseIfAmbiguous's FAST/FRIENDLY keypress-time
// message when a registry entry with a tty exists — it does not (and
// synchronously cannot) validate the tty↔pid binding. If that binding
// later fails inside control.ResolveActuationTarget (recycled tty, dead
// pid, or the tty simply doesn't resolve to a claude pane), Tier 1a is
// skipped and resolution falls to Tier 1b (cwd chain), whose LocateClaude
// carries its own internal ">1 match" refusal. These tests prove that
// fallback refusal actually fires — a genuinely ambiguous loop is never
// silently misrouted just because the keypress guard was bypassed.

func TestSendPromptCmd_TTYPlausibleButBindingFails_FallsToTierTwoNotMisrouted(t *testing.T) {
	// Tier 1 (both a and b) fails to find an unambiguous surface — for
	// resume/inject this correctly falls to Tier 2 (redrive by session id,
	// which doesn't care about cwd ambiguity at all), rather than guessing
	// at a Tier 1 target.
	redriveCalled := false
	withFakeActuationSeams(t,
		func(sessionsDir, sessionID, projectDir string) (control.Controller, control.Target, bool, bool) {
			// simulates: tty binding failed, AND the cwd chain's LocateClaude
			// refused internally because >1 loop matched that directory.
			return nil, control.Target{}, true, false
		},
		func(sessionID, prompt string) error {
			redriveCalled = true
			return nil
		},
	)
	l := domain.Loop{SessionID: "sess-1", Project: "aboard"}

	msg := sendPromptCmd(l, "do the thing", "resume", "resumed", "")()

	if !redriveCalled {
		t.Error("expected Tier 2 (redriveFn) to run once Tier 1 fails to find an unambiguous surface")
	}
	rm, ok := msg.(resumeResultMsg)
	if !ok || !rm.ok {
		t.Fatalf("got %+v, want a successful resumeResultMsg (Tier 2 succeeded)", msg)
	}
	if !strings.Contains(rm.text, "tier 2") {
		t.Errorf("text = %q, want it to mention the tier-2 fallback", rm.text)
	}
}

func TestApproveCmd_TierOneFailsAmbiguously_RefusesWithoutMisrouting(t *testing.T) {
	// approve/interrupt/kill have no Tier 2 — when Tier 1 fails to find an
	// unambiguous surface, they must refuse outright, never guess.
	withFakeActuationSeams(t,
		func(sessionsDir, sessionID, projectDir string) (control.Controller, control.Target, bool, bool) {
			return nil, control.Target{}, true, false // ambiguous cwd match, refused internally by LocateClaude
		},
		nil,
	)
	l := domain.Loop{SessionID: "sess-1", Project: "aboard", GateTS: 123}

	msg := approveCmd(l)()

	am, ok := msg.(approveResultMsg)
	if !ok {
		t.Fatalf("got %T, want approveResultMsg", msg)
	}
	if am.ok {
		t.Error("expected ok=false — must refuse rather than guess when Tier 1 is ambiguous")
	}
	if !strings.Contains(am.text, "no unambiguous claude surface") {
		t.Errorf("text = %q, want the ambiguity-refusal message", am.text)
	}
}

func TestUpdate_AKey_TTYPlausibleSkipsGuard_ButAsyncResultStillRefusesOnAmbiguity(t *testing.T) {
	// full round trip through the tui: two loops share a directory, so
	// refuseIfAmbiguous WOULD normally refuse at keypress time — but the
	// selected loop has a registry tty, so ttyPathPlausible skips that
	// keypress-time guard and dispatches approveCmd. The async resolution
	// (faked here to simulate a binding failure that falls to an ambiguous
	// cwd match) must still surface a refusal once the result arrives —
	// proving the skip never lets an ambiguous action silently succeed.
	dir := withSessionsDir(t)
	m := modelWithTwoLoopsSharingDir()
	m.loops[0].State = domain.StateGate
	if err := sessions.WriteSession(dir, m.loops[0].SessionID, sessions.SessionEntry{TTY: "ttys012"}); err != nil {
		t.Fatalf("WriteSession: %v", err)
	}
	withFakeActuationSeams(t,
		func(sessionsDir, sessionID, projectDir string) (control.Controller, control.Target, bool, bool) {
			return nil, control.Target{}, true, false
		},
		nil,
	)

	m, cmd := updateModel(t, m, runeKey('a'))
	if cmd == nil {
		t.Fatal("expected a non-nil tea.Cmd — the tty path skips the keypress-time ambiguity guard")
	}

	msg := cmd()
	m, _ = updateModel(t, m, msg)

	if m.statusKind != statusErr {
		t.Errorf("statusKind = %v, want statusErr — the async resolution must still refuse", m.statusKind)
	}
	if !strings.Contains(m.status, "no unambiguous claude surface") {
		t.Errorf("status = %q, want the ambiguity-refusal message once the async result arrives", m.status)
	}
}

// ── P1-2: in-flight actuation guard (m.actuating) ────────────────────
//
// A double-press of r/i on the SAME session must not fire two concurrent
// sends — most acutely, two concurrent Tier-2 `claude --resume` turns, each
// holding a 10-minute window.

func TestUpdate_RKey_SecondPressWhileActuating_RefusesWithoutSecondRedrive(t *testing.T) {
	redriveCalls := 0
	withFakeActuationSeams(t,
		func(sessionsDir, sessionID, projectDir string) (control.Controller, control.Target, bool, bool) {
			return nil, control.Target{}, false, false // Tier 1 never resolves — every dispatch would reach Tier 2
		},
		func(sessionID, prompt string) error {
			redriveCalls++
			return nil
		},
	)
	m := modelWithOneLoop()
	m.loops[0].State = domain.StateStalled

	// first press: dispatches resumeCmd and marks sess-1 as actuating.
	m, cmd1 := updateModel(t, m, runeKey('r'))
	if cmd1 == nil {
		t.Fatal("expected a non-nil tea.Cmd on the first press")
	}
	if !m.actuating["sess-1"] {
		t.Fatal("expected sess-1 to be marked actuating after the first dispatch")
	}

	// second press, BEFORE the first cmd's result has arrived: must refuse
	// without dispatching a second send.
	m, cmd2 := updateModel(t, m, runeKey('r'))
	if cmd2 != nil {
		t.Error("expected no tea.Cmd on the second press while still actuating")
	}
	if !strings.Contains(m.status, "already re-driving") {
		t.Errorf("status = %q, want it to mention the in-flight re-drive", m.status)
	}

	// now actually run the first (and only) dispatched cmd — redriveFn must
	// have been invoked exactly once, never twice.
	cmd1()
	if redriveCalls != 1 {
		t.Errorf("redriveFn called %d times, want exactly 1", redriveCalls)
	}
}

func TestUpdate_IKey_SecondPressWhileActuating_Refuses(t *testing.T) {
	m := modelWithOneLoop()
	if m.actuating == nil {
		m.actuating = map[string]bool{}
	}
	m.actuating["sess-1"] = true // simulate an in-flight send from an earlier r/i dispatch

	m, cmd := updateModel(t, m, runeKey('i'))

	if cmd != nil {
		t.Error("expected no tea.Cmd — sess-1 is already actuating")
	}
	if m.mode != modeNormal {
		t.Errorf("mode = %v, want modeNormal (must not enter inject mode while actuating)", m.mode)
	}
	if !strings.Contains(m.status, "already re-driving") {
		t.Errorf("status = %q, want it to mention the in-flight re-drive", m.status)
	}
}

func TestUpdate_InjectSubmit_SecondSubmitWhileActuating_Refuses(t *testing.T) {
	// exercises the belt-and-suspenders re-check at the actual inject
	// dispatch site (modeInjecting's enter handler), independent of the
	// "i" keypress's own early guard.
	m := modelWithOneLoop()
	m, _ = updateModel(t, m, runeKey('i'))
	if m.mode != modeInjecting {
		t.Fatalf("precondition failed: mode = %v, want modeInjecting", m.mode)
	}
	m.actuating = map[string]bool{"sess-1": true} // force in-flight, as if another dispatch raced in
	for _, r := range "do the thing" {
		m, _ = updateModel(t, m, runeKey(r))
	}

	m, cmd := updateModel(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	if cmd != nil {
		t.Error("expected no tea.Cmd (injectCmd) — sess-1 is already actuating")
	}
	if !strings.Contains(m.status, "already re-driving") {
		t.Errorf("status = %q, want it to mention the in-flight re-drive", m.status)
	}
}

func TestUpdate_ResumeResultMsg_ClearsActuatingGuard(t *testing.T) {
	m := modelWithOneLoop()
	m.actuating = map[string]bool{"sess-1": true}

	m, _ = updateModel(t, m, resumeResultMsg{sessionID: "sess-1", ok: true, text: "resumed aboard"})

	if m.actuating["sess-1"] {
		t.Error("expected sess-1 to be cleared from m.actuating once its result arrives")
	}
}

func TestUpdate_ResumeResultMsg_OnlyClearsMatchingSessionID(t *testing.T) {
	m := modelWithOneLoop()
	m.actuating = map[string]bool{"sess-1": true, "sess-2": true}

	m, _ = updateModel(t, m, resumeResultMsg{sessionID: "sess-1", ok: true, text: "resumed aboard"})

	if m.actuating["sess-1"] {
		t.Error("expected sess-1 to be cleared")
	}
	if !m.actuating["sess-2"] {
		t.Error("expected sess-2 to be UNAFFECTED — only the matching sessionID clears")
	}
}

func TestUpdate_RKey_AfterActuatingCleared_CanDispatchAgain(t *testing.T) {
	m := modelWithOneLoop()
	m.loops[0].State = domain.StateStalled
	m.actuating = map[string]bool{"sess-1": true}

	// clear it, as the real resumeResultMsg handler would once the first
	// send completes.
	m, _ = updateModel(t, m, resumeResultMsg{sessionID: "sess-1", ok: true, text: "resumed aboard"})

	m, cmd := updateModel(t, m, runeKey('r'))

	if cmd == nil {
		t.Error("expected a non-nil tea.Cmd — the guard must not stick around after the result clears it")
	}
	if strings.Contains(m.status, "already re-driving") {
		t.Errorf("status = %q, want a fresh resume attempt, not the in-flight refusal", m.status)
	}
}

// fakeController is a minimal control.Controller test double — only Resume
// is exercised by sendPromptCmd; the rest are unused stubs.
type fakeController struct {
	name             string
	resumeCalled     bool
	resumeErr        error
	lastResumePrompt string // feat/drift-guided-redrive: captures what Resume was actually sent, for asserting hint composition
}

func (f *fakeController) Name() string                               { return f.name }
func (f *fakeController) Available() bool                            { return true }
func (f *fakeController) Locate(string) (control.Target, bool)       { return control.Target{}, false }
func (f *fakeController) LocateClaude(string) (control.Target, bool) { return control.Target{}, false }
func (f *fakeController) Approve(control.Target) error               { return nil }
func (f *fakeController) Focus(control.Target) error                 { return nil }
func (f *fakeController) Interrupt(control.Target) error             { return nil }
func (f *fakeController) Spawn(string, string) error                 { return nil }
func (f *fakeController) Resume(t control.Target, prompt string) error {
	f.resumeCalled = true
	f.lastResumePrompt = prompt
	return f.resumeErr
}

func TestUpdate_ArrowKeysWhileInjecting_RouteToInputNotCursor(t *testing.T) {
	// two loops (distinct dirs so "i" isn't refused as ambiguous) — cursor
	// movement would be observable if the down arrow were (wrongly) still
	// handled by normal navigation while injecting.
	m := New()
	m.loops = []domain.Loop{
		{Project: "a", SessionID: "s1", ProjectDir: "-x-a", State: domain.StateRunning},
		{Project: "b", SessionID: "s2", ProjectDir: "-x-b", State: domain.StateRunning},
	}
	m.cursor = 0
	m, _ = updateModel(t, m, runeKey('i'))
	if m.mode != modeInjecting {
		t.Fatalf("precondition failed: mode = %v, want modeInjecting", m.mode)
	}

	m, _ = updateModel(t, m, tea.KeyMsg{Type: tea.KeyDown})

	if m.cursor != 0 {
		t.Errorf("cursor = %d, want unchanged at 0 (down arrow must route to the text input while injecting)", m.cursor)
	}
}

func TestUpdate_IKey_EmptyPrompt_CancelsWithoutInjecting(t *testing.T) {
	// empty prompt on enter cancels — same convention as the wizard's empty
	// goal (TestUpdate_Enter_EmptyGoal_CancelsWithoutSpawning).
	m := modelWithOneLoop()
	m, _ = updateModel(t, m, runeKey('i'))
	if m.mode != modeInjecting {
		t.Fatalf("precondition failed: mode = %v, want modeInjecting", m.mode)
	}

	m, cmd := updateModel(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	if m.mode != modeNormal {
		t.Errorf("mode = %v, want modeNormal after an empty-prompt enter", m.mode)
	}
	if !strings.Contains(m.status, "empty prompt") {
		t.Errorf("status = %q, want it to mention the empty prompt", m.status)
	}
	if cmd != nil {
		t.Error("expected no tea.Cmd for an empty prompt (inject must not be triggered)")
	}
}

func TestUpdate_IKey_EnterWithText_DispatchesInjectCmd(t *testing.T) {
	m := modelWithOneLoop()
	m, _ = updateModel(t, m, runeKey('i'))

	m, cmd := typeAndEnter(t, m, "run the tests again")

	if m.mode != modeNormal {
		t.Errorf("mode = %v, want modeNormal after submitting the prompt", m.mode)
	}
	if cmd == nil {
		t.Fatal("expected a non-nil tea.Cmd (injectCmd) after typing a prompt and pressing enter")
	}
	if !strings.Contains(m.status, "injecting into aboard") {
		t.Errorf("status = %q, want it to mention injecting into the target", m.status)
	}
}

func TestUpdate_IKey_TargetSnapshottedAtKeypress_SurvivesRescan(t *testing.T) {
	// the injection target is captured at "i" keypress time, NOT re-resolved
	// at submit time — a mid-typing rescan (loopsMsg) that reorders/removes
	// loops must not retarget the pending injection.
	m := modelWithOneLoop() // selects "aboard"/sess-1
	m, _ = updateModel(t, m, runeKey('i'))
	if m.injectTarget.SessionID != "sess-1" {
		t.Fatalf("precondition failed: injectTarget = %q, want sess-1", m.injectTarget.SessionID)
	}

	// fleet rescans mid-typing: "aboard" is gone, a different loop is now at
	// cursor 0.
	m, _ = updateModel(t, m, loopsMsg([]domain.Loop{
		{Project: "other", SessionID: "sess-9", ProjectDir: "-x-other", State: domain.StateRunning},
	}))

	if m.injectTarget.SessionID != "sess-1" {
		t.Errorf("injectTarget.SessionID = %q, want it to STAY the snapshotted sess-1 after a rescan", m.injectTarget.SessionID)
	}
	if m.injectTarget.Project != "aboard" {
		t.Errorf("injectTarget.Project = %q, want the snapshotted %q", m.injectTarget.Project, "aboard")
	}
}

func TestRenderInjectPrompt_RunningTarget_ShowsMidTurnWarning(t *testing.T) {
	// injecting into a StateRunning loop lands mid-turn — the prompt line must
	// surface a plain warning rather than pretend it's risk-free.
	m := modelWithOneLoop() // StateRunning
	m, _ = updateModel(t, m, runeKey('i'))

	out := renderInjectPrompt(m)

	if !strings.Contains(out, "aboard") {
		t.Errorf("rendered inject prompt = %q, want it to name the target loop", out)
	}
	if !strings.Contains(out, "lands mid-turn") {
		t.Errorf("rendered inject prompt = %q, want the mid-turn warning for a running target", out)
	}
}

func TestRenderInjectPrompt_IdleTarget_NoMidTurnWarning(t *testing.T) {
	// a non-running target has no mid-turn footgun — no warning.
	m := New()
	m.loops = []domain.Loop{{Project: "aboard", SessionID: "sess-1", ProjectDir: "-x-aboard", State: domain.StateIdle}}
	m.cursor = 0
	m, _ = updateModel(t, m, runeKey('i'))

	out := renderInjectPrompt(m)

	if strings.Contains(out, "lands mid-turn") {
		t.Errorf("rendered inject prompt = %q, want NO mid-turn warning for an idle target", out)
	}
}

// ── P2-1: RESTART callout reflects Tier 2 redrive ────────────────────

func TestRenderResumeCallout_StallGone_MentionsTier2Redrive(t *testing.T) {
	l := domain.Loop{SessionID: "sess-1", Stall: domain.StallGone}

	out := renderResumeCallout(l, 80, nil, time.Now())

	if !strings.Contains(out, "RESTART") {
		t.Errorf("callout = %q, want the RESTART label", out)
	}
	if !strings.Contains(out, "re-drive headlessly (tier 2)") {
		t.Errorf("callout = %q, want it to mention the tier-2 redrive path (r still works)", out)
	}
}

func TestRenderResumeCallout_OtherStall_KeepsResumeWording(t *testing.T) {
	l := domain.Loop{SessionID: "sess-1", Stall: domain.StallNoOutput}

	out := renderResumeCallout(l, 80, nil, time.Now())

	if !strings.Contains(out, "RESUME") {
		t.Errorf("callout = %q, want the RESUME label for a non-gone stall", out)
	}
	if !strings.Contains(out, "re-send prompt") {
		t.Errorf("callout = %q, want the ordinary re-send wording", out)
	}
}

// ── oracle judge trigger policy ────────────────────────────────────

func TestTriggerJudgments_FiresForBoundIdleLoopNeverJudged(t *testing.T) {
	m := New()
	m.loops = []domain.Loop{
		{SessionID: "s1", State: domain.StateIdle, Cycle: 3, Goal: domain.Goal{Text: "fix the bug"}},
	}

	cmd := m.triggerJudgments()

	if cmd == nil {
		t.Fatal("expected a non-nil batch cmd")
	}
	if !m.judging["s1"] {
		t.Error("expected s1 marked in-flight after triggering")
	}
}

func TestTriggerJudgments_SkipsUnboundLoops(t *testing.T) {
	m := New()
	m.loops = []domain.Loop{{SessionID: "s1", State: domain.StateIdle, Cycle: 1}} // no Goal.Text

	if cmd := m.triggerJudgments(); cmd != nil {
		t.Error("expected nil cmd for an unbound loop")
	}
}

func TestTriggerJudgments_SkipsNonIdleState(t *testing.T) {
	m := New()
	m.loops = []domain.Loop{{SessionID: "s1", State: domain.StateRunning, Cycle: 1, Goal: domain.Goal{Text: "x"}}}

	if cmd := m.triggerJudgments(); cmd != nil {
		t.Error("expected nil cmd for a non-idle loop (not a done-candidate checkpoint yet)")
	}
}

func TestTriggerJudgments_SkipsAlreadyJudgedThisCycle(t *testing.T) {
	m := New()
	m.loops = []domain.Loop{{
		SessionID: "s1", State: domain.StateIdle, Cycle: 3, Goal: domain.Goal{Text: "x"},
		Last: &domain.Verdict{Outcome: domain.OutcomeProgress, AtCycle: 3},
	}}

	if cmd := m.triggerJudgments(); cmd != nil {
		t.Error("expected nil cmd — this exact cycle was already judged")
	}
}

func TestTriggerJudgments_FiresAgainAfterCycleAdvances(t *testing.T) {
	m := New()
	m.loops = []domain.Loop{{
		SessionID: "s1", State: domain.StateIdle, Cycle: 4, Goal: domain.Goal{Text: "x"},
		Last: &domain.Verdict{Outcome: domain.OutcomeProgress, AtCycle: 3},
	}}

	if cmd := m.triggerJudgments(); cmd == nil {
		t.Error("expected a non-nil cmd — cycle advanced past the last judged cycle")
	}
}

func TestTriggerJudgments_InFlightGuardPreventsDoubleJudging(t *testing.T) {
	m := New()
	m.loops = []domain.Loop{{SessionID: "s1", State: domain.StateIdle, Cycle: 1, Goal: domain.Goal{Text: "x"}}}

	if cmd := m.triggerJudgments(); cmd == nil {
		t.Fatal("expected the first call to fire")
	}
	if cmd := m.triggerJudgments(); cmd != nil {
		t.Error("expected the second call (same scan cycle, still in-flight) to fire nothing")
	}
}

func TestUpdate_VerdictMsg_ClearsInFlightGuard(t *testing.T) {
	m := New()
	m.judging = map[string]bool{"s1": true}

	m, _ = updateModel(t, m, verdictMsg{sessionID: "s1", verdict: domain.Verdict{Outcome: domain.OutcomeDone}})

	if m.judging["s1"] {
		t.Error("expected in-flight guard cleared after verdictMsg")
	}
}

func TestJudgeCmd_SavesVerdictAndReportsResult(t *testing.T) {
	dir := t.TempDir()
	origDirFn, origJudgeFn, origHistoryDir := registryDirFn, judgeFn, historyDirFn
	defer func() { registryDirFn, judgeFn, historyDirFn = origDirFn, origJudgeFn, origHistoryDir }()
	registryDirFn = func() string { return dir }
	historyDirFn = func() string { return t.TempDir() }
	judgeFn = func(goal, cwd, lastText, doneWhen, oracleRubric string) (domain.Verdict, error) {
		return domain.Verdict{Outcome: domain.OutcomeRejected, Reason: "no test output shown"}, nil
	}

	if err := registry.Bind(dir, "s1", registry.BindSpec{Goal: "fix the bug"}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	l := domain.Loop{SessionID: "s1", Cycle: 2, Goal: domain.Goal{Text: "fix the bug"}, Cwd: "/x/aboard", Path: "/no/such/file.jsonl"}
	msg := judgeCmd(l)()

	vm, ok := msg.(verdictMsg)
	if !ok {
		t.Fatalf("got %T, want verdictMsg", msg)
	}
	if vm.err != nil {
		t.Fatalf("unexpected err: %v", vm.err)
	}
	if vm.verdict.Outcome != domain.OutcomeRejected {
		t.Errorf("Outcome = %v, want rejected", vm.verdict.Outcome)
	}

	rec, ok := registry.Load(dir, "s1")
	if !ok {
		t.Fatal("expected a record to exist")
	}
	if rec.NoImprove != 1 {
		t.Errorf("NoImprove = %d, want 1 (rejected increments it)", rec.NoImprove)
	}
	if rec.Verdict == nil || rec.Verdict.AtCycle != 2 {
		t.Errorf("Verdict.AtCycle = %+v, want AtCycle=2", rec.Verdict)
	}
}

func TestJudgeCmd_JudgeErrorReportedWithoutSaving(t *testing.T) {
	dir := t.TempDir()
	origDirFn, origJudgeFn := registryDirFn, judgeFn
	defer func() { registryDirFn, judgeFn = origDirFn, origJudgeFn }()
	registryDirFn = func() string { return dir }
	judgeFn = func(goal, cwd, lastText, doneWhen, oracleRubric string) (domain.Verdict, error) {
		return domain.Verdict{}, errTestJudgeFailed
	}

	if err := registry.Bind(dir, "s1", registry.BindSpec{Goal: "goal"}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	l := domain.Loop{SessionID: "s1", Cycle: 1, Goal: domain.Goal{Text: "goal"}}
	msg := judgeCmd(l)()

	vm, ok := msg.(verdictMsg)
	if !ok {
		t.Fatalf("got %T, want verdictMsg", msg)
	}
	if vm.err == nil {
		t.Error("expected a non-nil err")
	}
	if rec, _ := registry.Load(dir, "s1"); rec.Verdict != nil {
		t.Error("expected no verdict saved when judging fails")
	}
}

func TestJudgeCmd_PassesDoneWhenAndOracleFromGoal(t *testing.T) {
	dir := t.TempDir()
	origDirFn, origJudgeFn, origHistoryDir := registryDirFn, judgeFn, historyDirFn
	defer func() { registryDirFn, judgeFn, historyDirFn = origDirFn, origJudgeFn, origHistoryDir }()
	registryDirFn = func() string { return dir }
	historyDirFn = func() string { return t.TempDir() }

	var gotDoneWhen, gotOracle string
	judgeFn = func(goal, cwd, lastText, doneWhen, oracleRubric string) (domain.Verdict, error) {
		gotDoneWhen, gotOracle = doneWhen, oracleRubric
		return domain.Verdict{Outcome: domain.OutcomeProgress}, nil
	}

	spec := registry.BindSpec{Goal: "fix the bug", DoneCondition: "tests pass", Oracle: "run go test ./..."}
	if err := registry.Bind(dir, "s1", spec); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	l := domain.Loop{SessionID: "s1", Cycle: 1, Goal: domain.Goal{Text: "fix the bug", DoneWhen: "tests pass", Oracle: "run go test ./..."}}
	judgeCmd(l)()

	if gotDoneWhen != "tests pass" {
		t.Errorf("doneWhen passed to judgeFn = %q, want %q", gotDoneWhen, "tests pass")
	}
	if gotOracle != "run go test ./..." {
		t.Errorf("oracleRubric passed to judgeFn = %q, want %q", gotOracle, "run go test ./...")
	}
}

// ── ORACLE / N-I label pure funcs ───────────────────────────────────

func TestOracleLabel(t *testing.T) {
	cases := []struct {
		name string
		l    domain.Loop
		want string
	}{
		{"unbound/never judged", domain.Loop{}, "—"},
		{"done", domain.Loop{Last: &domain.Verdict{Outcome: domain.OutcomeDone}}, "✓ verified"},
		{"progress", domain.Loop{Last: &domain.Verdict{Outcome: domain.OutcomeProgress}}, "✓ progress"},
		{"rejected", domain.Loop{Last: &domain.Verdict{Outcome: domain.OutcomeRejected}}, "✗ rejected"},
	}
	for _, c := range cases {
		if got := oracleLabel(c.l); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestNoImproveLabel(t *testing.T) {
	if got := noImproveLabel(domain.Loop{}); got != "—" {
		t.Errorf("unbound: got %q, want %q", got, "—")
	}
	bound := domain.Loop{Goal: domain.Goal{Text: "x", NoImproveLimit: 3}, NoImprove: 1}
	if got := noImproveLabel(bound); got != "1/3" {
		t.Errorf("bound: got %q, want %q", got, "1/3")
	}
}

func TestNoteForRow_GovernorNotePreferredOverStall(t *testing.T) {
	l := domain.Loop{State: domain.StateRunning, Note: "⚠ over budget", Stall: domain.StallNoOutput}
	note, style := noteForRow(l)
	if note != "⚠ over budget" {
		t.Errorf("note = %q, want the governor note, not the stall text", note)
	}
	if style.GetForeground() != cAmber {
		t.Errorf("style foreground = %v, want cAmber for a non-failed governor note", style.GetForeground())
	}
}

func TestNoteForRow_GovernorNote_FailedStateIsRed(t *testing.T) {
	l := domain.Loop{State: domain.StateFailed, Note: "stopped: no improvement 3/3"}
	note, style := noteForRow(l)
	if note != "stopped: no improvement 3/3" {
		t.Errorf("note = %q, want the governor's stop note", note)
	}
	if style.GetForeground() != cRed {
		t.Errorf("style foreground = %v, want cRed for a StateFailed governor note", style.GetForeground())
	}
}

func TestNoteForRow_NoGovernorNote_FallsBackToStallText(t *testing.T) {
	l := domain.Loop{State: domain.StateStalled, Stall: domain.StallNoOutput}
	note, _ := noteForRow(l)
	if note != "⚠ no output" {
		t.Errorf("note = %q, want the stall-derived text", note)
	}
}

func TestNoteForRow_NoGovernorNote_FallsBackToDriftReason(t *testing.T) {
	l := domain.Loop{State: domain.StateDrift, Last: &domain.Verdict{Outcome: domain.OutcomeRejected, Reason: "no evidence shown"}}
	note, _ := noteForRow(l)
	if note != "✗ no evidence shown" {
		t.Errorf("note = %q, want the drift reason", note)
	}
}

func TestNoteForRow_NeitherGovernorNorStallNorDrift_Empty(t *testing.T) {
	note, _ := noteForRow(domain.Loop{State: domain.StateRunning})
	if note != "" {
		t.Errorf("note = %q, want empty", note)
	}
}

// ── detail pane TAIL row (wrapTailText / detailRowMultiline) ────────

func TestWrapTailText_WrapsToExpectedLineCount(t *testing.T) {
	// three 5-col lines, all under tailMaxLines → returned verbatim, no marker.
	got := wrapTailText("aa bb cc dd ee ff", 5, tailMaxLines)
	want := []string{"aa bb", "cc dd", "ee ff"}
	if len(got) != len(want) {
		t.Fatalf("got %d lines %q, want %d %q", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestWrapTailText_CapsAtMaxLinesWithMarker(t *testing.T) {
	// "one two ... ten" @ width 12 wraps to 5 lines; capping at 3 keeps 3 and
	// marks the last as truncated so it's clear content was dropped.
	got := wrapTailText("one two three four five six seven eight nine ten", 12, 3)
	if len(got) != 3 {
		t.Fatalf("got %d lines %q, want 3 (capped)", len(got), got)
	}
	if !strings.HasSuffix(got[2], "…") {
		t.Errorf("last shown line %q lacks the truncation marker", got[2])
	}
	if n := len([]rune(got[2])); n > 12 {
		t.Errorf("last line = %d runes, want <= width 12", n)
	}
}

func TestWrapTailText_FullWidthLastLineMarkerStaysWithinWidth(t *testing.T) {
	// each word is exactly the width; the marker must displace a rune rather
	// than overflow the column.
	got := wrapTailText("aaaaa bbbbb ccccc ddddd", 5, 2)
	if len(got) != 2 {
		t.Fatalf("got %d lines %q, want 2", len(got), got)
	}
	if got[1] != "bbbb…" {
		t.Errorf("last line = %q, want %q (last rune dropped for the marker)", got[1], "bbbb…")
	}
	if n := len([]rune(got[1])); n != 5 {
		t.Errorf("last line = %d runes, want exactly width 5 (no overflow)", n)
	}
}

func TestWrapTailText_ShortTextNoMarkerNoBlanks(t *testing.T) {
	got := wrapTailText("short text", 40, tailMaxLines)
	if len(got) != 1 {
		t.Fatalf("got %d lines %q, want 1", len(got), got)
	}
	if got[0] != "short text" {
		t.Errorf("got %q, want %q", got[0], "short text")
	}
	if strings.HasSuffix(got[0], "…") {
		t.Errorf("short text must not be marked truncated: %q", got[0])
	}
	for i, line := range got {
		if line == "" {
			t.Errorf("line %d is a wasted blank line", i)
		}
	}
}

func TestWrapTailText_NonPositiveArgsReturnNil(t *testing.T) {
	if got := wrapTailText("anything", 0, tailMaxLines); got != nil {
		t.Errorf("width 0: got %q, want nil", got)
	}
	if got := wrapTailText("anything", 40, 0); got != nil {
		t.Errorf("maxLines 0: got %q, want nil", got)
	}
}

func TestDetailRowMultiline_KeyOnFirstLineContinuationIndented(t *testing.T) {
	out := detailRowMultiline("TAIL", []string{"alpha", "beta", "gamma"})
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d physical lines %q, want 3", len(lines), lines)
	}
	if !strings.Contains(lines[0], "TAIL") || !strings.HasSuffix(lines[0], "alpha") {
		t.Errorf("first line %q should carry the KEY then the first value line", lines[0])
	}
	indent := strings.Repeat(" ", detailKeyWidth)
	for i, line := range lines[1:] {
		if !strings.HasPrefix(line, indent) {
			t.Errorf("continuation line %d = %q, want a %d-space indent under the value column", i+1, line, detailKeyWidth)
		}
		if strings.Contains(line, "TAIL") {
			t.Errorf("continuation line %d = %q should not repeat the KEY", i+1, line)
		}
		if strings.TrimLeft(line, " ") == "" {
			t.Errorf("continuation line %d = %q is blank", i+1, line)
		}
	}
}

func TestDetailRowMultiline_EmptyLinesRendersNothing(t *testing.T) {
	if out := detailRowMultiline("TAIL", nil); out != "" {
		t.Errorf("got %q, want empty for no value lines", out)
	}
}

func TestRenderDetail_EmptyLastText_NoTailRow(t *testing.T) {
	l := domain.Loop{Project: "aboard", SessionID: "s1", State: domain.StateIdle, Cwd: "/x", Path: "/x/s1.jsonl"}
	out := renderDetail(l, 80, 40, detailData{now: time.Now()})
	if strings.Contains(out, "TAIL") {
		t.Errorf("detail pane should have NO TAIL row when LastText is empty:\n%s", out)
	}
}

func TestRenderDetail_LongLastText_ShowsWrappedTruncatedTailRow(t *testing.T) {
	// long enough to overflow tailMaxLines at the pane width → wrapped + marked.
	l := domain.Loop{
		Project:   "aboard",
		SessionID: "s1",
		State:     domain.StateIdle,
		Cwd:       "/x",
		Path:      "/x/s1.jsonl",
		LastText:  strings.Repeat("lorem ipsum dolor sit amet ", 60),
	}
	out := renderDetail(l, 80, 40, detailData{now: time.Now()})
	if !strings.Contains(out, "TAIL") {
		t.Errorf("detail pane should show a TAIL row when LastText is present:\n%s", out)
	}
	if !strings.Contains(out, "…") {
		t.Errorf("an overflowing TAIL should carry a truncation marker:\n%s", out)
	}
}

// ── feat/detail-panel-v2 ──────────────────────────────────────────────────

// ── burn rate / ETA math ──────────────────────────────────────────────────

func TestBudgetBurnRateSuffix_UnboundLoop_Omitted(t *testing.T) {
	l := domain.Loop{Cycle: 5, TokensSpent: 100, Goal: domain.Goal{BudgetTokens: 1000}} // no Goal.Text — unbound
	if got := budgetBurnRateSuffix(l); got != "" {
		t.Errorf("got %q, want empty (unbound loop)", got)
	}
}

func TestBudgetBurnRateSuffix_CycleBelowTwo_Omitted(t *testing.T) {
	l := domain.Loop{Goal: domain.Goal{Text: "g", BudgetTokens: 1000}, Cycle: 1, TokensSpent: 100}
	if got := budgetBurnRateSuffix(l); got != "" {
		t.Errorf("got %q, want empty (cycle < 2)", got)
	}
}

func TestBudgetBurnRateSuffix_AlreadyOverBudget_Omitted(t *testing.T) {
	l := domain.Loop{Goal: domain.Goal{Text: "g", BudgetTokens: 1000}, Cycle: 5, TokensSpent: 1500}
	if got := budgetBurnRateSuffix(l); got != "" {
		t.Errorf("got %q, want empty (already over budget, no future ETA)", got)
	}
}

func TestBudgetBurnRateSuffix_ComputesRateAndETACycle(t *testing.T) {
	// rate = 1,800,000/6 = 300,000/cyc; remaining = 200,000; cyclesLeft =
	// round(200,000/300,000) = 1; etaCycle = 6+1 = 7.
	l := domain.Loop{Goal: domain.Goal{Text: "g", BudgetTokens: 2_000_000}, Cycle: 6, TokensSpent: 1_800_000}
	got := budgetBurnRateSuffix(l)
	if !strings.Contains(got, "300k/cyc") {
		t.Errorf("got %q, want it to mention the ~300k/cyc rate", got)
	}
	if !strings.Contains(got, "cap ~c7") {
		t.Errorf("got %q, want it to mention the ETA cycle ~c7", got)
	}
}

func TestBudgetLine_UnboundLoop_NoSuffixButBaseStillRenders(t *testing.T) {
	l := domain.Loop{Goal: domain.Goal{BudgetTokens: 1000}, TokensSpent: 500}
	got := budgetLine(l)
	if !strings.Contains(got, "500") {
		t.Errorf("got %q, want the base spent/cap text present regardless of the suffix", got)
	}
	if strings.Contains(got, "/cyc") {
		t.Errorf("got %q, want no burn-rate suffix for an unbound loop", got)
	}
}

// ── STAGE row ──────────────────────────────────────────────────────────────

func TestStageElapsed_PrefersBoundAt(t *testing.T) {
	now := time.Now()
	l := domain.Loop{BoundAt: now.Add(-90 * time.Second)}
	got, ok := stageElapsed(l, detailData{now: now})
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got != 90*time.Second {
		t.Errorf("got %v, want 90s", got)
	}
}

func TestStageElapsed_FallsBackToFirstEventTS(t *testing.T) {
	now := time.Now()
	evs := []events.Event{
		{TS: now.Add(-5 * time.Minute).UnixNano(), SessionID: "s1", ToState: "running"},
		{TS: now.Add(-3 * time.Minute).UnixNano(), SessionID: "s1", ToState: "idle"},
	}
	got, ok := stageElapsed(domain.Loop{}, detailData{now: now, events: evs})
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got != 5*time.Minute {
		t.Errorf("got %v, want 5m (from the FIRST/oldest event)", got)
	}
}

func TestStageElapsed_NeitherSource_Omitted(t *testing.T) {
	if _, ok := stageElapsed(domain.Loop{}, detailData{now: time.Now()}); ok {
		t.Error("expected ok=false — no BoundAt and no event history at all")
	}
}

func TestRenderStageRow_OmittedWithoutElapsedSource(t *testing.T) {
	if _, ok := renderStageRow(domain.Loop{Cycle: 3}, detailData{now: time.Now()}); ok {
		t.Error("expected ok=false — STAGE has nothing to compute elapsed from")
	}
}

func TestRenderStageRow_GitSegmentOmittedWhenNotOK(t *testing.T) {
	l := domain.Loop{Cycle: 3, BoundAt: time.Now().Add(-time.Minute)}
	got, ok := renderStageRow(l, detailData{now: time.Now(), git: gitStatsResult{ok: false}})
	if !ok {
		t.Fatal("expected ok=true (elapsed is computable)")
	}
	if strings.Contains(got, "file") {
		t.Errorf("got %q, want no file/± segment when git stats aren't ok", got)
	}
}

func TestRenderStageRow_IncludesGitSegmentWhenOK(t *testing.T) {
	l := domain.Loop{Cycle: 3, BoundAt: time.Now().Add(-time.Minute)}
	got, ok := renderStageRow(l, detailData{now: time.Now(), git: gitStatsResult{files: 2, plus: 47, minus: 9, ok: true}})
	if !ok {
		t.Fatal("expected ok=true")
	}
	if !strings.Contains(got, "2 files +47 −9") {
		t.Errorf("got %q, want the git file/± segment", got)
	}
}

func TestRenderDetail_StageRowAbsentForUnboundLoop(t *testing.T) {
	l := domain.Loop{Project: "aboard", SessionID: "s1", State: domain.StateIdle, BoundAt: time.Now().Add(-time.Minute)}
	out := renderDetail(l, 80, 40, detailData{now: time.Now()})
	if strings.Contains(out, "STAGE") {
		t.Errorf("STAGE must not render for an unbound loop even with a valid BoundAt:\n%s", out)
	}
}

func TestRenderDetail_StageRowPresentForBoundLoop(t *testing.T) {
	l := domain.Loop{Project: "aboard", SessionID: "s1", State: domain.StateIdle, Cycle: 3,
		Goal: domain.Goal{Text: "fix it", MaxCycles: 12}, BoundAt: time.Now().Add(-4 * time.Minute)}
	out := renderDetail(l, 80, 40, detailData{now: time.Now()})
	if !strings.Contains(out, "STAGE") {
		t.Errorf("STAGE should render for a bound loop with a valid elapsed source:\n%s", out)
	}
}

// ── LAST ERROR extraction + staleness ───────────────────────────────────────

func TestIsErrorStale_ErrorBeforeRecovery_Stale(t *testing.T) {
	now := time.Now()
	errTS := now.Add(-10 * time.Minute)
	evs := []events.Event{
		{TS: now.Add(-9 * time.Minute).UnixNano(), Trigger: events.TriggerScan, ToState: "running"}, // recovered AFTER the error
	}
	if !isErrorStale(errTS, evs) {
		t.Error("expected the error to be stale — the loop recovered after it")
	}
}

func TestIsErrorStale_ErrorAfterRecovery_NotStale(t *testing.T) {
	now := time.Now()
	errTS := now.Add(-1 * time.Minute)
	evs := []events.Event{
		{TS: now.Add(-9 * time.Minute).UnixNano(), Trigger: events.TriggerScan, ToState: "running"}, // recovery predates the error
	}
	if isErrorStale(errTS, evs) {
		t.Error("expected the error to be current — it happened AFTER the last recovery")
	}
}

func TestIsErrorStale_NoRecoveryEventAtAll_NotStale(t *testing.T) {
	if isErrorStale(time.Now(), nil) {
		t.Error("expected not stale — nothing to compare against, so don't suppress")
	}
}

// TestIsErrorStale_ZeroTimestamp_FailsOpen is the P2 review fix's
// regression: an unparseable transcript timestamp (claude.LastError /
// entryTimestamp return the zero time.Time) must NOT be treated as
// "infinitely old" — that would silently suppress a possibly-LIVE error
// any time there's ANY healthy transition on record, with no visible
// symptom other than "LAST ERROR never shows up". Fail open: show it.
func TestIsErrorStale_ZeroTimestamp_FailsOpen(t *testing.T) {
	now := time.Now()
	evs := []events.Event{
		{TS: now.Add(-time.Minute).UnixNano(), Trigger: events.TriggerScan, ToState: "running"},
	}
	if isErrorStale(time.Time{}, evs) {
		t.Error("expected NOT stale for a zero errorTS, even with a healthy transition on record — must fail open")
	}
}

func TestIsErrorStale_IdleAlsoCountsAsHealthy(t *testing.T) {
	now := time.Now()
	errTS := now.Add(-10 * time.Minute)
	evs := []events.Event{
		{TS: now.Add(-9 * time.Minute).UnixNano(), Trigger: events.TriggerScan, ToState: "idle"},
	}
	if !isErrorStale(errTS, evs) {
		t.Error("expected stale — idle counts as a healthy recovery state too")
	}
}

func TestRenderDetail_LastErrorBlock_ShownWhenCurrent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s1.jsonl")
	content := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"API Error: 429 rate limited"}]},"timestamp":"` + time.Now().Format(time.RFC3339) + `"}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	l := domain.Loop{Project: "aboard", SessionID: "s1", State: domain.StateStalled, Stall: domain.StallRateLimit, Path: path}
	out := renderDetail(l, 80, 40, detailData{now: time.Now()})
	if !strings.Contains(out, "LAST ERROR") {
		t.Errorf("expected a LAST ERROR block:\n%s", out)
	}
	if !strings.Contains(out, "API Error: 429 rate limited") {
		t.Errorf("expected the VERBATIM error text:\n%s", out)
	}
}

func TestRenderDetail_LastErrorBlock_SuppressedWhenStale(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s1.jsonl")
	oldTS := time.Now().Add(-time.Hour)
	content := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"API Error: 429 rate limited"}]},"timestamp":"` + oldTS.Format(time.RFC3339) + `"}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	l := domain.Loop{Project: "aboard", SessionID: "s1", State: domain.StateIdle, Path: path}
	evs := []events.Event{
		{TS: time.Now().Add(-30 * time.Minute).UnixNano(), Trigger: events.TriggerScan, ToState: "idle"}, // recovered since the error
	}
	out := renderDetail(l, 80, 40, detailData{now: time.Now(), events: evs})
	if strings.Contains(out, "LAST ERROR") {
		t.Errorf("expected NO LAST ERROR block — the loop recovered since this error:\n%s", out)
	}
}

// TestRenderDetail_HealthyConversationMentioningStatusCode_NoBlock is
// fix/last-error-false-positive's end-to-end regression, at the SAME
// renderDetail level an operator actually sees: a healthy loop whose
// transcript's last assistant message is ordinary conversation that
// happens to mention "429" (e.g. discussing this very repo's own "429
// auto-redrive" feature by name) must NOT show a LAST ERROR block — live-
// reproduced against this repo's own real transcript before the fix.
func TestRenderDetail_HealthyConversationMentioningStatusCode_NoBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s1.jsonl")
	content := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"landed #24 (429 auto-redrive) — Tier 2 only, opt-in. main is green."}]},"timestamp":"` + time.Now().Format(time.RFC3339) + `"}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	l := domain.Loop{Project: "aboard", SessionID: "s1", State: domain.StateRunning, Path: path}
	out := renderDetail(l, 80, 40, detailData{now: time.Now()})
	if strings.Contains(out, "LAST ERROR") {
		t.Errorf("expected NO LAST ERROR block — this is ordinary conversation mentioning a status code, not a real error:\n%s", out)
	}
}

func TestRenderDetail_NoErrorAtAll_NoBlock(t *testing.T) {
	l := domain.Loop{Project: "aboard", SessionID: "s1", State: domain.StateIdle, Path: "/no/such/file.jsonl"}
	out := renderDetail(l, 80, 40, detailData{now: time.Now()})
	if strings.Contains(out, "LAST ERROR") {
		t.Errorf("expected no LAST ERROR block when there's no transcript error:\n%s", out)
	}
}

// ── VERDICTS block ───────────────────────────────────────────────────────

func TestRenderVerdictsBlock_NoOracleEvents_Empty(t *testing.T) {
	if got := renderVerdictsBlock(nil, 80); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestRenderVerdictsBlock_ShowsNewestThreeInDescendingOrder(t *testing.T) {
	now := time.Now()
	evs := []events.Event{
		{TS: now.Add(-4 * time.Minute).UnixNano(), Trigger: events.TriggerOracle, Detail: "progress at cycle 1: first"},
		{TS: now.Add(-3 * time.Minute).UnixNano(), Trigger: events.TriggerOracle, Detail: "progress at cycle 2: second"},
		{TS: now.Add(-2 * time.Minute).UnixNano(), Trigger: events.TriggerOracle, Detail: "rejected at cycle 3: third"},
		{TS: now.Add(-1 * time.Minute).UnixNano(), Trigger: events.TriggerOracle, Detail: "done at cycle 4: fourth"},
	}
	got := renderVerdictsBlock(evs, 80)
	if strings.Contains(got, "\"first\"") {
		t.Errorf("got %q, want only the newest 3 (the oldest, \"first\", must be excluded)", got)
	}
	newestIdx := strings.Index(got, "\"fourth\"")
	middleIdx := strings.Index(got, "\"third\"")
	oldestIdx := strings.Index(got, "\"second\"")
	if newestIdx == -1 || middleIdx == -1 || oldestIdx == -1 {
		t.Fatalf("got %q, want second/third/fourth all present", got)
	}
	if !(newestIdx < middleIdx && middleIdx < oldestIdx) {
		t.Errorf("got %q, want newest-first ordering", got)
	}
	if !strings.Contains(got, "VERDICTS (4)") {
		t.Errorf("got %q, want the VERDICTS(4) header — the TOTAL oracle event count, not just the 3 shown", got)
	}
}

func TestRenderVerdictsBlock_DoneShowsCheckmark_RejectedShowsCross(t *testing.T) {
	evs := []events.Event{
		{TS: 1, Trigger: events.TriggerOracle, Detail: "done at cycle 1: ok"},
		{TS: 2, Trigger: events.TriggerOracle, Detail: "rejected at cycle 2: not ok"},
	}
	got := renderVerdictsBlock(evs, 80)
	if !strings.Contains(got, "✓") {
		t.Errorf("got %q, want a ✓ for the done verdict", got)
	}
	if !strings.Contains(got, "✗") {
		t.Errorf("got %q, want a ✗ for the rejected verdict", got)
	}
}

func TestRenderVerdictsBlock_ReasonRenderedVerbatim(t *testing.T) {
	evs := []events.Event{
		{TS: 1, Trigger: events.TriggerOracle, Detail: `done at cycle 1: the exact, unparaphrased reason text`},
	}
	got := renderVerdictsBlock(evs, 200)
	if !strings.Contains(got, `"the exact, unparaphrased reason text"`) {
		t.Errorf("got %q, want the verbatim reason quoted", got)
	}
}

func TestRenderDetail_VerdictsBlockAbsentForUnboundLoop(t *testing.T) {
	l := domain.Loop{Project: "aboard", SessionID: "s1", State: domain.StateIdle}
	evs := []events.Event{{TS: 1, Trigger: events.TriggerOracle, Detail: "done at cycle 1: ok"}}
	out := renderDetail(l, 80, 40, detailData{now: time.Now(), events: evs})
	if strings.Contains(out, "VERDICTS") {
		t.Errorf("VERDICTS must not render for an unbound loop:\n%s", out)
	}
}

// ── EVENTS block: height budgeting + actor glyphs ───────────────────────────

func TestEventActorGlyph(t *testing.T) {
	cases := []struct {
		actor events.Actor
		want  string
	}{
		{events.ActorHuman, "☺ "},
		{events.ActorAuto, "⎇ "},
		{events.ActorSystem, "  "},
	}
	for _, c := range cases {
		if got := eventActorGlyph(c.actor); got != c.want {
			t.Errorf("eventActorGlyph(%v) = %q, want %q", c.actor, got, c.want)
		}
	}
}

func TestRenderEventsBlock_BelowMinRows_Empty(t *testing.T) {
	evs := []events.Event{{TS: 1, Trigger: events.TriggerScan, ToState: "running"}}
	if got := renderEventsBlock(evs, 80, eventsMinRows-1); got != "" {
		t.Errorf("got %q, want empty below eventsMinRows", got)
	}
}

func TestRenderEventsBlock_NoEvents_Empty(t *testing.T) {
	if got := renderEventsBlock(nil, 80, 10); got != "" {
		t.Errorf("got %q, want empty with no history at all", got)
	}
}

func TestRenderEventsBlock_FillsExactlyMaxRows(t *testing.T) {
	var evs []events.Event
	for i := 0; i < 20; i++ {
		evs = append(evs, events.Event{TS: int64(i), Trigger: events.TriggerScan, FromState: "running", ToState: "idle"})
	}
	for _, maxRows := range []int{eventsMinRows, 5, 10} {
		got := renderEventsBlock(evs, 80, maxRows)
		lines := strings.Split(got, "\n")
		if len(lines) != maxRows {
			t.Errorf("maxRows=%d: got %d lines, want exactly %d", maxRows, len(lines), maxRows)
		}
	}
}

func TestRenderEventsBlock_NewestFirst_NeverCoalesced(t *testing.T) {
	// Three identical stalled->running->stalled flaps must all render as
	// separate lines — "flapping IS the signal" (never coalesced) — even
	// though every transition is the identical running<->stalled pair.
	now := time.Now()
	evs := []events.Event{
		{TS: now.Add(-3 * time.Minute).UnixNano(), Trigger: events.TriggerScan, FromState: "running", ToState: "stalled:no-output", Detail: "first"},
		{TS: now.Add(-2 * time.Minute).UnixNano(), Trigger: events.TriggerScan, FromState: "stalled:no-output", ToState: "running", Detail: "second"},
		{TS: now.Add(-1 * time.Minute).UnixNano(), Trigger: events.TriggerScan, FromState: "running", ToState: "stalled:no-output", Detail: "third"},
	}
	got := renderEventsBlock(evs, 80, 10)
	if strings.Count(got, "→") != 3 {
		t.Errorf("got %q, want all 3 transitions rendered separately (not coalesced)", got)
	}
	firstIdx := strings.Index(got, "first")
	secondIdx := strings.Index(got, "second")
	thirdIdx := strings.Index(got, "third")
	if firstIdx == -1 || secondIdx == -1 || thirdIdx == -1 {
		t.Fatalf("got %q, want all three distinct events present", got)
	}
	if !(thirdIdx < secondIdx && secondIdx < firstIdx) {
		t.Errorf("got %q, want newest-first ordering (third, then second, then first)", got)
	}
}

func TestRenderEventsBlock_ActuationEventShowsDetailVerbatim(t *testing.T) {
	evs := []events.Event{
		{TS: 1, Trigger: events.TriggerActuation, Detail: "kill tier1 ok", Actor: events.ActorHuman},
	}
	got := renderEventsBlock(evs, 80, 10)
	if !strings.Contains(got, "☺") {
		t.Errorf("got %q, want the human actor glyph", got)
	}
	if !strings.Contains(got, "kill tier1 ok") {
		t.Errorf("got %q, want the actuation detail verbatim", got)
	}
}

func TestRenderDetail_EventsBlockAbsentWhenTooLittleHeight(t *testing.T) {
	l := domain.Loop{Project: "aboard", SessionID: "s1", State: domain.StateIdle}
	evs := []events.Event{
		{TS: 1, Trigger: events.TriggerScan, FromState: "running", ToState: "idle"},
	}
	// A very small height budget must simply omit EVENTS, not error/panic.
	out := renderDetail(l, 80, 6, detailData{now: time.Now(), events: evs})
	if strings.Contains(out, "EVENTS") {
		t.Errorf("EVENTS should be omitted at a too-small height budget:\n%s", out)
	}
}

func TestRenderDetail_EventsBlockPresentWithEnoughHeight(t *testing.T) {
	l := domain.Loop{Project: "aboard", SessionID: "s1", State: domain.StateIdle}
	var evs []events.Event
	for i := 0; i < 10; i++ {
		evs = append(evs, events.Event{TS: int64(i), Trigger: events.TriggerScan, FromState: "running", ToState: "idle"})
	}
	out := renderDetail(l, 80, 60, detailData{now: time.Now(), events: evs})
	if !strings.Contains(out, "EVENTS") {
		t.Errorf("expected an EVENTS block with a generous height budget:\n%s", out)
	}
}

// ── flap counter ─────────────────────────────────────────────────────────

func TestOrdinal(t *testing.T) {
	cases := map[int]string{1: "1st", 2: "2nd", 3: "3rd", 4: "4th", 11: "11th", 12: "12th", 13: "13th", 21: "21st", 22: "22nd", 23: "23rd", 111: "111th"}
	for n, want := range cases {
		if got := ordinal(n); got != want {
			t.Errorf("ordinal(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestFlapCounter_SingleStall_NotFlagged(t *testing.T) {
	now := time.Now()
	evs := []events.Event{
		{TS: now.Add(-10 * time.Minute).UnixNano(), Trigger: events.TriggerScan, ToState: "stalled:no-output"},
	}
	if _, _, ok := flapCounter(evs, now); ok {
		t.Error("expected ok=false — a single stall isn't a flap")
	}
}

func TestFlapCounter_ThreeStallsWithinHour_Flagged(t *testing.T) {
	now := time.Now()
	evs := []events.Event{
		{TS: now.Add(-20 * time.Minute).UnixNano(), Trigger: events.TriggerScan, ToState: "stalled:no-output"},
		{TS: now.Add(-15 * time.Minute).UnixNano(), Trigger: events.TriggerScan, ToState: "stalled:rate-limit"},
		{TS: now.Add(-5 * time.Minute).UnixNano(), Trigger: events.TriggerScan, ToState: "stalled:gone"},
	}
	count, span, ok := flapCounter(evs, now)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if count != 3 {
		t.Errorf("count = %d, want 3", count)
	}
	if span != 20*time.Minute {
		t.Errorf("span = %v, want 20m (from the earliest counted stall)", span)
	}
}

func TestFlapCounter_StallOutsideWindow_Ignored(t *testing.T) {
	now := time.Now()
	evs := []events.Event{
		{TS: now.Add(-2 * time.Hour).UnixNano(), Trigger: events.TriggerScan, ToState: "stalled:no-output"}, // outside the 1h window
		{TS: now.Add(-5 * time.Minute).UnixNano(), Trigger: events.TriggerScan, ToState: "stalled:gone"},
	}
	if _, _, ok := flapCounter(evs, now); ok {
		t.Error("expected ok=false — only 1 stall within the window")
	}
}

func TestRenderResumeCallout_FlapCounterAppendedWhenFlapping(t *testing.T) {
	now := time.Now()
	l := domain.Loop{SessionID: "sess-1", Stall: domain.StallNoOutput}
	evs := []events.Event{
		{TS: now.Add(-20 * time.Minute).UnixNano(), Trigger: events.TriggerScan, ToState: "stalled:no-output"},
		{TS: now.Add(-15 * time.Minute).UnixNano(), Trigger: events.TriggerScan, ToState: "stalled:rate-limit"},
		{TS: now.Add(-5 * time.Minute).UnixNano(), Trigger: events.TriggerScan, ToState: "stalled:no-output"},
	}
	out := renderResumeCallout(l, 80, evs, now)
	if !strings.Contains(out, "3rd stall in 20:00") {
		t.Errorf("got %q, want the flap counter annotation (formatUptime's mm:ss form)", out)
	}
}

func TestRenderResumeCallout_NoFlap_NoAnnotation(t *testing.T) {
	l := domain.Loop{SessionID: "sess-1", Stall: domain.StallNoOutput}
	out := renderResumeCallout(l, 80, nil, time.Now())
	if strings.Contains(out, "stall in") {
		t.Errorf("got %q, want no flap annotation with no flap history", out)
	}
}

// errTestJudgeFailed is a sentinel error for TestJudgeCmd_JudgeErrorReportedWithoutSaving.
var errTestJudgeFailed = &testJudgeError{}

type testJudgeError struct{}

func (*testJudgeError) Error() string { return "test judge failure" }

// ── "/" filter ───────────────────────────────────────────────────

func TestMatchesFilter(t *testing.T) {
	l := domain.Loop{Project: "aboard", SessionID: "sess-asre1234", State: domain.StateStalled, Stall: domain.StallRateLimit}
	cases := []struct {
		name  string
		query string
		want  bool
	}{
		{"empty query matches everything", "", true},
		{"project, case-insensitive", "ABOARD", true},
		{"session id substring", "asre", true},
		{"state label substring", "429", true}, // stateLabel is "✗ 429"
		{"stall kind substring", "rate limited", true},
		{"no match", "nomatch", false},
	}
	for _, c := range cases {
		if got := matchesFilter(l, c.query); got != c.want {
			t.Errorf("%s: matchesFilter(%q) = %v, want %v", c.name, c.query, got, c.want)
		}
	}
}

func modelWithTwoLoops() Model {
	m := New()
	m.loops = []domain.Loop{
		{Project: "aboard", SessionID: "sess-1", State: domain.StateRunning},
		{Project: "asre", SessionID: "sess-2", State: domain.StateIdle},
	}
	m.cursor = 0
	return m
}

func TestUpdate_SlashKey_EntersFilteringMode(t *testing.T) {
	m := modelWithTwoLoops()

	m, cmd := updateModel(t, m, runeKey('/'))

	if m.mode != modeFiltering {
		t.Fatalf("mode = %v, want modeFiltering", m.mode)
	}
	if !m.input.Focused() {
		t.Error("expected the text input to be focused")
	}
	if cmd == nil {
		t.Error("expected a non-nil cmd (textinput.Blink)")
	}
}

func TestVisibleLoops_FiltersLiveWhileTyping(t *testing.T) {
	m := modelWithTwoLoops()
	m, _ = updateModel(t, m, runeKey('/'))

	for _, r := range "asre" {
		m, _ = updateModel(t, m, runeKey(r))
	}

	visible := m.visibleLoops()
	if len(visible) != 1 || visible[0].Project != "asre" {
		t.Errorf("got %+v, want only the \"asre\" loop (live-filtered while typing, before enter)", visible)
	}
}

func TestUpdate_FilterEnter_AppliesAndExitsToNormalMode(t *testing.T) {
	m := modelWithTwoLoops()
	m, _ = updateModel(t, m, runeKey('/'))
	for _, r := range "asre" {
		m, _ = updateModel(t, m, runeKey(r))
	}

	m, _ = updateModel(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	if m.mode != modeNormal {
		t.Errorf("mode = %v, want modeNormal after enter", m.mode)
	}
	if m.filterQuery != "asre" {
		t.Errorf("filterQuery = %q, want %q", m.filterQuery, "asre")
	}
	visible := m.visibleLoops()
	if len(visible) != 1 || visible[0].Project != "asre" {
		t.Errorf("got %+v, want the filter to stay applied after enter", visible)
	}
}

func TestUpdate_FilterEsc_ClearsAndExits(t *testing.T) {
	m := modelWithTwoLoops()
	m, _ = updateModel(t, m, runeKey('/'))
	for _, r := range "asre" {
		m, _ = updateModel(t, m, runeKey(r))
	}

	m, _ = updateModel(t, m, tea.KeyMsg{Type: tea.KeyEsc})

	if m.mode != modeNormal {
		t.Errorf("mode = %v, want modeNormal after esc", m.mode)
	}
	if m.filterQuery != "" {
		t.Errorf("filterQuery = %q, want empty (esc clears, doesn't apply)", m.filterQuery)
	}
	if len(m.visibleLoops()) != len(m.loops) {
		t.Error("expected all loops visible again after esc clears the filter")
	}
}

func TestUpdate_EscInNormalMode_ClearsAppliedFilter(t *testing.T) {
	m := modelWithTwoLoops()
	m, _ = updateModel(t, m, runeKey('/'))
	for _, r := range "asre" {
		m, _ = updateModel(t, m, runeKey(r))
	}
	m, _ = updateModel(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.filterQuery != "asre" {
		t.Fatalf("precondition failed: filterQuery = %q, want applied", m.filterQuery)
	}

	m, _ = updateModel(t, m, tea.KeyMsg{Type: tea.KeyEsc})

	if m.filterQuery != "" {
		t.Errorf("filterQuery = %q, want cleared by esc in normal mode", m.filterQuery)
	}
	if len(m.visibleLoops()) != len(m.loops) {
		t.Error("expected all loops visible again")
	}
}

func TestUpdate_CursorClampsToFilteredList(t *testing.T) {
	m := New()
	m.loops = []domain.Loop{
		{Project: "aboard", SessionID: "sess-1", State: domain.StateRunning},
		{Project: "aboard", SessionID: "sess-2", State: domain.StateRunning},
		{Project: "asre", SessionID: "sess-3", State: domain.StateIdle},
	}
	m.cursor = 1 // second "aboard" loop

	m, _ = updateModel(t, m, runeKey('/'))
	for _, r := range "asre" {
		m, _ = updateModel(t, m, runeKey(r))
	}

	// only one loop matches "asre" (index 0 of the filtered list) — cursor
	// must clamp down from 1, it can't stay pointing past the filtered set.
	if m.cursor != 0 {
		t.Errorf("cursor = %d, want clamped to 0 (only 1 loop matches)", m.cursor)
	}
	sel, ok := m.selected()
	if !ok || sel.Project != "asre" {
		t.Errorf("selected() = %+v, ok=%v, want the \"asre\" loop", sel, ok)
	}
}

func TestUpdate_ActionsOperateOnFilteredSelection(t *testing.T) {
	// r/a/k/p/enter/o all go through m.selected(), which now goes through
	// visibleLoops() — verify selected() picks the right loop once filtered,
	// not the raw m.loops[cursor].
	m := New()
	m.loops = []domain.Loop{
		{Project: "aboard", SessionID: "sess-1", State: domain.StateStalled},
		{Project: "asre", SessionID: "sess-2", State: domain.StateStalled},
	}
	m.filterQuery = "asre"
	m.cursor = 0

	sel, ok := m.selected()
	if !ok || sel.SessionID != "sess-2" {
		t.Errorf("selected() = %+v, ok=%v, want sess-2 (the filtered match at index 0)", sel, ok)
	}
}

// ── loop-creation wizard: parseMaxCycles ────────────────────────────

func TestParseMaxCycles_EmptyReturnsDefault(t *testing.T) {
	n, err := parseMaxCycles("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != registry.DefaultMaxCycles {
		t.Errorf("n = %d, want %d", n, registry.DefaultMaxCycles)
	}
}

func TestParseMaxCycles_ValidPositiveNumber(t *testing.T) {
	n, err := parseMaxCycles("20")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 20 {
		t.Errorf("n = %d, want 20", n)
	}
}

func TestParseMaxCycles_NonNumeric_Errors(t *testing.T) {
	if _, err := parseMaxCycles("abc"); err == nil {
		t.Error("expected an error for non-numeric input")
	}
}

func TestParseMaxCycles_Zero_Errors(t *testing.T) {
	if _, err := parseMaxCycles("0"); err == nil {
		t.Error("expected an error for zero (not a positive number)")
	}
}

func TestParseMaxCycles_Negative_Errors(t *testing.T) {
	if _, err := parseMaxCycles("-5"); err == nil {
		t.Error("expected an error for a negative number")
	}
}

// ── loop-creation wizard: buildSpawnPrompt (the contract block) ─────

func TestBuildSpawnPrompt_AllFieldsProvided(t *testing.T) {
	got := buildSpawnPrompt("fix the bug", "tests pass", "run go test ./...", "try to break it with -race", 20)
	want := "goal: fix the bug\n" +
		"complete condition: tests pass\n" +
		"oracle: run go test ./...\n" +
		"challenger: try to break it with -race\n" +
		"max_iteration: 20\n" +
		"\n" +
		"Work in cycles toward the goal. Report progress concretely each cycle.\n" +
		"Declare DONE only when the complete condition is met — state the evidence.\n" +
		"An independent oracle will verify your claim against this contract."
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestBuildSpawnPrompt_OptionalFieldsEmpty_UsesDefaultsAndOmitsChallenger(t *testing.T) {
	got := buildSpawnPrompt("fix the bug", "", "", "", 12)
	want := "goal: fix the bug\n" +
		"complete condition: you judge the goal fully achieved\n" +
		"oracle: an independent LLM judge verifies against the complete condition\n" +
		"max_iteration: 12\n" +
		"\n" +
		"Work in cycles toward the goal. Report progress concretely each cycle.\n" +
		"Declare DONE only when the complete condition is met — state the evidence.\n" +
		"An independent oracle will verify your claim against this contract."
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(got, "challenger:") {
		t.Error("expected the challenger line to be omitted entirely when empty")
	}
}

func TestBuildSpawnPrompt_ChallengerOnly_LineIncluded(t *testing.T) {
	got := buildSpawnPrompt("goal", "", "", "adversarial probe", 12)
	if !strings.Contains(got, "challenger: adversarial probe\n") {
		t.Errorf("got:\n%s\nwant a challenger line present when non-empty", got)
	}
}

// ── loop-creation wizard: step labels ────────────────────────────────

func TestWizardStepLabel_AllSteps(t *testing.T) {
	cases := []struct {
		step wizardStep
		want string
	}{
		{wizardGoal, "goal:"},
		{wizardDoneWhen, "complete condition:"},
		{wizardOracle, "oracle:"},
		{wizardChallenger, "challenger:"},
		{wizardMaxCycles, "max_iteration [12]:"},
	}
	for _, c := range cases {
		if got := wizardStepLabel(c.step); got != c.want {
			t.Errorf("wizardStepLabel(%v) = %q, want %q", c.step, got, c.want)
		}
	}
}

// ── worktree spawn: wizardWhere step ─────────────────────────────────

// reachWizardWhere drives the wizard from a fresh "n" keypress through all
// 5 free-text steps (goal filled, the rest left empty/default) with
// worktree eligibility forced true, landing at wizardWhere. Used by every
// wizardWhere test below so each one only has to exercise the final step.
func reachWizardWhere(t *testing.T, m Model) Model {
	t.Helper()
	m, _ = updateModel(t, m, runeKey('n'))
	m.spawnWorktreeEligible = true // simulate checkWorktreeEligibilityCmd's async result having already arrived
	m, _ = typeAndEnter(t, m, "goal")
	m, _ = typeAndEnter(t, m, "")
	m, _ = typeAndEnter(t, m, "")
	m, _ = typeAndEnter(t, m, "")
	m, _ = typeAndEnter(t, m, "")
	if m.mode != modePrompting || m.spawnStep != wizardWhere {
		t.Fatalf("precondition failed: mode=%v step=%v, want modePrompting at wizardWhere", m.mode, m.spawnStep)
	}
	return m
}

func TestWizard_SkipsWhereStep_WhenNotEligible(t *testing.T) {
	// the zero-value default (spawnWorktreeEligible=false, e.g. no backend
	// resolved, or tmux/cmux) — the wizard must submit directly from
	// wizardMaxCycles rather than showing a choice that always degrades.
	m := modelWithOneLoop()
	m, _ = updateModel(t, m, runeKey('n'))
	m, _ = typeAndEnter(t, m, "goal")
	m, _ = typeAndEnter(t, m, "")
	m, _ = typeAndEnter(t, m, "")
	m, _ = typeAndEnter(t, m, "")
	m, cmd := typeAndEnter(t, m, "")

	if m.mode != modeNormal {
		t.Errorf("mode = %v, want modeNormal (wizardWhere skipped)", m.mode)
	}
	if cmd == nil {
		t.Fatal("expected a non-nil tea.Cmd (spawnCmd) — submits directly when ineligible")
	}
	if strings.Contains(m.status, "new worktree") {
		t.Errorf("status = %q, want the current-dir spawn message", m.status)
	}
}

func TestWizard_ReachesWhereStep_WhenEligible(t *testing.T) {
	m := reachWizardWhere(t, modelWithOneLoop())
	if !strings.Contains(m.whereStepLabel(), "new worktree") {
		t.Errorf("whereStepLabel() = %q, want it to mention the worktree option", m.whereStepLabel())
	}
}

func TestUpdate_WizardWhere_DKey_SubmitsCurrentDirSpawn(t *testing.T) {
	m := reachWizardWhere(t, modelWithOneLoop())

	m, cmd := updateModel(t, m, runeKey('d'))

	if m.mode != modeNormal {
		t.Errorf("mode = %v, want modeNormal", m.mode)
	}
	if cmd == nil {
		t.Fatal("expected a non-nil tea.Cmd (spawnCmd)")
	}
	if strings.Contains(m.status, "new worktree") {
		t.Errorf("status = %q, want the current-dir spawn message, not worktree", m.status)
	}
}

func TestUpdate_WizardWhere_WKey_SubmitsWorktreeSpawn(t *testing.T) {
	m := reachWizardWhere(t, modelWithOneLoop())

	m, cmd := updateModel(t, m, runeKey('w'))

	if m.mode != modeNormal {
		t.Errorf("mode = %v, want modeNormal", m.mode)
	}
	if cmd == nil {
		t.Fatal("expected a non-nil tea.Cmd (spawnCmd)")
	}
	if !strings.Contains(m.status, "new worktree") {
		t.Errorf("status = %q, want the worktree spawn message", m.status)
	}
}

func TestUpdate_WizardWhere_EnterKey_DefaultsToWorktree_WhenHostsClaudeRepo(t *testing.T) {
	// modelWithOneLoop selects a loop with Cwd set, so spawnHostsClaudeRepo
	// is true — combined with the forced-eligible backend, enter's default
	// must resolve to worktree.
	m := reachWizardWhere(t, modelWithOneLoop())

	m, cmd := updateModel(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	if cmd == nil {
		t.Fatal("expected a non-nil tea.Cmd (spawnCmd)")
	}
	if !strings.Contains(m.status, "new worktree") {
		t.Errorf("status = %q, want the worktree default (eligible AND hosts a claude repo)", m.status)
	}
}

func TestUpdate_WizardWhere_EnterKey_DefaultsToCurrentDir_WhenNoRepoEvidence(t *testing.T) {
	// no loop selected ("n" pressed with nothing to select) — no evidence
	// spawnCwd is a real claude repo, so enter's default must NOT assume
	// worktree even though the backend is eligible.
	m := reachWizardWhere(t, New())

	m, cmd := updateModel(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	if cmd == nil {
		t.Fatal("expected a non-nil tea.Cmd (spawnCmd)")
	}
	if strings.Contains(m.status, "new worktree") {
		t.Errorf("status = %q, want the current-dir default (no evidence this cwd is a claude repo)", m.status)
	}
}

func TestUpdate_WizardWhere_Esc_Cancels(t *testing.T) {
	m := reachWizardWhere(t, modelWithOneLoop())

	m, cmd := updateModel(t, m, tea.KeyMsg{Type: tea.KeyEsc})

	if m.mode != modeNormal {
		t.Errorf("mode = %v, want modeNormal", m.mode)
	}
	if cmd != nil {
		t.Error("expected no tea.Cmd from cancelling")
	}
}

func TestUpdate_WizardWhere_IgnoresUnrelatedKeys(t *testing.T) {
	m := reachWizardWhere(t, modelWithOneLoop())

	m, cmd := updateModel(t, m, runeKey('x'))

	if cmd != nil {
		t.Error("expected no tea.Cmd for an unrelated key")
	}
	if m.mode != modePrompting || m.spawnStep != wizardWhere {
		t.Errorf("got mode=%v step=%v, want to remain at wizardWhere", m.mode, m.spawnStep)
	}
}

func TestWhereStepLabel_BusyDirNudge(t *testing.T) {
	m := New()
	m.loops = []domain.Loop{{Project: "aboard", SessionID: "sess-1", Cwd: "/x/aboard", State: domain.StateRunning}}
	m.spawnCwd = "/x/aboard"

	if !strings.Contains(m.whereStepLabel(), "dir busy") {
		t.Errorf("whereStepLabel() = %q, want the busy-dir nudge (a fleet loop shares spawnCwd)", m.whereStepLabel())
	}
}

func TestWhereStepLabel_NoBusyNudge_WhenDirEmpty(t *testing.T) {
	m := New()
	m.loops = []domain.Loop{{Project: "aboard", SessionID: "sess-1", Cwd: "/x/other", State: domain.StateRunning}}
	m.spawnCwd = "/x/aboard"

	if strings.Contains(m.whereStepLabel(), "dir busy") {
		t.Errorf("whereStepLabel() = %q, want no busy nudge (no loop shares spawnCwd)", m.whereStepLabel())
	}
}

func TestSpawnDirBusyCount(t *testing.T) {
	m := New()
	m.loops = []domain.Loop{
		{SessionID: "s1", Cwd: "/x/aboard"},
		{SessionID: "s2", Cwd: "/x/aboard"},
		{SessionID: "s3", Cwd: "/x/other"},
	}
	m.spawnCwd = "/x/aboard"

	if got := m.spawnDirBusyCount(); got != 2 {
		t.Errorf("got %d, want 2", got)
	}
}

// ── worktree spawn: worktreeNameFromGoal ─────────────────────────────

func TestWorktreeNameFromGoal_BasicSlug(t *testing.T) {
	got := worktreeNameFromGoal("Fix the flaky test")
	want := "mctl-fix-the-flaky-test"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestWorktreeNameFromGoal_TruncatesTo24Runes(t *testing.T) {
	goal := strings.Repeat("a", 40)
	got := worktreeNameFromGoal(goal)
	want := "mctl-" + strings.Repeat("a", 24)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestWorktreeNameFromGoal_NonAlnumCollapsesToSingleDash(t *testing.T) {
	got := worktreeNameFromGoal("fix: bug #123!!")
	want := "mctl-fix-bug-123"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestWorktreeNameFromGoal_EmptyGoal_FallsBackToLoop(t *testing.T) {
	if got := worktreeNameFromGoal(""); got != "mctl-loop" {
		t.Errorf("got %q, want %q", got, "mctl-loop")
	}
}

func TestWorktreeNameFromGoal_AllPunctuation_FallsBackToLoop(t *testing.T) {
	if got := worktreeNameFromGoal("!!!???"); got != "mctl-loop" {
		t.Errorf("got %q, want %q", got, "mctl-loop")
	}
}

// Regression: CJK text is double-width in the terminal — rune-count
// truncation overflowed the column cell and sheared the whole row
// (captain-reported with Korean DOING snippets).
func TestTrunc_CJKDisplayWidth(t *testing.T) {
	got := trunc("캡틴 재설치 완료 보고합니다", 10)
	if w := runewidth.StringWidth(got); w > 10 {
		t.Errorf("trunc CJK display width = %d, want <= 10 (%q)", w, got)
	}
	if got := trunc("short", 10); got != "short" {
		t.Errorf("ascii under width must pass through, got %q", got)
	}
	mixed := trunc("fix한글mix되는지123456", 12)
	if w := runewidth.StringWidth(mixed); w > 12 {
		t.Errorf("mixed trunc width = %d, want <= 12 (%q)", w, mixed)
	}
}

// ── event-log-and-notify: scan-triggered transition detection ───────────

func TestDetectTransitions_FirstAppearance_NoEvent(t *testing.T) {
	m := New() // m.loops is empty — every session in newLoops is "brand new"
	newLoops := []domain.Loop{{SessionID: "s1", State: domain.StateRunning}}

	got, _ := m.detectTransitions(newLoops, time.Now())

	if len(got) != 0 {
		t.Fatalf("got %d transitions on first appearance, want 0", len(got))
	}
}

// ── P2 review fix: restart-time re-notify for an already-open gate ───────

func TestSeedFirstAppearanceGate_AlreadyGated_SeedsNotifyAndEvent(t *testing.T) {
	// Simulates the exact restart gap the review flagged: m.loops is empty
	// (a fresh Model, as if missionctl just started), and the very FIRST
	// scan already shows a loop sitting in StateGate — there is no
	// "previous scan" to diff against, yet this must still notify once.
	m := New()
	newLoops := []domain.Loop{{SessionID: "s1", Project: "aboard", State: domain.StateGate, GatePrompt: "continue?"}}

	got, _ := m.detectTransitions(newLoops, time.Now())

	if len(got) != 1 {
		t.Fatalf("got %d transitions for an already-gated first appearance, want 1 (seeded)", len(got))
	}
	te := got[0]
	if !te.notify {
		t.Error("expected the seeded edge to be flagged for notify")
	}
	if te.title != notifyTitlePrefix+"missionctl · GATE" {
		t.Errorf("title = %q, want the GATE title", te.title)
	}
	if !strings.Contains(te.body, "aboard") || !strings.Contains(te.body, "continue?") {
		t.Errorf("body = %q, want it to mention the project and gate prompt", te.body)
	}
	if te.ev.FromState != "" {
		t.Errorf("FromState = %q, want empty (nothing to transition from — same convention as a spawn event)", te.ev.FromState)
	}
	if te.ev.ToState != string(domain.StateGate) {
		t.Errorf("ToState = %q, want %q", te.ev.ToState, domain.StateGate)
	}
}

func TestSeedFirstAppearanceGate_DedupAppliesOnRestartWithinWindow(t *testing.T) {
	// If a notification for this exact gate was ALREADY sent (e.g. it was
	// open before the restart and the ledger... well, the ledger doesn't
	// survive a restart by construction — but shouldNotify's dedup must
	// still apply WITHIN one process's lifetime: two back-to-back "restart
	// scans" for the same still-open gate must only notify once.
	m := New()
	now := time.Now()
	loops := []domain.Loop{{SessionID: "s1", Project: "aboard", State: domain.StateGate, GatePrompt: "continue?"}}

	first, _ := m.detectTransitions(loops, now)
	if len(first) != 1 || !first[0].notify {
		t.Fatalf("first seed: got %#v, want one notify-flagged transition", first)
	}

	// m.loops is STILL empty here on purpose: this simulates detectTransitions
	// being called again before Update ever assigns m.loops = newLoops (not
	// how Update actually sequences it, but shouldNotify's ledger is what's
	// under test here, not the m.loops assignment timing).
	second, _ := m.detectTransitions(loops, now.Add(time.Second))
	if len(second) != 1 {
		t.Fatalf("got %d transitions on the second identical seed, want 1 (still seeded, just not re-notified)", len(second))
	}
	if second[0].notify {
		t.Error("expected the second seed within the dedup window to NOT re-notify")
	}
}

func TestSeedFirstAppearanceGate_NonGateFirstAppearance_NotSeeded(t *testing.T) {
	m := New()
	newLoops := []domain.Loop{{SessionID: "s1", State: domain.StateStalled, Stall: domain.StallGone}}

	got, _ := m.detectTransitions(newLoops, time.Now())

	if len(got) != 0 {
		t.Fatalf("got %d transitions for a non-gate first appearance, want 0 (only StateGate is seeded, per the review's explicit scope)", len(got))
	}
}

func TestDetectTransitions_StateChange_EmitsOneEvent(t *testing.T) {
	m := New()
	m.loops = []domain.Loop{{SessionID: "s1", State: domain.StateRunning}}
	newLoops := []domain.Loop{{SessionID: "s1", State: domain.StateIdle}}

	got, _ := m.detectTransitions(newLoops, time.Now())

	if len(got) != 1 {
		t.Fatalf("got %d transitions, want 1", len(got))
	}
	ev := got[0].ev
	if ev.FromState != string(domain.StateRunning) || ev.ToState != string(domain.StateIdle) {
		t.Errorf("FromState/ToState = %q/%q, want running/idle", ev.FromState, ev.ToState)
	}
	if ev.Trigger != events.TriggerScan || ev.Actor != events.ActorSystem {
		t.Errorf("Trigger/Actor = %v/%v, want scan/system", ev.Trigger, ev.Actor)
	}
}

// TestDetectTransitions_SameStateAcrossTwoScans_OnlyOneEventTotal is the
// task's edge-trigger acceptance bar: a real A→B transition followed by B
// persisting unchanged on the NEXT poll must record exactly one event
// total, never a duplicate for "still B".
func TestDetectTransitions_SameStateAcrossTwoScans_OnlyOneEventTotal(t *testing.T) {
	m := New()
	m.loops = []domain.Loop{{SessionID: "s1", State: domain.StateRunning}}
	now := time.Now()

	scan2 := []domain.Loop{{SessionID: "s1", State: domain.StateStalled, Stall: domain.StallNoOutput}}
	firstTransitions, _ := m.detectTransitions(scan2, now)
	m.loops = scan2 // simulate Update overwriting m.loops after this scan

	scan3 := []domain.Loop{{SessionID: "s1", State: domain.StateStalled, Stall: domain.StallNoOutput}} // unchanged
	secondTransitions, _ := m.detectTransitions(scan3, now.Add(3*time.Second))

	if len(firstTransitions) != 1 {
		t.Fatalf("scan1→scan2: got %d transitions, want 1 (the real running→stalled edge)", len(firstTransitions))
	}
	if len(secondTransitions) != 0 {
		t.Fatalf("scan2→scan3 (unchanged): got %d transitions, want 0 (edge-triggered, no re-emit)", len(secondTransitions))
	}
}

func TestDetectTransitions_StallKindChange_SameLoopState_StillCountsAsATransition(t *testing.T) {
	// StallNoOutput -> StallGone: both StateStalled, but this is exactly the
	// edge the notify policy needs to catch (see stateSignature's doc).
	m := New()
	m.loops = []domain.Loop{{SessionID: "s1", State: domain.StateStalled, Stall: domain.StallNoOutput}}
	newLoops := []domain.Loop{{SessionID: "s1", State: domain.StateStalled, Stall: domain.StallGone}}

	got, _ := m.detectTransitions(newLoops, time.Now())

	if len(got) != 1 {
		t.Fatalf("got %d transitions for a stall-kind-only change, want 1", len(got))
	}
	if !got[0].notify {
		t.Error("expected this to be flagged for notify (entering StallGone)")
	}
	if got[0].title != notifyTitlePrefix+"missionctl · loop gone" {
		t.Errorf("title = %q, want the loop-gone title", got[0].title)
	}
	// P2 review fix regression: the PERSISTED FromState/ToState must also
	// differ, not just the in-memory notify decision — otherwise
	// `missionctl report`'s FromState!=ToState transition counting (and a
	// human reading the raw history log) can't see this incident happened
	// at all, since both sides would read the same plain "stalled".
	if got[0].ev.FromState == got[0].ev.ToState {
		t.Errorf("FromState == ToState == %q, want them to differ (stall kind must be encoded into the persisted state)", got[0].ev.FromState)
	}
	if got[0].ev.FromState != "stalled:no-output" || got[0].ev.ToState != "stalled:gone" {
		t.Errorf("FromState/ToState = %q/%q, want stalled:no-output/stalled:gone", got[0].ev.FromState, got[0].ev.ToState)
	}
}

func TestDetectTransitions_IntoGate_FlaggedForNotify(t *testing.T) {
	m := New()
	m.loops = []domain.Loop{{SessionID: "s1", Project: "aboard", State: domain.StateRunning}}
	newLoops := []domain.Loop{{SessionID: "s1", Project: "aboard", State: domain.StateGate, GatePrompt: "continue?"}}

	got, _ := m.detectTransitions(newLoops, time.Now())

	if len(got) != 1 || !got[0].notify {
		t.Fatalf("got %#v, want exactly one notify-flagged transition", got)
	}
	if got[0].title != notifyTitlePrefix+"missionctl · GATE" {
		t.Errorf("title = %q, want the GATE title", got[0].title)
	}
	if !strings.Contains(got[0].body, "aboard") || !strings.Contains(got[0].body, "continue?") {
		t.Errorf("body = %q, want it to mention the project and gate prompt", got[0].body)
	}
}

func TestDetectTransitions_OrdinaryTransition_NotFlaggedForNotify(t *testing.T) {
	// running -> idle is a real transition (recorded in history) but is NOT
	// one of the two notify-worthy edges — severity floor, per the task's
	// spec (no notify on done/drift/429 either, only gate+gone).
	m := New()
	m.loops = []domain.Loop{{SessionID: "s1", State: domain.StateRunning}}
	newLoops := []domain.Loop{{SessionID: "s1", State: domain.StateIdle}}

	got, _ := m.detectTransitions(newLoops, time.Now())

	if len(got) != 1 {
		t.Fatalf("got %d transitions, want 1", len(got))
	}
	if got[0].notify {
		t.Error("running->idle must not trigger a notification")
	}
}

func TestDetectTransitions_AlreadyGated_NoRepeatNotifyFlagOnUnrelatedChange(t *testing.T) {
	// staying in StateGate (not a fresh entry) must not re-flag notify, even
	// if something else about the loop incidentally changed this scan.
	m := New()
	m.loops = []domain.Loop{{SessionID: "s1", State: domain.StateGate, GatePrompt: "old?"}}
	newLoops := []domain.Loop{{SessionID: "s1", State: domain.StateGate, GatePrompt: "old?"}}

	got, _ := m.detectTransitions(newLoops, time.Now())

	if len(got) != 0 {
		t.Fatalf("got %d transitions for an unchanged gate, want 0 (same signature both scans)", len(got))
	}
}

// ── event-log-and-notify: dedup ledger ───────────────────────────────────

func TestShouldNotify_FirstCall_Allowed(t *testing.T) {
	m := New()
	if !m.shouldNotify("s1", "gate", time.Now()) {
		t.Error("expected the first notify for a fresh (session, edge) to be allowed")
	}
}

func TestShouldNotify_SecondCallWithinWindow_Refused(t *testing.T) {
	m := New()
	now := time.Now()
	if !m.shouldNotify("s1", "gate", now) {
		t.Fatal("first call should be allowed")
	}
	if m.shouldNotify("s1", "gate", now.Add(time.Minute)) {
		t.Error("expected a second notify within notifyDedupWindow to be refused")
	}
}

func TestShouldNotify_AfterWindowExpires_AllowedAgain(t *testing.T) {
	m := New()
	now := time.Now()
	if !m.shouldNotify("s1", "gate", now) {
		t.Fatal("first call should be allowed")
	}
	if !m.shouldNotify("s1", "gate", now.Add(notifyDedupWindow+time.Second)) {
		t.Error("expected a notify after the dedup window elapsed to be allowed")
	}
}

func TestShouldNotify_DifferentEdges_IndependentLedgerEntries(t *testing.T) {
	m := New()
	now := time.Now()
	if !m.shouldNotify("s1", "gate", now) {
		t.Fatal("first gate notify should be allowed")
	}
	if !m.shouldNotify("s1", "gone", now) {
		t.Error("a different edge for the SAME session must have its own ledger entry")
	}
}

func TestShouldNotify_DifferentSessions_IndependentLedgerEntries(t *testing.T) {
	m := New()
	now := time.Now()
	if !m.shouldNotify("s1", "gate", now) {
		t.Fatal("first notify should be allowed")
	}
	if !m.shouldNotify("s2", "gate", now) {
		t.Error("a different session must have its own ledger entry")
	}
}

func TestDetectTransitions_DedupAppliesAcrossScans_SecondGateEntryNotRenotified(t *testing.T) {
	// A loop that leaves and re-enters StateGate twice within the dedup
	// window must only be flagged for notify once — detectTransitions
	// itself consults m.shouldNotify (not just a standalone unit).
	m := New()
	now := time.Now()

	m.loops = []domain.Loop{{SessionID: "s1", State: domain.StateRunning}}
	first, _ := m.detectTransitions([]domain.Loop{{SessionID: "s1", State: domain.StateGate, GatePrompt: "p1"}}, now)
	if len(first) != 1 || !first[0].notify {
		t.Fatalf("first gate entry: got %#v, want one notify-flagged transition", first)
	}
	m.loops = []domain.Loop{{SessionID: "s1", State: domain.StateGate, GatePrompt: "p1"}}

	// leaves the gate, then re-enters it, both within the dedup window.
	left, _ := m.detectTransitions([]domain.Loop{{SessionID: "s1", State: domain.StateRunning}}, now.Add(time.Second))
	m.loops = []domain.Loop{{SessionID: "s1", State: domain.StateRunning}}
	second, _ := m.detectTransitions([]domain.Loop{{SessionID: "s1", State: domain.StateGate, GatePrompt: "p2"}}, now.Add(2*time.Second))

	if len(left) != 1 {
		t.Fatalf("leaving the gate: got %d transitions, want 1 (still a real, history-worthy transition)", len(left))
	}
	if len(second) != 1 {
		t.Fatalf("re-entering the gate: got %d transitions, want 1 (still history-worthy)", len(second))
	}
	if second[0].notify {
		t.Error("re-entering the gate within the dedup window must NOT re-notify")
	}
}

// ── event-log-and-notify: emitTransitionsCmd + judgeCmd wiring ───────────

func TestEmitTransitionsCmd_NilForNoTransitions(t *testing.T) {
	if cmd := emitTransitionsCmd(nil); cmd != nil {
		t.Error("expected a nil tea.Cmd when there's nothing to emit")
	}
}

func TestEmitTransitionsCmd_AppendsHistoryAndSendsNotifications(t *testing.T) {
	historyDir := t.TempDir()
	origHistoryDir, origNotify := historyDirFn, notifySendFn
	defer func() { historyDirFn, notifySendFn = origHistoryDir, origNotify }()
	historyDirFn = func() string { return historyDir }

	var gotTitle, gotBody string
	notifySendFn = func(title, body string) error {
		gotTitle, gotBody = title, body
		return nil
	}

	transitions := []transitionEvent{
		{ev: events.Event{SessionID: "s1", FromState: "running", ToState: "gate", Trigger: events.TriggerScan, Actor: events.ActorSystem}, notify: true, title: "missionctl · GATE", body: "aboard: continue?"},
		{ev: events.Event{SessionID: "s2", FromState: "running", ToState: "idle", Trigger: events.TriggerScan, Actor: events.ActorSystem}, notify: false},
	}

	cmd := emitTransitionsCmd(transitions)
	if cmd == nil {
		t.Fatal("expected a non-nil tea.Cmd")
	}
	cmd()

	got, err := events.ReadAll(historyDir)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got["s1"]) != 1 || len(got["s2"]) != 1 {
		t.Fatalf("got %#v, want exactly one event per session", got)
	}
	if gotTitle != "missionctl · GATE" || gotBody != "aboard: continue?" {
		t.Errorf("notify called with (%q, %q), want the flagged transition's title/body", gotTitle, gotBody)
	}
}

func TestEmitTransitionsCmd_NotifyErrorSwallowed(t *testing.T) {
	historyDir := t.TempDir()
	origHistoryDir, origNotify := historyDirFn, notifySendFn
	defer func() { historyDirFn, notifySendFn = origHistoryDir, origNotify }()
	historyDirFn = func() string { return historyDir }
	notifySendFn = func(title, body string) error { return errTestJudgeFailed }

	transitions := []transitionEvent{
		{ev: events.Event{SessionID: "s1", ToState: "gate", Trigger: events.TriggerScan, Actor: events.ActorSystem}, notify: true, title: "t", body: "b"},
	}
	cmd := emitTransitionsCmd(transitions)
	msg := cmd()
	if msg != nil {
		t.Errorf("got %v, want nil — a notify failure must not surface as a tea.Msg", msg)
	}
}

func TestJudgeCmd_RecordsOracleHistoryEvent(t *testing.T) {
	registryDir := t.TempDir()
	historyDir := t.TempDir()
	origRegDir, origJudgeFn, origHistoryDir := registryDirFn, judgeFn, historyDirFn
	defer func() { registryDirFn, judgeFn, historyDirFn = origRegDir, origJudgeFn, origHistoryDir }()
	registryDirFn = func() string { return registryDir }
	historyDirFn = func() string { return historyDir }
	judgeFn = func(goal, cwd, lastText, doneWhen, oracleRubric string) (domain.Verdict, error) {
		return domain.Verdict{Outcome: domain.OutcomeDone, Reason: "tests pass"}, nil
	}
	if err := registry.Bind(registryDir, "s1", registry.BindSpec{Goal: "fix the bug"}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	l := domain.Loop{SessionID: "s1", Cycle: 3, State: domain.StateIdle, Goal: domain.Goal{Text: "fix the bug"}}
	judgeCmd(l)()

	got, err := events.ReadAll(historyDir)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	evs := got["s1"]
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1: %#v", len(evs), evs)
	}
	ev := evs[0]
	if ev.Trigger != events.TriggerOracle || ev.Actor != events.ActorAuto {
		t.Errorf("Trigger/Actor = %v/%v, want oracle/auto", ev.Trigger, ev.Actor)
	}
	if !strings.Contains(ev.Detail, "done") || !strings.Contains(ev.Detail, "3") {
		t.Errorf("Detail = %q, want it to carry the outcome and cycle", ev.Detail)
	}
}

func TestSendPromptCmd_TierOneSuccess_RecordsActuationEvent(t *testing.T) {
	fakeCtrl := &fakeController{name: "tmux"}
	withFakeActuationSeams(t,
		func(sessionsDir, sessionID, projectDir string) (control.Controller, control.Target, bool, bool) {
			return fakeCtrl, control.Target{Backend: "tmux", ID: "%3"}, true, true
		},
		nil,
	)
	l := domain.Loop{SessionID: "sess-1", Project: "aboard", State: domain.StateStalled}

	sendPromptCmd(l, "do the thing", "resume", "resumed", "")()

	got, err := events.ReadAll(historyDirFn())
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	evs := got["sess-1"]
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1: %#v", len(evs), evs)
	}
	ev := evs[0]
	if ev.Trigger != events.TriggerActuation || ev.Actor != events.ActorHuman {
		t.Errorf("Trigger/Actor = %v/%v, want actuation/human", ev.Trigger, ev.Actor)
	}
	if !strings.Contains(ev.Detail, "resume") || !strings.Contains(ev.Detail, "tier1") || !strings.Contains(ev.Detail, "ok") {
		t.Errorf("Detail = %q, want it to mention the action, tier, and outcome", ev.Detail)
	}
}

func TestSendPromptCmd_StateFailedRefusal_NoActuationEventRecorded(t *testing.T) {
	historyDir := t.TempDir()
	origHistoryDir := historyDirFn
	defer func() { historyDirFn = origHistoryDir }()
	historyDirFn = func() string { return historyDir }

	l := domain.Loop{SessionID: "sess-1", Project: "aboard", State: domain.StateFailed}
	sendPromptCmd(l, "do the thing", "resume", "resumed", "")()

	got, err := events.ReadAll(historyDir)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d sessions with events, want 0 (refused before any tier was reached)", len(got))
	}
}

// ── feat/auto-redrive-429 ──────────────────────────────────────────────────

// withAutoRedriveEnabled overrides autoRedriveEnabledFn to the given value
// for the duration of one test, restoring the original on cleanup — the
// opt-in kill switch's test seam (see autoRedriveEnabledFn's doc).
func withAutoRedriveEnabled(t *testing.T, enabled bool) {
	t.Helper()
	orig := autoRedriveEnabledFn
	t.Cleanup(func() { autoRedriveEnabledFn = orig })
	autoRedriveEnabledFn = func() bool { return enabled }
}

// ── opt-in default / kill switch ─────────────────────────────────────────

func TestMaybeScheduleAutoRedrive429_OptOutDefault_NeverSchedules(t *testing.T) {
	withAutoRedriveEnabled(t, false)
	m := New()
	l := domain.Loop{SessionID: "s1", Project: "aboard", State: domain.StateStalled, Stall: domain.StallRateLimit}

	beforeStatus := m.status
	if cmd := m.maybeScheduleAutoRedrive429(l, true, time.Now()); cmd != nil {
		t.Error("expected nil — auto-redrive is opt-in, off by default")
	}
	if m.status != beforeStatus {
		t.Errorf("status = %q, want unchanged from %q — nothing should have happened", m.status, beforeStatus)
	}
}

// ── edge-triggered scheduling ─────────────────────────────────────────────

func TestMaybeScheduleAutoRedrive429_EdgeTriggersSchedule(t *testing.T) {
	withAutoRedriveEnabled(t, true)
	historyDirFn = func() string { return t.TempDir() }
	m := New()
	l := domain.Loop{SessionID: "s1", Project: "aboard", State: domain.StateStalled, Stall: domain.StallRateLimit}
	now := time.Now()

	cmd := m.maybeScheduleAutoRedrive429(l, true, now)

	if cmd == nil {
		t.Fatal("expected a non-nil tea.Cmd (the scheduled tea.Tick)")
	}
	if !strings.Contains(m.status, "auto: re-driving aboard in 5m (attempt 1/3)") {
		t.Errorf("status = %q, want the scheduled-status text", m.status)
	}
	if got, ok := m.autoRedriveScheduledAt["s1"]; !ok || !got.Equal(now) {
		t.Errorf("autoRedriveScheduledAt[s1] = %v, ok=%v, want %v", got, ok, now)
	}
}

func TestMaybeScheduleAutoRedrive429_NotAnEdge_NoSchedule(t *testing.T) {
	withAutoRedriveEnabled(t, true)
	m := New()
	l := domain.Loop{SessionID: "s1", Project: "aboard", State: domain.StateStalled, Stall: domain.StallRateLimit}

	if cmd := m.maybeScheduleAutoRedrive429(l, false, time.Now()); cmd != nil {
		t.Error("expected nil — enteredRateLimit=false is not a fresh edge")
	}
}

// ── dedup window ──────────────────────────────────────────────────────────

func TestMaybeScheduleAutoRedrive429_DedupWindow_SecondCallWithinWindowRefused(t *testing.T) {
	withAutoRedriveEnabled(t, true)
	historyDirFn = func() string { return t.TempDir() }
	m := New()
	l := domain.Loop{SessionID: "s1", Project: "aboard", State: domain.StateStalled, Stall: domain.StallRateLimit}
	now := time.Now()

	if cmd := m.maybeScheduleAutoRedrive429(l, true, now); cmd == nil {
		t.Fatal("first call should schedule")
	}
	if cmd := m.maybeScheduleAutoRedrive429(l, true, now.Add(time.Minute)); cmd != nil {
		t.Error("expected nil — within the dedup window of the first schedule")
	}
}

func TestMaybeScheduleAutoRedrive429_AfterDedupWindowExpires_SchedulesAgain(t *testing.T) {
	withAutoRedriveEnabled(t, true)
	historyDirFn = func() string { return t.TempDir() }
	m := New()
	l := domain.Loop{SessionID: "s1", Project: "aboard", State: domain.StateStalled, Stall: domain.StallRateLimit}
	now := time.Now()

	if cmd := m.maybeScheduleAutoRedrive429(l, true, now); cmd == nil {
		t.Fatal("first call should schedule")
	}
	if cmd := m.maybeScheduleAutoRedrive429(l, true, now.Add(autoRedriveDelay+time.Second)); cmd == nil {
		t.Error("expected a non-nil cmd — the dedup window has elapsed")
	}
}

// ── attempt ceiling ───────────────────────────────────────────────────────

func TestMaybeScheduleAutoRedrive429_AttemptCeiling_NoScheduleAtMax(t *testing.T) {
	withAutoRedriveEnabled(t, true)
	historyDirFn = func() string { return t.TempDir() }
	m := New()
	m.autoRedriveAttempts = map[string]int{"s1": autoRedriveMaxAttempts}
	l := domain.Loop{SessionID: "s1", Project: "aboard", State: domain.StateStalled, Stall: domain.StallRateLimit}

	if cmd := m.maybeScheduleAutoRedrive429(l, true, time.Now()); cmd != nil {
		t.Error("expected nil — already at the lifetime attempt ceiling")
	}
}

func TestMaybeScheduleAutoRedrive429_BelowCeiling_Schedules(t *testing.T) {
	withAutoRedriveEnabled(t, true)
	historyDirFn = func() string { return t.TempDir() }
	m := New()
	m.autoRedriveAttempts = map[string]int{"s1": autoRedriveMaxAttempts - 1}
	l := domain.Loop{SessionID: "s1", Project: "aboard", State: domain.StateStalled, Stall: domain.StallRateLimit}

	if cmd := m.maybeScheduleAutoRedrive429(l, true, time.Now()); cmd == nil {
		t.Error("expected a non-nil cmd — one attempt below the ceiling")
	}
}

// ── gate/failed defense in depth ─────────────────────────────────────────

func TestMaybeScheduleAutoRedrive429_StateFailed_NeverSchedules(t *testing.T) {
	withAutoRedriveEnabled(t, true)
	m := New()
	l := domain.Loop{SessionID: "s1", Project: "aboard", State: domain.StateFailed, Stall: domain.StallRateLimit}

	if cmd := m.maybeScheduleAutoRedrive429(l, true, time.Now()); cmd != nil {
		t.Error("expected nil — StateFailed must never auto-redrive")
	}
}

func TestMaybeScheduleAutoRedrive429_StateGate_NeverSchedules(t *testing.T) {
	withAutoRedriveEnabled(t, true)
	m := New()
	l := domain.Loop{SessionID: "s1", Project: "aboard", State: domain.StateGate}

	if cmd := m.maybeScheduleAutoRedrive429(l, true, time.Now()); cmd != nil {
		t.Error("expected nil — StateGate must never auto-redrive")
	}
}

// ── autoRedriveAttemptCount: lazy recount from the event log ──────────────

func TestAutoRedriveAttemptCount_LazySeedsFromEventLog(t *testing.T) {
	historyDir := t.TempDir()
	origHistoryDir := historyDirFn
	defer func() { historyDirFn = origHistoryDir }()
	historyDirFn = func() string { return historyDir }

	for i := 1; i <= 2; i++ {
		if err := events.Append(historyDir, events.Event{
			TS: int64(i), SessionID: "s1", ToState: "stalled:rate-limit",
			Trigger: events.TriggerActuation, Actor: events.ActorAuto,
			Detail: fmt.Sprintf("%s%d/%d", autoRedriveDetailPrefix, i, autoRedriveMaxAttempts),
		}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	m := New()
	if got := m.autoRedriveAttemptCount("s1"); got != 2 {
		t.Errorf("got %d, want 2 (recounted from the event log — restart-safe ceiling)", got)
	}
}

func TestAutoRedriveAttemptCount_NoHistory_Zero(t *testing.T) {
	historyDirFn = func() string { return t.TempDir() }
	m := New()
	if got := m.autoRedriveAttemptCount("no-such-session"); got != 0 {
		t.Errorf("got %d, want 0", got)
	}
}

func TestAutoRedriveAttemptCount_IgnoresOtherActuationEvents(t *testing.T) {
	historyDir := t.TempDir()
	historyDirFn = func() string { return historyDir }
	if err := events.Append(historyDir, events.Event{TS: 1, SessionID: "s1", Trigger: events.TriggerActuation, Actor: events.ActorHuman, Detail: "kill tier1 ok"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	m := New()
	if got := m.autoRedriveAttemptCount("s1"); got != 0 {
		t.Errorf("got %d, want 0 — a human kill event must not count as an auto-redrive attempt", got)
	}
}

// ── detectTransitions integration ────────────────────────────────────────

func TestDetectTransitions_EnteredRateLimit_Enabled_SchedulesAutoRedrive(t *testing.T) {
	withAutoRedriveEnabled(t, true)
	historyDirFn = func() string { return t.TempDir() }
	m := New()
	m.loops = []domain.Loop{{SessionID: "s1", Project: "aboard", State: domain.StateRunning}}
	newLoops := []domain.Loop{{SessionID: "s1", Project: "aboard", State: domain.StateStalled, Stall: domain.StallRateLimit}}

	_, cmds := m.detectTransitions(newLoops, time.Now())

	if len(cmds) != 1 {
		t.Fatalf("got %d auto-redrive cmds, want 1", len(cmds))
	}
}

func TestDetectTransitions_EnteredRateLimit_OptedOut_NoSchedule(t *testing.T) {
	withAutoRedriveEnabled(t, false)
	m := New()
	m.loops = []domain.Loop{{SessionID: "s1", Project: "aboard", State: domain.StateRunning}}
	newLoops := []domain.Loop{{SessionID: "s1", Project: "aboard", State: domain.StateStalled, Stall: domain.StallRateLimit}}

	_, cmds := m.detectTransitions(newLoops, time.Now())

	if len(cmds) != 0 {
		t.Errorf("got %d auto-redrive cmds, want 0 (opted out)", len(cmds))
	}
}

func TestDetectTransitions_AlreadyRateLimited_NotANewEdge_NoSchedule(t *testing.T) {
	withAutoRedriveEnabled(t, true)
	m := New()
	m.loops = []domain.Loop{{SessionID: "s1", Project: "aboard", State: domain.StateStalled, Stall: domain.StallRateLimit}}
	newLoops := []domain.Loop{{SessionID: "s1", Project: "aboard", State: domain.StateStalled, Stall: domain.StallRateLimit}}

	_, cmds := m.detectTransitions(newLoops, time.Now())

	if len(cmds) != 0 {
		t.Errorf("got %d auto-redrive cmds, want 0 — already rate-limited last scan, not a fresh edge", len(cmds))
	}
}

// ── autoRedriveScheduledMsg: re-check at fire time ────────────────────────

func TestUpdate_AutoRedriveScheduledMsg_StillRateLimited_FiresRedrive(t *testing.T) {
	withFakeActuationSeams(t, nil, func(sessionID, prompt string) error { return nil })
	m := New()
	m.loops = []domain.Loop{{SessionID: "s1", Project: "aboard", State: domain.StateStalled, Stall: domain.StallRateLimit}}

	m, cmd := updateModel(t, m, autoRedriveScheduledMsg{sessionID: "s1"})

	if cmd == nil {
		t.Fatal("expected a non-nil tea.Cmd (autoRedrive429Cmd)")
	}
	if m.autoRedriveAttempts["s1"] != 1 {
		t.Errorf("autoRedriveAttempts[s1] = %d, want 1", m.autoRedriveAttempts["s1"])
	}
}

func TestUpdate_AutoRedriveScheduledMsg_Recovered_SkipsFiring(t *testing.T) {
	m := New()
	m.loops = []domain.Loop{{SessionID: "s1", Project: "aboard", State: domain.StateRunning}} // recovered — no longer rate-limited

	m, cmd := updateModel(t, m, autoRedriveScheduledMsg{sessionID: "s1"})

	if cmd != nil {
		t.Error("expected nil — the loop recovered before the delayed redrive fired")
	}
}

func TestUpdate_AutoRedriveScheduledMsg_LoopGone_SkipsFiring(t *testing.T) {
	m := New()
	m.loops = nil // the session aged out of the fleet entirely

	m, cmd := updateModel(t, m, autoRedriveScheduledMsg{sessionID: "s1"})

	if cmd != nil {
		t.Error("expected nil — the session is no longer in the fleet at all")
	}
}

func TestUpdate_AutoRedriveScheduledMsg_NowGateOrFailed_SkipsFiring(t *testing.T) {
	m := New()
	m.loops = []domain.Loop{{SessionID: "s1", Project: "aboard", State: domain.StateGate}} // hit a gate during the delay
	m, cmd := updateModel(t, m, autoRedriveScheduledMsg{sessionID: "s1"})
	if cmd != nil {
		t.Error("expected nil — no longer StateStalled/StallRateLimit")
	}
}

// ── P1 review fix: auto-redrive joins the m.actuating interlock ─────────

func TestUpdate_AutoRedriveScheduledMsg_ManualRedriveInFlight_Skips(t *testing.T) {
	// A manual "r"/"i" resume already in flight for this session (e.g. the
	// human pressed r just before the scheduled tick fired) must make the
	// auto-redrive skip — not race a second concurrent Tier-2 send.
	m := New()
	m.loops = []domain.Loop{{SessionID: "s1", Project: "aboard", State: domain.StateStalled, Stall: domain.StallRateLimit}}
	m.actuating = map[string]bool{"s1": true} // simulates a manual resume already dispatched

	m, cmd := updateModel(t, m, autoRedriveScheduledMsg{sessionID: "s1"})

	if cmd != nil {
		t.Error("expected nil — a manual actuation is already in flight for this session")
	}
	if m.autoRedriveAttempts["s1"] != 0 {
		t.Errorf("autoRedriveAttempts[s1] = %d, want 0 — the skipped attempt must not count against the ceiling", m.autoRedriveAttempts["s1"])
	}
}

func TestUpdate_AutoRedriveScheduledMsg_Fires_SetsActuating(t *testing.T) {
	withFakeActuationSeams(t, nil, func(sessionID, prompt string) error { return nil })
	m := New()
	m.loops = []domain.Loop{{SessionID: "s1", Project: "aboard", State: domain.StateStalled, Stall: domain.StallRateLimit}}

	m, cmd := updateModel(t, m, autoRedriveScheduledMsg{sessionID: "s1"})

	if cmd == nil {
		t.Fatal("expected a non-nil tea.Cmd")
	}
	if !m.actuating["s1"] {
		t.Error("expected s1 to be marked actuating once the auto-redrive is dispatched")
	}
}

// TestUpdate_RKey_AutoRedriveInFlight_ManualResumeRefuses proves the
// interlock works in the OTHER direction: once an auto-redrive has set
// m.actuating, the EXISTING manual "r"-key guard (which already checks
// m.actuating before dispatching resumeCmd) now sees it and refuses — no
// change needed to that guard itself, just to what sets the flag.
func TestUpdate_RKey_AutoRedriveInFlight_ManualResumeRefuses(t *testing.T) {
	m := modelWithOneLoop()
	m.loops[0].State = domain.StateStalled
	m.loops[0].Stall = domain.StallRateLimit
	m.actuating = map[string]bool{"sess-1": true} // simulates an auto-redrive already dispatched

	m, cmd := updateModel(t, m, runeKey('r'))

	if cmd != nil {
		t.Error("expected no tea.Cmd — an auto-redrive is already in flight for this session")
	}
	if !strings.Contains(m.status, "already re-driving") {
		t.Errorf("status = %q, want the already-re-driving message", m.status)
	}
}

func TestUpdate_AutoRedriveResultMsg_ClearsActuatingInterlock(t *testing.T) {
	m := New()
	m.actuating = map[string]bool{"s1": true}

	m, _ = updateModel(t, m, autoRedriveResultMsg{sessionID: "s1", project: "aboard", attempt: 1, ok: true})

	if m.actuating["s1"] {
		t.Error("expected s1's actuating flag cleared once the auto-redrive result arrives")
	}
}

// ── autoRedrive429Cmd: event emission + exhausted notification ───────────

func TestAutoRedrive429Cmd_RecordsEventWithActorAuto(t *testing.T) {
	historyDir := t.TempDir()
	origHistoryDir := historyDirFn
	defer func() { historyDirFn = origHistoryDir }()
	historyDirFn = func() string { return historyDir }
	origRedrive := redriveFn
	defer func() { redriveFn = origRedrive }()
	redriveFn = func(sessionID, prompt string) error { return nil }

	l := domain.Loop{SessionID: "s1", Project: "aboard", State: domain.StateStalled, Stall: domain.StallRateLimit}
	autoRedrive429Cmd(l, 1)()

	got, err := events.ReadAll(historyDir)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	evs := got["s1"]
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1: %#v", len(evs), evs)
	}
	ev := evs[0]
	if ev.Trigger != events.TriggerActuation {
		t.Errorf("Trigger = %v, want TriggerActuation", ev.Trigger)
	}
	if ev.Actor != events.ActorAuto {
		t.Errorf("Actor = %v, want ActorAuto (unattended — distinct from every human actuation)", ev.Actor)
	}
	if ev.Detail != "auto-redrive-429 attempt 1/3" {
		t.Errorf("Detail = %q, want the exact literal format", ev.Detail)
	}
}

// TestAutoRedrive429Cmd_NeverSendsNotificationDirectly is the P2 review
// fix's structural regression: the exhaustion-notification DECISION moved
// to Update's autoRedriveResultMsg handler (keyed on the ceiling, via the
// shouldNotify dedup ledger, which only a Model method can mutate) —
// autoRedrive429Cmd itself must never call notifySendFn, regardless of
// attempt number or outcome.
func TestAutoRedrive429Cmd_NeverSendsNotificationDirectly(t *testing.T) {
	historyDirFn = func() string { return t.TempDir() }
	origRedrive := redriveFn
	defer func() { redriveFn = origRedrive }()
	origNotify := notifySendFn
	defer func() { notifySendFn = origNotify }()
	notifyCalled := false
	notifySendFn = func(title, body string) error { notifyCalled = true; return nil }

	l := domain.Loop{SessionID: "s1", Project: "aboard", State: domain.StateStalled, Stall: domain.StallRateLimit}
	for _, outcome := range []error{nil, errTestJudgeFailed} {
		redriveFn = func(sessionID, prompt string) error { return outcome }
		for attempt := 1; attempt <= autoRedriveMaxAttempts; attempt++ {
			autoRedrive429Cmd(l, attempt)()
		}
	}
	if notifyCalled {
		t.Error("autoRedrive429Cmd must never call notifySendFn directly, at any attempt or outcome")
	}
}

// ── autoRedriveResultMsg: exhaustion keyed on the ceiling, not err ────────

func TestUpdate_AutoRedriveResultMsg_FinalAttemptSuccess_StillNotifiesExhausted(t *testing.T) {
	// The P2 review fix's core case: the common exhaustion scenario is the
	// FINAL attempt sending just fine (ok=true) and the loop simply
	// staying rate-limited — the old err!=nil-only check left this
	// completely silent. Deliberately does NOT invoke the returned cmd —
	// that would call the real notify.Send (osascript) unless overridden;
	// TestAutoRedriveExhaustedNotifyCmd_SendsCorrectTitleAndBody already
	// covers the cmd's own behavior with notifySendFn properly stubbed.
	m := New()
	_, cmd := updateModel(t, m, autoRedriveResultMsg{sessionID: "s1", project: "aboard", attempt: autoRedriveMaxAttempts, ok: true})

	if cmd == nil {
		t.Fatal("expected a non-nil tea.Cmd (autoRedriveExhaustedNotifyCmd) even though the final attempt succeeded")
	}
}

func TestUpdate_AutoRedriveResultMsg_FinalAttemptFailure_NotifiesExhausted(t *testing.T) {
	m := New()
	_, cmd := updateModel(t, m, autoRedriveResultMsg{sessionID: "s1", project: "aboard", attempt: autoRedriveMaxAttempts, ok: false})
	if cmd == nil {
		t.Fatal("expected a non-nil tea.Cmd (autoRedriveExhaustedNotifyCmd)")
	}
}

func TestUpdate_AutoRedriveResultMsg_NonFinalAttempt_NoNotification(t *testing.T) {
	m := New()
	_, cmd1 := updateModel(t, m, autoRedriveResultMsg{sessionID: "s1", project: "aboard", attempt: 1, ok: false})
	if cmd1 != nil {
		t.Error("expected nil — attempt 1 of 3 is not the ceiling, no exhaustion yet")
	}
	m2 := New()
	_, cmd2 := updateModel(t, m2, autoRedriveResultMsg{sessionID: "s1", project: "aboard", attempt: 1, ok: true})
	if cmd2 != nil {
		t.Error("expected nil — attempt 1 of 3, even on success, is not the ceiling")
	}
}

func TestUpdate_AutoRedriveResultMsg_DedupedNotifyOnlyOnce(t *testing.T) {
	m := New()
	m, cmd1 := updateModel(t, m, autoRedriveResultMsg{sessionID: "s1", project: "aboard", attempt: autoRedriveMaxAttempts, ok: false})
	if cmd1 == nil {
		t.Fatal("expected the first exhaustion to notify")
	}
	_, cmd2 := updateModel(t, m, autoRedriveResultMsg{sessionID: "s1", project: "aboard", attempt: autoRedriveMaxAttempts, ok: false})
	if cmd2 != nil {
		t.Error("expected nil — a second exhaustion report for the SAME session must not re-notify (shouldNotify's dedup ledger)")
	}
}

func TestAutoRedriveExhaustedNotifyCmd_SendsCorrectTitleAndBody(t *testing.T) {
	origNotify := notifySendFn
	defer func() { notifySendFn = origNotify }()
	var gotTitle, gotBody string
	notifySendFn = func(title, body string) error {
		gotTitle, gotBody = title, body
		return nil
	}

	autoRedriveExhaustedNotifyCmd("aboard")()

	if gotTitle != notifyTitlePrefix+"missionctl · auto-redrive exhausted" {
		t.Errorf("title = %q, want the exhausted title", gotTitle)
	}
	if gotBody != "aboard" {
		t.Errorf("body = %q, want the project label", gotBody)
	}
}
