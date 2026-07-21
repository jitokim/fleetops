package accounts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig drops content into a temp accounts.json and returns its path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "accounts.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	return path
}

// ── Load ─────────────────────────────────────────────────────────────────

// The zero-config default: a path that does not exist is INACTIVE, not an
// error. This is the property the whole "spawn behaves exactly as today" promise
// rests on.
func TestLoad_MissingFileIsInactiveNotAnError(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Fatalf("missing file returned error %v, want nil", err)
	}
	if _, _, ok := cfg.ResolveForCwd("/anywhere", nil); ok {
		t.Fatal("an empty config resolved an account; want ok=false")
	}
}

// An empty home dir yields DefaultPath()=="" — Load must treat that like a
// missing file, not choke on it.
func TestLoad_EmptyPathIsInactiveNotAnError(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("empty path returned error %v, want nil", err)
	}
	if len(cfg.Bindings) != 0 || len(cfg.Aliases) != 0 {
		t.Fatalf("empty path produced non-empty config %+v", cfg)
	}
}

// A present-but-corrupt file is the opposite of a missing one: it MUST error,
// because the user opted in and a silent fallback to "no account" would spawn
// under the wrong identity.
func TestLoad_MalformedJSONIsAnError(t *testing.T) {
	path := writeConfig(t, `{ "aliases": { "company": `) // truncated
	if _, err := Load(path); err == nil {
		t.Fatal("malformed JSON loaded without error; want an error")
	}
}

// The fail-closed rule: a binding naming an alias that "aliases" does not
// define must stop the load, never be skipped into a default-account spawn.
func TestLoad_BindingWithUnknownAliasIsAnError(t *testing.T) {
	path := writeConfig(t, `{
	  "aliases": { "company": "/abs/.claude-work" },
	  "bindings": [ { "path": "/abs/work", "alias": "compayn" } ] }`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("binding with a typo'd alias loaded without error; want fail-closed")
	}
	if !strings.Contains(err.Error(), "compayn") {
		t.Fatalf("error %q does not name the offending alias", err)
	}
}

func TestLoad_ValidConfigLoads(t *testing.T) {
	path := writeConfig(t, `{
	  "aliases": { "company": "/abs/.claude-work", "personal": "/abs/.claude-personal" },
	  "bindings": [ { "path": "/abs/work", "alias": "company" } ] }`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("valid config errored: %v", err)
	}
	alias, dir, ok := cfg.ResolveForCwd("/abs/work/repo", nil)
	if !ok || alias != "company" || dir != "/abs/.claude-work" {
		t.Fatalf("ResolveForCwd = (%q,%q,%v), want (company,/abs/.claude-work,true)", alias, dir, ok)
	}
}

// ── ResolveForCwd ──────────────────────────────────────────────────────────

func cfgFixture() Config {
	return Config{
		Aliases: map[string]string{
			"company":  "/abs/.claude-work",
			"personal": "/abs/.claude-personal",
		},
		Bindings: []Binding{
			{Path: "/abs/work", Alias: "company"},
			{Path: "/abs/work/client", Alias: "personal"},
		},
	}
}

func TestResolveForCwd(t *testing.T) {
	cases := []struct {
		name      string
		cwd       string
		wantAlias string
		wantDir   string
		wantOK    bool
	}{
		{
			name:      "longest prefix wins over a shorter matching binding",
			cwd:       "/abs/work/client/repo",
			wantAlias: "personal",
			wantDir:   "/abs/.claude-personal",
			wantOK:    true,
		},
		{
			name:      "shorter binding matches when the longer one does not",
			cwd:       "/abs/work/other/repo",
			wantAlias: "company",
			wantDir:   "/abs/.claude-work",
			wantOK:    true,
		},
		{
			name:      "exact binding path matches itself",
			cwd:       "/abs/work",
			wantAlias: "company",
			wantDir:   "/abs/.claude-work",
			wantOK:    true,
		},
		{
			name:   "no binding matches an unrelated cwd",
			cwd:    "/somewhere/else",
			wantOK: false,
		},
		{
			name:   "component-wise prefix: /abs/work must not match /abs/workshop",
			cwd:    "/abs/workshop/repo",
			wantOK: false,
		},
		{
			name:      "trailing slash on cwd is normalized before matching",
			cwd:       "/abs/work/repo/",
			wantAlias: "company",
			wantDir:   "/abs/.claude-work",
			wantOK:    true,
		},
	}
	cfg := cfgFixture()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			alias, dir, ok := cfg.ResolveForCwd(tc.cwd, nil)
			if ok != tc.wantOK || alias != tc.wantAlias || dir != tc.wantDir {
				t.Fatalf("ResolveForCwd(%q) = (%q,%q,%v), want (%q,%q,%v)",
					tc.cwd, alias, dir, ok, tc.wantAlias, tc.wantDir, tc.wantOK)
			}
		})
	}
}

// The worktree-inheritance property, exercised through the injected seam: a
// worktree path that is bound to NOTHING resolves via its origin repo, which
// IS bound. This is exactly how a fleetops-spawned worktree inherits the
// origin's account without a binding of its own.
func TestResolveForCwd_WorktreeResolvesToMainRepoAccount(t *testing.T) {
	cfg := cfgFixture()
	worktree := "/abs/work-wt-20260722-010101"
	mainRepoDir := func(cwd string) (string, bool) {
		if cwd == worktree {
			return "/abs/work", true // git maps the worktree back to its origin
		}
		return "", false
	}

	// Without the seam the worktree path matches nothing.
	if _, _, ok := cfg.ResolveForCwd(worktree, nil); ok {
		t.Fatal("worktree path resolved without the main-repo seam; the fixture is wrong")
	}

	alias, dir, ok := cfg.ResolveForCwd(worktree, mainRepoDir)
	if !ok || alias != "company" || dir != "/abs/.claude-work" {
		t.Fatalf("worktree resolve = (%q,%q,%v), want the origin's (company,/abs/.claude-work,true)", alias, dir, ok)
	}
}

// A main-repo seam that reports ok=false (cwd is not a repo) must leave cwd
// untouched, not blank it out.
func TestResolveForCwd_SeamMissIsIgnored(t *testing.T) {
	cfg := cfgFixture()
	seamMiss := func(string) (string, bool) { return "", false }
	alias, _, ok := cfg.ResolveForCwd("/abs/work/repo", seamMiss)
	if !ok || alias != "company" {
		t.Fatalf("a seam miss changed resolution: (%q,%v)", alias, ok)
	}
}

// Fail closed at RESOLVE time too, for a Config built directly (bypassing
// Load's validate): a binding whose alias is undefined must yield ok=false.
func TestResolveForCwd_UnknownAliasFailsClosed(t *testing.T) {
	cfg := Config{
		Aliases:  map[string]string{"company": "/abs/.claude-work"},
		Bindings: []Binding{{Path: "/abs/work", Alias: "ghost"}},
	}
	if _, _, ok := cfg.ResolveForCwd("/abs/work/repo", nil); ok {
		t.Fatal("a binding with an undefined alias resolved; want fail-closed ok=false")
	}
}

// An alias defined with an empty config dir is not a usable account.
func TestResolveForCwd_EmptyConfigDirIsNotAnAccount(t *testing.T) {
	cfg := Config{
		Aliases:  map[string]string{"company": ""},
		Bindings: []Binding{{Path: "/abs/work", Alias: "company"}},
	}
	if _, _, ok := cfg.ResolveForCwd("/abs/work/repo", nil); ok {
		t.Fatal("an alias with an empty config dir resolved; want ok=false")
	}
}

func TestResolveForCwd_EmptyConfigResolvesNothing(t *testing.T) {
	var cfg Config
	if _, _, ok := cfg.ResolveForCwd("/abs/work/repo", nil); ok {
		t.Fatal("the zero Config resolved an account; want ok=false")
	}
}

// ── AliasForConfigDir ──────────────────────────────────────────────────────

func TestAliasForConfigDir_FindsTheAlias(t *testing.T) {
	cfg := cfgFixture()
	alias, ok := cfg.AliasForConfigDir("/abs/.claude-personal")
	if !ok || alias != "personal" {
		t.Fatalf("AliasForConfigDir = (%q,%v), want (personal,true)", alias, ok)
	}
}

// Trailing-slash / uncleaned dirs still match after normalization.
func TestAliasForConfigDir_NormalizesBeforeComparing(t *testing.T) {
	cfg := Config{Aliases: map[string]string{"company": "/abs/.claude-work/"}}
	alias, ok := cfg.AliasForConfigDir("/abs/.claude-work")
	if !ok || alias != "company" {
		t.Fatalf("AliasForConfigDir = (%q,%v), want (company,true) after cleaning", alias, ok)
	}
}

// Deterministic tie-break: two aliases sharing a config dir resolve to the
// lexicographically first name, on every run.
func TestAliasForConfigDir_TieBreakIsLexicographicallyFirst(t *testing.T) {
	cfg := Config{Aliases: map[string]string{
		"zeta":  "/abs/.shared",
		"alpha": "/abs/.shared",
		"mid":   "/abs/.shared",
	}}
	for i := 0; i < 20; i++ { // map order is randomized; the answer must not be
		alias, ok := cfg.AliasForConfigDir("/abs/.shared")
		if !ok || alias != "alpha" {
			t.Fatalf("tie-break = (%q,%v) on run %d, want a stable (alpha,true)", alias, ok, i)
		}
	}
}

func TestAliasForConfigDir_NotFound(t *testing.T) {
	cfg := cfgFixture()
	if alias, ok := cfg.AliasForConfigDir("/abs/.nope"); ok {
		t.Fatalf("AliasForConfigDir found %q for an unmapped dir; want ok=false", alias)
	}
}

func TestAliasForConfigDir_EmptyConfig(t *testing.T) {
	var cfg Config
	if _, ok := cfg.AliasForConfigDir("/abs/.claude-work"); ok {
		t.Fatal("the zero Config reverse-resolved an alias; want ok=false")
	}
}

// ── DefaultPath ────────────────────────────────────────────────────────────

func TestDefaultPath_LandsUnderDotFleetops(t *testing.T) {
	got := DefaultPath()
	if got == "" {
		t.Skip("home dir unavailable in this environment")
	}
	if filepath.Base(got) != "accounts.json" || filepath.Base(filepath.Dir(got)) != ".fleetops" {
		t.Fatalf("DefaultPath = %q, want …/.fleetops/accounts.json", got)
	}
}
