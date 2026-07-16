package claude

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jitokim/missionctl/internal/domain"
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

func TestLastAssistantText_CollapsesNewlinesAndCaps120(t *testing.T) {
	long := strings.Repeat("a", 200)
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
}

func TestLastAssistantText_MissingFile(t *testing.T) {
	if _, ok := LastAssistantText(filepath.Join(t.TempDir(), "does-not-exist.jsonl")); ok {
		t.Error("expected ok=false for missing file")
	}
}

func TestApplyLiveness_OneLiveProcess_NewestKeepsOlderDemoted(t *testing.T) {
	now := time.Now()
	loops := []domain.Loop{
		{SessionID: "newer", ProjectDir: "-x-aboard", Cwd: "/x/aboard", State: domain.StateIdle, LastActivity: now},
		{SessionID: "older-idle", ProjectDir: "-x-aboard", Cwd: "/x/aboard", State: domain.StateIdle, LastActivity: now.Add(-time.Hour)},
	}
	live := map[string]int{"/x/aboard": 1}

	out := applyLiveness(loops, live, true)

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

	out := applyLiveness(loops, live, true)

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

	out := applyLiveness(loops, live, true)

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

	out := applyLiveness(loops, live, true)

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

	out := applyLiveness(loops, live, true)

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

	out := applyLiveness(loops, live, true)

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

	out := applyLiveness(loops, live, true)

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

	out := applyLiveness(loops, live, true)

	if len(out) != 1 {
		t.Fatalf("got %d loops, want 1 (StateDrift must survive)", len(out))
	}
	if out[0].State != domain.StateDrift || out[0].Stall != domain.StallNone {
		t.Errorf("got %+v, want State=StateDrift Stall=StallNone (untouched by liveness)", out[0])
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

	out := applyLiveness(loops, map[string]int{}, false)

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

	out := applyLiveness(loops, live, true)

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

	out := applyLiveness(loops, live, true)

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

	out := applyLiveness(loops, live, true)

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

	out := applyLiveness(loops, live, true)

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

	out := applyLiveness(loops, live, true)

	if out[0].CwdVerified {
		t.Errorf("CwdVerified = true, want false — the ProjectDir is ambiguous between two distinct real paths")
	}
	if out[0].Cwd != "/x/foo-bar" {
		t.Errorf("Cwd = %q, want the original lossy decode left untouched (%q)", out[0].Cwd, "/x/foo-bar")
	}
}

func TestEnrichFromRegistry_UnboundLoopUntouched(t *testing.T) {
	loops := []domain.Loop{{SessionID: "unbound-1", State: domain.StateIdle}}

	out := enrichFromRegistry(loops, t.TempDir())

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
	out := enrichFromRegistry(loops, dir)

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
	out := enrichFromRegistry(loops, dir)

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
	out := enrichFromRegistry(loops, dir)

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
	out := enrichFromRegistry(loops, dir)

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
	out := enrichFromRegistry(loops, dir)

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
	out := enrichFromRegistry(loops, dir)

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
	out := enrichFromRegistry(loops, dir)

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
	out := enrichFromRegistry(loops, dir)

	if out[0].State != domain.StateGate {
		t.Errorf("State = %v, want StateGate preserved (gate wins over a same-cycle DRIFT verdict)", out[0].State)
	}
}
