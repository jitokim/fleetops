package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jitokim/missionctl/internal/domain"
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
	want := []string{"less", "-R", "+G", "--prompt=log: q to return to missionctl", "/x/sess.jsonl"}
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
	if _, _, _, wNote := columnWidths(minWidthForNote - 1); wNote != 0 {
		t.Errorf("at width %d, wNote = %d, want 0 (NOTE column dropped)", minWidthForNote-1, wNote)
	}
	if _, _, _, wNote := columnWidths(minWidthForNote); wNote == 0 {
		t.Errorf("at width %d, wNote = 0, want > 0 (NOTE column kept)", minWidthForNote)
	}
}

func TestColumnWidths_DropsBudgetBelowThreshold(t *testing.T) {
	if _, _, wBudget, _ := columnWidths(minWidthForBudget - 1); wBudget != 0 {
		t.Errorf("at width %d, wBudget = %d, want 0 (BUDGET column dropped)", minWidthForBudget-1, wBudget)
	}
	if _, _, wBudget, _ := columnWidths(minWidthForBudget); wBudget == 0 {
		t.Errorf("at width %d, wBudget = 0, want > 0 (BUDGET column kept)", minWidthForBudget)
	}
}

func TestColumnWidths_DropsCycleBelowThreshold(t *testing.T) {
	if _, wCycle, _, _ := columnWidths(minWidthForCycle - 1); wCycle != 0 {
		t.Errorf("at width %d, wCycle = %d, want 0 (CYCLE column dropped)", minWidthForCycle-1, wCycle)
	}
	if _, wCycle, _, _ := columnWidths(minWidthForCycle); wCycle == 0 {
		t.Errorf("at width %d, wCycle = 0, want > 0 (CYCLE column kept)", minWidthForCycle)
	}
}

func TestColumnWidths_DegradationOrder(t *testing.T) {
	// NOTE must drop before BUDGET, which must drop before CYCLE, as width
	// shrinks — never the other way around.
	if minWidthForNote <= minWidthForBudget {
		t.Errorf("minWidthForNote (%d) must be > minWidthForBudget (%d)", minWidthForNote, minWidthForBudget)
	}
	if minWidthForBudget <= minWidthForCycle {
		t.Errorf("minWidthForBudget (%d) must be > minWidthForCycle (%d)", minWidthForBudget, minWidthForCycle)
	}
}

func TestColumnWidths_NameNeverBelowMinimum(t *testing.T) {
	wName, _, _, _ := columnWidths(20)
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
	m.loops = []domain.Loop{{Project: "aboard", SessionID: "sess-1", Cwd: "/x/aboard", State: domain.StateRunning}}
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

func TestUpdate_TypeThenEnter_SubmitsSpawn(t *testing.T) {
	m := modelWithOneLoop()
	m, _ = updateModel(t, m, runeKey('n'))

	for _, r := range "fix the bug" {
		m, _ = updateModel(t, m, runeKey(r))
	}
	if m.input.Value() != "fix the bug" {
		t.Fatalf("input value = %q, want %q (typing must reach the textinput while prompting)", m.input.Value(), "fix the bug")
	}

	m, cmd := updateModel(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	if m.mode != modeNormal {
		t.Errorf("mode = %v, want modeNormal after submit", m.mode)
	}
	if cmd == nil {
		t.Fatal("expected a non-nil tea.Cmd (spawnCmd) on submit with a non-empty goal")
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
