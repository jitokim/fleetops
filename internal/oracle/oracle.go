// Package oracle independently judges whether a goal-bound loop's latest
// report demonstrates its goal is done, still in progress, or contradicts
// its own "done" claim (rejected/drift) — the human never has to trust the
// agent's self-report; see DESIGN.md §0's oracle/challenger/governor/gate
// layer. Uses a cheap model (haiku) via `claude -p`, since judging is a
// per-cycle cost paid for every bound loop.
package oracle

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/jitokim/fleetops/internal/domain"
)

// judgeTimeout bounds the claude -p call so a wedged judge can't hang the
// TUI's judgeCmd forever.
const judgeTimeout = 2 * time.Minute

// Judge asks an independent model to verdict a bound loop's progress toward
// goal, given only its last report (lastAssistantText) — never the agent's
// own claim of success, which is exactly what's being checked. cwd is the
// loop's working directory, told to the model explicitly: without it, the
// judge (itself running via `claude -p` from fleetops's own cwd) can
// wrongly reject a report for referencing paths that don't exist relative
// to ITS cwd — a real false rejection seen live ("file location does not
// match the current directory"). doneWhen and oracleRubric are the rest of
// the loop's wizard-collected contract (internal/registry.BindSpec /
// domain.Goal) — both may be "" (loop bound before the loop-contract slice,
// or left empty by the human at spawn time).
func Judge(goal, cwd, lastAssistantText, doneWhen, oracleRubric string) (domain.Verdict, error) {
	ctx, cancel := context.WithTimeout(context.Background(), judgeTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, "claude", "-p", buildPrompt(goal, cwd, lastAssistantText, doneWhen, oracleRubric),
		"--model", "haiku", "--output-format", "json").Output()
	if err != nil {
		return domain.Verdict{}, fmt.Errorf("oracle: claude -p failed: %w", err)
	}
	return parseVerdict(string(out))
}

// buildPrompt is the oracle's strict, JSON-only instruction — it must not
// trust the agent's own claims, only the evidence in its report. doneWhen
// and oracleRubric each add their own labeled line right after GOAL when
// non-empty — the same contract text the agent itself was given at spawn
// time (see the tui's buildSpawnPrompt), so what the agent was told and what
// it's judged against are the same document.
func buildPrompt(goal, cwd, lastAssistantText, doneWhen, oracleRubric string) string {
	var extra strings.Builder
	if doneWhen != "" {
		fmt.Fprintf(&extra, "\n\nCOMPLETION CONDITION (the goal counts as done ONLY if this is demonstrably met): %s", doneWhen)
	}
	if oracleRubric != "" {
		fmt.Fprintf(&extra, "\n\nVERIFICATION RUBRIC (how to judge): %s", oracleRubric)
	}

	return fmt.Sprintf(`You are an independent oracle judging an autonomous coding agent's progress toward a goal. You do NOT trust the agent's own claims of success — you verify against the evidence actually shown in its report.

The agent works in directory: %s. Paths under that directory ARE the agent's current directory — do not reject a report for referencing paths there as if they were somewhere else or didn't exist relative to your own working directory.

GOAL:
%s%s

AGENT'S LAST REPORT:
%s

Output ONLY a JSON object (no other text, no markdown code fences) with exactly these two fields:
{"outcome": "done" | "progress" | "rejected", "reason": "<one sentence>"}

Rules:
- "done": ONLY if the report clearly demonstrates the goal is fully achieved (e.g. tests shown passing, the described change verifiably complete). Do not accept a bare claim of completion as evidence.
- "rejected": the agent claims the goal is done, but the report's evidence is missing, incomplete, or contradicts that claim.
- "progress": neither of the above — real work is happening but the goal is not claimed done, nor refuted.`, cwd, goal, extra.String(), lastAssistantText)
}

// envelopeResult is the shape `claude -p --output-format json` wraps its
// answer in: {"result": "<the model's actual text output>", ...other
// fields we don't need}.
type envelopeResult struct {
	Result string `json:"result"`
}

// fencedJSON strips a ```json (or bare ```) fence around a JSON object, in
// case the model adds one despite being told not to.
var fencedJSON = regexp.MustCompile("(?s)```(?:json)?\\s*(\\{.*?\\})\\s*```")

// parseVerdict extracts the oracle's verdict from claude -p's raw stdout:
// unwraps the {"result": "..."} envelope (falling back to treating the raw
// output as the inner JSON directly, in case the envelope shape changes or
// isn't present), strips a code fence if the model added one, then parses
// the {"outcome","reason"} object. Returns an error on anything that isn't
// ultimately valid, recognized JSON — never guesses.
func parseVerdict(raw string) (domain.Verdict, error) {
	inner := raw
	var envelope envelopeResult
	if err := json.Unmarshal([]byte(raw), &envelope); err == nil && envelope.Result != "" {
		inner = envelope.Result
	}
	inner = extractJSONObject(inner)

	var v struct {
		Outcome string `json:"outcome"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(inner), &v); err != nil {
		return domain.Verdict{}, fmt.Errorf("oracle: could not parse verdict JSON: %w", err)
	}

	outcome := domain.Outcome(v.Outcome)
	switch outcome {
	case domain.OutcomeDone, domain.OutcomeProgress, domain.OutcomeRejected:
	default:
		return domain.Verdict{}, fmt.Errorf("oracle: unrecognized outcome %q", v.Outcome)
	}
	return domain.Verdict{Outcome: outcome, Reason: v.Reason}, nil
}

// extractJSONObject strips a ```json/``` fence around a JSON object if
// present, and trims surrounding whitespace.
func extractJSONObject(s string) string {
	s = strings.TrimSpace(s)
	if m := fencedJSON.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return s
}
