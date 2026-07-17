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
		Cwd:            "/home/user/fleetops",
		TranscriptPath: "/home/user/.claude/projects/foo/abc.jsonl",
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

// TestSessionsDir_Shape sanity-checks the ~/.fleetops/sessions layout under
// an overridden HOME so the real home dir is never touched.
func TestSessionsDir_Shape(t *testing.T) {
	t.Setenv("HOME", "/tmp/fake-home-xyz")
	want := filepath.Join("/tmp/fake-home-xyz", ".fleetops", "sessions")
	if got := SessionsDir(); got != want {
		t.Errorf("SessionsDir() = %q, want %q", got, want)
	}
}

// TestWriteSession_RejectsPathTraversal is the security property: session_id
// arrives from a Claude Code hook payload (external input), and a crafted
// value must not be able to escape SessionsDir via filepath.Join. Proven
// empirically (not just asserted): a "../canary"-style id must NOT create
// any file outside dir.
func TestWriteSession_RejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Dir(dir)
	canary := filepath.Join(parent, "canary-should-not-exist.json")
	defer os.Remove(canary) // best-effort cleanup if the guard ever regresses

	if err := WriteSession(dir, "../canary-should-not-exist", sampleEntry()); err == nil {
		t.Error("WriteSession(\"../canary-should-not-exist\") = nil error, want a rejection")
	}
	if _, err := os.Stat(canary); err == nil {
		t.Fatal("WriteSession escaped dir and wrote outside SessionsDir — path traversal succeeded")
	}
}

// TestReadSession_RejectsPathTraversal mirrors the write-side guard.
func TestReadSession_RejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadSession(dir, "../etc/passwd"); err == nil {
		t.Error("ReadSession(\"../etc/passwd\") = nil error, want a rejection")
	}
}

// TestDeleteSession_RejectsPathTraversal proves DeleteSession can't be used
// as an arbitrary-file-delete primitive via a crafted session_id — it must
// be a tolerant no-op (matching DeleteSession's existing "missing entry"
// tolerance), never attempt the os.Remove at all.
func TestDeleteSession_RejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Dir(dir)
	canary := filepath.Join(parent, "canary-must-survive.json")
	if err := os.WriteFile(canary, []byte("do not delete me"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(canary)

	if err := DeleteSession(dir, "../canary-must-survive"); err != nil {
		t.Errorf("DeleteSession with a traversal id = %v, want nil (tolerant no-op)", err)
	}
	if _, err := os.Stat(canary); err != nil {
		t.Fatal("DeleteSession escaped dir and deleted a file outside SessionsDir")
	}
}

func TestValidSessionID(t *testing.T) {
	cases := []struct {
		id   string
		want bool
	}{
		{"7666c9ac-fc6a-4824-8c33-cb2a7d810f99", true}, // real-shaped UUID
		{"sess-1", true},
		{"", false},
		{"..", true},         // harmless once ".json" is appended ("...json")
		{".", true},          // harmless once ".json" is appended ("..json")
		{"../canary", false}, // the actual escape vector
		{"a/b", false},
		{"a\\b", true}, // NOT a traversal on this tool's target platforms (macOS/Linux):
		// filepath.Base is OS-separator-aware, and "\" is just a regular
		// filename character on both, not a path separator — verified
		// (filepath.Base(`a\b`) == `a\b` here), so this is correctly accepted,
		// not a gap (this tool doesn't target Windows).
		{"/etc/passwd", false},
	}
	for _, c := range cases {
		if got := validSessionID(c.id); got != c.want {
			t.Errorf("validSessionID(%q) = %v, want %v", c.id, got, c.want)
		}
	}
}
