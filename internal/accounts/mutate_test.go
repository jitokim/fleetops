package accounts

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── LoadDocument ───────────────────────────────────────────────────────────

// A missing file is a writable empty document, not an error — the first
// `accounts add` must be able to create accounts.json from nothing.
func TestLoadDocument_MissingFileIsEmptyNotAnError(t *testing.T) {
	doc, err := LoadDocument(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("missing file returned error %v, want nil", err)
	}
	if len(doc.Aliases) != 0 || len(doc.Bindings) != 0 {
		t.Fatalf("missing file produced non-empty doc %+v", doc)
	}
}

// A present-but-malformed file MUST error — never a silent clobber that
// overwrites the user's file with a fresh empty one.
func TestLoadDocument_MalformedJSONIsAnError(t *testing.T) {
	path := writeConfig(t, `{ "aliases": { "work": `) // truncated
	if _, err := LoadDocument(path); err == nil {
		t.Fatal("malformed JSON loaded without error; want an error")
	}
}

// A top level that is not an object (e.g. an array) is malformed too.
func TestLoadDocument_NonObjectTopLevelIsAnError(t *testing.T) {
	path := writeConfig(t, `["not", "an", "object"]`)
	if _, err := LoadDocument(path); err == nil {
		t.Fatal("array top level loaded without error; want an error")
	}
}

// ── AddAlias ───────────────────────────────────────────────────────────────

func TestAddAlias_RejectsDuplicate(t *testing.T) {
	doc := emptyDoc(t)
	if err := doc.AddAlias("work", "/abs/work"); err != nil {
		t.Fatalf("first add: %v", err)
	}
	err := doc.AddAlias("work", "/abs/other")
	if !errors.Is(err, ErrAliasExists) {
		t.Fatalf("duplicate add error = %v, want ErrAliasExists", err)
	}
}

func TestAddAlias_RejectsUnsafeSlug(t *testing.T) {
	doc := emptyDoc(t)
	for _, bad := range []string{"", ".", "..", "a/b", "a b", "wörk", "a.b"} {
		if err := doc.AddAlias(bad, "/abs/dir"); !errors.Is(err, ErrInvalidAlias) {
			t.Fatalf("alias %q error = %v, want ErrInvalidAlias", bad, err)
		}
	}
}

func TestAddAlias_RejectsNonAbsoluteDir(t *testing.T) {
	doc := emptyDoc(t)
	if err := doc.AddAlias("work", "relative/dir"); !errors.Is(err, ErrPathNotAbsolute) {
		t.Fatalf("relative dir error = %v, want ErrPathNotAbsolute", err)
	}
}

func TestAddAlias_AcceptsSafeSlugAndAbsoluteDir(t *testing.T) {
	doc := emptyDoc(t)
	if err := doc.AddAlias("work-1_A", "/abs/work"); err != nil {
		t.Fatalf("valid add rejected: %v", err)
	}
	if doc.Aliases["work-1_A"] != "/abs/work" {
		t.Fatalf("alias not recorded: %+v", doc.Aliases)
	}
}

// ── Bind ───────────────────────────────────────────────────────────────────

func TestBind_RejectsUnknownAlias(t *testing.T) {
	doc := emptyDoc(t)
	if err := doc.Bind("/abs/repo", "ghost"); !errors.Is(err, ErrUnknownAlias) {
		t.Fatalf("bind to unknown alias error = %v, want ErrUnknownAlias", err)
	}
}

func TestBind_RejectsNonAbsolutePath(t *testing.T) {
	doc := emptyDoc(t)
	mustAdd(t, doc, "work", "/abs/work")
	if err := doc.Bind("relative/repo", "work"); !errors.Is(err, ErrPathNotAbsolute) {
		t.Fatalf("relative bind path error = %v, want ErrPathNotAbsolute", err)
	}
}

// Re-binding an existing path REPLACES its alias in place rather than appending
// a duplicate — otherwise two equal-length prefix matches would fight.
func TestBind_ReplacesExistingPathInPlace(t *testing.T) {
	doc := emptyDoc(t)
	mustAdd(t, doc, "work", "/abs/work")
	mustAdd(t, doc, "personal", "/abs/personal")
	mustBind(t, doc, "/abs/repo", "work")
	mustBind(t, doc, "/abs/repo", "personal")
	if len(doc.Bindings) != 1 {
		t.Fatalf("rebind produced %d bindings, want 1: %+v", len(doc.Bindings), doc.Bindings)
	}
	if doc.Bindings[0].Alias != "personal" {
		t.Fatalf("rebind alias = %q, want personal", doc.Bindings[0].Alias)
	}
}

// ── Unbind ─────────────────────────────────────────────────────────────────

func TestUnbind_ReportsMiss(t *testing.T) {
	doc := emptyDoc(t)
	if doc.Unbind("/abs/nothing") {
		t.Fatal("Unbind on absent path returned true, want false")
	}
}

func TestUnbind_RemovesMatch(t *testing.T) {
	doc := emptyDoc(t)
	mustAdd(t, doc, "work", "/abs/work")
	mustBind(t, doc, "/abs/repo", "work")
	if !doc.Unbind("/abs/repo/") { // trailing slash — clean before compare
		t.Fatal("Unbind on present path returned false, want true")
	}
	if len(doc.Bindings) != 0 {
		t.Fatalf("binding survived unbind: %+v", doc.Bindings)
	}
}

// ── RemoveAlias ────────────────────────────────────────────────────────────

func TestRemoveAlias_RejectsUnknown(t *testing.T) {
	doc := emptyDoc(t)
	if err := doc.RemoveAlias("ghost", false); !errors.Is(err, ErrUnknownAlias) {
		t.Fatalf("remove unknown error = %v, want ErrUnknownAlias", err)
	}
}

// Without --force, an alias still referenced by a binding must NOT be removed —
// that would leave the file failing to load.
func TestRemoveAlias_RefusesWhenBoundWithoutForce(t *testing.T) {
	doc := emptyDoc(t)
	mustAdd(t, doc, "work", "/abs/work")
	mustBind(t, doc, "/abs/repo", "work")
	if err := doc.RemoveAlias("work", false); !errors.Is(err, ErrAliasInUse) {
		t.Fatalf("remove bound alias error = %v, want ErrAliasInUse", err)
	}
	if _, ok := doc.Aliases["work"]; !ok {
		t.Fatal("alias was removed despite the refusal")
	}
}

// --force removes the alias AND its dangling bindings, keeping the file loadable.
func TestRemoveAlias_ForceDropsBindings(t *testing.T) {
	doc := emptyDoc(t)
	mustAdd(t, doc, "work", "/abs/work")
	mustBind(t, doc, "/abs/repo", "work")
	if err := doc.RemoveAlias("work", true); err != nil {
		t.Fatalf("forced remove: %v", err)
	}
	if _, ok := doc.Aliases["work"]; ok {
		t.Fatal("alias survived forced remove")
	}
	if len(doc.Bindings) != 0 {
		t.Fatalf("bindings survived forced remove: %+v", doc.Bindings)
	}
}

// RemoveAlias must never touch the config dir on disk — un-naming is not
// logging out.
func TestRemoveAlias_LeavesConfigDirOnDisk(t *testing.T) {
	dir := t.TempDir()
	credDir := filepath.Join(dir, "creds")
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		t.Fatal(err)
	}
	doc := emptyDoc(t)
	mustAdd(t, doc, "work", credDir)
	if err := doc.RemoveAlias("work", false); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(credDir); err != nil {
		t.Fatalf("config dir was disturbed by RemoveAlias: %v", err)
	}
}

// ── Save: atomic, field-preserving, loadable ──────────────────────────────

// A round trip through Save→Load must survive unknown top-level fields the CLI
// does not model — a forward-compat field must not vanish on the next write.
func TestSave_PreservesUnknownTopLevelFields(t *testing.T) {
	path := writeConfig(t, `{
      "aliases": { "work": "/abs/work" },
      "bindings": [],
      "futureFeature": { "keep": "me", "n": 7 },
      "schemaVersion": 3
    }`)
	doc, err := LoadDocument(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	mustAdd(t, doc, "personal", "/abs/personal")
	if err := doc.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	var got map[string]any
	raw, _ := os.ReadFile(path)
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if _, ok := got["futureFeature"]; !ok {
		t.Fatalf("unknown field futureFeature was dropped by Save: %s", raw)
	}
	if got["schemaVersion"].(float64) != 3 {
		t.Fatalf("schemaVersion changed: %v", got["schemaVersion"])
	}
	// And the mutation itself landed, readable by the pure resolver.
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("reload via Load: %v", err)
	}
	if cfg.Aliases["personal"] != "/abs/personal" {
		t.Fatalf("added alias missing after save: %+v", cfg.Aliases)
	}
}

// Save must not rewrite a "~"-form path in a field the command did not touch:
// adding a binding must leave a tilde alias dir literally as written.
func TestSave_PreservesUntouchedTildePaths(t *testing.T) {
	path := writeConfig(t, `{
      "aliases": { "work": "~/.claude-work" },
      "bindings": []
    }`)
	doc, err := LoadDocument(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	mustBind(t, doc, "/abs/repo", "work")
	if err := doc.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), `"~/.claude-work"`) {
		t.Fatalf("tilde path was rewritten by Save: %s", raw)
	}
}

// Save must refuse to write a file that Load would then reject — here a binding
// naming a missing alias, injected past the mutation guards, must be caught
// before the atomic write.
func TestSave_RefusesToWriteUnloadableFile(t *testing.T) {
	doc := emptyDoc(t)
	doc.Bindings = append(doc.Bindings, Binding{Path: "/abs/repo", Alias: "ghost"})
	if err := doc.Save(); err == nil {
		t.Fatal("Save wrote a file with a dangling binding; want an error")
	}
	if _, err := os.Stat(doc.path); !os.IsNotExist(err) {
		t.Fatalf("Save left a file behind on validation failure: stat err = %v", err)
	}
}

// A fresh add writes the documented shape: aliases object + empty bindings array
// (never a null), and reloads through the pure resolver.
func TestSave_FreshFileHasEmptyBindingsArray(t *testing.T) {
	doc := emptyDoc(t)
	mustAdd(t, doc, "work", "/abs/work")
	if err := doc.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, _ := os.ReadFile(doc.path)
	if !strings.Contains(string(raw), `"bindings": []`) {
		t.Fatalf("fresh file missing empty bindings array: %s", raw)
	}
}

// ── BindingsForAlias twins ─────────────────────────────────────────────────

// The Config and Document twins must return the SAME paths, in file order, for
// the same bindings — they share bindingPathsFor so they cannot drift.
func TestBindingsForAlias_ConfigAndDocumentAgree(t *testing.T) {
	doc := emptyDoc(t)
	mustAdd(t, doc, "work", "/abs/work")
	mustAdd(t, doc, "personal", "/abs/personal")
	mustBind(t, doc, "/abs/a", "work")
	mustBind(t, doc, "/abs/b", "personal")
	mustBind(t, doc, "/abs/c", "work")

	cfg := Config{Aliases: doc.Aliases, Bindings: doc.Bindings}
	want := []string{"/abs/a", "/abs/c"}
	for _, got := range [][]string{doc.BindingsForAlias("work"), cfg.BindingsForAlias("work")} {
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("BindingsForAlias(work) = %v, want %v", got, want)
		}
	}
	if len(cfg.BindingsForAlias("ghost")) != 0 {
		t.Fatalf("unknown alias should have no bindings")
	}
}

// ── NormalizePath ──────────────────────────────────────────────────────────

func TestNormalizePath_RejectsEmpty(t *testing.T) {
	if _, err := NormalizePath(""); !errors.Is(err, ErrEmptyPath) {
		t.Fatalf("empty path error = %v, want ErrEmptyPath", err)
	}
}

func TestNormalizePath_ExpandsTildeToAbsolute(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	got, err := NormalizePath("~/src/acme")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	want := filepath.Join(home, "src", "acme")
	if got != want {
		t.Fatalf("NormalizePath(~/src/acme) = %q, want %q", got, want)
	}
}

func TestNormalizePath_MakesRelativeAbsolute(t *testing.T) {
	got, err := NormalizePath("rel/dir")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("NormalizePath(rel/dir) = %q, not absolute", got)
	}
}

// ── IsSafeAlias ────────────────────────────────────────────────────────────

func TestIsSafeAlias(t *testing.T) {
	safe := []string{"work", "personal", "acct-1", "a_b", "ABC"}
	unsafe := []string{"", ".", "..", "a/b", "a b", "a.b", "wörk", "a\tb"}
	for _, s := range safe {
		if !IsSafeAlias(s) {
			t.Errorf("IsSafeAlias(%q) = false, want true", s)
		}
	}
	for _, s := range unsafe {
		if IsSafeAlias(s) {
			t.Errorf("IsSafeAlias(%q) = true, want false", s)
		}
	}
}

// ── helpers ────────────────────────────────────────────────────────────────

func emptyDoc(t *testing.T) *Document {
	t.Helper()
	doc, err := LoadDocument(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatalf("LoadDocument: %v", err)
	}
	return doc
}

func mustAdd(t *testing.T, doc *Document, alias, dir string) {
	t.Helper()
	if err := doc.AddAlias(alias, dir); err != nil {
		t.Fatalf("AddAlias(%q,%q): %v", alias, dir, err)
	}
}

func mustBind(t *testing.T, doc *Document, path, alias string) {
	t.Helper()
	if err := doc.Bind(path, alias); err != nil {
		t.Fatalf("Bind(%q,%q): %v", path, alias, err)
	}
}
