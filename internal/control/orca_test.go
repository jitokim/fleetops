package control

import "testing"

func TestOrcaResumeCmd(t *testing.T) {
	got := orcaResumeCmd("term_abc123", "hello world")
	want := []string{"orca", "terminal", "send", "--terminal", "term_abc123", "--text", "hello world", "--enter", "--json"}

	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	hasEnter := false
	for _, a := range got {
		if a == "--enter" {
			hasEnter = true
		}
	}
	if !hasEnter {
		t.Error("argv missing --enter")
	}
}

func TestOrcaResumeCmd_EmptyPromptFallback(t *testing.T) {
	got := orcaResumeCmd("term_abc123", "")
	want := []string{"orca", "terminal", "send", "--terminal", "term_abc123", "--text", "", "--enter", "--json"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestOrcaApproveCmd_ReusesResumeCmdWithEmptyPrompt(t *testing.T) {
	// Approve is defined as Resume with an empty prompt (--text '' --enter
	// accepts the default highlighted option via a bare Enter) — same argv
	// shape as the empty-prompt resume case.
	got := orcaResumeCmd("term_abc123", "")
	want := []string{"orca", "terminal", "send", "--terminal", "term_abc123", "--text", "", "--enter", "--json"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestOrcaFocusCmd(t *testing.T) {
	got := orcaFocusCmd("term_abc123")
	want := []string{"orca", "terminal", "switch", "--terminal", "term_abc123", "--json"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// realOrcaFixture is the (abridged, faithful) real `orca terminal list
// --json` output captured from the captain's machine: an RPC envelope with
// four terminals, two of them ("✳ team" and "Terminal 2") sharing the
// "aboard" worktree.
const realOrcaFixture = `{
	"id": "7c02555b-...",
	"ok": true,
	"result": {
		"terminals": [
			{"handle":"term_df51fe60-...","worktreePath":"/Users/imac/IdeaProjects/aboard","title":"✳ team","connected":true,"writable":true,"lastOutputAt":1784134661341,"preview":""},
			{"handle":"term_f15e252e-...","worktreePath":"/Users/imac/IdeaProjects/aboard","title":"Terminal 2","connected":true,"writable":true,"lastOutputAt":1784134661258,"preview":"zsh: command not found: cmux"},
			{"handle":"term_8d0a6496-...","worktreePath":"/Users/imac/IdeaProjects/dotfiles","title":"✳ team","connected":true,"writable":true,"lastOutputAt":1784028618065,"preview":"..."},
			{"handle":"term_73a99234-...","worktreePath":"/Users/imac/orca/projects/asre","title":"✳ team","connected":true,"writable":true,"lastOutputAt":1784134574859,"preview":""}
		],
		"visualLayouts": [],
		"totalCount": 4,
		"truncated": false
	},
	"_meta": {"runtimeId":"..."}
}`

func TestSelectClaudeOrcaTerminal_OnlyTier2Matches_NotFound(t *testing.T) {
	// P0-3: LocateClaude must NOT fall back below tier-1 (✳-titled, connected,
	// writable) — a connected+writable-but-not-Claude-titled tab (tier-2 in
	// selectOrcaTerminal's fallback) must come back not-found, not that tab.
	terminals := []orcaTerminal{
		{Handle: "term_shell", WorktreePath: "/Users/imac/IdeaProjects/aboard", Title: "Terminal 2", Connected: true, Writable: true, LastOutputAt: 999},
	}
	if _, ok := selectClaudeOrcaTerminal(terminals, "-Users-imac-IdeaProjects-aboard"); ok {
		t.Error("expected ok=false — only a tier-2 (non-✳) match exists, must not degrade to it")
	}
}

func TestSelectClaudeOrcaTerminal_PicksTier1EvenWithNewerTier2Present(t *testing.T) {
	terminals := []orcaTerminal{
		{Handle: "term_shell", WorktreePath: "/Users/imac/IdeaProjects/aboard", Title: "Terminal 2", Connected: true, Writable: true, LastOutputAt: 999999},
		{Handle: "term_claude", WorktreePath: "/Users/imac/IdeaProjects/aboard", Title: "✳ team", Connected: true, Writable: true, LastOutputAt: 1},
	}
	target, ok := selectClaudeOrcaTerminal(terminals, "-Users-imac-IdeaProjects-aboard")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if target.ID != "term_claude" {
		t.Errorf("got ID %q, want term_claude (only tier-1 is eligible)", target.ID)
	}
}

func TestSelectClaudeOrcaTerminal_TwoTier1Matches_Refuses(t *testing.T) {
	// Residual #1: two confirmed claude terminals at the same directory —
	// there is no way to tell which one the human meant, so LocateClaude's
	// backstop must refuse rather than pick either (freshest-tab tiering
	// would silently pick one, exactly the wrong-terminal hazard this exists
	// to prevent).
	terminals := []orcaTerminal{
		{Handle: "term_a", WorktreePath: "/Users/imac/IdeaProjects/aboard", Title: "✳ team", Connected: true, Writable: true, LastOutputAt: 1},
		{Handle: "term_b", WorktreePath: "/Users/imac/IdeaProjects/aboard", Title: "✳ team 2", Connected: true, Writable: true, LastOutputAt: 999},
	}
	if _, ok := selectClaudeOrcaTerminal(terminals, "-Users-imac-IdeaProjects-aboard"); ok {
		t.Error("expected ok=false — two tier-1 matches is ambiguous, must refuse")
	}
}

func TestSelectClaudeOrcaTerminal_OneTier1Match_Found(t *testing.T) {
	terminals := []orcaTerminal{
		{Handle: "term_a", WorktreePath: "/Users/imac/IdeaProjects/aboard", Title: "✳ team", Connected: true, Writable: true, LastOutputAt: 1},
	}
	target, ok := selectClaudeOrcaTerminal(terminals, "-Users-imac-IdeaProjects-aboard")
	if !ok {
		t.Fatal("expected ok=true — exactly one tier-1 match")
	}
	if target.ID != "term_a" {
		t.Errorf("got ID %q, want term_a", target.ID)
	}
}

func TestSelectClaudeOrcaTerminal_DotContainingWorktreePath_Matches(t *testing.T) {
	// Residual #4: encodeCwd (both "/" and "." -> "-") lets a dot-containing
	// worktreePath actuate instead of always degrading.
	terminals := []orcaTerminal{
		{Handle: "term_a", WorktreePath: "/Users/imac/.claude-mem/observer-sessions", Title: "✳ team", Connected: true, Writable: true, LastOutputAt: 1},
	}
	target, ok := selectClaudeOrcaTerminal(terminals, "-Users-imac--claude-mem-observer-sessions")
	if !ok {
		t.Fatal("expected ok=true — encodeCwd must match the dot-containing path")
	}
	if target.ID != "term_a" {
		t.Errorf("got ID %q, want term_a", target.ID)
	}
}

func TestSelectClaudeOrcaTerminal_NoMatchAtAll(t *testing.T) {
	terminals := []orcaTerminal{
		{Handle: "term_a", WorktreePath: "/x/other", Title: "✳ team", Connected: true, Writable: true},
	}
	if _, ok := selectClaudeOrcaTerminal(terminals, "-Users-imac-IdeaProjects-aboard"); ok {
		t.Error("expected ok=false when no terminal shares the directory")
	}
}

func TestParseOrcaTerminals_EnvelopeUnwrap_PicksClaudeTabOverShell(t *testing.T) {
	// Both "aboard" terminals are connected+writable, so without the title
	// tier this would be ambiguous / could pick the bare-zsh "Terminal 2".
	// The Claude Code tab ("✳" prefix) must win regardless — sending a
	// prompt into a bare shell would execute it as a shell command.
	target, ok := parseOrcaTerminals([]byte(realOrcaFixture), "-Users-imac-IdeaProjects-aboard")
	if !ok {
		t.Fatal("expected ok=true")
	}
	want := Target{Backend: "orca", ID: "term_df51fe60-...", Cwd: "/Users/imac/IdeaProjects/aboard"}
	if target != want {
		t.Errorf("got %+v, want %+v (the \"✳ team\" tab, not \"Terminal 2\")", target, want)
	}
}

func TestSelectOrcaTerminal_TitleTierBeatsRecencyAcrossTiers(t *testing.T) {
	// The Claude Code tab has an OLDER lastOutputAt than the bare shell tab —
	// title tier must still win; recency only breaks ties within a tier.
	terminals := []orcaTerminal{
		{Handle: "term_shell", WorktreePath: "/Users/imac/IdeaProjects/aboard", Title: "Terminal 2", Connected: true, Writable: true, LastOutputAt: 999999},
		{Handle: "term_claude", WorktreePath: "/Users/imac/IdeaProjects/aboard", Title: "✳ team", Connected: true, Writable: true, LastOutputAt: 1},
	}
	target, ok := selectOrcaTerminal(terminals, "-Users-imac-IdeaProjects-aboard")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if target.ID != "term_claude" {
		t.Errorf("got ID %q, want term_claude (title tier must win despite lower lastOutputAt)", target.ID)
	}
}

func TestSelectOrcaTerminal_RecencyBreaksTieWithinTier(t *testing.T) {
	// Two Claude Code tabs (same tier) sharing a worktree — the more
	// recently active one wins.
	terminals := []orcaTerminal{
		{Handle: "term_older", WorktreePath: "/Users/imac/IdeaProjects/aboard", Title: "✳ team", Connected: true, Writable: true, LastOutputAt: 100},
		{Handle: "term_newer", WorktreePath: "/Users/imac/IdeaProjects/aboard", Title: "✳ team", Connected: true, Writable: true, LastOutputAt: 200},
	}
	target, ok := selectOrcaTerminal(terminals, "-Users-imac-IdeaProjects-aboard")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if target.ID != "term_newer" {
		t.Errorf("got ID %q, want term_newer (higher lastOutputAt within same tier)", target.ID)
	}
}

func TestSelectOrcaTerminal_AnyMatchTierAlsoUsesRecency(t *testing.T) {
	// Neither terminal is connected+writable, so both fall to the last
	// (any-match) tier — recency still breaks the tie there.
	terminals := []orcaTerminal{
		{Handle: "term_a", WorktreePath: "/Users/imac/IdeaProjects/aboard", Connected: false, Writable: false, LastOutputAt: 5},
		{Handle: "term_b", WorktreePath: "/Users/imac/IdeaProjects/aboard", Connected: false, Writable: false, LastOutputAt: 10},
	}
	target, ok := selectOrcaTerminal(terminals, "-Users-imac-IdeaProjects-aboard")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if target.ID != "term_b" {
		t.Errorf("got ID %q, want term_b (highest lastOutputAt in the any-match tier)", target.ID)
	}
}

func TestParseOrcaTerminals_BareShapeFallback(t *testing.T) {
	// Source types also show a bare (non-envelope) shape — must still work.
	fixture := []byte(`{"terminals":[{"handle":"term_bare","worktreePath":"/Users/imac/IdeaProjects/aboard","title":"✳ team","connected":true,"writable":true,"lastOutputAt":1}]}`)

	target, ok := parseOrcaTerminals(fixture, "-Users-imac-IdeaProjects-aboard")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if target.ID != "term_bare" {
		t.Errorf("got ID %q, want term_bare", target.ID)
	}
}

func TestParseOrcaTerminals_EnvelopeOKFalse(t *testing.T) {
	fixture := []byte(`{"id":"x","ok":false,"result":{"terminals":[]}}`)
	if _, ok := parseOrcaTerminals(fixture, "-Users-imac-IdeaProjects-aboard"); ok {
		t.Error("expected ok=false when envelope reports ok:false")
	}
}

func TestParseOrcaTerminals_NoMatch(t *testing.T) {
	fixture := []byte(`{"ok":true,"result":{"terminals":[{"handle":"term_a","worktreePath":"/Users/imac/IdeaProjects/other","connected":true,"writable":true}]}}`)
	if _, ok := parseOrcaTerminals(fixture, "-Users-imac-IdeaProjects-aboard"); ok {
		t.Error("expected ok=false when no worktreePath matches projectDir")
	}
}

func TestParseOrcaTerminals_EmptyTerminals(t *testing.T) {
	if _, ok := parseOrcaTerminals([]byte(`{"ok":true,"result":{"terminals":[]}}`), "-Users-imac-IdeaProjects-aboard"); ok {
		t.Error("expected ok=false for empty terminals list")
	}
}

func TestParseOrcaTerminals_GarbageJSON(t *testing.T) {
	if _, ok := parseOrcaTerminals([]byte(`not json`), "-Users-imac-IdeaProjects-aboard"); ok {
		t.Error("expected ok=false for unparseable JSON")
	}
}

func TestOrcaInterruptCmd(t *testing.T) {
	got := orcaInterruptCmd("term_abc123")
	want := []string{"orca", "terminal", "send", "--terminal", "term_abc123", "--interrupt", "--json"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestParseOrcaCreateHandle(t *testing.T) {
	fixture := []byte(`{"id":"x","ok":true,"result":{"terminal":{"handle":"term_new123","worktreePath":"/x"}}}`)
	handle, ok := parseOrcaCreateHandle(fixture)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if handle != "term_new123" {
		t.Errorf("got %q, want %q", handle, "term_new123")
	}
}

func TestParseOrcaCreateHandle_OKFalse(t *testing.T) {
	fixture := []byte(`{"ok":false}`)
	if _, ok := parseOrcaCreateHandle(fixture); ok {
		t.Error("expected ok=false when envelope reports ok:false")
	}
}

func TestParseOrcaCreateHandle_MissingHandle(t *testing.T) {
	fixture := []byte(`{"ok":true,"result":{"terminal":{"worktreePath":"/x"}}}`)
	if _, ok := parseOrcaCreateHandle(fixture); ok {
		t.Error("expected ok=false when handle is empty/missing")
	}
}

func TestParseOrcaCreateHandle_GarbageJSON(t *testing.T) {
	if _, ok := parseOrcaCreateHandle([]byte(`not json`)); ok {
		t.Error("expected ok=false for unparseable JSON")
	}
}

func TestSelectSpawnedOrcaTerminal_PicksSpawnTitleAtCwd(t *testing.T) {
	terminals := []orcaTerminal{
		{Handle: "term_wrong_cwd", WorktreePath: "/x/other", Title: spawnTitle, LastOutputAt: 999},
		{Handle: "term_wrong_title", WorktreePath: "/x/aboard", Title: "Terminal 2", LastOutputAt: 999},
		{Handle: "term_match", WorktreePath: "/x/aboard", Title: spawnTitle, LastOutputAt: 1},
	}
	target, ok := selectSpawnedOrcaTerminal(terminals, "/x/aboard")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if target.ID != "term_match" {
		t.Errorf("got ID %q, want term_match", target.ID)
	}
}

func TestSelectSpawnedOrcaTerminal_AlsoMatchesClaudeTabPrefix(t *testing.T) {
	// once the TUI boots it may relabel the tab with the "✳" prefix instead
	// of keeping spawnTitle — Spawn must still find it.
	terminals := []orcaTerminal{
		{Handle: "term_relabeled", WorktreePath: "/x/aboard", Title: "✳ team", LastOutputAt: 1},
	}
	target, ok := selectSpawnedOrcaTerminal(terminals, "/x/aboard")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if target.ID != "term_relabeled" {
		t.Errorf("got ID %q, want term_relabeled", target.ID)
	}
}

func TestSelectSpawnedOrcaTerminal_PicksNewestAmongMatches(t *testing.T) {
	terminals := []orcaTerminal{
		{Handle: "term_older", WorktreePath: "/x/aboard", Title: spawnTitle, LastOutputAt: 100},
		{Handle: "term_newer", WorktreePath: "/x/aboard", Title: spawnTitle, LastOutputAt: 200},
	}
	target, ok := selectSpawnedOrcaTerminal(terminals, "/x/aboard")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if target.ID != "term_newer" {
		t.Errorf("got ID %q, want term_newer (highest lastOutputAt)", target.ID)
	}
}

func TestSelectSpawnedOrcaTerminal_NoMatch(t *testing.T) {
	terminals := []orcaTerminal{
		{Handle: "term_a", WorktreePath: "/x/other", Title: spawnTitle, LastOutputAt: 1},
	}
	if _, ok := selectSpawnedOrcaTerminal(terminals, "/x/aboard"); ok {
		t.Error("expected ok=false when no terminal matches cwd")
	}
}
