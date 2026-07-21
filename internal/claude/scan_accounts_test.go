package claude

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jitokim/fleetops/internal/domain"
	"github.com/jitokim/fleetops/internal/sessions"
)

// writeAccountsConfig writes a minimal ~/.fleetops/accounts.json-shaped file
// (internal/accounts.Config's JSON shape) at path, for enrichAccounts tests
// that need a real alias registered.
func writeAccountsConfig(t *testing.T, path, alias, configDir string) {
	t.Helper()
	content := `{"aliases":{"` + alias + `":"` + configDir + `"}}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write accounts config: %v", err)
	}
}

// TestEnrichAccounts_MatchingSession_AttachesAccount is the core wiring:
// a loop whose SessionID has a session-registry entry with a recorded
// ConfigDir gets that ConfigDir, its email/plan, and (when accounts.json
// registers one) its alias — all copied onto domain.Loop.Account.
func TestEnrichAccounts_MatchingSession_AttachesAccount(t *testing.T) {
	sessionsDir := t.TempDir()
	if err := sessions.WriteSession(sessionsDir, "sess-1", sessions.SessionEntry{
		ConfigDir:    "/home/user/.claude-work",
		AccountEmail: "jito@company.com",
		AccountPlan:  "team",
	}); err != nil {
		t.Fatalf("WriteSession: %v", err)
	}
	accountsPath := filepath.Join(t.TempDir(), "accounts.json")
	writeAccountsConfig(t, accountsPath, "company", "/home/user/.claude-work")

	loops := []domain.Loop{{SessionID: "sess-1"}}
	got := enrichAccounts(loops, sessionsDir, accountsPath)

	want := domain.Account{ConfigDir: "/home/user/.claude-work", Alias: "company", Email: "jito@company.com", Plan: "team"}
	if got[0].Account != want {
		t.Errorf("Account = %+v, want %+v", got[0].Account, want)
	}
}

// TestEnrichAccounts_NoAccountsConfig_StillAttachesRawFields confirms alias
// resolution failing (no accounts.json at all, or Load erroring) degrades to
// "" for Alias ONLY — ConfigDir/Email/Plan still come through untouched, since
// they need no accounts.json at all to be meaningful.
func TestEnrichAccounts_NoAccountsConfig_StillAttachesRawFields(t *testing.T) {
	sessionsDir := t.TempDir()
	if err := sessions.WriteSession(sessionsDir, "sess-1", sessions.SessionEntry{
		ConfigDir:    "/home/user/.claude-work",
		AccountEmail: "jito@company.com",
	}); err != nil {
		t.Fatalf("WriteSession: %v", err)
	}
	missingAccountsPath := filepath.Join(t.TempDir(), "does-not-exist.json")

	loops := []domain.Loop{{SessionID: "sess-1"}}
	got := enrichAccounts(loops, sessionsDir, missingAccountsPath)

	if got[0].Account.ConfigDir != "/home/user/.claude-work" {
		t.Errorf("ConfigDir = %q, want /home/user/.claude-work", got[0].Account.ConfigDir)
	}
	if got[0].Account.Email != "jito@company.com" {
		t.Errorf("Email = %q, want jito@company.com", got[0].Account.Email)
	}
	if got[0].Account.Alias != "" {
		t.Errorf("Alias = %q, want empty (no accounts.json to resolve one)", got[0].Account.Alias)
	}
}

// TestEnrichAccounts_MalformedAccountsConfig_TreatedAsInactive mirrors
// control.defaultAccountConfigDir's own precedent for the exact same file: a
// Load error (here, a binding naming an unknown alias) must not sink the
// whole enrichment — the loop still gets its raw ConfigDir/Email/Plan, just
// with no alias resolved, exactly as if accounts.json didn't exist.
func TestEnrichAccounts_MalformedAccountsConfig_TreatedAsInactive(t *testing.T) {
	sessionsDir := t.TempDir()
	if err := sessions.WriteSession(sessionsDir, "sess-1", sessions.SessionEntry{
		ConfigDir: "/home/user/.claude-work",
	}); err != nil {
		t.Fatalf("WriteSession: %v", err)
	}
	accountsPath := filepath.Join(t.TempDir(), "accounts.json")
	// A binding referencing an alias absent from "aliases" — accounts.Load's
	// own fail-closed validation error.
	badConfig := `{"aliases":{},"bindings":[{"path":"/x","alias":"missing"}]}`
	if err := os.WriteFile(accountsPath, []byte(badConfig), 0o644); err != nil {
		t.Fatal(err)
	}

	loops := []domain.Loop{{SessionID: "sess-1"}}
	got := enrichAccounts(loops, sessionsDir, accountsPath)

	if got[0].Account.ConfigDir != "/home/user/.claude-work" {
		t.Errorf("ConfigDir = %q, want /home/user/.claude-work despite the malformed accounts.json", got[0].Account.ConfigDir)
	}
	if got[0].Account.Alias != "" {
		t.Errorf("Alias = %q, want empty — a malformed accounts.json must not sink the whole enrichment", got[0].Account.Alias)
	}
}

// TestEnrichAccounts_NoSessionEntry_LeavesZeroAccount is the common,
// zero-config case at THIS layer: an observed loop the hook never wrote a
// session entry for (or whose entry predates ConfigDir) is left with the
// zero Account — never partially populated, never an error.
func TestEnrichAccounts_NoSessionEntry_LeavesZeroAccount(t *testing.T) {
	sessionsDir := t.TempDir() // empty — no entries at all

	loops := []domain.Loop{{SessionID: "sess-unknown"}}
	got := enrichAccounts(loops, sessionsDir, filepath.Join(t.TempDir(), "accounts.json"))

	if got[0].Account != (domain.Account{}) {
		t.Errorf("Account = %+v, want the zero value for a loop with no session entry", got[0].Account)
	}
}

// ── FINDING #1 (2nd review): durable account ConfigDir over the live entry ────

// THE WEDGE this fixes: a driven loop's transient session entry is GONE (deleted
// on SessionEnd / when the headless process ends), but the DURABLE registry
// ConfigDir — seeded onto Account.ConfigDir by enrichFromRegistry, which runs
// first — must survive enrichAccounts so a cycle-2 redrive still targets the
// right account. Simulated here by pre-seeding Account.ConfigDir with NO session
// entry present.
func TestEnrichAccounts_DurableConfigDir_SurvivesMissingSessionEntry(t *testing.T) {
	sessionsDir := t.TempDir() // no entry for this session — the wedge scenario
	accountsPath := filepath.Join(t.TempDir(), "accounts.json")
	writeAccountsConfig(t, accountsPath, "company", "/home/user/.claude-work")

	loops := []domain.Loop{{SessionID: "sess-gone", Account: domain.Account{ConfigDir: "/home/user/.claude-work"}}}
	got := enrichAccounts(loops, sessionsDir, accountsPath)

	if got[0].Account.ConfigDir != "/home/user/.claude-work" {
		t.Fatalf("ConfigDir = %q, want the durable value — a redrive after the session entry is gone would run the DEFAULT account", got[0].Account.ConfigDir)
	}
	if got[0].Account.Alias != "company" {
		t.Errorf("Alias = %q, want company (resolved from the durable ConfigDir even with no live entry)", got[0].Account.Alias)
	}
}

// When BOTH exist, the durable record ConfigDir WINS over the transient session
// entry's — the record is the source of truth; the entry is a live-only signal.
// Email/Plan still come from the live entry (display-only, best-effort).
func TestEnrichAccounts_DurableConfigDir_WinsOverSessionEntry(t *testing.T) {
	sessionsDir := t.TempDir()
	if err := sessions.WriteSession(sessionsDir, "sess-1", sessions.SessionEntry{
		ConfigDir:    "/home/user/.claude-personal", // a STALE/other value on the live entry
		AccountEmail: "jito@company.com",
		AccountPlan:  "team",
	}); err != nil {
		t.Fatalf("WriteSession: %v", err)
	}
	accountsPath := filepath.Join(t.TempDir(), "accounts.json")
	writeAccountsConfig(t, accountsPath, "company", "/home/user/.claude-work")

	loops := []domain.Loop{{SessionID: "sess-1", Account: domain.Account{ConfigDir: "/home/user/.claude-work"}}}
	got := enrichAccounts(loops, sessionsDir, accountsPath)

	if got[0].Account.ConfigDir != "/home/user/.claude-work" {
		t.Errorf("ConfigDir = %q, want the durable record value to win over the session entry", got[0].Account.ConfigDir)
	}
	if got[0].Account.Alias != "company" {
		t.Errorf("Alias = %q, want company", got[0].Account.Alias)
	}
	if got[0].Account.Email != "jito@company.com" {
		t.Errorf("Email = %q, want the live entry's email (display-only, best-effort)", got[0].Account.Email)
	}
}

// ── FINDING #4 (2nd review): collapse the same session under two roots ────────

// A `cp -R ~/.claude ~/.claude-work` seeds an alias config dir that is a COPY of
// the default one, so the identical session file exists under both roots and the
// cross-root glob lists it TWICE. dedupBySessionID collapses it, keeping the
// newest transcript — otherwise one session is two rows and kill/resume become
// ambiguous.
func TestDedupBySessionID_CollapsesCopyRoots_KeepsNewest(t *testing.T) {
	newer := time.Now()
	older := newer.Add(-time.Hour)

	// Both orderings must land on the newer transcript (the dedup can't rely on
	// the caller's sort).
	cases := []struct {
		name  string
		loops []domain.Loop
	}{
		{"older first", []domain.Loop{
			{SessionID: "sess-1", Path: "/home/user/.claude-work/projects/p/sess-1.jsonl", LastActivity: older},
			{SessionID: "sess-1", Path: "/home/user/.claude/projects/p/sess-1.jsonl", LastActivity: newer},
			{SessionID: "sess-2", Path: "/x/sess-2.jsonl", LastActivity: newer},
		}},
		{"newer first", []domain.Loop{
			{SessionID: "sess-1", Path: "/home/user/.claude/projects/p/sess-1.jsonl", LastActivity: newer},
			{SessionID: "sess-1", Path: "/home/user/.claude-work/projects/p/sess-1.jsonl", LastActivity: older},
			{SessionID: "sess-2", Path: "/x/sess-2.jsonl", LastActivity: newer},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := dedupBySessionID(c.loops)
			if len(got) != 2 {
				t.Fatalf("got %d loops, want 2 (the duplicate session-1 must collapse): %+v", len(got), got)
			}
			for _, l := range got {
				if l.SessionID == "sess-1" && !l.LastActivity.Equal(newer) {
					t.Errorf("kept the OLDER transcript for sess-1 (%v), want the newest (%v)", l.LastActivity, newer)
				}
			}
		})
	}
}

// A single unique session per SessionID passes through untouched — the common
// (no copied roots) case must not be perturbed.
func TestDedupBySessionID_NoDuplicates_Unchanged(t *testing.T) {
	loops := []domain.Loop{
		{SessionID: "a", LastActivity: time.Now()},
		{SessionID: "b", LastActivity: time.Now()},
	}
	if got := dedupBySessionID(loops); len(got) != 2 {
		t.Fatalf("got %d, want 2 — dedup must not drop distinct sessions", len(got))
	}
}

// TestEnrichAccounts_DefaultAccountConfigDir_NeverProbesAlias proves the
// empty-ConfigDir guard: a session recorded with NO CLAUDE_CONFIG_DIR (the
// default account) never even calls into AliasForConfigDir — proven
// indirectly here by registering an alias that (if the guard were absent)
// would spuriously match an empty ConfigDir via filepath.Clean("")=="." — see
// enrichAccounts' own doc for why an empty ConfigDir is never worth
// resolving.
func TestEnrichAccounts_DefaultAccountConfigDir_NeverProbesAlias(t *testing.T) {
	sessionsDir := t.TempDir()
	if err := sessions.WriteSession(sessionsDir, "sess-default", sessions.SessionEntry{
		ConfigDir: "", // default account
	}); err != nil {
		t.Fatalf("WriteSession: %v", err)
	}
	accountsPath := filepath.Join(t.TempDir(), "accounts.json")
	// A deliberately pathological alias mapped to "." (filepath.Clean("")),
	// so a guard-less implementation would wrongly resolve it for the
	// default account's empty ConfigDir.
	writeAccountsConfig(t, accountsPath, "should-never-match", ".")

	loops := []domain.Loop{{SessionID: "sess-default"}}
	got := enrichAccounts(loops, sessionsDir, accountsPath)

	if got[0].Account.Alias != "" {
		t.Errorf("Alias = %q, want empty — the default account (ConfigDir==\"\") must never resolve an alias", got[0].Account.Alias)
	}
	if !got[0].Account.IsDefault() {
		t.Errorf("Account.IsDefault() = false, want true for ConfigDir==\"\"")
	}
}
