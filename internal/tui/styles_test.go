package tui

import (
	"testing"

	"github.com/jitokim/fleetops/internal/domain"
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

func TestStateLabel_Failed(t *testing.T) {
	got := stateLabel(domain.Loop{State: domain.StateFailed})
	want := "✗ FAILED"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStateColor_FailedIsRed(t *testing.T) {
	if got := stateColor(domain.Loop{State: domain.StateFailed}); got != cRed {
		t.Errorf("got %v, want cRed", got)
	}
}

func TestStateStyle_FailedIsBold(t *testing.T) {
	if !stateStyle(domain.Loop{State: domain.StateFailed}).GetBold() {
		t.Error("expected StateFailed's style to be bold, like the other urgent states")
	}
}
