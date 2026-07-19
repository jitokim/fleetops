package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/jitokim/fleetops/internal/events"
)

func TestWriteReport_EmptyHistory_ReportsZeroLoops(t *testing.T) {
	var buf bytes.Buffer
	if err := writeReport(&buf, t.TempDir(), "24h", 24*time.Hour, time.Now()); err != nil {
		t.Fatalf("writeReport: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "(0 loops)") {
		t.Errorf("out = %q, want it to report 0 loops", out)
	}
	if !strings.Contains(out, "no history recorded") {
		t.Errorf("out = %q, want the empty-window message", out)
	}
}

func TestWriteReport_SingularLoopWording(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	if err := events.Append(dir, events.Event{TS: now.UnixNano(), SessionID: "s1", ToState: "running", Trigger: events.TriggerScan, Actor: events.ActorSystem}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	var buf bytes.Buffer
	if err := writeReport(&buf, dir, "24h", 24*time.Hour, now); err != nil {
		t.Fatalf("writeReport: %v", err)
	}
	if !strings.Contains(buf.String(), "(1 loop)") {
		t.Errorf("out = %q, want singular \"1 loop\" (not \"1 loops\")", buf.String())
	}
}

func TestWriteReport_ExcludesEventsOlderThanSince(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	if err := events.Append(dir, events.Event{TS: now.Add(-30 * time.Hour).UnixNano(), SessionID: "old", FromState: "", ToState: "running", Trigger: events.TriggerActuation, Actor: events.ActorHuman}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := events.Append(dir, events.Event{TS: now.Add(-1 * time.Hour).UnixNano(), SessionID: "recent", FromState: "", ToState: "running", Trigger: events.TriggerActuation, Actor: events.ActorHuman}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	var buf bytes.Buffer
	if err := writeReport(&buf, dir, "24h", 24*time.Hour, now); err != nil {
		t.Fatalf("writeReport: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "old") {
		t.Errorf("out = %q, want the 30h-old session excluded from a 24h window", out)
	}
	if !strings.Contains(out, "recent") {
		t.Errorf("out = %q, want the 1h-old session included", out)
	}
}

func TestWriteReport_TransitionsCountFromStateChangesOnly(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	sessionID := "s1"
	evs := []events.Event{
		{TS: now.Add(-3 * time.Hour).UnixNano(), SessionID: sessionID, FromState: "", ToState: "running", Trigger: events.TriggerActuation, Actor: events.ActorHuman},                         // spawn: a transition
		{TS: now.Add(-2 * time.Hour).UnixNano(), SessionID: sessionID, FromState: "running", ToState: "idle", Trigger: events.TriggerScan, Actor: events.ActorSystem},                         // a transition
		{TS: now.Add(-1 * time.Hour).UnixNano(), SessionID: sessionID, FromState: "idle", ToState: "idle", Trigger: events.TriggerOracle, Detail: "done at cycle 1", Actor: events.ActorAuto}, // NOT a transition (from==to)
	}
	for _, ev := range evs {
		if err := events.Append(dir, ev); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	var buf bytes.Buffer
	if err := writeReport(&buf, dir, "24h", 24*time.Hour, now); err != nil {
		t.Fatalf("writeReport: %v", err)
	}
	if !strings.Contains(buf.String(), "transitions:  2\n") {
		t.Errorf("out = %q, want exactly 2 transitions counted (oracle event excluded)", buf.String())
	}
}

func TestWriteReport_GatesOpenedAndAnswered(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	sessionID := "s1"
	evs := []events.Event{
		{TS: now.Add(-3 * time.Hour).UnixNano(), SessionID: sessionID, FromState: "idle", ToState: "gate", Trigger: events.TriggerScan, Actor: events.ActorSystem},
		{TS: now.Add(-2 * time.Hour).UnixNano(), SessionID: sessionID, FromState: "gate", ToState: "running", Trigger: events.TriggerScan, Actor: events.ActorSystem},
		{TS: now.Add(-1 * time.Hour).UnixNano(), SessionID: sessionID, FromState: "running", ToState: "gate", Trigger: events.TriggerScan, Actor: events.ActorSystem},
	}
	for _, ev := range evs {
		if err := events.Append(dir, ev); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	var buf bytes.Buffer
	if err := writeReport(&buf, dir, "24h", 24*time.Hour, now); err != nil {
		t.Fatalf("writeReport: %v", err)
	}
	if !strings.Contains(buf.String(), "gates:        2 opened, 1 answered\n") {
		t.Errorf("out = %q, want 2 opened, 1 answered", buf.String())
	}
}

func TestWriteReport_LastStateIsMostRecentEventToState(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	sessionID := "s1"
	evs := []events.Event{
		{TS: now.Add(-2 * time.Hour).UnixNano(), SessionID: sessionID, FromState: "", ToState: "running", Trigger: events.TriggerActuation, Actor: events.ActorHuman},
		{TS: now.Add(-1 * time.Hour).UnixNano(), SessionID: sessionID, FromState: "running", ToState: "drift", Trigger: events.TriggerScan, Actor: events.ActorSystem},
	}
	for _, ev := range evs {
		if err := events.Append(dir, ev); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	var buf bytes.Buffer
	if err := writeReport(&buf, dir, "24h", 24*time.Hour, now); err != nil {
		t.Fatalf("writeReport: %v", err)
	}
	if !strings.Contains(buf.String(), "last state:   drift\n") {
		t.Errorf("out = %q, want last state = drift (the most recent event)", buf.String())
	}
}

// TestWriteReport_ActuationsGroupedByOutcome_AllLanded is the happy path:
// every actuation confirmed dispatched (Outcome: events.OutcomeOK) tallies
// under "landed".
func TestWriteReport_ActuationsGroupedByOutcome_AllLanded(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	sessionID := "s1"
	evs := []events.Event{
		{TS: now.Add(-3 * time.Hour).UnixNano(), SessionID: sessionID, FromState: "running", ToState: "running", Trigger: events.TriggerActuation, Detail: "resume tier1 ok", Actor: events.ActorHuman, Outcome: events.OutcomeOK},
		{TS: now.Add(-2 * time.Hour).UnixNano(), SessionID: sessionID, FromState: "running", ToState: "running", Trigger: events.TriggerActuation, Detail: "inject tier1 ok", Actor: events.ActorHuman, Outcome: events.OutcomeOK},
	}
	for _, ev := range evs {
		if err := events.Append(dir, ev); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	var buf bytes.Buffer
	if err := writeReport(&buf, dir, "24h", 24*time.Hour, now); err != nil {
		t.Fatalf("writeReport: %v", err)
	}
	if !strings.Contains(buf.String(), "actuations:   2 (landed: 2)\n") {
		t.Errorf("out = %q, want 2 actuations grouped under outcome landed", buf.String())
	}
}

// TestWriteReport_ActuationsGroupedByOutcome_FailedNotCountedAsLanded is the
// core regression for issue #52: every kill attempt in the session failed
// ("no such pane"), and the report must show that — not the same "2
// actuations" a report of two SUCCESSFUL kills would show.
func TestWriteReport_ActuationsGroupedByOutcome_FailedNotCountedAsLanded(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	sessionID := "s1"
	evs := []events.Event{
		{TS: now.Add(-2 * time.Hour).UnixNano(), SessionID: sessionID, FromState: "running", ToState: "running", Trigger: events.TriggerActuation, Detail: "kill tier1h failed: no such pane", Actor: events.ActorHuman, Outcome: events.OutcomeFailed},
		{TS: now.Add(-1 * time.Hour).UnixNano(), SessionID: sessionID, FromState: "running", ToState: "running", Trigger: events.TriggerActuation, Detail: "kill tier1h failed: no such pane", Actor: events.ActorHuman, Outcome: events.OutcomeFailed},
	}
	for _, ev := range evs {
		if err := events.Append(dir, ev); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	var buf bytes.Buffer
	if err := writeReport(&buf, dir, "24h", 24*time.Hour, now); err != nil {
		t.Fatalf("writeReport: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "actuations:   2 (failed: 2)\n") {
		t.Errorf("out = %q, want 2 actuations grouped under outcome failed, none under landed", out)
	}
	if strings.Contains(out, "landed") {
		t.Errorf("out = %q, two FAILED actuations must never be tallied as landed", out)
	}
}

// TestWriteReport_ActuationsGroupedByOutcome_DeliveryTimeoutStaysUnknown
// guards the field's central rule: a host-send TIMEOUT
// (events.OutcomeUnknown, see internal/control.ErrSendDeliveryUnknown) means
// we never observed whether the action landed. The report must not collapse
// it into either "landed" or "failed" — both would be an over-claim of
// something that was never confirmed.
func TestWriteReport_ActuationsGroupedByOutcome_DeliveryTimeoutStaysUnknown(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	sessionID := "s1"
	if err := events.Append(dir, events.Event{
		TS: now.Add(-1 * time.Hour).UnixNano(), SessionID: sessionID,
		FromState: "running", ToState: "running", Trigger: events.TriggerActuation,
		Detail: "kill tier1h failed: send delivery unknown", Actor: events.ActorHuman,
		Outcome: events.OutcomeUnknown,
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	var buf bytes.Buffer
	if err := writeReport(&buf, dir, "24h", 24*time.Hour, now); err != nil {
		t.Fatalf("writeReport: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "actuations:   1 (delivery unknown: 1)\n") {
		t.Errorf("out = %q, want the timed-out actuation tallied under delivery unknown", out)
	}
	if strings.Contains(out, "landed: 1") || strings.Contains(out, "failed: 1") {
		t.Errorf("out = %q, a delivery-unknown actuation must not be folded into landed or failed", out)
	}
}

// TestWriteReport_ActuationsGroupedByOutcome_LegacyEmptyOutcomeFoldsIntoUnknown
// covers events written before Event.Outcome existed (Outcome == "" on
// disk). Per events.Event's doc, "" means "not confirmed" and must be
// treated the same as an explicit delivery-unknown timeout, never silently
// upgraded to "landed" just because it's the zero value.
func TestWriteReport_ActuationsGroupedByOutcome_LegacyEmptyOutcomeFoldsIntoUnknown(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	sessionID := "s1"
	// No Outcome field set at all — simulates a pre-Outcome-field event.
	if err := events.Append(dir, events.Event{
		TS: now.Add(-1 * time.Hour).UnixNano(), SessionID: sessionID,
		FromState: "running", ToState: "running", Trigger: events.TriggerActuation,
		Detail: "resume tier1 ok", Actor: events.ActorHuman,
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	var buf bytes.Buffer
	if err := writeReport(&buf, dir, "24h", 24*time.Hour, now); err != nil {
		t.Fatalf("writeReport: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "actuations:   1 (delivery unknown: 1)\n") {
		t.Errorf("out = %q, want a legacy event with no Outcome field tallied under delivery unknown", out)
	}
	if strings.Contains(out, "landed: 1") {
		t.Errorf("out = %q, a legacy event with no recorded outcome must not be counted as landed", out)
	}
}

// TestWriteReport_ActuationsGroupedByOutcome_MixedOutcomesAllBucketsDistinct
// exercises all three buckets together in one session, guarding that they
// stay separately countable (not, say, merged into a single "attempted"
// number) — this is the exact "12 actuations (10 landed, 1 failed, 1
// delivery unknown)" shape the issue asked for.
func TestWriteReport_ActuationsGroupedByOutcome_MixedOutcomesAllBucketsDistinct(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	sessionID := "s1"
	evs := []events.Event{
		{TS: now.Add(-3 * time.Hour).UnixNano(), SessionID: sessionID, FromState: "running", ToState: "running", Trigger: events.TriggerActuation, Detail: "resume tier1 ok", Actor: events.ActorHuman, Outcome: events.OutcomeOK},
		{TS: now.Add(-2 * time.Hour).UnixNano(), SessionID: sessionID, FromState: "running", ToState: "running", Trigger: events.TriggerActuation, Detail: "kill tier1h failed: no such pane", Actor: events.ActorHuman, Outcome: events.OutcomeFailed},
		{TS: now.Add(-1 * time.Hour).UnixNano(), SessionID: sessionID, FromState: "running", ToState: "running", Trigger: events.TriggerActuation, Detail: "kill tier1h failed: send delivery unknown", Actor: events.ActorHuman, Outcome: events.OutcomeUnknown},
	}
	for _, ev := range evs {
		if err := events.Append(dir, ev); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	var buf bytes.Buffer
	if err := writeReport(&buf, dir, "24h", 24*time.Hour, now); err != nil {
		t.Fatalf("writeReport: %v", err)
	}
	if !strings.Contains(buf.String(), "actuations:   3 (delivery unknown: 1, failed: 1, landed: 1)\n") {
		t.Errorf("out = %q, want all three outcome buckets reported distinctly", buf.String())
	}
}

func TestActuationOutcomeLabel_OK_ReturnsLanded(t *testing.T) {
	if got := actuationOutcomeLabel(events.OutcomeOK); got != "landed" {
		t.Errorf("got %q, want %q", got, "landed")
	}
}

func TestActuationOutcomeLabel_Failed_ReturnsFailed(t *testing.T) {
	if got := actuationOutcomeLabel(events.OutcomeFailed); got != "failed" {
		t.Errorf("got %q, want %q", got, "failed")
	}
}

func TestActuationOutcomeLabel_Unknown_ReturnsDeliveryUnknown(t *testing.T) {
	if got := actuationOutcomeLabel(events.OutcomeUnknown); got != "delivery unknown" {
		t.Errorf("got %q, want %q", got, "delivery unknown")
	}
}

// TestActuationOutcomeLabel_Empty_ReturnsDeliveryUnknown pins the zero-value
// case explicitly: "" must never fall through to "landed" by accident of
// switch-statement ordering.
func TestActuationOutcomeLabel_Empty_ReturnsDeliveryUnknown(t *testing.T) {
	if got := actuationOutcomeLabel(""); got != "delivery unknown" {
		t.Errorf("got %q, want %q", got, "delivery unknown")
	}
}

// TestActuationOutcomeLabel_UnrecognizedValue_ReturnsDeliveryUnknown covers
// any future/garbled Outcome string that is neither a known constant nor
// "" — the safe default is still "not confirmed," not a guess.
func TestActuationOutcomeLabel_UnrecognizedValue_ReturnsDeliveryUnknown(t *testing.T) {
	if got := actuationOutcomeLabel("bogus"); got != "delivery unknown" {
		t.Errorf("got %q, want %q", got, "delivery unknown")
	}
}

func TestWriteReport_VerdictsGroupedByOutcome(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	sessionID := "s1"
	evs := []events.Event{
		{TS: now.Add(-3 * time.Hour).UnixNano(), SessionID: sessionID, FromState: "idle", ToState: "idle", Trigger: events.TriggerOracle, Detail: "progress at cycle 1", Actor: events.ActorAuto},
		{TS: now.Add(-2 * time.Hour).UnixNano(), SessionID: sessionID, FromState: "idle", ToState: "idle", Trigger: events.TriggerOracle, Detail: "done at cycle 2", Actor: events.ActorAuto},
	}
	for _, ev := range evs {
		if err := events.Append(dir, ev); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	var buf bytes.Buffer
	if err := writeReport(&buf, dir, "24h", 24*time.Hour, now); err != nil {
		t.Fatalf("writeReport: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "done: 1") || !strings.Contains(out, "progress: 1") {
		t.Errorf("out = %q, want both outcome counts broken out", out)
	}
}

func TestWriteReport_MultipleLoops_SortedMostRecentFirst(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	if err := events.Append(dir, events.Event{TS: now.Add(-3 * time.Hour).UnixNano(), SessionID: "older", FromState: "", ToState: "running", Trigger: events.TriggerActuation, Actor: events.ActorHuman}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := events.Append(dir, events.Event{TS: now.Add(-1 * time.Hour).UnixNano(), SessionID: "newer", FromState: "", ToState: "running", Trigger: events.TriggerActuation, Actor: events.ActorHuman}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	var buf bytes.Buffer
	if err := writeReport(&buf, dir, "24h", 24*time.Hour, now); err != nil {
		t.Fatalf("writeReport: %v", err)
	}
	out := buf.String()
	newerIdx := strings.Index(out, "newer")
	olderIdx := strings.Index(out, "older")
	if newerIdx == -1 || olderIdx == -1 {
		t.Fatalf("out = %q, want both sessions present", out)
	}
	if newerIdx > olderIdx {
		t.Errorf("expected \"newer\" (more recent activity) to appear before \"older\", got them at indices %d/%d", newerIdx, olderIdx)
	}
}

func TestWriteReport_FleetTotalsAggregateAcrossLoops(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	if err := events.Append(dir, events.Event{TS: now.Add(-1 * time.Hour).UnixNano(), SessionID: "s1", FromState: "", ToState: "running", Trigger: events.TriggerActuation, Actor: events.ActorHuman}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := events.Append(dir, events.Event{TS: now.Add(-1 * time.Hour).UnixNano(), SessionID: "s2", FromState: "", ToState: "running", Trigger: events.TriggerActuation, Actor: events.ActorHuman}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	var buf bytes.Buffer
	if err := writeReport(&buf, dir, "24h", 24*time.Hour, now); err != nil {
		t.Fatalf("writeReport: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "FLEET TOTALS") {
		t.Fatalf("out = %q, want a FLEET TOTALS section", out)
	}
	if !strings.Contains(out, "loops:        2") {
		t.Errorf("out = %q, want fleet totals to report 2 loops", out)
	}
}

func TestFormatByKey_EmptyMap(t *testing.T) {
	if got := formatByKey(map[string]int{}); got != "0" {
		t.Errorf("got %q, want %q", got, "0")
	}
}

func TestFormatByKey_DeterministicKeyOrder(t *testing.T) {
	m := map[string]int{"zebra": 1, "alpha": 2}
	got := formatByKey(m)
	want := "3 (alpha: 2, zebra: 1)"
	if got != want {
		t.Errorf("got %q, want %q (alphabetical key order)", got, want)
	}
}

func TestVerdictOutcome_StandardDetailFormat(t *testing.T) {
	if got := verdictOutcome("done at cycle 3"); got != "done" {
		t.Errorf("got %q, want %q", got, "done")
	}
}

func TestVerdictOutcome_UnexpectedFormat_ReturnsWholeString(t *testing.T) {
	if got := verdictOutcome("weird"); got != "weird" {
		t.Errorf("got %q, want the whole string when there's no \" at cycle\" marker", got)
	}
}

// TestWriteReport_StallKindEncodedTransition_CountedAsRealTransition is the
// P2 review fix's report-level regression: a no-output → gone incident
// (internal/domain.Loop.StateString's encoding — "stalled:no-output" →
// "stalled:gone", not the old plain "stalled" → "stalled" that made this
// invisible) must be counted as a transition, since FromState != ToState
// once the stall kind is encoded into the persisted state string.
func TestWriteReport_StallKindEncodedTransition_CountedAsRealTransition(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	if err := events.Append(dir, events.Event{
		TS: now.Add(-1 * time.Hour).UnixNano(), SessionID: "s1",
		FromState: "stalled:no-output", ToState: "stalled:gone",
		Trigger: events.TriggerScan, Detail: "gone", Actor: events.ActorSystem,
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	var buf bytes.Buffer
	if err := writeReport(&buf, dir, "24h", 24*time.Hour, now); err != nil {
		t.Fatalf("writeReport: %v", err)
	}
	if !strings.Contains(buf.String(), "transitions:  1\n") {
		t.Errorf("out = %q, want the no-output→gone incident counted as 1 transition", buf.String())
	}
	if !strings.Contains(buf.String(), "last state:   stalled:gone\n") {
		t.Errorf("out = %q, want last state to show the encoded stall kind", buf.String())
	}
}
