package control

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// --- actuation argv shape (verified on cmux 0.64.15) ---
//
// Each builder must append `--window <ref>` when a window ref is supplied
// (Target.Window, set by cmux's Locate/LocateClaude for a cross-workspace
// surface) and omit it entirely when empty (same-caller-workspace target, or
// orca/tmux). The window flag sits among the other flags, before the trailing
// positional (the "--"/text for send, the key for send-key); focus-panel has
// no trailing positional.

func TestCmuxResumeCmd_NoWindow_OmitsFlag(t *testing.T) {
	got := cmuxResumeCmd("surface:2", "", "hello world")
	want := []string{"cmux", "send", "--surface", "surface:2", "--", "hello world\n"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got[len(got)-1][len(got[len(got)-1])-1] != '\n' {
		t.Errorf("last argv element must end in \\n (Enter), got %q", got[len(got)-1])
	}
}

func TestCmuxResumeCmd_WithWindow_AppendsFlagBeforeText(t *testing.T) {
	got := cmuxResumeCmd("surface:2", "window:1", "hello world")
	want := []string{"cmux", "send", "--surface", "surface:2", "--window", "window:1", "--", "hello world\n"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestCmuxFocusCmd_NoWindow_OmitsFlag(t *testing.T) {
	got := cmuxFocusCmd("surface:2", "")
	want := []string{"cmux", "focus-panel", "--panel", "surface:2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestCmuxFocusCmd_WithWindow_AppendsFlag(t *testing.T) {
	got := cmuxFocusCmd("surface:2", "window:2")
	want := []string{"cmux", "focus-panel", "--panel", "surface:2", "--window", "window:2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestCmuxApproveCmd_NoWindow_OmitsFlag(t *testing.T) {
	got := cmuxApproveCmd("surface:2", "")
	want := []string{"cmux", "send-key", "--surface", "surface:2", "enter"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestCmuxApproveCmd_WithWindow_AppendsFlagBeforeKey(t *testing.T) {
	got := cmuxApproveCmd("surface:2", "window:1")
	want := []string{"cmux", "send-key", "--surface", "surface:2", "--window", "window:1", "enter"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestCmuxInterruptCmd_NoWindow_OmitsFlag(t *testing.T) {
	got := cmuxInterruptCmd("surface:2", "")
	want := []string{"cmux", "send-key", "--surface", "surface:2", "escape"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestCmuxInterruptCmd_WithWindow_AppendsFlagBeforeKey(t *testing.T) {
	got := cmuxInterruptCmd("surface:2", "window:2")
	want := []string{"cmux", "send-key", "--surface", "surface:2", "--window", "window:2", "escape"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestCmuxController_Spawn_ReturnsUnsupportedError(t *testing.T) {
	err := (cmuxController{}).Spawn("/x/aboard", "do the thing")
	if err == nil {
		t.Fatal("expected an error — spawn is not supported on cmux yet")
	}
}

// --- parseCmuxTree against the REAL cmux 0.64.15 shape ---

// realCmuxTree loads the captured `cmux tree --json` fixture (real shape from
// cmux 0.64.15 on a real machine).
func realCmuxTree(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "cmux_tree.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

func TestParseCmuxTree_RealFixture_CollectsTerminalSurfacesWithTTYAndWindow(t *testing.T) {
	got := parseCmuxTree(realCmuxTree(t))
	// Only terminal-type surfaces, each with its ref + tty + enclosing window
	// ref. Browser surfaces (surface:18/23, tty:null) and the top-level "active"
	// pointer (uses surface_ref, has no ref/tty of its own) must be excluded.
	// surface:15/22 live in window:1, surface:50/9 in window:2 — the window ref
	// is what actuation passes as --window to reach a cross-workspace surface.
	// Order is the array-driven walk order.
	want := []cmuxSurface{
		{ref: "surface:15", tty: "ttys008", window: "window:1"},
		{ref: "surface:22", tty: "ttys009", window: "window:1"},
		{ref: "surface:50", tty: "ttys012", window: "window:2"},
		{ref: "surface:9", tty: "ttys012", window: "window:2"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseCmuxTree = %+v, want %+v", got, want)
	}
}

func TestParseCmuxTree_WindowRef_MatchesEachSurfacesEnclosingWindow(t *testing.T) {
	// The crux of the cross-workspace fix: every surface must carry the ref of
	// the window that actually encloses it in the tree — never a sibling
	// window's ref bleeding across the windows[] boundary.
	wantWindow := map[string]string{
		"surface:15": "window:1", // window:1 / workspace:9
		"surface:22": "window:1", // window:1 / workspace:9
		"surface:50": "window:2", // window:2 / workspace:23 — different window
		"surface:9":  "window:2", // window:2 / workspace:23
	}
	for _, s := range parseCmuxTree(realCmuxTree(t)) {
		if want := wantWindow[s.ref]; s.window != want {
			t.Errorf("surface %s: window = %q, want %q", s.ref, s.window, want)
		}
	}
}

func TestParseCmuxTree_ExcludesBrowserAndActivePointer(t *testing.T) {
	for _, s := range parseCmuxTree(realCmuxTree(t)) {
		if s.ref == "surface:18" || s.ref == "surface:23" {
			t.Errorf("browser surface %q must never be collected (tty:null)", s.ref)
		}
		if s.tty == "" {
			t.Errorf("collected surface %q has empty tty — must be filtered", s.ref)
		}
	}
}

func TestParseCmuxTree_LegacyKeyFallback_StillParses(t *testing.T) {
	// A future/older shape keying the id under surface_id (not ref) must still
	// parse, as long as it's a terminal surface with a tty.
	fixture := []byte(`{"nodes":[{"surface_id":"surface:9","type":"terminal","tty":"ttys003"}]}`)
	got := parseCmuxTree(fixture)
	want := []cmuxSurface{{ref: "surface:9", tty: "ttys003"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestParseCmuxTree_KindSurfaceFallback_BareID(t *testing.T) {
	// a sibling "kind":"surface" confirms a bare, unprefixed id.
	fixture := []byte(`{"kind":"surface","id":"surface:5","type":"terminal","tty":"ttys004"}`)
	got := parseCmuxTree(fixture)
	want := []cmuxSurface{{ref: "surface:5", tty: "ttys004"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestParseCmuxTree_NonSurfaceRefsRejected(t *testing.T) {
	// pane:/workspace:/window: nodes also use a "ref" key — they must not be
	// mistaken for surfaces.
	fixture := []byte(`{"ref":"pane:13","surfaces":[{"ref":"window:1","type":"terminal","tty":"ttys001"}]}`)
	if got := parseCmuxTree(fixture); len(got) != 0 {
		t.Errorf("got %+v, want none (pane:/window: are not surfaces)", got)
	}
}

func TestParseCmuxTree_UnknownShape_NeverPanics(t *testing.T) {
	if got := parseCmuxTree([]byte(`{"foo":"bar"}`)); len(got) != 0 {
		t.Errorf("got %d surfaces, want 0", len(got))
	}
	if got := parseCmuxTree([]byte(`not json`)); got != nil {
		t.Errorf("unparseable input: got %+v, want nil", got)
	}
	if got := parseCmuxTree([]byte(`[]`)); len(got) != 0 {
		t.Errorf("empty array: got %d surfaces, want 0", len(got))
	}
	// a terminal surface missing its tty (e.g. a just-exited surface) is
	// dropped, not a panic.
	if got := parseCmuxTree([]byte(`{"ref":"surface:1","type":"terminal","tty":null}`)); len(got) != 0 {
		t.Errorf("tty:null terminal: got %+v, want none", got)
	}
}

// --- locateCmux / locateCmuxClaude join logic (resolver mocked) ---

// stubResolver returns a fixed tty→resolution map, ignoring its input — lets
// the join logic be tested without shelling out to ps/lsof.
func stubResolver(m map[string]ttyResolution) ttyResolver {
	return func([]string) map[string]ttyResolution { return m }
}

func TestLocateCmux_TerminalSurfaceMatchingCwd_Found(t *testing.T) {
	resolve := stubResolver(map[string]ttyResolution{
		"ttys008": {cwd: "/Users/imac/IdeaProjects/aboard", hasClaude: true},
	})
	got, ok := locateCmux(realCmuxTree(t), "-Users-imac-IdeaProjects-aboard", resolve)
	if !ok {
		t.Fatal("expected ok=true")
	}
	want := Target{Backend: "cmux", ID: "surface:15", Cwd: "/Users/imac/IdeaProjects/aboard", Window: "window:1"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestLocateCmux_NoTTYResolves_NotFound(t *testing.T) {
	// resolver knows nothing (e.g. ps/lsof probe failed) — degrade to
	// not-found, never a stale/guessed target.
	got, ok := locateCmux(realCmuxTree(t), "-Users-imac-IdeaProjects-aboard", stubResolver(nil))
	if ok {
		t.Errorf("expected ok=false, got %+v", got)
	}
}

func TestLocateCmux_BrowserSurfaceNeverReturned(t *testing.T) {
	// Even if some browser tab's directory were probed, browser surfaces carry
	// no tty and are dropped by the parser — Locate can only ever return a
	// terminal surface ref.
	resolve := stubResolver(map[string]ttyResolution{
		"ttys009": {cwd: "/Users/imac/IdeaProjects/aboard", hasClaude: false},
	})
	got, ok := locateCmux(realCmuxTree(t), "-Users-imac-IdeaProjects-aboard", resolve)
	if !ok {
		t.Fatal("expected ok=true (matched the ttys009 terminal surface)")
	}
	if got.ID != "surface:22" {
		t.Errorf("got ID %q, want surface:22 (never a browser ref)", got.ID)
	}
}

func TestLocateCmuxClaude_SingleClaudeSurface_Found(t *testing.T) {
	resolve := stubResolver(map[string]ttyResolution{
		"ttys008": {cwd: "/Users/imac/IdeaProjects/aboard", hasClaude: true},
	})
	got, ok := locateCmuxClaude(realCmuxTree(t), "-Users-imac-IdeaProjects-aboard", resolve)
	if !ok {
		t.Fatal("expected ok=true — exactly one claude surface matches")
	}
	want := Target{Backend: "cmux", ID: "surface:15", Cwd: "/Users/imac/IdeaProjects/aboard", Window: "window:1"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestLocateCmuxClaude_NoClaudeAttached_NotFound_ButLocateFinds(t *testing.T) {
	// A bare shell surface in the loop's dir: Locate (permissive) matches it,
	// LocateClaude must NOT — driving keystrokes into a bare shell would run
	// them as shell commands (the wrong-terminal hazard LocateClaude guards).
	resolve := stubResolver(map[string]ttyResolution{
		"ttys008": {cwd: "/Users/imac/IdeaProjects/aboard", hasClaude: false},
	})
	if _, ok := locateCmuxClaude(realCmuxTree(t), "-Users-imac-IdeaProjects-aboard", resolve); ok {
		t.Error("expected ok=false — no claude process attached to the surface")
	}
	if _, ok := locateCmux(realCmuxTree(t), "-Users-imac-IdeaProjects-aboard", resolve); !ok {
		t.Error("expected Locate ok=true — a bare shell in the dir is a fine attach target")
	}
}

func TestLocateCmuxClaude_TwoDistinctClaudeTTYs_Refuses_ButLocateReturnsOne(t *testing.T) {
	// Two DIFFERENT terminals (ttys008, ttys009) both running claude in the
	// same dir — genuinely ambiguous, so LocateClaude refuses. Locate stays
	// permissive and returns the first match (same tier split as orca).
	resolve := stubResolver(map[string]ttyResolution{
		"ttys008": {cwd: "/Users/imac/IdeaProjects/aboard", hasClaude: true},
		"ttys009": {cwd: "/Users/imac/IdeaProjects/aboard", hasClaude: true},
	})
	if _, ok := locateCmuxClaude(realCmuxTree(t), "-Users-imac-IdeaProjects-aboard", resolve); ok {
		t.Error("expected ok=false — two distinct claude ttys is ambiguous, must refuse")
	}
	got, ok := locateCmux(realCmuxTree(t), "-Users-imac-IdeaProjects-aboard", resolve)
	if !ok {
		t.Fatal("expected Locate ok=true")
	}
	if got.ID != "surface:15" {
		t.Errorf("got ID %q, want surface:15 (first match in walk order)", got.ID)
	}
}

func TestLocateCmuxClaude_SameTTYTwoSurfaces_NotAmbiguous(t *testing.T) {
	// The real-data case: surface:50 and surface:9 both report ttys012 — the
	// SAME pty listed as two surfaces. Every ref drives the same claude, so
	// this is NOT the wrong-terminal hazard: LocateClaude must return ONE, not
	// refuse. Ambiguity is counted per distinct tty, not per surface ref.
	resolve := stubResolver(map[string]ttyResolution{
		"ttys012": {cwd: "/Users/imac/IdeaProjects/team", hasClaude: true},
	})
	got, ok := locateCmuxClaude(realCmuxTree(t), "-Users-imac-IdeaProjects-team", resolve)
	if !ok {
		t.Fatal("expected ok=true — one distinct tty (two mirrored surface refs)")
	}
	if got.ID != "surface:50" {
		t.Errorf("got ID %q, want surface:50 (first surface on ttys012)", got.ID)
	}
	if got.Cwd != "/Users/imac/IdeaProjects/team" {
		t.Errorf("got Cwd %q, want /Users/imac/IdeaProjects/team", got.Cwd)
	}
	// surface:50 lives in window:2 (a DIFFERENT window than surface:15's
	// window:1) — the Target must carry that window ref so actuation can pass
	// --window and reach it even from a caller in another workspace.
	if got.Window != "window:2" {
		t.Errorf("got Window %q, want window:2 (surface:50's enclosing window)", got.Window)
	}
}

func TestLocateCmuxClaude_DotContainingPath_Matches(t *testing.T) {
	// encodeCwd maps both "/" and "." to "-" — a dot-containing project path
	// must actuate, not degrade.
	resolve := stubResolver(map[string]ttyResolution{
		"ttys008": {cwd: "/Users/imac/.claude-mem/observer-sessions", hasClaude: true},
	})
	got, ok := locateCmuxClaude(realCmuxTree(t), "-Users-imac--claude-mem-observer-sessions", resolve)
	if !ok {
		t.Fatal("expected ok=true — encodeCwd must match the dot-containing path")
	}
	if got.ID != "surface:15" {
		t.Errorf("got ID %q, want surface:15", got.ID)
	}
}

func TestLocateCmuxClaude_NoCwdMatch_NotFound(t *testing.T) {
	resolve := stubResolver(map[string]ttyResolution{
		"ttys008": {cwd: "/some/other/dir", hasClaude: true},
	})
	if _, ok := locateCmuxClaude(realCmuxTree(t), "-Users-imac-IdeaProjects-aboard", resolve); ok {
		t.Error("expected ok=false — no surface's cwd encodes to projectDir")
	}
}

// --- ps / lsof parsing + tty resolution ---

func TestParseCmuxPsRows_FiltersTTYsAndDetectsClaudeAndForeground(t *testing.T) {
	// real `ps axo tty=,stat=,pid=,comm=` shape (right-justified pid, comm may
	// be a full path or carry args).
	out := "" +
		"ttys001  Ss    1584 /usr/bin/login\n" +
		"ttys001  S     1588 -/bin/zsh\n" +
		"ttys001  S+    1626 /Users/jito/.local/bin/claude\n" +
		"ttys001  S+    1982 npm exec figma-developer-mcp --stdio\n" +
		"ttys002  S+    9001 /Users/jito/.local/bin/claude\n" +
		"??       Ss    4242 /some/daemon\n"
	got := parseCmuxPsRows(out, map[string]bool{"ttys001": true})

	want := []cmuxProc{
		{tty: "ttys001", pid: 1584, foreground: false, isClaude: false},
		{tty: "ttys001", pid: 1588, foreground: false, isClaude: false},
		{tty: "ttys001", pid: 1626, foreground: true, isClaude: true},
		{tty: "ttys001", pid: 1982, foreground: true, isClaude: false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v (ttys002 and ?? must be filtered out)", got, want)
	}
}

// TestParseCmuxPsRows_ClaudeExeDetectedAsClaude is the review fix's
// regression: a comm of "claude.exe" (live 2026-07-17, see isClaudeComm's
// doc) must set isClaude=true, same as a bare "claude".
func TestParseCmuxPsRows_ClaudeExeDetectedAsClaude(t *testing.T) {
	out := "ttys001  S+    9001 /whatever/claude.exe\n"
	got := parseCmuxPsRows(out, map[string]bool{"ttys001": true})
	want := []cmuxProc{{tty: "ttys001", pid: 9001, foreground: true, isClaude: true}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v (claude.exe must be detected as claude)", got, want)
	}
}

func TestParseCmuxPsRows_SkipsBlankAndHeaderLikeRows(t *testing.T) {
	out := "\n   \nttys001  S+  x  /bin/zsh\nttys001  S+  55 /bin/zsh\n"
	got := parseCmuxPsRows(out, map[string]bool{"ttys001": true})
	want := []cmuxProc{{tty: "ttys001", pid: 55, foreground: true, isClaude: false}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v (non-numeric pid row must be skipped)", got, want)
	}
}

func TestParseLsofPidCwds(t *testing.T) {
	out := "p1626\nfcwd\nn/Users/jito/IdeaProjects/boxman\np15961\nfcwd\nn/Users/jito/.claude-mem\n"
	got := parseLsofPidCwds(out)
	want := map[int]string{
		1626:  "/Users/jito/IdeaProjects/boxman",
		15961: "/Users/jito/.claude-mem",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestParseLsofPidCwds_IgnoresOrphanNameAndBlankLines(t *testing.T) {
	// an "n" line before any "p" line, and empty lines, must not panic or
	// misattribute.
	out := "n/orphan/before/pid\n\np42\nfcwd\nn/real/cwd\n"
	got := parseLsofPidCwds(out)
	want := map[int]string{42: "/real/cwd"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestResolveOneTTY_PrefersClaudeCwd(t *testing.T) {
	procs := []cmuxProc{
		{tty: "ttys001", pid: 1, foreground: false},                // login, cwd "/"
		{tty: "ttys001", pid: 2, foreground: false},                // shell
		{tty: "ttys001", pid: 3, foreground: true, isClaude: true}, // claude (foreground)
	}
	cwds := map[int]string{1: "/", 2: "/proj", 3: "/proj"}
	got := resolveOneTTY(procs, cwds)
	if !got.hasClaude {
		t.Error("hasClaude should be true")
	}
	if got.cwd != "/proj" {
		t.Errorf("cwd = %q, want /proj (claude's cwd)", got.cwd)
	}
}

func TestResolveOneTTY_NoClaude_UsesForegroundCwd(t *testing.T) {
	procs := []cmuxProc{
		{tty: "ttys001", pid: 1, foreground: false}, // login at "/"
		{tty: "ttys001", pid: 2, foreground: true},  // foreground shell at /proj
	}
	cwds := map[int]string{1: "/", 2: "/proj"}
	got := resolveOneTTY(procs, cwds)
	if got.hasClaude {
		t.Error("hasClaude should be false")
	}
	if got.cwd != "/proj" {
		t.Errorf("cwd = %q, want /proj (foreground shell's cwd, not login's /)", got.cwd)
	}
}

func TestResolveOneTTY_ClaudeAttachedButSubprocessForeground_StaysConfirmed(t *testing.T) {
	// A tool subprocess momentarily holds the foreground; claude is backgrounded
	// but still attached. hasClaude must stay true, and the claude cwd is used.
	procs := []cmuxProc{
		{tty: "ttys001", pid: 1, foreground: false, isClaude: true}, // claude, backgrounded
		{tty: "ttys001", pid: 2, foreground: true},                  // tool subprocess (bash) foreground
	}
	cwds := map[int]string{1: "/proj", 2: "/proj/subdir"}
	got := resolveOneTTY(procs, cwds)
	if !got.hasClaude {
		t.Error("hasClaude must stay true when claude is attached but not foreground")
	}
	if got.cwd != "/proj" {
		t.Errorf("cwd = %q, want /proj (claude's own cwd wins)", got.cwd)
	}
}

func TestFoldTTYResolutions_GroupsByTTY(t *testing.T) {
	procs := []cmuxProc{
		{tty: "ttys001", pid: 1, foreground: true, isClaude: true},
		{tty: "ttys002", pid: 2, foreground: true},
	}
	cwds := map[int]string{1: "/a", 2: "/b"}
	got := foldTTYResolutions(procs, cwds)
	want := map[string]ttyResolution{
		"ttys001": {cwd: "/a", hasClaude: true},
		"ttys002": {cwd: "/b", hasClaude: false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestTTYsOfSurfaces_DistinctInOrder(t *testing.T) {
	got := ttysOfSurfaces([]cmuxSurface{
		{ref: "surface:50", tty: "ttys012"},
		{ref: "surface:9", tty: "ttys012"}, // duplicate tty collapses
		{ref: "surface:15", tty: "ttys008"},
	})
	want := []string{"ttys012", "ttys008"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
