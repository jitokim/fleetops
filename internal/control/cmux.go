package control

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// availabilityTimeout bounds liveness/listing probes so the TUI never hangs
// on a wedged multiplexer.
const availabilityTimeout = 2 * time.Second

// cmuxController drives a cmux terminal surface via the cmux CLI.
type cmuxController struct{}

func (cmuxController) Name() string { return "cmux" }

func (cmuxController) Available() bool {
	if _, err := exec.LookPath("cmux"); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), availabilityTimeout)
	defer cancel()
	return exec.CommandContext(ctx, "cmux", "ping").Run() == nil
}

func (cmuxController) Locate(projectDir string) (Target, bool) {
	out, err := cmuxTreeJSON()
	if err != nil {
		return Target{}, false
	}
	return locateCmux(out, projectDir, liveResolveCmuxTTYs)
}

// LocateClaude returns a cmux surface confirmed to be running `claude` — now
// IMPLEMENTED (it previously always returned not-found, before cmux's tree
// shape and the tty→cwd cross-reference were verified). cmux's `tree --json`
// carries no running-command field (nor any cwd), so confirmation is done
// out-of-band: each terminal surface's tty is cross-referenced against the OS
// process table for a live claude process (see liveResolveCmuxTTYs). Returns
// the SOLE claude surface matching projectDir, refusing on genuine ambiguity
// — the same wrong-terminal backstop selectClaudeOrcaTerminal enforces for
// orca (see locateCmuxClaude and Controller.LocateClaude).
func (cmuxController) LocateClaude(projectDir string) (Target, bool) {
	out, err := cmuxTreeJSON()
	if err != nil {
		return Target{}, false
	}
	return locateCmuxClaude(out, projectDir, liveResolveCmuxTTYs)
}

func (cmuxController) Resume(t Target, prompt string) error {
	return runWithTimeout(cmuxResumeCmd(t.ID, t.Window, prompt))
}

// cmuxResumeCmd builds the argv that re-sends prompt to a surface and submits
// it in one call ("\n" sends Enter): cmux send --surface <ref> [--window <ref>]
// -- "<prompt>\n". The optional --window is what lets a surface outside the
// caller's own workspace be reached (see appendCmuxWindow).
func cmuxResumeCmd(surfaceRef, windowRef, prompt string) []string {
	argv := appendCmuxWindow([]string{"cmux", "send", "--surface", surfaceRef}, windowRef)
	return append(argv, "--", prompt+"\n")
}

// appendCmuxWindow appends `--window <ref>` to a cmux actuation argv when
// windowRef is non-empty, and returns argv unchanged when it is empty. cmux
// resolves a `--surface`/`--panel` ref within a window context; with --window
// omitted that context defaults to the CALLER's own workspace
// ($CMUX_WORKSPACE_ID), so a surface in any OTHER workspace fails — verified
// live on cmux 0.64.15: cross-workspace `send`/`send-key`/`focus-panel` all
// fail without it ("Surface not found" / "Surface is not a terminal") and
// succeed with it, while a same-workspace target accepts --window as a no-op.
// windowRef is Target.Window: cmux populates it, orca/tmux leave it "" so their
// argv is never touched.
func appendCmuxWindow(argv []string, windowRef string) []string {
	if windowRef == "" {
		return argv
	}
	return append(argv, "--window", windowRef)
}

// Approve accepts claude's default highlighted option at a gate by sending
// a bare Enter key (distinct from Resume's `send`, which types literal
// text) targeted at the surface.
//
// Verified on cmux 0.64.15: `send-key --surface <ref> <key>` is a real
// subcommand and `enter` is a documented key token (`cmux send-key --help`
// shows `cmux send-key enter`). NOT exercised end-to-end against a live
// claude-in-cmux gate (none available to drive safely), so the semantic
// effect — Enter accepts the default — is contract-level, not runtime-tested.
func (cmuxController) Approve(t Target) error {
	return runWithTimeout(cmuxApproveCmd(t.ID, t.Window))
}

// cmuxApproveCmd builds the argv for a bare Enter keypress into a surface:
// cmux send-key --surface <ref> [--window <ref>] enter (see appendCmuxWindow
// for why --window is needed to reach a cross-workspace surface).
func cmuxApproveCmd(surfaceRef, windowRef string) []string {
	argv := appendCmuxWindow([]string{"cmux", "send-key", "--surface", surfaceRef}, windowRef)
	return append(argv, "enter")
}

func (cmuxController) Focus(t Target) error {
	return runWithTimeout(cmuxFocusCmd(t.ID, t.Window))
}

// cmuxFocusCmd builds the argv that brings a cmux surface to the front:
// focus-panel is the contract's compatibility alias over surface focus. The
// optional --window reaches a surface outside the caller's own workspace (see
// appendCmuxWindow) — reproduced live: `focus-panel --panel surface:N` alone
// fails "not_found: Surface not found" when surface:N is in another workspace,
// and succeeds once `--window <ref>` is added.
func cmuxFocusCmd(surfaceRef, windowRef string) []string {
	return appendCmuxWindow([]string{"cmux", "focus-panel", "--panel", surfaceRef}, windowRef)
}

// Spawn is not supported on cmux yet — creating a brand new surface running
// claude hasn't been verified against the real cmux CLI (unlike the other
// actions here, which at least have a plausible/partially-verified
// contract). Fail explicitly rather than guess at a create-surface command.
//
// Bug 1 note for whoever implements this: cmux's own CLI source (inspected
// directly — no `cmux` binary was installed on the machine this was
// investigated on, so this is NOT live-verified the way the rest of this
// file's "verified live" comments are) shows `new-workspace`/`new-surface`/
// `new-pane` all carrying a `--focus <true|false>` flag, defaulting
// (per the same source) to NOT switching focus — i.e. the cmux analog of
// tmux's `-d` fix (tmux.go's tmuxNewWindowCmd) may already be a non-issue by
// default here too, same shape as orca's Spawn (see orca.go). Re-verify
// against a real cmux instance before relying on this when Spawn gets
// implemented.
func (cmuxController) Spawn(cwd, goal string) error {
	return fmt.Errorf("spawn not supported on cmux yet")
}

// Interrupt stops the current turn without killing claude — a bare Escape.
//
// Verified on cmux 0.64.15: `send-key --surface <ref> <key>` exists (see
// Approve). The `escape` key TOKEN is by convention — cmux's `send-key --help`
// examples show `enter`/`ctrl+c` but not `escape` — and no live claude-in-cmux
// turn was available to confirm Esc interrupts (rather than kills), so this
// remains assumed, not runtime-tested.
func (cmuxController) Interrupt(t Target) error {
	return runWithTimeout(cmuxInterruptCmd(t.ID, t.Window))
}

// cmuxInterruptCmd builds the argv for an Escape keypress into a surface:
// cmux send-key --surface <ref> [--window <ref>] escape (see appendCmuxWindow
// for why --window is needed to reach a cross-workspace surface).
func cmuxInterruptCmd(surfaceRef, windowRef string) []string {
	argv := appendCmuxWindow([]string{"cmux", "send-key", "--surface", surfaceRef}, windowRef)
	return append(argv, "escape")
}

// cmuxTreeJSON runs `cmux tree --json`, bounded by availabilityTimeout so a
// wedged cmux never hangs a keypress.
func cmuxTreeJSON() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), availabilityTimeout)
	defer cancel()
	return exec.CommandContext(ctx, "cmux", "tree", "--json").Output()
}

// cmuxSurface is one terminal-type surface from `cmux tree --json`: its stable
// ref ("surface:<n>"), the tty it is attached to ("ttys008"), and the ref of
// the window that encloses it ("window:1"). cmux's tree carries NO cwd anywhere
// (verified against the full 26KB dump on cmux 0.64.15 — zero cwd/path-like
// keys), so a surface's cwd is resolved out-of-band from the OS by tty (see
// liveResolveCmuxTTYs), not read here. The window ref IS in the tree (the
// windows[] array element's "ref"), captured while walking so actuation can
// pass `--window` and reach surfaces outside the caller's own workspace (see
// appendCmuxWindow).
type cmuxSurface struct {
	ref    string
	tty    string
	window string
}

// ttyResolution is one tty's OS-derived facts: the representative cwd of the
// surface attached to it, and whether a live `claude` process is attached.
type ttyResolution struct {
	cwd       string
	hasClaude bool
}

// ttyResolver maps a set of ttys to their resolutions. Injected into the
// tree-join logic (locateCmux/locateCmuxClaude) so that logic is unit-testable
// against a fixture without shelling out to ps/lsof — the live implementation
// is liveResolveCmuxTTYs.
type ttyResolver func(ttys []string) map[string]ttyResolution

// locateCmux returns the first terminal surface whose OS-resolved cwd encodes
// to projectDir — permissive (a bare shell in the right dir is a fine attach
// target), the same tier split orca's Locate uses. Typed/destructive
// actuation must use locateCmuxClaude instead.
func locateCmux(jsonBytes []byte, projectDir string, resolve ttyResolver) (Target, bool) {
	surfaces := parseCmuxTree(jsonBytes)
	res := resolve(ttysOfSurfaces(surfaces))
	for _, s := range surfaces {
		r, ok := res[s.tty]
		if !ok || r.cwd == "" {
			continue
		}
		if encodeCwd(r.cwd) == projectDir {
			return Target{Backend: "cmux", ID: s.ref, Cwd: r.cwd, Window: s.window}, true
		}
	}
	return Target{}, false
}

// locateCmuxClaude returns the SOLE surface confirmed to be running claude (a
// live claude process attached to its tty) whose cwd encodes to projectDir.
// Ambiguity is counted by distinct TTY, not by surface ref: cmux can list the
// same terminal as more than one surface sharing a tty (verified live: two
// distinct surface refs on ttys012), and every ref on one tty drives the SAME
// pty — so that is NOT the wrong-terminal hazard. TWO DISTINCT ttys matching,
// though, is genuinely ambiguous (no way to know which the human meant), so
// ok=false — the authoritative backstop behind the TUI's fleet-ambiguity
// guard, mirroring selectClaudeOrcaTerminal (see Controller.LocateClaude).
func locateCmuxClaude(jsonBytes []byte, projectDir string, resolve ttyResolver) (Target, bool) {
	surfaces := parseCmuxTree(jsonBytes)
	res := resolve(ttysOfSurfaces(surfaces))
	firstByTTY := map[string]Target{}
	for _, s := range surfaces {
		r, ok := res[s.tty]
		if !ok || !r.hasClaude || r.cwd == "" {
			continue
		}
		if encodeCwd(r.cwd) != projectDir {
			continue
		}
		if _, seen := firstByTTY[s.tty]; !seen {
			firstByTTY[s.tty] = Target{Backend: "cmux", ID: s.ref, Cwd: r.cwd, Window: s.window}
		}
	}
	if len(firstByTTY) != 1 {
		return Target{}, false // 0 matches, or >1 distinct tty (ambiguous)
	}
	for _, t := range firstByTTY {
		return t, true
	}
	return Target{}, false
}

// LocateByTTY finds the cmux surface whose controlling tty matches tty (as
// recorded by the session registry, e.g. "ttys012") AND has a live claude
// process attached — the ADR Phase 2 tty-dispatch path (see
// ResolveActuationTarget and control.TTYLocator). cmux's `tree --json`
// already carries a per-surface tty directly (verified live on cmux
// 0.64.15 — see cmuxSurfaceTTY/parseCmuxTree's doc), so this reuses the
// SAME liveResolveCmuxTTYs cross-reference locateCmuxClaude already uses to
// confirm a live claude process is attached — a bare tty match alone isn't
// enough (the tty could be hosting a plain shell, same reasoning as
// LocateClaude vs Locate). tty is session-unique (unlike cwd), so — same as
// tmuxController.LocateByTTY — this does NOT apply the cwd-path's ambiguity
// refusal: at most one live surface can have a given controlling tty at any
// moment.
func (cmuxController) LocateByTTY(tty string) (Target, bool) {
	out, err := cmuxTreeJSON()
	if err != nil {
		return Target{}, false
	}
	return locateCmuxByTTY(parseCmuxTree(out), tty, liveResolveCmuxTTYs)
}

// locateCmuxByTTY is LocateByTTY's pure selection core, pulled out so the
// tty-matching logic is directly unit-testable against a fixture without a
// real cmux binary (same pattern as locateCmux/locateCmuxClaude). Multiple
// surfaces can share one tty (the SAME pty listed as more than one surface
// ref, verified live — see locateCmuxClaude's doc); any one of them is a
// correct answer, so the first tty+claude match wins.
func locateCmuxByTTY(surfaces []cmuxSurface, tty string, resolve ttyResolver) (Target, bool) {
	want := normalizeTTY(tty)
	res := resolve(ttysOfSurfaces(surfaces))
	for _, s := range surfaces {
		if normalizeTTY(s.tty) != want {
			continue
		}
		if r, ok := res[s.tty]; ok && r.hasClaude {
			return Target{Backend: "cmux", ID: s.ref, Cwd: r.cwd, Window: s.window}, true
		}
	}
	return Target{}, false
}

// ttysOfSurfaces returns the distinct ttys across surfaces — the input a
// ttyResolver scopes its ps/lsof pass to, so resolution touches only the ttys
// cmux actually reported.
func ttysOfSurfaces(surfaces []cmuxSurface) []string {
	seen := map[string]bool{}
	var ttys []string
	for _, s := range surfaces {
		if s.tty != "" && !seen[s.tty] {
			seen[s.tty] = true
			ttys = append(ttys, s.tty)
		}
	}
	return ttys
}

// parseCmuxTree tolerantly walks `cmux tree --json`, collecting every
// terminal-type surface as {ref, tty}. Unknown shape → empty slice, never
// panics (every type assertion is comma-ok).
//
// Verified against the REAL cmux 0.64.15 CLI on this machine (only — not all
// cmux versions): a surface's identity is its "ref" key ("surface:<n>"),
// terminal surfaces carry "type":"terminal" + "tty":"ttys<NNN>", browser
// surfaces carry "type":"browser" + "tty":null, and the structure is
// windows[].workspaces[].panes[].surfaces[]. The older guessed id keys
// (surfaceId/surface_id/id) are still accepted as fallbacks (see
// cmuxSurfaceID) so a differing shape degrades rather than regresses.
func parseCmuxTree(jsonBytes []byte) []cmuxSurface {
	var root any
	if err := json.Unmarshal(jsonBytes, &root); err != nil {
		return nil
	}
	var surfaces []cmuxSurface
	walkCmuxNode(root, "", &surfaces)
	return surfaces
}

// walkCmuxNode descends the tree carrying the enclosing window ref: whenever a
// map node is itself a window (its "ref" is "window:<n>", the windows[] array
// element), that ref becomes the window context for its whole subtree — so each
// surface is tagged with the window that actually contains it. window is passed
// by value, so sibling windows never bleed into each other's subtrees.
func walkCmuxNode(node any, window string, out *[]cmuxSurface) {
	switch v := node.(type) {
	case map[string]any:
		if ref, ok := v["ref"].(string); ok && strings.HasPrefix(ref, "window:") {
			window = ref
		}
		if s, ok := cmuxSurfaceFromNode(v); ok {
			s.window = window
			*out = append(*out, s)
		}
		for _, child := range v {
			walkCmuxNode(child, window, out)
		}
	case []any:
		for _, child := range v {
			walkCmuxNode(child, window, out)
		}
	}
}

// cmuxSurfaceFromNode extracts a terminal surface (ref + tty) from a node, or
// ok=false when the node isn't a terminal surface: no surface ref, not
// type:"terminal", or no tty (a browser surface's tty is null).
func cmuxSurfaceFromNode(m map[string]any) (cmuxSurface, bool) {
	ref, ok := cmuxSurfaceID(m)
	if !ok {
		return cmuxSurface{}, false
	}
	tty, ok := cmuxSurfaceTTY(m)
	if !ok {
		return cmuxSurface{}, false
	}
	return cmuxSurface{ref: ref, tty: tty}, true
}

// cmuxSurfaceIDKeys is the priority order cmuxSurfaceID checks for a surface's
// ref: real cmux 0.64.15's "ref" first, then the older guessed keys as
// tolerant fallbacks.
var cmuxSurfaceIDKeys = []string{"ref", "surfaceId", "surface_id", "id"}

// cmuxSurfaceID returns a node's surface ref. Real cmux 0.64.15 keys it under
// "ref" ("surface:<n>"); the guessed keys (surfaceId/surface_id/id) are kept
// as fallbacks so a differing cmux shape still parses. Non-surface refs
// (pane:/workspace:/window:, which also use "ref") are rejected by the
// "surface:" prefix guard; a sibling "kind":"surface" additionally confirms
// intent for a bare, unprefixed id.
func cmuxSurfaceID(m map[string]any) (string, bool) {
	for _, key := range cmuxSurfaceIDKeys {
		if s, ok := m[key].(string); ok && strings.HasPrefix(s, "surface:") {
			return s, true
		}
	}
	if kind, _ := m["kind"].(string); kind == "surface" {
		for _, key := range cmuxSurfaceIDKeys {
			if s, ok := m[key].(string); ok && s != "" {
				return s, true
			}
		}
	}
	return "", false
}

// cmuxSurfaceTTY returns a terminal surface's tty ("ttys<NNN>", no /dev/
// prefix — the exact token `ps` prints in its TTY column). ok=false for any
// non-terminal surface (a browser surface's "tty" is null), so browser tabs
// are never treated as controllable claude surfaces.
func cmuxSurfaceTTY(m map[string]any) (string, bool) {
	if t, _ := m["type"].(string); t != "terminal" {
		return "", false
	}
	tty, ok := m["tty"].(string)
	if !ok || tty == "" {
		return "", false
	}
	return tty, true
}

// cmuxProc is one process attached to a tty, from `ps axo tty,stat,pid,comm`.
type cmuxProc struct {
	tty        string
	pid        int
	foreground bool // "+" in the stat column: the tty's foreground process group
	isClaude   bool
}

// liveResolveCmuxTTYs is the production ttyResolver: it cross-references the OS
// for each tty's cwd, since cmux's tree carries none. One `ps` pass over the
// process table (kept only for the wanted ttys) then one batched `lsof` for
// those pids' cwds — the SAME ps→lsof pattern internal/claude/procs.go uses,
// deliberately DUPLICATED here (not imported) to keep internal/control a
// zero-internal-package pure actuation layer (same rationale documented on
// encodeCwd in control.go). Both calls share one availabilityTimeout deadline
// so the whole resolve stays inside the per-keypress budget; any probe failure
// degrades to "no cwd" (not-found), never a hang.
func liveResolveCmuxTTYs(ttys []string) map[string]ttyResolution {
	want := map[string]bool{}
	for _, t := range ttys {
		if t != "" {
			want[t] = true
		}
	}
	if len(want) == 0 {
		return map[string]ttyResolution{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), availabilityTimeout)
	defer cancel()
	psOut, err := exec.CommandContext(ctx, "ps", "axo", "tty=,stat=,pid=,comm=").Output()
	if err != nil {
		return map[string]ttyResolution{}
	}
	procs := parseCmuxPsRows(string(psOut), want)
	if len(procs) == 0 {
		return map[string]ttyResolution{}
	}
	// lsof exits NON-ZERO when ANY queried pid is inaccessible or exited
	// between the ps snapshot and here, yet still prints valid cwd records for
	// the rest — so parse its stdout regardless of exit status (Output()
	// returns the stdout it buffered even on an *exec.ExitError). This is why
	// this can't naively mirror internal/claude's "err → bail": that path only
	// ever queries a few user-owned claude pids and so rarely trips a partial
	// error, whereas a tty's full process set routinely includes one. A real
	// spawn/timeout failure yields empty output → empty map → graceful
	// not-found, never a hang.
	lsofOut, _ := exec.CommandContext(ctx, "lsof", "-a", "-p", pidCSV(procs), "-d", "cwd", "-Fpn").Output()
	return foldTTYResolutions(procs, parseLsofPidCwds(string(lsofOut)))
}

// parseCmuxPsRows parses `ps axo tty=,stat=,pid=,comm=` output into the
// processes on the wanted ttys. Columns: tty, stat, pid, comm — comm may
// itself contain spaces (kept whole, matched on filepath.Base, exactly like
// internal/claude.parsePsClaudePids). Rows on other ttys, on no tty ("??"),
// and unparseable rows are skipped, not treated as errors.
func parseCmuxPsRows(out string, want map[string]bool) []cmuxProc {
	var procs []cmuxProc
	for _, raw := range strings.Split(out, "\n") {
		tty, rest, ok := cutField(raw)
		if !ok || !want[tty] {
			continue
		}
		stat, rest, ok := cutField(rest)
		if !ok {
			continue
		}
		pidField, comm, ok := cutField(rest)
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(pidField)
		if err != nil {
			continue
		}
		procs = append(procs, cmuxProc{
			tty:        tty,
			pid:        pid,
			foreground: strings.Contains(stat, "+"),
			isClaude:   isClaudeComm(comm),
		})
	}
	return procs
}

// cutField trims leading whitespace and splits off the first
// whitespace-delimited field, returning it plus the trimmed remainder.
// ok=false for a blank line. Used to walk fixed-column ps output left to
// right while leaving the trailing field (comm) — which may contain spaces —
// intact.
func cutField(line string) (field, rest string, ok bool) {
	line = strings.TrimLeft(line, " \t")
	if line == "" {
		return "", "", false
	}
	idx := strings.IndexFunc(line, unicode.IsSpace)
	if idx < 0 {
		return line, "", true
	}
	return line[:idx], strings.TrimLeft(line[idx:], " \t"), true
}

// parseLsofPidCwds parses `lsof -a -p <pids> -d cwd -Fpn` output — interleaved
// "p<pid>" / "fcwd" / "n<path>" lines — into pid → cwd. The current pid comes
// from the most recent "p" line; each "n" line sets that pid's cwd. Same
// field-prefixed format internal/claude.parseLsofCwds reads, extended to keep
// the pid association ("-Fpn" adds the "p" field).
func parseLsofPidCwds(out string) map[int]string {
	cwds := map[int]string{}
	pid, have := 0, false
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 2 {
			continue
		}
		switch line[0] {
		case 'p':
			n, err := strconv.Atoi(line[1:])
			have = err == nil
			if have {
				pid = n
			}
		case 'n':
			if have {
				cwds[pid] = line[1:]
			}
		}
	}
	return cwds
}

// foldTTYResolutions groups procs by tty and resolves each tty to one cwd +
// hasClaude (see resolveOneTTY).
func foldTTYResolutions(procs []cmuxProc, cwds map[int]string) map[string]ttyResolution {
	grouped := map[string][]cmuxProc{}
	for _, p := range procs {
		grouped[p.tty] = append(grouped[p.tty], p)
	}
	out := make(map[string]ttyResolution, len(grouped))
	for tty, ps := range grouped {
		out[tty] = resolveOneTTY(ps, cwds)
	}
	return out
}

// resolveOneTTY picks a tty's representative cwd and claude-attachment. The cwd
// prefers the claude process's own cwd (the loop's project dir is exactly
// where claude runs), then the foreground process's (a bare shell surface's
// current dir), then any process with a known cwd. hasClaude is true whenever
// ANY attached process is claude — not only when it holds the foreground — so
// a momentary tool subprocess owning the foreground can't flip a real claude
// surface to unconfirmed on a keypress.
func resolveOneTTY(procs []cmuxProc, cwds map[int]string) ttyResolution {
	var claudeCwd, fgCwd, anyCwd string
	hasClaude := false
	for _, p := range procs {
		cwd := cwds[p.pid]
		if p.isClaude {
			hasClaude = true
			if claudeCwd == "" && cwd != "" {
				claudeCwd = cwd
			}
		}
		if fgCwd == "" && p.foreground && cwd != "" {
			fgCwd = cwd
		}
		if anyCwd == "" && cwd != "" {
			anyCwd = cwd
		}
	}
	return ttyResolution{cwd: firstNonEmpty(claudeCwd, fgCwd, anyCwd), hasClaude: hasClaude}
}

// firstNonEmpty returns the first non-empty string, or "".
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// pidCSV joins procs' pids into the comma-separated form `lsof -p` accepts.
func pidCSV(procs []cmuxProc) string {
	ids := make([]string, len(procs))
	for i, p := range procs {
		ids[i] = strconv.Itoa(p.pid)
	}
	return strings.Join(ids, ",")
}
