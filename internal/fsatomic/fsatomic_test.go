package fsatomic

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testPrefix = ".test-*.tmp"

// tmpLitter returns the names of leftover temp files in dir — the thing a
// failed atomic write must never leave behind.
func tmpLitter(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}
	var litter []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			litter = append(litter, e.Name())
		}
	}
	return litter
}

// TestWriteFile_MissingParentDir_Creates: the registries under ~/.fleetops call
// this before the directory necessarily exists, so creating it is part of the
// contract, not the caller's job.
func TestWriteFile_MissingParentDir_Creates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "rec.json")
	if err := WriteFile(path, []byte(`{"a":1}`), testPrefix); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != `{"a":1}` {
		t.Errorf("content = %q, want %q", got, `{"a":1}`)
	}
}

// TestWriteFile_Overwrite_ReplacesAndLeavesNoLitter: the rewrite-in-place case
// every caller actually runs — the previous bytes are fully replaced (not
// appended to, not partially overwritten) and the temp file is gone.
func TestWriteFile_Overwrite_ReplacesAndLeavesNoLitter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rec.json")
	if err := WriteFile(path, []byte("aaaaaaaaaa"), testPrefix); err != nil {
		t.Fatalf("first WriteFile: %v", err)
	}
	if err := WriteFile(path, []byte("bb"), testPrefix); err != nil {
		t.Fatalf("second WriteFile: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "bb" {
		t.Errorf("content = %q, want %q (short write must truncate, not overlay)", got, "bb")
	}
	if litter := tmpLitter(t, dir); len(litter) != 0 {
		t.Errorf("temp litter = %v, want none", litter)
	}
}

// TestWriteFile_UnwritableParent_ErrorsAndWritesNothing: a parent that cannot
// be created (a plain FILE occupies the path) must surface an error rather
// than silently dropping the record.
func TestWriteFile_UnwritableParent_ErrorsAndWritesNothing(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "notadir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}
	path := filepath.Join(blocker, "rec.json")
	if err := WriteFile(path, []byte("data"), testPrefix); err == nil {
		t.Fatal("WriteFile = nil, want an error when the parent path is a file")
	}
	if got, err := os.ReadFile(blocker); err != nil || string(got) != "x" {
		t.Errorf("blocker = %q (err %v), want it untouched", got, err)
	}
}

// TestWriteFile_UnwritableDir_LeavesNoLitter: when the temp file itself cannot
// be created the call fails cleanly — no partial file at path.
func TestWriteFile_UnwritableDir_LeavesNoLitter(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permissions are not enforced")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil { // r-x: no create allowed
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	path := filepath.Join(dir, "rec.json")
	if err := WriteFile(path, []byte("data"), testPrefix); err == nil {
		t.Fatal("WriteFile = nil, want an error when the temp file cannot be created")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("Stat(path) err = %v, want IsNotExist (nothing may be written)", err)
	}
}
