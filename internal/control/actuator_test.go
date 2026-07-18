package control

import (
	"errors"
	"testing"
)

// boundActuatorCtl records what a boundController forwarded, and with which
// Target — the whole point of the binding is that resolution captures the
// target ONCE and every later call reuses that same value, so the tests below
// assert the forwarded target, not just that a call happened.
type boundActuatorCtl struct {
	*fakeResolveCtl

	resumeTarget    Target
	resumePrompt    string
	approveTarget   Target
	interruptTarget Target

	resumeErr    error
	approveErr   error
	interruptErr error
}

func (f *boundActuatorCtl) Resume(t Target, prompt string) error {
	f.resumeTarget, f.resumePrompt = t, prompt
	return f.resumeErr
}
func (f *boundActuatorCtl) Approve(t Target) error   { f.approveTarget = t; return f.approveErr }
func (f *boundActuatorCtl) Interrupt(t Target) error { f.interruptTarget = t; return f.interruptErr }

func newBoundActuatorCtl(t *testing.T) *boundActuatorCtl {
	return &boundActuatorCtl{fakeResolveCtl: &fakeResolveCtl{t: t, name: "tmux", available: true}}
}

// TestBoundController_ForwardsWithBoundTarget is the core binding pin: each
// Actuator method reaches the SAME target resolution captured, so no call site
// can pass a different one by accident (the failure mode the old
// (Controller, Target) pair left open at every call site).
func TestBoundController_ForwardsWithBoundTarget(t *testing.T) {
	target := Target{Backend: "tmux", ID: "%3", Cwd: "/x/proj"}
	ctl := newBoundActuatorCtl(t)
	act := boundController{ctrl: ctl, target: target}

	if err := act.Resume("do the thing"); err != nil {
		t.Fatalf("Resume = %v, want nil", err)
	}
	if ctl.resumeTarget != target {
		t.Errorf("Resume target = %+v, want the bound %+v", ctl.resumeTarget, target)
	}
	if ctl.resumePrompt != "do the thing" {
		t.Errorf("Resume prompt = %q, want %q", ctl.resumePrompt, "do the thing")
	}

	if err := act.Approve(); err != nil {
		t.Fatalf("Approve = %v, want nil", err)
	}
	if ctl.approveTarget != target {
		t.Errorf("Approve target = %+v, want the bound %+v", ctl.approveTarget, target)
	}

	if err := act.Interrupt(); err != nil {
		t.Fatalf("Interrupt = %v, want nil", err)
	}
	if ctl.interruptTarget != target {
		t.Errorf("Interrupt target = %+v, want the bound %+v", ctl.interruptTarget, target)
	}
}

// TestBoundController_PropagatesErrors: the binding is a pass-through, never a
// place that swallows a backend failure into a false success.
func TestBoundController_PropagatesErrors(t *testing.T) {
	resumeBoom := errors.New("send-keys: no such pane")
	approveBoom := errors.New("approve failed")
	interruptBoom := errors.New("interrupt failed")

	ctl := newBoundActuatorCtl(t)
	ctl.resumeErr, ctl.approveErr, ctl.interruptErr = resumeBoom, approveBoom, interruptBoom
	act := boundController{ctrl: ctl, target: Target{Backend: "tmux", ID: "%3"}}

	if err := act.Resume("x"); !errors.Is(err, resumeBoom) {
		t.Errorf("Resume = %v, want %v", err, resumeBoom)
	}
	if err := act.Approve(); !errors.Is(err, approveBoom) {
		t.Errorf("Approve = %v, want %v", err, approveBoom)
	}
	if err := act.Interrupt(); !errors.Is(err, interruptBoom) {
		t.Errorf("Interrupt = %v, want %v", err, interruptBoom)
	}
}

// TestBoundController_BackendAndTier: Backend names the mechanism for the
// human-facing status text; Tier labels the actuation-event log. A multiplexer
// send is always Tier 1 — the label a Tier 1h host send must NOT share.
func TestBoundController_BackendAndTier(t *testing.T) {
	act := boundController{ctrl: newBoundActuatorCtl(t), target: Target{Backend: "tmux"}}

	if act.Backend() != "tmux" {
		t.Errorf("Backend() = %q, want tmux (the controller's Name)", act.Backend())
	}
	if act.Tier() != actuationTierMultiplexer {
		t.Errorf("Tier() = %q, want %q", act.Tier(), actuationTierMultiplexer)
	}
	if actuationTierMultiplexer == actuationTierHostSend {
		t.Fatal("tier labels must stay distinct — the actuation log is the only way to tell an in-place host write from a multiplexer send")
	}
}
