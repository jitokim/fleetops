package claude

import (
	"os"
	"path/filepath"
	"testing"
)

// The zero-config guarantee for discovery: with NO accounts.json, the scanner
// reads EXACTLY one root — ~/.claude/projects — byte-identical to before the
// multi-account feature. This is what the whole "single-account users see no
// change" promise rests on at the scan layer.
func TestProjectsRootsFrom_ZeroConfig_SingleDefaultRoot(t *testing.T) {
	defaultRoot := "/home/user/.claude/projects"
	missing := filepath.Join(t.TempDir(), "no-accounts.json")

	roots := projectsRootsFrom(defaultRoot, missing)

	if len(roots) != 1 || roots[0] != defaultRoot {
		t.Fatalf("roots = %v, want exactly [%q] for a zero-config machine", roots, defaultRoot)
	}
}

// With aliases configured, the scanner reads "<configDir>/projects" for EACH,
// alongside the default — otherwise a loop spawned under a non-default account
// is invisible to the whole fleet.
func TestProjectsRootsFrom_WithAliases_AddsEachConfigDirProjects(t *testing.T) {
	defaultRoot := "/home/user/.claude/projects"
	path := filepath.Join(t.TempDir(), "accounts.json")
	writeFile(t, path, `{"aliases":{"company":"/home/user/.claude-work","personal":"/home/user/.claude-personal"}}`)

	roots := projectsRootsFrom(defaultRoot, path)

	want := map[string]bool{
		defaultRoot:                            true,
		"/home/user/.claude-work/projects":     true,
		"/home/user/.claude-personal/projects": true,
	}
	if len(roots) != len(want) {
		t.Fatalf("roots = %v, want the default + one per alias (%d)", roots, len(want))
	}
	for _, r := range roots {
		if !want[r] {
			t.Fatalf("unexpected root %q in %v", r, roots)
		}
	}
	if roots[0] != defaultRoot {
		t.Fatalf("default root must come first, got %v", roots)
	}
}

// An alias that names the DEFAULT config dir (e.g. "main" → ~/.claude) must not
// double the default root — dedup by cleaned path.
func TestProjectsRootsFrom_AliasOnDefaultDir_IsDeduped(t *testing.T) {
	defaultRoot := "/home/user/.claude/projects"
	path := filepath.Join(t.TempDir(), "accounts.json")
	writeFile(t, path, `{"aliases":{"main":"/home/user/.claude"}}`)

	roots := projectsRootsFrom(defaultRoot, path)

	if len(roots) != 1 || roots[0] != defaultRoot {
		t.Fatalf("roots = %v, want a single deduped default root", roots)
	}
}

// A malformed accounts.json must NOT hide the default account's loops: it
// degrades to just the default root, never an empty set or a scan abort.
func TestProjectsRootsFrom_MalformedConfig_FallsBackToDefaultRoot(t *testing.T) {
	defaultRoot := "/home/user/.claude/projects"
	path := filepath.Join(t.TempDir(), "accounts.json")
	// A binding naming an unknown alias — accounts.Load's fail-closed error.
	writeFile(t, path, `{"aliases":{},"bindings":[{"path":"/x","alias":"missing"}]}`)

	roots := projectsRootsFrom(defaultRoot, path)

	if len(roots) != 1 || roots[0] != defaultRoot {
		t.Fatalf("roots = %v, want just the default root when accounts.json is malformed", roots)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
