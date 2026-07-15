package tui

import (
	"testing"

	"github.com/jitokim/missionctl/internal/domain"
)

func TestStateLabel_Gate(t *testing.T) {
	got := stateLabel(domain.Loop{State: domain.StateGate})
	want := "◆ GATE"
	if got != want {
		t.Errorf("got %q, want %q (uppercase, like the mockup)", got, want)
	}
}

func TestStateColor_GateIsAmber(t *testing.T) {
	if got := stateColor(domain.Loop{State: domain.StateGate}); got != cAmber {
		t.Errorf("got %v, want cAmber", got)
	}
}
