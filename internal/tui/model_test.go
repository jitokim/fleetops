package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/jitokim/missionctl/internal/domain"
)

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
