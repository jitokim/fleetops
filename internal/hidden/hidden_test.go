package hidden

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── Load: fail-open on everything unreadable ─────────────────────────────

func TestLoad_MissingFile_EmptySet(t *testing.T) {
	set := Load(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if set == nil {
		t.Fatal("Load returned nil, want a non-nil empty map")
	}
	if len(set) != 0 {
		t.Fatalf("set = %+v, want empty for a missing file", set)
	}
}

func TestLoad_CorruptFile_EmptySet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hidden.json")
	if err := os.WriteFile(path, []byte("{ this is not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	set := Load(path)
	if len(set) != 0 {
		t.Fatalf("set = %+v, want empty (fail-open) for a corrupt file", set)
	}
}

// TestAdd_CorruptFile_PreservesOriginalBeforeOverwriting is the data-loss pin.
// Load fail-opens a corrupt hidden.json to the EMPTY set, so Add's rewrite used
// to replace every tombstone in that file with just the one new id — silently
// unhiding every previously hidden loop. Fail-open on read is right; fail-open
// then overwrite is destruction. The damaged bytes must survive as <path>.bad.
func TestAdd_CorruptFile_PreservesOriginalBeforeOverwriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hidden.json")
	corrupt := `{"hidden":["old-1","old-2"` // truncated mid-array
	if err := os.WriteFile(path, []byte(corrupt), 0o644); err != nil {
		t.Fatal(err)
	}

	set, err := Add(path, "new-1")
	if err != nil {
		t.Fatalf("Add over a corrupt file = %v, want it to succeed after backing up", err)
	}
	if !set["new-1"] {
		t.Errorf("set = %+v, want the new tombstone present", set)
	}

	backup, err := os.ReadFile(path + corruptedFileSuffix)
	if err != nil {
		t.Fatalf("corrupt file was destroyed, not preserved: %v", err)
	}
	if string(backup) != corrupt {
		t.Errorf("backup = %q, want the original bytes %q", backup, corrupt)
	}
	// And the ids that were legible inside the damaged file are still
	// recoverable by hand from the backup.
	if !strings.Contains(string(backup), "old-1") {
		t.Errorf("backup lost the prior tombstones: %q", backup)
	}
}

// TestAdd_CorruptFile_UnbackupableRefusesWrite: if the damaged file cannot be
// set aside, Add must refuse rather than overwrite it — losing tombstones is
// worse than failing to add one (the caller still hides in memory).
func TestAdd_CorruptFile_UnbackupableRefusesWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hidden.json")
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Read-only parent dir → the rename to <path>.bad cannot happen.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	if _, err := Add(path, "new-1"); err == nil {
		t.Fatal("Add = nil error, want a refusal when the corrupt file can't be preserved")
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("original file disappeared: %v", readErr)
	}
	if string(data) != "not json" {
		t.Errorf("original file was modified to %q, want it untouched", data)
	}
}

// TestAdd_ValidFile_NoBackupLitter: the backup path is for corruption only.
func TestAdd_ValidFile_NoBackupLitter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hidden.json")
	if _, err := Add(path, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := Add(path, "b"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + corruptedFileSuffix); !os.IsNotExist(err) {
		t.Errorf("a healthy file produced a %s backup", corruptedFileSuffix)
	}
}

func TestLoad_EmptyPath_EmptySet(t *testing.T) {
	if len(Load("")) != 0 {
		t.Fatal("Load(\"\") should fail-open to an empty set")
	}
}

func TestLoad_ValidFile_ReadsIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hidden.json")
	if err := os.WriteFile(path, []byte(`{"hidden":["a","b"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	set := Load(path)
	if !set["a"] || !set["b"] || len(set) != 2 {
		t.Fatalf("set = %+v, want {a,b}", set)
	}
}

func TestLoad_SkipsEmptyID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hidden.json")
	if err := os.WriteFile(path, []byte(`{"hidden":["","a"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	set := Load(path)
	if len(set) != 1 || !set["a"] {
		t.Fatalf("set = %+v, want only {a} — empty id skipped", set)
	}
}

// ── Add: persist + round-trip ────────────────────────────────────────────

func TestAdd_PersistsAndRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hidden.json")

	set, err := Add(path, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if !set["sess-1"] {
		t.Fatalf("returned set = %+v, want sess-1", set)
	}
	// A fresh Load must see it — proves it hit disk.
	if !Load(path)["sess-1"] {
		t.Fatal("sess-1 not persisted to disk")
	}
}

func TestAdd_AccumulatesAcrossCalls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hidden.json")
	if _, err := Add(path, "sess-1"); err != nil {
		t.Fatal(err)
	}
	set, err := Add(path, "sess-2")
	if err != nil {
		t.Fatal(err)
	}
	if !set["sess-1"] || !set["sess-2"] || len(set) != 2 {
		t.Fatalf("set = %+v, want {sess-1,sess-2} — Add must not clobber prior entries", set)
	}
}

func TestAdd_CreatesParentDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "sub", "hidden.json")
	if _, err := Add(path, "sess-1"); err != nil {
		t.Fatalf("Add should MkdirAll parents: %v", err)
	}
	if !Load(path)["sess-1"] {
		t.Fatal("sess-1 not persisted under freshly-created parent dir")
	}
}

func TestAdd_EmptyPath_Errors(t *testing.T) {
	if _, err := Add("", "sess-1"); err == nil {
		t.Fatal("Add with an empty path should return an error, not silently succeed")
	}
}

func TestAdd_EmptyID_NoTombstoneWritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hidden.json")
	set, err := Add(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(set) != 0 {
		t.Fatalf("set = %+v, want empty — an empty id must not be tombstoned", set)
	}
}

// TestAdd_NoTempLitter: an atomic temp-file+rename must leave no ".hidden-*"
// temp behind on success.
func TestAdd_NoTempLitter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hidden.json")
	if _, err := Add(path, "sess-1"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".hidden-") {
			t.Errorf("leftover temp file %q — atomic write must clean up", e.Name())
		}
	}
	if len(entries) != 1 {
		t.Fatalf("dir has %d entries, want exactly 1 (hidden.json)", len(entries))
	}
}
