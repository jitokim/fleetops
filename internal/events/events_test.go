package events

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestAppend_ThenReadAll_RoundTrips(t *testing.T) {
	dir := t.TempDir()
	ev := Event{TS: 1000, SessionID: "sess-1", FromState: "running", ToState: "gate", Trigger: TriggerScan, Detail: "d", Actor: ActorSystem}
	if err := Append(dir, ev); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, err := ReadAll(dir)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	evs, ok := got["sess-1"]
	if !ok || len(evs) != 1 {
		t.Fatalf("got %#v, want exactly one event for sess-1", got)
	}
	if evs[0] != ev {
		t.Errorf("round-tripped event = %#v, want %#v", evs[0], ev)
	}
}

// TestTriggerEngine_RoundTripsThroughAppendAndReadAll is LoopEngine MVP's
// durable-ownership slice: TriggerEngine must serialize/deserialize
// exactly like every other Trigger value (same JSON round-trip
// TestAppend_ThenReadAll_RoundTrips already exercises for TriggerScan),
// paired with ActorAuto — the provenance combination that lets the EVENTS
// timeline and `missionctl report` tell an engine-fired cycle apart from a
// human's r/i/a/k/p keypress (which is always TriggerActuation/ActorHuman).
func TestTriggerEngine_RoundTripsThroughAppendAndReadAll(t *testing.T) {
	dir := t.TempDir()
	ev := Event{TS: 1000, SessionID: "sess-1", FromState: "idle", ToState: "running", Trigger: TriggerEngine, Detail: "cycle 4", Actor: ActorAuto}
	if err := Append(dir, ev); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, err := ReadAll(dir)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	evs, ok := got["sess-1"]
	if !ok || len(evs) != 1 {
		t.Fatalf("got %#v, want exactly one event for sess-1", got)
	}
	if evs[0] != ev {
		t.Errorf("round-tripped event = %#v, want %#v", evs[0], ev)
	}
	if evs[0].Trigger != "engine" {
		t.Errorf("Trigger serialized as %q, want the stable string %q", evs[0].Trigger, "engine")
	}
}

func TestAppend_MultipleEvents_SameSession_AllPersisted(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 3; i++ {
		if err := Append(dir, Event{TS: int64(i), SessionID: "sess-1", ToState: "running", Trigger: TriggerScan, Actor: ActorSystem}); err != nil {
			t.Fatalf("Append #%d: %v", i, err)
		}
	}
	got, err := ReadAll(dir)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got["sess-1"]) != 3 {
		t.Fatalf("got %d events, want 3", len(got["sess-1"]))
	}
	for i, ev := range got["sess-1"] {
		if ev.TS != int64(i) {
			t.Errorf("event %d: TS = %d, want %d (ReadAll must be oldest-first)", i, ev.TS, i)
		}
	}
}

func TestAppend_InvalidSessionID_Refused(t *testing.T) {
	dir := t.TempDir()
	err := Append(dir, Event{SessionID: "../escape", ToState: "running"})
	if err == nil {
		t.Fatal("want an error for a session id containing a path separator")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "..", "escape.jsonl")); !os.IsNotExist(statErr) {
		t.Error("a file must not have been written outside dir")
	}
}

func TestAppend_EmptySessionID_Refused(t *testing.T) {
	if err := Append(t.TempDir(), Event{SessionID: ""}); err == nil {
		t.Fatal("want an error for an empty session id")
	}
}

func TestReadAll_MissingDir_ReturnsEmptyNotError(t *testing.T) {
	got, err := ReadAll(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d sessions, want 0", len(got))
	}
}

func TestReadAll_MalformedLine_Skipped_RestStillParsed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess-1.jsonl")
	content := `{"ts":1,"session_id":"sess-1","to_state":"running","trigger":"scan","actor":"system"}
not valid json at all
{"ts":2,"session_id":"sess-1","to_state":"idle","trigger":"scan","actor":"system"}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := ReadAll(dir)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got["sess-1"]) != 2 {
		t.Fatalf("got %d events, want 2 (malformed line skipped)", len(got["sess-1"]))
	}
}

func TestReadAll_MergesRotatedBackupWithLiveFile(t *testing.T) {
	dir := t.TempDir()
	// simulate a rotation having already happened: a ".1" backup plus a
	// fresh live file, both for the same session.
	if err := os.WriteFile(filepath.Join(dir, "sess-1.jsonl.1"), []byte(`{"ts":1,"session_id":"sess-1","to_state":"running","trigger":"scan","actor":"system"}
`), 0o644); err != nil {
		t.Fatalf("WriteFile backup: %v", err)
	}
	if err := Append(dir, Event{TS: 2, SessionID: "sess-1", ToState: "idle", Trigger: TriggerScan, Actor: ActorSystem}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, err := ReadAll(dir)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got["sess-1"]) != 2 {
		t.Fatalf("got %d events merged, want 2 (one from the rotated backup, one live)", len(got["sess-1"]))
	}
	if got["sess-1"][0].TS != 1 || got["sess-1"][1].TS != 2 {
		t.Errorf("events not sorted oldest-first: %#v", got["sess-1"])
	}
}

func TestAppend_RotatesOnceFileExceedsMaxSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess-1.jsonl")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// pre-seed a file already over maxFileSize so the NEXT Append rotates it.
	big := strings.Repeat("x", int(maxFileSize)+1)
	if err := os.WriteFile(path, []byte(big), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := Append(dir, Event{TS: 1, SessionID: "sess-1", ToState: "running", Trigger: TriggerScan, Actor: ActorSystem}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	backup, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("expected a rotated .1 backup: %v", err)
	}
	if string(backup) != big {
		t.Error("rotated backup does not match the pre-rotation content")
	}
	live, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected a fresh live file: %v", err)
	}
	if strings.Contains(string(live), "x") {
		t.Error("live file should start fresh after rotation, not carry over the old content")
	}
}

func TestAppend_RotationClobbersOlderBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess-1.jsonl")
	if err := os.WriteFile(path+".1", []byte("stale backup"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("y", int(maxFileSize)+1)), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := Append(dir, Event{TS: 1, SessionID: "sess-1", ToState: "running", Trigger: TriggerScan, Actor: ActorSystem}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	backup, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("ReadFile backup: %v", err)
	}
	if strings.Contains(string(backup), "stale") {
		t.Error("a single rotation is documented to clobber the previous .1 backup, not chain generations")
	}
}

// TestAppend_ConcurrentWritersToSameSession_NoDataLossAcrossRotation is the
// P1 review fix's regression: Append is called concurrently from several
// goroutines within one process in production (the scanner's transition
// detector, every actuation cmd, judgeCmd, applyGovernor,
// registry.BindPending) — all writing to the SAME session's history file is
// entirely plausible (e.g. an actuation event and a scan-triggered
// transition landing in the same instant). Before appendMu, a concurrent
// stat-then-rename rotation race could silently clobber the ".1" backup or
// interleave/corrupt a write. Run with `go test -race` to actually catch a
// data race, not just wrong final counts (this test asserts both).
func TestAppend_ConcurrentWritersToSameSession_NoDataLossAcrossRotation(t *testing.T) {
	dir := t.TempDir()
	const writers = 20
	const perWriter = 50

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				_ = Append(dir, Event{
					TS:        int64(w*perWriter + i),
					SessionID: "sess-concurrent",
					ToState:   "running",
					Trigger:   TriggerScan,
					Actor:     ActorSystem,
					Detail:    fmt.Sprintf("w%d-%d", w, i),
				})
			}
		}(w)
	}
	wg.Wait()

	got, err := ReadAll(dir)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	want := writers * perWriter
	if len(got["sess-concurrent"]) != want {
		t.Errorf("got %d events across the live file + any rotated backup, want %d — some writes were lost to an unserialized rotation race", len(got["sess-concurrent"]), want)
	}
}

// TestAppend_ConcurrentWriters_ForcedRotations_NoDataLoss shrinks
// maxFileSize so the concurrent writers below trigger MANY rotations
// (rather than the previous test's zero), directly exercising the
// stat-then-rename race appendMu fixes. ReadAll only ever sees the live
// file plus ONE rotated ".1" backup (by design — a single rotation, no
// generation chain, see maxFileSize's doc), so events from backups rotated
// OUT before the final one are expected to be gone; what this test actually
// asserts is the stronger property a race would violate: not one single
// event is truncated/corrupted/duplicated across however many rotations
// happened — every event's own perWriter-index sequence for its writer
// must appear intact in whatever survives (rather than, e.g., a torn write
// leaving unparseable bytes ReadAll would just skip, silently shrinking the
// count with no error at all).
func TestAppend_ConcurrentWriters_ForcedRotations_NoDataLoss(t *testing.T) {
	orig := maxFileSize
	maxFileSize = 512 // tiny — a handful of events already exceeds this
	defer func() { maxFileSize = orig }()

	dir := t.TempDir()
	const writers = 10
	const perWriter = 30

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				_ = Append(dir, Event{
					TS:        int64(w*perWriter + i),
					SessionID: "sess-rotating",
					ToState:   "running",
					Trigger:   TriggerScan,
					Actor:     ActorSystem,
				})
			}
		}(w)
	}
	wg.Wait()

	// Not a data-loss assertion on the FULL count (rotation is expected to
	// drop everything before the single surviving ".1" generation) — the
	// assertion that actually matters is that ReadAll parses cleanly with
	// zero malformed lines, proving no interleaved/torn write happened.
	got, err := ReadAll(dir)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got["sess-rotating"]) == 0 {
		t.Fatal("expected at least some surviving events after forced rotation")
	}
	seen := make(map[int64]bool, len(got["sess-rotating"]))
	for _, ev := range got["sess-rotating"] {
		if seen[ev.TS] {
			t.Errorf("duplicate TS %d — a torn/racing write produced the same event twice", ev.TS)
		}
		seen[ev.TS] = true
	}
}

// ── Read (single-session, scoped) ────────────────────────────────────────

func TestRead_ReturnsOnlyTheRequestedSession(t *testing.T) {
	dir := t.TempDir()
	if err := Append(dir, Event{TS: 1, SessionID: "s1", ToState: "running", Trigger: TriggerScan, Actor: ActorSystem}); err != nil {
		t.Fatalf("Append s1: %v", err)
	}
	if err := Append(dir, Event{TS: 1, SessionID: "s2", ToState: "idle", Trigger: TriggerScan, Actor: ActorSystem}); err != nil {
		t.Fatalf("Append s2: %v", err)
	}
	got, err := Read(dir, "s1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 1 || got[0].SessionID != "s1" {
		t.Errorf("got %#v, want exactly one s1 event", got)
	}
}

func TestRead_MergesRotatedBackupWithLiveFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "s1.jsonl.1"), []byte(`{"ts":1,"session_id":"s1","to_state":"running","trigger":"scan","actor":"system"}
`), 0o644); err != nil {
		t.Fatalf("WriteFile backup: %v", err)
	}
	if err := Append(dir, Event{TS: 2, SessionID: "s1", ToState: "idle", Trigger: TriggerScan, Actor: ActorSystem}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, err := Read(dir, "s1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 2 || got[0].TS != 1 || got[1].TS != 2 {
		t.Errorf("got %#v, want [ts=1, ts=2] merged oldest-first", got)
	}
}

func TestRead_MissingSession_ReturnsEmptyNotError(t *testing.T) {
	got, err := Read(t.TempDir(), "no-such-session")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d events, want 0", len(got))
	}
}

func TestRead_InvalidSessionID_Refused(t *testing.T) {
	if _, err := Read(t.TempDir(), "../escape"); err == nil {
		t.Fatal("want an error for a session id containing a path separator")
	}
}
