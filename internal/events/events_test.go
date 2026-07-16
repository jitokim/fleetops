package events

import (
	"os"
	"path/filepath"
	"strings"
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
	big := strings.Repeat("x", maxFileSize+1)
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
	if err := os.WriteFile(path, []byte(strings.Repeat("y", maxFileSize+1)), 0o644); err != nil {
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
