package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/jitokim/missionctl/internal/domain"
	"github.com/jitokim/missionctl/internal/registry"
	runewidth "github.com/mattn/go-runewidth"
)

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

func TestPadBetween_OverflowStillFitsBothWithMinGap(t *testing.T) {
	// left+right alone exceed width — must not truncate either side, just
	// shrink the gap to its 1-space floor.
	got := padBetween("a very long left string", "right", 10)
	if !strings.HasPrefix(got, "a very long left string") || !strings.HasSuffix(got, "right") {
		t.Errorf("got %q, want both sides intact with at least a 1-space gap", got)
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

func TestColumnWidths_DropsNoteBelowThreshold(t *testing.T) {
	if _, _, _, _, _, _, wNote := columnWidths(minWidthForNote - 1); wNote != 0 {
		t.Errorf("at width %d, wNote = %d, want 0 (NOTE column dropped)", minWidthForNote-1, wNote)
	}
	if _, _, _, _, _, _, wNote := columnWidths(minWidthForNote); wNote == 0 {
		t.Errorf("at width %d, wNote = 0, want > 0 (NOTE column kept)", minWidthForNote)
	}
}

// sampleLoopForWidth builds a loop whose NAME/DOING/NOTE text all overflow any
// column, so every rendered cell is padded/truncated to exactly its column
// width — the worst case for total row width.
func sampleLoopForWidth() domain.Loop {
	return domain.Loop{
		Project: "IdeaProjects-very-long-label", SessionID: "abcd1234",
		State: domain.StateRunning, Cycle: 6,
		Goal:         domain.Goal{Text: "add pagination to the search results endpoint and cache it", MaxCycles: 12, BudgetTokens: 200000},
		TokensSpent:  64000,
		LastActivity: time.Now().Add(-30 * time.Second),
		Note:         "⚠ over budget please look",
	}
}

// renderedRowWidth is the true on-screen width of a row (ANSI-stripped, wide
// runes counted) — the ground truth for "does the row fit / would it wrap?".
// sel=false so padToWidth doesn't pad the row out to the terminal width and
// mask an overflow.
func renderedRowWidth(l domain.Loop, width int) int {
	wName, wDoing, wCycle, wOracle, wBudget, wNI, wNote := columnWidths(width)
	row := renderRow(l, false, false, wName, wDoing, wCycle, wOracle, wBudget, wNI, wNote, width)
	return lipgloss.Width(row)
}

// TestColumnWidths_RowNeverOverflowsWhenDoingShown is the core anti-regression
// property: whenever DOING is rendered (wDoing > 0), the row must fit within
// the terminal width — otherwise it soft-wraps onto a second physical line and
// the selection highlight stops covering it. This is exactly the class of bug
// the fixed-width DOING introduced at 120 cols; a wide sweep guards it.
func TestColumnWidths_RowNeverOverflowsWhenDoingShown(t *testing.T) {
	l := sampleLoopForWidth()
	for width := 40; width <= 260; width++ {
		_, wDoing, _, _, _, _, _ := columnWidths(width)
		if wDoing == 0 {
			continue // DOING hidden — narrow regime, pre-existing NAME-only behaviour
		}
		if got := renderedRowWidth(l, width); got > width {
			t.Errorf("width=%d: rendered row is %d cols wide (DOING shown, wDoing=%d) — overflows, would wrap", width, got, wDoing)
		}
	}
}

// TestColumnWidths_RowFitsAtCommonWidths pins the mainstream terminal sizes the
// human actually uses: the full table (DOING included where it fits) must
// render within the width, no wrapping. 100 is the lowest width the fixed
// columns themselves fit in; below it the pre-existing narrow overflow applies
// (and DOING is hidden, so it isn't DOING's doing) — see the hidden-widths test.
func TestColumnWidths_RowFitsAtCommonWidths(t *testing.T) {
	l := sampleLoopForWidth()
	for _, width := range []int{100, 118, 120, 130, 140, 160, 200} {
		if got := renderedRowWidth(l, width); got > width {
			t.Errorf("width=%d: rendered row is %d cols wide, want <= %d", width, got, width)
		}
	}
}

// TestColumnWidths_DoingHiddenWhenNoRoom: below the width that can seat both
// text columns' floors alongside the fixed columns, DOING is hidden entirely
// (never a squeezed sub-floor fragment), so it adds nothing to — and can't
// overflow — the pre-existing narrow layout.
func TestColumnWidths_DoingHiddenWhenNoRoom(t *testing.T) {
	for _, width := range []int{40, 60, 80, 100, 110, 115} {
		if _, wDoing, _, _, _, _, _ := columnWidths(width); wDoing != 0 {
			t.Errorf("width=%d: wDoing=%d, want 0 (hidden — not enough room for both text columns)", width, wDoing)
		}
	}
}

// TestColumnWidths_DoingShownAndReadableWhenWide: at mainstream+ widths DOING
// is shown and never below its floor (a readable phrase, not a fragment).
func TestColumnWidths_DoingShownAndReadableWhenWide(t *testing.T) {
	for _, width := range []int{120, 130, 140, 160, 200} {
		_, wDoing, _, _, _, _, _ := columnWidths(width)
		if wDoing < doingFloorWidth {
			t.Errorf("width=%d: wDoing=%d, want >= doingFloorWidth (%d) — a shown column must be readable", width, wDoing, doingFloorWidth)
		}
	}
}

// TestColumnWidths_NameAndDoingReachCapsWhenWide: on a very wide terminal both
// text columns cap out and the leftover flows to NOTE (unchanged behaviour).
func TestColumnWidths_NameAndDoingReachCapsWhenWide(t *testing.T) {
	wName, wDoing, _, _, _, _, wNote := columnWidths(240)
	if wName != nameCapWidth {
		t.Errorf("wName=%d at width 240, want cap %d", wName, nameCapWidth)
	}
	if wDoing != doingCapWidth {
		t.Errorf("wDoing=%d at width 240, want cap %d", wDoing, doingCapWidth)
	}
	if wNote <= 24 {
		t.Errorf("wNote=%d at width 240, want > 24 (spare beyond both caps flows to NOTE)", wNote)
	}
}

// TestFlexNameDoing_InvariantAndBounds: the split must conserve the budget
// (wName+wDoing+spare == remaining — this is what makes the row fit) and keep
// each column within [floor, cap], with non-negative spare, at every budget.
func TestFlexNameDoing_InvariantAndBounds(t *testing.T) {
	for remaining := nameFloorWidth + doingFloorWidth; remaining <= 320; remaining++ {
		wName, wDoing, spare := flexNameDoing(remaining)
		if wName+wDoing+spare != remaining {
			t.Fatalf("remaining=%d: wName(%d)+wDoing(%d)+spare(%d) != remaining", remaining, wName, wDoing, spare)
		}
		if wName < nameFloorWidth || wName > nameCapWidth {
			t.Errorf("remaining=%d: wName=%d out of [%d,%d]", remaining, wName, nameFloorWidth, nameCapWidth)
		}
		if wDoing < doingFloorWidth || wDoing > doingCapWidth {
			t.Errorf("remaining=%d: wDoing=%d out of [%d,%d]", remaining, wDoing, doingFloorWidth, doingCapWidth)
		}
		if spare < 0 {
			t.Errorf("remaining=%d: spare=%d is negative", remaining, spare)
		}
	}
}

func TestColumnWidths_DropsNIBelowThreshold(t *testing.T) {
	if _, _, _, _, _, wNI, _ := columnWidths(minWidthForNI - 1); wNI != 0 {
		t.Errorf("at width %d, wNI = %d, want 0 (N/I column dropped)", minWidthForNI-1, wNI)
	}
	if _, _, _, _, _, wNI, _ := columnWidths(minWidthForNI); wNI == 0 {
		t.Errorf("at width %d, wNI = 0, want > 0 (N/I column kept)", minWidthForNI)
	}
}

func TestColumnWidths_DropsOracleBelowThreshold(t *testing.T) {
	if _, _, _, wOracle, _, _, _ := columnWidths(minWidthForOracle - 1); wOracle != 0 {
		t.Errorf("at width %d, wOracle = %d, want 0 (ORACLE column dropped)", minWidthForOracle-1, wOracle)
	}
	if _, _, _, wOracle, _, _, _ := columnWidths(minWidthForOracle); wOracle == 0 {
		t.Errorf("at width %d, wOracle = 0, want > 0 (ORACLE column kept)", minWidthForOracle)
	}
}

func TestColumnWidths_DropsBudgetBelowThreshold(t *testing.T) {
	if _, _, _, _, wBudget, _, _ := columnWidths(minWidthForBudget - 1); wBudget != 0 {
		t.Errorf("at width %d, wBudget = %d, want 0 (BUDGET column dropped)", minWidthForBudget-1, wBudget)
	}
	if _, _, _, _, wBudget, _, _ := columnWidths(minWidthForBudget); wBudget == 0 {
		t.Errorf("at width %d, wBudget = 0, want > 0 (BUDGET column kept)", minWidthForBudget)
	}
}

func TestColumnWidths_DropsCycleBelowThreshold(t *testing.T) {
	if _, _, wCycle, _, _, _, _ := columnWidths(minWidthForCycle - 1); wCycle != 0 {
		t.Errorf("at width %d, wCycle = %d, want 0 (CYCLE column dropped)", minWidthForCycle-1, wCycle)
	}
	if _, _, wCycle, _, _, _, _ := columnWidths(minWidthForCycle); wCycle == 0 {
		t.Errorf("at width %d, wCycle = 0, want > 0 (CYCLE column kept)", minWidthForCycle)
	}
}

func TestColumnWidths_DegradationOrder(t *testing.T) {
	// NOTE must drop before N/I, before ORACLE, before BUDGET, before CYCLE, as
	// width shrinks — never any other order. (NAME and DOING aren't in this
	// order; they flex to share the leftover — see the row-fit / flex tests.)
	if minWidthForNote <= minWidthForNI {
		t.Errorf("minWidthForNote (%d) must be > minWidthForNI (%d)", minWidthForNote, minWidthForNI)
	}
	if minWidthForNI <= minWidthForOracle {
		t.Errorf("minWidthForNI (%d) must be > minWidthForOracle (%d)", minWidthForNI, minWidthForOracle)
	}
	if minWidthForOracle <= minWidthForBudget {
		t.Errorf("minWidthForOracle (%d) must be > minWidthForBudget (%d)", minWidthForOracle, minWidthForBudget)
	}
	if minWidthForBudget <= minWidthForCycle {
		t.Errorf("minWidthForBudget (%d) must be > minWidthForCycle (%d)", minWidthForBudget, minWidthForCycle)
	}
}

func TestColumnWidths_NameNeverBelowMinimum(t *testing.T) {
	wName, _, _, _, _, _, _ := columnWidths(20)
	if wName < 10 {
		t.Errorf("wName = %d at a very narrow width, want >= 10 (usable minimum)", wName)
	}
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

func TestUpdate_IKey_StallGone_BlockedByKeyGuard(t *testing.T) {
	// the "i" keypress guard must refuse a StallGone loop early — before the
	// human types anything — so it never even reaches inject mode.
	m := modelWithOneLoop()
	m.loops[0].Stall = domain.StallGone

	m, cmd := updateModel(t, m, runeKey('i'))

	if cmd != nil {
		t.Error("expected no tea.Cmd — StallGone is not injectable via the i key")
	}
	if m.mode != modeNormal {
		t.Errorf("mode = %v, want modeNormal (StallGone must not enter inject mode)", m.mode)
	}
	if !strings.Contains(m.status, "process gone") {
		t.Errorf("status = %q, want it to mention the process is gone", m.status)
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

	msg := sendPromptCmd(l, "do the thing", "injected into", "")()

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

func TestSendPromptCmd_StallGone_RefusesWithRestartHint(t *testing.T) {
	// belt-and-suspenders: sendPromptCmd itself must refuse on StallGone too,
	// pointing at a restart (a StallGone surface is a bare shell — the prompt
	// would be typed as a shell command).
	l := domain.Loop{SessionID: "sess-1", Project: "aboard", Stall: domain.StallGone}

	msg := sendPromptCmd(l, "do the thing", "injected into", "")()

	rm, ok := msg.(resumeResultMsg)
	if !ok {
		t.Fatalf("got %T, want resumeResultMsg", msg)
	}
	if rm.ok {
		t.Error("expected ok=false")
	}
	if !strings.Contains(rm.text, "process gone") {
		t.Errorf("text = %q, want it to mention the process is gone", rm.text)
	}
	if !strings.Contains(rm.text, "claude --resume sess-1") {
		t.Errorf("text = %q, want it to carry the restart hint", rm.text)
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
	origDirFn, origJudgeFn := registryDirFn, judgeFn
	defer func() { registryDirFn, judgeFn = origDirFn, origJudgeFn }()
	registryDirFn = func() string { return dir }
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
	origDirFn, origJudgeFn := registryDirFn, judgeFn
	defer func() { registryDirFn, judgeFn = origDirFn, origJudgeFn }()
	registryDirFn = func() string { return dir }

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

// ── DOING column (doingForRow) ──────────────────────────────────────

func TestDoingForRow_GoalTextPreferredOverLastText(t *testing.T) {
	// a goal-bound loop: Goal.Text is the ideal "what is this for", and wins
	// even when LastText is also present.
	l := domain.Loop{Goal: domain.Goal{Text: "refactor the scanner"}, LastText: "ran go test, 3 failing"}
	if got := doingForRow(l); got != "refactor the scanner" {
		t.Errorf("got %q, want the goal text (preferred over LastText)", got)
	}
}

func TestDoingForRow_FallsBackToLastText(t *testing.T) {
	// the majority case: a plain claude session missionctl only observes has no
	// Goal.Text, so its last assistant tail is what it's "doing".
	l := domain.Loop{LastText: "running go test ./..."}
	if got := doingForRow(l); got != "running go test ./..." {
		t.Errorf("got %q, want the LastText fallback", got)
	}
}

func TestDoingForRow_NeitherGoalNorLastText_Empty(t *testing.T) {
	if got := doingForRow(domain.Loop{}); got != "" {
		t.Errorf("got %q, want empty (a just-started loop with no goal and no tail yet)", got)
	}
}

func TestDoingForRow_TruncatedToColumnWidth(t *testing.T) {
	// doingForRow returns the raw text; the caller truncates it to the column
	// width with trunc — verify that path caps a long goal at the column and
	// marks it with an ellipsis.
	long := strings.Repeat("x", doingCapWidth+20)
	got := trunc(doingForRow(domain.Loop{Goal: domain.Goal{Text: long}}), doingCapWidth-1)
	if n := len([]rune(got)); n != doingCapWidth-1 {
		t.Errorf("truncated length = %d runes, want %d (column width - 1)", n, doingCapWidth-1)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("got %q, want a trailing ellipsis when truncated", got)
	}
}

func TestDoingForRow_CapBumpDoesNotChangeRenderedOutput(t *testing.T) {
	// Bumping LastText's cap (120→800 in internal/claude.summarizeTailText) must
	// be a no-op for DOING: DOING hard-truncates LastText to its own column
	// (doingCapWidth=30), far below either cap, so only the first ~30 runes ever
	// reach the screen. Two LastTexts that agree on their first 120 runes but
	// differ beyond it must render identically in DOING.
	prefix := strings.Repeat("a", 120)
	asIfCappedAt120 := prefix
	asIfCappedAt800 := prefix + strings.Repeat("b", 680)
	got120 := trunc(doingForRow(domain.Loop{LastText: asIfCappedAt120}), doingCapWidth)
	got800 := trunc(doingForRow(domain.Loop{LastText: asIfCappedAt800}), doingCapWidth)
	if got120 != got800 {
		t.Errorf("DOING render changed after cap bump: 120-cap=%q 800-cap=%q", got120, got800)
	}
}

// ── detail pane TAIL row (wrapTailText / detailRowMultiline) ────────

func TestWrapTailText_WrapsToExpectedLineCount(t *testing.T) {
	// three 5-col lines, all under maxTailLines → returned verbatim, no marker.
	got := wrapTailText("aa bb cc dd ee ff", 5, maxTailLines)
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
	got := wrapTailText("short text", 40, maxTailLines)
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
	if got := wrapTailText("anything", 0, maxTailLines); got != nil {
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
	out := renderDetail(l, 80)
	if strings.Contains(out, "TAIL") {
		t.Errorf("detail pane should have NO TAIL row when LastText is empty:\n%s", out)
	}
}

func TestRenderDetail_LongLastText_ShowsWrappedTruncatedTailRow(t *testing.T) {
	// long enough to overflow maxTailLines at the pane width → wrapped + marked.
	l := domain.Loop{
		Project:   "aboard",
		SessionID: "s1",
		State:     domain.StateIdle,
		Cwd:       "/x",
		Path:      "/x/s1.jsonl",
		LastText:  strings.Repeat("lorem ipsum dolor sit amet ", 60),
	}
	out := renderDetail(l, 80)
	if !strings.Contains(out, "TAIL") {
		t.Errorf("detail pane should show a TAIL row when LastText is present:\n%s", out)
	}
	if !strings.Contains(out, "…") {
		t.Errorf("an overflowing TAIL should carry a truncation marker:\n%s", out)
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
