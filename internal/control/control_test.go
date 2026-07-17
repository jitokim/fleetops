package control

import "testing"

func TestEncodeCwd_SlashesToHyphens(t *testing.T) {
	got := encodeCwd("/home/user/myproject")
	want := "-home-user-myproject"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEncodeCwd_DotsAlsoBecomeHyphens(t *testing.T) {
	// verified against the real Claude Code project-dir encoding (residual
	// #4 / internal/claude.encodeCwd's identical contract): both "/" AND "."
	// collapse to "-".
	got := encodeCwd("/home/user/.someplugin/agent-sessions")
	want := "-home-user--someplugin-agent-sessions"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEncodeCwd_NoSeparators_Unchanged(t *testing.T) {
	if got := encodeCwd("noseparators"); got != "noseparators" {
		t.Errorf("got %q, want unchanged input", got)
	}
}
