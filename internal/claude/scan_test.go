package claude

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jitokim/missionctl/internal/domain"
	"github.com/jitokim/missionctl/internal/events"
	"github.com/jitokim/missionctl/internal/gate"
	"github.com/jitokim/missionctl/internal/registry"
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
	path := writeJSONL(t,
		`{"type":"user","message":{"content":"go"}}`,
		`{"type":"assistant","message":{"content":"429 Too Many Requests: rate limit exceeded"}}`,
	)

	state, stall := tailState(path, IdleThreshold)
	if state != domain.StateStalled || stall != domain.StallRateLimit {
		t.Errorf("got (%v, %v), want (%v, %v)", state, stall, domain.StateStalled, domain.StallRateLimit)
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
	if err := gate.WriteMarker(gatesDir, session, "Claude is waiting for your input", "idle_prompt"); err != nil {
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
const askUserQuestionText = "이번 UPSTAGE_MODEL 변경 커밋에 어떤 JIRA 키를 붙일까요?"

const askUserQuestionLine = `{"type":"assistant","message":{"role":"assistant","stop_reason":"tool_use","content":[{"type":"tool_use","name":"AskUserQuestion","input":{"questions":[{"question":"이번 UPSTAGE_MODEL 변경 커밋에 어떤 JIRA 키를 붙일까요?","header":"JIRA 티켓","multiSelect":false,"options":[{"label":"새 JIRA 티켓 발급 (권장)","description":"신규 티켓 생성 후 그 키로 커밋"},{"label":"티켓 없이 진행","description":"컨벤션 예외"}]}]}}]}}`

func TestPendingAskUserQuestion(t *testing.T) {
	const bashToolUseLine = `{"type":"assistant","message":{"role":"assistant","stop_reason":"tool_use","content":[{"type":"tool_use","name":"Bash","input":{"command":"go test ./..."}}]}}`
	// A user entry answering the question (a tool_result) — once this is the
	// last user/assistant entry, the question is no longer pending.
	const answerLine = `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"새 JIRA 티켓 발급 (권장)"}]}}`

	cases := []struct {
		name         string
		lines        []string
		wantOK       bool
		wantQuestion string
	}{
		{
			name:         "pending AskUserQuestion is the last turn",
			lines:        []string{`{"type":"user","message":{"content":"어떤 키?"}}`, askUserQuestionLine},
			wantOK:       true,
			wantQuestion: askUserQuestionText,
		},
		{
			// The exact real-world case that broke the naive "check the literal
			// last line" approach: Claude Code appends non-turn system/attachment
			// entries AFTER a pending question. They must be filtered out.
			name: "trailing system/attachment noise after pending question",
			lines: []string{
				`{"type":"user","message":{"content":"어떤 키?"}}`,
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
			lines:  []string{`{"type":"user","message":{"content":"어떤 키?"}}`, askUserQuestionLine, answerLine},
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
			question, ok := pendingAskUserQuestion(buf)
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
	long := "line one\n" + strings.Repeat("가", tailTextCap+50)
	line := fmt.Sprintf(`{"type":"assistant","message":{"stop_reason":"tool_use","content":[{"type":"tool_use","name":"AskUserQuestion","input":{"questions":[{"question":%q}]}}]}}`, long)

	question, ok := pendingAskUserQuestion([]byte(line))
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
		`{"type":"user","message":{"content":"어떤 키?"}}`,
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
		`{"type":"user","message":{"content":"어떤 키?"}}`,
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
	// Korean is 3 bytes/rune in UTF-8, so a byte-index cut at tailTextCap
	// would land mid-rune for content like this (tailTextCap is not a
	// multiple of 3) — the exact hazard trunc's own doc comment warns about
	// ("byte-index slice can land mid-character... stray '�' glyphs"). This
	// content is real-world plausible: this codebase's own commit history and
	// UI copy are bilingual Korean/English.
	long := strings.Repeat("가", tailTextCap+100) // exceed the cap so truncation runs
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
		{SessionID: "newer", ProjectDir: "-x-aboard", Cwd: "/x/aboard", State: domain.StateIdle, LastActivity: now},
		{SessionID: "older-idle", ProjectDir: "-x-aboard", Cwd: "/x/aboard", State: domain.StateIdle, LastActivity: now.Add(-time.Hour)},
	}
	live := map[string]int{"/x/aboard": 1}

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
		{SessionID: "newer", ProjectDir: "-x-aboard", Cwd: "/x/aboard", State: domain.StateIdle, LastActivity: now},
		{SessionID: "older-stalled", ProjectDir: "-x-aboard", Cwd: "/x/aboard", State: domain.StateStalled, Stall: domain.StallNoOutput, LastActivity: now.Add(-time.Hour)},
	}
	live := map[string]int{"/x/aboard": 1}

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

func TestApplyLiveness_RunningPastLiveCount_BecomesGone(t *testing.T) {
	// a process that just died mid-turn: JSONL still says "running" but
	// there's no live process backing it once past the live count.
	now := time.Now()
	loops := []domain.Loop{
		{SessionID: "newer", ProjectDir: "-x-aboard", Cwd: "/x/aboard", State: domain.StateRunning, LastActivity: now},
		{SessionID: "just-died", ProjectDir: "-x-aboard", Cwd: "/x/aboard", State: domain.StateRunning, LastActivity: now.Add(-time.Second)},
	}
	live := map[string]int{"/x/aboard": 1}

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
		{SessionID: "a", ProjectDir: "-x-aboard", Cwd: "/x/aboard", State: domain.StateStalled, Stall: domain.StallNoOutput, LastActivity: now},
		{SessionID: "b", ProjectDir: "-x-aboard", Cwd: "/x/aboard", State: domain.StateIdle, LastActivity: now.Add(-time.Minute)},
	}
	live := map[string]int{"/x/aboard": 2} // one live process per loop

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
		{SessionID: "a", ProjectDir: "-x-aboard", Cwd: "/x/aboard", State: domain.StateIdle, LastActivity: now},
		{SessionID: "b", ProjectDir: "-x-other", Cwd: "/x/other", State: domain.StateIdle, LastActivity: now},
	}
	live := map[string]int{"/x/aboard": 1, "/x/other": 0}

	out := applyLiveness(loops, live, true, t.TempDir(), time.Now(), ActiveWindow)

	if len(out) != 1 || out[0].SessionID != "a" {
		t.Errorf("got %+v, want only loop \"a\" (its cwd has a live process; \"b\"'s doesn't)", out)
	}
}

func TestApplyLiveness_StateDone_NotDroppedNorDemoted(t *testing.T) {
	now := time.Now()
	loops := []domain.Loop{
		{SessionID: "done-one", ProjectDir: "-x-aboard", Cwd: "/x/aboard", State: domain.StateDone, LastActivity: now},
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

func TestApplyLiveness_StateDrift_NotDroppedNorDemoted(t *testing.T) {
	now := time.Now()
	loops := []domain.Loop{
		{SessionID: "drift-one", ProjectDir: "-x-aboard", Cwd: "/x/aboard", State: domain.StateDrift, LastActivity: now},
	}
	live := map[string]int{}

	out := applyLiveness(loops, live, true, t.TempDir(), time.Now(), ActiveWindow)

	if len(out) != 1 {
		t.Fatalf("got %d loops, want 1 (StateDrift must survive)", len(out))
	}
	if out[0].State != domain.StateDrift || out[0].Stall != domain.StallNone {
		t.Errorf("got %+v, want State=StateDrift Stall=StallNone (untouched by liveness)", out[0])
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
		Detail: "kill tier1 ok", Actor: events.ActorHuman,
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	loops := []domain.Loop{
		{SessionID: "drift-one", ProjectDir: "-x-aboard", Cwd: "/x/aboard", State: domain.StateDrift, LastActivity: now},
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
		Detail: "kill tier1 ok", Actor: events.ActorHuman,
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	loops := []domain.Loop{
		{SessionID: "idle-one", ProjectDir: "-x-aboard", Cwd: "/x/aboard", State: domain.StateIdle, LastActivity: now},
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
		Detail: "kill tier1 ok", Actor: events.ActorHuman,
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	loops := []domain.Loop{
		{SessionID: "drift-one", ProjectDir: "-x-aboard", Cwd: "/x/aboard", State: domain.StateDrift, LastActivity: now},
	}
	live := map[string]int{"/x/aboard": 1} // still alive

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
		{SessionID: "running-one", ProjectDir: "-x-aboard", Cwd: "/x/aboard", State: domain.StateRunning, LastActivity: now},
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
		Detail: "kill tier1 ok", Actor: events.ActorHuman,
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	loops := []domain.Loop{
		{SessionID: "drift-one", ProjectDir: "-x-aboard", Cwd: "/x/aboard", State: domain.StateDrift, LastActivity: now},
	}
	out := applyLiveness(loops, map[string]int{}, true, historyDir, now, 24*time.Hour)

	if out[0].State != domain.StateDrift {
		t.Errorf("State = %v, want unchanged StateDrift (the kill event is outside the active window)", out[0].State)
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
		Detail: "kill tier1 ok", Actor: events.ActorHuman,
	}); err != nil {
		t.Fatalf("Append kill: %v", err)
	}
	if err := events.Append(historyDir, events.Event{
		TS: now.Add(-time.Minute).UnixNano(), SessionID: "drift-one",
		FromState: "drift", ToState: "drift", Trigger: events.TriggerActuation,
		Detail: "resume tier2 ok", Actor: events.ActorHuman,
	}); err != nil {
		t.Fatalf("Append resume: %v", err)
	}
	loops := []domain.Loop{
		{SessionID: "drift-one", ProjectDir: "-x-aboard", Cwd: "/x/aboard", State: domain.StateDrift, LastActivity: now},
	}
	out := applyLiveness(loops, map[string]int{}, true, historyDir, now, ActiveWindow)

	if out[0].State != domain.StateDrift {
		t.Errorf("State = %v, want unchanged StateDrift (most recent actuation was a resume, not a kill)", out[0].State)
	}
}

func TestMostRecentActuationIsKill_NoEvents_False(t *testing.T) {
	if mostRecentActuationIsKill(t.TempDir(), "no-such-session", time.Now(), ActiveWindow) {
		t.Error("expected false when there's no history at all")
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
		{SessionID: "idle-one", ProjectDir: "-x-aboard", Cwd: "/x/aboard", State: domain.StateIdle, LastActivity: now},
		{SessionID: "stalled-one", ProjectDir: "-x-aboard", Cwd: "/x/aboard", State: domain.StateStalled, Stall: domain.StallNoOutput, LastActivity: now.Add(-time.Minute)},
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
			ProjectDir:   "-Users-imac--claude-mem-observer-sessions",
			Cwd:          "/Users/imac/-claude-mem-observer-sessions", // stale lossy decode
			State:        domain.StateIdle,
			LastActivity: now,
		},
	}
	live := map[string]int{"/Users/imac/.claude-mem/observer-sessions": 1}

	out := applyLiveness(loops, live, true, t.TempDir(), time.Now(), ActiveWindow)

	if len(out) != 1 || out[0].State != domain.StateIdle {
		t.Errorf("got %+v, want the loop kept and untouched (its real process is live)", out)
	}
}

func TestApplyLiveness_HealsCwdAndSetsCwdVerified(t *testing.T) {
	now := time.Now()
	loops := []domain.Loop{
		{SessionID: "s1", ProjectDir: "-x-aboard", Cwd: "/x/aboard", State: domain.StateIdle, LastActivity: now, CwdVerified: false},
	}
	live := map[string]int{"/x/aboard": 1}

	out := applyLiveness(loops, live, true, t.TempDir(), time.Now(), ActiveWindow)

	if out[0].Cwd != "/x/aboard" || !out[0].CwdVerified {
		t.Errorf("got Cwd=%q CwdVerified=%v, want the real lsof path and CwdVerified=true", out[0].Cwd, out[0].CwdVerified)
	}
}

func TestApplyLiveness_HealsCwdEvenWhileDemotedToGone(t *testing.T) {
	// the directory itself is confirmed real by SOME live process there,
	// independent of whether THIS specific loop's own process is the live
	// one — a demoted/stale loop still gets its Cwd healed.
	now := time.Now()
	loops := []domain.Loop{
		{SessionID: "newer", ProjectDir: "-x-aboard", Cwd: "/x/aboard", State: domain.StateRunning, LastActivity: now},
		{SessionID: "just-died", ProjectDir: "-x-aboard", Cwd: "/x/aboard", State: domain.StateRunning, LastActivity: now.Add(-time.Second), CwdVerified: false},
	}
	live := map[string]int{"/x/aboard": 1}

	out := applyLiveness(loops, live, true, t.TempDir(), time.Now(), ActiveWindow)

	demoted := out[1]
	if demoted.State != domain.StateStalled || demoted.Stall != domain.StallGone {
		t.Fatalf("got %+v, want demoted to StateStalled/StallGone", demoted)
	}
	if demoted.Cwd != "/x/aboard" || !demoted.CwdVerified {
		t.Errorf("got Cwd=%q CwdVerified=%v, want healed even though this loop was demoted", demoted.Cwd, demoted.CwdVerified)
	}
}

func TestApplyLiveness_EncodeCwdCollision_DoesNotHealCwd(t *testing.T) {
	// /x/foo-bar and /x/foo.bar BOTH encode to "-x-foo-bar" (encodeCwd
	// collapses both "/" and "." to "-") — two live claudes at those
	// distinct real paths means it's genuinely ambiguous which one a loop
	// with ProjectDir "-x-foo-bar" actually lives in. Healing must refuse
	// rather than silently pick (and potentially heal to) the wrong one.
	now := time.Now()
	loops := []domain.Loop{
		{SessionID: "s1", ProjectDir: "-x-foo-bar", Cwd: "/x/foo-bar", State: domain.StateIdle, LastActivity: now, CwdVerified: false},
	}
	live := map[string]int{
		"/x/foo-bar": 1,
		"/x/foo.bar": 1,
	}

	out := applyLiveness(loops, live, true, t.TempDir(), time.Now(), ActiveWindow)

	if out[0].CwdVerified {
		t.Errorf("CwdVerified = true, want false — the ProjectDir is ambiguous between two distinct real paths")
	}
	if out[0].Cwd != "/x/foo-bar" {
		t.Errorf("Cwd = %q, want the original lossy decode left untouched (%q)", out[0].Cwd, "/x/foo-bar")
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
		Oracle:        "run go test ./... and check for PASS",
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
	if out[0].Goal.Oracle != spec.Oracle {
		t.Errorf("Goal.Oracle = %q, want %q", out[0].Goal.Oracle, spec.Oracle)
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
