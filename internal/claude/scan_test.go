package claude

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jitokim/missionctl/internal/domain"
)

func writeJSONL(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	content := ""
	for _, l := range lines {
		content += l + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestLastUserPrompt_ReturnsLastOfTwo(t *testing.T) {
	path := writeJSONL(t,
		`{"type":"user","message":{"role":"user","content":"first prompt"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":"reply"}}`,
		`{"type":"user","message":{"role":"user","content":"second prompt"}}`,
	)

	got, ok := LastUserPrompt(path)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got != "second prompt" {
		t.Errorf("got %q, want %q", got, "second prompt")
	}
}

func TestLastUserPrompt_StringContent(t *testing.T) {
	path := writeJSONL(t, `{"type":"user","message":{"content":"plain string prompt"}}`)

	got, ok := LastUserPrompt(path)
	if !ok || got != "plain string prompt" {
		t.Errorf("got (%q, %v), want (%q, true)", got, ok, "plain string prompt")
	}
}

func TestLastUserPrompt_ArrayContent(t *testing.T) {
	path := writeJSONL(t, `{"type":"user","message":{"content":[{"type":"text","text":"array block prompt"}]}}`)

	got, ok := LastUserPrompt(path)
	if !ok || got != "array block prompt" {
		t.Errorf("got (%q, %v), want (%q, true)", got, ok, "array block prompt")
	}
}

func TestLastUserPrompt_ArrayContentSkipsNonTextBlocks(t *testing.T) {
	path := writeJSONL(t, `{"type":"user","message":{"content":[{"type":"tool_result","text":"ignored"},{"type":"text","text":"real prompt"}]}}`)

	got, ok := LastUserPrompt(path)
	if !ok || got != "real prompt" {
		t.Errorf("got (%q, %v), want (%q, true)", got, ok, "real prompt")
	}
}

func TestLastUserPrompt_EmptyFile(t *testing.T) {
	path := writeJSONL(t)

	if _, ok := LastUserPrompt(path); ok {
		t.Error("expected ok=false for empty file")
	}
}

func TestLastUserPrompt_NoUserMessages(t *testing.T) {
	path := writeJSONL(t, `{"type":"assistant","message":{"content":"reply only"}}`)

	if _, ok := LastUserPrompt(path); ok {
		t.Error("expected ok=false when no user messages present")
	}
}

func TestLastUserPrompt_UnparseableLineSkipped(t *testing.T) {
	path := writeJSONL(t,
		`{"type":"user","message":{"content":"good prompt"}}`,
		`not json at all`,
	)

	got, ok := LastUserPrompt(path)
	if !ok || got != "good prompt" {
		t.Errorf("got (%q, %v), want (%q, true) — malformed line should be skipped, not error", got, ok, "good prompt")
	}
}

func TestLastUserPrompt_MissingFile(t *testing.T) {
	if _, ok := LastUserPrompt(filepath.Join(t.TempDir(), "does-not-exist.jsonl")); ok {
		t.Error("expected ok=false for missing file")
	}
}

func TestIsHiddenProjectDir(t *testing.T) {
	cases := []struct {
		dir  string
		want bool
	}{
		{"-Users-imac--claude-mem-observer-sessions", true},
		{"-Users-imac-IdeaProjects-aboard", false},
	}
	for _, c := range cases {
		if got := isHiddenProjectDir(c.dir); got != c.want {
			t.Errorf("isHiddenProjectDir(%q) = %v, want %v", c.dir, got, c.want)
		}
	}
}

func TestTailState_AssistantEndTurn_Idle(t *testing.T) {
	path := writeJSONL(t,
		`{"type":"user","message":{"content":"do the thing"}}`,
		`{"type":"assistant","message":{"content":"done","stop_reason":"end_turn"}}`,
	)

	state, stall := tailState(path)
	if state != domain.StateIdle || stall != domain.StallNone {
		t.Errorf("got (%v, %v), want (%v, %v)", state, stall, domain.StateIdle, domain.StallNone)
	}
}

func TestTailState_LastEntryUser_StalledNoOutput(t *testing.T) {
	path := writeJSONL(t,
		`{"type":"assistant","message":{"content":"working","stop_reason":"end_turn"}}`,
		`{"type":"user","message":{"content":"still going"}}`,
	)

	state, stall := tailState(path)
	if state != domain.StateStalled || stall != domain.StallNoOutput {
		t.Errorf("got (%v, %v), want (%v, %v)", state, stall, domain.StateStalled, domain.StallNoOutput)
	}
}

func TestTailState_AssistantToolUse_StalledNoOutput(t *testing.T) {
	// an assistant message mid-work (tool_use, no stop_reason end_turn) is
	// not a finished turn — still an incident, not idle.
	path := writeJSONL(t,
		`{"type":"user","message":{"content":"do the thing"}}`,
		`{"type":"assistant","message":{"content":"working","stop_reason":"tool_use"}}`,
	)

	state, stall := tailState(path)
	if state != domain.StateStalled || stall != domain.StallNoOutput {
		t.Errorf("got (%v, %v), want (%v, %v)", state, stall, domain.StateStalled, domain.StallNoOutput)
	}
}

func TestTailState_RateLimitBeatsEndTurn(t *testing.T) {
	// even though the last message looks like a finished turn, a 429 marker
	// anywhere in the tail means the turn did NOT actually complete.
	path := writeJSONL(t,
		`{"type":"assistant","message":{"content":"429 Too Many Requests: rate limit exceeded"}}`,
		`{"type":"assistant","message":{"content":"done","stop_reason":"end_turn"}}`,
	)

	state, stall := tailState(path)
	if state != domain.StateStalled || stall != domain.StallRateLimit {
		t.Errorf("got (%v, %v), want (%v, %v)", state, stall, domain.StateStalled, domain.StallRateLimit)
	}
}

func TestTailState_MissingFile_StalledNoOutput(t *testing.T) {
	state, stall := tailState(filepath.Join(t.TempDir(), "does-not-exist.jsonl"))
	if state != domain.StateStalled || stall != domain.StallNoOutput {
		t.Errorf("got (%v, %v), want (%v, %v)", state, stall, domain.StateStalled, domain.StallNoOutput)
	}
}
