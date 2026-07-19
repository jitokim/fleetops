package claude

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jitokim/fleetops/internal/domain"
	"github.com/jitokim/fleetops/internal/events"
	"github.com/jitokim/fleetops/internal/gate"
	"github.com/jitokim/fleetops/internal/registry"
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
		{"-home-user--someplugin-agent-sessions", true},
		{"-home-user-myproject", false},
	}
	for _, c := range cases {
		if got := isHiddenProjectDir(c.dir); got != c.want {
			t.Errorf("isHiddenProjectDir(%q) = %v, want %v", c.dir, got, c.want)
		}
	}
}

func TestTailState_AssistantEndTurn_IdleRegardlessOfRecency(t *testing.T) {
	// "running" means a turn is in flight, not just "wrote recently" — a
	// finished turn is idle no matter how long (or briefly) ago that was.
	path := writeJSONL(t,
		`{"type":"user","message":{"content":"do the thing"}}`,
		`{"type":"assistant","message":{"content":"done","stop_reason":"end_turn"}}`,
	)

	for _, idleFor := range []time.Duration{time.Second, IdleThreshold * 10} {
		state, stall := tailState(path, idleFor)
		if state != domain.StateIdle || stall != domain.StallNone {
			t.Errorf("idleFor=%v: got (%v, %v), want (%v, %v)", idleFor, state, stall, domain.StateIdle, domain.StallNone)
		}
	}
}

func TestTailState_MidTurn_RunningWhenRecent(t *testing.T) {
	path := writeJSONL(t,
		`{"type":"assistant","message":{"content":"working","stop_reason":"end_turn"}}`,
		`{"type":"user","message":{"content":"still going"}}`,
	)

	state, stall := tailState(path, time.Second)
	if state != domain.StateRunning || stall != domain.StallNone {
		t.Errorf("got (%v, %v), want (%v, %v) — mid-turn + recent write = running, not stalled", state, stall, domain.StateRunning, domain.StallNone)
	}
}

func TestTailState_MidTurn_StalledNoOutputWhenIdle(t *testing.T) {
	path := writeJSONL(t,
		`{"type":"assistant","message":{"content":"working","stop_reason":"end_turn"}}`,
		`{"type":"user","message":{"content":"still going"}}`,
	)

	state, stall := tailState(path, IdleThreshold)
	if state != domain.StateStalled || stall != domain.StallNoOutput {
		t.Errorf("got (%v, %v), want (%v, %v)", state, stall, domain.StateStalled, domain.StallNoOutput)
	}
}

func TestTailState_AssistantToolUse_StalledWhenIdle(t *testing.T) {
	// an assistant message mid-work (tool_use, no stop_reason end_turn) is
	// not a finished turn — an incident once idle, not idle-state.
	path := writeJSONL(t,
		`{"type":"user","message":{"content":"do the thing"}}`,
		`{"type":"assistant","message":{"content":"working","stop_reason":"tool_use"}}`,
	)

	state, stall := tailState(path, IdleThreshold)
	if state != domain.StateStalled || stall != domain.StallNoOutput {
		t.Errorf("got (%v, %v), want (%v, %v)", state, stall, domain.StateStalled, domain.StallNoOutput)
	}
}

func TestTailState_RateLimitMidTurn_StalledRateLimit(t *testing.T) {
	// a 429 with no subsequent end_turn: the turn never completed.
	// fix/exit-gate-ux (architecture judge item C): fixture updated to the
	// genuine Claude-Code-synthesized "API Error:" shape (same convention
	// internal/claude.LastError's own fixtures use) — hasRateLimitMarker no
	// longer matches a bare "429"/"rate limit" mention without it (see its
	// doc), so a mid-turn tail carrying only that bare phrase would no
	// longer classify as StallRateLimit; this fixture stays REPRESENTATIVE
	// of what a real rate-limited entry looks like.
	path := writeJSONL(t,
		`{"type":"user","message":{"content":"go"}}`,
		`{"type":"assistant","message":{"content":"API Error: 429 Too Many Requests: rate limit exceeded"}}`,
	)

	state, stall := tailState(path, IdleThreshold)
	if state != domain.StateStalled || stall != domain.StallRateLimit {
		t.Errorf("got (%v, %v), want (%v, %v)", state, stall, domain.StateStalled, domain.StallRateLimit)
	}
}

// TestTailState_BareRateLimitMention_NotStalledRateLimit is the P2
// regression itself (architecture judge item C, the SAME false-positive
// class as fix/last-error-false-positive): a mid-turn tail whose only
// "429"/"rate limit" mention is ordinary conversation — no genuine
// synthesized API error present at all — must NOT classify as
// StallRateLimit. Falls back to the generic StallNoOutput, same as any
// other stalled-with-no-specific-cause tail.
func TestTailState_BareRateLimitMention_NotStalledRateLimit(t *testing.T) {
	path := writeJSONL(t,
		`{"type":"user","message":{"content":"go"}}`,
		`{"type":"assistant","message":{"content":"landed #24 (429 auto-redrive) — Tier 2 only, opt-in. main is green."}}`,
	)

	state, stall := tailState(path, IdleThreshold)
	if state != domain.StateStalled || stall != domain.StallNoOutput {
		t.Errorf("got (%v, %v), want (%v, %v) — mentioning \"429\" as a feature name is not a real rate-limit signal",
			state, stall, domain.StateStalled, domain.StallNoOutput)
	}
}

// TestHasRateLimitMarker_StructuredRateLimitErrorShape_MatchesWithoutAPIErrorPrefix
// pins the OTHER genuine signal (architecture judge item C's "OR the actual
// rate_limit_error shape"): Anthropic's real structured JSON error type
// slug (`"type":"rate_limit_error"`) is trustworthy on its own, without
// requiring Claude Code's "API Error:" text prefix too — it's exact JSON
// key:value shape, not prose, so it can't coincidentally appear in ordinary
// conversation the way a bare "429"/"rate limit" word can.
func TestHasRateLimitMarker_StructuredRateLimitErrorShape_MatchesWithoutAPIErrorPrefix(t *testing.T) {
	buf := []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
	if !hasRateLimitMarker(buf) {
		t.Error("expected the structured rate_limit_error JSON shape to match on its own")
	}
}

func TestTailState_EndTurnAfterEarlierRateLimitMention_Idle(t *testing.T) {
	// the LAST meaningful entry decides: an end_turn after an earlier
	// rate-limit mention elsewhere in the tail is still idle — the turn
	// evidently did complete after all.
	path := writeJSONL(t,
		`{"type":"assistant","message":{"content":"429 Too Many Requests: rate limit exceeded"}}`,
		`{"type":"assistant","message":{"content":"done","stop_reason":"end_turn"}}`,
	)

	state, stall := tailState(path, IdleThreshold)
	if state != domain.StateIdle || stall != domain.StallNone {
		t.Errorf("got (%v, %v), want (%v, %v)", state, stall, domain.StateIdle, domain.StallNone)
	}
}

func TestTailState_MissingFile_StalledNoOutput(t *testing.T) {
	state, stall := tailState(filepath.Join(t.TempDir(), "does-not-exist.jsonl"), IdleThreshold)
	if state != domain.StateStalled || stall != domain.StallNoOutput {
		t.Errorf("got (%v, %v), want (%v, %v)", state, stall, domain.StateStalled, domain.StallNoOutput)
	}
}

func TestLoopFromLog_EndTurnRecent_IsIdleNotRunning(t *testing.T) {
	// the addendum bug: a loop that finished its turn 1 minute ago (well
	// within IdleThreshold) must show idle, not running — "running" means a
	// turn is in flight, not merely "wrote recently".
	path := writeJSONL(t,
		`{"type":"user","message":{"content":"do the thing"}}`,
		`{"type":"assistant","message":{"content":"done","stop_reason":"end_turn"}}`,
	)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat fixture: %v", err)
	}

	l := loopFromLog(path, fi, time.Now(), t.TempDir(), nil)
	if l.State != domain.StateIdle {
		t.Errorf("got State=%v, want %v (a finished turn is idle regardless of recency, not running)", l.State, domain.StateIdle)
	}
}

func TestIsGateNotification(t *testing.T) {
	cases := []struct {
		name string
		info gate.Info
		want bool
	}{
		{"permission_prompt type", gate.Info{Type: "permission_prompt", Message: "Permission required to run: Bash(npm test)"}, true},
		{"elicitation_dialog type", gate.Info{Type: "elicitation_dialog", Message: "anything"}, true},
		{"agent_needs_input type", gate.Info{Type: "agent_needs_input", Message: "anything"}, true},
		{"idle_prompt type", gate.Info{Type: "idle_prompt", Message: "Claude is waiting for your input"}, false},
		{"unknown type", gate.Info{Type: "auth_success", Message: "anything"}, false},
		{"empty type, permission-ish message (fallback)", gate.Info{Type: "", Message: "Permission required to run: Bash(npm test)"}, true},
		{"empty type, idle message (fallback)", gate.Info{Type: "", Message: "Claude is waiting for your input"}, false},
		{"empty type, empty message (fallback)", gate.Info{Type: "", Message: ""}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isGateNotification(c.info); got != c.want {
				t.Errorf("isGateNotification(%+v) = %v, want %v", c.info, got, c.want)
			}
		})
	}
}

func TestLoopFromLog_ActivePermissionPromptType_BeatsAnyTailClassification(t *testing.T) {
	// mid-turn AND idle long past IdleThreshold — would classify Stalled on
	// its own — but an active permission_prompt gate marker must win
	// regardless.
	path := writeJSONL(t,
		`{"type":"user","message":{"content":"do the thing"}}`,
		`{"type":"assistant","message":{"content":"working","stop_reason":"tool_use"}}`,
	)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat fixture: %v", err)
	}
	session := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	pending := map[string]gate.Info{
		session: {Type: "permission_prompt", Message: "Permission required to run: Bash(npm test)", TS: fi.ModTime()},
	}

	l := loopFromLog(path, fi, fi.ModTime().Add(time.Hour), t.TempDir(), pending)
	if l.State != domain.StateGate {
		t.Errorf("got State=%v, want %v", l.State, domain.StateGate)
	}
	if l.GatePrompt != "Permission required to run: Bash(npm test)" {
		t.Errorf("GatePrompt = %q, want %q", l.GatePrompt, "Permission required to run: Bash(npm test)")
	}
	if l.GateTS != fi.ModTime().UnixNano() {
		t.Errorf("GateTS = %d, want %d (nanoseconds, so approveCmd's CAS delete can distinguish same-second markers)", l.GateTS, fi.ModTime().UnixNano())
	}
	if l.Stall != domain.StallNone {
		t.Errorf("Stall = %v, want %v (gated, not stalled)", l.Stall, domain.StallNone)
	}
}

func TestLoopFromLog_ActiveIdlePromptType_NotGated_MarkerDeleted(t *testing.T) {
	// Claude Code fires the SAME Notification hook for the 60s idle nudge
	// (notification_type "idle_prompt") — that is NOT a gate, it just means
	// idle. Ends on a finished turn, so the fallthrough classification lands
	// on StateIdle.
	path := writeJSONL(t,
		`{"type":"user","message":{"content":"do the thing"}}`,
		`{"type":"assistant","message":{"content":"done","stop_reason":"end_turn"}}`,
	)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat fixture: %v", err)
	}
	session := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	gatesDir := t.TempDir()
	if err := gate.WriteMarker(gatesDir, session, gate.Info{Message: "Claude is waiting for your input", Type: "idle_prompt"}); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
	// Read back the REAL on-disk TS rather than fabricating one — mirroring
	// production, where pending always comes from gate.Pending(gatesDir)
	// reading this exact file. DeleteMarkerIfTS's compare-and-swap only
	// deletes when the on-disk TS matches; nanosecond precision means even
	// a sub-millisecond gap between a synthetic TS and the real one would
	// (correctly) refuse the delete — see TestLoopFromLog_StaleGate_DeletedAndIgnored's
	// identical fixture-consistency requirement.
	pending := gate.Pending(gatesDir)

	l := loopFromLog(path, fi, fi.ModTime(), gatesDir, pending)
	if l.State != domain.StateIdle {
		t.Errorf("got State=%v, want %v (idle_prompt is not a gate)", l.State, domain.StateIdle)
	}
	if len(gate.Pending(gatesDir)) != 0 {
		t.Error("expected the non-gate marker to be deleted")
	}
}

func TestLoopFromLog_ActiveEmptyTypePermissionMessage_GatedViaFallback(t *testing.T) {
	// older claude versions that predate notification_type: falls back to
	// the message-text heuristic.
	path := writeJSONL(t,
		`{"type":"user","message":{"content":"do the thing"}}`,
		`{"type":"assistant","message":{"content":"working","stop_reason":"tool_use"}}`,
	)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat fixture: %v", err)
	}
	session := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	pending := map[string]gate.Info{
		session: {Message: "Permission required to run: Bash(npm test)", TS: fi.ModTime()},
	}

	l := loopFromLog(path, fi, fi.ModTime().Add(time.Hour), t.TempDir(), pending)
	if l.State != domain.StateGate {
		t.Errorf("got State=%v, want %v (empty-type fallback on a permission-ish message)", l.State, domain.StateGate)
	}
}

func TestLoopFromLog_StaleGate_DeletedAndIgnored(t *testing.T) {
	// the marker fired, but the log was written to well after — the human
	// already answered, so the marker is stale and must not override State,
	// and the marker file itself should get cleaned up.
	path := writeJSONL(t,
		`{"type":"user","message":{"content":"do the thing"}}`,
		`{"type":"assistant","message":{"content":"working","stop_reason":"tool_use"}}`,
	)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat fixture: %v", err)
	}
	session := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	gatesDir := t.TempDir()
	staleTS := fi.ModTime().Add(-time.Hour) // long before the log's last write

	// Write the on-disk marker with the SAME stale TS the pending map below
	// carries — mirroring production, where pending always comes from
	// gate.Pending(gatesDir) reading this exact file. DeleteMarkerIfTS's
	// compare-and-swap only deletes when the on-disk TS matches, so a
	// pending map with a synthetic TS that doesn't match what's on disk
	// would (correctly) refuse the delete — this fixture must keep them in
	// sync to exercise the stale-cleanup path.
	if err := os.WriteFile(
		filepath.Join(gatesDir, session+".json"),
		[]byte(fmt.Sprintf(`{"message":"old question","type":"permission_prompt","ts":%d}`, staleTS.UnixNano())),
		0o644,
	); err != nil {
		t.Fatalf("write stale marker fixture: %v", err)
	}
	pending := map[string]gate.Info{session: {Message: "old question", TS: staleTS}}

	l := loopFromLog(path, fi, fi.ModTime().Add(time.Hour), gatesDir, pending)
	if l.State == domain.StateGate {
		t.Errorf("got State=%v, want anything but StateGate (marker is stale)", l.State)
	}
	if len(gate.Pending(gatesDir)) != 0 {
		t.Error("expected the stale marker file to be deleted")
	}
}

// askUserQuestionText / askUserQuestionLine mirror the real shape of a pending
// AskUserQuestion tool_use (verified against a live session log): an assistant
// entry, stop_reason "tool_use", whose message.content array holds a tool_use
// block named "AskUserQuestion" with input.questions[0].question.
const askUserQuestionText = "Which JIRA key should this config-change commit use?"

const askUserQuestionLine = `{"type":"assistant","message":{"role":"assistant","stop_reason":"tool_use","content":[{"type":"tool_use","name":"AskUserQuestion","input":{"questions":[{"question":"Which JIRA key should this config-change commit use?","header":"JIRA ticket","multiSelect":false,"options":[{"label":"File a new JIRA ticket (recommended)","description":"Create a new ticket and commit under that key"},{"label":"Proceed without a ticket","description":"Convention exception"}]}]}}]}}`

func TestPendingAskUserQuestion(t *testing.T) {
	const bashToolUseLine = `{"type":"assistant","message":{"role":"assistant","stop_reason":"tool_use","content":[{"type":"tool_use","name":"Bash","input":{"command":"go test ./..."}}]}}`
	// A user entry answering the question (a tool_result) — once this is the
	// last user/assistant entry, the question is no longer pending.
	const answerLine = `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"File a new JIRA ticket (recommended)"}]}}`

	cases := []struct {
		name         string
		lines        []string
		wantOK       bool
		wantQuestion string
	}{
		{
			name:         "pending AskUserQuestion is the last turn",
			lines:        []string{`{"type":"user","message":{"content":"which key?"}}`, askUserQuestionLine},
			wantOK:       true,
			wantQuestion: askUserQuestionText,
		},
		{
			// The exact real-world case that broke the naive "check the literal
			// last line" approach: Claude Code appends non-turn system/attachment
			// entries AFTER a pending question. They must be filtered out.
			name: "trailing system/attachment noise after pending question",
			lines: []string{
				`{"type":"user","message":{"content":"which key?"}}`,
				askUserQuestionLine,
				`{"type":"attachment","attachment":{"type":"task_reminder"}}`,
				`{"type":"attachment","attachment":{"type":"task_reminder"}}`,
				`{"type":"system","content":"periodic reminder"}`,
			},
			wantOK:       true,
			wantQuestion: askUserQuestionText,
		},
		{
			name:   "answered — a later user tool_result is now the last entry",
			lines:  []string{`{"type":"user","message":{"content":"which key?"}}`, askUserQuestionLine, answerLine},
			wantOK: false,
		},
		{
			name:   "completed turn (end_turn) is not a gate",
			lines:  []string{`{"type":"user","message":{"content":"do it"}}`, `{"type":"assistant","message":{"content":"done","stop_reason":"end_turn"}}`},
			wantOK: false,
		},
		{
			name:   "a different tool (Bash) is not a gate",
			lines:  []string{`{"type":"user","message":{"content":"do it"}}`, bashToolUseLine},
			wantOK: false,
		},
		{
			name:   "content is a plain string, not a block array",
			lines:  []string{`{"type":"assistant","message":{"content":"AskUserQuestion","stop_reason":"tool_use"}}`},
			wantOK: false,
		},
		{
			name:   "malformed: input.questions missing",
			lines:  []string{`{"type":"assistant","message":{"stop_reason":"tool_use","content":[{"type":"tool_use","name":"AskUserQuestion","input":{}}]}}`},
			wantOK: false,
		},
		{
			name:   "malformed: questions is an empty array",
			lines:  []string{`{"type":"assistant","message":{"stop_reason":"tool_use","content":[{"type":"tool_use","name":"AskUserQuestion","input":{"questions":[]}}]}}`},
			wantOK: false,
		},
		{
			name:   "malformed: question field missing on first question",
			lines:  []string{`{"type":"assistant","message":{"stop_reason":"tool_use","content":[{"type":"tool_use","name":"AskUserQuestion","input":{"questions":[{"header":"JIRA"}]}}]}}`},
			wantOK: false,
		},
		{
			name:   "empty buffer",
			lines:  nil,
			wantOK: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			buf := []byte(strings.Join(c.lines, "\n"))
			question, _, ok := pendingAskUserQuestion(buf)
			if ok != c.wantOK {
				t.Fatalf("pendingAskUserQuestion ok=%v, want %v (question=%q)", ok, c.wantOK, question)
			}
			if ok && question != c.wantQuestion {
				t.Errorf("question=%q, want %q", question, c.wantQuestion)
			}
			if !ok && question != "" {
				t.Errorf("question=%q, want empty string when ok=false", question)
			}
		})
	}
}

func TestPendingAskUserQuestion_LongQuestionCollapsedAndCapped(t *testing.T) {
	// The extracted question feeds a single-line, status-line-style GatePrompt,
	// so it must be newline-collapsed and length-bounded (via summarizeTailText,
	// tailTextCap) — a pathological multi-line/very-long question must not blow
	// up the gate callout.
	long := "line one\n" + strings.Repeat("字", tailTextCap+50) // "字" is a 3-byte-per-rune CJK filler, same width class as the multibyte hazard being tested
	line := fmt.Sprintf(`{"type":"assistant","message":{"stop_reason":"tool_use","content":[{"type":"tool_use","name":"AskUserQuestion","input":{"questions":[{"question":%q}]}}]}}`, long)

	question, _, ok := pendingAskUserQuestion([]byte(line))
	if !ok {
		t.Fatal("expected ok=true for a valid (if long) question")
	}
	if strings.Contains(question, "\n") {
		t.Error("expected newlines collapsed to spaces in the gate prompt")
	}
	if n := utf8.RuneCountInString(question); n > tailTextCap+1 { // +1 for the ellipsis rune
		t.Errorf("question rune length = %d, want <= %d (bounded by tailTextCap)", n, tailTextCap+1)
	}
}

func TestLoopFromLog_PendingAskUserQuestion_ClassifiesAsGate(t *testing.T) {
	// End-to-end: a loop whose tail is a pending AskUserQuestion (with the
	// real-world trailing attachment/system noise) classifies as ◆ GATE with a
	// non-empty GatePrompt and StallNone — REGARDLESS of idleFor, because a
	// pending human decision is not a recency question. No hook marker exists
	// for AskUserQuestion (upstream gap), so pending is nil.
	path := writeJSONL(t,
		`{"type":"user","message":{"content":"which key?"}}`,
		askUserQuestionLine,
		`{"type":"attachment","attachment":{"type":"task_reminder"}}`,
		`{"type":"system","content":"periodic reminder"}`,
	)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat fixture: %v", err)
	}

	// fresh (well within IdleThreshold) AND long-idle (well past it): a pending
	// AskUserQuestion is a gate in both.
	for _, now := range []time.Time{fi.ModTime().Add(time.Second), fi.ModTime().Add(time.Hour)} {
		l := loopFromLog(path, fi, now, t.TempDir(), nil)
		if l.State != domain.StateGate {
			t.Errorf("now=%v: got State=%v, want %v", now, l.State, domain.StateGate)
		}
		if l.Stall != domain.StallNone {
			t.Errorf("now=%v: got Stall=%v, want %v (gated, not stalled)", now, l.Stall, domain.StallNone)
		}
		if l.GatePrompt != askUserQuestionText {
			t.Errorf("now=%v: GatePrompt=%q, want %q", now, l.GatePrompt, askUserQuestionText)
		}
		if l.GateTS != 0 {
			t.Errorf("now=%v: GateTS=%d, want 0 (no hook marker backs an AskUserQuestion gate)", now, l.GateTS)
		}
	}
}

func TestLoopFromLog_HookMarkerGate_BeatsAskUserQuestion(t *testing.T) {
	// If a real Notification-hook gate marker AND a pending AskUserQuestion
	// coexist (rare — some OTHER gate fired around the same time), the
	// authoritative hook marker wins: its Message is the GatePrompt and GateTS
	// is set, not clobbered by the tail-derived AskUserQuestion.
	path := writeJSONL(t,
		`{"type":"user","message":{"content":"which key?"}}`,
		askUserQuestionLine,
	)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat fixture: %v", err)
	}
	session := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	pending := map[string]gate.Info{
		session: {Type: "permission_prompt", Message: "Permission required to run: Bash(npm test)", TS: fi.ModTime()},
	}

	l := loopFromLog(path, fi, fi.ModTime().Add(time.Hour), t.TempDir(), pending)
	if l.State != domain.StateGate {
		t.Fatalf("got State=%v, want %v", l.State, domain.StateGate)
	}
	if l.GatePrompt != "Permission required to run: Bash(npm test)" {
		t.Errorf("GatePrompt=%q, want the hook marker's Message (not the AskUserQuestion text)", l.GatePrompt)
	}
	if l.GateTS != fi.ModTime().UnixNano() {
		t.Errorf("GateTS=%d, want %d (the hook marker's TS, preserved)", l.GateTS, fi.ModTime().UnixNano())
	}
}

func TestLastAssistantText_StringContent(t *testing.T) {
	path := writeJSONL(t, `{"type":"assistant","message":{"content":"plain string reply"}}`)

	got, ok := LastAssistantText(path)
	if !ok || got != "plain string reply" {
		t.Errorf("got (%q, %v), want (%q, true)", got, ok, "plain string reply")
	}
}

func TestLastAssistantText_ArrayContentSkipsToolUse(t *testing.T) {
	path := writeJSONL(t, `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash"},{"type":"text","text":"array block reply"}]}}`)

	got, ok := LastAssistantText(path)
	if !ok || got != "array block reply" {
		t.Errorf("got (%q, %v), want (%q, true)", got, ok, "array block reply")
	}
}

func TestLastAssistantText_None(t *testing.T) {
	path := writeJSONL(t, `{"type":"user","message":{"content":"just a user message"}}`)

	if _, ok := LastAssistantText(path); ok {
		t.Error("expected ok=false when no assistant text present")
	}
}

func TestLastAssistantText_ReturnsLastOfTwo(t *testing.T) {
	path := writeJSONL(t,
		`{"type":"assistant","message":{"content":"first reply"}}`,
		`{"type":"user","message":{"content":"more instructions"}}`,
		`{"type":"assistant","message":{"content":"second reply"}}`,
	)

	got, ok := LastAssistantText(path)
	if !ok || got != "second reply" {
		t.Errorf("got (%q, %v), want (%q, true)", got, ok, "second reply")
	}
}

func TestLastAssistantText_CollapsesNewlinesAndCapsAtTailTextCap(t *testing.T) {
	long := strings.Repeat("a", tailTextCap+100) // exceed the cap so truncation runs
	path := writeJSONL(t, fmt.Sprintf(`{"type":"assistant","message":{"content":%q}}`, "line one\nline two\n"+long))

	got, ok := LastAssistantText(path)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if strings.Contains(got, "\n") {
		t.Errorf("got %q, want newlines collapsed to spaces", got)
	}
	if !strings.HasPrefix(got, "line one line two") {
		t.Errorf("got %q, want to start with %q", got, "line one line two")
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("got %q, want truncated with an ellipsis", got)
	}
	// tailTextCap runes of content + the 1-rune ellipsis marker.
	if n := len([]rune(got)); n != tailTextCap+1 {
		t.Errorf("capped length = %d runes, want %d (tailTextCap + ellipsis)", n, tailTextCap+1)
	}
}

func TestLastAssistantText_CapIsRuneSafeForMultibyteText(t *testing.T) {
	// Many scripts (CJK, for example) are 3 bytes/rune in UTF-8, so a
	// byte-index cut at tailTextCap would land mid-rune for content like
	// this (tailTextCap is not a multiple of 3) — the exact hazard trunc's
	// own doc comment warns about ("byte-index slice can land mid-
	// character... stray '�' glyphs"). Agent transcripts routinely contain
	// non-ASCII text, so this is a realistic case, not a contrived one.
	long := strings.Repeat("字", tailTextCap+100) // exceed the cap so truncation runs
	path := writeJSONL(t, fmt.Sprintf(`{"type":"assistant","message":{"content":%q}}`, long))

	got, ok := LastAssistantText(path)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if !utf8.ValidString(got) {
		t.Errorf("got invalid UTF-8 (mid-rune cut): %q", got)
	}
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Errorf("got a replacement char (byte-boundary cut a multi-byte rune): %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("got %q, want truncated with an ellipsis", got)
	}
	if n := len([]rune(got)); n != tailTextCap+1 {
		t.Errorf("capped length = %d runes, want %d (tailTextCap + ellipsis)", n, tailTextCap+1)
	}
}

func TestLastAssistantText_UnderCapNotTruncated(t *testing.T) {
	// A message shorter than the cap is returned whole, with no ellipsis — the
	// detail pane's TAIL row now shows several lines, so short reports aren't cut.
	body := strings.Repeat("b", tailTextCap-100)
	path := writeJSONL(t, fmt.Sprintf(`{"type":"assistant","message":{"content":%q}}`, body))

	got, ok := LastAssistantText(path)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got != body {
		t.Errorf("got %d runes, want the message returned verbatim (%d runes, no truncation)", len([]rune(got)), len([]rune(body)))
	}
	if strings.HasSuffix(got, "…") {
		t.Errorf("under-cap message must not be marked truncated: ends with … = %q…", got[:20])
	}
}

func TestLastAssistantText_MissingFile(t *testing.T) {
	if _, ok := LastAssistantText(filepath.Join(t.TempDir(), "does-not-exist.jsonl")); ok {
		t.Error("expected ok=false for missing file")
	}
}

// ── F2: DOING/TAIL must not go empty for the busiest loops ───────────

func TestLastAssistantText_LastMessageToolOnly_FallsBackToEarlierText(t *testing.T) {
	// the literal F2 scenario: the LAST assistant message is a separate,
	// later entry with no text block at all — must fall back to the most
	// recent assistant text found anywhere in the tail, not just the very
	// last assistant entry.
	path := writeJSONL(t,
		`{"type":"assistant","message":{"content":"did the work, tests pass"}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"ok"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}`,
	)

	got, ok := LastAssistantText(path)
	if !ok || got != "did the work, tests pass" {
		t.Errorf("got (%q, %v), want (%q, true)", got, ok, "did the work, tests pass")
	}
}

// bigToolResultFiller is comfortably bigger than tailBytes (24KB) but well
// under lastTextTailBytes (256KB) — big enough that a real assistant text
// entry preceding it falls OUTSIDE the standard tail window, small enough
// that the widened window still reaches it.
func bigToolResultFiller() string {
	return strings.Repeat("x", 40*1024)
}

func TestLastAssistantText_EarlierTextOutsideSmallTail_WidenedSearchFindsIt(t *testing.T) {
	// F2's actual root cause for "the busiest loops": a large tool_use/
	// tool_result payload can push the real report outside the cheap
	// 24KB tail entirely, not just past the very last message.
	path := writeJSONL(t,
		`{"type":"assistant","message":{"content":"did the real work, evidence attached"}}`,
		fmt.Sprintf(`{"type":"user","message":{"content":[{"type":"tool_result","content":"%s"}]}}`, bigToolResultFiller()),
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}`,
	)

	got, ok := LastAssistantText(path)
	if !ok || got != "did the real work, evidence attached" {
		t.Errorf("got (%q, %v), want (%q, true) — the widened search must find text outside the small tail", got, ok, "did the real work, evidence attached")
	}
}

func TestLastAssistantTextFull_EarlierTextOutsideSmallTail_WidenedSearchFindsIt(t *testing.T) {
	// the oracle's judged report (internal/oracle.Judge, via
	// LastAssistantTextFull) shares the exact same root cause and fix.
	path := writeJSONL(t,
		`{"type":"assistant","message":{"content":"did the real work, evidence attached"}}`,
		fmt.Sprintf(`{"type":"user","message":{"content":[{"type":"tool_result","content":"%s"}]}}`, bigToolResultFiller()),
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}`,
	)

	got, ok := LastAssistantTextFull(path)
	if !ok || got != "did the real work, evidence attached" {
		t.Errorf("got (%q, %v), want (%q, true)", got, ok, "did the real work, evidence attached")
	}
}

func TestLastAssistantText_NoTextEvenInWidenedWindow_ReturnsFalse(t *testing.T) {
	path := writeJSONL(t,
		fmt.Sprintf(`{"type":"user","message":{"content":[{"type":"tool_result","content":"%s"}]}}`, bigToolResultFiller()),
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}`,
	)

	if _, ok := LastAssistantText(path); ok {
		t.Error("expected ok=false — no assistant text anywhere, even in the widened window")
	}
}

func TestLoopFromLog_LastTextOutsideSmallTail_WidenedSearchFillsDoingAndTail(t *testing.T) {
	// end-to-end: the domain.Loop.LastText field (which feeds both the
	// detail pane's TAIL row and the fleet table's DOING column) must not
	// go empty for a busy loop whose last few messages are tool-only.
	path := writeJSONL(t,
		`{"type":"assistant","message":{"content":"did the real work, evidence attached"}}`,
		fmt.Sprintf(`{"type":"user","message":{"content":[{"type":"tool_result","content":"%s"}]}}`, bigToolResultFiller()),
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}`,
	)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat fixture: %v", err)
	}

	l := loopFromLog(path, fi, time.Now(), t.TempDir(), nil)

	if l.LastText != "did the real work, evidence attached" {
		t.Errorf("LastText = %q, want %q (DOING/TAIL must not go empty for a busy loop)", l.LastText, "did the real work, evidence attached")
	}
}

func TestApplyLiveness_OneLiveProcess_NewestKeepsOlderDemoted(t *testing.T) {
	now := time.Now()
	loops := []domain.Loop{
		{SessionID: "newer", ProjectDir: "-x-myproject", Cwd: "/x/myproject", State: domain.StateIdle, LastActivity: now},
		{SessionID: "older-idle", ProjectDir: "-x-myproject", Cwd: "/x/myproject", State: domain.StateIdle, LastActivity: now.Add(-time.Hour)},
	}
	live := map[string]int{"/x/myproject": 1}

	out := applyLiveness(loops, live, true, t.TempDir(), time.Now(), ActiveWindow)

	if len(out) != 1 {
		t.Fatalf("got %d loops, want 1 (older idle loop dropped): %+v", len(out), out)
	}
	if out[0].SessionID != "newer" || out[0].State != domain.StateIdle {
		t.Errorf("got %+v, want the newer loop untouched (idle)", out[0])
	}
}

func TestApplyLiveness_OlderStalled_BecomesGone(t *testing.T) {
	now := time.Now()
	loops := []domain.Loop{
		{SessionID: "newer", ProjectDir: "-x-myproject", Cwd: "/x/myproject", State: domain.StateIdle, LastActivity: now},
		{SessionID: "older-stalled", ProjectDir: "-x-myproject", Cwd: "/x/myproject", State: domain.StateStalled, Stall: domain.StallNoOutput, LastActivity: now.Add(-time.Hour)},
	}
	live := map[string]int{"/x/myproject": 1}

	out := applyLiveness(loops, live, true, t.TempDir(), time.Now(), ActiveWindow)

	if len(out) != 2 {
		t.Fatalf("got %d loops, want 2 (stalled loop kept, reclassified): %+v", len(out), out)
	}
	demoted := out[1]
	if demoted.SessionID != "older-stalled" || demoted.State != domain.StateStalled || demoted.Stall != domain.StallGone {
		t.Errorf("got %+v, want {older-stalled, StateStalled, StallGone}", demoted)
	}
}

func TestApplyLiveness_ZeroLiveProcesses_IdleDroppedStalledGone(t *testing.T) {
	now := time.Now()
	loops := []domain.Loop{
		{SessionID: "idle-one", ProjectDir: "-x-dead", Cwd: "/x/dead", State: domain.StateIdle, LastActivity: now},
		{SessionID: "stalled-one", ProjectDir: "-x-dead", Cwd: "/x/dead", State: domain.StateStalled, Stall: domain.StallNoOutput, LastActivity: now.Add(-time.Minute)},
	}
	live := map[string]int{} // no live process at all for this cwd

	out := applyLiveness(loops, live, true, t.TempDir(), time.Now(), ActiveWindow)

	if len(out) != 1 {
		t.Fatalf("got %d loops, want 1 (idle dropped, stalled kept): %+v", len(out), out)
	}
	if out[0].SessionID != "stalled-one" || out[0].Stall != domain.StallGone {
		t.Errorf("got %+v, want {stalled-one, StallGone}", out[0])
	}
}

// ── LoopEngine MVP (feat/engine-cycle): dormancy exception ────────────────

// TestApplyLiveness_DrivenIdle_RecentActivity_HeldDormant_NotDropped is
// the dormancy exception's core case: a Driven loop between cycles has NO
// live process by design (claude -p exits once each cycle finishes) — the
// usual "no process backing this Idle loop → dropped" rule must not apply
// to it. Held as Idle, still present in the fleet, not reclassified.
func TestApplyLiveness_DrivenIdle_RecentActivity_HeldDormant_NotDropped(t *testing.T) {
	loops := []domain.Loop{
		{SessionID: "engine-1", ProjectDir: "-x-dead", Cwd: "/x/dead", State: domain.StateIdle,
			LastActivity: time.Now().Add(-2 * time.Minute), Driven: true},
	}
	live := map[string]int{} // no live process at all for this cwd

	out := applyLiveness(loops, live, true, t.TempDir(), time.Now(), ActiveWindow)

	if len(out) != 1 {
		t.Fatalf("got %d loops, want 1 (dormant, not dropped): %+v", len(out), out)
	}
	if out[0].State != domain.StateIdle {
		t.Errorf("State = %v, want StateIdle (dormant, awaiting the engine's next drive)", out[0].State)
	}
	if out[0].Stall != domain.StallNone {
		t.Errorf("Stall = %v, want StallNone", out[0].Stall)
	}
}

// TestApplyLiveness_DrivenIdle_StaleActivity_BecomesStalledGone_NotDropped:
// past drivenDormantStale with still no live process, a Driven loop is
// presumed genuinely dead — NOT dropped (that would silently disappear a
// truly stuck engine loop), but reclassified StateStalled/StallGone, the
// SAME surfacing a non-driven presumed-dead loop already gets.
func TestApplyLiveness_DrivenIdle_StaleActivity_BecomesStalledGone_NotDropped(t *testing.T) {
	loops := []domain.Loop{
		{SessionID: "engine-1", ProjectDir: "-x-dead", Cwd: "/x/dead", State: domain.StateIdle,
			LastActivity: time.Now().Add(-drivenDormantStale - time.Minute), Driven: true},
	}
	live := map[string]int{}

	out := applyLiveness(loops, live, true, t.TempDir(), time.Now(), ActiveWindow)

	if len(out) != 1 {
		t.Fatalf("got %d loops, want 1 (surfaced, not dropped): %+v", len(out), out)
	}
	if out[0].State != domain.StateStalled || out[0].Stall != domain.StallGone {
		t.Errorf("got State=%v Stall=%v, want StateStalled/StallGone — stale past drivenDormantStale", out[0].State, out[0].Stall)
	}
}

// TestApplyLiveness_DrivenIdle_ExactlyAtStaleThreshold_StillDormant pins
// the boundary: "<= drivenDormantStale" (not "<") is the held-dormant
// condition — exactly at the threshold is still dormant, not yet stale.
func TestApplyLiveness_DrivenIdle_ExactlyAtStaleThreshold_StillDormant(t *testing.T) {
	// a SINGLE now reference for both the fixture's LastActivity and the
	// call below — two separate time.Now() calls would let a few
	// nanoseconds of real test-execution time push now.Sub(LastActivity)
	// microscopically PAST drivenDormantStale, flaking exactly the
	// boundary this test exists to pin.
	now := time.Now()
	loops := []domain.Loop{
		{SessionID: "engine-1", ProjectDir: "-x-dead", Cwd: "/x/dead", State: domain.StateIdle,
			LastActivity: now.Add(-drivenDormantStale), Driven: true},
	}
	live := map[string]int{}

	out := applyLiveness(loops, live, true, t.TempDir(), now, ActiveWindow)

	if len(out) != 1 || out[0].State != domain.StateIdle {
		t.Errorf("got %+v, want StateIdle (still within the dormancy window)", out)
	}
}

// TestApplyLiveness_DrivenLoopKilled_WinsOverDormancy: a human's kill
// decision must win over the dormancy exception exactly like it already
// wins over every other rule in this function (fix/killed-state) — a
// Driven loop is no exception.
func TestApplyLiveness_DrivenLoopKilled_WinsOverDormancy(t *testing.T) {
	historyDir := t.TempDir()
	now := time.Now()
	if err := events.Append(historyDir, events.Event{
		TS: now.Add(-time.Minute).UnixNano(), SessionID: "engine-1",
		Trigger: events.TriggerActuation, Detail: "kill tier1 ok", Actor: events.ActorHuman, Outcome: events.OutcomeOK,
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	loops := []domain.Loop{
		{SessionID: "engine-1", ProjectDir: "-x-dead", Cwd: "/x/dead", State: domain.StateIdle,
			LastActivity: now.Add(-time.Minute), Driven: true},
	}
	live := map[string]int{}

	out := applyLiveness(loops, live, true, historyDir, now, ActiveWindow)

	if len(out) != 1 || out[0].State != domain.StateKilled {
		t.Errorf("got %+v, want StateKilled — a human kill wins over the dormancy exception", out)
	}
}

// TestApplyLiveness_NonDrivenIdle_StillDroppedAsUsual is the coordinator's
// explicit ask: confirm a NON-driven loop's liveness behavior is
// byte-for-byte unchanged by the dormancy exception — same fixture shape
// as the dormancy tests above (Idle, no live process), Driven simply
// false, still dropped exactly like TestApplyLiveness_ZeroLiveProcesses_
// IdleDroppedStalledGone already established.
func TestApplyLiveness_NonDrivenIdle_StillDroppedAsUsual(t *testing.T) {
	loops := []domain.Loop{
		{SessionID: "observed-1", ProjectDir: "-x-dead", Cwd: "/x/dead", State: domain.StateIdle,
			LastActivity: time.Now().Add(-2 * time.Minute), Driven: false},
	}
	live := map[string]int{}

	out := applyLiveness(loops, live, true, t.TempDir(), time.Now(), ActiveWindow)

	if len(out) != 0 {
		t.Errorf("got %d loops, want 0 (dropped — the dormancy exception is Driven-only)", len(out))
	}
}

func TestApplyLiveness_RunningPastLiveCount_BecomesGone(t *testing.T) {
	// a process that just died mid-turn: JSONL still says "running" but
	// there's no live process backing it once past the live count.
	now := time.Now()
	loops := []domain.Loop{
		{SessionID: "newer", ProjectDir: "-x-myproject", Cwd: "/x/myproject", State: domain.StateRunning, LastActivity: now},
		{SessionID: "just-died", ProjectDir: "-x-myproject", Cwd: "/x/myproject", State: domain.StateRunning, LastActivity: now.Add(-time.Second)},
	}
	live := map[string]int{"/x/myproject": 1}

	out := applyLiveness(loops, live, true, t.TempDir(), time.Now(), ActiveWindow)

	if len(out) != 2 {
		t.Fatalf("got %d loops, want 2: %+v", len(out), out)
	}
	if out[1].State != domain.StateStalled || out[1].Stall != domain.StallGone {
		t.Errorf("got %+v, want {StateStalled, StallGone}", out[1])
	}
}

func TestApplyLiveness_EnoughLiveProcesses_Untouched(t *testing.T) {
	now := time.Now()
	loops := []domain.Loop{
		{SessionID: "a", ProjectDir: "-x-myproject", Cwd: "/x/myproject", State: domain.StateStalled, Stall: domain.StallNoOutput, LastActivity: now},
		{SessionID: "b", ProjectDir: "-x-myproject", Cwd: "/x/myproject", State: domain.StateIdle, LastActivity: now.Add(-time.Minute)},
	}
	live := map[string]int{"/x/myproject": 2} // one live process per loop

	out := applyLiveness(loops, live, true, t.TempDir(), time.Now(), ActiveWindow)

	if len(out) != 2 {
		t.Fatalf("got %d loops, want 2 (nothing dropped)", len(out))
	}
	if out[0].Stall != domain.StallNoOutput || out[1].State != domain.StateIdle {
		t.Errorf("got %+v, want both loops untouched", out)
	}
}

func TestApplyLiveness_DifferentCwdsIndependent(t *testing.T) {
	now := time.Now()
	loops := []domain.Loop{
		{SessionID: "a", ProjectDir: "-x-myproject", Cwd: "/x/myproject", State: domain.StateIdle, LastActivity: now},
		{SessionID: "b", ProjectDir: "-x-other", Cwd: "/x/other", State: domain.StateIdle, LastActivity: now},
	}
	live := map[string]int{"/x/myproject": 1, "/x/other": 0}

	out := applyLiveness(loops, live, true, t.TempDir(), time.Now(), ActiveWindow)

	if len(out) != 1 || out[0].SessionID != "a" {
		t.Errorf("got %+v, want only loop \"a\" (its cwd has a live process; \"b\"'s doesn't)", out)
	}
}

func TestApplyLiveness_StateDone_NotDroppedNorDemoted(t *testing.T) {
	now := time.Now()
	loops := []domain.Loop{
		{SessionID: "done-one", ProjectDir: "-x-myproject", Cwd: "/x/myproject", State: domain.StateDone, LastActivity: now},
	}
	live := map[string]int{} // zero live processes for this cwd — would normally drop/demote everything

	out := applyLiveness(loops, live, true, t.TempDir(), time.Now(), ActiveWindow)

	if len(out) != 1 {
		t.Fatalf("got %d loops, want 1 (StateDone must survive, not be dropped)", len(out))
	}
	if out[0].State != domain.StateDone || out[0].Stall != domain.StallNone {
		t.Errorf("got %+v, want State=StateDone Stall=StallNone (untouched by liveness)", out[0])
	}
}

// Stage 1 / defect #1 (design-loop-state-model.md §4): drift is NOT final.
// It means "the oracle rejected this, re-drive it" — a claim about a loop
// that is still supposed to be workable. A dead process can't be re-driven
// in place, so the observed death outranks the interpretation. This test
// replaces an earlier one that asserted the opposite (the exemption arm
// used to read `case StateDone, StateDrift:`), which is the bug: tonight's
// incident screen showed ✗ DRIFT for a loop dead 40 minutes.
func TestApplyLiveness_StateDriftAndProcessGone_BecomesGone(t *testing.T) {
	now := time.Now()
	loops := []domain.Loop{
		{SessionID: "drift-one", ProjectDir: "-x-myproject", Cwd: "/x/myproject", State: domain.StateDrift, LastActivity: now},
	}
	live := map[string]int{} // no live claude in this dir

	out := applyLiveness(loops, live, true, t.TempDir(), time.Now(), ActiveWindow)

	if len(out) != 1 {
		t.Fatalf("got %d loops, want 1 (a gone drift loop is kept, not dropped — it's an incident)", len(out))
	}
	if out[0].State != domain.StateStalled || out[0].Stall != domain.StallGone {
		t.Errorf("got State=%v Stall=%v, want StateStalled/StallGone — an observed death outranks a non-final oracle interpretation", out[0].State, out[0].Stall)
	}
}

// The other half of the precedence rule: "final beats non-final" — a
// converged loop whose process exited is done, NOT gone. Guards against a
// naive "OS facts always win" reading of the fix above.
func TestApplyLiveness_StateDoneAndProcessGone_StaysDone(t *testing.T) {
	now := time.Now()
	loops := []domain.Loop{
		{SessionID: "done-one", ProjectDir: "-x-myproject", Cwd: "/x/myproject", State: domain.StateDone, LastActivity: now},
	}

	out := applyLiveness(loops, map[string]int{}, true, t.TempDir(), time.Now(), ActiveWindow)

	if len(out) != 1 {
		t.Fatalf("got %d loops, want 1", len(out))
	}
	if out[0].State != domain.StateDone {
		t.Errorf("State = %v, want StateDone — a converged loop's process exiting is the expected epilogue, not news", out[0].State)
	}
	if out[0].Stall != domain.StallNone {
		t.Errorf("Stall = %v, want StallNone (untouched)", out[0].Stall)
	}
}

// The third final state, and the one that was actually broken. Unlike
// StateKilled, StateFailed genuinely IS an input to applyLiveness:
// applyGovernor sets it inside enrichFromRegistry, which DiscoverLoops runs
// one pass earlier. So before this arm existed, a governor-stopped loop
// whose process then exited was demoted FAILED → STALLED/gone, losing the
// governor's conclusion while keeping its note. A pre-existing violation of
// the ladder's rung 1, surfaced (not caused) by Stage 1.
func TestApplyLiveness_StateFailedAndProcessGone_StaysFailed(t *testing.T) {
	now := time.Now()
	loops := []domain.Loop{
		{SessionID: "failed-one", ProjectDir: "-x-myproject", Cwd: "/x/myproject",
			State: domain.StateFailed, Note: "stopped: no improvement", LastActivity: now},
	}

	out := applyLiveness(loops, map[string]int{}, true, t.TempDir(), time.Now(), ActiveWindow)

	if len(out) != 1 {
		t.Fatalf("got %d loops, want 1", len(out))
	}
	if out[0].State != domain.StateFailed {
		t.Errorf("State = %v, want StateFailed — the governor stopped this deliberately; the process exiting afterwards doesn't make that judgment less true", out[0].State)
	}
	if out[0].Stall != domain.StallNone {
		t.Errorf("Stall = %v, want StallNone (untouched)", out[0].Stall)
	}
}

// The rule the three arms share, asserted as the rule rather than three
// times as instances: every LoopState.Terminal() state survives an observed
// death intact, and every non-terminal one is demoted. This is what keeps
// the code's Terminal() guard and domain's Terminal() definition from
// drifting apart — add a terminal state and this test covers it for free.
func TestApplyLiveness_ProcessGone_TerminalStatesSurviveNonTerminalDemoted(t *testing.T) {
	// Every LoopState except StateIdle, which is excluded on purpose: it
	// has its own older rule (a cleanly-finished loop is DROPPED, not
	// demoted — see TestApplyLiveness_ZeroLiveProcesses_* and the Driven
	// dormancy exception), so it is neither a survivor nor a demotion and
	// would not fit this assertion's shape.
	all := []domain.LoopState{
		domain.StateRunning, domain.StateGate, domain.StateStalled,
		domain.StateDrift, domain.StateDone, domain.StateFailed,
		domain.StatePaused, domain.StateKilled,
	}
	for _, st := range all {
		t.Run(string(st), func(t *testing.T) {
			loops := []domain.Loop{
				{SessionID: "s1", ProjectDir: "-x-myproject", Cwd: "/x/myproject", State: st, LastActivity: time.Now()},
			}

			out := applyLiveness(loops, map[string]int{}, true, t.TempDir(), time.Now(), ActiveWindow)

			if len(out) != 1 {
				t.Fatalf("got %d loops, want 1", len(out))
			}
			if st.Terminal() {
				if out[0].State != st {
					t.Errorf("State = %v, want unchanged %v — final beats non-final", out[0].State, st)
				}
				return
			}
			if out[0].State != domain.StateStalled || out[0].Stall != domain.StallGone {
				t.Errorf("State = %v Stall = %v, want StateStalled/StallGone — among non-final states, observed beats inferred", out[0].State, out[0].Stall)
			}
		})
	}
}

// Same guard for the human's own final word. Note the fixture goes through
// the PRODUCTION route: StateKilled is never an INPUT to applyLiveness
// (nothing upstream produces it — classifyLoop and enrichFromRegistry
// can't), it is derived here from a kill on record. So "killed + dead
// stays killed" is really "the kill-on-record check still outranks the
// liveness demotion", which Stage 1 must not have disturbed: the
// mostRecentActuationIsKill branch runs BEFORE the switch, and drift
// leaving the exemption arm changes nothing about it.
func TestApplyLiveness_KilledOnRecordAndProcessGone_StaysKilledNotGone(t *testing.T) {
	historyDir := t.TempDir()
	now := time.Now()
	if err := events.Append(historyDir, events.Event{
		TS: now.Add(-time.Minute).UnixNano(), SessionID: "killed-one",
		FromState: "drift", ToState: "drift", Trigger: events.TriggerActuation,
		Detail: "kill tier1 ok", Actor: events.ActorHuman, Outcome: events.OutcomeOK,
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	loops := []domain.Loop{
		{SessionID: "killed-one", ProjectDir: "-x-myproject", Cwd: "/x/myproject", State: domain.StateDrift, LastActivity: now},
	}

	out := applyLiveness(loops, map[string]int{}, true, historyDir, now, ActiveWindow)

	if len(out) != 1 {
		t.Fatalf("got %d loops, want 1", len(out))
	}
	if out[0].State != domain.StateKilled || out[0].Stall != domain.StallNone {
		t.Errorf("got State=%v Stall=%v, want StateKilled/StallNone — a human's kill is final and outranks the gone demotion", out[0].State, out[0].Stall)
	}
}

// The failure case that keeps the fix honest: drift with a live process
// backing it is still DRIFT. Liveness only demotes when the process is
// actually observed gone.
func TestApplyLiveness_StateDriftAndProcessAlive_StaysDrift(t *testing.T) {
	now := time.Now()
	loops := []domain.Loop{
		{SessionID: "drift-one", ProjectDir: "-x-myproject", Cwd: "/x/myproject", State: domain.StateDrift, LastActivity: now},
	}
	live := map[string]int{"/x/myproject": 1}

	out := applyLiveness(loops, live, true, t.TempDir(), time.Now(), ActiveWindow)

	if len(out) != 1 {
		t.Fatalf("got %d loops, want 1", len(out))
	}
	if out[0].State != domain.StateDrift || out[0].Stall != domain.StallNone {
		t.Errorf("got State=%v Stall=%v, want StateDrift/StallNone — nothing observed contradicts the verdict", out[0].State, out[0].Stall)
	}
}

// The probe-failed case must not manufacture a death: with ok=false the
// whole fleet is left alone, so drift stays drift even with zero live
// processes reported (scan.go's P1-2 guard).
func TestApplyLiveness_StateDriftAndProbeFailed_StaysDrift(t *testing.T) {
	now := time.Now()
	loops := []domain.Loop{
		{SessionID: "drift-one", ProjectDir: "-x-myproject", Cwd: "/x/myproject", State: domain.StateDrift, LastActivity: now},
	}

	out := applyLiveness(loops, map[string]int{}, false, t.TempDir(), time.Now(), ActiveWindow)

	if len(out) != 1 || out[0].State != domain.StateDrift {
		t.Errorf("got %+v, want the drift loop untouched — a failed probe is not evidence of death", out)
	}
}

// ── fix/killed-state: KILLED derivation ──────────────────────────────────

func TestApplyLiveness_DriftLoopKilledAndGone_BecomesKilled(t *testing.T) {
	// The exact bug this fixes: a human killed a StateDrift loop (k k →
	// /exit sent, process exits) — without this, the settled-verdict
	// exemption meant it stayed ✗ DRIFT forever, since nothing ever
	// re-examined it once "settled".
	historyDir := t.TempDir()
	now := time.Now()
	if err := events.Append(historyDir, events.Event{
		TS: now.Add(-time.Minute).UnixNano(), SessionID: "drift-one",
		FromState: "drift", ToState: "drift", Trigger: events.TriggerActuation,
		Detail: "kill tier1 ok", Actor: events.ActorHuman, Outcome: events.OutcomeOK,
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	loops := []domain.Loop{
		{SessionID: "drift-one", ProjectDir: "-x-myproject", Cwd: "/x/myproject", State: domain.StateDrift, LastActivity: now},
	}
	live := map[string]int{} // process confirmed gone

	out := applyLiveness(loops, live, true, historyDir, now, ActiveWindow)

	if len(out) != 1 {
		t.Fatalf("got %d loops, want 1 (killed loops are shown, not dropped)", len(out))
	}
	if out[0].State != domain.StateKilled {
		t.Errorf("State = %v, want StateKilled", out[0].State)
	}
	if out[0].Stall != domain.StallNone {
		t.Errorf("Stall = %v, want StallNone (KILLED is not a stall)", out[0].Stall)
	}
}

func TestApplyLiveness_IdleLoopKilledAndGone_BecomesKilled_NotDropped(t *testing.T) {
	// Without a kill event, a gone StateIdle loop is DROPPED entirely
	// (existing behavior). With one, it must survive as StateKilled — the
	// human's kill decision should be visible, not silently vanish.
	historyDir := t.TempDir()
	now := time.Now()
	if err := events.Append(historyDir, events.Event{
		TS: now.Add(-time.Minute).UnixNano(), SessionID: "idle-one",
		FromState: "idle", ToState: "idle", Trigger: events.TriggerActuation,
		Detail: "kill tier1 ok", Actor: events.ActorHuman, Outcome: events.OutcomeOK,
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	loops := []domain.Loop{
		{SessionID: "idle-one", ProjectDir: "-x-myproject", Cwd: "/x/myproject", State: domain.StateIdle, LastActivity: now},
	}
	out := applyLiveness(loops, map[string]int{}, true, historyDir, now, ActiveWindow)

	if len(out) != 1 {
		t.Fatalf("got %d loops, want 1 (killed, not dropped)", len(out))
	}
	if out[0].State != domain.StateKilled {
		t.Errorf("State = %v, want StateKilled", out[0].State)
	}
}

func TestApplyLiveness_KillEventButProcessStillAlive_NotKilled(t *testing.T) {
	// The kill keystroke was sent, but the process hasn't exited yet (or
	// never will — e.g. it ignored /exit) — as long as a live process still
	// backs it, liveness must not touch it at all, killed event or not.
	historyDir := t.TempDir()
	now := time.Now()
	if err := events.Append(historyDir, events.Event{
		TS: now.Add(-time.Minute).UnixNano(), SessionID: "drift-one",
		FromState: "drift", ToState: "drift", Trigger: events.TriggerActuation,
		Detail: "kill tier1 ok", Actor: events.ActorHuman, Outcome: events.OutcomeOK,
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	loops := []domain.Loop{
		{SessionID: "drift-one", ProjectDir: "-x-myproject", Cwd: "/x/myproject", State: domain.StateDrift, LastActivity: now},
	}
	live := map[string]int{"/x/myproject": 1} // still alive

	out := applyLiveness(loops, live, true, historyDir, now, ActiveWindow)

	if out[0].State != domain.StateDrift {
		t.Errorf("State = %v, want unchanged StateDrift (process still alive — a pending kill doesn't matter yet)", out[0].State)
	}
}

func TestApplyLiveness_NoKillEvent_GoneStalledLoop_UnaffectedByFix(t *testing.T) {
	// No kill event at all — the pre-existing StallGone demotion for an
	// ordinary (non-exempt) state must still apply exactly as before.
	historyDir := t.TempDir()
	now := time.Now()
	loops := []domain.Loop{
		{SessionID: "running-one", ProjectDir: "-x-myproject", Cwd: "/x/myproject", State: domain.StateRunning, LastActivity: now},
	}
	out := applyLiveness(loops, map[string]int{}, true, historyDir, now, ActiveWindow)

	if out[0].State != domain.StateStalled || out[0].Stall != domain.StallGone {
		t.Errorf("got %+v, want State=StateStalled Stall=StallGone (existing gone behavior, no kill event on record)", out[0])
	}
}

func TestApplyLiveness_KillEventOutsideActiveWindow_Ignored(t *testing.T) {
	// A kill event older than `within` must not resurrect as a KILLED
	// derivation — "consider only events within the active window".
	historyDir := t.TempDir()
	now := time.Now()
	if err := events.Append(historyDir, events.Event{
		TS: now.Add(-48 * time.Hour).UnixNano(), SessionID: "drift-one",
		FromState: "drift", ToState: "drift", Trigger: events.TriggerActuation,
		Detail: "kill tier1 ok", Actor: events.ActorHuman, Outcome: events.OutcomeOK,
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	loops := []domain.Loop{
		{SessionID: "drift-one", ProjectDir: "-x-myproject", Cwd: "/x/myproject", State: domain.StateDrift, LastActivity: now},
	}
	out := applyLiveness(loops, map[string]int{}, true, historyDir, now, 24*time.Hour)

	// Stage 1: the drift loop is now demoted to gone by the liveness pass
	// (the drift exemption is gone — see
	// TestApplyLiveness_StateDriftAndProcessGone_BecomesGone). What this
	// test pins is unchanged: the stale kill event must NOT produce
	// StateKilled.
	if out[0].State == domain.StateKilled {
		t.Errorf("State = %v, want anything but StateKilled (the kill event is outside the active window)", out[0].State)
	}
	if out[0].State != domain.StateStalled || out[0].Stall != domain.StallGone {
		t.Errorf("got State=%v Stall=%v, want StateStalled/StallGone (ordinary process-gone treatment)", out[0].State, out[0].Stall)
	}
}

func TestApplyLiveness_LaterResumeAfterKill_NotTreatedAsKilled(t *testing.T) {
	// A kill followed by a LATER successful actuation (e.g. a race, or a
	// resume that somehow reached a since-revived session) means the kill
	// is stale — mostRecentActuationIsKill must look at the MOST RECENT
	// human actuation, not "was there ever a kill".
	historyDir := t.TempDir()
	now := time.Now()
	if err := events.Append(historyDir, events.Event{
		TS: now.Add(-time.Hour).UnixNano(), SessionID: "drift-one",
		FromState: "drift", ToState: "drift", Trigger: events.TriggerActuation,
		Detail: "kill tier1 ok", Actor: events.ActorHuman, Outcome: events.OutcomeOK,
	}); err != nil {
		t.Fatalf("Append kill: %v", err)
	}
	if err := events.Append(historyDir, events.Event{
		TS: now.Add(-time.Minute).UnixNano(), SessionID: "drift-one",
		FromState: "drift", ToState: "drift", Trigger: events.TriggerActuation,
		Detail: "resume tier2 ok", Actor: events.ActorHuman, Outcome: events.OutcomeOK,
	}); err != nil {
		t.Fatalf("Append resume: %v", err)
	}
	loops := []domain.Loop{
		{SessionID: "drift-one", ProjectDir: "-x-myproject", Cwd: "/x/myproject", State: domain.StateDrift, LastActivity: now},
	}
	out := applyLiveness(loops, map[string]int{}, true, historyDir, now, ActiveWindow)

	// As above: post-Stage-1 the gone drift loop lands on StateStalled/
	// StallGone. The assertion that matters here is that the STALE kill
	// does not win over the later resume.
	if out[0].State == domain.StateKilled {
		t.Errorf("State = %v, want anything but StateKilled (most recent actuation was a resume, not a kill)", out[0].State)
	}
	if out[0].State != domain.StateStalled || out[0].Stall != domain.StallGone {
		t.Errorf("got State=%v Stall=%v, want StateStalled/StallGone (ordinary process-gone treatment)", out[0].State, out[0].Stall)
	}
}

func TestMostRecentActuationIsKill_NoEvents_False(t *testing.T) {
	if mostRecentActuationIsKill(t.TempDir(), "no-such-session", time.Now(), ActiveWindow) {
		t.Error("expected false when there's no history at all")
	}
}

// ── issue #50: a kill that FAILED is not a kill ──────────────────────────

// The defect itself. logActuationEvent writes a failed kill's Detail as
// "kill <tier> failed: <err>", which the old HasPrefix(Detail, "kill ")
// matched — so a kill the human was told had failed still promoted the
// loop to StateKilled once its process was later observed gone.
func TestMostRecentActuationIsKill_FailedKill_False(t *testing.T) {
	historyDir := t.TempDir()
	now := time.Now()
	if err := events.Append(historyDir, events.Event{
		TS: now.UnixNano(), SessionID: "s1", Trigger: events.TriggerActuation,
		Detail: "kill tier1h failed: no such pane", Actor: events.ActorHuman,
		Outcome: events.OutcomeFailed,
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if mostRecentActuationIsKill(historyDir, "s1", now, ActiveWindow) {
		t.Error("expected false — the user was told the kill FAILED; we must not record it as a kill")
	}
}

// The delivery-timeout case, decided explicitly: OutcomeUnknown is neither
// confirmed-sent nor confirmed-failed, and StateKilled is an assertion that
// a human ended this loop — an unconfirmed send does not license it.
func TestMostRecentActuationIsKill_UnknownDeliveryKill_False(t *testing.T) {
	historyDir := t.TempDir()
	now := time.Now()
	if err := events.Append(historyDir, events.Event{
		TS: now.UnixNano(), SessionID: "s1", Trigger: events.TriggerActuation,
		Detail: "kill tier1h failed: send delivery unknown", Actor: events.ActorHuman,
		Outcome: events.OutcomeUnknown,
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if mostRecentActuationIsKill(historyDir, "s1", now, ActiveWindow) {
		t.Error("expected false — delivery UNKNOWN must not be asserted as a landed kill")
	}
}

// Back-compat, stated as a decision rather than discovered later: a kill
// event written before Event.Outcome existed carries "", and is treated as
// "not confirmed" rather than as success.
func TestMostRecentActuationIsKill_LegacyEventWithoutOutcome_False(t *testing.T) {
	historyDir := t.TempDir()
	now := time.Now()
	if err := events.Append(historyDir, events.Event{
		TS: now.UnixNano(), SessionID: "s1", Trigger: events.TriggerActuation,
		Detail: "kill tier1 ok", Actor: events.ActorHuman, // no Outcome — pre-#50 record
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if mostRecentActuationIsKill(historyDir, "s1", now, ActiveWindow) {
		t.Error("expected false — an event with no structured outcome is not a confirmed kill")
	}
}

// The success case, so the filter can't be satisfied by refusing everything.
func TestMostRecentActuationIsKill_SucceededKill_True(t *testing.T) {
	historyDir := t.TempDir()
	now := time.Now()
	if err := events.Append(historyDir, events.Event{
		TS: now.UnixNano(), SessionID: "s1", Trigger: events.TriggerActuation,
		Detail: "kill tier1 ok", Actor: events.ActorHuman, Outcome: events.OutcomeOK,
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if !mostRecentActuationIsKill(historyDir, "s1", now, ActiveWindow) {
		t.Error("expected true — a confirmed-dispatched kill is a kill")
	}
}

// A SUCCEEDED non-kill actuation must not be read as a kill either: the
// outcome filter narrows, it does not replace, the action check.
func TestMostRecentActuationIsKill_SucceededResume_False(t *testing.T) {
	historyDir := t.TempDir()
	now := time.Now()
	if err := events.Append(historyDir, events.Event{
		TS: now.UnixNano(), SessionID: "s1", Trigger: events.TriggerActuation,
		Detail: "resume tier2 ok", Actor: events.ActorHuman, Outcome: events.OutcomeOK,
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if mostRecentActuationIsKill(historyDir, "s1", now, ActiveWindow) {
		t.Error("expected false — a successful resume is not a kill")
	}
}

// End to end through the pass that consumes it: a failed kill leaves the
// gone loop reading STALLED/gone, not KILLED — and therefore still
// killable, which is the user-visible half of #50.
func TestApplyLiveness_FailedKillAndProcessGone_NotKilled(t *testing.T) {
	historyDir := t.TempDir()
	now := time.Now()
	if err := events.Append(historyDir, events.Event{
		TS: now.Add(-time.Minute).UnixNano(), SessionID: "s1",
		Trigger: events.TriggerActuation, Detail: "kill tier1h failed: no such pane",
		Actor: events.ActorHuman, Outcome: events.OutcomeFailed,
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	loops := []domain.Loop{
		{SessionID: "s1", ProjectDir: "-x-myproject", Cwd: "/x/myproject", State: domain.StateRunning, LastActivity: now},
	}

	out := applyLiveness(loops, map[string]int{}, true, historyDir, now, ActiveWindow)

	if len(out) != 1 {
		t.Fatalf("got %d loops, want 1", len(out))
	}
	if out[0].State == domain.StateKilled {
		t.Error("State = killed, but the kill demonstrably did not land — fleetops must not assert a human ended this loop")
	}
	if out[0].State != domain.StateStalled || out[0].Stall != domain.StallGone {
		t.Errorf("got State=%v Stall=%v, want StateStalled/StallGone (ordinary process-gone treatment)", out[0].State, out[0].Stall)
	}
}

func TestMostRecentActuationIsKill_IgnoresNonActuationEvents(t *testing.T) {
	historyDir := t.TempDir()
	now := time.Now()
	if err := events.Append(historyDir, events.Event{
		TS: now.UnixNano(), SessionID: "s1", ToState: "idle",
		Trigger: events.TriggerScan, Actor: events.ActorSystem,
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if mostRecentActuationIsKill(historyDir, "s1", now, ActiveWindow) {
		t.Error("expected false — a scan-trigger event is not a human actuation")
	}
}

func TestApplyLiveness_ProbeFailed_FleetUnchanged(t *testing.T) {
	// ok=false (ps/lsof error or timeout) must NOT be treated as "confirmed
	// zero live processes" — that would wrongly drop/demote the entire
	// fleet on a transient probe hiccup (P1-2).
	now := time.Now()
	loops := []domain.Loop{
		{SessionID: "idle-one", ProjectDir: "-x-myproject", Cwd: "/x/myproject", State: domain.StateIdle, LastActivity: now},
		{SessionID: "stalled-one", ProjectDir: "-x-myproject", Cwd: "/x/myproject", State: domain.StateStalled, Stall: domain.StallNoOutput, LastActivity: now.Add(-time.Minute)},
	}

	out := applyLiveness(loops, map[string]int{}, false, t.TempDir(), time.Now(), ActiveWindow)

	if len(out) != 2 {
		t.Fatalf("got %d loops, want 2 (probe failure must leave the fleet untouched): %+v", len(out), out)
	}
	if out[0].State != domain.StateIdle || out[1].Stall != domain.StallNoOutput {
		t.Errorf("got %+v, want both loops exactly as classified before the probe", out)
	}
}

func TestApplyLiveness_HyphenatedRealDir_MatchesEncodedProjectDir(t *testing.T) {
	// the real directory name itself contains a "-" (e.g. "my-app") — a
	// naive decode-and-compare would be ambiguous about which "-" came from
	// "/" and which was literal. Matching in the encode direction
	// (encodeCwd(realPath) == ProjectDir) is lossless and must succeed here.
	now := time.Now()
	loops := []domain.Loop{
		{SessionID: "s1", ProjectDir: "-Users-x-my-app", Cwd: "/Users/x/my-app", State: domain.StateIdle, LastActivity: now},
	}
	live := map[string]int{"/Users/x/my-app": 1}

	out := applyLiveness(loops, live, true, t.TempDir(), time.Now(), ActiveWindow)

	if len(out) != 1 || out[0].State != domain.StateIdle {
		t.Errorf("got %+v, want the loop kept and untouched (its real process is live)", out)
	}
}

func TestApplyLiveness_DottedRealDir_MatchesEncodedProjectDir(t *testing.T) {
	// verifies the exact example from the hardening spec: both "/" and "."
	// encode to "-".
	now := time.Now()
	loops := []domain.Loop{
		{
			SessionID:    "s1",
			ProjectDir:   "-home-user--someplugin-agent-sessions",
			Cwd:          "/home/user/-someplugin-agent-sessions", // stale lossy decode
			State:        domain.StateIdle,
			LastActivity: now,
		},
	}
	live := map[string]int{"/home/user/.someplugin/agent-sessions": 1}

	out := applyLiveness(loops, live, true, t.TempDir(), time.Now(), ActiveWindow)

	if len(out) != 1 || out[0].State != domain.StateIdle {
		t.Errorf("got %+v, want the loop kept and untouched (its real process is live)", out)
	}
}

func TestApplyLiveness_HealsCwdAndSetsCwdVerified(t *testing.T) {
	now := time.Now()
	loops := []domain.Loop{
		{SessionID: "s1", ProjectDir: "-x-myproject", Cwd: "/x/myproject", State: domain.StateIdle, LastActivity: now, CwdVerified: false},
	}
	live := map[string]int{"/x/myproject": 1}

	out := applyLiveness(loops, live, true, t.TempDir(), time.Now(), ActiveWindow)

	if out[0].Cwd != "/x/myproject" || !out[0].CwdVerified {
		t.Errorf("got Cwd=%q CwdVerified=%v, want the real lsof path and CwdVerified=true", out[0].Cwd, out[0].CwdVerified)
	}
}

func TestApplyLiveness_HealsCwdEvenWhileDemotedToGone(t *testing.T) {
	// the directory itself is confirmed real by SOME live process there,
	// independent of whether THIS specific loop's own process is the live
	// one — a demoted/stale loop still gets its Cwd healed.
	now := time.Now()
	loops := []domain.Loop{
		{SessionID: "newer", ProjectDir: "-x-myproject", Cwd: "/x/myproject", State: domain.StateRunning, LastActivity: now},
		{SessionID: "just-died", ProjectDir: "-x-myproject", Cwd: "/x/myproject", State: domain.StateRunning, LastActivity: now.Add(-time.Second), CwdVerified: false},
	}
	live := map[string]int{"/x/myproject": 1}

	out := applyLiveness(loops, live, true, t.TempDir(), time.Now(), ActiveWindow)

	demoted := out[1]
	if demoted.State != domain.StateStalled || demoted.Stall != domain.StallGone {
		t.Fatalf("got %+v, want demoted to StateStalled/StallGone", demoted)
	}
	if demoted.Cwd != "/x/myproject" || !demoted.CwdVerified {
		t.Errorf("got Cwd=%q CwdVerified=%v, want healed even though this loop was demoted", demoted.Cwd, demoted.CwdVerified)
	}
}

func TestApplyLiveness_EncodeCwdCollision_DoesNotHealCwd(t *testing.T) {
	// /x/foo-bar and /x/foo.bar BOTH encode to "-x-foo-bar" (encodeCwd
	// collapses both "/" and "." to "-") — two live claudes at those
	// distinct real paths means it's genuinely ambiguous which one a loop
	// with ProjectDir "-x-foo-bar" actually lives in. Healing must refuse
	// rather than silently pick (and potentially heal to) the wrong one.
	//
	// State is StateRunning, not StateIdle: fix/exit-gate-ux (architecture
	// judge item D) made the count-based guard ALSO distrust a collided
	// ProjectDir's live count (see TestApplyLiveness_
	// EncodeCwdCollision_CountNotTrusted_AllScrutinized), which would drop
	// an Idle loop here entirely (out would be empty) — a side effect this
	// test isn't about. StateRunning demotes to StallGone but stays in
	// `out`, keeping this test focused purely on the healing guard.
	now := time.Now()
	loops := []domain.Loop{
		{SessionID: "s1", ProjectDir: "-x-foo-bar", Cwd: "/x/foo-bar", State: domain.StateRunning, LastActivity: now, CwdVerified: false},
	}
	live := map[string]int{
		"/x/foo-bar": 1,
		"/x/foo.bar": 1,
	}

	out := applyLiveness(loops, live, true, t.TempDir(), time.Now(), ActiveWindow)

	if len(out) != 1 {
		t.Fatalf("got %d loops, want 1 (StateRunning must not be dropped, only demoted)", len(out))
	}
	if out[0].CwdVerified {
		t.Errorf("CwdVerified = true, want false — the ProjectDir is ambiguous between two distinct real paths")
	}
	if out[0].Cwd != "/x/foo-bar" {
		t.Errorf("Cwd = %q, want the original lossy decode left untouched (%q)", out[0].Cwd, "/x/foo-bar")
	}
}

// TestApplyLiveness_EncodeCwdCollision_CountNotTrusted_AllScrutinized is
// the architecture judge's item D: the SAME /x/foo-bar vs /x/foo.bar
// collision as the healing test above, but checking the OTHER guard the
// count-based drop/demote decision must ALSO respect. Two independent live
// processes (one per colliding real dir) sum to a live count of 2 — under
// the OLD code, "enough" to exempt BOTH loops sharing this ProjectDir from
// any scrutiny at all, even though we have NO idea which real directory's
// process backs which loop entry. A loop that's actually dead must not
// escape StallGone just because an UNRELATED colliding directory happens
// to have its own live process counted into the same sum.
func TestApplyLiveness_EncodeCwdCollision_CountNotTrusted_AllScrutinized(t *testing.T) {
	now := time.Now()
	loops := []domain.Loop{
		{SessionID: "s1", ProjectDir: "-x-foo-bar", State: domain.StateRunning, LastActivity: now},
		{SessionID: "s2", ProjectDir: "-x-foo-bar", State: domain.StateRunning, LastActivity: now.Add(-time.Minute)},
	}
	live := map[string]int{
		"/x/foo-bar": 1,
		"/x/foo.bar": 1,
	}

	out := applyLiveness(loops, live, true, t.TempDir(), time.Now(), ActiveWindow)

	for _, l := range out {
		if l.State != domain.StateStalled || l.Stall != domain.StallGone {
			t.Errorf("session %s: State=%v Stall=%v, want StateStalled/StallGone — an ambiguous collided live count must not exempt ANY loop from scrutiny", l.SessionID, l.State, l.Stall)
		}
	}
}

func TestEnrichFromRegistry_UnboundLoopUntouched(t *testing.T) {
	loops := []domain.Loop{{SessionID: "unbound-1", State: domain.StateIdle}}

	out := enrichFromRegistry(loops, t.TempDir(), t.TempDir())

	if out[0].Goal.Text != "" {
		t.Errorf("Goal.Text = %q, want empty (no registry record)", out[0].Goal.Text)
	}
	if out[0].State != domain.StateIdle {
		t.Errorf("State = %v, want unchanged StateIdle", out[0].State)
	}
	if out[0].Driven {
		t.Error("Driven = true, want false — an unbound loop was never handed to the engine")
	}
}

// TestEnrichFromRegistry_BoundAndDriven_SurfacesDrivenOnLoop is the
// requirement: a bound+driven registry record must surface as
// loop.Driven==true — the projection enrichFromRegistry already
// does for BoundAt, extended to LoopEngine's ownership flag.
func TestEnrichFromRegistry_BoundAndDriven_SurfacesDrivenOnLoop(t *testing.T) {
	dir := t.TempDir()
	if err := registry.Bind(dir, "sess-1", registry.BindSpec{Goal: "fix the flaky test", Driven: true}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	loops := []domain.Loop{{SessionID: "sess-1", State: domain.StateIdle}}
	out := enrichFromRegistry(loops, dir, t.TempDir())

	if !out[0].Driven {
		t.Error("Driven = false, want true — a driven registry record must surface onto the loop")
	}
}

// TestEnrichFromRegistry_BoundNotDriven_LoopStaysNotDriven: the opt-in-
// spike's off-by-default contract, checked at the enrichment boundary too
// — a bound-but-not-driven loop (the common case until a later slice's "n"
// wizard offers the engine-drive choice) must never surface as Driven.
func TestEnrichFromRegistry_BoundNotDriven_LoopStaysNotDriven(t *testing.T) {
	dir := t.TempDir()
	if err := registry.Bind(dir, "sess-1", registry.BindSpec{Goal: "fix the flaky test"}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	loops := []domain.Loop{{SessionID: "sess-1", State: domain.StateIdle}}
	out := enrichFromRegistry(loops, dir, t.TempDir())

	if out[0].Driven {
		t.Error("Driven = true, want false — a bound-but-not-driven record must not surface as Driven")
	}
}

func TestEnrichFromRegistry_BoundLoop_SetsGoalCapsAndNoImprove(t *testing.T) {
	dir := t.TempDir()
	if err := registry.Bind(dir, "sess-1", registry.BindSpec{Goal: "fix the flaky test"}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := registry.SaveVerdict(dir, "sess-1", domain.Verdict{Outcome: domain.OutcomeRejected, Reason: "no evidence"}, 2); err != nil {
		t.Fatalf("SaveVerdict: %v", err)
	}

	loops := []domain.Loop{{SessionID: "sess-1", State: domain.StateStalled, Cycle: 3}}
	out := enrichFromRegistry(loops, dir, t.TempDir())

	if out[0].Goal.Text != "fix the flaky test" {
		t.Errorf("Goal.Text = %q, want %q", out[0].Goal.Text, "fix the flaky test")
	}
	if out[0].Goal.MaxCycles != registry.DefaultMaxCycles {
		t.Errorf("Goal.MaxCycles = %d, want %d", out[0].Goal.MaxCycles, registry.DefaultMaxCycles)
	}
	if out[0].Goal.NoImproveLimit != registry.DefaultNoImproveLimit {
		t.Errorf("Goal.NoImproveLimit = %d, want %d", out[0].Goal.NoImproveLimit, registry.DefaultNoImproveLimit)
	}
	if out[0].NoImprove != 1 {
		t.Errorf("NoImprove = %d, want 1", out[0].NoImprove)
	}
	if out[0].Last == nil || out[0].Last.Outcome != domain.OutcomeRejected {
		t.Errorf("Last = %+v, want a rejected verdict", out[0].Last)
	}
}

func TestEnrichFromRegistry_MapsFullContractOntoGoal(t *testing.T) {
	dir := t.TempDir()
	spec := registry.BindSpec{
		Goal:          "fix the flaky test",
		DoneCondition: "go test ./... passes 5 times in a row",
		Rubric:        "run go test ./... and check for PASS",
		Challenger:    "try to break it with -race",
	}
	if err := registry.Bind(dir, "sess-1", spec); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	loops := []domain.Loop{{SessionID: "sess-1", State: domain.StateRunning}}
	out := enrichFromRegistry(loops, dir, t.TempDir())

	if out[0].Goal.DoneWhen != spec.DoneCondition {
		t.Errorf("Goal.DoneWhen = %q, want %q", out[0].Goal.DoneWhen, spec.DoneCondition)
	}
	if out[0].Goal.Rubric != spec.Rubric {
		t.Errorf("Goal.Rubric = %q, want %q", out[0].Goal.Rubric, spec.Rubric)
	}
	if out[0].Goal.Challenger != spec.Challenger {
		t.Errorf("Goal.Challenger = %q, want %q", out[0].Goal.Challenger, spec.Challenger)
	}
}

func TestEnrichFromRegistry_DoneAtCurrentCycle_PromotesToStateDone(t *testing.T) {
	dir := t.TempDir()
	if err := registry.Bind(dir, "sess-1", registry.BindSpec{Goal: "goal"}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := registry.SaveVerdict(dir, "sess-1", domain.Verdict{Outcome: domain.OutcomeDone, Reason: "tests pass"}, 5); err != nil {
		t.Fatalf("SaveVerdict: %v", err)
	}

	loops := []domain.Loop{{SessionID: "sess-1", State: domain.StateIdle, Cycle: 5}}
	out := enrichFromRegistry(loops, dir, t.TempDir())

	if out[0].State != domain.StateDone {
		t.Errorf("State = %v, want StateDone (verdict.AtCycle == Cycle)", out[0].State)
	}
}

func TestEnrichFromRegistry_RejectedAtCurrentCycle_PromotesToStateDrift(t *testing.T) {
	dir := t.TempDir()
	if err := registry.Bind(dir, "sess-1", registry.BindSpec{Goal: "goal"}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := registry.SaveVerdict(dir, "sess-1", domain.Verdict{Outcome: domain.OutcomeRejected, Reason: "no evidence"}, 5); err != nil {
		t.Fatalf("SaveVerdict: %v", err)
	}

	loops := []domain.Loop{{SessionID: "sess-1", State: domain.StateIdle, Cycle: 5}}
	out := enrichFromRegistry(loops, dir, t.TempDir())

	if out[0].State != domain.StateDrift {
		t.Errorf("State = %v, want StateDrift (verdict.AtCycle == Cycle)", out[0].State)
	}
}

func TestEnrichFromRegistry_ProgressAtCurrentCycle_LeavesStateUnchanged(t *testing.T) {
	dir := t.TempDir()
	if err := registry.Bind(dir, "sess-1", registry.BindSpec{Goal: "goal"}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := registry.SaveVerdict(dir, "sess-1", domain.Verdict{Outcome: domain.OutcomeProgress, Reason: "still working"}, 5); err != nil {
		t.Fatalf("SaveVerdict: %v", err)
	}

	loops := []domain.Loop{{SessionID: "sess-1", State: domain.StateIdle, Cycle: 5}}
	out := enrichFromRegistry(loops, dir, t.TempDir())

	if out[0].State != domain.StateIdle {
		t.Errorf("State = %v, want unchanged StateIdle (progress doesn't override state)", out[0].State)
	}
	if out[0].Last == nil || out[0].Last.Outcome != domain.OutcomeProgress {
		t.Errorf("Last = %+v, want a progress verdict (still shown for the ORACLE column)", out[0].Last)
	}
}

func TestEnrichFromRegistry_VerdictFromEarlierCycle_DoesNotOverrideState(t *testing.T) {
	dir := t.TempDir()
	if err := registry.Bind(dir, "sess-1", registry.BindSpec{Goal: "goal"}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := registry.SaveVerdict(dir, "sess-1", domain.Verdict{Outcome: domain.OutcomeDone, Reason: "tests pass"}, 3); err != nil {
		t.Fatalf("SaveVerdict: %v", err)
	}

	// the loop has since moved to cycle 5 — the cycle-3 "done" verdict is
	// stale and must not resurrect StateDone.
	loops := []domain.Loop{{SessionID: "sess-1", State: domain.StateRunning, Cycle: 5}}
	out := enrichFromRegistry(loops, dir, t.TempDir())

	if out[0].State != domain.StateRunning {
		t.Errorf("State = %v, want unchanged StateRunning (verdict is from an earlier cycle)", out[0].State)
	}
	if out[0].Last == nil || out[0].Last.Outcome != domain.OutcomeDone {
		t.Errorf("Last = %+v, want the stale done verdict still surfaced for the ORACLE column", out[0].Last)
	}
}

func TestEnrichFromRegistry_GateWinsOverDoneVerdict(t *testing.T) {
	// P2-2: a live gate is more urgent and more current than a verdict
	// rendered against this exact cycle — must not be clobbered by DONE/DRIFT.
	dir := t.TempDir()
	if err := registry.Bind(dir, "sess-1", registry.BindSpec{Goal: "goal"}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := registry.SaveVerdict(dir, "sess-1", domain.Verdict{Outcome: domain.OutcomeDone, Reason: "tests pass"}, 5); err != nil {
		t.Fatalf("SaveVerdict: %v", err)
	}

	loops := []domain.Loop{{SessionID: "sess-1", State: domain.StateGate, GatePrompt: "approve?", Cycle: 5}}
	out := enrichFromRegistry(loops, dir, t.TempDir())

	if out[0].State != domain.StateGate {
		t.Errorf("State = %v, want StateGate preserved (gate wins over a same-cycle DONE verdict)", out[0].State)
	}
	if out[0].Last == nil || out[0].Last.Outcome != domain.OutcomeDone {
		t.Errorf("Last = %+v, want the verdict still surfaced for the ORACLE column despite not overriding State", out[0].Last)
	}
}

func TestEnrichFromRegistry_GateWinsOverRejectedVerdict(t *testing.T) {
	dir := t.TempDir()
	if err := registry.Bind(dir, "sess-1", registry.BindSpec{Goal: "goal"}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := registry.SaveVerdict(dir, "sess-1", domain.Verdict{Outcome: domain.OutcomeRejected, Reason: "no evidence"}, 5); err != nil {
		t.Fatalf("SaveVerdict: %v", err)
	}

	loops := []domain.Loop{{SessionID: "sess-1", State: domain.StateGate, GatePrompt: "approve?", Cycle: 5}}
	out := enrichFromRegistry(loops, dir, t.TempDir())

	if out[0].State != domain.StateGate {
		t.Errorf("State = %v, want StateGate preserved (gate wins over a same-cycle DRIFT verdict)", out[0].State)
	}
}

// ── governor runtime enforcement (internal/engine.Check via applyGovernor) ──

func TestEnrichFromRegistry_NoImproveAtLimit_BecomesStateFailedWithNote(t *testing.T) {
	dir := t.TempDir()
	if err := registry.Bind(dir, "sess-1", registry.BindSpec{Goal: "goal", MaxCycles: 12}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	// three rejections in a row push NoImprove to the default NoImproveLimit (3).
	for cycle := 1; cycle <= registry.DefaultNoImproveLimit; cycle++ {
		if err := registry.SaveVerdict(dir, "sess-1", domain.Verdict{Outcome: domain.OutcomeRejected, Reason: "no evidence"}, cycle); err != nil {
			t.Fatalf("SaveVerdict: %v", err)
		}
	}

	loops := []domain.Loop{{SessionID: "sess-1", State: domain.StateRunning, Cycle: 10}}
	out := enrichFromRegistry(loops, dir, t.TempDir())

	if out[0].State != domain.StateFailed {
		t.Errorf("State = %v, want StateFailed once NoImprove reaches the limit", out[0].State)
	}
	want := fmt.Sprintf("stopped: no improvement %d/%d", registry.DefaultNoImproveLimit, registry.DefaultNoImproveLimit)
	if out[0].Note != want {
		t.Errorf("Note = %q, want %q", out[0].Note, want)
	}
}

func TestEnrichFromRegistry_CycleAtMaxCycles_EscalatesWithNote_StateUnchanged(t *testing.T) {
	dir := t.TempDir()
	if err := registry.Bind(dir, "sess-1", registry.BindSpec{Goal: "goal", MaxCycles: 5}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	loops := []domain.Loop{{SessionID: "sess-1", State: domain.StateRunning, Cycle: 5}}
	out := enrichFromRegistry(loops, dir, t.TempDir())

	if out[0].State != domain.StateRunning {
		t.Errorf("State = %v, want unchanged StateRunning (Escalate keeps the current State)", out[0].State)
	}
	if out[0].Note != "⚠ max cycles reached" {
		t.Errorf("Note = %q, want %q", out[0].Note, "⚠ max cycles reached")
	}
}

func TestEnrichFromRegistry_BudgetExhausted_EscalatesWithNote(t *testing.T) {
	dir := t.TempDir()
	if err := registry.Bind(dir, "sess-1", registry.BindSpec{Goal: "goal"}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	loops := []domain.Loop{{
		SessionID:   "sess-1",
		State:       domain.StateIdle,
		Goal:        domain.Goal{BudgetTokens: 1000},
		TokensSpent: 1000,
	}}
	out := enrichFromRegistry(loops, dir, t.TempDir())

	if out[0].Note != "⚠ over budget" {
		t.Errorf("Note = %q, want %q", out[0].Note, "⚠ over budget")
	}
	if out[0].State != domain.StateIdle {
		t.Errorf("State = %v, want unchanged StateIdle", out[0].State)
	}
}

func TestEnrichFromRegistry_UnboundLoop_GovernorNeverRuns(t *testing.T) {
	// no registry record at all — enrichFromRegistry's early continue means
	// applyGovernor never sees this loop, regardless of its counters.
	loops := []domain.Loop{{
		SessionID: "never-bound",
		State:     domain.StateRunning,
		NoImprove: 999,
		Cycle:     999,
	}}
	out := enrichFromRegistry(loops, t.TempDir(), t.TempDir())

	if out[0].State != domain.StateRunning {
		t.Errorf("State = %v, want unchanged StateRunning (unbound loops are untouched)", out[0].State)
	}
	if out[0].Note != "" {
		t.Errorf("Note = %q, want empty (unbound loops are untouched)", out[0].Note)
	}
}

func TestEnrichFromRegistry_GateWins_GovernorSkipped(t *testing.T) {
	// a live gate must win over the governor too — same reasoning as gate
	// winning over a stale verdict (see TestEnrichFromRegistry_GateWinsOverDoneVerdict).
	dir := t.TempDir()
	if err := registry.Bind(dir, "sess-1", registry.BindSpec{Goal: "goal"}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	for cycle := 1; cycle <= 3; cycle++ {
		if err := registry.SaveVerdict(dir, "sess-1", domain.Verdict{Outcome: domain.OutcomeRejected}, cycle); err != nil {
			t.Fatalf("SaveVerdict: %v", err)
		}
	}

	loops := []domain.Loop{{SessionID: "sess-1", State: domain.StateGate, GatePrompt: "approve?"}}
	out := enrichFromRegistry(loops, dir, t.TempDir())

	if out[0].State != domain.StateGate {
		t.Errorf("State = %v, want StateGate preserved (gate wins over the governor's Stop)", out[0].State)
	}
	if out[0].Note != "" {
		t.Errorf("Note = %q, want empty (governor skipped while gated)", out[0].Note)
	}
}

func TestEnrichFromRegistry_AlreadyTerminalState_GovernorSkipped(t *testing.T) {
	// an already-terminal loop (e.g. StateDone from this cycle's verdict)
	// must not be re-decided by the governor even if its counters would
	// otherwise trigger Stop/Escalate.
	dir := t.TempDir()
	if err := registry.Bind(dir, "sess-1", registry.BindSpec{Goal: "goal"}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := registry.SaveVerdict(dir, "sess-1", domain.Verdict{Outcome: domain.OutcomeDone, Reason: "tests pass"}, 5); err != nil {
		t.Fatalf("SaveVerdict: %v", err)
	}

	// force a synthetic NoImprove that would otherwise trigger Stop — done
	// resets NoImprove in practice (SaveVerdict), but the governor's
	// terminal-state guard must hold independent of that.
	loops := []domain.Loop{{SessionID: "sess-1", State: domain.StateIdle, Cycle: 5, NoImprove: 3}}
	out := enrichFromRegistry(loops, dir, t.TempDir())

	if out[0].State != domain.StateDone {
		t.Errorf("State = %v, want StateDone (already terminal, governor must not override it)", out[0].State)
	}
	if out[0].Note != "" {
		t.Errorf("Note = %q, want empty (governor skipped once terminal)", out[0].Note)
	}
}

// ── governor history events (event-log-and-notify) ──────────────────────

func TestApplyGovernor_Stop_RecordsOneGovernorEvent(t *testing.T) {
	registryDir := t.TempDir()
	historyDir := t.TempDir()
	if err := registry.Bind(registryDir, "sess-1", registry.BindSpec{Goal: "goal"}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	for cycle := 1; cycle <= registry.DefaultNoImproveLimit; cycle++ {
		if err := registry.SaveVerdict(registryDir, "sess-1", domain.Verdict{Outcome: domain.OutcomeRejected}, cycle); err != nil {
			t.Fatalf("SaveVerdict: %v", err)
		}
	}

	loops := []domain.Loop{{SessionID: "sess-1", State: domain.StateRunning, Cycle: 10}}
	enrichFromRegistry(loops, registryDir, historyDir)

	got, err := events.ReadAll(historyDir)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	evs := got["sess-1"]
	if len(evs) != 1 {
		t.Fatalf("got %d events, want exactly 1: %#v", len(evs), evs)
	}
	ev := evs[0]
	if ev.Trigger != events.TriggerGovernor {
		t.Errorf("Trigger = %v, want TriggerGovernor", ev.Trigger)
	}
	if ev.FromState != string(domain.StateRunning) || ev.ToState != string(domain.StateFailed) {
		t.Errorf("FromState/ToState = %q/%q, want running/failed", ev.FromState, ev.ToState)
	}
	if ev.Actor != events.ActorSystem {
		t.Errorf("Actor = %v, want ActorSystem", ev.Actor)
	}
	if ev.Detail == "" {
		t.Error("Detail should carry the governor's stop note, got empty")
	}
}

func TestApplyGovernor_Stop_EdgeTriggered_NoDuplicateOnNextScan(t *testing.T) {
	// Once a loop is StateFailed (terminal), the guard clause at the top of
	// applyGovernor makes the Stop branch structurally unreachable on every
	// SUBSEQUENT scan — so calling enrichFromRegistry twice in a row (as a
	// live poll loop would) must record the governor event only ONCE, not
	// once per poll.
	registryDir := t.TempDir()
	historyDir := t.TempDir()
	if err := registry.Bind(registryDir, "sess-1", registry.BindSpec{Goal: "goal"}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	for cycle := 1; cycle <= registry.DefaultNoImproveLimit; cycle++ {
		if err := registry.SaveVerdict(registryDir, "sess-1", domain.Verdict{Outcome: domain.OutcomeRejected}, cycle); err != nil {
			t.Fatalf("SaveVerdict: %v", err)
		}
	}

	loops1 := []domain.Loop{{SessionID: "sess-1", State: domain.StateRunning, Cycle: 10}}
	out1 := enrichFromRegistry(loops1, registryDir, historyDir)

	// simulate the NEXT scan: the loop is now already StateFailed (as
	// out1[0] reflects), same as a real re-scan would observe.
	loops2 := []domain.Loop{out1[0]}
	enrichFromRegistry(loops2, registryDir, historyDir)

	got, err := events.ReadAll(historyDir)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got["sess-1"]) != 1 {
		t.Fatalf("got %d events across two scans, want exactly 1 (edge-triggered)", len(got["sess-1"]))
	}
}

func TestApplyGovernor_Escalate_NoEventRecorded(t *testing.T) {
	// Escalate doesn't change State (see applyGovernor's Escalate case) and
	// would otherwise re-fire every poll for as long as the condition
	// persists — deliberately NOT logged (see applyGovernor's doc).
	registryDir := t.TempDir()
	historyDir := t.TempDir()
	if err := registry.Bind(registryDir, "sess-1", registry.BindSpec{Goal: "goal", MaxCycles: 5}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	loops := []domain.Loop{{SessionID: "sess-1", State: domain.StateRunning, Cycle: 5}}
	enrichFromRegistry(loops, registryDir, historyDir)

	got, err := events.ReadAll(historyDir)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got["sess-1"]) != 0 {
		t.Fatalf("got %d events for an Escalate decision, want 0", len(got["sess-1"]))
	}
}

func TestApplyGovernor_GateOrTerminalSkip_NoEventRecorded(t *testing.T) {
	registryDir := t.TempDir()
	historyDir := t.TempDir()
	if err := registry.Bind(registryDir, "sess-1", registry.BindSpec{Goal: "goal"}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	for cycle := 1; cycle <= registry.DefaultNoImproveLimit; cycle++ {
		if err := registry.SaveVerdict(registryDir, "sess-1", domain.Verdict{Outcome: domain.OutcomeRejected}, cycle); err != nil {
			t.Fatalf("SaveVerdict: %v", err)
		}
	}

	// StateGate short-circuits applyGovernor entirely (see its guard clause).
	loops := []domain.Loop{{SessionID: "sess-1", State: domain.StateGate}}
	enrichFromRegistry(loops, registryDir, historyDir)

	got, err := events.ReadAll(historyDir)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got["sess-1"]) != 0 {
		t.Fatalf("got %d events while gated, want 0 (governor must not run at all)", len(got["sess-1"]))
	}
}

// ── LastError (feat/detail-panel-v2) ─────────────────────────────────────

func TestLastError_ApiErrorInAssistantText(t *testing.T) {
	path := writeJSONL(t,
		`{"type":"user","message":{"role":"user","content":"do the thing"},"timestamp":"2026-01-01T10:00:00Z"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"API Error: 429 Too Many Requests — rate limit exceeded, retry after 30s"}]},"timestamp":"2026-01-01T10:00:05Z"}`,
	)
	text, ts, ok := LastError(path)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if !strings.Contains(text, "429 Too Many Requests") {
		t.Errorf("text = %q, want the verbatim API error text", text)
	}
	if text != "API Error: 429 Too Many Requests — rate limit exceeded, retry after 30s" {
		t.Errorf("text = %q, must be VERBATIM (council hard rule), not paraphrased", text)
	}
	want := time.Date(2026, 1, 1, 10, 0, 5, 0, time.UTC)
	if !ts.Equal(want) {
		t.Errorf("ts = %v, want %v", ts, want)
	}
}

func TestLastError_IsErrorToolResult(t *testing.T) {
	path := writeJSONL(t,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":"command failed: exit status 1: permission denied"}]},"timestamp":"2026-01-01T11:30:00Z"}`,
	)
	text, ts, ok := LastError(path)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if text != "command failed: exit status 1: permission denied" {
		t.Errorf("text = %q, want the verbatim tool_result error content", text)
	}
	want := time.Date(2026, 1, 1, 11, 30, 0, 0, time.UTC)
	if !ts.Equal(want) {
		t.Errorf("ts = %v, want %v", ts, want)
	}
}

func TestLastError_ToolResultNotAnError_Ignored(t *testing.T) {
	path := writeJSONL(t,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","is_error":false,"content":"success"}]}}`,
	)
	if _, _, ok := LastError(path); ok {
		t.Error("expected ok=false — a successful tool_result must not be treated as an error")
	}
}

func TestLastError_NoErrorAtAll_ReturnsFalse(t *testing.T) {
	path := writeJSONL(t,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"all tests pass"}]}}`,
	)
	if _, _, ok := LastError(path); ok {
		t.Error("expected ok=false — no error entry present")
	}
}

// TestLastError_BareStatusCodeMention_NotAnError is fix/last-error-false-
// positive's core regression: live-reproduced against this repo's OWN real
// transcript (~/.claude/projects/.../myproject/*.jsonl), a healthy loop's
// ordinary status-report text mentioning THIS CODEBASE's "429 auto-redrive"
// feature by name got flagged as a "verbatim error" — because the old
// matcher treated a bare "429" substring anywhere in assistant text as an
// error signal. Without the literal "API Error" marker, a mention of "429"
// (a status code number, a feature name, a port, anything) must NEVER
// qualify.
func TestLastError_BareStatusCodeMention_NotAnError(t *testing.T) {
	path := writeJSONL(t,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"landed #24 (429 auto-redrive) — Tier 2 only, opt-in"}]}}`,
	)
	if _, _, ok := LastError(path); ok {
		t.Error("expected ok=false — mentioning '429' as a feature name/status code is not a real error, per fix/last-error-false-positive")
	}
}

// TestLastError_BareRateLimitMention_NotAnError: same class of false
// positive, for the "rate limit" phrase — ordinary conversation ABOUT rate
// limiting (e.g. discussing this feature's design) must not be mistaken
// for an actual rate-limit error.
func TestLastError_BareRateLimitMention_NotAnError(t *testing.T) {
	path := writeJSONL(t,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"the rate limit auto-redrive only fires on a real 429, per design"}]}}`,
	)
	if _, _, ok := LastError(path); ok {
		t.Error("expected ok=false — discussing rate limiting is not a real error, per fix/last-error-false-positive")
	}
}

func TestLastError_ReturnsTheMostRecentOfTwoErrors(t *testing.T) {
	path := writeJSONL(t,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"API Error: 429 first"}]},"timestamp":"2026-01-01T10:00:00Z"}`,
		`{"type":"user","message":{"role":"user","content":"retry"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"API Error: 429 second"}]},"timestamp":"2026-01-01T10:05:00Z"}`,
	)
	text, _, ok := LastError(path)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if text != "API Error: 429 second" {
		t.Errorf("text = %q, want the SECOND (most recent) error", text)
	}
}

func TestLastError_MissingFile_ReturnsFalse(t *testing.T) {
	if _, _, ok := LastError(filepath.Join(t.TempDir(), "does-not-exist.jsonl")); ok {
		t.Error("expected ok=false for a missing file")
	}
}

func TestLastError_MalformedTimestamp_ZeroTime(t *testing.T) {
	path := writeJSONL(t,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"API Error: 429"}]},"timestamp":"not-a-timestamp"}`,
	)
	_, ts, ok := LastError(path)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if !ts.IsZero() {
		t.Errorf("ts = %v, want the zero value for an unparseable timestamp", ts)
	}
}

// TestEnrichFromRegistry_NameCopiedOntoLoop: the wizard's display name
// (registry.Record.Name) must surface as Loop.Name — the FLEET list's
// DisplayLabel reads it from there, same copy-in seam as BoundAt/Driven.
func TestEnrichFromRegistry_NameCopiedOntoLoop(t *testing.T) {
	dir := t.TempDir()
	if err := registry.Bind(dir, "sess-1", registry.BindSpec{Name: "bugfix loop", Goal: "fix the flaky test"}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	loops := []domain.Loop{{SessionID: "sess-1", State: domain.StateIdle}}
	out := enrichFromRegistry(loops, dir, t.TempDir())

	if out[0].Name != "bugfix loop" {
		t.Errorf("Name = %q, want %q (registry display name must surface onto the loop)", out[0].Name, "bugfix loop")
	}
}

// The two passes composed, in DiscoverLoops' own order — the design
// (design-loop-state-model.md §2.1) warned that enrichFromRegistry
// overwrites the scanner's inferred State one pass BEFORE applyLiveness
// runs, and asked whether that destroys the input the drift/gone fix needs.
// It does not: enrich clobbers the ACTIVITY (idle/running), which the fix
// never consults — the demotion keys off the verdict-derived StateDrift
// itself. This reproduces the reported incident end to end: a rejected
// verdict at the current cycle, no live process, screen must read gone.
//
// It also pins the half the fix is careful not to lose: the rejected
// verdict survives on Loop.Last, which is what the ORACLE column renders,
// so "gone" is displayed WITH its drift verdict rather than instead of it.
func TestEnrichThenLiveness_RejectedVerdictAndProcessGone_ShowsGoneKeepingVerdict(t *testing.T) {
	loopsDir := t.TempDir()
	if err := registry.Bind(loopsDir, "sess-1", registry.BindSpec{Goal: "ship it", MaxCycles: 99}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := registry.SaveVerdict(loopsDir, "sess-1", domain.Verdict{Outcome: domain.OutcomeRejected, Reason: "not done"}, 16); err != nil {
		t.Fatalf("SaveVerdict: %v", err)
	}
	now := time.Now()
	historyDir := t.TempDir()

	// the scanner's own inference from the transcript tail
	loops := []domain.Loop{
		{SessionID: "sess-1", ProjectDir: "-x-myproject", Cwd: "/x/myproject", State: domain.StateIdle, Cycle: 16, LastActivity: now.Add(-40 * time.Minute)},
	}

	loops = enrichFromRegistry(loops, loopsDir, historyDir)
	if loops[0].State != domain.StateDrift {
		t.Fatalf("after enrich: State = %v, want StateDrift (fixture no longer reproduces the incident)", loops[0].State)
	}

	out := applyLiveness(loops, map[string]int{}, true, historyDir, now, ActiveWindow)

	if len(out) != 1 {
		t.Fatalf("got %d loops, want 1", len(out))
	}
	if out[0].State != domain.StateStalled || out[0].Stall != domain.StallGone {
		t.Errorf("got State=%v Stall=%v, want StateStalled/StallGone — the incident screen must read gone, not ✗ DRIFT", out[0].State, out[0].Stall)
	}
	if out[0].Last == nil || out[0].Last.Outcome != domain.OutcomeRejected {
		t.Errorf("Last = %+v, want the rejected verdict preserved for the ORACLE column — gone must not erase what it was doing", out[0].Last)
	}
}

// ── A finished turn with background work outstanding is not idle ─────────
//
// Reported live: a session sat on the fleet list as `idle` for minutes while a
// background agent worked. StateIdle asserts "waiting on a human", and that
// was false — the human had nothing to do and the session would wake itself.

func bgLaunch(id string) string {
	return `{"type":"assistant","message":{"stop_reason":"tool_use","content":[{"type":"tool_use","id":"` + id +
		`","name":"Agent","input":{"run_in_background":true,"prompt":"go"}}]}}`
}

func bgNotification(id string) string {
	return `{"type":"user","message":{"content":[{"type":"tool_result","content":"<task-notification><tool-use-id>` +
		id + `</tool-use-id></task-notification>"}]}}`
}

const turnEnded = `{"type":"assistant","message":{"stop_reason":"end_turn","content":[{"type":"text","text":"done"}]}}`

func TestClassifyLoop_TurnEndedButBackgroundWorkOutstanding_NotIdle(t *testing.T) {
	buf := []byte(bgLaunch("toolu_01AAA") + "\n" + turnEnded)

	state, stall := classifyLoop(buf, time.Second)

	if state == domain.StateIdle {
		t.Fatalf("state = idle, but a background agent is still outstanding — idle asserts the human's turn")
	}
	if state != domain.StateRunning || stall != domain.StallNone {
		t.Errorf("state=%v stall=%v, want running/none while the work is recent", state, stall)
	}
}

// The case that motivated this: a background agent that DIES leaves its
// launcher waiting forever. Stalled/no-output is the honest reading, and the
// operator gets a signal instead of a permanent lie.
func TestClassifyLoop_BackgroundWorkOutstandingAndSilentTooLong_Stalled(t *testing.T) {
	buf := []byte(bgLaunch("toolu_01BBB") + "\n" + turnEnded)

	state, stall := classifyLoop(buf, IdleThreshold*2)

	if state != domain.StateStalled || stall != domain.StallNoOutput {
		t.Errorf("state=%v stall=%v, want stalled/no-output — the launch never reported back", state, stall)
	}
}

// Once the completion notification arrives, the session really IS waiting on
// the human again.
func TestClassifyLoop_BackgroundWorkCompleted_IsIdleAgain(t *testing.T) {
	buf := []byte(bgLaunch("toolu_01CCC") + "\n" + bgNotification("toolu_01CCC") + "\n" + turnEnded)

	if state, _ := classifyLoop(buf, time.Second); state != domain.StateIdle {
		t.Errorf("state = %v, want idle once the launch has reported back", state)
	}
}

// Pairing is by id, not by count — two launches and one completion is not
// "all done", which a counting implementation would get wrong.
func TestOutstandingBackgroundWork_PairsByIDNotCount(t *testing.T) {
	buf := []byte(bgLaunch("toolu_01DDD") + "\n" + bgLaunch("toolu_01EEE") + "\n" + bgNotification("toolu_01DDD"))

	if !outstandingBackgroundWork(buf) {
		t.Error("reported no outstanding work, but toolu_01EEE never reported back")
	}
}

// A foreground Agent call does not outlive the turn, so it must not hold the
// session out of idle.
func TestOutstandingBackgroundWork_ForegroundLaunchDoesNotCount(t *testing.T) {
	buf := []byte(`{"type":"assistant","message":{"stop_reason":"tool_use","content":[{"type":"tool_use","id":"toolu_01FFF","name":"Agent","input":{"run_in_background":false}}]}}`)

	if outstandingBackgroundWork(buf) {
		t.Error("a foreground Agent call was treated as outstanding background work")
	}
}

// Graceful degradation: a tail with no launch in it reads as before.
func TestOutstandingBackgroundWork_NoLaunchInTail_False(t *testing.T) {
	if outstandingBackgroundWork([]byte(turnEnded)) {
		t.Error("reported outstanding work with no launch in the tail")
	}
}

// Same tolerance lastTurnEnded has: a tail's first line is usually cut
// mid-record, and that must not fail the whole scan.
func TestOutstandingBackgroundWork_SkipsUnparseableLines(t *testing.T) {
	buf := []byte(`{"type":"assis` + "\n" + bgLaunch("toolu_01GGG"))

	if !outstandingBackgroundWork(buf) {
		t.Error("a truncated leading line swallowed the launch that followed it")
	}
}

func TestPendingAskUserQuestion_Options(t *testing.T) {
	// Options are what let an operator — human or agent — judge a gate WITHOUT
	// attaching, which is the whole point of surfacing them. Each case below is
	// a shape the real transcript can produce; the malformed ones assert the
	// deliberate asymmetry: a bad options array degrades to "no options", it
	// never sinks a question that was otherwise extracted fine.
	line := func(input string) []byte {
		return []byte(`{"type":"assistant","message":{"stop_reason":"tool_use","content":[{"type":"tool_use","name":"AskUserQuestion","input":` + input + `}]}}`)
	}
	cases := []struct {
		name        string
		input       string
		wantOK      bool
		wantOptions []string
	}{
		{
			name:        "labels extracted in order",
			input:       `{"questions":[{"question":"Which?","options":[{"label":"Ship it"},{"label":"Hold"}]}]}`,
			wantOK:      true,
			wantOptions: []string{"Ship it", "Hold"},
		},
		{
			name:        "descriptions are dropped, labels kept",
			input:       `{"questions":[{"question":"Which?","options":[{"label":"Ship it","description":"a long prose rationale that would not fit a cockpit row"}]}]}`,
			wantOK:      true,
			wantOptions: []string{"Ship it"},
		},
		{
			name:  "only the FIRST question's options — GatePrompt shows only the first question",
			input: `{"questions":[{"question":"Which?","options":[{"label":"A"}]},{"question":"And?","options":[{"label":"B"}]}]}`,
			// "B" belongs to a question the operator cannot see; showing it
			// beside question one would misattribute the choice.
			wantOK:      true,
			wantOptions: []string{"A"},
		},
		{
			name:        "no options key: still a gate, just without choices",
			input:       `{"questions":[{"question":"Which?"}]}`,
			wantOK:      true,
			wantOptions: nil,
		},
		{
			name:        "options is not an array",
			input:       `{"questions":[{"question":"Which?","options":"Ship it"}]}`,
			wantOK:      true,
			wantOptions: nil,
		},
		{
			name:        "option entries are not objects",
			input:       `{"questions":[{"question":"Which?","options":["Ship it","Hold"]}]}`,
			wantOK:      true,
			wantOptions: nil,
		},
		{
			name:        "label missing or empty is skipped, siblings survive",
			input:       `{"questions":[{"question":"Which?","options":[{"description":"no label"},{"label":""},{"label":"Hold"}]}]}`,
			wantOK:      true,
			wantOptions: []string{"Hold"},
		},
		{
			name:        "label is not a string",
			input:       `{"questions":[{"question":"Which?","options":[{"label":42},{"label":"Hold"}]}]}`,
			wantOK:      true,
			wantOptions: []string{"Hold"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, options, ok := pendingAskUserQuestion(line(c.input))
			if ok != c.wantOK {
				t.Fatalf("ok=%v, want %v", ok, c.wantOK)
			}
			if len(options) != len(c.wantOptions) {
				t.Fatalf("options=%q, want %q", options, c.wantOptions)
			}
			for i := range options {
				if options[i] != c.wantOptions[i] {
					t.Errorf("options[%d]=%q, want %q", i, options[i], c.wantOptions[i])
				}
			}
		})
	}
}

func TestPendingAskUserQuestion_LongOptionCapped(t *testing.T) {
	// Several options share one callout line, so a single pathological label
	// must not push the others off screen. Bounded by gateOptionCap, which is
	// deliberately tighter than the cap on the question itself.
	long := "choose\nthis\none " + strings.Repeat("字", gateOptionCap+50)
	input := fmt.Sprintf(`{"questions":[{"question":"Which?","options":[{"label":%q}]}]}`, long)
	line := []byte(`{"type":"assistant","message":{"stop_reason":"tool_use","content":[{"type":"tool_use","name":"AskUserQuestion","input":` + input + `}]}}`)

	_, options, ok := pendingAskUserQuestion(line)
	if !ok || len(options) != 1 {
		t.Fatalf("ok=%v options=%q, want ok=true with one option", ok, options)
	}
	if strings.Contains(options[0], "\n") {
		t.Error("expected newlines collapsed in the option label")
	}
	if n := utf8.RuneCountInString(options[0]); n > gateOptionCap+1 { // +1 for the ellipsis rune
		t.Errorf("option rune length = %d, want <= %d (bounded by gateOptionCap)", n, gateOptionCap+1)
	}
}

// TestLoopFromLog_PermissionRequestOnlyMarker_IsAGate covers the window
// between the two hooks that write one gate's marker. Measured 2026-07-20:
// PermissionRequest lands first with tool_name/tool_input, and the generic
// Notification follows 6.01s later.
//
// A PermissionRequest payload carries NO notification_type and NO message, so
// during those six seconds the marker satisfies neither of the original gate
// tests. Without the tool check the loop would be judged a non-gate and the
// marker compare-and-swap DELETED — then the late generic notification would
// land and the tool name would be gone permanently. The visible symptom would
// be "the feature works sometimes", which is the worst kind to diagnose.
func TestLoopFromLog_PermissionRequestOnlyMarker_IsAGate(t *testing.T) {
	path := writeJSONL(t,
		`{"type":"user","message":{"content":"push the branch"}}`,
		`{"type":"assistant","message":{"content":"ok","stop_reason":"end_turn"}}`,
	)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat fixture: %v", err)
	}
	session := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	gatesDir := t.TempDir()

	// Exactly what cmd/fleetops's permission hook writes: no Type, no Message.
	if err := gate.WriteMarker(gatesDir, session, gate.Info{
		PromptID: "77e62224-b63c-4744-ae73-38eb3764e406",
		Tool:     "Bash", ToolDetail: "git push origin main",
	}); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
	pending := gate.Pending(gatesDir)

	l := loopFromLog(path, fi, fi.ModTime(), gatesDir, pending)

	if l.State != domain.StateGate {
		t.Fatalf("got State=%v, want %v — a tool-bearing marker is a permission gate by construction", l.State, domain.StateGate)
	}
	if want := "Bash: git push origin main"; l.GatePrompt != want {
		t.Errorf("GatePrompt = %q, want %q", l.GatePrompt, want)
	}
	if len(gate.Pending(gatesDir)) == 0 {
		t.Error("the marker was deleted — the detail is now unrecoverable, and the late generic notification will be all that is left")
	}
	if l.GateTS == 0 {
		t.Error("GateTS not set — approve's compare-and-swap has no token to delete this marker with")
	}
}
