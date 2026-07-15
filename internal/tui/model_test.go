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
	if _, wNote := columnWidths(60); wNote != 0 {
		t.Errorf("at width 60, wNote = %d, want 0 (NOTE column dropped)", wNote)
	}
	if _, wNote := columnWidths(minWidthForNote); wNote == 0 {
		t.Errorf("at width %d, wNote = 0, want > 0 (NOTE column kept)", minWidthForNote)
	}
}

func TestColumnWidths_NameNeverBelowMinimum(t *testing.T) {
	wName, _ := columnWidths(20)
	if wName < 10 {
		t.Errorf("wName = %d at a very narrow width, want >= 10 (usable minimum)", wName)
	}
}
