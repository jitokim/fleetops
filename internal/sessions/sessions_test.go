package sessions

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func sampleEntry() SessionEntry {
	return SessionEntry{
		PID:            4242,
		TTY:            "ttys004",
		Cwd:            "/Users/imac/IdeaProjects/missionctl",
		TranscriptPath: "/Users/imac/.claude/projects/foo/abc.jsonl",
		Source:         "startup",
		StartedAt:      time.Date(2026, 7, 16, 9, 30, 0, 0, time.UTC),
	}
}

func TestWriteReadSession_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	in := sampleEntry()

	if err := WriteSession(dir, "sess-1", in); err != nil {
		t.Fatalf("WriteSession: %v", err)
	}
	got, err := ReadSession(dir, "sess-1")
	if err != nil {
		t.Fatalf("ReadSession: %v", err)
	}
	if got != in {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, in)
	}
}

// TestWriteSession_EmptyTTY confirms a headless session (no controlling tty)
// round-trips as an empty TTY, not an error.
func TestWriteSession_EmptyTTY(t *testing.T) {
	dir := t.TempDir()
	in := sampleEntry()
	in.TTY = ""

	if err := WriteSession(dir, "headless", in); err != nil {
		t.Fatalf("WriteSession: %v", err)
	}
	got, err := ReadSession(dir, "headless")
	if err != nil {
		t.Fatalf("ReadSession: %v", err)
	}
	if got.TTY != "" {
		t.Errorf("TTY = %q, want empty", got.TTY)
	}
}

func TestReadSession_Missing(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadSession(dir, "nope"); err == nil {
		t.Error("ReadSession on missing entry = nil error, want error")
	}
}

func TestReadSession_Corrupt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSession(dir, "bad"); err == nil {
		t.Error("ReadSession on corrupt file = nil error, want error")
	}
}

func TestListSessions_SkipsCorruptAndNonJSON(t *testing.T) {
	dir := t.TempDir()
	if err := WriteSession(dir, "good", sampleEntry()); err != nil {
		t.Fatal(err)
	}
	// a corrupt .json (must be skipped, not fatal) and a non-json file (must
	// be ignored by extension).
	if err := os.WriteFile(filepath.Join(dir, "corrupt.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	// a subdirectory (must be skipped, not descended).
	if err := os.Mkdir(filepath.Join(dir, "sub.json"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := ListSessions(dir)
	if len(got) != 1 {
		t.Fatalf("ListSessions returned %d entries, want 1 (only the good one): %+v", len(got), got)
	}
	if _, ok := got["good"]; !ok {
		t.Errorf("ListSessions missing 'good': %+v", got)
	}
}

func TestListSessions_MissingDir(t *testing.T) {
	got := ListSessions(filepath.Join(t.TempDir(), "does-not-exist"))
	if len(got) != 0 {
		t.Errorf("ListSessions on missing dir = %+v, want empty map (not nil, not error)", got)
	}
}

func TestDeleteSession_Existing(t *testing.T) {
	dir := t.TempDir()
	if err := WriteSession(dir, "sess-1", sampleEntry()); err != nil {
		t.Fatal(err)
	}
	if err := DeleteSession(dir, "sess-1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := ReadSession(dir, "sess-1"); err == nil {
		t.Error("entry still readable after DeleteSession")
	}
}

// TestDeleteSession_NonExistent is the key tolerance property: deleting an
// entry that was never written (SessionEnd firing for a session with no
// SessionStart record, or a double-delete) is a no-op, NOT an error.
func TestDeleteSession_NonExistent(t *testing.T) {
	dir := t.TempDir()
	if err := DeleteSession(dir, "never-existed"); err != nil {
		t.Errorf("DeleteSession on missing entry = %v, want nil (no-op tolerance)", err)
	}
}

func TestDeleteSession_MissingDir(t *testing.T) {
	if err := DeleteSession(filepath.Join(t.TempDir(), "gone"), "x"); err != nil {
		t.Errorf("DeleteSession in missing dir = %v, want nil", err)
	}
}

// TestSessionsDir_Shape sanity-checks the ~/.missionctl/sessions layout under
// an overridden HOME so the real home dir is never touched.
func TestSessionsDir_Shape(t *testing.T) {
	t.Setenv("HOME", "/tmp/fake-home-xyz")
	want := filepath.Join("/tmp/fake-home-xyz", ".missionctl", "sessions")
	if got := SessionsDir(); got != want {
		t.Errorf("SessionsDir() = %q, want %q", got, want)
	}
}
