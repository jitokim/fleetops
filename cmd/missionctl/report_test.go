package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/jitokim/missionctl/internal/events"
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

func TestWriteReport_ActuationsGroupedByActor(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	sessionID := "s1"
	evs := []events.Event{
		{TS: now.Add(-3 * time.Hour).UnixNano(), SessionID: sessionID, FromState: "running", ToState: "running", Trigger: events.TriggerActuation, Detail: "resume tier1 ok", Actor: events.ActorHuman},
		{TS: now.Add(-2 * time.Hour).UnixNano(), SessionID: sessionID, FromState: "running", ToState: "running", Trigger: events.TriggerActuation, Detail: "inject tier1 ok", Actor: events.ActorHuman},
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
	if !strings.Contains(buf.String(), "actuations:   2 (human: 2)\n") {
		t.Errorf("out = %q, want 2 actuations grouped under actor human", buf.String())
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
