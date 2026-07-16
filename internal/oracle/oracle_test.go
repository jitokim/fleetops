package oracle

import (
	"strings"
	"testing"

	"github.com/jitokim/missionctl/internal/domain"
)

func TestParseVerdict_EnvelopeCleanJSON(t *testing.T) {
	raw := `{"result":"{\"outcome\":\"done\",\"reason\":\"tests pass\"}"}`
	v, err := parseVerdict(raw)
	if err != nil {
		t.Fatalf("parseVerdict: %v", err)
	}
	if v.Outcome != domain.OutcomeDone || v.Reason != "tests pass" {
		t.Errorf("got %+v, want {done, tests pass}", v)
	}
}

func TestParseVerdict_EnvelopeFencedJSON(t *testing.T) {
	raw := `{"result":"Here you go:\n\n` + "```json\\n{\\\"outcome\\\":\\\"rejected\\\",\\\"reason\\\":\\\"no test output shown\\\"}\\n```" + `"}`
	v, err := parseVerdict(raw)
	if err != nil {
		t.Fatalf("parseVerdict: %v", err)
	}
	if v.Outcome != domain.OutcomeRejected || v.Reason != "no test output shown" {
		t.Errorf("got %+v, want {rejected, no test output shown}", v)
	}
}

func TestParseVerdict_BareInnerJSONNoEnvelope(t *testing.T) {
	// tolerate the inner JSON directly, in case the envelope shape isn't
	// present (defensive — the documented shape always wraps in "result",
	// but parseVerdict shouldn't hard-fail if that ever changes).
	raw := `{"outcome":"progress","reason":"working on it"}`
	v, err := parseVerdict(raw)
	if err != nil {
		t.Fatalf("parseVerdict: %v", err)
	}
	if v.Outcome != domain.OutcomeProgress || v.Reason != "working on it" {
		t.Errorf("got %+v, want {progress, working on it}", v)
	}
}

func TestParseVerdict_BareFencedJSON(t *testing.T) {
	raw := "```json\n{\"outcome\":\"done\",\"reason\":\"ok\"}\n```"
	v, err := parseVerdict(raw)
	if err != nil {
		t.Fatalf("parseVerdict: %v", err)
	}
	if v.Outcome != domain.OutcomeDone {
		t.Errorf("Outcome = %v, want done", v.Outcome)
	}
}

func TestParseVerdict_GarbageReturnsError(t *testing.T) {
	if _, err := parseVerdict("not json at all"); err == nil {
		t.Error("expected an error for unparseable input")
	}
}

func TestParseVerdict_UnrecognizedOutcomeReturnsError(t *testing.T) {
	raw := `{"result":"{\"outcome\":\"maybe\",\"reason\":\"unsure\"}"}`
	if _, err := parseVerdict(raw); err == nil {
		t.Error("expected an error for an unrecognized outcome value")
	}
}

func TestBuildPrompt_NoDoneWhenNoOracle_OmitsExtraLines(t *testing.T) {
	prompt := buildPrompt("fix the bug", "/x/aboard", "done, tests pass", "", "")
	if strings.Contains(prompt, "COMPLETION CONDITION") {
		t.Error("expected no COMPLETION CONDITION line when doneWhen is empty")
	}
	if strings.Contains(prompt, "VERIFICATION RUBRIC") {
		t.Error("expected no VERIFICATION RUBRIC line when oracleRubric is empty")
	}
	if !strings.Contains(prompt, "GOAL:\nfix the bug") {
		t.Error("expected the GOAL line to be present and unmodified")
	}
}

func TestBuildPrompt_WithDoneWhen_IncludesCompletionConditionLine(t *testing.T) {
	prompt := buildPrompt("fix the bug", "/x/aboard", "report", "go test ./... passes", "")
	if !strings.Contains(prompt, "COMPLETION CONDITION (the goal counts as done ONLY if this is demonstrably met): go test ./... passes") {
		t.Errorf("prompt missing the completion condition line: %s", prompt)
	}
	if strings.Contains(prompt, "VERIFICATION RUBRIC") {
		t.Error("expected no VERIFICATION RUBRIC line when oracleRubric is empty")
	}
}

func TestBuildPrompt_WithOracleRubric_IncludesVerificationRubricLine(t *testing.T) {
	prompt := buildPrompt("fix the bug", "/x/aboard", "report", "", "go test ./... must pass")
	if !strings.Contains(prompt, "VERIFICATION RUBRIC (how to judge): go test ./... must pass") {
		t.Errorf("prompt missing the verification rubric line: %s", prompt)
	}
	if strings.Contains(prompt, "COMPLETION CONDITION") {
		t.Error("expected no COMPLETION CONDITION line when doneWhen is empty")
	}
}

func TestBuildPrompt_WithBoth_IncludesBothLines(t *testing.T) {
	prompt := buildPrompt("fix the bug", "/x/aboard", "report", "tests pass", "check CI output")
	if !strings.Contains(prompt, "COMPLETION CONDITION (the goal counts as done ONLY if this is demonstrably met): tests pass") {
		t.Errorf("prompt missing the completion condition line: %s", prompt)
	}
	if !strings.Contains(prompt, "VERIFICATION RUBRIC (how to judge): check CI output") {
		t.Errorf("prompt missing the verification rubric line: %s", prompt)
	}
}

func TestParseVerdict_EmptyEnvelopeResultFallsBackToRawInner(t *testing.T) {
	// an envelope that parses but has an empty "result": inner falls back to
	// the raw envelope string itself, which has no outcome/reason fields —
	// unmarshal succeeds with a zero-value outcome, which is then rejected
	// as unrecognized.
	if _, err := parseVerdict(`{"result":""}`); err == nil {
		t.Error("expected an error — no outcome/reason to parse")
	}
}
