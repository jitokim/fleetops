package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/jitokim/fleetops/internal/claude"
	"github.com/jitokim/fleetops/internal/control"
	"github.com/jitokim/fleetops/internal/domain"
	"github.com/jitokim/fleetops/internal/engine"
	"github.com/jitokim/fleetops/internal/events"
	"github.com/jitokim/fleetops/internal/registry"
	"github.com/jitokim/fleetops/internal/sessions"
	runewidth "github.com/mattn/go-runewidth"
)

// TestMain is this package's safety net against the real
// ~/.fleetops/history: feat/detail-panel-v2's detailPanelLines reads it
// (via events.Read) on EVERY m.View() call, and this file has many tests
// that call View() without any reason to care about history data at all.
// Defaulting historyDirFn to a deliberately-nonexistent path here means
// every such test is hermetic by default — reads simply find nothing
// (events.Read tolerates a missing file, same as a missing dir) — while
// tests that DO need specific history data still override historyDirFn
// themselves (see withFakeActuationSeams and others), same
// save-then-restore pattern as always.
func TestMain(m *testing.M) {
	historyDirFn = func() string { return filepath.Join(os.TempDir(), "fleetops-tui-tests-unused-history") }
	// Same hermeticity net for the persisted hide-set: New() loads it, and
	// many tests call New() with no reason to care about a real captain's
	// ~/.fleetops/hidden.json. Point it at a per-run temp file (missing at
	// start → fail-open empty); hide/delete tests that need real persistence
	// override hiddenFileFn themselves (save-then-restore).
	hiddenFileFn = func() string {
		return filepath.Join(os.TempDir(), "fleetops-tui-tests-unused-hidden.json")
	}
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
	got := manualAttachHint("/home/user/myproject")
	want := "cd /home/user/myproject"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestPagerCmd(t *testing.T) {
	got := pagerCmd("/x/sess.jsonl")
	want := []string{"less", "-R", "+G", "-M", "-PMfleetops log — q to return (%pB\\%)", "/x/sess.jsonl"}
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
		{Project: "myproject", SessionID: "ccc3"},
	}
	dup := duplicateLabels(loops)
	if !dup["sessions"] {
		t.Error(`dup["sessions"] = false, want true (shared by 2 loops)`)
	}
	if dup["myproject"] {
		t.Error(`dup["myproject"] = true, want false (unique label)`)
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
// columns (marker+NAME+STATE[+CYCLE][+ORACLE][+LAST]) must sum to <=
// innerWidth — the same "prove it fits, don't just assume the thresholds
// line up" guarantee F1 established for the old columnWidths.
// TestListRowWidths_NeverOverflows sweeps from wMarker+wState (the
// structural floor this row format needs — marker+STATE alone, see
// listRowWidths' doc) up to a very wide panel: the row must never exceed
// innerWidth. Below that floor marker+STATE alone already overflow no
// matter what listRowWidths returns for NAME — an acknowledged edge, not a
// guaranteed one (same spirit as F1's own "not fully guaranteed under ~40
// cols" caveat for the old columnWidths).
func TestListRowWidths_NeverOverflows(t *testing.T) {
	for innerWidth := wMarker + wState; innerWidth <= 200; innerWidth++ {
		wName, showCycle, showOracle, showLast := listRowWidths(innerWidth)
		sum := wMarker + wName + wState
		if showCycle {
			sum += wCycle
		}
		if showOracle {
			sum += wOracle
		}
		if showLast {
			sum += wLast
		}
		if sum > innerWidth {
			t.Errorf("innerWidth=%d: sum = %d (wName=%d cycle=%v oracle=%v last=%v), want <= %d",
				innerWidth, sum, wName, showCycle, showOracle, showLast, innerWidth)
		}
	}
}

// TestListRowWidths_DropOrder_OracleThenCycleThenLast is feat/panel-info's
// exact specified degradation order: as width shrinks, ORACLE
// drops FIRST, then CYCLE, then LAST — never any other order, never two
// dropped at once when dropping one alone would fit.
//
// feat/loop-display-name moved ORACLE/CYCLE's drop threshold from the
// physical floor (listNameFloor) to the readability floor (nameGoodWidth)
// — see listRowWidths' doc — so "full" here means "all three columns AND a
// readable label"; LAST alone keeps the physical threshold.
func TestListRowWidths_DropOrder_OracleThenCycleThenLast(t *testing.T) {
	full := wMarker + wState + wCycle + wOracle + wLast + nameGoodWidth
	_, showCycle, showOracle, showLast := listRowWidths(full)
	if !showCycle || !showOracle || !showLast {
		t.Fatalf("precondition failed: at full width want all three shown, got cycle=%v oracle=%v last=%v", showCycle, showOracle, showLast)
	}

	// one step narrower than "all three fit" — ORACLE (the least
	// essential) must be the one to go, CYCLE and LAST both survive.
	_, showCycle, showOracle, showLast = listRowWidths(full - 1)
	if showOracle {
		t.Error("showOracle = true with insufficient room, want false (ORACLE drops first)")
	}
	if !showCycle || !showLast {
		t.Errorf("got cycle=%v last=%v, want both still shown once only ORACLE was dropped", showCycle, showLast)
	}

	// narrow enough that CYCLE can't keep the label readable either —
	// CYCLE goes next, LAST still survives alone.
	tight := wMarker + wState + wLast + listNameFloor
	_, showCycle, showOracle, showLast = listRowWidths(tight)
	if showOracle || showCycle {
		t.Errorf("got cycle=%v oracle=%v, want both dropped at this width", showCycle, showOracle)
	}
	if !showLast {
		t.Error("showLast = false, want true — LAST is the last of the three to drop")
	}

	// narrower still — even LAST alone doesn't fit.
	_, showCycle, showOracle, showLast = listRowWidths(wMarker + wState + listNameFloor - 1)
	if showCycle || showOracle || showLast {
		t.Errorf("got cycle=%v oracle=%v last=%v, want all three dropped at this width", showCycle, showOracle, showLast)
	}
}

// TestListRowWidths_ReadabilityFloor_ProtectsLabelOverOracleCycle pins the
// feature's own contract: at a width where everything WOULD physically fit
// but only with a fragment-sized label, ORACLE/CYCLE yield their room to
// NAME instead (the 100-col-terminal case: innerWidth 48 used to give
// NAME 7).
func TestListRowWidths_ReadabilityFloor_ProtectsLabelOverOracleCycle(t *testing.T) {
	wName, showCycle, showOracle, showLast := listRowWidths(48)
	if showOracle || showCycle {
		t.Errorf("got cycle=%v oracle=%v, want both dropped in favor of a readable label", showCycle, showOracle)
	}
	if !showLast {
		t.Error("showLast = false, want true (LAST keeps its physical-fit-only rule)")
	}
	if wName < nameGoodWidth {
		t.Errorf("wName = %d, want >= nameGoodWidth (%d)", wName, nameGoodWidth)
	}
}

// TestListRowWidths_NameWithinBounds: NAME never exceeds its cap, and never
// goes below listNameFloor once there's actually enough room for it (at
// innerWidth=1 there manifestly isn't — see TestListRowWidths_NeverOverflows'
// doc on the structural floor this row format needs).
func TestListRowWidths_NameWithinBounds(t *testing.T) {
	for _, innerWidth := range []int{wMarker + wState + listNameFloor, 40, 100, 300} {
		wName, _, _, _ := listRowWidths(innerWidth)
		if wName < listNameFloor {
			t.Errorf("innerWidth=%d: wName=%d, want >= listNameFloor (%d)", innerWidth, wName, listNameFloor)
		}
		if wName > nameCapWidth {
			t.Errorf("innerWidth=%d: wName=%d, want <= nameCapWidth (%d)", innerWidth, wName, nameCapWidth)
		}
	}
}

// ── feat/panel-info: FLEET's ORACLE×N column ─────────────────────────────

func TestOracleCompactLabel_NeverJudged_Dash(t *testing.T) {
	l := domain.Loop{Goal: domain.Goal{Text: "goal"}} // bound, Last==nil
	if got := oracleCompactLabel(l, 0); got != "—" {
		t.Errorf("got %q, want %q", got, "—")
	}
}

func TestOracleCompactLabel_Unbound_Dash(t *testing.T) {
	l := domain.Loop{} // no goal at all, Last==nil
	if got := oracleCompactLabel(l, 0); got != "—" {
		t.Errorf("got %q, want %q", got, "—")
	}
}

func TestOracleCompactLabel_DoneVerdict_CheckGlyphTimesCount(t *testing.T) {
	l := domain.Loop{Last: &domain.Verdict{Outcome: domain.OutcomeDone}}
	if got := oracleCompactLabel(l, 3); got != "✓×3" {
		t.Errorf("got %q, want %q", got, "✓×3")
	}
}

func TestOracleCompactLabel_ProgressVerdict_CheckGlyphTimesCount(t *testing.T) {
	// progress shares the "✓" glyph with done — oracleLabel's own DETAIL
	// row does the same ("✓ progress" vs "✓ verified"); the compact FLEET
	// column doesn't have room to distinguish the two in a 6-col cell,
	// only rejected-vs-not.
	l := domain.Loop{Last: &domain.Verdict{Outcome: domain.OutcomeProgress}}
	if got := oracleCompactLabel(l, 2); got != "✓×2" {
		t.Errorf("got %q, want %q", got, "✓×2")
	}
}

func TestOracleCompactLabel_RejectedVerdict_CrossGlyphTimesCount(t *testing.T) {
	l := domain.Loop{Last: &domain.Verdict{Outcome: domain.OutcomeRejected}}
	if got := oracleCompactLabel(l, 5); got != "✗×5" {
		t.Errorf("got %q, want %q", got, "✗×5")
	}
}

// TestFleetOracleCountsCmd_CountsTriggerOracleEventsPerSession is
// fleetOracleCountsCmd's core contract: count TriggerOracle events per
// BOUND session, off the event loop, for the whole fleet in one pass.
func TestFleetOracleCountsCmd_CountsTriggerOracleEventsPerSession(t *testing.T) {
	dir := t.TempDir()
	origHistoryDir := historyDirFn
	defer func() { historyDirFn = origHistoryDir }()
	historyDirFn = func() string { return dir }

	for i, ev := range []events.Event{
		{TS: 1, SessionID: "s1", Trigger: events.TriggerOracle, Detail: "done at cycle 1: ok"},
		{TS: 2, SessionID: "s1", Trigger: events.TriggerScan, ToState: "idle"},
		{TS: 3, SessionID: "s1", Trigger: events.TriggerOracle, Detail: "rejected at cycle 2: no"},
		{TS: 4, SessionID: "s2", Trigger: events.TriggerOracle, Detail: "done at cycle 1: ok"},
	} {
		if err := events.Append(dir, ev); err != nil {
			t.Fatalf("Append #%d: %v", i, err)
		}
	}

	loops := []domain.Loop{
		{SessionID: "s1", Goal: domain.Goal{Text: "goal 1"}},
		{SessionID: "s2", Goal: domain.Goal{Text: "goal 2"}},
	}
	msg := fleetOracleCountsCmd(loops)()

	counts, ok := msg.(fleetOracleCountsMsg)
	if !ok {
		t.Fatalf("got %T, want fleetOracleCountsMsg", msg)
	}
	if counts["s1"] != 2 {
		t.Errorf("counts[s1] = %d, want 2", counts["s1"])
	}
	if counts["s2"] != 1 {
		t.Errorf("counts[s2] = %d, want 1", counts["s2"])
	}
}

// TestFleetOracleCountsCmd_UnboundLoop_SkippedEntirely: an unbound loop
// (no goal — was never, and will never be, judged) must not even trigger
// an event-log read, let alone appear in the result map.
func TestFleetOracleCountsCmd_UnboundLoop_SkippedEntirely(t *testing.T) {
	dir := t.TempDir()
	origHistoryDir := historyDirFn
	defer func() { historyDirFn = origHistoryDir }()
	historyDirFn = func() string { return dir }

	loops := []domain.Loop{{SessionID: "unbound-1"}} // Goal.Text == ""
	msg := fleetOracleCountsCmd(loops)()

	counts, ok := msg.(fleetOracleCountsMsg)
	if !ok {
		t.Fatalf("got %T, want fleetOracleCountsMsg", msg)
	}
	if _, present := counts["unbound-1"]; present {
		t.Error("expected unbound-1 to be entirely absent from the result map")
	}
}

// TestUpdate_LoopsMsg_DispatchesFleetOracleCountsCmd is the wiring check:
// a scan tick's batched cmd must include fleetOracleCountsCmd's work —
// mirrors TestUpdate_LoopsMsg_DispatchesDetailCacheCmd's shape exactly.
func TestUpdate_LoopsMsg_DispatchesFleetOracleCountsCmd(t *testing.T) {
	dir := t.TempDir()
	origHistoryDir := historyDirFn
	defer func() { historyDirFn = origHistoryDir }()
	historyDirFn = func() string { return dir }
	if err := events.Append(dir, events.Event{TS: 1, SessionID: "s1", Trigger: events.TriggerOracle, Detail: "done at cycle 1: ok"}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	m := New()
	// State: StateRunning (not StateIdle) — deliberately, so
	// triggerJudgments' policy does NOT ALSO decide this loop needs
	// judging and dispatch a REAL judgeCmd (unmocked judgeFn would shell
	// out to a real `claude` CLI when this test's batch loop below calls
	// every sub-cmd to inspect its type). fleetOracleCountsCmd itself
	// doesn't care about State, only Goal.Text != "" — see its doc.
	l := domain.Loop{Project: "myproject", SessionID: "s1", State: domain.StateRunning, Goal: domain.Goal{Text: "goal"}}
	_, cmd := m.Update(loopsMsg([]domain.Loop{l}))
	if cmd == nil {
		t.Fatal("expected a non-nil batched cmd from loopsMsg")
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatalf("expected loopsMsg's cmd to be a tea.Batch, got %T", cmd())
	}
	found := false
	for _, sub := range batch {
		if sub == nil {
			continue
		}
		if counts, ok := sub().(fleetOracleCountsMsg); ok && counts["s1"] == 1 {
			found = true
		}
	}
	if !found {
		t.Error("expected loopsMsg's batched cmds to include fleetOracleCountsCmd's result for s1")
	}
}

// TestUpdate_FleetOracleCountsMsg_PopulatesCache mirrors the gitStatsMsg/
// detailCacheMsg handlers' own shape.
func TestUpdate_FleetOracleCountsMsg_PopulatesCache(t *testing.T) {
	m := New()
	updated, cmd := m.Update(fleetOracleCountsMsg{"s1": 3, "s2": 0})
	mm := updated.(Model)
	if cmd != nil {
		t.Error("expected no follow-up cmd from fleetOracleCountsMsg")
	}
	if mm.fleetOracleCounts["s1"] != 3 {
		t.Errorf("fleetOracleCounts[s1] = %d, want 3", mm.fleetOracleCounts["s1"])
	}
}

// TestFleetPanelLines_ShowsCycleAndOracleColumns_AtGenerousWidth: an
// end-to-end check that the new columns actually render through
// fleetPanelLines, not just that the lower-level helpers work in
// isolation.
func TestFleetPanelLines_ShowsCycleAndOracleColumns_AtGenerousWidth(t *testing.T) {
	m := New()
	m.loops = []domain.Loop{
		{Project: "myproject", SessionID: "s1", State: domain.StateRunning, Cycle: 4,
			Goal: domain.Goal{Text: "goal", MaxCycles: 12}, Last: &domain.Verdict{Outcome: domain.OutcomeDone}},
	}
	m.fleetOracleCounts = map[string]int{"s1": 3}
	m.cursor = 0

	lines := m.fleetPanelLines(120, 10)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "4/12") {
		t.Errorf("expected the CYCLE column (4/12) present, got:\n%s", joined)
	}
	if !strings.Contains(joined, "✓×3") {
		t.Errorf("expected the ORACLE×N column (✓×3) present, got:\n%s", joined)
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
			Project: "very-long-project-label-example", SessionID: "abcd1234", ProjectDir: "-x-a",
			Cwd: "/home/user/very-long-label", Path: "/home/user/.claude/projects/-x-a/abcd1234.jsonl",
			State: domain.StateRunning, Cycle: 6,
			Goal:         domain.Goal{Text: "add pagination to the search results endpoint and cache it", MaxCycles: 12, BudgetTokens: 200000},
			TokensSpent:  64000,
			LastActivity: now.Add(-30 * time.Second),
			Note:         "⚠ over budget please look",
			LastText:     "I added this feature and ran the tests. All tests passed.",
		},
		{
			Project: "voc-triage", SessionID: "sess00001", ProjectDir: "-x-b",
			Cwd: "/home/user/voc-triage", Path: "/home/user/.claude/projects/-x-b/sess00001.jsonl",
			State: domain.StateGate, GatePrompt: "The reinstall finished. Continue?",
			LastActivity: now.Add(-2 * time.Minute),
		},
		{
			Project: "flaky-hunt", SessionID: "drift001", ProjectDir: "-x-c",
			Cwd: "/home/user/flaky-hunt", Path: "/home/user/.claude/projects/-x-c/drift001.jsonl",
			State: domain.StateDrift, Cycle: 3,
			Goal:         domain.Goal{Text: "fix the flaky auth test", MaxCycles: 12},
			Last:         &domain.Verdict{Outcome: domain.OutcomeRejected, Reason: "no evidence of a passing test run shown, claim unsubstantiated"},
			LastActivity: now.Add(-10 * time.Minute),
		},
		{
			Project: "asre", SessionID: "idle0001", ProjectDir: "-x-d",
			Cwd: "/home/user/orca/projects/asre", Path: "/home/user/.claude/projects/-x-d/idle0001.jsonl",
			State:        domain.StateIdle,
			LastActivity: now.Add(-1 * time.Hour),
		},
		{
			Project: "dotfiles", SessionID: "fail0001", ProjectDir: "-x-e",
			Cwd: "/home/user/dotfiles", Path: "/home/user/.claude/projects/-x-e/fail0001.jsonl",
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
// cmd/fleetops/main.go runs in tea.WithAltScreen() mode, where content
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
		Cwd: "/home/user/v2-regress", Path: transcriptPath, CwdVerified: true,
		State: domain.StateStalled, Stall: domain.StallRateLimit, Cycle: 3,
		Goal:         domain.Goal{Text: "fix the flaky auth test", MaxCycles: 12, BudgetTokens: 2_000_000},
		TokensSpent:  1_200_000,
		Last:         &domain.Verdict{Outcome: domain.OutcomeProgress, Reason: "made partial progress, one test still failing intermittently under load"},
		LastActivity: now,
		LastText:     "still working on stabilizing the flaky test",
		BoundAt:      now.Add(-50 * time.Minute),
	}
}

// primeDetailCache runs the REAL detailCacheCmd synchronously and returns
// its result as a ready-to-assign m.detailCache map — fix/exit-gate-ux
// moved the DETAIL panel's event-log read and transcript LAST ERROR parse
// off View() into an async Model-cache (mirroring gitStatsCmd), so any
// test that drives m.View() directly (bypassing Update()'s loopsMsg
// dispatch) must warm the cache itself first, exactly like this helper
// does — otherwise the DETAIL panel would just see the safe "not computed
// yet" zero value and silently render nothing new.
func primeDetailCache(t *testing.T, l domain.Loop) map[string]detailCacheEntry {
	t.Helper()
	msg, ok := detailCacheCmd(l)().(detailCacheMsg)
	if !ok {
		t.Fatalf("detailCacheCmd(%q) did not return a detailCacheMsg", l.SessionID)
	}
	return map[string]detailCacheEntry{msg.sessionID: msg.entry}
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
				m.detailCache = primeDetailCache(t, l) // fix/exit-gate-ux: events+LAST ERROR are now async-cached, not read synchronously by View()

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
	m.detailCache = primeDetailCache(t, l) // fix/exit-gate-ux: events+LAST ERROR are now async-cached, not read synchronously by View()
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

// TestHeaderHintColumnCount_DropsColumnsAsAvailShrinks pins the column
// count at representative (totalWidth, availForHints) pairs — hint columns
// must drop right-to-left as the space renderHeaderBlock hands them
// shrinks, reaching 0 either below headerHintMinWidth OR whenever
// availForHints itself is 0 (fix/exit-gate-ux: availForHints is now
// computed by renderHeaderBlock AFTER giving LEFT/MIDDLE priority — see
// its doc — so this function no longer derives it from width itself).
func TestHeaderHintColumnCount_DropsColumnsAsAvailShrinks(t *testing.T) {
	cases := []struct {
		totalWidth, avail, want int
	}{
		{headerHintMinWidth - 1, 1000, 0}, // below the total-width floor — whole grid dropped regardless of avail
		{headerHintMinWidth, 0, 0},        // MIDDLE claimed everything — no room left for hints
		{headerHintMinWidth, headerHintColWidth, 1},
		{300, headerHintColWidth * 100, 4}, // capped at the number of columns headerHintKeys actually needs
	}
	for _, c := range cases {
		if got := headerHintColumnCount(c.totalWidth, c.avail); got != c.want {
			t.Errorf("headerHintColumnCount(%d, %d) = %d, want %d", c.totalWidth, c.avail, got, c.want)
		}
	}
}

// TestHeaderHintColumnCount_MonotonicallyNonDecreasingInAvail: columns must
// never drop as availForHints GROWS (no oscillation), for a fixed
// totalWidth above the minimum threshold.
func TestHeaderHintColumnCount_MonotonicallyNonDecreasingInAvail(t *testing.T) {
	prev := -1
	for avail := 0; avail <= 300; avail++ {
		got := headerHintColumnCount(300, avail)
		if got < prev {
			t.Fatalf("avail=%d: cols=%d, want >= previous avail's %d (must not decrease as avail grows)", avail, got, prev)
		}
		prev = got
	}
}

// ── fix/exit-gate-ux: header width-priority order (UX judge items 2+3) ───

// headerHeavyModel builds a Model whose fleet-stats band is long enough to
// have previously truncated at ~80 cols (the judge's exact repro: "fleet 10
// · 1 ru…", "budget 7.4M · o…") — 10 loops across every counted state, real
// token spend, and a judged verdict so both MIDDLE lines are non-trivial,
// plus gated>0 so the attention badge is also present.
func headerHeavyModel() Model {
	m := New()
	m.hostname = "host"
	loops := []domain.Loop{
		{Project: "p1", SessionID: "s1", State: domain.StateRunning, TokensSpent: 1_000_000},
		{Project: "p2", SessionID: "s2", State: domain.StateGate, TokensSpent: 1_000_000},
		{Project: "p3", SessionID: "s3", State: domain.StateStalled, TokensSpent: 1_000_000},
		{Project: "p4", SessionID: "s4", State: domain.StateStalled, TokensSpent: 1_000_000},
		{Project: "p5", SessionID: "s5", State: domain.StateStalled, TokensSpent: 1_000_000},
		{Project: "p6", SessionID: "s6", State: domain.StateIdle, TokensSpent: 1_000_000},
		{Project: "p7", SessionID: "s7", State: domain.StateIdle, TokensSpent: 1_000_000,
			Last: &domain.Verdict{Outcome: domain.OutcomeDone}},
		{Project: "p8", SessionID: "s8", State: domain.StateDone, TokensSpent: 400_000},
		{Project: "p9", SessionID: "s9", State: domain.StateFailed},
		{Project: "p10", SessionID: "s10", State: domain.StateKilled},
	}
	m.loops = loops
	return m
}

// TestRenderHeaderBlock_StatsSurviveBeforeHintsAt80Cols is the judge's
// exact live repro, fixed: at 80 cols, a heavy fleet-stats band must
// render FULLY (no "…" truncation) — hint columns give up room first.
func TestRenderHeaderBlock_StatsSurviveBeforeHintsAt80Cols(t *testing.T) {
	m := headerHeavyModel()
	out := renderHeaderBlock(m, 80)
	if strings.Contains(out, "…") {
		t.Errorf("expected the fleet-stats band to render in full at 80 cols (hints must drop first), got:\n%s", out)
	}
	if !strings.Contains(out, "fleet 10") || !strings.Contains(out, "budget 7.4M") || !strings.Contains(out, "oracle") {
		t.Errorf("expected the full fleet-stats content to appear at 80 cols, got:\n%s", out)
	}
}

// TestRenderHeaderBlock_HintsDropBeforeStatsTruncate: as width shrinks from
// a generous value down to 80, the hint grid's column count must shrink
// (or vanish) BEFORE the fleet-stats band ever loses a character — the
// exact priority flip the judge asked for.
func TestRenderHeaderBlock_HintsDropBeforeStatsTruncate(t *testing.T) {
	m := headerHeavyModel()
	wideCols := headerHintColumnCountForModel(m, 175)
	tightCols := headerHintColumnCountForModel(m, 80)
	if tightCols >= wideCols {
		t.Errorf("expected fewer hint columns at 80 cols (%d) than at 175 cols (%d) — hints must degrade before stats", tightCols, wideCols)
	}
	out := renderHeaderBlock(m, 80)
	if strings.Contains(out, "…") {
		t.Errorf("stats truncated at 80 cols before hints were fully dropped:\n%s", out)
	}
}

// headerHintColumnCountForModel renders the header and counts how many
// "<" hint-chip openers appear in the FIRST hint row — a black-box way to
// read back how many columns actually rendered, without exposing
// renderHeaderBlock's internal width-allocation math to the test.
func headerHintColumnCountForModel(m Model, width int) int {
	out := renderHeaderBlock(m, width)
	lines := strings.Split(out, "\n")
	if len(lines) == 0 {
		return 0
	}
	return strings.Count(lines[0], "<")
}

// TestRenderHeaderBlock_BadgeNeverTruncates is UX judge item 3's direct
// regression: with a GATE pending, the "▲ N GATE NEEDS YOU" badge (or its
// abbreviated "▲N GATE" fallback) must NEVER be ansi-truncated with "…" —
// it's the sole attention cue in narrow/list-only mode — across a sweep
// down to a realistically narrow width.
func TestRenderHeaderBlock_BadgeNeverTruncates(t *testing.T) {
	m := headerHeavyModel() // gated=1
	for _, width := range []int{45, 50, 60, 70, 80, 90, 120, 175} {
		out := renderHeaderBlock(m, width)
		if !strings.Contains(out, "GATE") {
			t.Errorf("width=%d: expected the GATE badge to appear somewhere in the header:\n%s", width, out)
			continue
		}
		if strings.Contains(out, "GATE…") || strings.Contains(out, "▲…") {
			t.Errorf("width=%d: badge appears truncated:\n%s", width, out)
		}
	}
}

// TestRenderHeaderBlock_BadgeFallsBackToAbbreviatedForm_WhenTight: at a
// width too tight for the full "▲ N GATE NEEDS YOU" form but wide enough
// for the abbreviated "▲N GATE" form, the abbreviated form must render
// (not the full form clipped, and not nothing).
func TestRenderHeaderBlock_BadgeFallsBackToAbbreviatedForm_WhenTight(t *testing.T) {
	m := New()
	m.hostname = "h"
	m.loops = []domain.Loop{{Project: "p", SessionID: "s", State: domain.StateGate}}
	out := renderHeaderBlock(m, headerLeftWidth+9) // room for "▲1 GATE" (~8 cols) but not "▲ 1 GATE NEEDS YOU" (~19 cols)
	if !strings.Contains(out, "▲1 GATE") {
		t.Errorf("expected the abbreviated badge form, got:\n%s", out)
	}
	if strings.Contains(out, "NEEDS YOU") {
		t.Errorf("expected the FULL badge form to be replaced, not shown truncated, got:\n%s", out)
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

// TestHeaderHintKeys_GroupedByFunction pins fix/exit-gate-ux item 7's
// reordering: column-major fill groups send actions (r/i/a), lifecycle
// (n/k/p), nav (↵/o//), and session housekeeping (q/d/x) into adjacent
// cells — see headerHintKeys' doc for the grouping rationale.
func TestHeaderHintKeys_GroupedByFunction(t *testing.T) {
	want := []string{"r", "i", "a", "n", "k", "p", "↵", "o", "/", "q", "d", "x"}
	if len(headerHintKeys) != len(want) {
		t.Fatalf("got %d keys, want %d", len(headerHintKeys), len(want))
	}
	for i, k := range headerHintKeys {
		if k.key != want[i] {
			t.Errorf("headerHintKeys[%d] = %q, want %q", i, k.key, want[i])
		}
	}
}

// TestRenderHeaderHintGrid_AllKeysPresent_NothingRegressed verifies every
// keybinding the old bottom keybar used to show still appears somewhere in
// the hint grid at a generous width — the item-7 reorder above must not
// have dropped anything.
func TestRenderHeaderHintGrid_AllKeysPresent_NothingRegressed(t *testing.T) {
	out := renderHeaderHintGrid(4, 4*headerHintColWidth)
	for _, k := range []string{"r", "a", "i", "↵", "p", "k", "n", "o", "/", "q", "d", "x"} {
		if !strings.Contains(out, "<"+k+">") {
			t.Errorf("expected hint grid to contain key %q, got:\n%s", k, out)
		}
	}
}

// TestRenderHeaderLeft_HostnameNotTruncatedAtTypicalLength verifies
// headerLeftWidth is generous enough for a realistic hostname (the
// regression this constant's own doc comment cites) — a real bug caught
// empirically while building this feature (an earlier, narrower
// headerLeftWidth truncated "workstation.local").
func TestRenderHeaderLeft_HostnameNotTruncatedAtTypicalLength(t *testing.T) {
	m := New()
	m.hostname = "workstation.local"
	out := renderHeaderLeft(m, headerLeftWidth)
	if strings.Contains(out, "…") {
		t.Errorf("hostname line got truncated at headerLeftWidth=%d:\n%s", headerLeftWidth, out)
	}
	if !strings.Contains(out, m.hostname) {
		t.Errorf("expected the full hostname to appear untruncated, got:\n%s", out)
	}
}

// ── fix/exit-gate-ux: empty-state onboarding (UX judge item 5) ───────────

// TestFleetPanelLines_EmptyFleet_ShowsOnboardingHint: the empty FLEET
// panel used to dead-end a new user with just a fact ("no active Claude
// Code loops in the window.") — it must also point at the two most likely
// next actions: spawning a loop, and installing the hooks gate detection
// depends on.
func TestFleetPanelLines_EmptyFleet_ShowsOnboardingHint(t *testing.T) {
	m := New()
	lines := m.fleetPanelLines(80, 10)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "no active Claude Code loops") {
		t.Errorf("expected the empty-fleet fact to still be present:\n%s", joined)
	}
	if !strings.Contains(joined, "press n to spawn a loop") {
		t.Errorf("expected a hint to spawn a loop with n:\n%s", joined)
	}
	if !strings.Contains(joined, "fleetops hooks install") {
		t.Errorf("expected a hint to install hooks for gate detection:\n%s", joined)
	}
}

// TestFleetPanelLines_EmptyFilterResult_NoOnboardingHint: the OTHER empty
// state — a filter that matches nothing — is a different situation (loops
// exist, the filter just excludes all of them) and must NOT show the
// onboarding hint, which would be misleading there.
func TestFleetPanelLines_EmptyFilterResult_NoOnboardingHint(t *testing.T) {
	m := New()
	m.loops = []domain.Loop{{Project: "myproject", SessionID: "s1", State: domain.StateRunning}}
	m.filterQuery = "no-such-match-xyz"
	lines := m.fleetPanelLines(80, 10)
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "press n to spawn") {
		t.Errorf("did not expect the onboarding hint for a filter-empty result:\n%s", joined)
	}
	if !strings.Contains(joined, "no loops match filter") {
		t.Errorf("expected the filter-empty message:\n%s", joined)
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
//
// feat/panel-info: the distinguishing digits are FIRST in the name
// ("l037", not "loop-037") — with the FLEET row's new CYCLE/ORACLE columns
// eating into the NAME budget at narrow widths, a shared 6-char prefix
// ("loop-0", common to loop-000..loop-099) truncated identically for
// every row, making TestView_SelectedRowVisibleInFleetPanel's width=45
// case unable to tell rows apart at all. Putting the unique part first
// means even an aggressively truncated NAME column still shows it.
func manyLoopsForScrollTest(n int) []domain.Loop {
	now := time.Now()
	out := make([]domain.Loop, n)
	for i := 0; i < n; i++ {
		out[i] = domain.Loop{
			Project:      fmt.Sprintf("l%03d", i),
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
	m.loops = []domain.Loop{{Project: "myproject", SessionID: "sess-1", ProjectDir: "-x-myproject", Cwd: "/x/myproject", CwdVerified: true, State: domain.StateRunning}}
	m.cursor = 0
	return m
}

func TestUpdate_NKey_UsesLaunchDirNotSelectedLoop(t *testing.T) {
	// The spawn base must be the dir fleetops runs in — NEVER silently
	// inherited from whatever loop the list cursor happens to sit on (that
	// inheritance is how a new loop ended up in an unrelated repo's
	// worktree). The selected loop's dir is still reachable, but only via
	// wizardWhere's explicit [s] choice.
	m := modelWithOneLoop()

	m, _ = updateModel(t, m, runeKey('n'))

	if m.mode != modePrompting {
		t.Fatalf("mode = %v, want modePrompting", m.mode)
	}
	wd, _ := os.Getwd()
	if m.spawnCwd != wd {
		t.Errorf("spawnCwd = %q, want os.Getwd() %q — not the selected loop's Cwd", m.spawnCwd, wd)
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

func TestUpdate_NKey_UnverifiedSelectionNeverLeaksIntoSpawnCwd(t *testing.T) {
	// P1-3 still holds under the launch-dir default: an unverified Cwd (a
	// lossy decode of ProjectDir) must never become the spawn base — and
	// with inheritance gone entirely, neither does a verified one.
	m := New()
	m.loops = []domain.Loop{{Project: "myproject", SessionID: "sess-1", Cwd: "/x/myproject", CwdVerified: false, State: domain.StateStalled}}
	m.cursor = 0

	m, _ = updateModel(t, m, runeKey('n'))

	if m.spawnCwd == "/x/myproject" {
		t.Error("expected spawnCwd NOT to use the selected loop's Cwd")
	}
	if m.spawnCwd == "" {
		t.Error("expected spawnCwd to fall back to a non-empty cwd (os.Getwd)")
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

func TestUpdate_NoDoneCondition_NudgeSurfacesInStatusOnSubmit(t *testing.T) {
	// the nudge must actually reach the user — View() replaces the status
	// line with the prompt while prompting, so it can only surface at the
	// submit status message (which now fires at the wizardWhere step, after
	// the 5 free-text answers).
	m := New()
	m.loops = []domain.Loop{{Project: "myproject", SessionID: "sess-1", Cwd: "/x/myproject", CwdVerified: false, State: domain.StateStalled}}
	m.cursor = 0
	m, _ = updateModel(t, m, runeKey('n'))

	m, _ = typeAndEnter(t, m, "goal") // step 1: goal
	m, _ = typeAndEnter(t, m, "")     // step 2: name, skipped
	m, _ = typeAndEnter(t, m, "")     // step 3: done-when, skipped
	m, _ = typeAndEnter(t, m, "")     // step 4: oracle, skipped
	m, _ = typeAndEnter(t, m, "")     // step 5: challenger, skipped
	m, _ = typeAndEnter(t, m, "")     // step 6: max_iteration, default
	if m.spawnStep != wizardWhere {
		t.Fatalf("spawnStep = %v, want wizardWhere before the final submit", m.spawnStep)
	}
	m, cmd := updateModel(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // where: default (current dir — ineligible backend)

	if cmd == nil {
		t.Fatal("expected a non-nil tea.Cmd (spawnCmd)")
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
	// esc must cancel the wizard regardless of which of the 6 steps is
	// currently active.
	steps := []struct {
		name    string
		answers []string // typed+entered before esc
	}{
		{"step1_goal", nil},
		{"step2_name", []string{"goal"}},
		{"step3_doneWhen", []string{"goal", ""}},
		{"step4_oracle", []string{"goal", "", ""}},
		{"step5_challenger", []string{"goal", "", "", ""}},
		{"step6_maxCycles", []string{"goal", "", "", "", ""}},
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
	m, _ = typeAndEnter(t, m, "bugfix loop")                // step 2: name
	m, _ = typeAndEnter(t, m, "tests pass")                 // step 3: done when
	m, _ = typeAndEnter(t, m, "run go test ./...")          // step 4: oracle
	m, _ = typeAndEnter(t, m, "try to break it with -race") // step 5: challenger
	m, _ = typeAndEnter(t, m, "20")                         // step 6: max cycles
	if m.spawnStep != wizardWhere {
		t.Fatalf("spawnStep = %v, want wizardWhere before the final submit", m.spawnStep)
	}
	m, cmd := updateModel(t, m, runeKey('d')) // where: this dir

	if m.mode != modeNormal {
		t.Errorf("mode = %v, want modeNormal after the full wizard", m.mode)
	}
	if cmd == nil {
		t.Fatal("expected a non-nil tea.Cmd (spawnCmd)")
	}
	if m.spawnGoal != "fix the bug" {
		t.Errorf("spawnGoal = %q, want %q", m.spawnGoal, "fix the bug")
	}
	if m.spawnName != "bugfix loop" {
		t.Errorf("spawnName = %q, want %q", m.spawnName, "bugfix loop")
	}
	if m.spawnDoneWhen != "tests pass" {
		t.Errorf("spawnDoneWhen = %q, want %q", m.spawnDoneWhen, "tests pass")
	}
	if m.spawnRubric != "run go test ./..." {
		t.Errorf("spawnRubric = %q, want %q", m.spawnRubric, "run go test ./...")
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
	m, _ = typeAndEnter(t, m, "")            // step 2: name — skipped
	m, _ = typeAndEnter(t, m, "")            // step 3: done when — skipped
	m, _ = typeAndEnter(t, m, "")            // step 4: rubric — skipped
	m, _ = typeAndEnter(t, m, "")            // step 5: challenger — skipped

	// each of steps 2-4 returns textinput.Blink (a non-nil cmd) to advance
	// to the next question — only the mode/step, not cmd-nilness, indicates
	// whether the wizard has actually submitted yet.
	if m.mode != modePrompting || m.spawnStep != wizardMaxCycles {
		t.Fatalf("expected to be sitting at step 5 (max cycles), got mode=%v step=%v", m.mode, m.spawnStep)
	}
	if m.spawnDoneWhen != "" || m.spawnRubric != "" || m.spawnChallenger != "" {
		t.Errorf("got doneWhen=%q rubric=%q challenger=%q, want all empty (skipped)", m.spawnDoneWhen, m.spawnRubric, m.spawnChallenger)
	}

	m, _ = typeAndEnter(t, m, "") // step 5: max cycles — default
	if m.mode != modePrompting || m.spawnStep != wizardWhere {
		t.Fatalf("got mode=%v step=%v, want modePrompting at wizardWhere after step 5", m.mode, m.spawnStep)
	}
	m, cmd := updateModel(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // where: default
	if cmd == nil {
		t.Fatal("expected a non-nil tea.Cmd (spawnCmd) once the where step is answered")
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
		t.Fatalf("mode = %v, want modePrompting (5 more steps to go)", m.mode)
	}
	if m.spawnStep != wizardName {
		t.Fatalf("spawnStep = %v, want wizardName", m.spawnStep)
	}

	// steps 2-6 all skipped/defaulted, then the where step's enter submits.
	m, _ = typeAndEnter(t, m, "")
	m, _ = typeAndEnter(t, m, "")
	m, _ = typeAndEnter(t, m, "")
	m, _ = typeAndEnter(t, m, "")
	m, _ = typeAndEnter(t, m, "")
	m, cmd := updateModel(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // wizardWhere: default

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
	m.pendingKillAt = time.Now().Add(-destructiveConfirmWindow - time.Second) // simulate the window having expired

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
	l := domain.Loop{SessionID: "sess-1", Project: "myproject", State: domain.StateKilled}
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
		func(sessionsDir, sessionID, projectDir string) (control.Actuator, bool, bool) {
			return &fakeActuator{backend: "tmux"}, true, true
		},
		nil,
	)
	l := domain.Loop{SessionID: "sess-1", Project: "myproject", State: domain.StateDrift}
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
		func(sessionsDir, sessionID, projectDir string) (control.Actuator, bool, bool) {
			return &fakeActuator{backend: "tmux"}, true, true
		},
		nil,
	)
	l := domain.Loop{SessionID: "sess-1", Project: "myproject", State: domain.StateDrift}
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
	l := domain.Loop{SessionID: "sess-1", Project: "myproject", State: domain.StateKilled}

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
		{Project: "myproject", SessionID: "sess-1", ProjectDir: "-x-myproject", Cwd: "/x/myproject", CwdVerified: true, State: domain.StateStalled},
		{Project: "myproject", SessionID: "sess-2", ProjectDir: "-x-myproject", Cwd: "/x/myproject", CwdVerified: true, State: domain.StateStalled},
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

// TestUpdate_RKey_AmbiguousSharedDir_MessageIsActionable pins Bug 2's Option
// B honesty fix: the ambiguity refusal is reached specifically because none
// of the loops sharing this directory has a session-registry tty (see
// ttyPathPlausible's doc — a registry tty would have routed through the
// session-unique Tier 1a path instead of ever reaching this cwd-based
// guard). The message must name the actual fix (`fleetops hooks install`),
// not just the manual-attach workaround.
func TestUpdate_RKey_AmbiguousSharedDir_MessageIsActionable(t *testing.T) {
	m := modelWithTwoLoopsSharingDir()

	m, _ = updateModel(t, m, runeKey('r'))

	if !strings.Contains(m.status, "session-registry tty") {
		t.Errorf("status = %q, want it to explain no session has a registry tty", m.status)
	}
	if !strings.Contains(m.status, "fleetops hooks install") {
		t.Errorf("status = %q, want it to point at the actual fix (fleetops hooks install)", m.status)
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
	fakeCtrl := &fakeActuator{backend: "tmux"}
	withFakeActuationSeams(t,
		func(sessionsDir, sessionID, projectDir string) (control.Actuator, bool, bool) {
			return fakeCtrl, true, true
		},
		nil,
	)
	path := writeTranscriptLastUserPrompt(t, "fix the flaky auth test")
	l := domain.Loop{SessionID: "sess-1", Project: "myproject", State: domain.StateDrift, Path: path}

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
	fakeCtrl := &fakeActuator{backend: "tmux"}
	withFakeActuationSeams(t,
		func(sessionsDir, sessionID, projectDir string) (control.Actuator, bool, bool) {
			return fakeCtrl, true, true
		},
		nil,
	)
	path := writeTranscriptLastUserPrompt(t, "fix the flaky auth test")
	l := domain.Loop{SessionID: "sess-1", Project: "myproject", State: domain.StateDrift, Path: path}

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
	l := domain.Loop{SessionID: "sess-1", Project: "myproject", State: domain.StateFailed}

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

// TestUpdate_IKey_AmbiguousSharedDir_StalledLoop_EntersInjectingModeForHeadlessFallback
// pins feat/inject-headless-exact-fallback: a STALLED loop is on
// injectHeadlessFallbackEligible's allowlist, so an ambiguous cwd (no
// session-registry tty — e.g. orca, which has no CLI tty/tab-by-session
// mapping at all) no longer dead-ends here. The human can still type a
// prompt; sendPromptCmd's existing Tier1→Tier2 fallthrough (unchanged)
// routes it to the exact session_id via headless redrive — see
// TestSendPromptCmd_TierOneNotFound_DowngradeMessage_ExplainsWhy for the
// resulting status message. This test supersedes the old
// TestUpdate_IKey_AmbiguousSharedDir_Refuses (same fixture is StateStalled
// by default) — see TestUpdate_IKey_AmbiguousSharedDir_RunningLoop_StillRefuses
// immediately below for the negative case that's still refused.
func TestUpdate_IKey_AmbiguousSharedDir_StalledLoop_EntersInjectingModeForHeadlessFallback(t *testing.T) {
	m := modelWithTwoLoopsSharingDir() // both loops StateStalled by default

	m, cmd := updateModel(t, m, runeKey('i'))

	if cmd == nil {
		t.Fatal("expected a non-nil tea.Cmd (textinput.Blink) — a stalled loop is eligible for the headless fallback, must not refuse")
	}
	if m.mode != modeInjecting {
		t.Errorf("mode = %v, want modeInjecting", m.mode)
	}
	if m.statusKind == statusErr {
		t.Errorf("statusKind = %v, want not statusErr (must not be refused)", m.statusKind)
	}
}

// TestUpdate_IKey_AmbiguousSharedDir_RunningLoop_StillRefuses pins the
// NEGATIVE case of the same feature: a RUNNING loop is NOT on
// injectHeadlessFallbackEligible's allowlist (a live mid-turn `claude
// --resume` risks conflicting with/forking the transcript — see its doc),
// so an ambiguous cwd still dead-ends exactly as before this feature.
func TestUpdate_IKey_AmbiguousSharedDir_RunningLoop_StillRefuses(t *testing.T) {
	m := modelWithTwoLoopsSharingDir()
	m.loops[0].State = domain.StateRunning
	m.loops[1].State = domain.StateRunning
	// Defense in depth: redriveFn must never even be REACHABLE for this
	// case — cmd==nil already proves nothing was dispatched, but fail loudly
	// if some future change accidentally wires a path to it anyway.
	origRedrive := redriveFn
	t.Cleanup(func() { redriveFn = origRedrive })
	redriveFn = func(sessionID, prompt string) error {
		t.Fatal("redriveFn must not be called — a running loop must never get the headless fallback")
		return nil
	}

	m, cmd := updateModel(t, m, runeKey('i'))

	if cmd != nil {
		t.Error("expected no tea.Cmd — a running loop must never get the headless fallback")
	}
	if m.mode != modeNormal {
		t.Errorf("mode = %v, want modeNormal (ambiguous running loop must not enter inject mode)", m.mode)
	}
	if m.statusKind != statusErr {
		t.Errorf("statusKind = %v, want statusErr", m.statusKind)
	}
	if !strings.Contains(m.status, "ambiguous") {
		t.Errorf("status = %q, want it to mention the ambiguity", m.status)
	}
}

// TestUpdate_IKey_StalledAmbiguous_FullRoundTrip_RoutesToExactSessionIDHeadlessly
// is feat/inject-headless-exact-fallback's end-to-end proof: press i on an
// ambiguous STALLED loop (no session-registry tty — the orca case), type a
// prompt, submit, and verify the dispatched injectCmd actually re-drives the
// SELECTED loop's EXACT session_id (never its sibling's) via control.Redrive,
// and the resulting status names that exact session as a background turn.
func TestUpdate_IKey_StalledAmbiguous_FullRoundTrip_RoutesToExactSessionIDHeadlessly(t *testing.T) {
	m := modelWithTwoLoopsSharingDir() // both StateStalled, share ProjectDir, no tty entries — cursor 0 = sess-1
	var tier1Called bool
	var redriveSessionID, redrivePrompt string
	withFakeActuationSeams(t,
		func(sessionsDir, sessionID, projectDir string) (control.Actuator, bool, bool) {
			tier1Called = true
			return nil, true, false // backend resolves but can't disambiguate — the orca/cwd-chain outcome
		},
		func(sessionID, prompt string) error {
			redriveSessionID, redrivePrompt = sessionID, prompt
			return nil
		},
	)

	m, cmd := updateModel(t, m, runeKey('i'))
	if m.mode != modeInjecting {
		t.Fatalf("precondition failed: mode = %v, want modeInjecting", m.mode)
	}

	m, cmd = typeAndEnter(t, m, "run the tests again")
	if cmd == nil {
		t.Fatal("expected a non-nil tea.Cmd (injectCmd)")
	}

	msg := cmd()
	m, _ = updateModel(t, m, msg)

	if !tier1Called {
		t.Error("expected Tier 1 to have been attempted (and to have failed to disambiguate) before falling to Tier 2")
	}
	if redriveSessionID != "sess-1" {
		t.Errorf("redriveFn called with sessionID %q, want %q (the SELECTED loop's exact session_id, not a guess)", redriveSessionID, "sess-1")
	}
	if redrivePrompt != "run the tests again" {
		t.Errorf("redriveFn called with prompt %q, want the typed prompt", redrivePrompt)
	}
	rm, ok := msg.(resumeResultMsg)
	if !ok || !rm.ok {
		t.Fatalf("got %+v, want a successful resumeResultMsg", msg)
	}
	if rm.sessionID != "sess-1" {
		t.Errorf("resumeResultMsg.sessionID = %q, want %q", rm.sessionID, "sess-1")
	}
	if !strings.Contains(m.status, "background turn") {
		t.Errorf("status = %q, want the background-turn notice", m.status)
	}
	if !strings.Contains(m.status, shortID("sess-1")) {
		t.Errorf("status = %q, want it to name the exact session %s", m.status, shortID("sess-1"))
	}
}

// TestUpdate_IKey_ResolvableInPlace_FullRoundTrip_UsesTierOneNotHeadless is
// the unchanged-behavior pin: when Tier 1 CAN resolve a target (an
// unambiguous single loop, or the tty path), inject still goes in-place —
// the headless fallback is reached ONLY when Tier 1 genuinely can't
// disambiguate, never as a shortcut around a resolvable target.
func TestUpdate_IKey_ResolvableInPlace_FullRoundTrip_UsesTierOneNotHeadless(t *testing.T) {
	m := modelWithOneLoop() // single loop — not ambiguous, so the eligibility gate is never even consulted
	ctrl := &fakeActuator{backend: "orca"}
	var redriveCalled bool
	withFakeActuationSeams(t,
		func(sessionsDir, sessionID, projectDir string) (control.Actuator, bool, bool) {
			return ctrl, true, true
		},
		func(sessionID, prompt string) error { redriveCalled = true; return nil },
	)

	m, cmd := updateModel(t, m, runeKey('i'))
	if m.mode != modeInjecting {
		t.Fatalf("precondition failed: mode = %v, want modeInjecting", m.mode)
	}

	m, cmd = typeAndEnter(t, m, "keep going")
	if cmd == nil {
		t.Fatal("expected a non-nil tea.Cmd (injectCmd)")
	}
	msg := cmd()
	m, _ = updateModel(t, m, msg)

	if !ctrl.resumeCalled {
		t.Error("expected Tier 1's ctrl.Resume to have been called — a resolvable target must go in-place")
	}
	if redriveCalled {
		t.Error("expected redriveFn NOT to be called — Tier 1 resolved, headless fallback must not fire")
	}
	rm, ok := msg.(resumeResultMsg)
	if !ok || !rm.ok {
		t.Fatalf("got %+v, want a successful in-place resumeResultMsg", msg)
	}
	if !strings.Contains(rm.text, "via orca") {
		t.Errorf("text = %q, want the in-place success message naming the backend", rm.text)
	}
	if !strings.Contains(m.status, "injected into") || !strings.Contains(m.status, "via orca") {
		t.Errorf("status = %q, want the in-place success status text (not the headless-fallback wording)", m.status)
	}
}

// TestUpdate_InjectSubmit_TargetWentRunningWhileTyping_Refuses pins the
// submit-time re-check: eligibility is judged at "i" keypress, but fleet
// refresh messages still flow while the human types (modeInjecting only
// captures KEYS) — if the Idle/Stalled target goes Running mid-typing,
// Enter must NOT headlessly re-drive into the now-live interactive turn.
// sendPromptCmd's Tier1→Tier2 fallthrough is unconditional, so this
// submit-time guard is the only protection against that race.
func TestUpdate_InjectSubmit_TargetWentRunningWhileTyping_Refuses(t *testing.T) {
	m := modelWithTwoLoopsSharingDir() // cursor 0 = sess-1, StateStalled (eligible at keypress)
	var redriveCalled bool
	withFakeActuationSeams(t,
		func(sessionsDir, sessionID, projectDir string) (control.Actuator, bool, bool) {
			return nil, true, false
		},
		func(sessionID, prompt string) error {
			redriveCalled = true
			return nil
		},
	)

	m, _ = updateModel(t, m, runeKey('i'))
	if m.mode != modeInjecting {
		t.Fatalf("precondition failed: mode = %v, want modeInjecting (Stalled ambiguous is eligible)", m.mode)
	}

	// Mid-typing fleet rescan: sess-1 is now mid-turn (Running), still
	// ambiguous with sess-2, still no registry tty.
	m, _ = updateModel(t, m, loopsMsg([]domain.Loop{
		{Project: "myproject", SessionID: "sess-1", ProjectDir: "-x-myproject", Cwd: "/x/myproject", CwdVerified: true, State: domain.StateRunning},
		{Project: "myproject", SessionID: "sess-2", ProjectDir: "-x-myproject", Cwd: "/x/myproject", CwdVerified: true, State: domain.StateStalled},
	}))

	m, cmd := typeAndEnter(t, m, "keep going")
	if cmd != nil {
		t.Error("expected no tea.Cmd — the submit-time re-check must refuse, not dispatch")
	}
	if redriveCalled {
		t.Error("redriveFn must NOT be called for a target that went Running while typing")
	}
	if m.statusKind != statusErr {
		t.Errorf("statusKind = %v, want statusErr", m.statusKind)
	}
	if m.mode != modeNormal {
		t.Errorf("mode = %v, want modeNormal after the refusal", m.mode)
	}
}

// TestUpdate_InjectSubmit_AmbiguousEligible_InterimStatusSaysHeadless pins
// the honest interim status (same discipline as the Tier 2 result message):
// an ambiguous-but-eligible target is all but certain to route headlessly,
// so the "working…" status must not imply an in-place injection the open
// window will never show.
func TestUpdate_InjectSubmit_AmbiguousEligible_InterimStatusSaysHeadless(t *testing.T) {
	m := modelWithTwoLoopsSharingDir()
	withFakeActuationSeams(t,
		func(sessionsDir, sessionID, projectDir string) (control.Actuator, bool, bool) {
			return nil, true, false
		},
		func(sessionID, prompt string) error { return nil },
	)

	m, _ = updateModel(t, m, runeKey('i'))
	m, cmd := typeAndEnter(t, m, "run the tests again")
	if cmd == nil {
		t.Fatal("expected a non-nil tea.Cmd (injectCmd)")
	}
	if !strings.Contains(m.status, "headlessly (tier 2)") {
		t.Errorf("interim status = %q, want it to say the prompt is routing headlessly (tier 2)", m.status)
	}
	if m.statusKind != statusNeutral {
		t.Errorf("statusKind = %v, want statusNeutral for the interim status", m.statusKind)
	}
}

// modelWithThreeOrcaLoopsSharingWorktree models the real motivating case for
// this feature: orca has no CLI tty/tab-by-session mapping at all
// (confirmed live — see docs/adr-vendor-independent-actuation.md §4 and
// orcaController's deliberate lack of a TTYLocator implementation), so three
// sessions sharing one worktree can never be disambiguated via a
// session-registry tty — only via this feature's exact-session_id headless
// fallback (for the eligible ones) or a dead-end refusal (for the rest).
func modelWithThreeOrcaLoopsSharingWorktree() Model {
	m := New()
	m.loops = []domain.Loop{
		{Project: "orca-repo", SessionID: "orca-sess-1", ProjectDir: "-x-orca-repo", Cwd: "/x/orca-repo", CwdVerified: true, State: domain.StateIdle},
		{Project: "orca-repo", SessionID: "orca-sess-2", ProjectDir: "-x-orca-repo", Cwd: "/x/orca-repo", CwdVerified: true, State: domain.StateStalled},
		{Project: "orca-repo", SessionID: "orca-sess-3", ProjectDir: "-x-orca-repo", Cwd: "/x/orca-repo", CwdVerified: true, State: domain.StateRunning},
	}
	m.cursor = 0
	return m
}

// TestUpdate_IKey_OrcaThreeSessionsOneWorktree_IdleSelected_RoutesHeadlessly
// exercises the orca-flavored fixture's ELIGIBLE case (idle) end-to-end.
func TestUpdate_IKey_OrcaThreeSessionsOneWorktree_IdleSelected_RoutesHeadlessly(t *testing.T) {
	m := modelWithThreeOrcaLoopsSharingWorktree() // cursor 0 = orca-sess-1, StateIdle
	var redriveSessionID string
	withFakeActuationSeams(t,
		func(sessionsDir, sessionID, projectDir string) (control.Actuator, bool, bool) {
			return nil, true, false // orca resolves as a backend, but the 3-way cwd match can't disambiguate (LocateClaude's own >1 refusal)
		},
		func(sessionID, prompt string) error { redriveSessionID = sessionID; return nil },
	)

	m, cmd := updateModel(t, m, runeKey('i'))
	if m.mode != modeInjecting {
		t.Fatalf("precondition failed: mode = %v, want modeInjecting (idle is eligible for the fallback)", m.mode)
	}

	_, cmd = typeAndEnter(t, m, "keep going")
	if cmd == nil {
		t.Fatal("expected a non-nil tea.Cmd (injectCmd)")
	}
	cmd()

	if redriveSessionID != "orca-sess-1" {
		t.Errorf("redriveFn called with sessionID %q, want the SELECTED loop's exact id %q (not orca-sess-2 or orca-sess-3)", redriveSessionID, "orca-sess-1")
	}
}

// TestUpdate_IKey_OrcaThreeSessionsOneWorktree_RunningSelected_StillRefuses
// exercises the orca-flavored fixture's INELIGIBLE case (running) — same
// three-session worktree, but the running one still dead-ends.
func TestUpdate_IKey_OrcaThreeSessionsOneWorktree_RunningSelected_StillRefuses(t *testing.T) {
	m := modelWithThreeOrcaLoopsSharingWorktree()
	m.cursor = 2 // orca-sess-3, StateRunning

	m, cmd := updateModel(t, m, runeKey('i'))

	if cmd != nil {
		t.Error("expected no tea.Cmd — running is never eligible for the fallback, even on orca")
	}
	if m.mode != modeNormal {
		t.Errorf("mode = %v, want modeNormal", m.mode)
	}
}

// TestInjectHeadlessFallbackEligible_ExplicitAllowlist exhaustively pins the
// fail-closed allowlist: only StateIdle/StateStalled are eligible, every
// other domain.LoopState value (including hypothetical future ones — this
// is why it's an allowlist, not "everything except StateRunning") stays
// ineligible by default.
func TestInjectHeadlessFallbackEligible_ExplicitAllowlist(t *testing.T) {
	cases := []struct {
		state domain.LoopState
		want  bool
	}{
		{domain.StateIdle, true},
		{domain.StateStalled, true},
		{domain.StateRunning, false},
		{domain.StateGate, false},
		{domain.StateDrift, false},
		{domain.StateDone, false},
		{domain.StateFailed, false},
		{domain.StatePaused, false},
		{domain.StateKilled, false},
		{domain.LoopState("some-future-state"), false},
	}
	for _, c := range cases {
		if got := injectHeadlessFallbackEligible(c.state); got != c.want {
			t.Errorf("injectHeadlessFallbackEligible(%q) = %v, want %v", c.state, got, c.want)
		}
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
	l := domain.Loop{SessionID: "sess-1", Project: "myproject", State: domain.StateFailed}

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
// real ~/.fleetops/sessions or shelling out to tmux/claude.

// withFakeActuationSeams overrides resolveActuationTargetFn/redriveFn for
// the duration of one test, restoring the originals on cleanup.
// withFakeActuationSeams also overrides historyDirFn to a t.TempDir() — any
// test that reaches a real tier dispatch (success or failure) now also
// triggers logActuationEvent's events.Append call, which must never touch
// the real ~/.fleetops/history during `go test`.
func withFakeActuationSeams(t *testing.T, resolve func(sessionsDir, sessionID, projectDir string) (control.Actuator, bool, bool), redrive func(sessionID, prompt string) error) {
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
		func(sessionsDir, sessionID, projectDir string) (control.Actuator, bool, bool) {
			tier1Called = true
			return nil, true, true // would succeed if tried — must NOT be tried
		},
		func(sessionID, prompt string) error {
			gotSessionID, gotPrompt = sessionID, prompt
			return nil
		},
	)
	l := domain.Loop{SessionID: "sess-1", Project: "myproject", Stall: domain.StallGone}

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
	fakeCtrl := &fakeActuator{backend: "tmux"}
	withFakeActuationSeams(t,
		func(sessionsDir, sessionID, projectDir string) (control.Actuator, bool, bool) {
			return fakeCtrl, true, true
		},
		func(sessionID, prompt string) error {
			redriveCalled = true
			return nil
		},
	)
	l := domain.Loop{SessionID: "sess-1", Project: "myproject"}

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
		func(sessionsDir, sessionID, projectDir string) (control.Actuator, bool, bool) {
			return nil, true, false // backend resolved, but no surface located
		},
		func(sessionID, prompt string) error {
			redriveCalled = true
			return nil
		},
	)
	l := domain.Loop{SessionID: "sess-1", Project: "myproject"}

	msg := sendPromptCmd(l, "do the thing", "resume", "resumed", "")()

	if !redriveCalled {
		t.Error("expected redriveFn to be called once Tier 1 fails to find a surface")
	}
	rm, ok := msg.(resumeResultMsg)
	if !ok || !rm.ok {
		t.Fatalf("got %+v, want a successful resumeResultMsg", msg)
	}
}

// TestSendPromptCmd_TierOneHSendFails_FallsToTierTwoRedrive is the
// capability-regression guard for the iTerm2 Tier 1h slice.
//
// Tier 1h resolves OPTIMISTICALLY (the registry says the loop lives in an
// iTerm2 session) and only discovers a closed tab / moved tty at SEND time.
// Before 1h existed, such a loop resolved nothing and the prompt landed via
// Tier 2. Reporting the 1h failure as terminal would therefore have made "r"/
// "i" strictly WORSE for exactly the sessions this tier was added to serve.
//
// Safe precisely because a 1h failure never half-delivers (see
// control.IsHostSendTier), so the redrive cannot double-send.
func TestSendPromptCmd_TierOneHSendFails_FallsToTierTwoRedrive(t *testing.T) {
	for _, sendErr := range []error{
		control.ErrSendNoSession,    // the tab was closed
		control.ErrSendTTYMismatch,  // the session moved / tab recycled
		control.ErrNoSendSurface,    // refused before exec
		errors.New("exit status 1"), // osascript itself failed
	} {
		t.Run(sendErr.Error(), func(t *testing.T) {
			redriveCalled := false
			act := &fakeActuator{backend: "iterm2", tier: "tier1h", resumeErr: sendErr}
			withFakeActuationSeams(t,
				func(sessionsDir, sessionID, projectDir string) (control.Actuator, bool, bool) {
					return act, true, true
				},
				func(sessionID, prompt string) error { redriveCalled = true; return nil },
			)
			l := domain.Loop{SessionID: "sess-1", Project: "myproject"}

			msg := sendPromptCmd(l, "do the thing", "resume", "resumed", "")()

			if !act.resumeCalled {
				t.Fatal("Tier 1h was never attempted")
			}
			if !redriveCalled {
				t.Fatal("a failed Tier 1h send did not fall through to Tier 2 — capability regression")
			}
			rm, ok := msg.(resumeResultMsg)
			if !ok || !rm.ok {
				t.Fatalf("got %+v, want a successful (degraded) resumeResultMsg", msg)
			}
		})
	}
}

// TestSendPromptCmd_TierOneHTimeout_DoesNotRedrive is the double-delivery
// guard, and the one carve-out in the Tier 1h degrade.
//
// Every other 1h failure happens before osascript can write, so a redrive
// provably cannot duplicate anything. A DEADLINE kill is different in kind: the
// script was running and we stopped it, so the write may already have landed.
// Re-driving would then deliver the same prompt twice — a duplicate injection
// or re-send, with a transcript that reads as if the human pressed the key
// twice.
//
// The repo's rule is that fleetops never claims more than it knows, so this
// must resolve to neither "sent" nor a plain failure that quietly triggers a
// retry: the operator is told the outcome is UNKNOWN and decides.
func TestSendPromptCmd_TierOneHTimeout_DoesNotRedrive(t *testing.T) {
	redriveCalled := false
	timedOut := fmt.Errorf("%w (signal: killed)", control.ErrSendDeliveryUnknown)
	withFakeActuationSeams(t,
		func(sessionsDir, sessionID, projectDir string) (control.Actuator, bool, bool) {
			return &fakeActuator{backend: "iterm2", tier: "tier1h", resumeErr: timedOut}, true, true
		},
		func(sessionID, prompt string) error { redriveCalled = true; return nil },
	)
	l := domain.Loop{SessionID: "sess-1", Project: "myproject"}

	msg := sendPromptCmd(l, "do the thing", "inject", "injected into", "")()

	if redriveCalled {
		t.Fatal("a timed-out Tier 1h send fell through to Tier 2 — risks delivering the same prompt twice")
	}
	rm, ok := msg.(resumeResultMsg)
	if !ok {
		t.Fatalf("got %T, want resumeResultMsg", msg)
	}
	if rm.ok {
		t.Error("a timeout must not be reported as a success — nothing confirmed the write landed")
	}
	if !strings.Contains(rm.text, "UNKNOWN") {
		t.Errorf("text = %q, want it to say the delivery outcome is unknown", rm.text)
	}
	if !strings.Contains(rm.text, "Attach") {
		t.Errorf("text = %q, want it to tell the operator to go look before retrying", rm.text)
	}
}

// TestSendPromptCmd_TierOneMultiplexerSendFails_IsTerminal is the other half of
// the classification: a MULTIPLEXER send that fails must NOT be retried on
// Tier 2. `tmux send-keys` offers no fail-closed guarantee, so a redrive after
// it could deliver the same prompt twice.
func TestSendPromptCmd_TierOneMultiplexerSendFails_IsTerminal(t *testing.T) {
	redriveCalled := false
	act := &fakeActuator{backend: "tmux", resumeErr: errors.New("send-keys: no such pane")}
	withFakeActuationSeams(t,
		func(sessionsDir, sessionID, projectDir string) (control.Actuator, bool, bool) {
			return act, true, true
		},
		func(sessionID, prompt string) error { redriveCalled = true; return nil },
	)
	l := domain.Loop{SessionID: "sess-1", Project: "myproject"}

	msg := sendPromptCmd(l, "do the thing", "resume", "resumed", "")()

	if redriveCalled {
		t.Error("a failed multiplexer send fell through to Tier 2 — risks a double delivery")
	}
	rm, ok := msg.(resumeResultMsg)
	if !ok || rm.ok {
		t.Fatalf("got %+v, want a failed resumeResultMsg", msg)
	}
}

// TestSendPromptCmd_TierOneNotFound_DowngradeMessage_ExplainsWhy pins Bug 2's
// Option B honesty fix: a non-StallGone loop that downgrades from Tier 1 to
// Tier 2 (couldn't disambiguate the on-screen session — the common case with
// N>1 sessions sharing a cwd on a backend with no per-session tty dispatch,
// e.g. cmux/orca) must say WHY in its success message, not reuse StallGone's
// plain "output lands in the transcript" text — the human is watching a
// terminal window that may not visibly update.
func TestSendPromptCmd_TierOneNotFound_DowngradeMessage_ExplainsWhy(t *testing.T) {
	withFakeActuationSeams(t,
		func(sessionsDir, sessionID, projectDir string) (control.Actuator, bool, bool) {
			return nil, true, false // backend resolved, but no surface located
		},
		func(sessionID, prompt string) error { return nil },
	)
	l := domain.Loop{SessionID: "sess-1", Project: "myproject"} // Stall is zero-value, i.e. NOT StallGone

	msg := sendPromptCmd(l, "do the thing", "inject", "injected into", "")()

	rm, ok := msg.(resumeResultMsg)
	if !ok || !rm.ok {
		t.Fatalf("got %+v, want a successful resumeResultMsg", msg)
	}
	if !strings.Contains(rm.text, "couldn't target the on-screen session unambiguously") {
		t.Errorf("text = %q, want it to explain the Tier 1→2 downgrade", rm.text)
	}
	// feat/inject-headless-exact-fallback: the message now names the EXACT
	// session (shortID) the prompt was routed to, and calls it a
	// "background turn" explicitly — the honest-UX requirement for routing
	// an ambiguous inject to control.Redrive by exact session_id.
	if !strings.Contains(rm.text, "background turn") {
		t.Errorf("text = %q, want it to say the prompt landed as a background turn", rm.text)
	}
	if !strings.Contains(rm.text, shortID(l.SessionID)) {
		t.Errorf("text = %q, want it to name the exact session (%s) the prompt was delivered to", rm.text, shortID(l.SessionID))
	}
	if !strings.Contains(rm.text, "won't appear in the open window") {
		t.Errorf("text = %q, want it to warn the open window won't update", rm.text)
	}
}

// TestSendPromptCmd_StallGone_TierTwoMessage_UnchangedPlainText proves the
// downgrade explanation above is scoped to the non-StallGone case only — a
// StallGone loop's Tier 2 re-drive is its NORMAL path (there's no on-screen
// session to fail to disambiguate: the process is simply gone), so it must
// keep the original plain message, not the new "couldn't target
// unambiguously" wording (which would be misleading here).
func TestSendPromptCmd_StallGone_TierTwoMessage_UnchangedPlainText(t *testing.T) {
	withFakeActuationSeams(t,
		func(sessionsDir, sessionID, projectDir string) (control.Actuator, bool, bool) {
			t.Fatal("resolveActuationTargetFn must not be called for a StallGone loop")
			return nil, false, false
		},
		func(sessionID, prompt string) error { return nil },
	)
	l := domain.Loop{SessionID: "sess-1", Project: "myproject", Stall: domain.StallGone}

	msg := sendPromptCmd(l, "do the thing", "resume", "resumed", "")()

	rm, ok := msg.(resumeResultMsg)
	if !ok || !rm.ok {
		t.Fatalf("got %+v, want a successful resumeResultMsg", msg)
	}
	if strings.Contains(rm.text, "couldn't target the on-screen session unambiguously") {
		t.Errorf("text = %q, StallGone's normal Tier 2 path must not use the downgrade wording", rm.text)
	}
	if !strings.Contains(rm.text, "output lands in the transcript") {
		t.Errorf("text = %q, want the original plain Tier 2 message", rm.text)
	}
}

func TestSendPromptCmd_TierTwoRedriveFails_ReportsError(t *testing.T) {
	withFakeActuationSeams(t,
		func(sessionsDir, sessionID, projectDir string) (control.Actuator, bool, bool) {
			return nil, false, false
		},
		func(sessionID, prompt string) error {
			return errTestJudgeFailed
		},
	)
	l := domain.Loop{SessionID: "sess-1", Project: "myproject"}

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
		func(sessionsDir, sessionID, projectDir string) (control.Actuator, bool, bool) {
			// simulates: tty binding failed, AND the cwd chain's LocateClaude
			// refused internally because >1 loop matched that directory.
			return nil, true, false
		},
		func(sessionID, prompt string) error {
			redriveCalled = true
			return nil
		},
	)
	l := domain.Loop{SessionID: "sess-1", Project: "myproject"}

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
		func(sessionsDir, sessionID, projectDir string) (control.Actuator, bool, bool) {
			return nil, true, false // ambiguous cwd match, refused internally by LocateClaude
		},
		nil,
	)
	l := domain.Loop{SessionID: "sess-1", Project: "myproject", GateTS: 123}

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

// TestKPA_TierOneHTimeout_LeadsWithUncertaintyNotFailure covers the three verbs
// that have NO Tier 2. r/i can degrade to a headless redrive on an unknown
// delivery; k/p/a cannot, so they have to say the honest thing themselves.
//
// "kill X failed: <err>" was the bug: the prefix asserts a definite outcome
// while the error body says the outcome is unknown, and an operator scanning
// the status line reads the prefix. For `k` that reads as "press it again" —
// which reintroduces by hand exactly the double-send control.ErrSendDeliveryUnknown
// exists to prevent, just moved from automatic to manual.
func TestKPA_TierOneHTimeout_LeadsWithUncertaintyNotFailure(t *testing.T) {
	timedOut := fmt.Errorf("%w (signal: killed)", control.ErrSendDeliveryUnknown)

	cases := map[string]struct {
		loop    domain.Loop
		run     func(domain.Loop) tea.Msg
		text    func(tea.Msg) (string, bool)
		warnKey string
	}{
		"approve": {
			loop:    domain.Loop{SessionID: "sess-1", Project: "myproject", GateTS: 123},
			run:     func(l domain.Loop) tea.Msg { return approveCmd(l)() },
			text:    func(m tea.Msg) (string, bool) { r, ok := m.(approveResultMsg); return r.text, ok && r.ok },
			warnKey: "pressing a again",
		},
		"kill": {
			loop:    domain.Loop{SessionID: "sess-1", Project: "myproject"},
			run:     func(l domain.Loop) tea.Msg { return killCmd(l)() },
			text:    func(m tea.Msg) (string, bool) { r, ok := m.(killResultMsg); return r.text, ok && r.ok },
			warnKey: "pressing k again",
		},
		"interrupt": {
			loop:    domain.Loop{SessionID: "sess-1", Project: "myproject"},
			run:     func(l domain.Loop) tea.Msg { return interruptCmd(l)() },
			text:    func(m tea.Msg) (string, bool) { r, ok := m.(interruptResultMsg); return r.text, ok && r.ok },
			warnKey: "pressing p again",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			withFakeActuationSeams(t,
				func(sessionsDir, sessionID, projectDir string) (control.Actuator, bool, bool) {
					return &fakeActuator{
						backend:      "iterm2",
						tier:         "tier1h",
						resumeErr:    timedOut,
						approveErr:   timedOut,
						interruptErr: timedOut,
					}, true, true
				},
				nil,
			)

			text, okFlag := tc.text(tc.run(tc.loop))

			if okFlag {
				t.Error("an unknown delivery must not be reported as a success")
			}
			if !strings.Contains(text, "delivery UNKNOWN") {
				t.Errorf("text = %q, want it to LEAD with the uncertainty", text)
			}
			if strings.Contains(text, "failed") {
				t.Errorf("text = %q, must not assert a definite failure it cannot know", text)
			}
			if !strings.Contains(text, tc.warnKey) {
				t.Errorf("text = %q, want it to warn against %q", text, tc.warnKey)
			}
		})
	}
}

// TestKPA_OrdinaryTierOneFailure_StillSaysFailed: the carve-out is for unknown
// delivery ONLY. Every other failure provably delivered nothing, and softening
// those into hedged language would be the opposite overclaim — under-reporting
// a definite failure the operator needs to act on.
func TestKPA_OrdinaryTierOneFailure_StillSaysFailed(t *testing.T) {
	boom := control.ErrSendTTYMismatch
	withFakeActuationSeams(t,
		func(sessionsDir, sessionID, projectDir string) (control.Actuator, bool, bool) {
			return &fakeActuator{backend: "iterm2", tier: "tier1h", resumeErr: boom, approveErr: boom, interruptErr: boom}, true, true
		},
		nil,
	)

	km, ok := killCmd(domain.Loop{SessionID: "sess-1", Project: "myproject"})().(killResultMsg)
	if !ok || km.ok {
		t.Fatalf("got %+v, want a failed killResultMsg", km)
	}
	if !strings.Contains(km.text, "failed") {
		t.Errorf("text = %q, want a definite failure for a definite refusal", km.text)
	}
	if strings.Contains(km.text, "UNKNOWN") {
		t.Errorf("text = %q, must not hedge a failure we actually know about", km.text)
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
		func(sessionsDir, sessionID, projectDir string) (control.Actuator, bool, bool) {
			return nil, true, false
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
		func(sessionsDir, sessionID, projectDir string) (control.Actuator, bool, bool) {
			return nil, false, false // Tier 1 never resolves — every dispatch would reach Tier 2
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

	m, _ = updateModel(t, m, resumeResultMsg{sessionID: "sess-1", ok: true, text: "resumed myproject"})

	if m.actuating["sess-1"] {
		t.Error("expected sess-1 to be cleared from m.actuating once its result arrives")
	}
}

func TestUpdate_ResumeResultMsg_OnlyClearsMatchingSessionID(t *testing.T) {
	m := modelWithOneLoop()
	m.actuating = map[string]bool{"sess-1": true, "sess-2": true}

	m, _ = updateModel(t, m, resumeResultMsg{sessionID: "sess-1", ok: true, text: "resumed myproject"})

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
	m, _ = updateModel(t, m, resumeResultMsg{sessionID: "sess-1", ok: true, text: "resumed myproject"})

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
//
// locateCalled/locateClaudeCalled/focusCalled (feat/engine-driver's
// attach-preservation AC) track which methods attachCmd actually invokes,
// configurable via locateTarget/locateOK/focusErr — every OTHER existing
// test leaves these at their zero values (false/empty/nil), which
// reproduces the exact same stub behavior these methods always had.
// fakeActuator stands in for a resolved control.Actuator — what
// resolveActuationTargetFn hands back once resolution has bound a target. It
// replaces fakeController in the ACTUATION tests specifically: Actuator's
// methods are target-free (the binding happened during resolution), so a type
// cannot implement both Resume(Target, string) and Resume(string). fakeController
// stays for the ATTACH tests, which genuinely still exercise Locate/Focus on a
// control.Controller.
//
// tier defaults to control's "tier1" label so the many existing multiplexer
// tests keep asserting that tier without restating it. Tests that exercise the
// iTerm2 Tier 1h dispatch set it to "tier1h" — the label is what
// logActuationEvent records, and telling an in-place host write apart from a
// multiplexer send after the fact is the entire reason Actuator.Tier() exists.
type fakeActuator struct {
	backend string
	tier    string

	resumeCalled     bool
	resumeErr        error
	lastResumePrompt string // captures what Resume was actually sent, for asserting hint composition

	approveCalled   bool
	approveErr      error
	interruptCalled bool
	interruptErr    error
}

func (f *fakeActuator) Backend() string { return f.backend }
func (f *fakeActuator) Tier() string {
	if f.tier == "" {
		return "tier1"
	}
	return f.tier
}
func (f *fakeActuator) Resume(prompt string) error {
	f.resumeCalled = true
	f.lastResumePrompt = prompt
	return f.resumeErr
}
func (f *fakeActuator) Approve() error   { f.approveCalled = true; return f.approveErr }
func (f *fakeActuator) Interrupt() error { f.interruptCalled = true; return f.interruptErr }

type fakeController struct {
	name             string
	resumeCalled     bool
	resumeErr        error
	lastResumePrompt string // feat/drift-guided-redrive: captures what Resume was actually sent, for asserting hint composition

	locateCalled       bool
	locateClaudeCalled bool
	locateTarget       control.Target
	locateOK           bool
	focusCalled        bool
	focusErr           error
}

func (f *fakeController) Name() string                   { return f.name }
func (f *fakeController) Available() bool                { return true }
func (f *fakeController) Approve(control.Target) error   { return nil }
func (f *fakeController) Interrupt(control.Target) error { return nil }
func (f *fakeController) Spawn(string, string) error     { return nil }
func (f *fakeController) Locate(string) (control.Target, bool) {
	f.locateCalled = true
	return f.locateTarget, f.locateOK
}
func (f *fakeController) LocateClaude(string) (control.Target, bool) {
	f.locateClaudeCalled = true
	return control.Target{}, false
}
func (f *fakeController) Focus(control.Target) error {
	f.focusCalled = true
	return f.focusErr
}
func (f *fakeController) Resume(t control.Target, prompt string) error {
	f.resumeCalled = true
	f.lastResumePrompt = prompt
	return f.resumeErr
}

// ── attachCmd: the attach-preservation requirement ────────────────────────
//
// feat/engine-driver's seed spec locks a top-level AC: an OBSERVED loop's
// `↵` attach behavior must NEVER regress across any engine slice —
// unchanged Locate (not the stricter LocateClaude) → Focus, exactly as
// today. This is slice 1's only touch of attachCmd: a testability seam
// (controlResolveFn) with ZERO behavior change, so this pin can actually
// run hermetically instead of only against a real (never-available-in-CI)
// orca/tmux/cmux backend.

func withFakeControlResolve(t *testing.T, ctrl control.Controller, ok bool) {
	t.Helper()
	orig := controlResolveFn
	t.Cleanup(func() { controlResolveFn = orig })
	controlResolveFn = func() (control.Controller, bool) { return ctrl, ok }
}

// withFakeControlResolveForLocate fakes attachCmd's ATTACH-resolver seam. It
// DELEGATES to ctrl.Locate (never LocateClaude) exactly as the real
// control.ResolveForLocate does, so the attach-preservation pin
// (TestAttachCmd_ObservedLoop_UsesLocateNotLocateClaude) still observes the
// Locate-not-LocateClaude invariant through the fake controller after the seam
// split.
func withFakeControlResolveForLocate(t *testing.T, ctrl control.Controller, ok bool) {
	t.Helper()
	orig := controlResolveForLocateFn
	t.Cleanup(func() { controlResolveForLocateFn = orig })
	controlResolveForLocateFn = func(projectDir string) (control.Controller, control.Target, bool) {
		if !ok || ctrl == nil {
			return nil, control.Target{}, false
		}
		target, located := ctrl.Locate(projectDir)
		return ctrl, target, located
	}
}

func TestAttachCmd_ObservedLoop_UsesLocateNotLocateClaude(t *testing.T) {
	fakeCtrl := &fakeController{name: "tmux", locateTarget: control.Target{Backend: "tmux", ID: "%1"}, locateOK: true}
	withFakeControlResolveForLocate(t, fakeCtrl, true)
	l := domain.Loop{Project: "myproject", SessionID: "s1", ProjectDir: "-x-myproject", State: domain.StateRunning}

	msg := attachCmd(l)()

	if !fakeCtrl.locateCalled {
		t.Error("expected Locate to be called")
	}
	if fakeCtrl.locateClaudeCalled {
		t.Error("expected LocateClaude NOT to be called — attach uses the permissive Locate, same as before the engine existed")
	}
	if !fakeCtrl.focusCalled {
		t.Error("expected Focus to be called once Locate found a surface")
	}
	am, ok := msg.(attachResultMsg)
	if !ok || !am.ok {
		t.Fatalf("got %+v, want a successful attachResultMsg", msg)
	}
}

func TestAttachCmd_NoBackendAvailable_ManualHintFallback(t *testing.T) {
	withFakeControlResolveForLocate(t, nil, false)
	l := domain.Loop{Project: "myproject", SessionID: "s1", Cwd: "/home/user/myproject", State: domain.StateRunning}

	msg := attachCmd(l)()

	am, ok := msg.(attachResultMsg)
	if !ok || am.ok {
		t.Fatalf("got %+v, want a failed attachResultMsg with a manual hint", msg)
	}
	if !strings.Contains(am.text, "cd /home/user/myproject") {
		t.Errorf("text = %q, want the manual attach hint", am.text)
	}
}

// TestAttachCmd_CaptainTopology_FocusesLocatedTmuxSurface pins the fix's
// user-visible payoff at the attach level: on the captain's machine orca is
// always available, but the loop lives in a tmux surface. ResolveForLocate
// hands attachCmd the tmux Target directly, and attachCmd Focuses THAT (no
// second Locate), reporting the tmux backend — orca never gets to win by
// install order.
func TestAttachCmd_CaptainTopology_FocusesLocatedTmuxSurface(t *testing.T) {
	tmuxCtrl := &fakeController{name: "tmux", focusErr: nil}
	tmuxTarget := control.Target{Backend: "tmux", ID: "%3"}
	orig := controlResolveForLocateFn
	t.Cleanup(func() { controlResolveForLocateFn = orig })
	controlResolveForLocateFn = func(projectDir string) (control.Controller, control.Target, bool) {
		return tmuxCtrl, tmuxTarget, true // as if orca couldn't Locate, tmux did
	}
	l := domain.Loop{Project: "api", ProjectDir: "-x-api", Cwd: "/home/user/api", State: domain.StateRunning}

	msg := attachCmd(l)()

	if !tmuxCtrl.focusCalled {
		t.Error("expected Focus to be called on the located tmux surface")
	}
	am, ok := msg.(attachResultMsg)
	if !ok || !am.ok {
		t.Fatalf("got %+v, want a successful attachResultMsg", msg)
	}
	if !strings.Contains(am.text, "via tmux") {
		t.Errorf("text = %q, want it to report the tmux backend (locate-based, not orca by install order)", am.text)
	}
}

// ── attachCmd step 1: host_app FocusAdapter ────────────────────────────────

// fakeFocusAdapter is a control.FocusAdapter test double that records whether
// Raise was called and with which entry, and returns a configurable error.
type fakeFocusAdapter struct {
	raiseCalled bool
	raiseEntry  sessions.SessionEntry
	raiseErr    error
}

func (f *fakeFocusAdapter) Raise(entry sessions.SessionEntry) error {
	f.raiseCalled = true
	f.raiseEntry = entry
	return f.raiseErr
}

// withFakeAttachEntry makes attachCmd see a fixed SessionEntry for any session.
func withFakeAttachEntry(t *testing.T, entry sessions.SessionEntry) {
	t.Helper()
	orig := sessionEntryFn
	t.Cleanup(func() { sessionEntryFn = orig })
	sessionEntryFn = func(string) sessions.SessionEntry { return entry }
}

// withFakeFocusAdapter makes attachCmd's step-1 resolver return adapter for
// hostApp (and nothing for any other host_app).
func withFakeFocusAdapter(t *testing.T, hostApp string, adapter control.FocusAdapter) {
	t.Helper()
	orig := resolveFocusAdapterFn
	t.Cleanup(func() { resolveFocusAdapterFn = orig })
	resolveFocusAdapterFn = func(h string) (control.FocusAdapter, bool) {
		if h == hostApp {
			return adapter, true
		}
		return nil, false
	}
}

// TestAttachCmd_ITerm2Entry_RaisesViaFocusAdapter is step (a): a loop whose
// registry entry carries an iTerm2 host_app + a window_id is raised through the
// FocusAdapter — step 1 wins, the multiplexer path is never consulted.
func TestAttachCmd_ITerm2Entry_RaisesViaFocusAdapter(t *testing.T) {
	adapter := &fakeFocusAdapter{}
	withFakeAttachEntry(t, sessions.SessionEntry{HostApp: "iTerm.app", WindowID: "w0t1p0:GUID"})
	withFakeFocusAdapter(t, "iTerm.app", adapter)
	// If step 2 were reached it would panic the test by being unexpectedly hit:
	muxCtrl := &fakeController{name: "tmux", locateTarget: control.Target{Backend: "tmux", ID: "%1"}, locateOK: true}
	withFakeControlResolveForLocate(t, muxCtrl, true)

	l := domain.Loop{Project: "api", SessionID: "s1", ProjectDir: "-x-api", State: domain.StateRunning}
	msg := attachCmd(l)()

	if !adapter.raiseCalled {
		t.Fatal("expected FocusAdapter.Raise to be called for an iTerm2 entry")
	}
	if adapter.raiseEntry.WindowID != "w0t1p0:GUID" {
		t.Errorf("Raise got WindowID %q, want the recorded one", adapter.raiseEntry.WindowID)
	}
	if muxCtrl.focusCalled {
		t.Error("step 2 (multiplexer Focus) must NOT run once step 1 raised the window")
	}
	am, ok := msg.(attachResultMsg)
	if !ok || !am.ok || !strings.Contains(am.text, "via iTerm.app") {
		t.Fatalf("got %+v, want a successful attach reporting the iTerm.app host", msg)
	}
}

// TestAttachCmd_NoAdapterButMultiplexerLocatable_UsesResolveForLocate is step
// (b): an entry with no recognized host_app (legacy/multiplexer loop) falls
// through to today's ResolveForLocate+Focus path, unchanged.
func TestAttachCmd_NoAdapterButMultiplexerLocatable_UsesResolveForLocate(t *testing.T) {
	// Zero entry (no host_app) — the shape of a pre-schema-extension record.
	withFakeAttachEntry(t, sessions.SessionEntry{})
	muxCtrl := &fakeController{name: "tmux", locateTarget: control.Target{Backend: "tmux", ID: "%3"}, locateOK: true}
	withFakeControlResolveForLocate(t, muxCtrl, true)

	l := domain.Loop{Project: "api", SessionID: "s1", ProjectDir: "-x-api", State: domain.StateRunning}
	msg := attachCmd(l)()

	if !muxCtrl.locateCalled || !muxCtrl.focusCalled {
		t.Error("expected the multiplexer ResolveForLocate+Focus path to run for a no-adapter loop")
	}
	am, ok := msg.(attachResultMsg)
	if !ok || !am.ok || !strings.Contains(am.text, "via tmux") {
		t.Fatalf("got %+v, want a successful attach via tmux", msg)
	}
}

// TestAttachCmd_ITerm2NoFocusSurface_DegradesToMultiplexer confirms an adapter
// that reports ErrNoFocusSurface (window gone) degrades to step 2 rather than
// hard-failing.
func TestAttachCmd_ITerm2NoFocusSurface_DegradesToMultiplexer(t *testing.T) {
	adapter := &fakeFocusAdapter{raiseErr: control.ErrNoFocusSurface}
	withFakeAttachEntry(t, sessions.SessionEntry{HostApp: "iTerm.app", WindowID: "w0t1p0:GUID"})
	withFakeFocusAdapter(t, "iTerm.app", adapter)
	muxCtrl := &fakeController{name: "tmux", locateTarget: control.Target{Backend: "tmux", ID: "%7"}, locateOK: true}
	withFakeControlResolveForLocate(t, muxCtrl, true)

	l := domain.Loop{Project: "api", SessionID: "s1", ProjectDir: "-x-api", State: domain.StateRunning}
	msg := attachCmd(l)()

	if !adapter.raiseCalled {
		t.Error("expected Raise to be attempted")
	}
	if !muxCtrl.focusCalled {
		t.Error("expected degrade to the multiplexer Focus path on ErrNoFocusSurface")
	}
	am, ok := msg.(attachResultMsg)
	if !ok || !am.ok || !strings.Contains(am.text, "via tmux") {
		t.Fatalf("got %+v, want a successful attach via tmux after degrade", msg)
	}
}

// TestAttachCmd_AdapterWithoutWindowID_StillDelegatesToAdapter pins that the
// TUI does NOT second-guess an adapter's preconditions. "Needs a window_id" is
// iTerm2's rule, enforced inside its Raise (ErrNoFocusSurface); duplicating it
// here would silently skip any future adapter that keys on something else. The
// degrade path is identical either way, so the only observable difference — and
// the thing this test locks down — is that Raise gets asked.
func TestAttachCmd_AdapterWithoutWindowID_StillDelegatesToAdapter(t *testing.T) {
	adapter := &fakeFocusAdapter{raiseErr: control.ErrNoFocusSurface}
	withFakeAttachEntry(t, sessions.SessionEntry{HostApp: "iTerm.app"}) // no WindowID
	withFakeFocusAdapter(t, "iTerm.app", adapter)
	muxCtrl := &fakeController{name: "tmux", locateTarget: control.Target{Backend: "tmux", ID: "%9"}, locateOK: true}
	withFakeControlResolveForLocate(t, muxCtrl, true)

	l := domain.Loop{Project: "api", SessionID: "s1", ProjectDir: "-x-api", State: domain.StateRunning}
	msg := attachCmd(l)()

	if !adapter.raiseCalled {
		t.Error("the adapter must be consulted even with an empty WindowID — that precondition belongs to the adapter")
	}
	if !muxCtrl.focusCalled {
		t.Error("expected degrade to the multiplexer path after ErrNoFocusSurface")
	}
	am, ok := msg.(attachResultMsg)
	if !ok || !am.ok || !strings.Contains(am.text, "via tmux") {
		t.Fatalf("got %+v, want a successful attach via tmux after degrade", msg)
	}
}

// TestAttachCmd_ITerm2RaiseError_DegradesToMultiplexer: a genuine Raise error
// (NOT ErrNoFocusSurface) must still degrade to step 2. osascript exits
// non-zero when macOS Automation permission has not been granted — a normal
// first-run state — and hard-failing there would strand the human on an error
// while a working tmux surface sat one step away.
func TestAttachCmd_ITerm2RaiseError_DegradesToMultiplexer(t *testing.T) {
	adapter := &fakeFocusAdapter{raiseErr: errors.New("not authorized to send Apple events")}
	withFakeAttachEntry(t, sessions.SessionEntry{HostApp: "iTerm.app", WindowID: "w0t1p0:GUID"})
	withFakeFocusAdapter(t, "iTerm.app", adapter)
	muxCtrl := &fakeController{name: "tmux", locateTarget: control.Target{Backend: "tmux"}, locateOK: true}
	withFakeControlResolveForLocate(t, muxCtrl, true)

	l := domain.Loop{Project: "api", SessionID: "s1", ProjectDir: "-x-api", State: domain.StateRunning}
	msg := attachCmd(l)()

	if !muxCtrl.focusCalled {
		t.Error("a Raise error must degrade to the multiplexer path, not hard-fail")
	}
	am, ok := msg.(attachResultMsg)
	if !ok || !am.ok || !strings.Contains(am.text, "via tmux") {
		t.Fatalf("got %+v, want a successful attach via tmux after degrading past the Raise error", msg)
	}
}

// TestAttachCmd_RaiseErrorAndNoMultiplexer_ReportsWithManualHint: when every
// step comes up empty the human finally hears about the Raise error — but the
// message must still carry the manual hint, because attach never leaves someone
// with an error and no way forward.
func TestAttachCmd_RaiseErrorAndNoMultiplexer_ReportsWithManualHint(t *testing.T) {
	adapter := &fakeFocusAdapter{raiseErr: errors.New("not authorized to send Apple events")}
	withFakeAttachEntry(t, sessions.SessionEntry{HostApp: "iTerm.app", WindowID: "w0t1p0:GUID"})
	withFakeFocusAdapter(t, "iTerm.app", adapter)
	withFakeControlResolveForLocate(t, nil, false) // no multiplexer either

	l := domain.Loop{Project: "api", SessionID: "s1", ProjectDir: "-x-api", Cwd: "/w/api", State: domain.StateRunning}
	msg := attachCmd(l)()

	am, ok := msg.(attachResultMsg)
	if !ok || am.ok {
		t.Fatalf("got %+v, want a non-ok attach result", msg)
	}
	if !strings.Contains(am.text, manualAttachHint("/w/api")) {
		t.Errorf("text = %q, want it to end with the manual hint", am.text)
	}
	if !strings.Contains(am.text, "not authorized") {
		t.Errorf("text = %q, want the underlying Raise error reported once nothing else worked", am.text)
	}
}

// TestAttachCmd_NoFocusSurfaceAndNoMultiplexer_HintOnly: the DESIGNED degrade
// (ErrNoFocusSurface) is not an error the human can act on, so it must not be
// pasted into the status line — just the hint.
func TestAttachCmd_NoFocusSurfaceAndNoMultiplexer_HintOnly(t *testing.T) {
	adapter := &fakeFocusAdapter{raiseErr: control.ErrNoFocusSurface}
	withFakeAttachEntry(t, sessions.SessionEntry{HostApp: "iTerm.app"})
	withFakeFocusAdapter(t, "iTerm.app", adapter)
	withFakeControlResolveForLocate(t, nil, false)

	l := domain.Loop{Project: "api", SessionID: "s1", ProjectDir: "-x-api", Cwd: "/w/api", State: domain.StateRunning}
	msg := attachCmd(l)()

	am, ok := msg.(attachResultMsg)
	if !ok || am.ok {
		t.Fatalf("got %+v, want a non-ok attach result", msg)
	}
	if !strings.Contains(am.text, manualAttachHint("/w/api")) {
		t.Errorf("text = %q, want the manual hint", am.text)
	}
	if strings.Contains(am.text, "no focus surface") {
		t.Errorf("text = %q, must not leak the internal degrade sentinel to the human", am.text)
	}
}

// TestAttachCmd_MultiplexerFocusFails_StillHints: step 2 failing for real also
// lands on step 3 rather than a bare error.
func TestAttachCmd_MultiplexerFocusFails_StillHints(t *testing.T) {
	withFakeAttachEntry(t, sessions.SessionEntry{})
	muxCtrl := &fakeController{name: "tmux", locateTarget: control.Target{Backend: "tmux"}, locateOK: true, focusErr: errors.New("pane vanished")}
	withFakeControlResolveForLocate(t, muxCtrl, true)

	l := domain.Loop{Project: "api", SessionID: "s1", ProjectDir: "-x-api", Cwd: "/w/api", State: domain.StateRunning}
	msg := attachCmd(l)()

	am, ok := msg.(attachResultMsg)
	if !ok || am.ok {
		t.Fatalf("got %+v, want a non-ok attach result", msg)
	}
	if !strings.Contains(am.text, manualAttachHint("/w/api")) {
		t.Errorf("text = %q, want the manual hint even when Focus itself failed", am.text)
	}
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
	if !strings.Contains(m.status, "injecting into myproject") {
		t.Errorf("status = %q, want it to mention injecting into the target", m.status)
	}
}

func TestUpdate_IKey_TargetSnapshottedAtKeypress_SurvivesRescan(t *testing.T) {
	// the injection target is captured at "i" keypress time — a mid-typing
	// rescan (loopsMsg) that reorders/removes loops must not RETARGET the
	// pending injection. (Submit does refresh the SAME session's data by
	// SessionID — never by cursor — for its ambiguity/eligibility re-check;
	// a session that vanished from the fleet keeps its snapshot, as here.)
	m := modelWithOneLoop() // selects "myproject"/sess-1
	m, _ = updateModel(t, m, runeKey('i'))
	if m.injectTarget.SessionID != "sess-1" {
		t.Fatalf("precondition failed: injectTarget = %q, want sess-1", m.injectTarget.SessionID)
	}

	// fleet rescans mid-typing: "myproject" is gone, a different loop is now at
	// cursor 0.
	m, _ = updateModel(t, m, loopsMsg([]domain.Loop{
		{Project: "other", SessionID: "sess-9", ProjectDir: "-x-other", State: domain.StateRunning},
	}))

	if m.injectTarget.SessionID != "sess-1" {
		t.Errorf("injectTarget.SessionID = %q, want it to STAY the snapshotted sess-1 after a rescan", m.injectTarget.SessionID)
	}
	if m.injectTarget.Project != "myproject" {
		t.Errorf("injectTarget.Project = %q, want the snapshotted %q", m.injectTarget.Project, "myproject")
	}
}

func TestRenderInjectPrompt_RunningTarget_ShowsMidTurnWarning(t *testing.T) {
	// injecting into a StateRunning loop lands mid-turn — the prompt line must
	// surface a plain warning rather than pretend it's risk-free.
	m := modelWithOneLoop() // StateRunning
	m, _ = updateModel(t, m, runeKey('i'))

	out := renderInjectPrompt(m)

	if !strings.Contains(out, "myproject") {
		t.Errorf("rendered inject prompt = %q, want it to name the target loop", out)
	}
	if !strings.Contains(out, "lands mid-turn") {
		t.Errorf("rendered inject prompt = %q, want the mid-turn warning for a running target", out)
	}
}

func TestRenderInjectPrompt_IdleTarget_NoMidTurnWarning(t *testing.T) {
	// a non-running target has no mid-turn footgun — no warning.
	m := New()
	m.loops = []domain.Loop{{Project: "myproject", SessionID: "sess-1", ProjectDir: "-x-myproject", State: domain.StateIdle}}
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

	l := domain.Loop{SessionID: "s1", Cycle: 2, Goal: domain.Goal{Text: "fix the bug"}, Cwd: "/x/myproject", Path: "/no/such/file.jsonl"}
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

	spec := registry.BindSpec{Goal: "fix the bug", DoneCondition: "tests pass", Rubric: "run go test ./..."}
	if err := registry.Bind(dir, "s1", spec); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	l := domain.Loop{SessionID: "s1", Cycle: 1, Goal: domain.Goal{Text: "fix the bug", DoneWhen: "tests pass", Rubric: "run go test ./..."}}
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

// TestNoteForRow_NoGovernorNote_StalledStaysEmpty is fix/exit-gate-ux's
// (UX judge item 4) reversal of the old fallback: StateStalled already has
// its own callout box (renderResumeCallout) stating the stall reason — the
// NOTE row must NOT also echo it (that was 1 of the 3 repeats the judge
// flagged).
func TestNoteForRow_NoGovernorNote_StalledStaysEmpty(t *testing.T) {
	l := domain.Loop{State: domain.StateStalled, Stall: domain.StallNoOutput}
	note, _ := noteForRow(l)
	if note != "" {
		t.Errorf("note = %q, want empty — the STALLED callout already states this", note)
	}
}

// TestNoteForRow_NoGovernorNote_DriftStaysEmpty: same reversal for
// StateDrift — renderDriftCallout already states l.Last.Reason as its own
// headline.
func TestNoteForRow_NoGovernorNote_DriftStaysEmpty(t *testing.T) {
	l := domain.Loop{State: domain.StateDrift, Last: &domain.Verdict{Outcome: domain.OutcomeRejected, Reason: "no evidence shown"}}
	note, _ := noteForRow(l)
	if note != "" {
		t.Errorf("note = %q, want empty — the DRIFT callout already states this", note)
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
	// each word is exactly the width; the marker must displace enough of the
	// text to land exactly on the column, rather than overflow it.
	//
	// Asserted in DISPLAY width, not rune count, and measured with the SAME
	// condition trunc() truncates with (narrowAmbiguous, #44). "…" is East
	// Asian Ambiguous: a rune-count assertion would encode the ambiguous-width
	// policy rather than wrapTailText's own logic, and measuring with
	// go-runewidth's locale-inheriting DefaultCondition would report 6 columns
	// for a line trunc deliberately cut to 5 under ko/ja/zh. The invariant
	// below is the one wrapTailText actually owns.
	const width = 5
	got := wrapTailText("aaaaa bbbbb ccccc ddddd", width, 2)
	if len(got) != 2 {
		t.Fatalf("got %d lines %q, want 2", len(got), got)
	}
	if !strings.HasSuffix(got[1], "…") {
		t.Errorf("last shown line %q lacks the truncation marker", got[1])
	}
	if w := narrowAmbiguous.StringWidth(got[1]); w != width {
		t.Errorf("last line %q = %d columns, want exactly width %d (marker displaces, never overflows)", got[1], w, width)
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
	l := domain.Loop{Project: "myproject", SessionID: "s1", State: domain.StateIdle, Cwd: "/x", Path: "/x/s1.jsonl"}
	out := renderDetail(l, 80, 40, detailData{now: time.Now()})
	if strings.Contains(out, "TAIL") {
		t.Errorf("detail pane should have NO TAIL row when LastText is empty:\n%s", out)
	}
}

func TestRenderDetail_LongLastText_ShowsWrappedTruncatedTailRow(t *testing.T) {
	// long enough to overflow tailMaxLines at the pane width → wrapped + marked.
	l := domain.Loop{
		Project:   "myproject",
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

// TestBudgetLine_NoGoalText_NoSuffixButBaseStillRenders: a loop with a cap
// set but no Goal.Text (budgetBurnRateSuffix's OWN "unbound" check, keyed
// on Goal.Text — a different condition than budgetLine's own
// BudgetTokens<=0 check below) still shows the base spent/cap/percent, just
// without a burn-rate suffix.
func TestBudgetLine_NoGoalText_NoSuffixButBaseStillRenders(t *testing.T) {
	l := domain.Loop{Goal: domain.Goal{BudgetTokens: 1000}, TokensSpent: 500}
	got := budgetLine(l)
	if !strings.Contains(got, "500") {
		t.Errorf("got %q, want the base spent/cap text present regardless of the suffix", got)
	}
	if strings.Contains(got, "/cyc") {
		t.Errorf("got %q, want no burn-rate suffix without Goal.Text", got)
	}
}

// TestBudgetLine_UnboundBudget_NoCapNoPercentNoSuffix is fix/exit-gate-ux's
// P1 regression ("most common view is broken" — most real OBSERVED loops
// have no wizard-set contract at all, so Goal.BudgetTokens is always 0):
// budgetLine used to render a fabricated "<spent> / 0 (0%)" — a cap and
// percentage against a budget that was never set. It must show ONLY the
// pretty-printed spend, with no "/ 0 (0%)" and no burn-rate suffix (ETA
// needs a real cap to project against).
func TestBudgetLine_UnboundBudget_NoCapNoPercentNoSuffix(t *testing.T) {
	l := domain.Loop{Project: "observed", TokensSpent: 380_000} // BudgetTokens unset — an observed, non-contracted session
	got := budgetLine(l)
	if got != "380k" {
		t.Errorf("got %q, want exactly the pretty-printed spend %q — no cap, no percent, no suffix", got, "380k")
	}
}

// TestBudgetLine_BoundLoop_Unchanged pins that a loop WITH a real budget
// cap keeps showing the full "<spent> / <cap> (P%)" form — the fix above
// must only change the BudgetTokens<=0 case, not bound loops.
func TestBudgetLine_BoundLoop_Unchanged(t *testing.T) {
	l := domain.Loop{Goal: domain.Goal{Text: "g", BudgetTokens: 1_000_000}, TokensSpent: 500_000}
	got := budgetLine(l)
	if !strings.Contains(got, "500k / 1.0M (50%)") {
		t.Errorf("got %q, want the full spent/cap/percent form for a bound loop", got)
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
	l := domain.Loop{Project: "myproject", SessionID: "s1", State: domain.StateIdle, BoundAt: time.Now().Add(-time.Minute)}
	out := renderDetail(l, 80, 40, detailData{now: time.Now()})
	if strings.Contains(out, "STAGE") {
		t.Errorf("STAGE must not render for an unbound loop even with a valid BoundAt:\n%s", out)
	}
}

func TestRenderDetail_StageRowPresentForBoundLoop(t *testing.T) {
	l := domain.Loop{Project: "myproject", SessionID: "s1", State: domain.StateIdle, Cycle: 3,
		Goal: domain.Goal{Text: "fix it", MaxCycles: 12}, BoundAt: time.Now().Add(-4 * time.Minute)}
	out := renderDetail(l, 80, 40, detailData{now: time.Now()})
	if !strings.Contains(out, "STAGE") {
		t.Errorf("STAGE should render for a bound loop with a valid elapsed source:\n%s", out)
	}
}

// ── feat/panel-info: RUBRIC/CHALL contract rows ("leave it blank if there's nothing") ────────

// TestRenderDetail_RubricAndChallFilled_ShowValues: a bound loop with both
// fields set shows them verbatim.
func TestRenderDetail_RubricAndChallFilled_ShowValues(t *testing.T) {
	l := domain.Loop{Project: "myproject", SessionID: "s1", State: domain.StateRunning,
		Goal: domain.Goal{Text: "fix it", Rubric: "run go test ./...", Challenger: "adversarial probe"}}
	out := renderDetail(l, 100, 40, detailData{now: time.Now()})
	if !strings.Contains(out, "run go test ./...") {
		t.Errorf("expected the RUBRIC value present:\n%s", out)
	}
	if !strings.Contains(out, "adversarial probe") {
		t.Errorf("expected the CHALL value present:\n%s", out)
	}
}

// TestRenderDetail_RubricAndChallEmpty_ShowDashNotHidden is the
// "leave it blank if there's nothing" behavior: a bound loop with NEITHER field set must still
// show BOTH rows (with a "—" placeholder), not omit them — a predictable
// row count regardless of what the wizard collected, unlike the OLD
// behavior (RUBRIC hidden when empty, CHALLENGER never shown at all).
func TestRenderDetail_RubricAndChallEmpty_ShowDashNotHidden(t *testing.T) {
	l := domain.Loop{Project: "myproject", SessionID: "s1", State: domain.StateRunning,
		Goal: domain.Goal{Text: "fix it"}} // Rubric/Challenger both ""
	out := renderDetail(l, 100, 40, detailData{now: time.Now()})
	if !strings.Contains(out, "RUBRIC") {
		t.Errorf("expected the RUBRIC row to still be present:\n%s", out)
	}
	if !strings.Contains(out, "CHALL") {
		t.Errorf("expected the CHALL row to still be present:\n%s", out)
	}
}

// TestRenderDetail_UnboundLoop_NoRubricOrChallRow: an unbound loop (no
// Goal.Text at all) shows neither row — there's no contract to display a
// placeholder FOR.
func TestRenderDetail_UnboundLoop_NoRubricOrChallRow(t *testing.T) {
	l := domain.Loop{Project: "myproject", SessionID: "s1", State: domain.StateRunning}
	out := renderDetail(l, 100, 40, detailData{now: time.Now()})
	if strings.Contains(out, "RUBRIC") {
		t.Errorf("expected no RUBRIC row for an unbound loop:\n%s", out)
	}
	if strings.Contains(out, "CHALL") {
		t.Errorf("expected no CHALL row for an unbound loop:\n%s", out)
	}
}

func TestOrDash(t *testing.T) {
	if got := orDash(""); got != "—" {
		t.Errorf("orDash(\"\") = %q, want %q", got, "—")
	}
	if got := orDash("value"); got != "value" {
		t.Errorf("orDash(%q) = %q, want unchanged", "value", got)
	}
}

// TestDetailRow_KeyWidth_NoKeyExceedsDetailKeyWidth is a structural
// regression pin: lipgloss WRAPS (does not overflow-in-place or truncate)
// a .Width()-styled key longer than detailKeyWidth — verified empirically
// while adding the CHALL row (an earlier "CHALLENGER" label silently broke
// row alignment). Every detailRow KEY literal in this file must stay
// within detailKeyWidth runes, checked here so a future row addition
// can't reintroduce the same class of bug silently.
func TestDetailRow_KeyWidth_NoKeyExceedsDetailKeyWidth(t *testing.T) {
	keys := []string{"STATE", "NOTE", "CYCLE", "GOAL", "ORACLE", "RUBRIC", "CHALL", "STAGE", "BUDGET", "N/I", "LAST", "CWD", "LOG", "TAIL", "EVENTS"}
	for _, k := range keys {
		if len(k) > detailKeyWidth {
			t.Errorf("key %q is %d runes, want <= detailKeyWidth (%d) — lipgloss wraps rather than overflowing", k, len(k), detailKeyWidth)
		}
	}
}

// ── fix/exit-gate-ux: DETAIL self-repetition (UX judge item 4) ───────────

// TestRenderDetail_FirstLine_SessionIDOnly_NoProjectEcho: the panel's own
// title already reads "DETAIL ▸ <project>" (see detailTitle) — the
// content block's first line must not print the project name a second
// time, just the session id.
func TestRenderDetail_FirstLine_SessionIDOnly_NoProjectEcho(t *testing.T) {
	l := domain.Loop{Project: "myproject", SessionID: "sess-xyz", State: domain.StateRunning}
	out := renderDetail(l, 80, 40, detailData{now: time.Now()})
	lines := strings.Split(out, "\n")
	if len(lines) == 0 {
		t.Fatal("expected at least one line")
	}
	if strings.Contains(lines[0], "myproject") {
		t.Errorf("first line = %q, must not repeat the project name (already in the panel title)", lines[0])
	}
	if !strings.Contains(lines[0], "sess-xyz") {
		t.Errorf("first line = %q, want the session id", lines[0])
	}
}

// TestRenderOracleDetail_DriftLoop_ShowsCycleNotReason: on a DRIFT loop,
// renderDriftCallout already prints l.Last.Reason as its own headline
// below — ORACLE must show a DIFFERENT fact (the verdict's cycle), not the
// same reason string a second (well, third — NOTE used to be the second)
// time.
func TestRenderOracleDetail_DriftLoop_ShowsCycleNotReason(t *testing.T) {
	l := domain.Loop{State: domain.StateDrift, Last: &domain.Verdict{Outcome: domain.OutcomeRejected, Reason: "no evidence shown", AtCycle: 4}}
	got := renderOracleDetail(l)
	if strings.Contains(got, "no evidence shown") {
		t.Errorf("got %q, must NOT repeat the reason text the DRIFT callout already shows", got)
	}
	if !strings.Contains(got, "4") {
		t.Errorf("got %q, want it to mention the verdict's cycle (4)", got)
	}
}

// TestRenderOracleDetail_DoneLoop_ShowsCycleNotReason is fix/detail-dedup's
// re-judge regression: a DONE loop (no callout at all — DONE has none)
// STILL had the same reason string repeating between ORACLE and VERDICTS.
// The compact glyph+cycle form is now universal, not StateDrift-specific —
// VERDICTS (renderVerdictsBlock) is the ONE place the verbatim reason
// lives, regardless of outcome/state.
func TestRenderOracleDetail_DoneLoop_ShowsCycleNotReason(t *testing.T) {
	l := domain.Loop{State: domain.StateDone, Last: &domain.Verdict{Outcome: domain.OutcomeDone, Reason: "all tests pass, feature verified", AtCycle: 6}}
	got := renderOracleDetail(l)
	if strings.Contains(got, "all tests pass") {
		t.Errorf("got %q, must NOT repeat the reason text VERDICTS already shows", got)
	}
	if !strings.Contains(got, "✓") || !strings.Contains(got, "6") {
		t.Errorf("got %q, want a compact ✓ glyph + cycle 6", got)
	}
}

// TestRenderOracleDetail_NonDriftRejected_AlsoCompact: fix/detail-dedup
// dropped the StateDrift-only carve-out entirely — a loop whose State has
// since moved on from DRIFT (e.g. re-driven back to running) but still
// carries an old rejected verdict ALSO gets the compact form now, not the
// full reason (VERDICTS is the one place for that, regardless of State).
func TestRenderOracleDetail_NonDriftRejected_AlsoCompact(t *testing.T) {
	l := domain.Loop{State: domain.StateRunning, Last: &domain.Verdict{Outcome: domain.OutcomeRejected, Reason: "no evidence shown", AtCycle: 4}}
	got := renderOracleDetail(l)
	if strings.Contains(got, "no evidence shown") {
		t.Errorf("got %q, want the compact form — VERDICTS is the one place for the full reason now", got)
	}
	if !strings.Contains(got, "✗") || !strings.Contains(got, "4") {
		t.Errorf("got %q, want a compact ✗ glyph + cycle 4", got)
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

// lastErrorFromFile writes content to a transcript file and runs the REAL
// claude.LastError extraction against it, wrapping the result as
// detailData's lastError field — fix/exit-gate-ux moved that extraction
// off renderDetail (see detailCacheCmd), so tests that want to exercise
// the real parsing pipeline now do so explicitly at this seam, then feed
// the result into renderDetail exactly as detailPanelLines does.
func lastErrorFromFile(t *testing.T, dir, content string) lastErrorResult {
	t.Helper()
	path := filepath.Join(dir, "s1.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	text, ts, ok := claude.LastError(path)
	return lastErrorResult{text: text, ts: ts, ok: ok}
}

func TestRenderDetail_LastErrorBlock_ShownWhenCurrent(t *testing.T) {
	dir := t.TempDir()
	content := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"API Error: 429 rate limited"}]},"timestamp":"` + time.Now().Format(time.RFC3339) + `"}` + "\n"
	lastErr := lastErrorFromFile(t, dir, content)
	l := domain.Loop{Project: "myproject", SessionID: "s1", State: domain.StateStalled, Stall: domain.StallRateLimit}
	out := renderDetail(l, 80, 40, detailData{now: time.Now(), lastError: lastErr})
	if !strings.Contains(out, "LAST ERROR") {
		t.Errorf("expected a LAST ERROR block:\n%s", out)
	}
	if !strings.Contains(out, "API Error: 429 rate limited") {
		t.Errorf("expected the VERBATIM error text:\n%s", out)
	}
}

func TestRenderDetail_LastErrorBlock_SuppressedWhenStale(t *testing.T) {
	dir := t.TempDir()
	oldTS := time.Now().Add(-time.Hour)
	content := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"API Error: 429 rate limited"}]},"timestamp":"` + oldTS.Format(time.RFC3339) + `"}` + "\n"
	lastErr := lastErrorFromFile(t, dir, content)
	l := domain.Loop{Project: "myproject", SessionID: "s1", State: domain.StateIdle}
	evs := []events.Event{
		{TS: time.Now().Add(-30 * time.Minute).UnixNano(), Trigger: events.TriggerScan, ToState: "idle"}, // recovered since the error
	}
	out := renderDetail(l, 80, 40, detailData{now: time.Now(), events: evs, lastError: lastErr})
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
	content := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"landed #24 (429 auto-redrive) — Tier 2 only, opt-in. main is green."}]},"timestamp":"` + time.Now().Format(time.RFC3339) + `"}` + "\n"
	lastErr := lastErrorFromFile(t, dir, content)
	l := domain.Loop{Project: "myproject", SessionID: "s1", State: domain.StateRunning}
	out := renderDetail(l, 80, 40, detailData{now: time.Now(), lastError: lastErr})
	if strings.Contains(out, "LAST ERROR") {
		t.Errorf("expected NO LAST ERROR block — this is ordinary conversation mentioning a status code, not a real error:\n%s", out)
	}
}

func TestRenderDetail_NoErrorAtAll_NoBlock(t *testing.T) {
	l := domain.Loop{Project: "myproject", SessionID: "s1", State: domain.StateIdle}
	out := renderDetail(l, 80, 40, detailData{now: time.Now()})
	if strings.Contains(out, "LAST ERROR") {
		t.Errorf("expected no LAST ERROR block when there's no transcript error:\n%s", out)
	}
}

// ── DETAIL panel async cache (fix/exit-gate-ux, architecture judge P1) ───

// TestDetailCacheCmd_GathersEventsAndLastError is detailCacheCmd's own
// direct regression: it must gather BOTH the event log (via events.Read)
// AND the transcript's LAST ERROR (via claude.LastError) off the render
// path, bundled into one detailCacheMsg keyed by SessionID.
func TestDetailCacheCmd_GathersEventsAndLastError(t *testing.T) {
	historyDir := t.TempDir()
	origHistoryDir := historyDirFn
	defer func() { historyDirFn = origHistoryDir }()
	historyDirFn = func() string { return historyDir }

	ev := events.Event{SessionID: "s1", TS: time.Now().UnixNano(), Trigger: events.TriggerScan, ToState: "running"}
	if err := events.Append(historyDir, ev); err != nil {
		t.Fatalf("Append: %v", err)
	}

	transcriptPath := filepath.Join(t.TempDir(), "s1.jsonl")
	content := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"API Error: 429 rate limited"}]},"timestamp":"` + time.Now().Format(time.RFC3339) + `"}` + "\n"
	if err := os.WriteFile(transcriptPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	l := domain.Loop{SessionID: "s1", Path: transcriptPath}
	msg, ok := detailCacheCmd(l)().(detailCacheMsg)
	if !ok {
		t.Fatalf("detailCacheCmd did not return a detailCacheMsg")
	}
	if msg.sessionID != "s1" {
		t.Errorf("sessionID = %q, want %q", msg.sessionID, "s1")
	}
	if len(msg.entry.events) != 1 {
		t.Errorf("got %d events, want 1 (from the real history dir)", len(msg.entry.events))
	}
	if !msg.entry.lastError.ok || msg.entry.lastError.text != "API Error: 429 rate limited" {
		t.Errorf("lastError = %+v, want ok=true text=%q", msg.entry.lastError, "API Error: 429 rate limited")
	}
}

// TestUpdate_DetailCacheMsg_PopulatesCache mirrors gitStatsMsg's own
// Update handling: a detailCacheMsg must land in m.detailCache keyed by
// SessionID, lazily initializing the map on first use.
func TestUpdate_DetailCacheMsg_PopulatesCache(t *testing.T) {
	m := New()
	entry := detailCacheEntry{
		events:    []events.Event{{SessionID: "s1", TS: time.Now().UnixNano()}},
		lastError: lastErrorResult{text: "boom", ok: true},
	}
	updated, cmd := m.Update(detailCacheMsg{sessionID: "s1", entry: entry})
	mm := updated.(Model)
	if cmd != nil {
		t.Errorf("expected no follow-up cmd from detailCacheMsg, got one")
	}
	got, ok := mm.detailCache["s1"]
	if !ok {
		t.Fatal("expected detailCache[\"s1\"] to be populated")
	}
	if len(got.events) != 1 || !got.lastError.ok || got.lastError.text != "boom" {
		t.Errorf("detailCache[\"s1\"] = %+v, want the entry passed in the msg", got)
	}
}

// TestUpdate_LoopsMsg_DispatchesDetailCacheCmd is the P1 regression itself:
// a scan tick (loopsMsg) for a fleet with a selected loop must dispatch a
// detailCacheCmd for it — the SAME cadence gitStatsCmd already gets — so
// events.Read/claude.LastError run off the Update/View goroutine, not
// synchronously inside View() on every keystroke/tick.
func TestUpdate_LoopsMsg_DispatchesDetailCacheCmd(t *testing.T) {
	historyDir := t.TempDir()
	origHistoryDir := historyDirFn
	defer func() { historyDirFn = origHistoryDir }()
	historyDirFn = func() string { return historyDir }

	m := New()
	l := domain.Loop{Project: "myproject", SessionID: "s1", State: domain.StateRunning}
	_, cmd := m.Update(loopsMsg([]domain.Loop{l}))
	if cmd == nil {
		t.Fatal("expected a non-nil batched cmd from loopsMsg")
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatalf("expected loopsMsg's cmd to be a tea.Batch, got %T", cmd())
	}
	found := false
	for _, sub := range batch {
		if sub == nil {
			continue
		}
		if msg, ok := sub().(detailCacheMsg); ok && msg.sessionID == "s1" {
			found = true
		}
	}
	if !found {
		t.Error("expected loopsMsg's batched cmds to include a detailCacheCmd for the selected loop (s1)")
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
	l := domain.Loop{Project: "myproject", SessionID: "s1", State: domain.StateIdle}
	evs := []events.Event{{TS: 1, Trigger: events.TriggerOracle, Detail: "done at cycle 1: ok"}}
	out := renderDetail(l, 80, 40, detailData{now: time.Now(), events: evs})
	if strings.Contains(out, "VERDICTS") {
		t.Errorf("VERDICTS must not render for an unbound loop:\n%s", out)
	}
}

// ── fix/detail-dedup: end-to-end reason-appears-once (UX judge re-judge) ──

// TestRenderDetail_DoneLoop_ReasonAppearsExactlyOnce is the re-judge's
// exact repro for a DONE loop: verified live that the SAME verdict reason
// rendered 3x within one DETAIL viewport. DONE has no action callout at
// all, so after this fix the reason must appear EXACTLY ONCE in the whole
// DETAIL output — in VERDICTS, and nowhere else (not ORACLE, not EVENTS).
func TestRenderDetail_DoneLoop_ReasonAppearsExactlyOnce(t *testing.T) {
	const reason = "all tests pass, feature verified end to end"
	now := time.Now()
	l := domain.Loop{
		Project: "myproject", SessionID: "s1", State: domain.StateDone, Cycle: 6,
		Goal: domain.Goal{Text: "ship the feature", MaxCycles: 12},
		Last: &domain.Verdict{Outcome: domain.OutcomeDone, Reason: reason, AtCycle: 6},
	}
	evs := []events.Event{
		{TS: now.Add(-10 * time.Minute).UnixNano(), Trigger: events.TriggerScan, FromState: "running", ToState: "idle"},
		{TS: now.Add(-1 * time.Minute).UnixNano(), Trigger: events.TriggerOracle, Detail: "done at cycle 6: " + reason},
	}
	out := renderDetail(l, 100, 40, detailData{now: now, events: evs})

	if got := strings.Count(out, reason); got != 1 {
		t.Errorf("reason %q appears %d times in the DONE loop's DETAIL output, want exactly 1:\n%s", reason, got, out)
	}
	if !strings.Contains(out, "VERDICTS") {
		t.Errorf("expected VERDICTS to be present (it's the one place the reason should live):\n%s", out)
	}
}

// TestRenderDetail_DriftLoop_ReasonAppearsOnceInVerdictsPlusOnceInCallout
// is the re-judge's DRIFT repro: the reason must appear in VERDICTS once
// AND in the DRIFT callout's headline once (that's "the problem + what to
// act on now" — a defensible distinct purpose from VERDICTS' judgment
// history) — exactly 2 total, never a 3rd copy in ORACLE or EVENTS.
func TestRenderDetail_DriftLoop_ReasonAppearsOnceInVerdictsPlusOnceInCallout(t *testing.T) {
	const reason = "no evidence the feature was actually tested"
	now := time.Now()
	l := domain.Loop{
		Project: "myproject", SessionID: "s1", State: domain.StateDrift, Cycle: 4,
		Goal: domain.Goal{Text: "ship the feature", MaxCycles: 12},
		Last: &domain.Verdict{Outcome: domain.OutcomeRejected, Reason: reason, AtCycle: 4},
	}
	evs := []events.Event{
		{TS: now.Add(-10 * time.Minute).UnixNano(), Trigger: events.TriggerScan, FromState: "running", ToState: "idle"},
		{TS: now.Add(-1 * time.Minute).UnixNano(), Trigger: events.TriggerOracle, Detail: "rejected at cycle 4: " + reason},
	}
	out := renderDetail(l, 100, 40, detailData{now: now, events: evs})

	if got := strings.Count(out, reason); got != 2 {
		t.Errorf("reason %q appears %d times in the DRIFT loop's DETAIL output, want exactly 2 (VERDICTS + callout):\n%s", reason, got, out)
	}
	if !strings.Contains(out, "VERDICTS") {
		t.Errorf("expected VERDICTS to be present:\n%s", out)
	}
	if !strings.Contains(out, "DRIFT ▸") {
		t.Errorf("expected the DRIFT callout to be present:\n%s", out)
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

// TestRenderEventsBlock_OracleEventsExcluded is fix/detail-dedup's core
// regression: VERDICTS (renderVerdictsBlock) is the dedicated oracle view
// — a TriggerOracle event showing up here TOO (same ts, same verbatim
// reason, only the glyph differing) was the exact 3-peat the UX judge's
// re-judge caught. EVENTS is the actuation/scan/governor timeline only.
func TestRenderEventsBlock_OracleEventsExcluded(t *testing.T) {
	evs := []events.Event{
		{TS: 1, Trigger: events.TriggerScan, FromState: "running", ToState: "idle"},
		{TS: 2, Trigger: events.TriggerOracle, Detail: `done at cycle 1: "all tests pass, feature verified"`},
		{TS: 3, Trigger: events.TriggerActuation, Detail: "resume tier1 ok", Actor: events.ActorHuman},
	}
	got := renderEventsBlock(evs, 80, 10)
	if strings.Contains(got, "all tests pass") {
		t.Errorf("got %q, want the oracle verdict's reason NOT to appear in EVENTS (VERDICTS owns it)", got)
	}
	if !strings.Contains(got, "→") {
		t.Errorf("got %q, want the scan transition still present", got)
	}
	if !strings.Contains(got, "resume tier1 ok") {
		t.Errorf("got %q, want the actuation event still present", got)
	}
}

// TestRenderEventsBlock_OnlyOracleEvents_RendersEmpty: if EVERY event in
// the history happens to be an oracle verdict (e.g. a brand new goal-bound
// loop judged once, no other history yet), EVENTS must render nothing at
// all — an "EVENTS" header with zero data rows under it would be worse
// than omitting the block, same convention renderVerdictsBlock already
// follows for "nothing to show".
func TestRenderEventsBlock_OnlyOracleEvents_RendersEmpty(t *testing.T) {
	evs := []events.Event{
		{TS: 1, Trigger: events.TriggerOracle, Detail: "done at cycle 1: ok"},
	}
	if got := renderEventsBlock(evs, 80, 10); got != "" {
		t.Errorf("got %q, want empty when every event is an oracle verdict", got)
	}
}

func TestRenderDetail_EventsBlockAbsentWhenTooLittleHeight(t *testing.T) {
	l := domain.Loop{Project: "myproject", SessionID: "s1", State: domain.StateIdle}
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
	l := domain.Loop{Project: "myproject", SessionID: "s1", State: domain.StateIdle}
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
	l := domain.Loop{Project: "myproject", SessionID: "sess-asre1234", State: domain.StateStalled, Stall: domain.StallRateLimit}
	cases := []struct {
		name  string
		query string
		want  bool
	}{
		{"empty query matches everything", "", true},
		{"project, case-insensitive", "MYPROJECT", true},
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
		{Project: "myproject", SessionID: "sess-1", State: domain.StateRunning},
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
		{Project: "myproject", SessionID: "sess-1", State: domain.StateRunning},
		{Project: "myproject", SessionID: "sess-2", State: domain.StateRunning},
		{Project: "asre", SessionID: "sess-3", State: domain.StateIdle},
	}
	m.cursor = 1 // second "myproject" loop

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
		{Project: "myproject", SessionID: "sess-1", State: domain.StateStalled},
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
		"rubric: run go test ./...\n" +
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
		"rubric: an independent LLM judge verifies against the complete condition\n" +
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
		{wizardRubric, "rubric:"},
		{wizardChallenger, "challenger:"},
		{wizardName, "name (fleet list label, optional):"},
		{wizardMaxCycles, "max_iteration [12]:"},
		{wizardDir, "dir (absolute or ~ path; empty keeps current):"},
	}
	for _, c := range cases {
		if got := wizardStepLabel(c.step); got != c.want {
			t.Errorf("wizardStepLabel(%v) = %q, want %q", c.step, got, c.want)
		}
	}
}

// ── worktree spawn: wizardWhere step ─────────────────────────────────

// reachWizardWhere drives the wizard from a fresh "n" keypress through all
// 6 free-text steps (goal filled, the rest left empty/default) with
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
	m, _ = typeAndEnter(t, m, "")
	if m.mode != modePrompting || m.spawnStep != wizardWhere {
		t.Fatalf("precondition failed: mode=%v step=%v, want modePrompting at wizardWhere", m.mode, m.spawnStep)
	}
	return m
}

func TestWizard_ShowsWhereStep_EvenWhenNotEligible(t *testing.T) {
	// the zero-value default (spawnWorktreeEligible=false, e.g. no backend
	// resolved, or tmux/cmux) — wizardWhere must STILL be shown: it's the
	// step that displays the spawn target dir and offers [c]/[s] to change
	// it, so skipping it would commit the spawn to a dir the human never
	// saw. [w] just isn't offered.
	m := modelWithOneLoop()
	m, _ = updateModel(t, m, runeKey('n'))
	m, _ = typeAndEnter(t, m, "goal")
	m, _ = typeAndEnter(t, m, "")
	m, _ = typeAndEnter(t, m, "")
	m, _ = typeAndEnter(t, m, "")
	m, _ = typeAndEnter(t, m, "")
	m, _ = typeAndEnter(t, m, "") // step 6: max cycles (name step shifted everything by one)

	if m.mode != modePrompting || m.spawnStep != wizardWhere {
		t.Fatalf("got mode=%v step=%v, want modePrompting at wizardWhere even when ineligible", m.mode, m.spawnStep)
	}
	if strings.Contains(m.whereStepLabel(), "new worktree") {
		t.Errorf("whereStepLabel() = %q, want no [w] option when the backend can't isolate", m.whereStepLabel())
	}
	if !strings.Contains(m.whereStepLabel(), "change dir") {
		t.Errorf("whereStepLabel() = %q, want the [c] change-dir option", m.whereStepLabel())
	}

	m, cmd := updateModel(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.mode != modeNormal {
		t.Errorf("mode = %v, want modeNormal after enter", m.mode)
	}
	if cmd == nil {
		t.Fatal("expected a non-nil tea.Cmd (spawnCmd)")
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
	// a fleet loop's Cwd matching the spawn target (the launch dir) is the
	// "claude has actually run here" evidence — combined with the
	// forced-eligible backend, enter's default must resolve to worktree.
	wd, _ := os.Getwd()
	m := New()
	m.loops = []domain.Loop{{Project: "myproject", SessionID: "sess-1", Cwd: wd, CwdVerified: true, State: domain.StateRunning}}
	m.cursor = 0
	m = reachWizardWhere(t, m)

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

// ── explicit spawn-dir choice: [c]/[s] at wizardWhere, wizardDir step ────

func TestUpdate_WizardWhere_WKey_IgnoredWhenNotEligible(t *testing.T) {
	// [w] isn't offered when the backend can't isolate — pressing it anyway
	// must not submit a "worktree" spawn that would silently degrade to a
	// current-dir one.
	m := reachWizardWhere(t, modelWithOneLoop())
	m.spawnWorktreeEligible = false

	m, cmd := updateModel(t, m, runeKey('w'))

	if cmd != nil {
		t.Error("expected no tea.Cmd — [w] must be inert when ineligible")
	}
	if m.mode != modePrompting || m.spawnStep != wizardWhere {
		t.Errorf("got mode=%v step=%v, want to remain at wizardWhere", m.mode, m.spawnStep)
	}
}

func TestUpdate_WizardWhere_CKey_OpensDirStep_Prefilled(t *testing.T) {
	m := reachWizardWhere(t, modelWithOneLoop())

	m, _ = updateModel(t, m, runeKey('c'))

	if m.spawnStep != wizardDir {
		t.Fatalf("spawnStep = %v, want wizardDir", m.spawnStep)
	}
	if m.input.Value() != m.spawnCwd {
		t.Errorf("input prefill = %q, want the current target %q", m.input.Value(), m.spawnCwd)
	}
}

func TestUpdate_WizardDir_ValidPath_UpdatesTargetAndReturnsToWhere(t *testing.T) {
	dir := t.TempDir()
	m := reachWizardWhere(t, modelWithOneLoop())
	m, _ = updateModel(t, m, runeKey('c'))

	m.input.SetValue(dir)
	m, _ = updateModel(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	if m.spawnStep != wizardWhere {
		t.Errorf("spawnStep = %v, want back at wizardWhere", m.spawnStep)
	}
	if m.spawnCwd != dir {
		t.Errorf("spawnCwd = %q, want the entered dir %q", m.spawnCwd, dir)
	}
}

func TestUpdate_WizardDir_InvalidPath_RePrompts(t *testing.T) {
	m := reachWizardWhere(t, modelWithOneLoop())
	before := m.spawnCwd
	m, _ = updateModel(t, m, runeKey('c'))

	m.input.SetValue("/definitely/not/a/real/dir-xyz")
	m, cmd := updateModel(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	if cmd != nil {
		t.Error("expected no tea.Cmd — an invalid dir must not advance")
	}
	if m.spawnStep != wizardDir {
		t.Errorf("spawnStep = %v, want to stay at wizardDir (re-prompt)", m.spawnStep)
	}
	if m.statusKind != statusErr || !strings.Contains(m.status, "not a directory") {
		t.Errorf("status = %q (kind %v), want a not-a-directory error", m.status, m.statusKind)
	}
	if m.spawnCwd != before {
		t.Errorf("spawnCwd = %q, want unchanged %q", m.spawnCwd, before)
	}
}

func TestUpdate_WizardDir_Empty_KeepsCurrentTarget(t *testing.T) {
	m := reachWizardWhere(t, modelWithOneLoop())
	before := m.spawnCwd
	m, _ = updateModel(t, m, runeKey('c'))

	m.input.SetValue("")
	m, _ = updateModel(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	if m.spawnStep != wizardWhere {
		t.Errorf("spawnStep = %v, want back at wizardWhere", m.spawnStep)
	}
	if m.spawnCwd != before {
		t.Errorf("spawnCwd = %q, want unchanged %q", m.spawnCwd, before)
	}
}

func TestUpdate_WizardDir_TildeExpands(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	m := reachWizardWhere(t, modelWithOneLoop())
	m, _ = updateModel(t, m, runeKey('c'))

	m.input.SetValue("~")
	m, _ = updateModel(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	if m.spawnCwd != home {
		t.Errorf("spawnCwd = %q, want the expanded home dir %q", m.spawnCwd, home)
	}
}

func TestUpdate_WizardWhere_SKey_UsesSelectedVerifiedDir(t *testing.T) {
	// the explicit replacement for the old silent inheritance: [s] adopts
	// the selected loop's verified cwd as the spawn target — and stays on
	// wizardWhere so the re-rendered label shows the new target before the
	// human commits.
	m := reachWizardWhere(t, modelWithOneLoop())

	m, cmd := updateModel(t, m, runeKey('s'))

	if cmd != nil {
		t.Error("expected no tea.Cmd — [s] only retargets, never submits")
	}
	if m.spawnStep != wizardWhere {
		t.Errorf("spawnStep = %v, want to remain at wizardWhere", m.spawnStep)
	}
	if m.spawnCwd != "/x/myproject" {
		t.Errorf("spawnCwd = %q, want the selected loop's verified Cwd %q", m.spawnCwd, "/x/myproject")
	}
	if !m.spawnHostsClaudeRepo {
		t.Error("expected spawnHostsClaudeRepo recomputed true for the adopted dir")
	}
}

func TestUpdate_WizardWhere_SKey_IgnoredWhenUnverified(t *testing.T) {
	// P1-3 gating carries over to the explicit path: an unverified Cwd (a
	// lossy decode) must not become the spawn target even on request.
	m := New()
	m.loops = []domain.Loop{{Project: "myproject", SessionID: "sess-1", Cwd: "/x/myproject", CwdVerified: false, State: domain.StateStalled}}
	m.cursor = 0
	m = reachWizardWhere(t, m)
	before := m.spawnCwd

	m, _ = updateModel(t, m, runeKey('s'))

	if m.spawnCwd != before {
		t.Errorf("spawnCwd = %q, want unchanged %q (unverified selection)", m.spawnCwd, before)
	}
}

func TestUpdate_WizardEngineDrive_CKey_OpensDirStep_ReturnsToEngineDrive(t *testing.T) {
	// engine-drive spawns headless in spawnCwd without ever reaching
	// wizardWhere — [c] here is the engine path's only dir control, and a
	// valid entry must return to the engine-drive choice, not wizardWhere.
	dir := t.TempDir()
	m := reachWizardEngineDrive(t, modelWithOneLoop())

	m, _ = updateModel(t, m, runeKey('c'))
	if m.spawnStep != wizardDir {
		t.Fatalf("spawnStep = %v, want wizardDir", m.spawnStep)
	}
	m.input.SetValue(dir)
	m, _ = updateModel(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	if m.spawnStep != wizardEngineDrive {
		t.Errorf("spawnStep = %v, want back at wizardEngineDrive", m.spawnStep)
	}
	if m.spawnCwd != dir {
		t.Errorf("spawnCwd = %q, want the entered dir %q", m.spawnCwd, dir)
	}
}

// ── LoopEngine MVP Slice 2: wizardEngineDrive gating + routing ──────────
//
// The standing spike discipline, verified here: the engine is
// reachable ONLY behind the explicit env opt-in (engineEnabledFn) — when
// it's off (the default), the wizard's behavior is byte-for-byte the
// manual path that existed before this step did.

// reachWizardEngineDrive drives the wizard through steps 1-5 with the
// engine gate forced on (withEngineEnabled), landing at wizardEngineDrive
// — mirrors reachWizardWhere's shape exactly.
func reachWizardEngineDrive(t *testing.T, m Model) Model {
	t.Helper()
	withEngineEnabled(t, true)
	m, _ = updateModel(t, m, runeKey('n'))
	m, _ = typeAndEnter(t, m, "goal")
	m, _ = typeAndEnter(t, m, "")
	m, _ = typeAndEnter(t, m, "")
	m, _ = typeAndEnter(t, m, "")
	m, _ = typeAndEnter(t, m, "")
	m, _ = typeAndEnter(t, m, "")
	if m.mode != modePrompting || m.spawnStep != wizardEngineDrive {
		t.Fatalf("precondition failed: mode=%v step=%v, want modePrompting at wizardEngineDrive", m.mode, m.spawnStep)
	}
	return m
}

// TestWizard_EngineDisabled_StepAbsent_ManualPathByteForByte is the
// requirement to regression-test this: with the engine gate off (the
// default — no override at all here, proving the REAL default, not just an
// explicit false), the wizard must behave EXACTLY as it did before
// wizardEngineDrive existed — landing at wizardWhere when eligible, same
// as TestWizard_ReachesWhereStep_WhenEligible.
func TestWizard_EngineDisabled_StepAbsent_ManualPathByteForByte(t *testing.T) {
	m := reachWizardWhere(t, modelWithOneLoop())
	if m.spawnStep != wizardWhere {
		t.Errorf("spawnStep = %v, want wizardWhere — engine-drive step must be entirely absent when disabled", m.spawnStep)
	}
}

// TestWizard_EngineDisabled_NotEligible_ReachesWhere_ManualPathByteForByte
// is the OTHER manual-path fork (no worktree-capable backend) under the
// same engine-disabled default — mirrors TestWizard_ShowsWhereStep_EvenWhenNotEligible
// exactly, confirming that path is ALSO the same with the engine gate off.
func TestWizard_EngineDisabled_NotEligible_ReachesWhere_ManualPathByteForByte(t *testing.T) {
	m := modelWithOneLoop()
	m, _ = updateModel(t, m, runeKey('n'))
	m, _ = typeAndEnter(t, m, "goal")
	m, _ = typeAndEnter(t, m, "")
	m, _ = typeAndEnter(t, m, "")
	m, _ = typeAndEnter(t, m, "")
	m, _ = typeAndEnter(t, m, "")
	m, _ = typeAndEnter(t, m, "") // step 6: max cycles (name step shifted everything by one)

	if m.mode != modePrompting || m.spawnStep != wizardWhere {
		t.Fatalf("got mode=%v step=%v, want modePrompting at wizardWhere (no engine-drive step)", m.mode, m.spawnStep)
	}
	m, cmd := updateModel(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.mode != modeNormal {
		t.Errorf("mode = %v, want modeNormal after enter", m.mode)
	}
	if cmd == nil {
		t.Fatal("expected a non-nil tea.Cmd (spawnCmd) — the manual path, unchanged")
	}
}

// TestWizard_EngineEnabled_ReachesEngineDriveStep: the step only appears
// once the opt-in gate is on.
func TestWizard_EngineEnabled_ReachesEngineDriveStep(t *testing.T) {
	reachWizardEngineDrive(t, modelWithOneLoop()) // fails via t.Fatalf inside if the precondition isn't met
}

func TestUpdate_WizardEngineDrive_EKey_SubmitsBootstrap(t *testing.T) {
	origBootstrap := bootstrapClaudeFn
	defer func() { bootstrapClaudeFn = origBootstrap }()
	bootstrapClaudeFn = func(ctx context.Context, cwd, prompt string) ([]byte, error) {
		return []byte(`{"session_id":"s-new"}`), nil
	}
	registryDirFn = func() string { return t.TempDir() }
	historyDirFn = func() string { return t.TempDir() }

	m := reachWizardEngineDrive(t, modelWithOneLoop())

	m, cmd := updateModel(t, m, runeKey('e'))

	if m.mode != modeNormal {
		t.Errorf("mode = %v, want modeNormal", m.mode)
	}
	if cmd == nil {
		t.Fatal("expected a non-nil tea.Cmd (bootstrapEngineCmd)")
	}
	if !strings.Contains(m.status, "bootstrapping") || !strings.Contains(m.status, "cycle 1") {
		t.Errorf("status = %q, want the bootstrapping status line", m.status)
	}
}

// TestUpdate_WizardEngineDrive_MKey_ContinuesManualPath_Eligible: 'm'
// proceeds to EXACTLY wizardWhere, same as if wizardEngineDrive never
// intercepted anything — the manual path continues unmodified past this
// choice.
func TestUpdate_WizardEngineDrive_MKey_ContinuesManualPath_Eligible(t *testing.T) {
	m := reachWizardEngineDrive(t, modelWithOneLoop())
	m.spawnWorktreeEligible = true

	// 'm' advances to wizardWhere (like every other free-text-step
	// advance, it returns textinput.Blink — a non-nil cmd — not a
	// completed submission), so this test checks mode/step, not cmd-nilness.
	m, _ = updateModel(t, m, runeKey('m'))

	if m.mode != modePrompting || m.spawnStep != wizardWhere {
		t.Errorf("got mode=%v step=%v, want modePrompting at wizardWhere", m.mode, m.spawnStep)
	}
}

// TestUpdate_WizardEngineDrive_EnterKey_SameAsM: enter is a synonym for
// 'm' at this step (both mean "manual/observe only").
func TestUpdate_WizardEngineDrive_EnterKey_SameAsM(t *testing.T) {
	m := reachWizardEngineDrive(t, modelWithOneLoop())
	m.spawnWorktreeEligible = true

	m, _ = updateModel(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	if m.mode != modePrompting || m.spawnStep != wizardWhere {
		t.Errorf("got mode=%v step=%v, want modePrompting at wizardWhere (enter == m)", m.mode, m.spawnStep)
	}
}

// TestUpdate_WizardEngineDrive_MKey_ContinuesManualPath_NotEligible: 'm'
// with no worktree-capable backend still proceeds to wizardWhere — the dir
// visibility/choice step is never skipped on the manual path.
func TestUpdate_WizardEngineDrive_MKey_ContinuesManualPath_NotEligible(t *testing.T) {
	m := reachWizardEngineDrive(t, modelWithOneLoop()) // spawnWorktreeEligible left at its zero value (false)

	m, _ = updateModel(t, m, runeKey('m'))

	if m.mode != modePrompting || m.spawnStep != wizardWhere {
		t.Errorf("got mode=%v step=%v, want modePrompting at wizardWhere", m.mode, m.spawnStep)
	}
	if strings.Contains(m.status, "bootstrapping") {
		t.Errorf("status = %q, want no bootstrap status on the manual path", m.status)
	}
}

func TestUpdate_WizardEngineDrive_Esc_Cancels(t *testing.T) {
	m := reachWizardEngineDrive(t, modelWithOneLoop())

	m, cmd := updateModel(t, m, tea.KeyMsg{Type: tea.KeyEsc})

	if m.mode != modeNormal {
		t.Errorf("mode = %v, want modeNormal", m.mode)
	}
	if cmd != nil {
		t.Error("expected no tea.Cmd from cancelling")
	}
}

func TestUpdate_WizardEngineDrive_IgnoresUnrelatedKeys(t *testing.T) {
	m := reachWizardEngineDrive(t, modelWithOneLoop())

	m, cmd := updateModel(t, m, runeKey('x'))

	if cmd != nil {
		t.Error("expected no tea.Cmd for an unrelated key")
	}
	if m.mode != modePrompting || m.spawnStep != wizardEngineDrive {
		t.Errorf("got mode=%v step=%v, want to remain at wizardEngineDrive", m.mode, m.spawnStep)
	}
}

func TestRenderNewLoopPrompt_EngineDriveStep_ShowsChoiceLabel(t *testing.T) {
	m := reachWizardEngineDrive(t, modelWithOneLoop())
	got := renderNewLoopPrompt(m)
	if !strings.Contains(got, "engine-drive") || !strings.Contains(got, "manual") {
		t.Errorf("got %q, want the engine-drive/manual choice label", got)
	}
}

func TestWhereStepLabel_BusyDirNudge(t *testing.T) {
	m := New()
	m.loops = []domain.Loop{{Project: "myproject", SessionID: "sess-1", Cwd: "/x/myproject", State: domain.StateRunning}}
	m.spawnCwd = "/x/myproject"

	if !strings.Contains(m.whereStepLabel(), "dir busy") {
		t.Errorf("whereStepLabel() = %q, want the busy-dir nudge (a fleet loop shares spawnCwd)", m.whereStepLabel())
	}
}

func TestWhereStepLabel_NoBusyNudge_WhenDirEmpty(t *testing.T) {
	m := New()
	m.loops = []domain.Loop{{Project: "myproject", SessionID: "sess-1", Cwd: "/x/other", State: domain.StateRunning}}
	m.spawnCwd = "/x/myproject"

	if strings.Contains(m.whereStepLabel(), "dir busy") {
		t.Errorf("whereStepLabel() = %q, want no busy nudge (no loop shares spawnCwd)", m.whereStepLabel())
	}
}

func TestWhereStepLabel_ShowsTargetDir(t *testing.T) {
	// the spawn target must be visible BEFORE the human commits — the whole
	// point of the where step carrying the dir now.
	m := New()
	m.spawnCwd = "/x/myproject"

	if !strings.Contains(m.whereStepLabel(), "/x/myproject") {
		t.Errorf("whereStepLabel() = %q, want it to name the target dir", m.whereStepLabel())
	}
}

func TestWhereStepLabel_OffersW_OnlyWhenEligible(t *testing.T) {
	m := New()
	m.spawnCwd = "/x/myproject"

	if strings.Contains(m.whereStepLabel(), "new worktree") {
		t.Errorf("whereStepLabel() = %q, want no [w] option when ineligible", m.whereStepLabel())
	}
	m.spawnWorktreeEligible = true
	if !strings.Contains(m.whereStepLabel(), "new worktree") {
		t.Errorf("whereStepLabel() = %q, want the [w] option when eligible", m.whereStepLabel())
	}
}

func TestWhereStepLabel_OffersSelectedDir_OnlyWhenVerifiedAndDifferent(t *testing.T) {
	m := New()
	m.loops = []domain.Loop{{Project: "myproject", SessionID: "sess-1", Cwd: "/x/myproject", CwdVerified: true, State: domain.StateRunning}}
	m.cursor = 0
	m.spawnCwd = "/somewhere/else"

	if !strings.Contains(m.whereStepLabel(), "[s] myproject's dir") {
		t.Errorf("whereStepLabel() = %q, want the [s] option (verified, different dir)", m.whereStepLabel())
	}

	m.spawnCwd = "/x/myproject" // same dir — [s] would be a no-op
	if strings.Contains(m.whereStepLabel(), "[s]") {
		t.Errorf("whereStepLabel() = %q, want no [s] option when the target already IS the selected dir", m.whereStepLabel())
	}

	m.spawnCwd = "/somewhere/else"
	m.loops[0].CwdVerified = false // lossy decode — never offered
	if strings.Contains(m.whereStepLabel(), "[s]") {
		t.Errorf("whereStepLabel() = %q, want no [s] option for an unverified cwd", m.whereStepLabel())
	}
}

func TestEngineDriveStepLabel_ShowsTargetDir(t *testing.T) {
	m := New()
	m.spawnCwd = "/x/myproject"

	if !strings.Contains(m.engineDriveStepLabel(), "/x/myproject") {
		t.Errorf("engineDriveStepLabel() = %q, want it to name the target dir", m.engineDriveStepLabel())
	}
	if !strings.Contains(m.engineDriveStepLabel(), "change dir") {
		t.Errorf("engineDriveStepLabel() = %q, want the [c] change-dir option", m.engineDriveStepLabel())
	}
}

func TestSpawnDirBusyCount(t *testing.T) {
	m := New()
	m.loops = []domain.Loop{
		{SessionID: "s1", Cwd: "/x/myproject"},
		{SessionID: "s2", Cwd: "/x/myproject"},
		{SessionID: "s3", Cwd: "/x/other"},
	}
	m.spawnCwd = "/x/myproject"

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
// truncation overflowed the column cell and sheared the whole row (a real
// hazard for any agent transcript containing CJK DOING snippets).
func TestTrunc_CJKDisplayWidth(t *testing.T) {
	got := trunc("字字字字字字字字字字字字", 10)
	if w := runewidth.StringWidth(got); w > 10 {
		t.Errorf("trunc CJK display width = %d, want <= 10 (%q)", w, got)
	}
	if got := trunc("short", 10); got != "short" {
		t.Errorf("ascii under width must pass through, got %q", got)
	}
	mixed := trunc("fix字字mix字字字123456", 12)
	if w := runewidth.StringWidth(mixed); w > 12 {
		t.Errorf("mixed trunc width = %d, want <= 12 (%q)", w, mixed)
	}
}

// Regression (#44): trunc must not inherit the process locale. go-runewidth's
// DefaultCondition auto-detects it and widens East Asian Ambiguous glyphs
// ("…", "●", "◆", box-drawing) to 2 columns under ko/ja/zh, while lipgloss and
// iTerm2 both draw them as 1 — so an inherited condition made trunc reserve a
// column nothing else used, cutting text one column early for exactly the
// users whose transcripts most need the room. trunc pins its own condition
// (narrowAmbiguous); this asserts that pin holds by moving the global out from
// under it, which is what a ko_KR.UTF-8 machine does at init.
func TestTrunc_IgnoresAmbientLocaleCondition(t *testing.T) {
	const width = 5
	// each input is exactly `width` columns once truncated, and contains an
	// Ambiguous glyph either in the text or via trunc's own "…" marker.
	inputs := []string{"aaaaa bbbbb", "●●●●●●●●", "abcdefgh"}

	saved := runewidth.DefaultCondition.EastAsianWidth
	t.Cleanup(func() { runewidth.DefaultCondition.EastAsianWidth = saved })

	runewidth.DefaultCondition.EastAsianWidth = false
	narrow := make([]string, len(inputs))
	for i, in := range inputs {
		narrow[i] = trunc(in, width)
	}

	runewidth.DefaultCondition.EastAsianWidth = true // simulate a ko/ja/zh locale
	for i, in := range inputs {
		if got := trunc(in, width); got != narrow[i] {
			t.Errorf("trunc(%q, %d) = %q under an East Asian ambient locale, want %q (identical) — the pin leaked",
				in, width, got, narrow[i])
		}
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
	// (a fresh Model, as if fleetops just started), and the very FIRST
	// scan already shows a loop sitting in StateGate — there is no
	// "previous scan" to diff against, yet this must still notify once.
	m := New()
	newLoops := []domain.Loop{{SessionID: "s1", Project: "myproject", State: domain.StateGate, GatePrompt: "continue?"}}

	got, _ := m.detectTransitions(newLoops, time.Now())

	if len(got) != 1 {
		t.Fatalf("got %d transitions for an already-gated first appearance, want 1 (seeded)", len(got))
	}
	te := got[0]
	if !te.notify {
		t.Error("expected the seeded edge to be flagged for notify")
	}
	if te.title != notifyTitlePrefix+"fleetops · GATE" {
		t.Errorf("title = %q, want the GATE title", te.title)
	}
	if !strings.Contains(te.body, "myproject") || !strings.Contains(te.body, "continue?") {
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
	loops := []domain.Loop{{SessionID: "s1", Project: "myproject", State: domain.StateGate, GatePrompt: "continue?"}}

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
	if got[0].title != notifyTitlePrefix+"fleetops · loop gone" {
		t.Errorf("title = %q, want the loop-gone title", got[0].title)
	}
	// P2 review fix regression: the PERSISTED FromState/ToState must also
	// differ, not just the in-memory notify decision — otherwise
	// `fleetops report`'s FromState!=ToState transition counting (and a
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
	m.loops = []domain.Loop{{SessionID: "s1", Project: "myproject", State: domain.StateRunning}}
	newLoops := []domain.Loop{{SessionID: "s1", Project: "myproject", State: domain.StateGate, GatePrompt: "continue?"}}

	got, _ := m.detectTransitions(newLoops, time.Now())

	if len(got) != 1 || !got[0].notify {
		t.Fatalf("got %#v, want exactly one notify-flagged transition", got)
	}
	if got[0].title != notifyTitlePrefix+"fleetops · GATE" {
		t.Errorf("title = %q, want the GATE title", got[0].title)
	}
	if !strings.Contains(got[0].body, "myproject") || !strings.Contains(got[0].body, "continue?") {
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
		{ev: events.Event{SessionID: "s1", FromState: "running", ToState: "gate", Trigger: events.TriggerScan, Actor: events.ActorSystem}, notify: true, title: "fleetops · GATE", body: "myproject: continue?"},
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
	if gotTitle != "fleetops · GATE" || gotBody != "myproject: continue?" {
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
	fakeCtrl := &fakeActuator{backend: "tmux"}
	withFakeActuationSeams(t,
		func(sessionsDir, sessionID, projectDir string) (control.Actuator, bool, bool) {
			return fakeCtrl, true, true
		},
		nil,
	)
	l := domain.Loop{SessionID: "sess-1", Project: "myproject", State: domain.StateStalled}

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

// TestSendPromptCmd_TierOneHSuccess_RecordsTierOneHLabel is the reason
// Actuator.Tier() exists at all. The actuation event log is the ONLY post-hoc
// way to tell an in-place iTerm2 write from a multiplexer send when debugging a
// misrouted keystroke; if 1h logged "tier1" the two mechanisms would be
// indistinguishable in the record and the field would be decoration.
func TestSendPromptCmd_TierOneHSuccess_RecordsTierOneHLabel(t *testing.T) {
	withFakeActuationSeams(t,
		func(sessionsDir, sessionID, projectDir string) (control.Actuator, bool, bool) {
			return &fakeActuator{backend: "iterm2", tier: "tier1h"}, true, true
		},
		nil,
	)
	l := domain.Loop{SessionID: "sess-1", Project: "myproject", State: domain.StateStalled}

	sendPromptCmd(l, "do the thing", "inject", "injected into", "")()

	got, err := events.ReadAll(historyDirFn())
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	evs := got["sess-1"]
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1: %#v", len(evs), evs)
	}
	if !strings.Contains(evs[0].Detail, "tier1h") {
		t.Errorf("Detail = %q, want the tier1h label", evs[0].Detail)
	}
}

// TestSendPromptCmd_TierOneHFailureThenTierTwo_RecordsBoth: the degraded path
// must leave BOTH facts in the log — the 1h attempt and why it failed, then the
// Tier 2 redrive that actually delivered. Logging only the outcome would erase
// the evidence that the host send is misbehaving, which is exactly the signal
// design §5.3 names as the SLI to watch after rollout.
func TestSendPromptCmd_TierOneHFailureThenTierTwo_RecordsBoth(t *testing.T) {
	withFakeActuationSeams(t,
		func(sessionsDir, sessionID, projectDir string) (control.Actuator, bool, bool) {
			return &fakeActuator{backend: "iterm2", tier: "tier1h", resumeErr: control.ErrSendTTYMismatch}, true, true
		},
		func(sessionID, prompt string) error { return nil },
	)
	l := domain.Loop{SessionID: "sess-1", Project: "myproject", State: domain.StateStalled}

	sendPromptCmd(l, "do the thing", "resume", "resumed", "")()

	got, err := events.ReadAll(historyDirFn())
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	evs := got["sess-1"]
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2 (the tier1h failure AND the tier2 success): %#v", len(evs), evs)
	}
	if !strings.Contains(evs[0].Detail, "tier1h") || !strings.Contains(evs[0].Detail, "failed") {
		t.Errorf("first event Detail = %q, want the tier1h failure", evs[0].Detail)
	}
	if !strings.Contains(evs[1].Detail, "tier2") || !strings.Contains(evs[1].Detail, "ok") {
		t.Errorf("second event Detail = %q, want the tier2 success", evs[1].Detail)
	}
}

func TestSendPromptCmd_StateFailedRefusal_NoActuationEventRecorded(t *testing.T) {
	historyDir := t.TempDir()
	origHistoryDir := historyDirFn
	defer func() { historyDirFn = origHistoryDir }()
	historyDirFn = func() string { return historyDir }

	l := domain.Loop{SessionID: "sess-1", Project: "myproject", State: domain.StateFailed}
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
	l := domain.Loop{SessionID: "s1", Project: "myproject", State: domain.StateStalled, Stall: domain.StallRateLimit}

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
	l := domain.Loop{SessionID: "s1", Project: "myproject", State: domain.StateStalled, Stall: domain.StallRateLimit}
	now := time.Now()

	cmd := m.maybeScheduleAutoRedrive429(l, true, now)

	if cmd == nil {
		t.Fatal("expected a non-nil tea.Cmd (the scheduled tea.Tick)")
	}
	if !strings.Contains(m.status, "auto: re-driving myproject in 5m (attempt 1/3)") {
		t.Errorf("status = %q, want the scheduled-status text", m.status)
	}
	if got, ok := m.autoRedriveScheduledAt["s1"]; !ok || !got.Equal(now) {
		t.Errorf("autoRedriveScheduledAt[s1] = %v, ok=%v, want %v", got, ok, now)
	}
}

func TestMaybeScheduleAutoRedrive429_NotAnEdge_NoSchedule(t *testing.T) {
	withAutoRedriveEnabled(t, true)
	m := New()
	l := domain.Loop{SessionID: "s1", Project: "myproject", State: domain.StateStalled, Stall: domain.StallRateLimit}

	if cmd := m.maybeScheduleAutoRedrive429(l, false, time.Now()); cmd != nil {
		t.Error("expected nil — enteredRateLimit=false is not a fresh edge")
	}
}

// ── dedup window ──────────────────────────────────────────────────────────

func TestMaybeScheduleAutoRedrive429_DedupWindow_SecondCallWithinWindowRefused(t *testing.T) {
	withAutoRedriveEnabled(t, true)
	historyDirFn = func() string { return t.TempDir() }
	m := New()
	l := domain.Loop{SessionID: "s1", Project: "myproject", State: domain.StateStalled, Stall: domain.StallRateLimit}
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
	l := domain.Loop{SessionID: "s1", Project: "myproject", State: domain.StateStalled, Stall: domain.StallRateLimit}
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
	l := domain.Loop{SessionID: "s1", Project: "myproject", State: domain.StateStalled, Stall: domain.StallRateLimit}

	if cmd := m.maybeScheduleAutoRedrive429(l, true, time.Now()); cmd != nil {
		t.Error("expected nil — already at the lifetime attempt ceiling")
	}
}

func TestMaybeScheduleAutoRedrive429_BelowCeiling_Schedules(t *testing.T) {
	withAutoRedriveEnabled(t, true)
	historyDirFn = func() string { return t.TempDir() }
	m := New()
	m.autoRedriveAttempts = map[string]int{"s1": autoRedriveMaxAttempts - 1}
	l := domain.Loop{SessionID: "s1", Project: "myproject", State: domain.StateStalled, Stall: domain.StallRateLimit}

	if cmd := m.maybeScheduleAutoRedrive429(l, true, time.Now()); cmd == nil {
		t.Error("expected a non-nil cmd — one attempt below the ceiling")
	}
}

// ── gate/failed defense in depth ─────────────────────────────────────────

func TestMaybeScheduleAutoRedrive429_StateFailed_NeverSchedules(t *testing.T) {
	withAutoRedriveEnabled(t, true)
	m := New()
	l := domain.Loop{SessionID: "s1", Project: "myproject", State: domain.StateFailed, Stall: domain.StallRateLimit}

	if cmd := m.maybeScheduleAutoRedrive429(l, true, time.Now()); cmd != nil {
		t.Error("expected nil — StateFailed must never auto-redrive")
	}
}

func TestMaybeScheduleAutoRedrive429_StateGate_NeverSchedules(t *testing.T) {
	withAutoRedriveEnabled(t, true)
	m := New()
	l := domain.Loop{SessionID: "s1", Project: "myproject", State: domain.StateGate}

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
	m.loops = []domain.Loop{{SessionID: "s1", Project: "myproject", State: domain.StateRunning}}
	newLoops := []domain.Loop{{SessionID: "s1", Project: "myproject", State: domain.StateStalled, Stall: domain.StallRateLimit}}

	_, cmds := m.detectTransitions(newLoops, time.Now())

	if len(cmds) != 1 {
		t.Fatalf("got %d auto-redrive cmds, want 1", len(cmds))
	}
}

func TestDetectTransitions_EnteredRateLimit_OptedOut_NoSchedule(t *testing.T) {
	withAutoRedriveEnabled(t, false)
	m := New()
	m.loops = []domain.Loop{{SessionID: "s1", Project: "myproject", State: domain.StateRunning}}
	newLoops := []domain.Loop{{SessionID: "s1", Project: "myproject", State: domain.StateStalled, Stall: domain.StallRateLimit}}

	_, cmds := m.detectTransitions(newLoops, time.Now())

	if len(cmds) != 0 {
		t.Errorf("got %d auto-redrive cmds, want 0 (opted out)", len(cmds))
	}
}

func TestDetectTransitions_AlreadyRateLimited_NotANewEdge_NoSchedule(t *testing.T) {
	withAutoRedriveEnabled(t, true)
	m := New()
	m.loops = []domain.Loop{{SessionID: "s1", Project: "myproject", State: domain.StateStalled, Stall: domain.StallRateLimit}}
	newLoops := []domain.Loop{{SessionID: "s1", Project: "myproject", State: domain.StateStalled, Stall: domain.StallRateLimit}}

	_, cmds := m.detectTransitions(newLoops, time.Now())

	if len(cmds) != 0 {
		t.Errorf("got %d auto-redrive cmds, want 0 — already rate-limited last scan, not a fresh edge", len(cmds))
	}
}

// ── autoRedriveScheduledMsg: re-check at fire time ────────────────────────

func TestUpdate_AutoRedriveScheduledMsg_StillRateLimited_FiresRedrive(t *testing.T) {
	withFakeActuationSeams(t, nil, func(sessionID, prompt string) error { return nil })
	m := New()
	m.loops = []domain.Loop{{SessionID: "s1", Project: "myproject", State: domain.StateStalled, Stall: domain.StallRateLimit}}

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
	m.loops = []domain.Loop{{SessionID: "s1", Project: "myproject", State: domain.StateRunning}} // recovered — no longer rate-limited

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
	m.loops = []domain.Loop{{SessionID: "s1", Project: "myproject", State: domain.StateGate}} // hit a gate during the delay
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
	m.loops = []domain.Loop{{SessionID: "s1", Project: "myproject", State: domain.StateStalled, Stall: domain.StallRateLimit}}
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
	m.loops = []domain.Loop{{SessionID: "s1", Project: "myproject", State: domain.StateStalled, Stall: domain.StallRateLimit}}

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

	m, _ = updateModel(t, m, autoRedriveResultMsg{sessionID: "s1", project: "myproject", attempt: 1, ok: true})

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

	l := domain.Loop{SessionID: "s1", Project: "myproject", State: domain.StateStalled, Stall: domain.StallRateLimit}
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

	l := domain.Loop{SessionID: "s1", Project: "myproject", State: domain.StateStalled, Stall: domain.StallRateLimit}
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
	_, cmd := updateModel(t, m, autoRedriveResultMsg{sessionID: "s1", project: "myproject", attempt: autoRedriveMaxAttempts, ok: true})

	if cmd == nil {
		t.Fatal("expected a non-nil tea.Cmd (autoRedriveExhaustedNotifyCmd) even though the final attempt succeeded")
	}
}

func TestUpdate_AutoRedriveResultMsg_FinalAttemptFailure_NotifiesExhausted(t *testing.T) {
	m := New()
	_, cmd := updateModel(t, m, autoRedriveResultMsg{sessionID: "s1", project: "myproject", attempt: autoRedriveMaxAttempts, ok: false})
	if cmd == nil {
		t.Fatal("expected a non-nil tea.Cmd (autoRedriveExhaustedNotifyCmd)")
	}
}

func TestUpdate_AutoRedriveResultMsg_NonFinalAttempt_NoNotification(t *testing.T) {
	m := New()
	_, cmd1 := updateModel(t, m, autoRedriveResultMsg{sessionID: "s1", project: "myproject", attempt: 1, ok: false})
	if cmd1 != nil {
		t.Error("expected nil — attempt 1 of 3 is not the ceiling, no exhaustion yet")
	}
	m2 := New()
	_, cmd2 := updateModel(t, m2, autoRedriveResultMsg{sessionID: "s1", project: "myproject", attempt: 1, ok: true})
	if cmd2 != nil {
		t.Error("expected nil — attempt 1 of 3, even on success, is not the ceiling")
	}
}

func TestUpdate_AutoRedriveResultMsg_DedupedNotifyOnlyOnce(t *testing.T) {
	m := New()
	m, cmd1 := updateModel(t, m, autoRedriveResultMsg{sessionID: "s1", project: "myproject", attempt: autoRedriveMaxAttempts, ok: false})
	if cmd1 == nil {
		t.Fatal("expected the first exhaustion to notify")
	}
	_, cmd2 := updateModel(t, m, autoRedriveResultMsg{sessionID: "s1", project: "myproject", attempt: autoRedriveMaxAttempts, ok: false})
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

	autoRedriveExhaustedNotifyCmd("myproject")()

	if gotTitle != notifyTitlePrefix+"fleetops · auto-redrive exhausted" {
		t.Errorf("title = %q, want the exhausted title", gotTitle)
	}
	if gotBody != "myproject" {
		t.Errorf("body = %q, want the project label", gotBody)
	}
}

// ── LoopEngine MVP: opt-in kill-switch seam ──────────────────────────────
//
// engineEnabledFn is not called from anywhere yet in this slice (a later
// slice's triggerDrives is the first caller) — these tests pin the seam's
// contract now so that slice can trust it without re-deriving/re-testing
// the env-var behavior itself.

func TestEngineEnabledFn_Unset_DefaultsFalse(t *testing.T) {
	t.Setenv("FLEETOPS_ENGINE", "") // t.Setenv auto-restores on cleanup; "" mirrors "not set" for this equality check
	if engineEnabledFn() {
		t.Error("expected false — the engine is off by default")
	}
}

func TestEngineEnabledFn_SetToOne_True(t *testing.T) {
	t.Setenv("FLEETOPS_ENGINE", "1")
	if !engineEnabledFn() {
		t.Error("expected true with FLEETOPS_ENGINE=1")
	}
}

// TestEngineEnabledFn_AnyOtherValue_False pins the SAME strict-equality
// contract autoRedriveEnabledFn already has ("==\"1\"", not a truthy
// parse) — "true"/"yes"/"2" must NOT enable the engine, only the exact
// string "1". A kill-switch's opt-in side should never have surprising
// truthy-string ambiguity.
func TestEngineEnabledFn_AnyOtherValue_False(t *testing.T) {
	for _, v := range []string{"true", "yes", "0", "2", "TRUE"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("FLEETOPS_ENGINE", v)
			if engineEnabledFn() {
				t.Errorf("FLEETOPS_ENGINE=%q: expected false — only the exact string \"1\" enables the engine", v)
			}
		})
	}
}

// withEngineEnabled overrides engineEnabledFn to the given value for the
// duration of one test, restoring the original on cleanup — mirrors
// withAutoRedriveEnabled exactly, ready for the later slice that actually
// calls engineEnabledFn from wiring logic.
func withEngineEnabled(t *testing.T, enabled bool) {
	t.Helper()
	orig := engineEnabledFn
	t.Cleanup(func() { engineEnabledFn = orig })
	engineEnabledFn = func() bool { return enabled }
}

// TestWithEngineEnabled_OverridesAndRestores proves the test seam itself
// works: a subtest's override takes effect during the subtest, and is
// restored (subtest t.Cleanup runs at subtest end, before the parent
// resumes) by the time the parent test observes engineEnabledFn again —
// cheap insurance so a later slice's tests can trust withEngineEnabled
// blindly.
func TestWithEngineEnabled_OverridesAndRestores(t *testing.T) {
	orig := engineEnabledFn
	defer func() { engineEnabledFn = orig }()      // belt-and-suspenders: don't leak into other tests even if this one fails oddly
	engineEnabledFn = func() bool { return false } // known starting value, independent of the real env

	t.Run("override", func(t *testing.T) {
		withEngineEnabled(t, true)
		if !engineEnabledFn() {
			t.Error("expected true while overridden")
		}
	})

	if engineEnabledFn() {
		t.Error("expected false after the subtest ended — withEngineEnabled's cleanup must restore the prior value")
	}
}

// ── LoopEngine MVP Slice 2: bootstrap envelope parsing ───────────────────

func TestParseBootstrapSessionID_ValidJSON(t *testing.T) {
	got, ok := parseBootstrapSessionID([]byte(`{"session_id":"sess-abc-123","result":"done","is_error":false}`))
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got != "sess-abc-123" {
		t.Errorf("got %q, want %q", got, "sess-abc-123")
	}
}

func TestParseBootstrapSessionID_MissingSessionID_NotOK(t *testing.T) {
	if _, ok := parseBootstrapSessionID([]byte(`{"result":"done","is_error":false}`)); ok {
		t.Error("expected ok=false — no session_id field at all")
	}
}

func TestParseBootstrapSessionID_EmptySessionID_NotOK(t *testing.T) {
	if _, ok := parseBootstrapSessionID([]byte(`{"session_id":"","result":"done"}`)); ok {
		t.Error("expected ok=false — an empty session_id must not count as found")
	}
}

func TestParseBootstrapSessionID_NotJSONAtAll_NotOK(t *testing.T) {
	if _, ok := parseBootstrapSessionID([]byte("claude: command not found")); ok {
		t.Error("expected ok=false for stdout that isn't JSON at all (no session_id substring either)")
	}
}

// TestParseBootstrapSessionID_ControlCharInResult_FallsBackToRegex is the
// exact live-verification caveat: claude -p --output-format
// json's "result" field can carry a raw (unescaped) control character that
// trips strict encoding/json parsing for the object as a WHOLE, even
// though session_id itself — elsewhere in the same object — is perfectly
// well-formed. The lenient regex fallback must still find it.
func TestParseBootstrapSessionID_ControlCharInResult_FallsBackToRegex(t *testing.T) {
	// a literal, unescaped 0x01 byte inside the "result" string value —
	// invalid per strict JSON string-literal rules, but session_id is
	// still extractable via the fallback regex.
	raw := []byte("{\"session_id\":\"sess-ctrl-1\",\"result\":\"partial output \x01 more text\",\"is_error\":false}")

	// sanity: confirm this fixture actually DOES fail strict decoding,
	// otherwise this test would be exercising the wrong path.
	var probe bootstrapEnvelope
	if err := json.Unmarshal(raw, &probe); err == nil {
		t.Fatalf("test fixture is invalid: expected the raw control char to break strict JSON decoding, but it parsed cleanly as %+v", probe)
	}

	got, ok := parseBootstrapSessionID(raw)
	if !ok {
		t.Fatal("expected ok=true via the lenient regex fallback")
	}
	if got != "sess-ctrl-1" {
		t.Errorf("got %q, want %q", got, "sess-ctrl-1")
	}
}

// ── LoopEngine MVP Slice 2: bootstrapEngineCmd ───────────────────────────

// withFakeBootstrapClaude overrides bootstrapClaudeFn for the duration of
// one test, restoring the original on cleanup.
func withFakeBootstrapClaude(t *testing.T, fn func(ctx context.Context, cwd, prompt string) ([]byte, error)) {
	t.Helper()
	orig := bootstrapClaudeFn
	t.Cleanup(func() { bootstrapClaudeFn = orig })
	bootstrapClaudeFn = fn
}

func TestBootstrapEngineCmd_Success_BindsWithDrivenTrue_EmitsEvent(t *testing.T) {
	loopsDir, historyDir := t.TempDir(), t.TempDir()
	origRegistryDir, origHistoryDir := registryDirFn, historyDirFn
	defer func() { registryDirFn, historyDirFn = origRegistryDir, origHistoryDir }()
	registryDirFn = func() string { return loopsDir }
	historyDirFn = func() string { return historyDir }

	var gotCwd, gotPrompt string
	withFakeBootstrapClaude(t, func(ctx context.Context, cwd, prompt string) ([]byte, error) {
		gotCwd, gotPrompt = cwd, prompt
		return []byte(`{"session_id":"sess-boot-1","result":"cycle 1 done","is_error":false}`), nil
	})

	spec := registry.BindSpec{Goal: "fix the flaky test", DoneCondition: "tests pass", Rubric: "run go test", MaxCycles: 8}
	msg := bootstrapEngineCmd("/x/myproject", spec)()

	rm, ok := msg.(bootstrapResultMsg)
	if !ok {
		t.Fatalf("got %T, want bootstrapResultMsg", msg)
	}
	if !rm.ok {
		t.Fatalf("ok = false, want true; text = %q", rm.text)
	}
	if rm.sessionID != "sess-boot-1" {
		t.Errorf("sessionID = %q, want %q", rm.sessionID, "sess-boot-1")
	}
	if gotCwd != "/x/myproject" {
		t.Errorf("bootstrapClaudeFn was called with cwd=%q, want /x/myproject", gotCwd)
	}
	if !strings.Contains(gotPrompt, "fix the flaky test") || !strings.Contains(gotPrompt, "tests pass") {
		t.Errorf("prompt = %q, want the composed contract (buildSpawnPrompt output)", gotPrompt)
	}

	rec, found := registry.Load(loopsDir, "sess-boot-1")
	if !found {
		t.Fatal("expected a registry record for sess-boot-1")
	}
	if !rec.Driven {
		t.Error("Driven = false, want true — bootstrap must always create a DRIVEN record")
	}
	if rec.Goal != "fix the flaky test" || rec.DoneCondition != "tests pass" || rec.Rubric != "run go test" || rec.MaxCycles != 8 {
		t.Errorf("got %+v, want the full contract bound", rec)
	}

	evs, err := events.ReadAll(historyDir)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	sessEvs := evs["sess-boot-1"]
	if len(sessEvs) != 1 {
		t.Fatalf("got %d events for sess-boot-1, want 1", len(sessEvs))
	}
	if sessEvs[0].Trigger != events.TriggerEngine {
		t.Errorf("Trigger = %v, want TriggerEngine", sessEvs[0].Trigger)
	}
	if sessEvs[0].Actor != events.ActorAuto {
		t.Errorf("Actor = %v, want ActorAuto", sessEvs[0].Actor)
	}
	if !strings.Contains(sessEvs[0].Detail, "fix the flaky test") {
		t.Errorf("Detail = %q, want it to mention the goal", sessEvs[0].Detail)
	}
}

// TestBootstrapEngineCmd_CallerDrivenFalse_StillBindsDrivenTrue is the
// defense-in-depth check: even if a (hypothetical, buggy) caller passes
// spec.Driven=false, bootstrapEngineCmd must still Bind Driven=true — its
// entire reason to exist is creating an engine-driven loop.
func TestBootstrapEngineCmd_CallerDrivenFalse_StillBindsDrivenTrue(t *testing.T) {
	loopsDir := t.TempDir()
	origRegistryDir, origHistoryDir := registryDirFn, historyDirFn
	defer func() { registryDirFn, historyDirFn = origRegistryDir, origHistoryDir }()
	registryDirFn = func() string { return loopsDir }
	historyDirFn = func() string { return t.TempDir() }
	withFakeBootstrapClaude(t, func(ctx context.Context, cwd, prompt string) ([]byte, error) {
		return []byte(`{"session_id":"sess-boot-2"}`), nil
	})

	spec := registry.BindSpec{Goal: "goal", Driven: false} // deliberately wrong, to prove the function doesn't trust it
	bootstrapEngineCmd("/x/myproject", spec)()

	rec, found := registry.Load(loopsDir, "sess-boot-2")
	if !found {
		t.Fatal("expected a registry record")
	}
	if !rec.Driven {
		t.Error("Driven = false, want true — bootstrapEngineCmd must assert Driven=true regardless of the caller's spec")
	}
}

func TestBootstrapEngineCmd_ExecError_NoRecordCreated(t *testing.T) {
	loopsDir := t.TempDir()
	origRegistryDir, origHistoryDir := registryDirFn, historyDirFn
	defer func() { registryDirFn, historyDirFn = origRegistryDir, origHistoryDir }()
	registryDirFn = func() string { return loopsDir }
	historyDirFn = func() string { return t.TempDir() }
	withFakeBootstrapClaude(t, func(ctx context.Context, cwd, prompt string) ([]byte, error) {
		return nil, errTestJudgeFailed // any non-nil error — the sentinel already used elsewhere in this file for exactly this purpose
	})

	spec := registry.BindSpec{Goal: "goal"}
	msg := bootstrapEngineCmd("/x/myproject", spec)()

	rm, ok := msg.(bootstrapResultMsg)
	if !ok {
		t.Fatalf("got %T, want bootstrapResultMsg", msg)
	}
	if rm.ok {
		t.Error("ok = true, want false — the exec call failed")
	}
	if !strings.Contains(rm.text, "engine bootstrap failed") {
		t.Errorf("text = %q, want it to explain the failure", rm.text)
	}
	if rm.sessionID != "" {
		t.Errorf("sessionID = %q, want empty on failure", rm.sessionID)
	}

	entries, _ := os.ReadDir(loopsDir)
	if len(entries) != 0 {
		t.Errorf("got %d files in loopsDir, want 0 — no phantom record on exec failure", len(entries))
	}
}

func TestBootstrapEngineCmd_NoSessionIDInResponse_NoRecordCreated(t *testing.T) {
	loopsDir := t.TempDir()
	origRegistryDir, origHistoryDir := registryDirFn, historyDirFn
	defer func() { registryDirFn, historyDirFn = origRegistryDir, origHistoryDir }()
	registryDirFn = func() string { return loopsDir }
	historyDirFn = func() string { return t.TempDir() }
	withFakeBootstrapClaude(t, func(ctx context.Context, cwd, prompt string) ([]byte, error) {
		return []byte(`{"result":"something went sideways","is_error":true}`), nil // no session_id at all
	})

	spec := registry.BindSpec{Goal: "goal"}
	msg := bootstrapEngineCmd("/x/myproject", spec)()

	rm, ok := msg.(bootstrapResultMsg)
	if !ok {
		t.Fatalf("got %T, want bootstrapResultMsg", msg)
	}
	if rm.ok {
		t.Error("ok = true, want false — no session_id means no loop was identifiable")
	}
	if !strings.Contains(rm.text, "no session_id") {
		t.Errorf("text = %q, want it to explain the missing session_id", rm.text)
	}

	entries, _ := os.ReadDir(loopsDir)
	if len(entries) != 0 {
		t.Errorf("got %d files in loopsDir, want 0 — no phantom record when session_id is missing", len(entries))
	}
}

// TestBootstrapEngineCmd_ReusesBuildSpawnPromptVerbatim confirms the
// contract sent to claude -p is EXACTLY buildSpawnPrompt's output for the
// same fields — one contract document, not a bootstrap-specific
// reimplementation that could drift from the manual path's.
func TestBootstrapEngineCmd_ReusesBuildSpawnPromptVerbatim(t *testing.T) {
	origRegistryDir, origHistoryDir := registryDirFn, historyDirFn
	defer func() { registryDirFn, historyDirFn = origRegistryDir, origHistoryDir }()
	registryDirFn = func() string { return t.TempDir() }
	historyDirFn = func() string { return t.TempDir() }

	var gotPrompt string
	withFakeBootstrapClaude(t, func(ctx context.Context, cwd, prompt string) ([]byte, error) {
		gotPrompt = prompt
		return []byte(`{"session_id":"s1"}`), nil
	})

	spec := registry.BindSpec{Goal: "ship it", DoneCondition: "tests pass", Rubric: "run tests", Challenger: "adversarial probe", MaxCycles: 5}
	bootstrapEngineCmd("/x/myproject", spec)()

	want := buildSpawnPrompt(spec.Goal, spec.DoneCondition, spec.Rubric, spec.Challenger, spec.MaxCycles)
	if gotPrompt != want {
		t.Errorf("prompt sent to claude -p was NOT buildSpawnPrompt's output:\ngot:  %q\nwant: %q", gotPrompt, want)
	}
}

// ── LoopEngine: triggerDrives / driveCmd (the cycle) ─────────────────────
//
// These tests exercise the TWO-GATE opt-in (engineEnabledFn() env
// kill-switch AND per-loop Driven) and the
// fail-closed drive predicate at the INTEGRATION level — engine.ShouldDrive
// itself already has an exhaustive pure truth table in
// internal/engine/driver_test.go; what's new here is proving triggerDrives
// actually wires that predicate up correctly (kill-switch short-circuit
// before ANY loop is even considered, the shared m.actuating interlock,
// driveCmd's exact event/status/prompt shape).

// engineDriveReadyLoop is a loop that is eligible for a drive under every
// clause of engine.ShouldDrive: Driven, StateIdle, governor Continue (no
// ceilings set), and a FRESH verdict (Last.AtCycle == Cycle) — the
// baseline every fail-closed test below starts from and then breaks
// exactly one clause of.
func engineDriveReadyLoop() domain.Loop {
	return domain.Loop{
		SessionID: "sess-1",
		Project:   "myproject",
		State:     domain.StateIdle,
		Cycle:     2,
		Driven:    true,
		Last:      &domain.Verdict{Outcome: domain.OutcomeProgress, AtCycle: 2},
		Goal:      domain.Goal{Text: "ship it"},
	}
}

func TestTriggerDrives_KillSwitchOff_NoDriveEverFires(t *testing.T) {
	withEngineEnabled(t, false)
	m := New()
	m.loops = []domain.Loop{engineDriveReadyLoop()} // otherwise perfectly eligible

	cmd := m.triggerDrives()

	if cmd != nil {
		t.Error("expected nil cmd — the env kill-switch must block every drive, even for a fully-eligible Driven loop")
	}
	if m.actuating["sess-1"] {
		t.Error("expected no in-flight guard set — triggerDrives must not touch m.loops at all when the kill-switch is off")
	}
}

func TestTriggerDrives_KillSwitchOn_EligibleLoop_DispatchesDriveCmd(t *testing.T) {
	withEngineEnabled(t, true)
	m := New()
	m.loops = []domain.Loop{engineDriveReadyLoop()}

	cmd := m.triggerDrives()

	if cmd == nil {
		t.Fatal("expected a non-nil batch cmd for a fully-eligible Driven loop")
	}
	if !m.actuating["sess-1"] {
		t.Error("expected sess-1 marked in-flight (m.actuating) after dispatch")
	}
}

func TestTriggerDrives_NotDriven_NoDispatch(t *testing.T) {
	withEngineEnabled(t, true)
	m := New()
	l := engineDriveReadyLoop()
	l.Driven = false
	m.loops = []domain.Loop{l}

	if cmd := m.triggerDrives(); cmd != nil {
		t.Error("expected nil cmd — a non-Driven loop must never be engine-drivable, even with the kill-switch on")
	}
	if m.actuating["sess-1"] {
		t.Error("expected no in-flight guard set for a non-Driven loop")
	}
}

// TestTriggerDrives_StateGate_NeverDrives_NoApprovePath is the coordinator's
// explicit fail-closed review-bar test: a live permission prompt / gate must
// NEVER be driven past by the engine — it has no approve path, by
// construction (engine.ShouldDrive's notGated clause). Proven here at the
// triggerDrives integration level, not just ShouldDrive's own pure table.
func TestTriggerDrives_StateGate_NeverDrives_NoApprovePath(t *testing.T) {
	withEngineEnabled(t, true)
	m := New()
	l := engineDriveReadyLoop()
	l.State = domain.StateGate
	m.loops = []domain.Loop{l}

	if cmd := m.triggerDrives(); cmd != nil {
		t.Error("expected nil cmd — StateGate must never be driven; the engine has no approve path")
	}
	if m.actuating["sess-1"] {
		t.Error("expected no in-flight guard set for a gated loop")
	}
}

func TestTriggerDrives_BudgetExhausted_GovernorStop_NoDispatch(t *testing.T) {
	withEngineEnabled(t, true)
	m := New()
	l := engineDriveReadyLoop()
	l.Goal.BudgetTokens = 1000
	l.TokensSpent = 1000 // exhausted
	m.loops = []domain.Loop{l}

	if cmd := m.triggerDrives(); cmd != nil {
		t.Error("expected nil cmd — budget exhausted means governor Escalate, no drive")
	}
}

func TestTriggerDrives_MaxCyclesReached_GovernorEscalate_NoDispatch(t *testing.T) {
	withEngineEnabled(t, true)
	m := New()
	l := engineDriveReadyLoop()
	l.Goal.MaxCycles = 2
	l.Cycle = 2 // reached
	l.Last = &domain.Verdict{Outcome: domain.OutcomeProgress, AtCycle: 2}
	m.loops = []domain.Loop{l}

	if cmd := m.triggerDrives(); cmd != nil {
		t.Error("expected nil cmd — max cycles reached means governor Escalate, no drive (surfaces to human)")
	}
}

func TestTriggerDrives_NoImproveAtLimit_GovernorStop_NoDispatch(t *testing.T) {
	withEngineEnabled(t, true)
	m := New()
	l := engineDriveReadyLoop()
	l.Goal.NoImproveLimit = 3
	l.NoImprove = 3 // at limit
	m.loops = []domain.Loop{l}

	if cmd := m.triggerDrives(); cmd != nil {
		t.Error("expected nil cmd — no-improve ceiling hit means governor Stop, no drive")
	}
}

func TestTriggerDrives_StaleVerdict_RacesAheadOfJudge_NoDispatch(t *testing.T) {
	withEngineEnabled(t, true)
	m := New()
	l := engineDriveReadyLoop()
	l.Cycle = 3
	l.Last = &domain.Verdict{Outcome: domain.OutcomeProgress, AtCycle: 2} // stale — cycle 3 not yet judged
	m.loops = []domain.Loop{l}

	if cmd := m.triggerDrives(); cmd != nil {
		t.Error("expected nil cmd — a stale (unjudged) verdict must never let the engine race ahead of the judge")
	}
}

// TestTriggerDrives_ManualActuationInFlight_BlocksEngineDrive is interlock
// proof, direction 1: a manual r/i already in flight (m.actuating set
// BEFORE triggerDrives runs, as if a human just pressed r/i on this exact
// session) must block the engine from also driving it this tick.
func TestTriggerDrives_ManualActuationInFlight_BlocksEngineDrive(t *testing.T) {
	withEngineEnabled(t, true)
	m := New()
	m.loops = []domain.Loop{engineDriveReadyLoop()}
	m.actuating = map[string]bool{"sess-1": true} // simulate a manual r/i already in flight

	if cmd := m.triggerDrives(); cmd != nil {
		t.Error("expected nil cmd — a manual actuation already in flight on this session must block the engine drive")
	}
}

// TestTriggerDrives_SetsActuating_BlocksSubsequentManualInject is interlock
// proof, direction 2: once triggerDrives dispatches a drive (setting
// m.actuating), a human's SUBSEQUENT "i" keypress on the SAME session must
// be refused by the EXISTING "already re-driving" guard — proving the
// engine and manual actuation share the same interlock map bidirectionally,
// not two independent mechanisms that happen to look similar.
func TestTriggerDrives_SetsActuating_BlocksSubsequentManualInject(t *testing.T) {
	withEngineEnabled(t, true)
	m := modelWithOneLoop()
	l := engineDriveReadyLoop()
	l.SessionID = "sess-1" // matches modelWithOneLoop's fixture session, so m.selected() targets the same loop
	m.loops = []domain.Loop{l}

	cmd := m.triggerDrives()
	if cmd == nil {
		t.Fatal("expected a non-nil cmd — precondition: the engine must actually dispatch a drive first")
	}
	if !m.actuating["sess-1"] {
		t.Fatal("expected sess-1 marked in-flight after the engine's drive — precondition for this test")
	}

	m, cmd = updateModel(t, m, runeKey('i'))

	if cmd != nil {
		t.Error("expected nil cmd — a manual inject must be refused while the engine's drive is in flight")
	}
	if !strings.Contains(m.status, "already re-driving") {
		t.Errorf("status = %q, want the already-re-driving message — the engine and manual actuation must share one interlock", m.status)
	}
}

// TestDriveCmd_Success_EmitsEventAndReturnsResumeResultMsg confirms
// driveCmd's full happy path in one place: the prompt sent to redriveFn is
// EXACTLY engine.NextWorkPrompt's output (reused verbatim, not
// reimplemented), a TriggerEngine/ActorAuto history event lands BEFORE
// dispatch, and the returned resumeResultMsg's status text matches the
// coordinator's exact spec: "engine: cycle N — <goal-slug>".
func TestDriveCmd_Success_EmitsEventAndReturnsResumeResultMsg(t *testing.T) {
	registryDir := t.TempDir()
	historyDir := t.TempDir()
	origRegDir, origHistoryDir, origRedrive := registryDirFn, historyDirFn, redriveFn
	defer func() { registryDirFn, historyDirFn, redriveFn = origRegDir, origHistoryDir, origRedrive }()
	registryDirFn = func() string { return registryDir }
	historyDirFn = func() string { return historyDir }

	if err := registry.Bind(registryDir, "sess-1", registry.BindSpec{Goal: "ship it", DoneCondition: "tests pass", Rubric: "run the suite", Driven: true}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	var gotSessionID, gotPrompt string
	redriveFn = func(sessionID, prompt string) error {
		gotSessionID, gotPrompt = sessionID, prompt
		return nil
	}

	l := engineDriveReadyLoop()
	contract, _ := registry.Load(registryDir, "sess-1")
	wantPrompt := engine.NextWorkPrompt(l, contract)

	msg := driveCmd(l)()

	if gotSessionID != "sess-1" {
		t.Errorf("redriveFn sessionID = %q, want sess-1", gotSessionID)
	}
	if gotPrompt != wantPrompt {
		t.Errorf("redriveFn prompt was NOT engine.NextWorkPrompt's output:\ngot:  %q\nwant: %q", gotPrompt, wantPrompt)
	}

	rm, ok := msg.(resumeResultMsg)
	if !ok {
		t.Fatalf("got %T, want resumeResultMsg (REUSED, not a new message type)", msg)
	}
	if !rm.ok {
		t.Errorf("ok = false, want true: %q", rm.text)
	}
	wantText := "engine: cycle 2 — ship-it"
	if rm.text != wantText {
		t.Errorf("text = %q, want %q", rm.text, wantText)
	}

	got, err := events.ReadAll(historyDir)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	evs := got["sess-1"]
	if len(evs) != 1 {
		t.Fatalf("got %d events, want exactly 1", len(evs))
	}
	if evs[0].Trigger != events.TriggerEngine || evs[0].Actor != events.ActorAuto {
		t.Errorf("event = %+v, want Trigger=TriggerEngine Actor=ActorAuto", evs[0])
	}
	if evs[0].Detail != "cycle 2" {
		t.Errorf("Detail = %q, want %q", evs[0].Detail, "cycle 2")
	}
}

func TestDriveCmd_NoRegistryRecord_GracefulFailure(t *testing.T) {
	registryDir := t.TempDir()
	historyDir := t.TempDir()
	origRegDir, origHistoryDir, origRedrive := registryDirFn, historyDirFn, redriveFn
	defer func() { registryDirFn, historyDirFn, redriveFn = origRegDir, origHistoryDir, origRedrive }()
	registryDirFn = func() string { return registryDir }
	historyDirFn = func() string { return historyDir }
	redriveCalled := false
	redriveFn = func(sessionID, prompt string) error { redriveCalled = true; return nil }

	l := engineDriveReadyLoop() // no matching registry.Bind for sess-1

	msg := driveCmd(l)()

	rm, ok := msg.(resumeResultMsg)
	if !ok {
		t.Fatalf("got %T, want resumeResultMsg", msg)
	}
	if rm.ok {
		t.Error("ok = true, want false — no registry record means the cycle must be skipped, not sent with a zero-value contract")
	}
	if redriveCalled {
		t.Error("redriveFn must not be called when there's no registry record")
	}
}

func TestDriveCmd_RedriveError_ReturnsFailureResult(t *testing.T) {
	registryDir := t.TempDir()
	historyDir := t.TempDir()
	origRegDir, origHistoryDir, origRedrive := registryDirFn, historyDirFn, redriveFn
	defer func() { registryDirFn, historyDirFn, redriveFn = origRegDir, origHistoryDir, origRedrive }()
	registryDirFn = func() string { return registryDir }
	historyDirFn = func() string { return historyDir }

	if err := registry.Bind(registryDir, "sess-1", registry.BindSpec{Goal: "ship it", Driven: true}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	redriveFn = func(sessionID, prompt string) error { return errTestJudgeFailed }

	msg := driveCmd(engineDriveReadyLoop())()

	rm, ok := msg.(resumeResultMsg)
	if !ok {
		t.Fatalf("got %T, want resumeResultMsg", msg)
	}
	if rm.ok {
		t.Error("ok = true, want false — redriveFn returned an error")
	}
	if !strings.Contains(rm.text, "cycle 2 failed") {
		t.Errorf("text = %q, want it to mention the failed cycle", rm.text)
	}
}

// TestDriveCmd_ResumeResultMsg_ClearsActuating confirms the EXISTING
// resumeResultMsg Update handler (unchanged by this slice, reused verbatim
// by design) correctly clears m.actuating when
// the message originated from an engine-driven cycle, not just a manual
// r/i — the whole point of reusing the message type rather than adding a
// new one.
func TestDriveCmd_ResumeResultMsg_ClearsActuating(t *testing.T) {
	registryDir := t.TempDir()
	historyDir := t.TempDir()
	origRegDir, origHistoryDir, origRedrive := registryDirFn, historyDirFn, redriveFn
	defer func() { registryDirFn, historyDirFn, redriveFn = origRegDir, origHistoryDir, origRedrive }()
	registryDirFn = func() string { return registryDir }
	historyDirFn = func() string { return historyDir }
	if err := registry.Bind(registryDir, "sess-1", registry.BindSpec{Goal: "ship it", Driven: true}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	redriveFn = func(sessionID, prompt string) error { return nil }

	m := New()
	m.actuating = map[string]bool{"sess-1": true}

	msg := driveCmd(engineDriveReadyLoop())()
	m, _ = updateModel(t, m, msg)

	if m.actuating["sess-1"] {
		t.Error("expected m.actuating cleared after the driveCmd result lands, via the existing resumeResultMsg handler")
	}
	if m.statusKind != statusOK {
		t.Errorf("statusKind = %v, want statusOK", m.statusKind)
	}
}

// ── LoopEngine: provenance + kill adapter + take-over attach ─────────────
//
// A Driven loop must be visually distinguishable (⚙, FLEET + DETAIL), killable
// without a terminal surface (registry.MarkDriven false, not /exit), and
// take-over-able (↵ opens a real terminal running `claude --resume <id>` and
// clears Driven — the hard requirement, "the payoff": a human can always
// reclaim the wheel).

// ── provenance marker (⚙) ──────────────────────────────────────────────────

func TestFleetPanelLines_DrivenLoop_ShowsGearMarker(t *testing.T) {
	m := New()
	m.loops = []domain.Loop{{Project: "myproject", SessionID: "s1", ProjectDir: "-x-myproject", State: domain.StateIdle, Driven: true}}
	lines := m.fleetPanelLines(80, 10)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "⚙") {
		t.Errorf("expected the ⚙ provenance marker for a Driven loop:\n%s", joined)
	}
}

func TestFleetPanelLines_ObservedLoop_NoGearMarker(t *testing.T) {
	m := New()
	m.loops = []domain.Loop{{Project: "myproject", SessionID: "s1", ProjectDir: "-x-myproject", State: domain.StateIdle, Driven: false}}
	lines := m.fleetPanelLines(80, 10)
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "⚙") {
		t.Errorf("did not expect the ⚙ marker for an observed loop:\n%s", joined)
	}
}

// TestFleetPanelLines_DrivenAndSelected_BothGlyphsPresent proves the
// 2-column marker cell holds the cursor "▸" and the Driven "⚙" glyph
// simultaneously (they occupy DIFFERENT columns of wMarker, not the same
// one) — a Driven row that's also the selected row must not lose either
// signal.
func TestFleetPanelLines_DrivenAndSelected_BothGlyphsPresent(t *testing.T) {
	m := New()
	m.loops = []domain.Loop{{Project: "myproject", SessionID: "s1", ProjectDir: "-x-myproject", State: domain.StateIdle, Driven: true}}
	m.cursor = 0
	lines := m.fleetPanelLines(80, 10)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "▸") {
		t.Errorf("expected the cursor glyph still present on a selected AND Driven row:\n%s", joined)
	}
	if !strings.Contains(joined, "⚙") {
		t.Errorf("expected the ⚙ marker still present on a selected AND Driven row:\n%s", joined)
	}
}

func TestRenderDetail_DrivenLoop_ShowsDriveRow(t *testing.T) {
	l := domain.Loop{Project: "myproject", SessionID: "s1", State: domain.StateIdle, Cycle: 2,
		Driven: true, Goal: domain.Goal{Text: "ship it", MaxCycles: 8}}
	out := renderDetail(l, 100, 40, detailData{now: time.Now()})
	if !strings.Contains(out, "DRIVE") {
		t.Errorf("expected the DRIVE row present for a Driven loop:\n%s", out)
	}
	if !strings.Contains(out, "engine-driven") {
		t.Errorf("expected the DRIVE row's value text present:\n%s", out)
	}
	if !strings.Contains(out, "cycle 2/8") {
		t.Errorf("expected the DRIVE row to show cycle N/max via cycleLabel:\n%s", out)
	}
}

func TestRenderDetail_ObservedLoop_NoDriveRow(t *testing.T) {
	l := domain.Loop{Project: "myproject", SessionID: "s1", State: domain.StateIdle, Cycle: 2,
		Driven: false, Goal: domain.Goal{Text: "ship it"}}
	out := renderDetail(l, 100, 40, detailData{now: time.Now()})
	if strings.Contains(out, "DRIVE") {
		t.Errorf("did not expect a DRIVE row for an observed loop:\n%s", out)
	}
}

// ── kill adapter for Driven loops (design doc §4) ─────────────────────────

func TestKillCmd_DrivenLoop_ClearsDrivenInsteadOfTierOneExit(t *testing.T) {
	registryDir := t.TempDir()
	historyDir := t.TempDir()
	origRegDir, origHistoryDir, origResolve := registryDirFn, historyDirFn, resolveActuationTargetFn
	defer func() {
		registryDirFn, historyDirFn, resolveActuationTargetFn = origRegDir, origHistoryDir, origResolve
	}()
	registryDirFn = func() string { return registryDir }
	historyDirFn = func() string { return historyDir }
	resolveCalled := false
	resolveActuationTargetFn = func(sessionsDir, sessionID, projectDir string) (control.Actuator, bool, bool) {
		resolveCalled = true
		return nil, false, false
	}
	if err := registry.Bind(registryDir, "sess-1", registry.BindSpec{Goal: "ship it", Driven: true}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	l := domain.Loop{SessionID: "sess-1", Project: "myproject", State: domain.StateIdle, Driven: true}
	msg := killCmd(l)()

	if resolveCalled {
		t.Error("expected resolveActuationTargetFn NOT to be called for a Driven loop — no Tier-1 /exit send, it has no terminal surface")
	}
	km, ok := msg.(killResultMsg)
	if !ok || !km.ok {
		t.Fatalf("got %+v, want a successful killResultMsg", msg)
	}
	if !strings.Contains(km.text, "Driven cleared") {
		t.Errorf("text = %q, want it to mention Driven being cleared", km.text)
	}

	rec, ok := registry.Load(registryDir, "sess-1")
	if !ok {
		t.Fatal("expected a record to exist")
	}
	if rec.Driven {
		t.Error("expected Driven cleared in the registry after kill")
	}
}

// TestKillCmd_DrivenLoop_EmitsKillEventMostRecentActuationIsKillRecognizes
// proves the event this writes is in the EXACT shape
// mostRecentActuationIsKill (internal/claude/scan.go) looks for — so the
// next scan still promotes StateKilled for a Driven loop's kill exactly as
// it would for an observed loop's.
func TestKillCmd_DrivenLoop_EmitsKillEventMostRecentActuationIsKillRecognizes(t *testing.T) {
	registryDir := t.TempDir()
	historyDir := t.TempDir()
	origRegDir, origHistoryDir := registryDirFn, historyDirFn
	defer func() { registryDirFn, historyDirFn = origRegDir, origHistoryDir }()
	registryDirFn = func() string { return registryDir }
	historyDirFn = func() string { return historyDir }
	if err := registry.Bind(registryDir, "sess-1", registry.BindSpec{Goal: "ship it", Driven: true}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	l := domain.Loop{SessionID: "sess-1", Project: "myproject", State: domain.StateIdle, Driven: true}
	killCmd(l)()

	got, err := events.ReadAll(historyDir)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	evs := got["sess-1"]
	if len(evs) != 1 {
		t.Fatalf("got %d events, want exactly 1: %#v", len(evs), evs)
	}
	if evs[0].Trigger != events.TriggerActuation || evs[0].Actor != events.ActorHuman {
		t.Errorf("event = %+v, want Trigger=TriggerActuation Actor=ActorHuman (a human keypress, matching mostRecentActuationIsKill's filter)", evs[0])
	}
	if !strings.HasPrefix(evs[0].Detail, "kill ") {
		t.Errorf("Detail = %q, want it prefixed \"kill \" (mostRecentActuationIsKill's exact match)", evs[0].Detail)
	}
}

func TestKillCmd_DrivenLoop_MarkDrivenErrorSurfacesAsFailure(t *testing.T) {
	registryDir := t.TempDir() // no Bind — MarkDriven errors on a missing record
	origRegDir := registryDirFn
	defer func() { registryDirFn = origRegDir }()
	registryDirFn = func() string { return registryDir }

	l := domain.Loop{SessionID: "sess-1", Project: "myproject", State: domain.StateIdle, Driven: true}
	msg := killCmd(l)()

	km, ok := msg.(killResultMsg)
	if !ok || km.ok {
		t.Fatalf("got %+v, want a failed killResultMsg", msg)
	}
}

// TestUpdate_SecondKWithinWindow_DrivenLoop_SkipsAmbiguityGuard proves the
// keypress-time ambiguity guard (refuseIfAmbiguous) is skipped for a Driven
// loop's kill — it exists solely to protect an actual keystroke from
// landing on the wrong sibling terminal, and killCmd's Driven branch never
// sends one, so two Driven loops sharing an (irrelevant) ProjectDir must
// not spuriously refuse the kill.
func TestUpdate_SecondKWithinWindow_DrivenLoop_SkipsAmbiguityGuard(t *testing.T) {
	registryDir := t.TempDir()
	origRegDir := registryDirFn
	defer func() { registryDirFn = origRegDir }()
	registryDirFn = func() string { return registryDir }
	if err := registry.Bind(registryDir, "sess-1", registry.BindSpec{Goal: "ship it", Driven: true}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	m := New()
	m.loops = []domain.Loop{
		// two loops sharing a ProjectDir would normally trip refuseIfAmbiguous
		// (see TestUpdate_SecondKWithinWindow_Ambiguous_Refuses-style tests
		// elsewhere in this file) — irrelevant here since kill never dispatches
		// into either terminal surface for a Driven loop.
		{Project: "myproject", SessionID: "sess-1", ProjectDir: "-x-myproject", State: domain.StateIdle, Driven: true},
		{Project: "myproject-2", SessionID: "sess-2", ProjectDir: "-x-myproject", State: domain.StateIdle, Driven: true},
	}
	m.cursor = 0

	m, _ = updateModel(t, m, runeKey('k'))
	m, cmd := updateModel(t, m, runeKey('k'))

	if cmd == nil {
		t.Fatal("expected a non-nil tea.Cmd (killCmd) — the ambiguity guard must not refuse a Driven loop's kill")
	}
	if strings.Contains(m.status, "ambiguous") {
		t.Errorf("status = %q, did not want an ambiguity refusal for a Driven kill", m.status)
	}
}

// ── take-over attach: the hard requirement ("the payoff") ─────────────────

// fakeTerminalOpenerController extends fakeController with
// control.TerminalOpener support — a SEPARATE type (not a field toggle on
// fakeController itself) so a plain *fakeController continues to correctly
// simulate a backend WITHOUT TerminalOpener support (e.g. cmux, see its own
// doc) via Go's ordinary interface type-assertion semantics — exactly the
// real-world distinction takeOverCmd's own type-assert branches on.
type fakeTerminalOpenerController struct {
	*fakeController
	openTerminalCalled  bool
	openTerminalCwd     string
	openTerminalCommand string
	openTerminalErr     error
}

func (f *fakeTerminalOpenerController) OpenTerminal(cwd, command string) error {
	f.openTerminalCalled = true
	f.openTerminalCwd = cwd
	f.openTerminalCommand = command
	return f.openTerminalErr
}

func TestTakeOverCmd_DrivenLoop_OpensTerminalAndClearsDriven(t *testing.T) {
	registryDir := t.TempDir()
	historyDir := t.TempDir()
	origRegDir, origHistoryDir := registryDirFn, historyDirFn
	defer func() { registryDirFn, historyDirFn = origRegDir, origHistoryDir }()
	registryDirFn = func() string { return registryDir }
	historyDirFn = func() string { return historyDir }
	if err := registry.Bind(registryDir, "s1", registry.BindSpec{Goal: "ship it", Driven: true}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	fakeCtrl := &fakeTerminalOpenerController{fakeController: &fakeController{name: "orca"}}
	withFakeControlResolve(t, fakeCtrl, true)

	l := domain.Loop{Project: "myproject", SessionID: "s1", Cwd: "/x/myproject", State: domain.StateIdle, Driven: true}

	msg := takeOverCmd(l)()

	if !fakeCtrl.openTerminalCalled {
		t.Fatal("expected OpenTerminal to be called")
	}
	if fakeCtrl.openTerminalCwd != "/x/myproject" {
		t.Errorf("OpenTerminal cwd = %q, want the loop's cwd", fakeCtrl.openTerminalCwd)
	}
	if fakeCtrl.openTerminalCommand != "claude --resume s1" {
		t.Errorf("OpenTerminal command = %q, want the manual resume hint (claude --resume <id>)", fakeCtrl.openTerminalCommand)
	}
	am, ok := msg.(attachResultMsg)
	if !ok || !am.ok {
		t.Fatalf("got %+v, want a successful attachResultMsg", msg)
	}

	rec, ok := registry.Load(registryDir, "s1")
	if !ok {
		t.Fatal("expected a record to exist")
	}
	if rec.Driven {
		t.Error("expected Driven cleared after a successful take-over — the engine must stop driving it")
	}

	got, err := events.ReadAll(historyDir)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	evs := got["s1"]
	if len(evs) != 1 {
		t.Fatalf("got %d events, want exactly 1: %#v", len(evs), evs)
	}
	if evs[0].Trigger != events.TriggerActuation || evs[0].Actor != events.ActorHuman {
		t.Errorf("event = %+v, want Trigger=TriggerActuation Actor=ActorHuman (a human keypress take-over)", evs[0])
	}
	if !strings.HasPrefix(evs[0].Detail, "take-over ") {
		t.Errorf("Detail = %q, want it prefixed \"take-over \"", evs[0].Detail)
	}
}

func TestTakeOverCmd_NoBackend_ManualHintFallback_DrivenUntouched(t *testing.T) {
	registryDir := t.TempDir()
	origRegDir := registryDirFn
	defer func() { registryDirFn = origRegDir }()
	registryDirFn = func() string { return registryDir }
	if err := registry.Bind(registryDir, "s1", registry.BindSpec{Goal: "ship it", Driven: true}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	withFakeControlResolve(t, nil, false)

	l := domain.Loop{Project: "myproject", SessionID: "s1", Cwd: "/x/myproject", State: domain.StateIdle, Driven: true}

	msg := takeOverCmd(l)()

	am, ok := msg.(attachResultMsg)
	if !ok || am.ok {
		t.Fatalf("got %+v, want a failed attachResultMsg with a manual hint", msg)
	}
	if !strings.Contains(am.text, "claude --resume s1") {
		t.Errorf("text = %q, want the manual resume hint", am.text)
	}

	rec, ok := registry.Load(registryDir, "s1")
	if !ok {
		t.Fatal("expected a record to exist")
	}
	if !rec.Driven {
		t.Error("expected Driven UNTOUCHED when no backend is available — fleetops can't confirm a human actually took over by hand, so the engine keeps driving it headlessly")
	}
}

// TestTakeOverCmd_BackendWithoutTerminalOpener_ManualHintFallback: a plain
// *fakeController (no OpenTerminal method — same shape as an unenhanced
// cmux resolve) must fall back exactly like the no-backend case, not panic
// on the type assertion or silently degrade to some other action.
func TestTakeOverCmd_BackendWithoutTerminalOpener_ManualHintFallback(t *testing.T) {
	fakeCtrl := &fakeController{name: "cmux"}
	withFakeControlResolve(t, fakeCtrl, true)
	l := domain.Loop{Project: "myproject", SessionID: "s1", Cwd: "/x/myproject", State: domain.StateIdle, Driven: true}

	msg := takeOverCmd(l)()

	am, ok := msg.(attachResultMsg)
	if !ok || am.ok {
		t.Fatalf("got %+v, want a failed attachResultMsg with a manual hint", msg)
	}
	if !strings.Contains(am.text, "claude --resume s1") {
		t.Errorf("text = %q, want the manual resume hint", am.text)
	}
}

// TestTakeOverCmd_OpenTerminalFails_DrivenNotCleared is the ordering proof:
// clearing Driven BEFORE confirming the terminal opened would strand the
// loop owned by neither the engine (no longer Driven) nor a human (no
// terminal actually opened for them) — see takeOverCmd's own doc.
func TestTakeOverCmd_OpenTerminalFails_DrivenNotCleared(t *testing.T) {
	registryDir := t.TempDir()
	origRegDir := registryDirFn
	defer func() { registryDirFn = origRegDir }()
	registryDirFn = func() string { return registryDir }
	if err := registry.Bind(registryDir, "s1", registry.BindSpec{Goal: "ship it", Driven: true}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	fakeCtrl := &fakeTerminalOpenerController{fakeController: &fakeController{name: "orca"}, openTerminalErr: errTestJudgeFailed}
	withFakeControlResolve(t, fakeCtrl, true)

	l := domain.Loop{Project: "myproject", SessionID: "s1", Cwd: "/x/myproject", State: domain.StateIdle, Driven: true}

	msg := takeOverCmd(l)()

	am, ok := msg.(attachResultMsg)
	if !ok || am.ok {
		t.Fatalf("got %+v, want a failed attachResultMsg", msg)
	}

	rec, ok := registry.Load(registryDir, "s1")
	if !ok {
		t.Fatal("expected a record to exist")
	}
	if !rec.Driven {
		t.Error("expected Driven UNTOUCHED when OpenTerminal fails")
	}
}

// ── "enter" key dispatch: Driven → take-over, observed → attach (unchanged) ─

func TestUpdate_EnterKey_DrivenLoop_DispatchesTakeOverNotAttach(t *testing.T) {
	m := modelWithOneLoop()
	m.loops[0].Driven = true

	m, cmd := updateModel(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	if cmd == nil {
		t.Fatal("expected a non-nil tea.Cmd")
	}
	if !strings.Contains(m.status, "taking over") {
		t.Errorf("status = %q, want it to mention taking over (not attaching)", m.status)
	}
}

// TestUpdate_EnterKey_ObservedLoop_StillDispatchesAttach is the
// attach-preservation regression pin at the KEYPRESS level (the existing
// TestAttachCmd_ObservedLoop_UsesLocateNotLocateClaude pins attachCmd's own
// internals; this pins that the "enter" handler still ROUTES an observed
// loop to it at all, unchanged by this slice's new Driven branch).
func TestUpdate_EnterKey_ObservedLoop_StillDispatchesAttach(t *testing.T) {
	m := modelWithOneLoop() // Driven defaults false

	m, cmd := updateModel(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	if cmd == nil {
		t.Fatal("expected a non-nil tea.Cmd")
	}
	if !strings.Contains(m.status, "attaching") {
		t.Errorf("status = %q, want it to mention attaching (observed-loop path unchanged)", m.status)
	}
}

// ── fleetops --demo: synthetic fleet, no real data, no disk writes ─────

func TestDemoFleet_ReturnsExpectedLoops(t *testing.T) {
	loops, detailCache, oracleCounts := demoFleet()

	if len(loops) != 6 {
		t.Fatalf("got %d loops, want 6", len(loops))
	}
	byProject := make(map[string]domain.Loop, len(loops))
	for _, l := range loops {
		byProject[l.Project] = l
	}

	authHarden, ok := byProject["auth-harden"]
	if !ok {
		t.Fatal("expected an auth-harden loop")
	}
	if authHarden.State != domain.StateGate || authHarden.GatePrompt != "Allow Bash(git push origin main)?" {
		t.Errorf("auth-harden = %+v, want StateGate with the git-push permission prompt", authHarden)
	}
	if authHarden.Goal.MaxCycles != 12 || authHarden.Cycle != 6 || authHarden.TokensSpent != 640000 || authHarden.Goal.BudgetTokens != 2_000_000 {
		t.Errorf("auth-harden contract/usage fields = %+v, want the spec'd values", authHarden)
	}
	if authHarden.Cwd != "/home/user/api" {
		t.Errorf("auth-harden.Cwd = %q, want /home/user/api", authHarden.Cwd)
	}

	flakyTests, ok := byProject["flaky-tests"]
	if !ok || flakyTests.State != domain.StateRunning || flakyTests.Cycle != 4 {
		t.Errorf("flaky-tests = %+v, want StateRunning cycle 4", flakyTests)
	}

	depUpgrade, ok := byProject["dep-upgrade"]
	if !ok || depUpgrade.State != domain.StateDrift || depUpgrade.Cycle != 9 || depUpgrade.NoImprove != 2 {
		t.Errorf("dep-upgrade = %+v, want StateDrift cycle 9 NoImprove 2", depUpgrade)
	}
	if depUpgrade.Last == nil || depUpgrade.Last.Outcome != domain.OutcomeRejected || depUpgrade.Last.AtCycle != 9 {
		t.Errorf("dep-upgrade.Last = %+v, want a rejected verdict at cycle 9", depUpgrade.Last)
	}

	docsGen, ok := byProject["docs-gen"]
	if !ok || docsGen.State != domain.StateIdle || docsGen.Cycle != 2 || docsGen.Goal.Text != "" {
		t.Errorf("docs-gen = %+v, want an UNBOUND StateIdle loop at cycle 2", docsGen)
	}

	coverage, ok := byProject["coverage"]
	if !ok || coverage.State != domain.StateStalled || coverage.Stall != domain.StallRateLimit {
		t.Errorf("coverage = %+v, want StateStalled/StallRateLimit", coverage)
	}

	refactorCore, ok := byProject["refactor-core"]
	if !ok || !refactorCore.Driven || refactorCore.State != domain.StateIdle || refactorCore.Cycle != 3 {
		t.Errorf("refactor-core = %+v, want a Driven StateIdle loop at cycle 3", refactorCore)
	}
	if refactorCore.Goal.Rubric == "" || refactorCore.Goal.Challenger == "" {
		t.Errorf("refactor-core Goal = %+v, want both Rubric and Challenger set (so DRIVE/RUBRIC/CHALL rows render)", refactorCore.Goal)
	}
	if refactorCore.Last == nil || refactorCore.Last.Outcome != domain.OutcomeProgress {
		t.Errorf("refactor-core.Last = %+v, want a progress verdict", refactorCore.Last)
	}

	// auth-harden gets 2 seeded events (spawn, then the gate transition);
	// refactor-core gets 1 seeded TriggerOracle event, so VERDICTS renders.
	if evs := detailCache[authHarden.SessionID].events; len(evs) != 2 {
		t.Errorf("auth-harden seeded events = %d, want 2: %#v", len(evs), evs)
	}
	if evs := detailCache[refactorCore.SessionID].events; len(evs) != 1 || evs[0].Trigger != events.TriggerOracle {
		t.Errorf("refactor-core seeded events = %#v, want exactly 1 TriggerOracle event", evs)
	}
	if oracleCounts[depUpgrade.SessionID] == 0 || oracleCounts[refactorCore.SessionID] == 0 {
		t.Errorf("oracleCounts = %#v, want a non-zero count for dep-upgrade and refactor-core", oracleCounts)
	}
}

func TestNewDemo_SeedsFleetCursorOnGateAndSetsDemoFlag(t *testing.T) {
	m := NewDemo()

	if !m.demo {
		t.Error("expected m.demo = true")
	}
	if len(m.loops) != 6 {
		t.Fatalf("got %d loops, want 6", len(m.loops))
	}
	if m.cursor != 0 || m.loops[0].Project != "auth-harden" || m.loops[0].State != domain.StateGate {
		t.Errorf("cursor = %d on %+v, want cursor 0 on the auth-harden GATE (the hero frame)", m.cursor, m.loops[m.cursor])
	}
	if len(m.detailCache) == 0 {
		t.Error("expected detailCache pre-seeded (no detailCacheCmd ever runs for a demo Model)")
	}
}

// TestModel_ScanCmd_DemoModeReturnsNil / _NormalModeReturnsScan are the
// "assert via seam" proof the coordinator asked for: NewDemo's Model
// structurally can never dispatch a real scan, because Init/tickMsg only
// ever call m.scanCmd() (never the bare scan cmd directly — see both call
// sites), and scanCmd() itself returns nil whenever m.demo is true.
func TestModel_ScanCmd_DemoModeReturnsNil(t *testing.T) {
	if cmd := NewDemo().scanCmd(); cmd != nil {
		t.Error("expected a nil scan cmd for a demo Model — the synthetic fleet must never be rescanned")
	}
}

func TestModel_ScanCmd_NormalModeReturnsScan(t *testing.T) {
	if cmd := New().scanCmd(); cmd == nil {
		t.Error("expected a non-nil scan cmd for a normal (non-demo) Model")
	}
}

// TestScanCmd_DemoMode_NeverCallsDiscoverLoopsFn is the end-to-end version:
// override the discoverLoopsFn seam with a spy, exercise BOTH scanCmd()
// paths, and confirm the spy fires for a normal Model but never for a demo
// one — proving the "no DiscoverLoops call" claim isn't just "no code
// happens to reach it today" but structurally guaranteed by scanCmd()
// itself.
func TestScanCmd_DemoMode_NeverCallsDiscoverLoopsFn(t *testing.T) {
	orig := discoverLoopsFn
	defer func() { discoverLoopsFn = orig }()
	called := false
	discoverLoopsFn = func(now time.Time, within time.Duration) ([]domain.Loop, error) {
		called = true
		return nil, nil
	}

	if cmd := NewDemo().scanCmd(); cmd != nil {
		t.Fatal("expected a nil scan cmd for a demo Model")
	}
	if called {
		t.Error("expected discoverLoopsFn NOT to be called for a demo Model")
	}

	// Sanity-check the seam is actually wired for the normal path — proves
	// the assertion above has teeth (it's not vacuously true because the
	// seam is dead code).
	if cmd := New().scanCmd(); cmd == nil {
		t.Fatal("expected a non-nil scan cmd for a normal Model")
	} else {
		cmd()
	}
	if !called {
		t.Error("expected discoverLoopsFn to be called for a normal (non-demo) Model's scan cmd")
	}
}

// TestUpdate_DemoMode_MutatingKeyRefused_NavigationStillWorks proves the
// keypress-level guard: every mutating/actuation key is refused with a
// read-only message (no cmd dispatched — so no real subprocess call, no
// disk write under ~/.fleetops for the synthetic session ids), while
// plain navigation is completely unaffected.
func TestUpdate_DemoMode_MutatingKeyRefused_NavigationStillWorks(t *testing.T) {
	for _, key := range []string{"r", "a", "i", "o", "n", "k", "p"} {
		t.Run("blocked:"+key, func(t *testing.T) {
			m := NewDemo()
			m, cmd := updateModel(t, m, runeKey(rune(key[0])))
			if cmd != nil {
				t.Errorf("key %q: expected no tea.Cmd in demo mode", key)
			}
			if !strings.Contains(m.status, "read-only") {
				t.Errorf("key %q: status = %q, want the demo read-only message", key, m.status)
			}
		})
	}

	t.Run("blocked:enter", func(t *testing.T) {
		m := NewDemo()
		m, cmd := updateModel(t, m, tea.KeyMsg{Type: tea.KeyEnter})
		if cmd != nil {
			t.Error("expected no tea.Cmd for enter in demo mode")
		}
		if !strings.Contains(m.status, "read-only") {
			t.Errorf("status = %q, want the demo read-only message", m.status)
		}
	})

	t.Run("navigation still works", func(t *testing.T) {
		m := NewDemo()
		beforeCursor := m.cursor
		m, _ = updateModel(t, m, tea.KeyMsg{Type: tea.KeyDown})
		if m.cursor == beforeCursor {
			t.Error("expected the cursor to move — navigation must be unaffected by the demo guard")
		}
	})

	t.Run("filter still works", func(t *testing.T) {
		m := NewDemo()
		m, _ = updateModel(t, m, runeKey('/'))
		if m.mode != modeFiltering {
			t.Errorf("mode = %v, want modeFiltering — the in-memory filter must be unaffected by the demo guard", m.mode)
		}
	})
}

func TestIsDemoBlockedKey(t *testing.T) {
	blocked := []string{"r", "a", "i", "enter", "o", "n", "k", "p", "d", "x"}
	for _, key := range blocked {
		if !isDemoBlockedKey(key) {
			t.Errorf("isDemoBlockedKey(%q) = false, want true", key)
		}
	}
	allowed := []string{"up", "down", "j", "g", "G", "/", "esc", "q", "ctrl+c"}
	for _, key := range allowed {
		if isDemoBlockedKey(key) {
			t.Errorf("isDemoBlockedKey(%q) = true, want false", key)
		}
	}
}

// ── "d" hidden / "x" delete: persisted hide-set (survives restart) ───────

// withHiddenFile points hiddenFileFn at a fresh temp file for one test,
// restoring the original on cleanup — mirrors withSessionsDir. The file does
// not exist yet (fail-open empty until the first hide writes it).
func withHiddenFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hidden.json")
	orig := hiddenFileFn
	t.Cleanup(func() { hiddenFileFn = orig })
	hiddenFileFn = func() string { return path }
	return path
}

// withDeletableSession is the shared fixture for the "x" (delete) tests: a
// temp hide-file, a temp sessions dir, and a registry entry for sess-1 — the
// thing "x" must remove on a confirmed press and must NOT touch otherwise. It
// returns that entry's path so a test can assert on its presence.
//
// The record goes through sessions.WriteSession rather than a hand-written
// JSON literal so the on-disk shape and the <id>.json filename convention stay
// owned by the package that defines them; a literal here would keep passing
// while silently ceasing to represent a real record.
func withDeletableSession(t *testing.T) string {
	t.Helper()
	withHiddenFile(t)
	sessionsDir := withSessionsDir(t)
	if err := sessions.WriteSession(sessionsDir, "sess-1", sessions.SessionEntry{PID: 1, TTY: "ttys001"}); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(sessionsDir, "sess-1.json")
}

// withUnwritableHiddenFile points hiddenFileFn at a path that can never be
// written (its "directory" is actually a regular file), so hidden.Add always
// errors — the only way to exercise hideSession's persistence-failure branch.
func withUnwritableHiddenFile(t *testing.T) {
	t.Helper()
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("i am a file"), 0o644); err != nil {
		t.Fatal(err)
	}
	orig := hiddenFileFn
	t.Cleanup(func() { hiddenFileFn = orig })
	hiddenFileFn = func() string { return filepath.Join(blocker, "hidden.json") }
}

// TestUpdate_DKey_PersistFails_StillHidesInMemory pins hideSession's fail-open
// branch: persistence is best-effort, but the human's intent is not. A loop
// they hid must disappear from the list for THIS session even when the
// tombstone can't be written — and they must be told it won't survive a
// restart, rather than silently believing it will.
func TestUpdate_DKey_PersistFails_StillHidesInMemory(t *testing.T) {
	withUnwritableHiddenFile(t)
	m := modelWithTwoLoops()

	m, _ = updateModel(t, m, runeKey('d'))

	if !m.hidden["sess-1"] {
		t.Error("sess-1 not hidden in memory after a persist failure — the hide must still take effect")
	}
	if len(m.loops) != 1 || m.loops[0].SessionID != "sess-2" {
		t.Fatalf("loops = %+v, want sess-1 pruned from the list regardless of the persist failure", m.loops)
	}
	if m.statusKind != statusErr {
		t.Errorf("statusKind = %v, want statusErr to surface the persistence failure", m.statusKind)
	}
	if !strings.Contains(m.status, "persisting the hide failed") {
		t.Errorf("status = %q, want it to say the hide was not persisted", m.status)
	}
}

// TestUpdate_XKey_PersistFails_ReportsHideFailure: delete routes through the
// same hideSession, so a persist failure there is reported too — with the
// registry removal (which DID succeed) still called out.
func TestUpdate_XKey_PersistFails_ReportsHideFailure(t *testing.T) {
	withUnwritableHiddenFile(t)
	withSessionsDir(t)
	m := modelWithTwoLoops()

	m = confirmDelete(t, m)

	if !m.hidden["sess-1"] {
		t.Error("sess-1 not hidden in memory after a persist failure")
	}
	if m.statusKind != statusErr {
		t.Errorf("statusKind = %v, want statusErr", m.statusKind)
	}
	if !strings.Contains(m.status, "persisting the hide failed") {
		t.Errorf("status = %q, want the hide-persistence failure reported", m.status)
	}
}

func TestUpdate_DKey_HidesSelectedLoop(t *testing.T) {
	withHiddenFile(t)
	m := modelWithTwoLoops()

	m, _ = updateModel(t, m, runeKey('d'))

	if len(m.loops) != 1 || m.loops[0].SessionID != "sess-2" {
		t.Fatalf("loops = %+v, want only sess-2 left", m.loops)
	}
	if !m.hidden["sess-1"] {
		t.Error("expected sess-1 recorded in the hidden set")
	}
	if !strings.Contains(m.status, "hidden myproject") {
		t.Errorf("status = %q, want a hidden-myproject message", m.status)
	}
}

// TestUpdate_DKey_HidePersistsAcrossRestart is the headline requirement: a
// hide written by "d" must still filter the loop after fleetops restarts,
// which we model by building a FRESH Model (New reloads hidden.Load from the
// same file) and feeding it a scan that re-derives sess-1.
func TestUpdate_DKey_HidePersistsAcrossRestart(t *testing.T) {
	withHiddenFile(t)
	m := modelWithTwoLoops()

	m, _ = updateModel(t, m, runeKey('d')) // hide sess-1, persisted to disk

	// "restart": a brand-new Model loads the persisted hide-set from disk.
	restarted := modelWithTwoLoops()
	if !restarted.hidden["sess-1"] {
		t.Fatalf("restarted model's hidden set = %+v, want sess-1 loaded from disk", restarted.hidden)
	}
	rescan := loopsMsg{
		{Project: "myproject", SessionID: "sess-1", State: domain.StateRunning},
		{Project: "asre", SessionID: "sess-2", State: domain.StateIdle},
	}
	restarted, _ = updateModel(t, restarted, rescan)
	if len(restarted.loops) != 1 || restarted.loops[0].SessionID != "sess-2" {
		t.Fatalf("loops after restart+rescan = %+v, want sess-1 still hidden", restarted.loops)
	}
}

func TestUpdate_DKey_EmptyFleet_RefusesWithoutCrashing(t *testing.T) {
	withHiddenFile(t)
	m := New()

	m, cmd := updateModel(t, m, runeKey('d'))

	if cmd != nil {
		t.Error("expected no tea.Cmd for an empty fleet")
	}
	if !strings.Contains(m.status, "select a loop to hide") {
		t.Errorf("status = %q, want the select-a-loop refusal", m.status)
	}
	if len(m.hidden) != 0 {
		t.Errorf("hidden = %+v, want empty — a no-selection refusal must not change state", m.hidden)
	}
}

func TestUpdate_DKey_LastRow_ClampsCursor(t *testing.T) {
	withHiddenFile(t)
	m := modelWithTwoLoops()
	m.cursor = 1

	m, _ = updateModel(t, m, runeKey('d'))

	if len(m.loops) != 1 || m.loops[0].SessionID != "sess-1" {
		t.Fatalf("loops = %+v, want only sess-1 left", m.loops)
	}
	if m.cursor != 0 {
		t.Errorf("cursor = %d, want 0 (clamped onto the remaining row)", m.cursor)
	}
}

func TestUpdate_LoopsMsg_DoesNotResurrectHidden(t *testing.T) {
	withHiddenFile(t)
	m := modelWithTwoLoops()
	m, _ = updateModel(t, m, runeKey('d')) // hide sess-1

	rescan := loopsMsg{
		{Project: "myproject", SessionID: "sess-1", State: domain.StateRunning},
		{Project: "asre", SessionID: "sess-2", State: domain.StateIdle},
	}
	m, _ = updateModel(t, m, rescan)

	if len(m.loops) != 1 || m.loops[0].SessionID != "sess-2" {
		t.Fatalf("loops after rescan = %+v, want sess-1 still hidden", m.loops)
	}
}

func TestWithoutHidden_EmptySet_ReturnsInputUnchanged(t *testing.T) {
	m := modelWithTwoLoops()
	m.hidden = nil
	loops := m.withoutHidden(m.loops)
	if len(loops) != 2 {
		t.Fatalf("got %d loops, want 2 — an empty hidden set must filter nothing", len(loops))
	}
}

// TestNew_CorruptHiddenFile_FailsOpen: a garbage hidden.json must load as an
// empty set (show every loop), never crash — the fail-open invariant.
func TestNew_CorruptHiddenFile_FailsOpen(t *testing.T) {
	path := withHiddenFile(t)
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := modelWithTwoLoops() // New() → hidden.Load on the corrupt file

	if len(m.hidden) != 0 {
		t.Fatalf("hidden = %+v, want empty (fail-open on corrupt file)", m.hidden)
	}
	rescan := loopsMsg{
		{Project: "myproject", SessionID: "sess-1", State: domain.StateRunning},
		{Project: "asre", SessionID: "sess-2", State: domain.StateIdle},
	}
	m, _ = updateModel(t, m, rescan)
	if len(m.loops) != 2 {
		t.Fatalf("loops = %+v, want both shown (corrupt tombstone must not hide anything)", m.loops)
	}
}

// confirmDelete presses "x" twice — delete is gated behind the same two-press
// confirm as kill, so a single press only arms it.
func confirmDelete(t *testing.T, m Model) Model {
	t.Helper()
	m, _ = updateModel(t, m, runeKey('x'))
	m, _ = updateModel(t, m, runeKey('x'))
	return m
}

// TestUpdate_XKey_SinglePress_DeletesNothing is the guard pin. Delete removes
// the registry registration and writes a permanent tombstone, and NOTHING in
// the TUI can unhide a loop — so a single stray "x" was unrecoverable short of
// hand-editing hidden.json, while the strictly-more-reversible "k" already
// required two presses.
func TestUpdate_XKey_SinglePress_DeletesNothing(t *testing.T) {
	regPath := withDeletableSession(t)
	m := modelWithTwoLoops()

	m, _ = updateModel(t, m, runeKey('x'))

	if _, err := os.Stat(regPath); err != nil {
		t.Errorf("registry entry removed on the FIRST x (err=%v), want it untouched", err)
	}
	if m.hidden["sess-1"] {
		t.Error("sess-1 was tombstoned on the first x, want the press to only arm the confirm")
	}
	if len(m.loops) != 2 {
		t.Errorf("loops = %d, want both still listed after one x", len(m.loops))
	}
	if !strings.Contains(m.status, "press x again") {
		t.Errorf("status = %q, want a confirm prompt", m.status)
	}
}

// TestUpdate_XKey_ConfirmExpires_DeletesNothing: a second "x" arriving after
// the window starts a fresh confirm rather than deleting.
func TestUpdate_XKey_ConfirmExpires_DeletesNothing(t *testing.T) {
	regPath := withDeletableSession(t)
	m := modelWithTwoLoops()

	m, _ = updateModel(t, m, runeKey('x'))
	m.pendingDeleteAt = time.Now().Add(-destructiveConfirmWindow - time.Second) // window elapsed
	m, _ = updateModel(t, m, runeKey('x'))

	if _, err := os.Stat(regPath); err != nil {
		t.Errorf("registry entry removed after an EXPIRED confirm (err=%v), want it untouched", err)
	}
	if m.hidden["sess-1"] {
		t.Error("sess-1 tombstoned after an expired confirm")
	}
	if !strings.Contains(m.status, "press x again") {
		t.Errorf("status = %q, want a fresh confirm prompt", m.status)
	}
}

// TestUpdate_XKey_InterveningKeyCancelsConfirm: any other key cancels a pending
// delete, so "x" then something else then "x" cannot delete on that second x.
func TestUpdate_XKey_InterveningKeyCancelsConfirm(t *testing.T) {
	regPath := withDeletableSession(t)
	m := modelWithTwoLoops()

	m, _ = updateModel(t, m, runeKey('x'))
	m, _ = updateModel(t, m, tea.KeyMsg{Type: tea.KeyDown}) // cursor move cancels
	m, _ = updateModel(t, m, runeKey('x'))

	if _, err := os.Stat(regPath); err != nil {
		t.Errorf("registry entry removed despite an intervening keypress (err=%v)", err)
	}
	if m.pendingDeleteSession == "" {
		t.Error("expected the second x to re-arm a fresh confirm")
	}
}

// TestUpdate_XKey_DeletesRegistryEntryAndHides: "x" twice removes the session
// registry .json AND persists the hide, while the conversation jsonl is left
// untouched.
func TestUpdate_XKey_DeletesRegistryEntryAndHides(t *testing.T) {
	// regPath is the registry entry for sess-1 -- what "x" must remove...
	regPath := withDeletableSession(t)
	// ...and a stand-in conversation jsonl "x" must NOT touch.
	jsonlPath := filepath.Join(t.TempDir(), "sess-1.jsonl")
	if err := os.WriteFile(jsonlPath, []byte("conversation history\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := modelWithTwoLoops()

	m = confirmDelete(t, m)

	if _, err := os.Stat(regPath); !os.IsNotExist(err) {
		t.Errorf("registry entry still present (err=%v), want removed", err)
	}
	if _, err := os.Stat(jsonlPath); err != nil {
		t.Errorf("conversation jsonl was disturbed (err=%v), want preserved", err)
	}
	if !m.hidden["sess-1"] {
		t.Error("expected sess-1 in the hidden set after delete")
	}
	if len(m.loops) != 1 || m.loops[0].SessionID != "sess-2" {
		t.Fatalf("loops = %+v, want only sess-2 left", m.loops)
	}
	if !strings.Contains(m.status, "deleted myproject") || !strings.Contains(m.status, "registry entry removed") {
		t.Errorf("status = %q, want a deleted/registry-removed message", m.status)
	}
}

// TestUpdate_XKey_MissingRegistryEntry_StillHides: DeleteSession treats a
// missing entry as a no-op (nil error), so "x" on a loop with no registry
// record still hides it — no error status.
func TestUpdate_XKey_MissingRegistryEntry_StillHides(t *testing.T) {
	withHiddenFile(t)
	withSessionsDir(t) // empty — sess-1 has no registry entry
	m := modelWithTwoLoops()

	m = confirmDelete(t, m)

	if !m.hidden["sess-1"] {
		t.Error("expected sess-1 hidden even with no registry entry to delete")
	}
	if m.statusKind == statusErr {
		t.Errorf("statusKind = %v, want not statusErr (missing registry entry is a no-op)", m.statusKind)
	}
}

func TestUpdate_XKey_EmptyFleet_RefusesWithoutCrashing(t *testing.T) {
	withHiddenFile(t)
	m := New()

	m, cmd := updateModel(t, m, runeKey('x'))

	if cmd != nil {
		t.Error("expected no tea.Cmd for an empty fleet")
	}
	if !strings.Contains(m.status, "select a loop to delete") {
		t.Errorf("status = %q, want the select-a-loop-to-delete refusal", m.status)
	}
	if len(m.hidden) != 0 {
		t.Errorf("hidden = %+v, want empty — a no-selection refusal must not change state", m.hidden)
	}
}

// TestUpdate_DXKeys_DemoBlocked: in demo mode both keys are refused before
// touching disk (no persisted tombstone keyed by a synthetic session id).
func TestUpdate_DXKeys_DemoBlocked(t *testing.T) {
	withHiddenFile(t)
	for _, key := range []rune{'d', 'x'} {
		m := NewDemo()
		before := len(m.loops)
		m, _ = updateModel(t, m, runeKey(key))
		if len(m.loops) != before {
			t.Errorf("key %q: loops changed in demo mode, want refused", string(key))
		}
		if !strings.Contains(m.status, "demo mode is read-only") {
			t.Errorf("key %q: status = %q, want the demo read-only refusal", string(key), m.status)
		}
	}
}

// ── loop display labels in the FLEET panel (feat/loop-display-name) ──────
//
// The panel's whole point is answering "what is each loop doing" WITHOUT
// opening DETAIL — so a bound loop's row must carry its display name or
// goal text, not the project-dir label that forced exactly that detour.

func TestFleetPanelLines_BoundLoopNoName_ShowsGoalText(t *testing.T) {
	m := New()
	m.loops = []domain.Loop{
		{Project: "myproject", SessionID: "s1", State: domain.StateRunning,
			Goal: domain.Goal{Text: "fix the flaky auth test", MaxCycles: 12}},
	}
	m.cursor = 0

	joined := strings.Join(m.fleetPanelLines(120, 10), "\n")
	if !strings.Contains(joined, "fix the flaky auth") {
		t.Errorf("expected the goal text as the row label, got:\n%s", joined)
	}
}

func TestFleetPanelLines_ExplicitName_ShownInsteadOfGoal(t *testing.T) {
	m := New()
	m.loops = []domain.Loop{
		{Name: "auth-bugfix", Project: "myproject", SessionID: "s1", State: domain.StateRunning,
			Goal: domain.Goal{Text: "fix the flaky auth test", MaxCycles: 12}},
	}
	m.cursor = 0

	joined := strings.Join(m.fleetPanelLines(120, 10), "\n")
	if !strings.Contains(joined, "auth-bugfix") {
		t.Errorf("expected the explicit display name as the row label, got:\n%s", joined)
	}
}

// TestDuplicateLabels_SameProjectDifferentGoals_NotDuplicate: two loops in
// the SAME repo pursuing DIFFERENT goals already read apart by their goal
// labels — no session-id suffix needed (dup is keyed by DisplayLabel, not
// Project).
func TestDuplicateLabels_SameProjectDifferentGoals_NotDuplicate(t *testing.T) {
	loops := []domain.Loop{
		{Project: "backend", SessionID: "aaa1", Goal: domain.Goal{Text: "fix the auth bug"}},
		{Project: "backend", SessionID: "bbb2", Goal: domain.Goal{Text: "add rate limiting"}},
	}
	dup := duplicateLabels(loops)
	if dup["fix the auth bug"] || dup["add rate limiting"] {
		t.Errorf("dup = %v, want neither goal-labeled loop marked duplicate", dup)
	}
}

// TestFleetPanelLines_IdenticalLabels_StillDisambiguatedByShortID: the
// session-id fragment remains as the LAST-RESORT disambiguator — two loops
// whose labels truly collide must still be tellable apart.
func TestFleetPanelLines_IdenticalLabels_StillDisambiguatedByShortID(t *testing.T) {
	m := New()
	m.loops = []domain.Loop{
		{Project: "backend", SessionID: "aaa1zzzz", State: domain.StateRunning, Goal: domain.Goal{Text: "fix the bug"}},
		{Project: "backend", SessionID: "bbb2zzzz", State: domain.StateIdle, Goal: domain.Goal{Text: "fix the bug"}},
	}
	m.cursor = 0

	joined := strings.Join(m.fleetPanelLines(120, 10), "\n")
	if !strings.Contains(joined, "·aaa1") || !strings.Contains(joined, "·bbb2") {
		t.Errorf("expected ·shortID suffixes on colliding labels, got:\n%s", joined)
	}
}
