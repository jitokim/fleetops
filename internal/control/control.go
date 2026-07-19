// Package control re-drives a stalled loop by re-sending its prompt to the
// terminal surface that hosts it — abstracted over the multiplexer
// (orca/cmux/tmux) so a fleet board on any terminal can resume a loop
// (DESIGN.md: pluggable ports). Observation works everywhere; actuation
// degrades gracefully when no backend is available (see internal/tui's
// manual resume hint).
package control

import (
	"context"
	"github.com/jitokim/fleetops/internal/domain"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// actuationTimeout bounds a single typed-action exec call (Resume/Approve/
// Focus/Interrupt) so a wedged multiplexer CLI never hangs the TUI. No
// backend's Spawn uses it, and Spawn is NOT uniformly bounded by something
// longer instead — do not read this constant as evidence that spawning is
// covered elsewhere:
//
//   - orca.go's Spawn IS bounded, per step, via exec.CommandContext
//     (spawnCreateTimeout / spawnWaitTimeout / spawnLocateTimeout /
//     spawnSendTextTimeout).
//   - tmux.go's Spawn is UNBOUNDED. It shells out with bare exec.Command and
//     no context at all, and spawnBootWait is a flat time.Sleep, not a
//     deadline — so a wedged `tmux new-window` or `send-keys` hangs that
//     goroutine indefinitely. Adding a timeout there is a behaviour change,
//     tracked separately; this comment only stops claiming otherwise.
//
// Treat a Spawn as unbounded until its own implementation shows a context,
// rather than assuming the list above stays exhaustive as backends are added.
const actuationTimeout = 5 * time.Second

// runWithTimeout runs argv[0] with argv[1:] bounded by actuationTimeout —
// the shared exec path for every backend's Resume/Approve/Focus/Interrupt,
// matching the same never-hang discipline availabilityTimeout already
// enforces on Locate/Available (see cmux.go).
func runWithTimeout(argv []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), actuationTimeout)
	defer cancel()
	return exec.CommandContext(ctx, argv[0], argv[1:]...).Run()
}

// isClaudeComm reports whether comm (a tmux pane_current_command / ps comm
// field) names a `claude` process — its base name is exactly "claude", or
// exactly "claude" once a trailing ".exe" is stripped. Observed in the
// wild: some installs report the process as
// "/whatever/claude.exe" (lsof-confirmed, origin of the binary name TBD —
// possibly a native-build install); a strict "claude" comparison made every
// tmux pane hosting it invisible to LocateByTTY/parseTmuxClaudePanes,
// misrouting or refusing actuation for it. Mirrors
// internal/claude.matchesClaudeComm exactly (duplicated rather than
// imported — internal/control and internal/claude are siblings with no
// existing dependency between them, and this is 2 lines). Deliberately NOT
// loosened to a prefix match: "claude-helper" and similar must stay
// excluded.
func isClaudeComm(comm string) bool {
	name := strings.TrimSuffix(filepath.Base(comm), ".exe")
	return name == "claude"
}

// encodeCwd applies Claude Code's own project-dir encoding to a real
// (unencoded) absolute path — both "/" AND "." become "-" (verified:
// "/home/user/.someplugin/agent-sessions" →
// "-home-user--someplugin-agent-sessions"). Deliberately duplicated
// (not imported) from internal/claude.encodeCwd — keeps this package's
// dependency footprint at zero internal packages (it's a pure actuation
// layer, DESIGN.md's pluggable-ports boundary), for a stable, already-tested
// 2-line function. Keep both copies in sync if the encoding scheme ever
// changes. Using the exact same scheme here (rather than the old bare
// "/"→"-" replace) is what lets a dot-containing project path (e.g.
// "~/.claude-mem/...") actuate via a matched surface instead of always
// degrading to a manual hint.
func encodeCwd(realPath string) string {
	return domain.EncodeCwd(realPath)
}

// Target is a controllable terminal surface hosting a loop.
type Target struct {
	Backend string // "orca" | "cmux" | "tmux"
	ID      string // orca terminal handle ("term_abc123") / cmux surface ref ("surface:2") / tmux pane id ("%3")
	Cwd     string
	// Window is the enclosing cmux window ref ("window:1") that disambiguates a
	// surface ref across workspaces. Populated ONLY by cmux's Locate/LocateClaude
	// (captured while walking `cmux tree --json`) and threaded into every cmux
	// actuation as `--window <ref>`; orca/tmux leave it empty because their
	// handles/pane-ids are already globally addressable. Without it, cmux scopes
	// a `--surface`/`--panel` ref to the CALLER's own workspace
	// ($CMUX_WORKSPACE_ID) and any target in another workspace fails (verified
	// live on cmux 0.64.15 — see appendCmuxWindow).
	Window string
}

// Controller locates and re-drives loops on one multiplexer backend.
type Controller interface {
	Name() string
	Available() bool                         // backend usable right now
	Locate(projectDir string) (Target, bool) // match ANY surface by encoded cwd — for attach/Focus, where a bare shell is a fine target
	// LocateClaude is like Locate, but returns ONLY a surface confirmed to be
	// running `claude` (never a bare shell tab that merely shares the
	// directory). Required before any typed/destructive actuation
	// (Resume/Approve/Interrupt, and the TUI's kill) — see DESIGN.md and the
	// hardening-slice P0-3 rationale: Locate's permissive multi-tier
	// fallback exists for attach, and using it for typed actions can drive
	// keystrokes into the wrong terminal.
	//
	// ok is also false when MORE THAN ONE claude surface matches projectDir
	// (e.g. two claude panes/terminals open in the same directory) — this is
	// the authoritative backstop for the same wrong-terminal hazard the
	// TUI's keypress-time fleet-ambiguity guard exists to catch (see
	// Model.refuseIfAmbiguous): the TUI check is the fast/friendly path with
	// a good error message, but LocateClaude must refuse on its own too,
	// since it's the last thing standing between a keystroke and a real
	// terminal.
	LocateClaude(projectDir string) (Target, bool)
	Resume(t Target, prompt string) error // re-send prompt + submit
	Focus(t Target) error                 // bring the surface to the front (attach)
	Approve(t Target) error               // accept the default option at a gate (bare Enter)
	Spawn(cwd, goal string) error         // start a brand new claude loop in cwd
	Interrupt(t Target) error             // stop the current turn (Esc) without killing the process
}

// WorktreeSpawner is an OPTIONAL capability a Controller may additionally
// implement — spawning a loop into a fresh, isolated worktree rather than an
// existing directory. This is orca-specific (verified against its CLI's
// one-shot `worktree create --agent` contract — see orca.go's SpawnWorktree)
// with no tmux/cmux equivalent, so it's a separate, narrow interface rather
// than a new Controller method: widening Controller for one backend's
// capability would bloat every other implementation with a stub (review
// debt P2-5 — don't bloat the interface). Callers type-assert
// (ctrl.(WorktreeSpawner)) and fall back to the ordinary Spawn/current-dir
// path when a backend doesn't implement it.
type WorktreeSpawner interface {
	SpawnWorktree(repoCwd, name, prompt string) (worktreePath string, err error)
}

// TerminalOpener is another OPTIONAL capability, same shape/reasoning as
// WorktreeSpawner (narrow interface, not a Controller method — don't bloat
// every backend with a stub): opens a FRESH terminal running an arbitrary
// command in cwd. Added for LoopEngine's take-over attach — a
// Driven (engine-owned) loop has no existing terminal surface to Locate/
// Focus into (it's a headless bootstrap loop), so handing it to a human
// means CREATING one, running `claude --resume <id>` so the human inherits
// the exact session, not Spawn's "start a brand new claude loop" (which
// begins a NEW session, wrong for take-over).
//
// orca and tmux both have a verified one-shot "run this command in a new
// terminal" primitive (orca: `terminal create --command`, reusing the exact
// create call Spawn already verified live, just swapping the fixed
// "--command claude" for an arbitrary command; tmux: `new-window
// "<command>"`, reusing tmuxNewWindowCmd's `-c cwd` shape). cmux has no
// verified equivalent one-shot command-in-new-surface primitive today, so it
// does NOT implement this — callers type-assert (same pattern as
// WorktreeSpawner's ctrl.(control.WorktreeSpawner)) and fall back to the
// manual "claude --resume <id>" hint when the resolved controller doesn't
// support it, exactly like attach's own no-backend fallback.
type TerminalOpener interface {
	OpenTerminal(cwd, command string) error
}

// TTYLocator is another OPTIONAL capability (same shape/reasoning as
// WorktreeSpawner/TerminalOpener — narrow interface, not a Controller
// method): locate a live terminal surface by its OS-level controlling tty,
// the ADR Phase 2 Tier 1a dispatch path (see actuation.go's
// ResolveActuationTarget). tty is session-unique where cwd is many-to-one
// (domain.EncodeCwd's own doc) — this is what lets N sessions sharing one
// worktree/cwd still target the RIGHT one instead of refusing on ambiguity
// or silently downgrading to Tier 2's headless re-drive.
//
// Only a backend whose CLI actually exposes a per-terminal tty (or data
// resolvable to one, e.g. cmux's tree carries tty directly; see cmux.go's
// LocateByTTY) can implement this — verified per-backend, never assumed.
// orca's `terminal list`/`terminal show --json` schema carries NO tty or
// pid field at all (confirmed live against a running orca instance, see
// docs/adr-vendor-independent-actuation.md §4's honesty ledger), so
// orcaController deliberately does NOT implement TTYLocator today. tmux
// implements it (tmuxController.LocateByTTY, pre-existing). Callers
// type-assert (same pattern as ctrl.(WorktreeSpawner)/ctrl.(TerminalOpener))
// and fall back to the cwd-based LocateClaude chain when the resolved
// backend doesn't support it.
type TTYLocator interface {
	LocateByTTY(tty string) (Target, bool)
}

// backends is the ordered, install-preference backend list every resolver in
// this package shares — orca preferred (the user's own environment), cmux then
// tmux as fallbacks. Extracted to a single package var (rather than a literal
// re-spelled inside each resolver) so Resolve/ResolveForLocate/
// ResolveActuationTarget all iterate the SAME list, and so tests can inject
// fake Controllers in one place instead of needing real orca/cmux/tmux
// binaries on the machine running them.
var backends = []Controller{orcaController{}, cmuxController{}, tmuxController{}}

// Resolve returns the first available controller: orca preferred (the
// user's own environment), cmux then tmux as fallbacks; ok is false if
// none of the three backends is available.
//
// This is the CREATION/CAPABILITY resolver — "give me some backend that can
// spawn / open a terminal / answer a capability question," where install order
// is the right tiebreak (spawnCmd, checkWorktreeEligibilityCmd, takeOverCmd).
// It deliberately does NOT consult Locate: for ATTACH (which backend actually
// hosts an EXISTING surface) use ResolveForLocate; for typed/destructive
// actuation use ResolveActuationTarget.
func Resolve() (Controller, bool) {
	for _, c := range backends {
		if c.Available() {
			return c, true
		}
	}
	return nil, false
}

// Spawner is the narrow "start a brand new claude loop in cwd" capability —
// the subset of Controller that creating a loop actually needs (Name,
// Available, Spawn), and nothing else.
//
// It exists so a host terminal that can spawn but cannot do anything else a
// Controller promises — iTerm2, which has no concept of locating a surface by
// cwd and no pane addressing — can participate in spawn WITHOUT being added to
// the `backends` slice. That distinction is the whole point: `backends` feeds
// ResolveForLocate and ResolveActuationTarget, so an entry there would join
// Tier 1b's cwd probe and its cross-backend ambiguity counting and thereby
// change actuation dispatch. A separate interface keeps the new capability
// strictly additive, the same reasoning that made WorktreeSpawner/
// TerminalOpener/TTYLocator narrow interfaces rather than Controller methods.
//
// Every Controller satisfies Spawner already, so ResolveSpawner can hand back
// either kind and callers can still type-assert for the richer capabilities
// (spawnCmd does exactly that for WorktreeSpawner).
type Spawner interface {
	Name() string
	Available() bool
	Spawn(cwd, goal string) error
}

// ResolveSpawner returns something that can start a new loop: the first
// available multiplexer Controller, or — only when there is NO multiplexer at
// all — the host-terminal spawner (iTerm2). ok is false when neither exists.
//
// Multiplexers keep strict priority, which makes this a pure superset: on any
// machine with orca/cmux/tmux available, this returns exactly what Resolve
// returns and the iTerm2 path is never reached. The only behaviour that
// changes is on a machine where spawn previously failed outright.
//
// iTerm2 is checked LAST rather than by host affinity on purpose. A loop
// spawned into a multiplexer gets pane-exact addressing for every later
// actuation (Tier 1a); one spawned into a bare iTerm2 window relies on Tier 1h
// and on the SessionStart hook having recorded its window id. The
// better-addressable surface should win whenever it exists.
func ResolveSpawner() (Spawner, bool) {
	if c, ok := Resolve(); ok {
		return c, true
	}
	if host := (iterm2Spawner{}); host.Available() {
		return host, true
	}
	return nil, false
}

// ResolveForLocate is the ATTACH resolver: it returns the first AVAILABLE
// backend that can actually Locate a surface for projectDir, together with the
// Target it located — not merely the first backend that happens to be
// installed. This closes the "orca is always Available on the captain's
// machine, so Resolve() always picks it even when the loop physically lives in
// a tmux/cmux surface orca can't Locate" hazard: attach must follow the
// surface, not the install order.
//
// It iterates backends in the same install-preference order, gating each on
// Available() BEFORE any Locate call (an unavailable backend is never probed),
// and STOPS AT THE FIRST HIT. On multiple matches (two backends both Locate a
// surface for the same projectDir) the first by order wins — permissive by
// design, mirroring Locate's own "a bare shell tab is a fine attach target"
// contract; attach NEVER refuses on ambiguity the way the typed/destructive
// path does. ok is false when no backend is available OR none can Locate.
//
// Locate-based, NEVER LocateClaude: attach is the attach-preservation path (a
// bare shell sharing the directory is a valid jump target), and the stricter
// claude-only refusal belongs only to typed/destructive actuation
// (ResolveActuationTarget).
func ResolveForLocate(projectDir string) (Controller, Target, bool) {
	for _, c := range backends {
		if !c.Available() {
			continue
		}
		if t, ok := c.Locate(projectDir); ok {
			return c, t, true
		}
	}
	return nil, Target{}, false
}
