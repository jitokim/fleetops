package control

import (
	"strings"
	"testing"
)

func TestOrcaEnvelopeErr_OKFalse_ReturnsDescriptiveError(t *testing.T) {
	// verified live: `orca terminal create` exits 0 with this exact shape
	// when cwd isn't a worktree Orca knows about.
	fixture := []byte(`{"ok":false,"error":{"code":"selector_not_found"}}`)

	err := orcaEnvelopeErr(fixture, "/home/user/unregistered")
	if err == nil {
		t.Fatal("expected a non-nil error for an ok:false envelope")
	}
	want := "orca: selector_not_found — /home/user/unregistered is not a worktree registered in Orca (open the repo in Orca first, or select a loop that lives in one)"
	if err.Error() != want {
		t.Errorf("got %q, want %q", err.Error(), want)
	}
}

func TestOrcaEnvelopeErr_OKFalse_MissingCode_FallsBackToUnknown(t *testing.T) {
	fixture := []byte(`{"ok":false}`)
	err := orcaEnvelopeErr(fixture, "/x")
	if err == nil {
		t.Fatal("expected a non-nil error")
	}
	if !strings.Contains(err.Error(), "unknown") {
		t.Errorf("got %q, want it to mention \"unknown\" for a missing error code", err.Error())
	}
}

func TestOrcaEnvelopeErr_OKTrue_ReturnsNil(t *testing.T) {
	if err := orcaEnvelopeErr([]byte(`{"ok":true,"result":{}}`), "/x"); err != nil {
		t.Errorf("got %v, want nil for ok:true", err)
	}
}

func TestOrcaEnvelopeErr_NoOKField_ReturnsNil(t *testing.T) {
	// e.g. `terminal list`'s bare (non-envelope) shape — no "ok" field at all.
	if err := orcaEnvelopeErr([]byte(`{"terminals":[]}`), "/x"); err != nil {
		t.Errorf("got %v, want nil when there's no explicit ok:false", err)
	}
}

func TestOrcaEnvelopeErr_GarbageJSON_ReturnsNil(t *testing.T) {
	// falls through to the caller's own "could not parse..." error instead.
	if err := orcaEnvelopeErr([]byte(`not json`), "/x"); err != nil {
		t.Errorf("got %v, want nil (caller handles the parse failure itself)", err)
	}
}

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
// --json` output captured from a real machine: an RPC envelope with
// four terminals, two of them ("✳ team" and "Terminal 2") sharing the
// "myproject" worktree.
const realOrcaFixture = `{
	"id": "7c02555b-...",
	"ok": true,
	"result": {
		"terminals": [
			{"handle":"term_df51fe60-...","worktreePath":"/home/user/myproject","title":"✳ team","connected":true,"writable":true,"lastOutputAt":1784134661341,"preview":""},
			{"handle":"term_f15e252e-...","worktreePath":"/home/user/myproject","title":"Terminal 2","connected":true,"writable":true,"lastOutputAt":1784134661258,"preview":"zsh: command not found: cmux"},
			{"handle":"term_8d0a6496-...","worktreePath":"/home/user/dotfiles","title":"✳ team","connected":true,"writable":true,"lastOutputAt":1784028618065,"preview":"..."},
			{"handle":"term_73a99234-...","worktreePath":"/home/user/orca/projects/asre","title":"✳ team","connected":true,"writable":true,"lastOutputAt":1784134574859,"preview":""}
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
		{Handle: "term_shell", WorktreePath: "/home/user/myproject", Title: "Terminal 2", Connected: true, Writable: true, LastOutputAt: 999},
	}
	if _, ok := selectClaudeOrcaTerminal(terminals, "-home-user-myproject"); ok {
		t.Error("expected ok=false — only a tier-2 (non-✳) match exists, must not degrade to it")
	}
}

func TestSelectClaudeOrcaTerminal_PicksTier1EvenWithNewerTier2Present(t *testing.T) {
	terminals := []orcaTerminal{
		{Handle: "term_shell", WorktreePath: "/home/user/myproject", Title: "Terminal 2", Connected: true, Writable: true, LastOutputAt: 999999},
		{Handle: "term_claude", WorktreePath: "/home/user/myproject", Title: "✳ team", Connected: true, Writable: true, LastOutputAt: 1},
	}
	target, ok := selectClaudeOrcaTerminal(terminals, "-home-user-myproject")
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
		{Handle: "term_a", WorktreePath: "/home/user/myproject", Title: "✳ team", Connected: true, Writable: true, LastOutputAt: 1},
		{Handle: "term_b", WorktreePath: "/home/user/myproject", Title: "✳ team 2", Connected: true, Writable: true, LastOutputAt: 999},
	}
	if _, ok := selectClaudeOrcaTerminal(terminals, "-home-user-myproject"); ok {
		t.Error("expected ok=false — two tier-1 matches is ambiguous, must refuse")
	}
}

func TestSelectClaudeOrcaTerminal_OneTier1Match_Found(t *testing.T) {
	terminals := []orcaTerminal{
		{Handle: "term_a", WorktreePath: "/home/user/myproject", Title: "✳ team", Connected: true, Writable: true, LastOutputAt: 1},
	}
	target, ok := selectClaudeOrcaTerminal(terminals, "-home-user-myproject")
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
		{Handle: "term_a", WorktreePath: "/home/user/.someplugin/agent-sessions", Title: "✳ team", Connected: true, Writable: true, LastOutputAt: 1},
	}
	target, ok := selectClaudeOrcaTerminal(terminals, "-home-user--someplugin-agent-sessions")
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
	if _, ok := selectClaudeOrcaTerminal(terminals, "-home-user-myproject"); ok {
		t.Error("expected ok=false when no terminal shares the directory")
	}
}

func TestParseOrcaTerminals_EnvelopeUnwrap_PicksClaudeTabOverShell(t *testing.T) {
	// Both "myproject" terminals are connected+writable, so without the title
	// tier this would be ambiguous / could pick the bare-zsh "Terminal 2".
	// The Claude Code tab ("✳" prefix) must win regardless — sending a
	// prompt into a bare shell would execute it as a shell command.
	target, ok := parseOrcaTerminals([]byte(realOrcaFixture), "-home-user-myproject")
	if !ok {
		t.Fatal("expected ok=true")
	}
	want := Target{Backend: "orca", ID: "term_df51fe60-...", Cwd: "/home/user/myproject"}
	if target != want {
		t.Errorf("got %+v, want %+v (the \"✳ team\" tab, not \"Terminal 2\")", target, want)
	}
}

func TestSelectOrcaTerminal_TitleTierBeatsRecencyAcrossTiers(t *testing.T) {
	// The Claude Code tab has an OLDER lastOutputAt than the bare shell tab —
	// title tier must still win; recency only breaks ties within a tier.
	terminals := []orcaTerminal{
		{Handle: "term_shell", WorktreePath: "/home/user/myproject", Title: "Terminal 2", Connected: true, Writable: true, LastOutputAt: 999999},
		{Handle: "term_claude", WorktreePath: "/home/user/myproject", Title: "✳ team", Connected: true, Writable: true, LastOutputAt: 1},
	}
	target, ok := selectOrcaTerminal(terminals, "-home-user-myproject")
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
		{Handle: "term_older", WorktreePath: "/home/user/myproject", Title: "✳ team", Connected: true, Writable: true, LastOutputAt: 100},
		{Handle: "term_newer", WorktreePath: "/home/user/myproject", Title: "✳ team", Connected: true, Writable: true, LastOutputAt: 200},
	}
	target, ok := selectOrcaTerminal(terminals, "-home-user-myproject")
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
		{Handle: "term_a", WorktreePath: "/home/user/myproject", Connected: false, Writable: false, LastOutputAt: 5},
		{Handle: "term_b", WorktreePath: "/home/user/myproject", Connected: false, Writable: false, LastOutputAt: 10},
	}
	target, ok := selectOrcaTerminal(terminals, "-home-user-myproject")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if target.ID != "term_b" {
		t.Errorf("got ID %q, want term_b (highest lastOutputAt in the any-match tier)", target.ID)
	}
}

func TestParseOrcaTerminals_BareShapeFallback(t *testing.T) {
	// Source types also show a bare (non-envelope) shape — must still work.
	fixture := []byte(`{"terminals":[{"handle":"term_bare","worktreePath":"/home/user/myproject","title":"✳ team","connected":true,"writable":true,"lastOutputAt":1}]}`)

	target, ok := parseOrcaTerminals(fixture, "-home-user-myproject")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if target.ID != "term_bare" {
		t.Errorf("got ID %q, want term_bare", target.ID)
	}
}

func TestParseOrcaTerminals_EnvelopeOKFalse(t *testing.T) {
	fixture := []byte(`{"id":"x","ok":false,"result":{"terminals":[]}}`)
	if _, ok := parseOrcaTerminals(fixture, "-home-user-myproject"); ok {
		t.Error("expected ok=false when envelope reports ok:false")
	}
}

func TestParseOrcaTerminals_NoMatch(t *testing.T) {
	fixture := []byte(`{"ok":true,"result":{"terminals":[{"handle":"term_a","worktreePath":"/home/user/other","connected":true,"writable":true}]}}`)
	if _, ok := parseOrcaTerminals(fixture, "-home-user-myproject"); ok {
		t.Error("expected ok=false when no worktreePath matches projectDir")
	}
}

func TestParseOrcaTerminals_EmptyTerminals(t *testing.T) {
	if _, ok := parseOrcaTerminals([]byte(`{"ok":true,"result":{"terminals":[]}}`), "-home-user-myproject"); ok {
		t.Error("expected ok=false for empty terminals list")
	}
}

func TestParseOrcaTerminals_GarbageJSON(t *testing.T) {
	if _, ok := parseOrcaTerminals([]byte(`not json`), "-home-user-myproject"); ok {
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

// realSpawnedLoopFixture is a faithful `orca terminal list --json` capture
// for the regression this fix targets: a fleetops-spawned loop (title
// spawnTitle "mctl loop", connected+writable) living in the fleetops
// worktree, alongside a bare shell tab (title:"") in the SAME worktree and
// unrelated terminals in OTHER worktrees. Extra fields orca emits (ptyId,
// tabId, preview) are present but ignored by the decoder. The loop's title
// is the STATIC create-time --title, NEVER "✳" — the exact live-proven shape
// that made the old ✳-only selectClaudeOrcaTerminal return not-found.
const realSpawnedLoopFixture = `{
	"id": "9f13aa02-...",
	"ok": true,
	"result": {
		"terminals": [
			{"handle":"term_loop","ptyId":"pty_1","tabId":"tab_1","worktreePath":"/Users/imac/IdeaProjects/fleetops","title":"mctl loop","connected":true,"writable":true,"lastOutputAt":1784134661341,"preview":"● Running…"},
			{"handle":"term_shell","ptyId":"pty_2","tabId":"tab_2","worktreePath":"/Users/imac/IdeaProjects/fleetops","title":"","connected":true,"writable":true,"lastOutputAt":1784134600000,"preview":"imac@host fleetops %"},
			{"handle":"term_other","ptyId":"pty_3","tabId":"tab_3","worktreePath":"/Users/imac/IdeaProjects/other","title":"mctl loop","connected":true,"writable":true,"lastOutputAt":1784134574859,"preview":""},
			{"handle":"term_dotfiles","ptyId":"pty_4","tabId":"tab_4","worktreePath":"/Users/imac/dotfiles","title":"✳ team","connected":true,"writable":true,"lastOutputAt":1784028618065,"preview":""}
		],
		"visualLayouts": [],
		"totalCount": 4,
		"truncated": false
	},
	"_meta": {"runtimeId":"..."}
}`

// locateClaudeFromJSON mirrors LocateClaude's body (decode then select) so a
// realistic raw `orca terminal list --json` payload can be driven through the
// actuation-time locator end-to-end, exactly as LocateClaude does at runtime.
func locateClaudeFromJSON(t *testing.T, jsonBytes []byte, projectDir string) (Target, bool) {
	t.Helper()
	terminals, ok := decodeOrcaTerminals(jsonBytes)
	if !ok {
		t.Fatal("decodeOrcaTerminals failed on the fixture")
	}
	return selectClaudeOrcaTerminal(terminals, projectDir)
}

func TestSelectClaudeOrcaTerminal_FindsSpawnTitleLoop_RealPayload(t *testing.T) {
	// THE BUG: a fleetops-spawned loop (title "mctl loop", connected+writable)
	// in the fleetops worktree must be ACTUABLE. orca reports the static
	// create-time --title, never "✳", so the old ✳-only check returned
	// not-found — killing kill/inject/resume/approve for every spawned loop.
	target, ok := locateClaudeFromJSON(t, []byte(realSpawnedLoopFixture), "-Users-imac-IdeaProjects-fleetops")
	if !ok {
		t.Fatal("expected ok=true — the spawnTitle loop is a confirmed Claude surface and must be found")
	}
	want := Target{Backend: "orca", ID: "term_loop", Cwd: "/Users/imac/IdeaProjects/fleetops"}
	if target != want {
		t.Errorf("got %+v, want %+v (the \"mctl loop\" tab, unambiguously — the bare shell must not compete)", target, want)
	}
}

func TestSelectClaudeOrcaTerminal_AcceptsSpawnTitle(t *testing.T) {
	terminals := []orcaTerminal{
		{Handle: "term_loop", WorktreePath: "/home/user/myproject", Title: spawnTitle, Connected: true, Writable: true, LastOutputAt: 1},
	}
	target, ok := selectClaudeOrcaTerminal(terminals, "-home-user-myproject")
	if !ok {
		t.Fatal("expected ok=true — spawnTitle is a confirmed Claude surface")
	}
	if target.ID != "term_loop" {
		t.Errorf("got ID %q, want term_loop", target.ID)
	}
}

func TestSelectClaudeOrcaTerminal_AcceptsTakeOverTitle(t *testing.T) {
	// take-over terminals (OpenTerminal, title "mctl take-over") are confirmed
	// Claude surfaces too — fleetops launched the claude command in them.
	terminals := []orcaTerminal{
		{Handle: "term_takeover", WorktreePath: "/home/user/myproject", Title: takeOverTitle, Connected: true, Writable: true, LastOutputAt: 1},
	}
	target, ok := selectClaudeOrcaTerminal(terminals, "-home-user-myproject")
	if !ok {
		t.Fatal("expected ok=true — takeOverTitle is a confirmed Claude surface")
	}
	if target.ID != "term_takeover" {
		t.Errorf("got ID %q, want term_takeover", target.ID)
	}
}

func TestSelectClaudeOrcaTerminal_BareShellTitle_Rejected(t *testing.T) {
	// The safety property: a bare shell tab (title:"") sharing the worktree
	// must NEVER be actuated — a prompt sent there executes as a shell command.
	terminals := []orcaTerminal{
		{Handle: "term_shell", WorktreePath: "/home/user/myproject", Title: "", Connected: true, Writable: true, LastOutputAt: 999},
	}
	if _, ok := selectClaudeOrcaTerminal(terminals, "-home-user-myproject"); ok {
		t.Error("expected ok=false — a bare-shell (title:\"\") tab is not a Claude surface, must never actuate")
	}
}

func TestSelectClaudeOrcaTerminal_SpawnTitleAmbiguity_Refuses(t *testing.T) {
	// Two confirmed-Claude terminals (both spawnTitle) at the same worktree —
	// the exactly-one guard must still refuse rather than pick the freshest.
	terminals := []orcaTerminal{
		{Handle: "term_a", WorktreePath: "/home/user/myproject", Title: spawnTitle, Connected: true, Writable: true, LastOutputAt: 1},
		{Handle: "term_b", WorktreePath: "/home/user/myproject", Title: spawnTitle, Connected: true, Writable: true, LastOutputAt: 999},
	}
	if _, ok := selectClaudeOrcaTerminal(terminals, "-home-user-myproject"); ok {
		t.Error("expected ok=false — two confirmed-Claude matches is ambiguous, must refuse")
	}
}

func TestSelectSpawnedOrcaTerminal_PicksSpawnTitleAtCwd(t *testing.T) {
	terminals := []orcaTerminal{
		{Handle: "term_wrong_cwd", WorktreePath: "/x/other", Title: spawnTitle, LastOutputAt: 999},
		{Handle: "term_wrong_title", WorktreePath: "/x/myproject", Title: "Terminal 2", LastOutputAt: 999},
		{Handle: "term_match", WorktreePath: "/x/myproject", Title: spawnTitle, LastOutputAt: 1},
	}
	target, ok := selectSpawnedOrcaTerminal(terminals, "/x/myproject")
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
		{Handle: "term_relabeled", WorktreePath: "/x/myproject", Title: "✳ team", LastOutputAt: 1},
	}
	target, ok := selectSpawnedOrcaTerminal(terminals, "/x/myproject")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if target.ID != "term_relabeled" {
		t.Errorf("got ID %q, want term_relabeled", target.ID)
	}
}

func TestSelectSpawnedOrcaTerminal_AlsoMatchesTakeOverTitle(t *testing.T) {
	// After unifying on isClaudeSurfaceTitle, the spawn re-finder also accepts
	// takeOverTitle — harmless and consistent: a take-over terminal is a
	// confirmed Claude surface, and the freshest match still wins.
	terminals := []orcaTerminal{
		{Handle: "term_takeover", WorktreePath: "/x/myproject", Title: takeOverTitle, LastOutputAt: 1},
	}
	target, ok := selectSpawnedOrcaTerminal(terminals, "/x/myproject")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if target.ID != "term_takeover" {
		t.Errorf("got ID %q, want term_takeover", target.ID)
	}
}

func TestSelectSpawnedOrcaTerminal_PicksNewestAmongMatches(t *testing.T) {
	terminals := []orcaTerminal{
		{Handle: "term_older", WorktreePath: "/x/myproject", Title: spawnTitle, LastOutputAt: 100},
		{Handle: "term_newer", WorktreePath: "/x/myproject", Title: spawnTitle, LastOutputAt: 200},
	}
	target, ok := selectSpawnedOrcaTerminal(terminals, "/x/myproject")
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
	if _, ok := selectSpawnedOrcaTerminal(terminals, "/x/myproject"); ok {
		t.Error("expected ok=false when no terminal matches cwd")
	}
}

// realWorktreeCreateFixture is the VERIFIED live response captured from
// a real machine (`orca worktree create --repo "path:/home/user/myproject"
// --name mctl-wt-probe --agent claude --prompt "..." --json`) — result.worktree.path
// is the confirmed, primary source extractWorktreePath must use.
const realWorktreeCreateFixture = `{
	"id": "c30a6466-...",
	"ok": true,
	"result": {
		"worktree": {
			"id": "2727286c-...::/home/user/myproject::workspace:592cd765-...",
			"instanceId": "592cd765-...",
			"repoId": "2727286c-...",
			"hostId": "local",
			"path": "/home/user/myproject",
			"head": "",
			"branch": "",
			"isBare": false,
			"isMainWorktree": false,
			"displayName": "mctl-wt-probe",
			"createdWithAgent": "claude",
			"workspaceStatus": "in-progress"
		}
	}
}`

func TestExtractWorktreePath_RealLiveFixture_SharedWorkspace(t *testing.T) {
	// the shared-workspace case: a path-registered ("folder") repo — path
	// comes back equal to the repo cwd, branch/head empty. Still a valid,
	// confirmed path — extractWorktreePath doesn't itself decide
	// shared-vs-isolated (that's the tui's spawnCmd, comparing against
	// repoCwd), it just needs to extract this path correctly.
	got := extractWorktreePath([]byte(realWorktreeCreateFixture))
	want := "/home/user/myproject"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestExtractWorktreePath_PlausibleFixture_WorktreePathKey(t *testing.T) {
	fixture := []byte(`{"ok":true,"result":{"agentTerminalHandle":"term_new123","worktree":{"worktreePath":"/home/user/orca/worktrees/mctl-fix-the-bug"}}}`)
	got := extractWorktreePath(fixture)
	want := "/home/user/orca/worktrees/mctl-fix-the-bug"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestExtractWorktreePath_PlausibleFixture_PlainPathKey(t *testing.T) {
	fixture := []byte(`{"ok":true,"result":{"path":"/home/user/orca/worktrees/mctl-fix-the-bug","startupTerminal":{"handle":"term_old456"}}}`)
	got := extractWorktreePath(fixture)
	want := "/home/user/orca/worktrees/mctl-fix-the-bug"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestExtractWorktreePath_NestedCheckoutPathKey(t *testing.T) {
	fixture := []byte(`{"ok":true,"result":{"worktree":{"repo":{"checkoutPath":"/home/user/orca/worktrees/mctl-fix-the-bug"}}}}`)
	got := extractWorktreePath(fixture)
	want := "/home/user/orca/worktrees/mctl-fix-the-bug"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestExtractWorktreePath_ShapelessFixture_ReturnsEmpty(t *testing.T) {
	// no key resembling path/worktreePath/checkoutPath anywhere, and no
	// value looks like an absolute path — spawn still succeeded, just no
	// path could be extracted.
	fixture := []byte(`{"ok":true,"result":{"agentTerminalHandle":"term_new123","status":"launched"}}`)
	got := extractWorktreePath(fixture)
	if got != "" {
		t.Errorf("got %q, want empty (no plausible path found)", got)
	}
}

func TestExtractWorktreePath_RelativeLookingValueRejected(t *testing.T) {
	// a "path"-keyed value that doesn't look absolute must not be returned.
	fixture := []byte(`{"ok":true,"result":{"path":"relative/not/absolute"}}`)
	if got := extractWorktreePath(fixture); got != "" {
		t.Errorf("got %q, want empty (value doesn't look like an absolute path)", got)
	}
}

func TestExtractWorktreePath_GarbageJSON_ReturnsEmpty(t *testing.T) {
	if got := extractWorktreePath([]byte("not json")); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestExtractWorktreePath_NoResultField_ReturnsEmpty(t *testing.T) {
	if got := extractWorktreePath([]byte(`{"ok":true}`)); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}
