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
