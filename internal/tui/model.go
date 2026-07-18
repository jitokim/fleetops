// Package tui is the fleet cockpit (Bubble Tea): aggregate list + right-pane
// detail + one-key action, refreshed from the Claude Code logs (seed spec §UX).
// Visual language matches the approved mockup (html-artifacts/mission-control-tui.html).
package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/jitokim/fleetops/internal/claude"
	"github.com/jitokim/fleetops/internal/control"
	"github.com/jitokim/fleetops/internal/domain"
	"github.com/jitokim/fleetops/internal/engine"
	"github.com/jitokim/fleetops/internal/events"
	"github.com/jitokim/fleetops/internal/gate"
	"github.com/jitokim/fleetops/internal/hidden"
	"github.com/jitokim/fleetops/internal/notify"
	"github.com/jitokim/fleetops/internal/oracle"
	"github.com/jitokim/fleetops/internal/registry"
	"github.com/jitokim/fleetops/internal/sessions"
	runewidth "github.com/mattn/go-runewidth"
)

// judgeFn/registryDirFn are oracle.Judge/registry.LoopsDir by default,
// overridable in tests so the judge trigger-policy state machine (and
// judgeCmd's registry write) can be verified without exec or touching the
// real ~/.fleetops/loops.
//
// resolveActuationTargetFn/redriveFn/sessionsDirFn are
// control.ResolveActuationTarget/control.Redrive/sessions.SessionsDir by
// default — the ADR Phase 2 tier policy (tty → host send → cwd → headless
// redrive) —
// overridable so sendPromptCmd/approveCmd/interruptCmd/killCmd's tier state
// machine (and ttyPathPlausible's keypress-time check) can be verified
// without exec or touching the real ~/.fleetops/sessions.
//
// historyDirFn/notifySendFn are events.HistoryDir/notify.Send by default —
// event-log-and-notify's own seams, same reasoning: overridable so the
// scan-transition detector, judgeCmd's verdict event, and each actuation
// cmd's event can be verified without touching the real
// ~/.fleetops/history, and so tests never actually invoke osascript (which
// would pop a real, visible desktop notification and doesn't exist outside
// macOS).
var (
	judgeFn                  = oracle.Judge
	registryDirFn            = registry.LoopsDir
	resolveActuationTargetFn = control.ResolveActuationTarget
	redriveFn                = control.Redrive
	sessionsDirFn            = sessions.SessionsDir
	historyDirFn             = events.HistoryDir
	notifySendFn             = notify.Send
	// hiddenFileFn is hidden.HiddenFile by default — the persisted hide-set
	// seam ("d"/"x"). Overridable so the hide/delete keys and startup load can
	// be verified without touching the real ~/.fleetops/hidden.json.
	hiddenFileFn = hidden.HiddenFile
	// controlResolveFn is control.Resolve by default — the CREATION/CAPABILITY
	// resolver seam. takeOverCmd still uses it (open a fresh terminal via
	// whichever backend is available); attach does NOT — see
	// controlResolveForLocateFn. Kept as an overridable seam because
	// control.Resolve does real exec/Available probes, so tests can't rely on
	// a real orca/tmux/cmux backend being installed.
	controlResolveFn = control.Resolve
	// controlResolveForLocateFn is control.ResolveForLocate by default —
	// attachCmd's ATTACH resolver seam. Split from controlResolveFn on purpose:
	// attach must follow whichever backend can actually Locate the loop's
	// surface (orca is always Available on the captain's machine and would
	// otherwise always win via Resolve() even when the loop lives in a
	// tmux/cmux surface it can't Locate). Locate-based, NEVER LocateClaude —
	// the attach-preservation invariant a bare shell tab is a valid jump
	// target (see TestAttachCmd_ObservedLoop_UsesLocateNotLocateClaude).
	controlResolveForLocateFn = control.ResolveForLocate
	// resolveFocusAdapterFn is control.ResolveFocusAdapter by default —
	// attachCmd's STEP 1 seam. Given a loop's recorded host_app it
	// returns the FocusAdapter that can Raise the loop's own window (iTerm2
	// today), or (nil,false) to degrade to step 2. Overridable so attach's
	// host_app routing is unit-testable with a fake adapter, without a real
	// iTerm2/osascript on the machine.
	resolveFocusAdapterFn = control.ResolveFocusAdapter
	// sessionEntryFn reads the selected loop's identity record so attachCmd can
	// resolve its host_app/window_id for step 1. sessions.ReadSession (against
	// sessionsDirFn) by default; overridable so attach's FocusAdapter routing is
	// testable without a real ~/.fleetops/sessions file.
	//
	// Returns the entry ALONE, no error: a miss (no record — a loop from before
	// the schema extension, or a headless one) is not exceptional here and its
	// only handling is the zero SessionEntry, whose empty HostApp/WindowID
	// already degrades attach to step 2 exactly as it did before this seam
	// existed. ReadSession returns that zero value on every failure path, so the
	// error carried no information the caller could act on — and the sole caller
	// discarded it.
	sessionEntryFn = func(sessionID string) sessions.SessionEntry {
		entry, _ := sessions.ReadSession(sessionsDirFn(), sessionID)
		return entry
	}
)

type loopsMsg []domain.Loop
type tickMsg time.Time

// resumeResultMsg reports the outcome of a resume (r key) attempt, computed
// off the event loop by resumeCmd so the TUI never blocks on exec. Also
// reused by injectCmd (see sendPromptCmd's doc) — sessionID is what lets the
// Update handler clear the right entry in m.actuating regardless of which
// of the two dispatched it.
type resumeResultMsg struct {
	sessionID string
	ok        bool
	text      string
}

// attachResultMsg reports the outcome of an attach (enter key) attempt,
// computed off the event loop by attachCmd, mirroring resumeResultMsg.
type attachResultMsg struct {
	ok   bool
	text string
}

// logClosedMsg reports that the pager opened by the "o" key has exited and
// control has returned to the TUI (tea.ExecProcess suspends the program
// while the pager runs).
type logClosedMsg struct{ err error }

// approveResultMsg reports the outcome of an approve (a key) attempt,
// computed off the event loop by approveCmd, mirroring resumeResultMsg.
type approveResultMsg struct {
	ok   bool
	text string
}

// spawnResultMsg reports the outcome of a new-loop spawn (n key) attempt,
// computed off the event loop by spawnCmd, mirroring resumeResultMsg.
type spawnResultMsg struct {
	ok   bool
	text string
}

// bootstrapResultMsg reports the outcome of LoopEngine MVP's headless
// bootstrap (the "n" wizard's 'e' choice) — computed off the event loop by
// bootstrapEngineCmd, mirroring spawnResultMsg's ok/text shape.
// sessionID is only meaningful when ok=true (the loop that got Bound); ""
// on failure, since no record was created to key anything on.
type bootstrapResultMsg struct {
	ok        bool
	text      string
	sessionID string
}

// worktreeEligibilityMsg reports whether the resolved backend implements
// control.WorktreeSpawner — computed off the event loop (control.Resolve
// does real exec calls) by checkWorktreeEligibilityCmd, fired at "n"
// keypress time so the result is (almost always) ready well before the
// wizard reaches its final wizardWhere step.
type worktreeEligibilityMsg bool

// killResultMsg reports the outcome of a kill (k key, double-press confirm)
// attempt, computed off the event loop by killCmd, mirroring resumeResultMsg.
type killResultMsg struct {
	ok   bool
	text string
}

// interruptResultMsg reports the outcome of an interrupt (p key) attempt,
// computed off the event loop by interruptCmd, mirroring resumeResultMsg.
type interruptResultMsg struct {
	ok   bool
	text string
}

// verdictMsg reports the outcome of a background oracle judgment, computed
// off the event loop by judgeCmd — one per bound loop the trigger policy
// (Model.triggerJudgments) decided was due. Clears that loop's in-flight
// guard regardless of success; the next scan picks up any saved verdict.
type verdictMsg struct {
	sessionID string
	verdict   domain.Verdict
	err       error
}

const refreshEvery = 3 * time.Second

// discoverLoopsFn is claude.DiscoverLoops by default — a seam (same
// package-level func-var pattern as judgeFn/redriveFn/registryDirFn) so
// tests can swap it out, most notably to PROVE demo mode never calls it:
// Model.scanCmd() returns nil (not scan) whenever m.demo is true, so this
// seam is structurally unreachable from a demo Model — see
// TestModel_ScanCmd_DemoModeNeverScans.
var discoverLoopsFn = claude.DiscoverLoops

// scan is a tea.Cmd: rediscover the fleet from the logs.
func scan() tea.Msg {
	loops, _ := discoverLoopsFn(time.Now(), claude.ActiveWindow)
	return loopsMsg(loops)
}

// scanCmd is the demo-aware wrapper Init/tickMsg dispatch instead of the
// bare scan cmd directly: nil for a demo Model (the synthetic fleet is
// seeded once, in NewDemo, and never rediscovered — see Model.demo's doc),
// otherwise scan unchanged. tea.Batch filters out nil cmds, so
// tea.Batch(m.scanCmd(), tick()) degrades to "just re-arm the ticker" for
// a demo Model with no special-casing needed at the call sites.
func (m Model) scanCmd() tea.Cmd {
	if m.demo {
		return nil
	}
	return scan
}

// isDemoBlockedKey names every key that would dispatch a mutating/actuation
// tea.Cmd in modeNormal — refused outright in demo mode (see the KeyMsg
// handler's demo guard). Kept as its own function, not inlined into the
// guard, so the "which keys are actually safe in demo mode" list is a
// single named, greppable thing to audit if a new mutating key is ever
// added to the switch below it.
func isDemoBlockedKey(key string) bool {
	switch key {
	// d/x now persist a tombstone (and x removes a registry entry), so both
	// write under ~/.fleetops keyed by a synthetic session id nothing else
	// would clean up — refused in demo like the other mutating keys, unlike
	// the old in-memory-only "d" dismiss.
	case "r", "a", "i", "enter", "o", "n", "k", "p", "d", "x":
		return true
	default:
		return false
	}
}

func tick() tea.Cmd {
	return tea.Tick(refreshEvery, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// statusKind colors the status/result line above the keybar (resume
// successes read green, failures red, a pending kill-confirm reads amber —
// anything else is neutral/dim).
type statusKind int

const (
	statusNeutral statusKind = iota
	statusOK
	statusErr
	statusWarn
)

// mode distinguishes normal fleet-navigation input from the "n" key's
// free-text goal prompt, the "/" key's filter query, the "i" key's
// arbitrary-prompt injection, and the "r" key's DRIFT-loop hint prompt
// (feat/drift-guided-redrive), so arrow/letter keys route to the text
// input instead of moving the cursor or triggering actions while typing.
type mode int

const (
	modeNormal mode = iota
	modePrompting
	modeFiltering
	modeInjecting
	modeDriftHint
)

// wizardStep is which question of the "n" key's loop-contract wizard is
// currently active. The wizard collects the full contract the wizard's
// caller (spawnCmd/buildSpawnPrompt) injects into the new session AND the
// same contract the oracle later judges against (internal/oracle.Judge) —
// one document, told to the agent and used to verify it.
type wizardStep int

const (
	wizardGoal       wizardStep = iota // required; empty cancels
	wizardName                         // optional; display name for the FLEET list (falls back to goal text — see domain.Loop.DisplayLabel)
	wizardDoneWhen                     // optional; completion condition
	wizardRubric                       // optional; verification rubric (was wizardOracle — feat/panel-info's precise rename, see domain.Goal's doc)
	wizardChallenger                   // optional; adversarial probe description, STORED ONLY
	wizardMaxCycles                    // optional; empty = registry.DefaultMaxCycles
	// wizardEngineDrive: single-key e/m/enter, LoopEngine's opt-in
	// choice. Reached ONLY when engineEnabledFn() (env
	// FLEETOPS_ENGINE=1) is true — the two-gate opt-in
	// (env AND this per-loop choice, both required). When the engine is
	// disabled, this step is skipped entirely: advanceSpawnWizard falls
	// straight through from wizardMaxCycles to wizardWhere exactly as it
	// did before this field existed — byte-for-byte the same manual path
	// (see TestAdvanceSpawnWizard_EngineDisabled_SkipsStraightToWhere).
	wizardEngineDrive
	// wizardWhere: single-key w/d/s/c/enter, ALWAYS reached on the manual
	// path (even when the backend can't do worktree spawn — [w] is simply
	// not offered then). It both picks worktree-vs-dir AND is the one place
	// the spawn target directory is shown and can be changed ([c] → wizardDir
	// free-text, [s] → the selected loop's verified cwd) — the spawn base is
	// otherwise ALWAYS the directory fleetops was launched from, never
	// silently inherited from the list cursor (that inheritance is exactly
	// what used to send a new loop into whatever repo/worktree the cursor
	// happened to sit on).
	wizardWhere
	wizardDir // free-text absolute/~ path, entered via [c] from wizardWhere or wizardEngineDrive; returns to spawnDirReturn
)

type Model struct {
	loops      []domain.Loop
	cursor     int
	w, h       int
	status     string
	statusKind statusKind
	lastScan   time.Time
	start      time.Time // for the header's uptime clock
	hostname   string

	// demo is true only for a Model built by NewDemo() (fleetops --demo):
	// a read-only preview over a fixed synthetic fleet (demoFleet()), never
	// touching ~/.claude/projects or ~/.fleetops. Gates two things: the
	// scan tea.Cmd (Init/tickMsg dispatch tick() alone, never scan — the
	// synthetic loops are seeded once, in NewDemo, and never re-discovered),
	// and every mutating/actuation key (see isDemoBlockedKey) — both would
	// otherwise touch disk or shell out against session ids that were never
	// real.
	demo bool

	mode     mode
	input    textinput.Model
	spawnCwd string // the wizard's spawn target dir: os.Getwd() at "n" (the dir fleetops runs in), changed ONLY by an explicit wizard choice ([c]/[s] at wizardWhere, [c] at wizardEngineDrive) — never inherited from the list cursor

	// spawnDirReturn is which single-key step ([c] at wizardWhere or
	// wizardEngineDrive) opened the wizardDir free-text step, so a valid
	// path entry can return there with the label re-rendered against the
	// new spawnCwd.
	spawnDirReturn wizardStep

	// spawnStep/spawnGoal/spawnDoneWhen/spawnRubric/spawnChallenger/
	// spawnMaxCycles hold the "n" key wizard's in-progress answers across
	// its steps (see wizardStep) — spawnCwd above is captured before step 1
	// and changes only via the explicit wizardDir/[s] choices.
	// spawnRubric was spawnOracle until feat/panel-info's precise rename —
	// see domain.Goal's doc.
	spawnStep       wizardStep
	spawnGoal       string
	spawnName       string
	spawnDoneWhen   string
	spawnRubric     string
	spawnChallenger string
	spawnMaxCycles  int

	// spawnWorktreeEligible/spawnHostsClaudeRepo drive the final wizardWhere
	// step's default and whether [w] is offered:
	//   - spawnWorktreeEligible: does the resolved backend implement
	//     control.WorktreeSpawner (orca only)? Computed OFF the event loop
	//     (control.Resolve does real exec calls) by checkWorktreeEligibilityCmd,
	//     fired at "n" keypress time — by the time a human types through 4-5
	//     wizard steps the result has almost always arrived, but the
	//     zero-value (false) is a safe fallback if it hasn't.
	//   - spawnHostsClaudeRepo: true when a fleet loop's Cwd matches the
	//     CURRENT spawnCwd (independent evidence claude has actually run
	//     there) — recomputed whenever spawnCwd changes (see
	//     dirHostsClaudeRepo).
	spawnWorktreeEligible bool
	spawnHostsClaudeRepo  bool

	filterQuery string // the APPLIED "/" filter (post-enter); "" means no filter

	// hidden is the persisted hide-set: sessionID -> hidden from the fleet
	// list. Both "d" (hide) and "x" (delete) add to it. UNLIKE the in-memory
	// dismiss it replaced, this is loaded from disk at startup (New →
	// hidden.Load) and rewritten on every add (hidden.Add), so a hidden loop
	// stays filtered across a fleetops restart even though loops are
	// re-derived by scanning ~/.claude on every launch. The loopsMsg handler
	// filters each scan's result through this set BEFORE detectTransitions,
	// so a hidden loop neither reappears on rescan nor keeps firing gate/gone
	// notifications from off-screen. See internal/hidden for the persistence
	// (atomic, fail-open) and Model.withoutHidden for the filter.
	hidden map[string]bool

	// injectTarget is the loop a modeInjecting prompt will be sent to,
	// snapshotted at "i" keypress time — NOT re-resolved from m.selected() at
	// submit time, because the fleet list can rescan (and reorder) while the
	// human is mid-typing, which would silently retarget the injection (same
	// staleness hazard the "n" wizard's spawnCwd capture guards against). The
	// whole Loop is captured so injectCmd has ProjectDir/SessionID plus the
	// Stall/State fields sendPromptCmd's guards re-check.
	injectTarget domain.Loop

	// driftHintTarget is the StateDrift loop a modeDriftHint prompt's
	// corrective hint will be re-driven against — same snapshot-at-
	// keypress-time reasoning as injectTarget (the fleet can rescan/reorder
	// while the human is mid-typing the hint).
	driftHintTarget domain.Loop

	pendingKillSession string // non-empty while awaiting the confirming second "k"
	pendingKillAt      time.Time

	// Same two-press confirm as kill, for "x" (delete). Delete removes the
	// session-registry registration and writes a permanent tombstone, and there
	// is no unhide path anywhere in the TUI — a stray keypress is unrecoverable
	// without hand-editing hidden.json, so it gets the same guard kill has.
	pendingDeleteSession string
	pendingDeleteAt      time.Time

	judging map[string]bool // sessionID -> a judgeCmd is in flight for it (in-flight guard, see triggerJudgments)

	// actuating guards against a double-press of r/i firing two concurrent
	// sends (resumeCmd/injectCmd, both routed through sendPromptCmd) onto
	// the SAME session — set at the r/i dispatch sites (Update), cleared in
	// the resumeResultMsg handler once the send completes. Most acutely
	// protects Tier 2 (control.Redrive): two concurrent `claude --resume`
	// headless turns against the same session, each holding a 10-minute
	// window, is unverified same-session-concurrency territory per the ADR
	// — same in-flight-guard shape as m.judging.
	actuating map[string]bool

	// notifiedAt is the desktop-notification dedup ledger: key is
	// sessionID+":"+edge (edge is "gate" or "gone" — see shouldNotify),
	// value is when that edge last fired a notify.Send. In-memory only —
	// not persisted, so a fleetops restart resets it: the first scan
	// after a restart treats every loop as a "first appearance" (see
	// detectTransitions), but seedFirstAppearanceGate specifically seeds a
	// synthetic edge for one already sitting in StateGate, so a restart
	// DOES still re-notify a still-open gate once (still subject to this
	// same dedup ledger, so a restart within the SAME 10-minute window as
	// an already-delivered notification does not double-fire). Accepted by
	// the council's design: a restart is rare enough that one extra
	// notification for a still-open gate is a fine trade against
	// persisting this ledger to disk for a purely cosmetic dedup window.
	notifiedAt map[string]time.Time

	// gitStats caches the selected loop's working-tree diff stats (files
	// changed, +/- lines — feat/detail-panel-v2's STAGE row), keyed by
	// SessionID. Populated by gitStatsCmd, dispatched once per scan tick
	// (loopsMsg) for ONLY the currently-selected loop — no other loop's
	// stats are ever rendered, so computing them for the whole fleet would
	// be pure waste. A zero-value entry (ok=false) simply means "not
	// computed yet" or "not a git repo" — STAGE omits the file/± portion
	// either way (see renderStageRow).
	gitStats map[string]gitStatsResult

	// fleetOracleCounts caches EVERY loop's total judged-verdict count
	// (TriggerOracle event count), keyed by SessionID — backs the FLEET
	// panel's new ORACLE×N column (feat/panel-info). UNLIKE gitStats/
	// detailCache above (which only ever compute for the SELECTED loop,
	// since only DETAIL renders their data), this covers the WHOLE fleet:
	// every visible row needs its own count. Still populated off the
	// render path, once per scan tick (fleetOracleCountsCmd, dispatched
	// alongside gitCmd/detailCmd in the loopsMsg handler) — NOT a
	// per-render event-log read, which is exactly the class of bug fix/
	// exit-gate-ux's architecture judge finding already eliminated
	// elsewhere in this file; this column must not reintroduce it. A
	// missing key (loop not yet scanned, or unbound) renders as the
	// zero-value 0, which oracleCompactLabel treats as "never judged" —
	// see its own doc.
	fleetOracleCounts map[string]int

	// detailCache caches the selected loop's event-log history and
	// transcript LAST ERROR extraction, keyed by SessionID — the SAME
	// off-loop tea.Cmd/Model-cache pattern as gitStats (see its doc),
	// applied to the two remaining pieces of the DETAIL panel that used to
	// do real disk I/O (events.Read) and transcript parsing (claude.LastError)
	// synchronously inside View() itself. fix/exit-gate-ux (architecture
	// judge, P1): that ran on the render path on EVERY keystroke/tick,
	// contradicting this file's own off-loop discipline. Populated by
	// detailCacheCmd, dispatched once per scan tick (loopsMsg) for ONLY the
	// currently-selected loop, same cadence/scope as gitStats. A zero-value
	// entry (empty events, lastError.ok=false) simply means "not computed
	// yet" — renderDetail already tolerates both inputs being empty/absent
	// (VERDICTS/EVENTS/LAST ERROR blocks are all optional and independently
	// omitted when there's nothing to show), so there is no separate
	// loading-placeholder state to manage.
	detailCache map[string]detailCacheEntry

	// autoRedriveAttempts/autoRedriveScheduledAt back feat/auto-redrive-429
	// — the opt-in 429 auto-redrive policy (see
	// maybeScheduleAutoRedrive429). autoRedriveAttempts is sessionID ->
	// LIFETIME attempt count (capped at autoRedriveMaxAttempts), lazily
	// seeded from the event log on first need per session (see
	// autoRedriveAttemptCount) so a fleetops restart doesn't reset the
	// ceiling. autoRedriveScheduledAt is sessionID -> when an auto-redrive
	// was last SCHEDULED — the dedup window that keeps a second
	// rate-limit edge for the same session, within autoRedriveDelay of the
	// last one, from scheduling another.
	autoRedriveAttempts    map[string]int
	autoRedriveScheduledAt map[string]time.Time
}

func New() Model {
	host, err := os.Hostname()
	if err != nil {
		host = "localhost"
	}
	return Model{
		status:   "watching ~/.claude/projects",
		start:    time.Now(),
		hostname: host,
		// Load the persisted hide-set up front so the FIRST scan's loopsMsg
		// already filters (and detectTransitions already skips) every hidden
		// loop — a restart must not re-surface, or re-notify for, anything the
		// human hid or deleted last session. Fail-open: a missing/corrupt file
		// loads as an empty set (see hidden.Load).
		hidden: hidden.Load(hiddenFileFn()),
	}
}

// NewDemo builds a Model seeded with demoFleet()'s fixed synthetic fleet
// instead of scanning ~/.claude/projects — fleetops --demo (see main.go).
// Everything demoFleet returns is seeded directly here, once: m.loops
// itself, plus the detailCache/fleetOracleCounts entries the normal
// loopsMsg pipeline would otherwise populate via real disk I/O
// (detailCacheCmd/fleetOracleCountsCmd). Init/tickMsg never dispatch scan
// for a demo Model (see Model.demo's doc), so that pipeline never runs
// again after this — the fleet is intentionally static for the whole
// session.
func NewDemo() Model {
	m := New()
	m.demo = true
	m.hostname = "dev-box"
	m.status = "demo mode — synthetic fleet, nothing real"
	loops, detailCache, oracleCounts := demoFleet()
	m.loops = loops
	m.detailCache = detailCache
	m.fleetOracleCounts = oracleCounts
	// Demo is hermetic ("nothing real"): drop whatever New loaded from the
	// real ~/.fleetops/hidden.json so a captain's actual tombstones can't
	// filter the synthetic fleet, and so d/x (demo-blocked anyway) have
	// nothing real to touch.
	m.hidden = nil
	return m
}

// demoFleet returns fleetops --demo's fixed synthetic fleet: NewDemo
// seeds a Model with exactly this, never scanning ~/.claude/projects or
// touching ~/.fleetops. Loop order matters: auth-harden is first so
// NewDemo's zero-value cursor (0) lands on it — the GATE is the "needs
// you" hero frame a first look (or a screenshot) should land on.
//
// The fixture deliberately covers every renderable shape this file knows
// about, in one small fleet: a GATE (permission prompt), a plain RUNNING
// loop, a DRIFT (oracle-rejected) loop, two unbound/observed loops (no
// goal at all — docs-gen idle, coverage stalled/429) proving the "no
// goal, no oracle" case renders correctly, and one Driven (engine-owned)
// loop so the ⚙ marker and the DRIVE/RUBRIC/CHALL contract rows have
// something real to show.
//
// The two returned maps seed exactly what the normal loopsMsg pipeline
// would otherwise populate via detailCacheCmd/fleetOracleCountsCmd (real
// disk reads) — kept in sync with what those functions actually read:
// renderOracleDetail's ORACLE row reads Loop.Last directly (no event log
// needed, so dep-upgrade's rejected verdict alone is enough for it), but
// renderVerdictsBlock's VERDICTS block and the FLEET panel's ORACLE×N
// column both read the event log / fleetOracleCounts — so a loop that
// should show VERDICTS (refactor-core) needs an actual TriggerOracle event
// seeded here, in the exact Detail encoding judgeCmd itself writes
// ("<outcome> at cycle <N>: <reason>", see events.ParseOracleDetail).
func demoFleet() (loops []domain.Loop, detailCache map[string]detailCacheEntry, oracleCounts map[string]int) {
	now := time.Now()

	authHarden := domain.Loop{
		Project:      "auth-harden",
		SessionID:    "demo-auth-harden",
		ProjectDir:   "-home-user-api",
		Cwd:          "/home/user/api",
		Path:         "/home/user/.claude/projects/-home-user-api/demo-auth-harden.jsonl",
		State:        domain.StateGate,
		GatePrompt:   "Allow Bash(git push origin main)?",
		Cycle:        6,
		Goal:         domain.Goal{Text: "Harden the auth middleware and open a PR", MaxCycles: 12, BudgetTokens: 2_000_000},
		TokensSpent:  640000,
		LastActivity: now.Add(-40 * time.Second),
	}
	flakyTests := domain.Loop{
		Project:      "flaky-tests",
		SessionID:    "demo-flaky-tests",
		ProjectDir:   "-home-user-web",
		Cwd:          "/home/user/web",
		Path:         "/home/user/.claude/projects/-home-user-web/demo-flaky-tests.jsonl",
		State:        domain.StateRunning,
		Cycle:        4,
		Goal:         domain.Goal{Text: "fix flaky tests", MaxCycles: 12},
		LastActivity: now.Add(-3 * time.Second),
	}
	depUpgrade := domain.Loop{
		Project:    "dep-upgrade",
		SessionID:  "demo-dep-upgrade",
		ProjectDir: "-home-user-lib",
		Cwd:        "/home/user/lib",
		Path:       "/home/user/.claude/projects/-home-user-lib/demo-dep-upgrade.jsonl",
		State:      domain.StateDrift,
		Cycle:      9,
		NoImprove:  2,
		Goal:       domain.Goal{Text: "upgrade deps to latest, keep CI green", MaxCycles: 12},
		Last: &domain.Verdict{Outcome: domain.OutcomeRejected,
			Reason: "claimed green but 3 tests still fail on a fresh run", AtCycle: 9},
		LastActivity: now.Add(-8 * time.Minute),
	}
	docsGen := domain.Loop{
		Project:      "docs-gen",
		SessionID:    "demo-docs-gen",
		ProjectDir:   "-home-user-docs",
		Cwd:          "/home/user/docs",
		Path:         "/home/user/.claude/projects/-home-user-docs/demo-docs-gen.jsonl",
		State:        domain.StateIdle,
		Cycle:        2,
		LastActivity: now.Add(-24 * time.Minute),
	}
	coverage := domain.Loop{
		Project:      "coverage",
		SessionID:    "demo-coverage",
		ProjectDir:   "-home-user-core",
		Cwd:          "/home/user/core",
		Path:         "/home/user/.claude/projects/-home-user-core/demo-coverage.jsonl",
		State:        domain.StateStalled,
		Stall:        domain.StallRateLimit,
		Cycle:        3,
		LastActivity: now.Add(-12 * time.Minute),
	}
	refactorCoreReason := "payment handlers moved into internal/payment, existing tests still pass — ledger reconciliation not yet covered"
	refactorCore := domain.Loop{
		Project:    "refactor-core",
		SessionID:  "demo-refactor-core",
		ProjectDir: "-home-user-svc",
		Cwd:        "/home/user/svc",
		Path:       "/home/user/.claude/projects/-home-user-svc/demo-refactor-core.jsonl",
		State:      domain.StateIdle,
		Cycle:      3,
		Driven:     true,
		Goal: domain.Goal{
			Text:         "extract the payment module into its own package",
			Rubric:       "rerun `go test ./... -count=2` and show it green; reject bare claims",
			Challenger:   "try to pass by deleting failing tests",
			MaxCycles:    8,
			BudgetTokens: 2_000_000,
		},
		TokensSpent:  410000,
		Last:         &domain.Verdict{Outcome: domain.OutcomeProgress, Reason: refactorCoreReason, AtCycle: 3},
		LastActivity: now.Add(-90 * time.Second),
	}

	loops = []domain.Loop{authHarden, flakyTests, depUpgrade, docsGen, coverage, refactorCore}

	detailCache = map[string]detailCacheEntry{
		authHarden.SessionID: {events: []events.Event{
			{TS: now.Add(-6 * time.Minute).UnixNano(), SessionID: authHarden.SessionID,
				FromState: "", ToState: "running", Trigger: events.TriggerActuation,
				Detail: "spawn: harden auth middleware", Actor: events.ActorHuman},
			{TS: now.Add(-40 * time.Second).UnixNano(), SessionID: authHarden.SessionID,
				FromState: "running", ToState: "gate", Trigger: events.TriggerScan,
				Detail: "on_done", Actor: events.ActorSystem},
		}},
		refactorCore.SessionID: {events: []events.Event{
			{TS: now.Add(-90 * time.Second).UnixNano(), SessionID: refactorCore.SessionID,
				FromState: "idle", ToState: "idle", Trigger: events.TriggerOracle,
				Detail: fmt.Sprintf("%s at cycle %d: %s", domain.OutcomeProgress, refactorCore.Cycle, refactorCoreReason),
				Actor:  events.ActorAuto},
		}},
	}

	oracleCounts = map[string]int{
		depUpgrade.SessionID:   1,
		refactorCore.SessionID: 1,
	}

	return loops, detailCache, oracleCounts
}

// destructiveConfirmWindow: the confirming second press must land within this
// long of the first to actually run — otherwise it starts a fresh confirm cycle
// instead. Shared by the two destructive keys, "k" (kill) and "x" (delete),
// hence the name: it stopped being kill-specific when "x" adopted the gate.
const destructiveConfirmWindow = 3 * time.Second

// Init dispatches the first scan (loopsMsg) and arms the refresh ticker —
// EXCEPT in demo mode, which never scans at all (m.scanCmd() returns nil):
// NewDemo already seeded m.loops directly, and re-running scan would
// immediately overwrite the synthetic fleet with whatever (if anything)
// ~/.claude/projects actually contains, defeating the entire point of
// --demo.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.scanCmd(), tick())
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
	case loopsMsg:
		newLoops := m.withoutHidden([]domain.Loop(msg))
		now := time.Now()
		transitions, autoRedriveCmds := m.detectTransitions(newLoops, now) // must run BEFORE m.loops is overwritten — compares old vs new
		m.loops = newLoops
		if m.cursor >= len(m.visibleLoops()) {
			m.cursor = maxInt(0, len(m.visibleLoops())-1)
		}
		m.lastScan = now
		// feat/detail-panel-v2: refresh the SELECTED loop's git stats once
		// per scan tick — never the whole fleet's (no other loop's stats
		// are ever rendered, so that would be pure waste).
		var gitCmd, detailCmd tea.Cmd
		if sel, ok := m.selected(); ok {
			gitCmd = gitStatsCmd(sel)
			// fix/exit-gate-ux (architecture judge, P1): mirrors gitCmd
			// exactly — see detailCache's doc for why this replaced the old
			// synchronous events.Read/claude.LastError calls in View().
			detailCmd = detailCacheCmd(sel)
		}
		// feat/panel-info: UNLIKE gitCmd/detailCmd above, this one DOES
		// cover the whole fleet, not just the selected loop — every FLEET
		// row now renders an ORACLE×N column (see renderListRow), so every
		// row needs its own judged-count, not only the one loop DETAIL
		// happens to be showing. Still off the render path and still only
		// once per scan tick — see fleetOracleCountsCmd's own doc for why
		// this doesn't reintroduce the per-render event-log read
		// fix/exit-gate-ux just finished eliminating.
		//
		// feat/engine-cycle: triggerDrives fires AFTER m.loops is already
		// overwritten (line above) — same ordering triggerJudgments already
		// relies on, and load-bearing for ShouldDrive's verdictFresh clause
		// (see its doc): this scan's enrichFromRegistry has already
		// promoted State/Last for anything that converged, so triggerDrives
		// only ever sees the POST-scan picture, never a stale pre-scan one.
		cmds := append([]tea.Cmd{m.triggerJudgments(), m.triggerDrives(), emitTransitionsCmd(transitions), gitCmd, detailCmd, fleetOracleCountsCmd(newLoops)}, autoRedriveCmds...)
		return m, tea.Batch(cmds...)
	case gitStatsMsg:
		if m.gitStats == nil {
			m.gitStats = make(map[string]gitStatsResult)
		}
		m.gitStats[msg.sessionID] = msg.stats
		return m, nil
	case fleetOracleCountsMsg:
		m.fleetOracleCounts = map[string]int(msg)
		return m, nil
	case detailCacheMsg:
		if m.detailCache == nil {
			m.detailCache = make(map[string]detailCacheEntry)
		}
		m.detailCache[msg.sessionID] = msg.entry
		return m, nil
	case autoRedriveScheduledMsg:
		// Re-check the CURRENT (latest scan) state before firing — a loop
		// that recovered (or aged out of the fleet) during the 5-minute
		// delay is silently skipped, per the task ("just don't fire", no
		// skipped event logged).
		l, found := m.loopBySessionID(msg.sessionID)
		if !found || l.State != domain.StateStalled || l.Stall != domain.StallRateLimit {
			return m, nil
		}
		// Review fix (P1): auto-redrive now joins the SAME m.actuating
		// interlock the manual r/i actuations already use — a manual
		// resume/inject already in flight for this session must not race
		// against an auto-redrive firing at the same time (most acutely,
		// two concurrent Tier-2 `claude --resume` headless turns). If
		// something is already in flight, skip silently: the next 429
		// scan edge (if the loop is still rate-limited once the manual
		// action completes) will reschedule.
		if m.actuating[l.SessionID] {
			return m, nil
		}
		attempt := m.autoRedriveAttemptCount(l.SessionID) + 1
		m.autoRedriveAttempts[l.SessionID] = attempt
		m.setActuating(l.SessionID)
		return m, autoRedrive429Cmd(l, attempt)
	case autoRedriveResultMsg:
		// Review fix (P1): clear the SAME interlock set when
		// autoRedrive429Cmd was dispatched — mirrors resumeResultMsg's own
		// clear exactly, so a manual r/i press right after an auto-redrive
		// completes is never wrongly refused as "already re-driving".
		if m.actuating != nil {
			delete(m.actuating, msg.sessionID)
		}
		if msg.ok {
			m.status, m.statusKind = fmt.Sprintf("auto-redrive %s: attempt %d/%d sent", msg.project, msg.attempt, autoRedriveMaxAttempts), statusNeutral
		} else {
			m.status, m.statusKind = fmt.Sprintf("auto-redrive %s: attempt %d/%d failed", msg.project, msg.attempt, autoRedriveMaxAttempts), statusErr
		}
		// Review fix (P2): the exhaustion notification is keyed on
		// REACHING THE CEILING, not on the redrive's own transport error —
		// the common exhaustion case is the 3rd attempt sending just fine
		// (ok=true) and the loop simply staying rate-limited (the API
		// itself is still saying no), which the old err!=nil-only check
		// left completely silent. Deduped via the SAME notify ledger
		// mechanism as the gate/gone edges (shouldNotify), keyed
		// "auto-exhausted" so it only ever fires once per session.
		if msg.attempt >= autoRedriveMaxAttempts && m.shouldNotify(msg.sessionID, "auto-exhausted", time.Now()) {
			return m, autoRedriveExhaustedNotifyCmd(msg.project)
		}
		return m, nil
	case tickMsg:
		return m, tea.Batch(m.scanCmd(), tick())
	case tea.KeyMsg:
		key := msg.String()
		if key != "k" {
			m.pendingKillSession = "" // any key other than a repeat "k" cancels a pending kill-confirm
		}
		if key != "x" {
			m.pendingDeleteSession = "" // ditto for a pending delete-confirm
		}

		if m.mode == modePrompting {
			if m.spawnStep == wizardEngineDrive {
				// single-key step (LoopEngine MVP) — same shape as
				// wizardWhere just below: route keys directly rather than
				// falling into the generic free-text switch.
				switch key {
				case "esc":
					m.mode = modeNormal
					m.input.Blur()
					m.status, m.statusKind = "cancelled", statusNeutral
					return m, nil
				case "e":
					return m.submitSpawnWizardEngineDrive()
				case "c":
					// explicit spawn-dir change — engine-drive spawns
					// headless in spawnCwd with no wizardWhere step, so the
					// dir must be changeable from here too.
					return m.enterWizardDir(wizardEngineDrive)
				case "m", "enter":
					// manual path: always show wizardWhere — it's now also
					// the step that displays (and can change) the spawn dir,
					// so it must not be skipped even when the backend can't
					// do worktree spawn.
					m.spawnStep = wizardWhere
					return m, textinput.Blink
				}
				return m, nil // ignore any other key at this single-key step
			}
			if m.spawnStep == wizardWhere {
				// single-key step — no textinput involved, so route keys
				// directly instead of falling into the generic
				// m.input.Update default case below.
				switch key {
				case "esc":
					m.mode = modeNormal
					m.input.Blur()
					m.status, m.statusKind = "cancelled", statusNeutral
					return m, nil
				case "w":
					if !m.spawnWorktreeEligible {
						return m, nil // [w] isn't offered — don't promise an isolation the backend can't deliver
					}
					return m.submitSpawnWizard(true)
				case "c":
					return m.enterWizardDir(wizardWhere)
				case "s":
					// explicit opt-in to the OLD implicit behavior: spawn in
					// the selected loop's directory. Only a verified-real cwd
					// qualifies (P1-3: a dead loop's Cwd is a lossy decode of
					// ProjectDir and could point anywhere).
					if sel, ok := m.selected(); ok && sel.CwdVerified && sel.Cwd != "" {
						m.spawnCwd = sel.Cwd
						m.spawnHostsClaudeRepo = m.dirHostsClaudeRepo(sel.Cwd)
					}
					return m, nil // stay on wizardWhere — the label re-renders with the new target
				case "enter":
					return m.submitSpawnWizard(m.spawnWorktreeEligible && m.spawnHostsClaudeRepo) // default
				case "d":
					return m.submitSpawnWizard(false)
				}
				return m, nil // ignore any other key at this single-key step
			}
			switch key {
			case "esc":
				m.mode = modeNormal
				m.input.Blur()
				m.status, m.statusKind = "cancelled", statusNeutral
				return m, nil
			case "enter":
				return m.advanceSpawnWizard()
			default:
				var cmd tea.Cmd
				m.input, cmd = m.input.Update(msg)
				return m, cmd
			}
		}

		if m.mode == modeFiltering {
			switch key {
			case "esc":
				m.mode = modeNormal
				m.input.Blur()
				m.filterQuery = ""
				if m.cursor >= len(m.visibleLoops()) {
					m.cursor = maxInt(0, len(m.visibleLoops())-1)
				}
				m.status, m.statusKind = "filter cleared", statusNeutral
				return m, nil
			case "enter":
				m.filterQuery = strings.TrimSpace(m.input.Value())
				m.mode = modeNormal
				m.input.Blur()
				if m.cursor >= len(m.visibleLoops()) {
					m.cursor = maxInt(0, len(m.visibleLoops())-1)
				}
				if m.filterQuery == "" {
					m.status, m.statusKind = "filter cleared", statusNeutral
				} else {
					m.status, m.statusKind = fmt.Sprintf("filter: %q", m.filterQuery), statusNeutral
				}
				return m, nil
			default:
				var cmd tea.Cmd
				m.input, cmd = m.input.Update(msg)
				// live-filter: clamp as the matching set shrinks/grows while typing.
				if m.cursor >= len(m.visibleLoops()) {
					m.cursor = maxInt(0, len(m.visibleLoops())-1)
				}
				return m, cmd
			}
		}

		if m.mode == modeInjecting {
			switch key {
			case "esc":
				m.mode = modeNormal
				m.input.Blur()
				m.status, m.statusKind = "cancelled", statusNeutral
				return m, nil
			case "enter":
				prompt := strings.TrimSpace(m.input.Value())
				if prompt == "" {
					// empty prompt cancels — same convention as the "n"
					// wizard's empty-goal cancel (see advanceSpawnWizard).
					m.mode = modeNormal
					m.input.Blur()
					m.status, m.statusKind = "cancelled (empty prompt)", statusNeutral
					return m, nil
				}
				m.mode = modeNormal
				m.input.Blur()
				// Belt-and-suspenders re-check immediately before dispatch
				// (mirrors sendPromptCmd's own re-checks of guards the
				// keypress-time gate already covers) — in the current call
				// graph this can't actually flip true→false during typing
				// (modeInjecting captures every key, so nothing else can
				// call setActuating for this session while it's open), but
				// checking again right at the dispatch site is the same
				// discipline as every other actuation guard in this file.
				if m.actuating[m.injectTarget.SessionID] {
					m.status, m.statusKind = fmt.Sprintf("already re-driving %s…", m.injectTarget.Project), statusNeutral
					return m, nil
				}
				// feat/inject-headless-exact-fallback: the ambiguity/
				// eligibility guard is the ONE keypress-time gate
				// sendPromptCmd does NOT re-check (its Tier1→Tier2
				// fallthrough is unconditional), and loop state can change
				// while the human types (fleet refresh messages still flow
				// during modeInjecting — only KEYS are captured) — e.g. an
				// Idle target picked up by a human typing in its own pane,
				// now mid-turn. Re-resolve the target from the CURRENT
				// fleet snapshot and re-run the same guard, so a target
				// that was eligible at keypress but went Running while
				// typing is refused here rather than headlessly re-driven
				// into a live interactive turn (the exact hazard
				// injectHeadlessFallbackEligible exists to exclude).
				headlessBound := false
				if fresh, ok := m.loopBySessionID(m.injectTarget.SessionID); ok {
					m.injectTarget = fresh
					if fresh.Stall != domain.StallGone && !m.ttyPathPlausible(fresh) {
						if msg, ambiguous := m.refuseIfAmbiguous(fresh); ambiguous {
							if !injectHeadlessFallbackEligible(fresh.State) {
								m.status, m.statusKind = msg, statusErr
								return m, nil
							}
							headlessBound = true
						}
					}
				}
				if m.injectTarget.Stall == domain.StallGone {
					m.status, m.statusKind = fmt.Sprintf("re-driving %s headlessly (tier 2)... this can take a few minutes", m.injectTarget.Project), statusNeutral
				} else if headlessBound {
					// Honest interim status (same discipline as the Tier 2
					// result message): this prompt is all but certain to
					// route headlessly — don't imply an in-place injection
					// the open window will never show.
					m.status, m.statusKind = fmt.Sprintf("injecting into %s headlessly (tier 2)... this can take a few minutes", m.injectTarget.Project), statusNeutral
				} else {
					m.status, m.statusKind = fmt.Sprintf("injecting into %s...", m.injectTarget.Project), statusNeutral
				}
				m.setActuating(m.injectTarget.SessionID)
				return m, injectCmd(m.injectTarget, prompt)
			default:
				var cmd tea.Cmd
				m.input, cmd = m.input.Update(msg)
				return m, cmd
			}
		}

		if m.mode == modeDriftHint {
			switch key {
			case "esc":
				m.mode = modeNormal
				m.input.Blur()
				m.status, m.statusKind = "cancelled", statusNeutral
				return m, nil
			case "enter":
				// Unlike modeInjecting, an EMPTY submission does NOT
				// cancel — "hint (enter=none)" means pressing Enter with
				// nothing typed is a valid choice: re-drive with no
				// corrective hint at all (composeDriftPrompt returns the
				// last prompt unchanged). Only Esc cancels.
				hint := strings.TrimSpace(m.input.Value())
				m.mode = modeNormal
				m.input.Blur()
				if m.actuating[m.driftHintTarget.SessionID] {
					m.status, m.statusKind = fmt.Sprintf("already re-driving %s…", m.driftHintTarget.Project), statusNeutral
					return m, nil
				}
				m.status, m.statusKind = fmt.Sprintf("re-driving %s...", m.driftHintTarget.Project), statusNeutral
				m.setActuating(m.driftHintTarget.SessionID)
				return m, driftRedriveCmd(m.driftHintTarget, hint)
			default:
				var cmd tea.Cmd
				m.input, cmd = m.input.Update(msg)
				return m, cmd
			}
		}

		// demo mode (fleetops --demo): a read-only preview over a fixed
		// synthetic fleet — every mutating/actuation key (spawn, kill, resume,
		// inject, approve, stop, attach/take-over, view-log) would either
		// shell out against a fake session that doesn't exist, or — for a
		// take-over/kill on the synthetic Driven loop — write a real file
		// under ~/.fleetops keyed by a session id nothing else will ever
		// clean up. Refused before any of that dispatches, not caught deeper
		// in the *Cmd functions, so this is a single, auditable choke point.
		// Navigation (arrows/g/G/j), the in-memory "/" filter, and q/ctrl+c
		// are unaffected — those never touch disk or a subprocess.
		if m.demo && isDemoBlockedKey(key) {
			m.status, m.statusKind = "demo mode is read-only — no actuation, nothing written to disk", statusNeutral
			return m, nil
		}

		switch key {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "up":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.visibleLoops())-1 {
				m.cursor++
			}
		case "g":
			m.cursor = 0
		case "G":
			m.cursor = maxInt(0, len(m.visibleLoops())-1)
		case "/":
			m.mode = modeFiltering
			m.input = textinput.New()
			m.input.Prompt = ""
			m.input.Focus()
			if m.filterQuery != "" {
				m.input.SetValue(m.filterQuery)
				m.input.CursorEnd()
			}
			return m, textinput.Blink
		case "esc":
			if m.filterQuery == "" {
				return m, nil
			}
			m.filterQuery = ""
			if m.cursor >= len(m.visibleLoops()) {
				m.cursor = maxInt(0, len(m.visibleLoops())-1)
			}
			m.status, m.statusKind = "filter cleared", statusNeutral
		case "r":
			sel, ok := m.selected()
			if !ok || (sel.State != domain.StateStalled && sel.State != domain.StateDrift) {
				m.status, m.statusKind = "select a stalled or drifted loop to resume", statusNeutral
				return m, nil
			}
			// In-flight guard (mirrors m.judging): a double-press of r/i on
			// the SAME session must not fire two concurrent sends — most
			// acutely, two concurrent Tier-2 `claude --resume` turns, each
			// holding a 10-minute window (unverified same-session
			// concurrency per the ADR).
			if m.actuating[sel.SessionID] {
				m.status, m.statusKind = fmt.Sprintf("already re-driving %s…", sel.Project), statusNeutral
				return m, nil
			}
			// feat/drift-guided-redrive: a DRIFT loop's "r" no longer
			// blindly resends the exact prompt the oracle just rejected —
			// that throws away its reason. Instead it opens a one-line
			// hint input (same shape as the "i" key's inject prompt); the
			// ambiguity guard still applies at THIS keypress time (fail
			// fast before the human even starts typing a hint), same as
			// every other actuation dispatch in this file.
			if sel.State == domain.StateDrift {
				if !m.ttyPathPlausible(sel) {
					if msg, ambiguous := m.refuseIfAmbiguous(sel); ambiguous {
						m.status, m.statusKind = msg, statusErr
						return m, nil
					}
				}
				m.driftHintTarget = sel
				m.mode = modeDriftHint
				m.input = textinput.New()
				m.input.Prompt = ""
				m.input.Focus()
				return m, textinput.Blink
			}
			if sel.Stall == domain.StallGone {
				// Goes straight to Tier 2 (headless redrive, see
				// sendPromptCmd) — there's no terminal surface to resolve
				// at all, so the ambiguity guard (which only protects the
				// cwd-based surface lookup) doesn't apply here either.
				m.status, m.statusKind = fmt.Sprintf("re-driving %s headlessly (tier 2)... this can take a few minutes", sel.Project), statusNeutral
				m.setActuating(sel.SessionID)
				return m, resumeCmd(sel)
			}
			if !m.ttyPathPlausible(sel) {
				if msg, ambiguous := m.refuseIfAmbiguous(sel); ambiguous {
					m.status, m.statusKind = msg, statusErr
					return m, nil
				}
			}
			m.status, m.statusKind = fmt.Sprintf("resuming %s...", sel.Project), statusNeutral
			m.setActuating(sel.SessionID)
			return m, resumeCmd(sel)
		case "a":
			sel, ok := m.selected()
			if !ok || sel.State != domain.StateGate {
				m.status, m.statusKind = "select a gated loop", statusNeutral
				return m, nil
			}
			if !m.ttyPathPlausible(sel) {
				if msg, ambiguous := m.refuseIfAmbiguous(sel); ambiguous {
					m.status, m.statusKind = msg, statusErr
					return m, nil
				}
			}
			m.status, m.statusKind = fmt.Sprintf("approving %s...", sel.Project), statusNeutral
			return m, approveCmd(sel)
		case "i":
			sel, ok := m.selected()
			if !ok {
				m.status, m.statusKind = "select a loop to send a prompt to", statusNeutral
				return m, nil
			}
			// Keypress-time state gate mirroring sendPromptCmd's own guards, so
			// the human doesn't type a whole prompt only to have it silently
			// refused after Enter (fail fast, before they invest typing effort).
			// StateFailed/StateKilled are the SAME conditions sendPromptCmd
			// itself re-checks — surfaced early here (belt-and-suspenders,
			// like the r-key guard). Unlike "r", injection is deliberately
			// NOT restricted to stalled/drifted loops: idle/running/gated
			// loops are all valid targets — flexibility is the point of the
			// feature. StallGone no longer refuses (see sendPromptCmd's
			// Tier 2 redrive path) — it's now a perfectly valid inject
			// target, just routed headlessly.
			if sel.State == domain.StateFailed {
				m.status, m.statusKind = "governor stopped this loop — k kill or start a new contract, don't inject", statusErr
				return m, nil
			}
			// fix/killed-state: a human's kill decision must not be
			// silently overridable via Tier 2's headless redrive
			// (`claude --resume <id> -p <prompt>` would actually REVIVE a
			// killed session) — StateKilled is domain.LoopState.Terminal(),
			// same policy-not-capability reasoning as StateFailed above.
			if sel.State == domain.StateKilled {
				m.status, m.statusKind = "this loop was killed — start a new contract, don't inject", statusErr
				return m, nil
			}
			// In-flight guard, same reasoning as the r-key's: fail fast
			// before the human types a whole prompt, rather than only
			// discovering the refusal after they press enter.
			if m.actuating[sel.SessionID] {
				m.status, m.statusKind = fmt.Sprintf("already re-driving %s…", sel.Project), statusNeutral
				return m, nil
			}
			if sel.Stall != domain.StallGone && !m.ttyPathPlausible(sel) {
				if msg, ambiguous := m.refuseIfAmbiguous(sel); ambiguous && !injectHeadlessFallbackEligible(sel.State) {
					m.status, m.statusKind = msg, statusErr
					return m, nil
				}
				// feat/inject-headless-exact-fallback: ambiguous AND eligible
				// (idle/stalled) — do NOT refuse. Let the human type their
				// prompt; sendPromptCmd's existing Tier1→Tier2 fallthrough
				// (unchanged) will find Tier 1 still can't disambiguate and
				// route this to `claude --resume <exact SessionID> -p
				// <prompt>` — the same trusted, session-unique headless
				// mechanism StallGone already always uses unconditionally,
				// see injectHeadlessFallbackEligible's doc.
			}
			m.injectTarget = sel
			m.mode = modeInjecting
			m.input = textinput.New()
			m.input.Prompt = ""
			m.input.Focus()
			return m, textinput.Blink
		case "enter":
			sel, ok := m.selected()
			if !ok {
				m.status, m.statusKind = "select a loop to attach", statusNeutral
				return m, nil
			}
			// feat/engine-provenance (design doc §4, the take-over
			// requirement): a Driven loop has no existing terminal surface — ↵ means
			// something structurally different for it than for an observed
			// loop (open ONE, not find one). attachCmd itself stays
			// completely untouched for the observed path (see its own doc's
			// attach-preservation note); this is a SEPARATE dispatch, not a
			// branch inside it.
			if sel.Driven {
				m.status, m.statusKind = fmt.Sprintf("taking over %s...", sel.Project), statusNeutral
				return m, takeOverCmd(sel)
			}
			m.status, m.statusKind = fmt.Sprintf("attaching %s...", sel.Project), statusNeutral
			return m, attachCmd(sel)
		case "o":
			sel, ok := m.selected()
			if !ok {
				m.status, m.statusKind = "select a loop to view its log", statusNeutral
				return m, nil
			}
			argv := pagerCmd(sel.Path)
			pager := exec.Command(argv[0], argv[1:]...)
			return m, tea.ExecProcess(pager, func(err error) tea.Msg {
				return logClosedMsg{err}
			})
		case "n":
			// The spawn base is ALWAYS the directory fleetops was launched
			// from — never the list cursor's loop. Inheriting the selected
			// loop's cwd here (the old behavior) meant "n" spawned into
			// whatever repo/worktree the cursor happened to sit on, which is
			// exactly the surprise wizardWhere's [s]/[c] choices now make
			// explicit instead.
			cwd, err := os.Getwd()
			if err != nil {
				cwd = "."
			}
			m.spawnCwd = cwd
			m.spawnStep = wizardGoal
			m.spawnGoal = ""
			m.spawnName = ""
			m.spawnDoneWhen = ""
			m.spawnRubric = ""
			m.spawnChallenger = ""
			m.spawnMaxCycles = 0
			m.spawnWorktreeEligible = false // set once checkWorktreeEligibilityCmd's result arrives
			m.spawnHostsClaudeRepo = m.dirHostsClaudeRepo(cwd)
			m.mode = modePrompting
			m.input = newWizardInput()
			return m, tea.Batch(textinput.Blink, checkWorktreeEligibilityCmd())
		case "k":
			sel, ok := m.selected()
			if !ok {
				m.status, m.statusKind = "select a loop to kill", statusNeutral
				return m, nil
			}
			// fix/killed-state: fail fast for a loop that's already gone —
			// no point running the two-press confirm dance (or reaching
			// killCmd's own resolveActuationTargetFn call, which would
			// otherwise surface a confusing "no unambiguous claude
			// surface" error for a process that simply isn't there
			// anymore) just to end up back here with the same message.
			if sel.State == domain.StateKilled || sel.Stall == domain.StallGone {
				m.status, m.statusKind = fmt.Sprintf("%s already killed/gone — it will age out of the window", sel.Project), statusNeutral
				return m, nil
			}
			now := time.Now()
			if m.pendingKillSession == sel.SessionID && now.Sub(m.pendingKillAt) <= destructiveConfirmWindow {
				m.pendingKillSession = ""
				// feat/engine-provenance: a Driven loop's kill never touches
				// an existing terminal surface at all (killCmd's own Driven
				// branch just clears the registry flag + logs an event) — the
				// ambiguity guard exists solely to protect a real keystroke
				// from landing on the wrong sibling surface, so it has
				// nothing to check for this path and would only risk a
				// spurious "N loops share this directory" refusal unrelated
				// to what's actually about to happen.
				if !sel.Driven {
					if !m.ttyPathPlausible(sel) {
						if msg, ambiguous := m.refuseIfAmbiguous(sel); ambiguous {
							m.status, m.statusKind = msg, statusErr
							return m, nil
						}
					}
				}
				m.status, m.statusKind = fmt.Sprintf("killing %s...", sel.Project), statusNeutral
				return m, killCmd(sel)
			}
			m.pendingKillSession = sel.SessionID
			m.pendingKillAt = now
			m.status, m.statusKind = fmt.Sprintf("press k again within 3s to kill %s", sel.Project), statusWarn
		case "p":
			sel, ok := m.selected()
			if !ok || (sel.State != domain.StateRunning && sel.State != domain.StateGate) {
				m.status, m.statusKind = "select a running or gated loop to stop", statusNeutral
				return m, nil
			}
			if !m.ttyPathPlausible(sel) {
				if msg, ambiguous := m.refuseIfAmbiguous(sel); ambiguous {
					m.status, m.statusKind = msg, statusErr
					return m, nil
				}
			}
			m.status, m.statusKind = fmt.Sprintf("stopping %s...", sel.Project), statusNeutral
			return m, interruptCmd(sel)
		case "d":
			// Hide: tombstone the selected loop into the PERSISTED hide-set so
			// it stays filtered across a fleetops restart (a restart otherwise
			// re-derives it from the ~/.claude scan). Non-destructive — nothing
			// in ~/.claude and no registry record is removed; the loop is only
			// filtered. See Model.hidden / hideSession for the semantics.
			sel, ok := m.selected()
			if !ok {
				m.status, m.statusKind = "select a loop to hide", statusNeutral
				return m, nil
			}
			if err := m.hideSession(sel.SessionID); err != nil {
				m.status, m.statusKind = fmt.Sprintf("hid %s in view, but persisting the hide failed: %v", sel.Project, err), statusErr
			} else {
				m.status, m.statusKind = fmt.Sprintf("hidden %s — stays hidden after restart", sel.Project), statusNeutral
			}
		case "x":
			// Delete (registry-only, safe): remove the session-registry tty
			// registration AND tombstone the same session into the persisted
			// hide-set, so the jsonl-scan-derived loop never reappears. NEVER
			// deletes the ~/.claude conversation log — history is preserved;
			// this is registry-entry removal + persistent tombstone only.
			sel, ok := m.selected()
			if !ok {
				m.status, m.statusKind = "select a loop to delete", statusNeutral
				return m, nil
			}
			// Two-press confirm, identical to "k". Delete drops the registry
			// registration and writes a permanent tombstone with no unhide
			// affordance anywhere in the TUI, so a single stray "x" is
			// unrecoverable short of hand-editing hidden.json — strictly less
			// reversible than the kill this guard was built for.
			now := time.Now()
			if m.pendingDeleteSession != sel.SessionID || now.Sub(m.pendingDeleteAt) > destructiveConfirmWindow {
				m.pendingDeleteSession = sel.SessionID
				m.pendingDeleteAt = now
				m.status, m.statusKind = fmt.Sprintf("press x again within 3s to delete %s", sel.Project), statusWarn
				return m, nil
			}
			m.pendingDeleteSession = ""
			delErr := sessions.DeleteSession(sessionsDirFn(), sel.SessionID)
			hideErr := m.hideSession(sel.SessionID)
			switch {
			case delErr != nil:
				m.status, m.statusKind = fmt.Sprintf("deleted %s — hidden, but registry removal failed: %v", sel.Project, delErr), statusErr
			case hideErr != nil:
				m.status, m.statusKind = fmt.Sprintf("deleted %s — registry entry removed, but persisting the hide failed: %v", sel.Project, hideErr), statusErr
			default:
				m.status, m.statusKind = fmt.Sprintf("deleted %s — registry entry removed, hidden", sel.Project), statusNeutral
			}
		}
	case resumeResultMsg:
		if m.actuating != nil {
			delete(m.actuating, msg.sessionID)
		}
		m.status = msg.text
		if msg.ok {
			m.statusKind = statusOK
		} else {
			m.statusKind = statusErr
		}
	case attachResultMsg:
		m.status = msg.text
		if msg.ok {
			m.statusKind = statusOK
		} else {
			m.statusKind = statusErr
		}
	case approveResultMsg:
		m.status = msg.text
		if msg.ok {
			m.statusKind = statusOK
		} else {
			m.statusKind = statusErr
		}
	case spawnResultMsg:
		m.status = msg.text
		if msg.ok {
			m.statusKind = statusOK
		} else {
			m.statusKind = statusErr
		}
	case bootstrapResultMsg:
		m.status = msg.text
		if msg.ok {
			m.statusKind = statusOK
		} else {
			m.statusKind = statusErr
		}
	case worktreeEligibilityMsg:
		m.spawnWorktreeEligible = bool(msg)
	case killResultMsg:
		m.status = msg.text
		if msg.ok {
			m.statusKind = statusOK
		} else {
			m.statusKind = statusErr
		}
	case interruptResultMsg:
		m.status = msg.text
		if msg.ok {
			m.statusKind = statusOK
		} else {
			m.statusKind = statusErr
		}
	case logClosedMsg:
		if msg.err != nil {
			m.status, m.statusKind = fmt.Sprintf("open log failed: %v", msg.err), statusErr
		} else {
			m.status, m.statusKind = "closed log", statusNeutral
		}
	case verdictMsg:
		// Clear the in-flight guard regardless of outcome — the next scan
		// (which re-reads the registry) is the source of truth for what got
		// saved; a background judgment failure/success intentionally does
		// NOT overwrite m.status, so it can't clobber more pressing
		// foreground feedback (e.g. a pending kill-confirm warning).
		if m.judging != nil {
			delete(m.judging, msg.sessionID)
		}
	}
	return m, nil
}

// sendPromptCmd is the shared, prompt-agnostic core behind both resumeCmd
// (re-send the last prompt) and injectCmd (send an arbitrary human-typed
// prompt): the StateFailed SAFETY guard + the ADR Phase 2 tier policy +
// Resume mechanics, with the prompt passed IN rather than looked up
// internally. Keeping the guard and tier policy in exactly ONE place is a
// safety-invariant-single-source-of-truth move — duplicating them across two
// send functions is exactly the drift-prone hazard LocateClaude's own
// ambiguity-refusal comments warn about. successVerb/note only shape the
// happy-path status text ("resumed X" vs "injected into X"); every guard,
// tier, and failure message is shared verbatim. Runs off the event loop —
// exec calls belong in a tea.Cmd, never in Update.
//
// Tier policy (docs/adr-vendor-independent-actuation.md §2.2/§3 step 2):
//  1. Tier 1 — tty (session-unique) → host send (1h, session-exact) → cwd
//     (ambiguity-guarded) chain, via resolveActuationTargetFn — skipped
//     entirely for a StallGone loop (see below).
//  2. Tier 2 — vendor-independent headless re-drive (redriveFn), reached
//     when Tier 1 didn't resolve a surface, when a resolved Tier 1h host
//     send REFUSED (see the dispatch below — EXCEPT on a timeout, whose
//     delivery is unknown and so must not be retried), OR when l.Stall is
//     StallGone.
//     StallGone no longer refuses: the claude process behind the ON-SCREEN
//     terminal is gone, but `claude --resume <id> -p <prompt>` restarts the
//     SAME conversation headlessly — that IS the restart the old manual
//     hint told the human to type, and it works with zero terminal surface
//     at all. Tier 1 is skipped rather than attempted-then-ignored for
//     StallGone specifically because a stale/recycled tty could otherwise
//     coincidentally match a DIFFERENT, unrelated live pane (ttys are
//     OS-recycled — see ResolveActuationTarget's doc).
//
// action is the actuation-event label ("resume"/"inject" — see
// logActuationEvent), distinct from successVerb (display text: "resumed" vs
// "injected into") purely because the two callers' display verbs don't
// share a mechanical stem worth deriving one from the other.
func sendPromptCmd(l domain.Loop, prompt, action, successVerb, note string) tea.Cmd {
	return func() tea.Msg {
		// SAFETY: the governor stopped this loop (internal/engine.Check via
		// applyGovernor, no-improve limit reached) — StateFailed is
		// deliberately terminal (domain.LoopState.Terminal()). Resuming it
		// would silently re-drive a loop the runtime already decided to
		// fail closed on; the human must make a new decision (kill, or a
		// fresh contract), not have "r"/"i" quietly override the governor.
		// This is policy, not capability — unlike StallGone, it applies
		// regardless of which tier could technically reach the session.
		// No actuation event here — nothing was dispatched to any tier.
		if l.State == domain.StateFailed {
			return resumeResultMsg{sessionID: l.SessionID, ok: false, text: "governor stopped this loop (no improvement) — k kill or start a new contract"}
		}
		// fix/killed-state: same policy-not-capability reasoning as
		// StateFailed above — Tier 2's headless redrive
		// (`claude --resume <id> -p <prompt>`) is fully capable of
		// reviving a killed session (it doesn't care whether a human
		// killed it), so this must be blocked at the policy layer, not
		// left to accidentally succeed. Belt-and-suspenders: the "i" key's
		// keypress-time guard (Update) already refuses before this is ever
		// reached via the TUI, but resumeCmd's own "r" guard only checks
		// Stalled/Drift (which already excludes Killed) — this is the one
		// shared choke point both paths funnel through.
		if l.State == domain.StateKilled {
			return resumeResultMsg{sessionID: l.SessionID, ok: false, text: "this loop was killed — start a new contract, don't resume/inject"}
		}

		if l.Stall != domain.StallGone {
			act, backendAvailable, found := resolveActuationTargetFn(sessionsDirFn(), l.SessionID, l.ProjectDir)
			if backendAvailable && found {
				err := act.Resume(prompt)
				if err == nil {
					logActuationEvent(l, action, act.Tier(), nil)
					return resumeResultMsg{sessionID: l.SessionID, ok: true, text: fmt.Sprintf("%s %s via %s%s", successVerb, l.Project, act.Backend(), note)}
				}
				logActuationEvent(l, action, act.Tier(), err)
				// A failed Tier 1h send is a DEGRADE, not a dead end. Tier 1h
				// resolves optimistically — the registry says the loop lives in
				// an iTerm2 session, and only the send itself can discover the
				// tab was closed, moved, or now hosts a different tty. Before
				// Tier 1h existed such a loop fell to Tier 2 and the prompt
				// landed; reporting the failure as terminal here would be a
				// capability REGRESSION for exactly the sessions the tier was
				// meant to help. Safe because 1h does not half-deliver on any
				// failure that reaches the fall-through below
				// (control.IsHostSendTier documents why), so the redrive cannot
				// double-send. The one failure that CAN have delivered is
				// carved out immediately after.
				//
				// Only r/i degrade: they have a Tier 2 (a headless redrive of
				// the same session). k/p/a have none, so their call sites keep
				// reporting a 1h failure as terminal rather than inventing a
				// fallback that does not exist.
				if !control.IsHostSendTier(act) {
					return resumeResultMsg{sessionID: l.SessionID, ok: false, text: fmt.Sprintf("resume %s failed: %v", l.Project, err)}
				}
				// ...with ONE exception: a deadline kill (see
				// control.ErrSendDeliveryUnknown) is the single 1h failure that
				// may have already delivered, because it interrupted a script
				// that was running rather than one that never started. Falling
				// through would risk re-sending the same prompt into a session
				// that already got it. Stop here and say the honest thing —
				// the outcome is UNKNOWN, so the human, not the runtime, makes
				// the call about retrying.
				if errors.Is(err, control.ErrSendDeliveryUnknown) {
					return resumeResultMsg{sessionID: l.SessionID, ok: false, text: fmt.Sprintf("%s %s: delivery UNKNOWN — the host send timed out and may or may not have landed. Attach (↵) and check before retrying; NOT re-driven, to avoid sending it twice", action, l.Project)}
				}
			}
		}

		// Tier 2: vendor-independent headless re-drive. Works on every
		// host (including a StallGone bare shell, or no backend/ambiguous
		// cwd match) — see docs/adr-vendor-independent-actuation.md §2.2.
		if err := redriveFn(l.SessionID, prompt); err != nil {
			logActuationEvent(l, action, "tier2", err)
			return resumeResultMsg{sessionID: l.SessionID, ok: false, text: fmt.Sprintf("re-drive %s failed: %v", l.Project, err)}
		}
		logActuationEvent(l, action, "tier2", nil)
		if l.Stall != domain.StallGone {
			// Bug 2 (Option B honesty fix, refined by
			// feat/inject-headless-exact-fallback): this branch is a
			// DOWNGRADE, not StallGone's normal Tier-2 path — Tier 1 either
			// found no backend, or a resolved Tier 1h
			// host send refused (the iTerm2 tab was closed, or its tty
			// moved), or (most commonly, with N>1 sessions
			// sharing a cwd on a backend with no per-session tty dispatch,
			// e.g. cmux/orca — see docs/adr-vendor-independent-actuation.md
			// §4 and ResolveActuationTarget's doc) couldn't disambiguate
			// which on-screen session was meant, so it fell through to the
			// SAME session's exact SessionID via the headless re-drive
			// instead of silently claiming success in the open window (or,
			// pre-feat/inject-headless-exact-fallback, refusing outright —
			// see injectHeadlessFallbackEligible). Name the exact session
			// the human's prompt actually reached (shortID) — the human is
			// watching a terminal window that will NOT visibly update.
			return resumeResultMsg{sessionID: l.SessionID, ok: true, text: fmt.Sprintf("delivered to %s's session %s as a background turn (tier 2) — couldn't target the on-screen session unambiguously, won't appear in the open window", l.Project, shortID(l.SessionID))}
		}
		return resumeResultMsg{sessionID: l.SessionID, ok: true, text: fmt.Sprintf("re-drove %s headlessly (tier 2) — output lands in the transcript", l.Project)}
	}
}

// resumeCmd re-sends a stalled loop's LAST USER PROMPT to the terminal surface
// hosting it, via whichever multiplexer backend (orca/cmux/tmux) is available.
// It's a thin wrapper over sendPromptCmd: its only prompt-specific work is
// looking up claude.LastUserPrompt — kept INSIDE the returned closure (off the
// event loop) because it reads/parses the session JSONL, which must not block
// Update.
func resumeCmd(l domain.Loop) tea.Cmd {
	return func() tea.Msg {
		prompt, ok := claude.LastUserPrompt(l.Path)
		note := ""
		if !ok {
			note = " (no prior prompt found — sent Enter only)"
		}
		return sendPromptCmd(l, prompt, "resume", "resumed", note)()
	}
}

// composeDriftPrompt appends an operator's corrective hint to lastPrompt —
// "<lastPrompt>\n\n[operator correction] <hint>" — the feat/drift-guided-
// redrive fix for "r" on a StateDrift loop blindly resending the exact
// prompt the oracle just rejected, throwing away its reason. hint=""
// (enter=none — the RE-DRIVE prompt's own label) returns lastPrompt
// unchanged. Pure function, directly unit-testable without driving key
// presses through Update.
func composeDriftPrompt(lastPrompt, hint string) string {
	hint = strings.TrimSpace(hint)
	if hint == "" {
		return lastPrompt
	}
	return lastPrompt + "\n\n[operator correction] " + hint
}

// driftRedriveCmd is resumeCmd's DRIFT-specific sibling: same fetch-last-
// prompt-then-sendPromptCmd shape, with the operator's hint woven in via
// composeDriftPrompt. Kept as its own function (a little duplication with
// resumeCmd) rather than threading a hint parameter through resumeCmd
// itself, since resumeCmd's OTHER caller (StateStalled, via the "r" key)
// never has a hint to offer at all.
func driftRedriveCmd(l domain.Loop, hint string) tea.Cmd {
	return func() tea.Msg {
		prompt, ok := claude.LastUserPrompt(l.Path)
		note := ""
		if !ok {
			note = " (no prior prompt found — sent Enter only)"
		}
		prompt = composeDriftPrompt(prompt, hint)
		return sendPromptCmd(l, prompt, "resume", "re-drove", note)()
	}
}

// injectCmd sends an arbitrary, human-typed prompt into loop l's confirmed
// claude surface and submits it — the "command the fleet from the dashboard"
// action (the "i" key). Thin wrapper over sendPromptCmd, the same shared core
// resumeCmd uses, so the StallGone/StateFailed safety guards live in exactly
// ONE place.
//
// It reuses resumeResultMsg (rather than a parallel injectResultMsg) on
// PURPOSE: from the runtime's perspective an inject IS a resume — "send this
// text to the surface and press Enter" — so the status-line handling in
// Update is byte-for-byte identical. A separate message type + Update case
// would be pure duplication for zero behavioral gain, not a copy-paste
// oversight.
func injectCmd(l domain.Loop, prompt string) tea.Cmd {
	return sendPromptCmd(l, prompt, "inject", "injected into", "")
}

// manualResumeHint is the copy-pasteable fallback for bare terminals (no
// orca/cmux/tmux to actuate into) — observation still works everywhere, but
// actuation degrades to "tell the human what to type".
func manualResumeHint(sessionID string) string {
	return "claude --resume " + sessionID
}

// attachCmd brings the terminal surface hosting l to the front. Works for any
// loop state (not just stalled) — "jump to it" is useful for a running loop
// too. Runs off the event loop, same non-blocking pattern as resumeCmd.
//
// Resolution order — a pure superset of the pre-FocusAdapter behavior, so no
// loop that attached before can stop attaching now:
//
//  1. The loop's recorded host_app has a FocusAdapter → ask it to Raise that
//     host's own window directly (iTerm2 today). What each adapter needs in
//     order to raise anything is the ADAPTER's business: iTerm2 requires a
//     window_id and answers ErrNoFocusSurface without one, so this layer does
//     not re-check that precondition (a copy here would silently skip a future
//     adapter that keys on something else entirely). Any Raise error degrades
//     to step 2 — see the error handling below for why none of them is fatal.
//  2. else ResolveForLocate(projectDir) → Focus — today's multiplexer path,
//     UNCHANGED. This is the orca/cmux/tmux adapter family: a loop hosted in a
//     multiplexer that didn't record a recognized host_app/window_id still
//     attaches exactly as before.
//  3. else the manual attach hint ("cd <cwd>").
//
// feat/engine-driver (attach-preservation requirement): step 2 stays Locate
// (not the stricter LocateClaude), unchanged from before the engine existed —
// a bare shell tab sharing the directory is a valid jump target. An
// engine-driven loop (no terminal surface at all) is a DIFFERENT path
// (takeOverCmd), not a change here. See
// TestAttachCmd_ObservedLoop_UsesLocateNotLocateClaude.
func attachCmd(l domain.Loop) tea.Cmd {
	return func() tea.Msg {
		// Step 1 — the loop's own host window, if the registry knows it and a
		// FocusAdapter can raise it. A missing/legacy record yields a zero entry
		// (empty host_app) that simply falls through to step 2.
		entry := sessionEntryFn(l.SessionID)
		var raiseErr error
		if adapter, ok := resolveFocusAdapterFn(entry.HostApp); ok {
			if raiseErr = adapter.Raise(entry); raiseErr == nil {
				return attachResultMsg{true, fmt.Sprintf("attached %s via %s", l.Project, entry.HostApp)}
			}
			// EVERY Raise error degrades to step 2 — none of them is fatal here.
			// ErrNoFocusSurface is the expected "nothing to raise", but a real
			// exec error is just as non-fatal: osascript exits non-zero when
			// macOS Automation permission has not been granted, which is a
			// normal first-run state, not a broken loop. Hard-failing there
			// would strand the human on an error with no way forward even
			// though a perfectly good tmux surface may be one step away. The
			// error is remembered so it can be reported IF every later step
			// also comes up empty.
		}

		// Step 2 — today's multiplexer path, unchanged. ResolveForLocate picks
		// whichever available backend can actually Locate the surface (not just
		// whichever is installed first) and hands back the Target it located, so
		// we Focus that Target directly. ok=false collapses both "no backend
		// available" and "no backend could locate a surface"; the manual-hint
		// fallback (step 3) is identical for both.
		ctrl, target, ok := controlResolveForLocateFn(l.ProjectDir)
		if !ok {
			return attachResultMsg{false, attachDegradedText(l, raiseErr, "no orca/tmux/cmux surface")}
		}
		if err := ctrl.Focus(target); err != nil {
			return attachResultMsg{false, attachDegradedText(l, raiseErr, fmt.Sprintf("attach %s failed: %v", l.Project, err))}
		}
		return attachResultMsg{true, fmt.Sprintf("attached %s via %s", l.Project, ctrl.Name())}
	}
}

// attachDegradedText builds the step-3 message for an attach that raised
// nothing: why the last step failed, the earlier step-1 Raise error when there
// was a real one worth reporting, and ALWAYS the manual hint. That trailing
// hint is the contract — attach never hard-fails, it either raises the surface
// or tells the human exactly where the loop lives, so there is always a way
// forward. ErrNoFocusSurface is deliberately omitted: "this host had no window
// to raise" is the designed degrade, not information the human can act on.
func attachDegradedText(l domain.Loop, raiseErr error, reason string) string {
	text := reason
	if raiseErr != nil && !errors.Is(raiseErr, control.ErrNoFocusSurface) {
		text = fmt.Sprintf("%s (%s raise failed: %v)", reason, l.Project, raiseErr)
	}
	return text + " — attach manually: " + manualAttachHint(l.Cwd)
}

// manualAttachHint is the copy-pasteable fallback for bare terminals (no
// orca/cmux/tmux to focus) — at least point the human at where the loop lives.
func manualAttachHint(cwd string) string {
	return "cd " + cwd
}

// takeOverCmd hands a Driven (engine-owned) loop's wheel to the human — the
// LoopEngine MVP's take-over requirement (design doc §4). Unlike
// attachCmd (Locate an EXISTING surface + Focus it), a Driven loop has no
// terminal surface to find — headless bootstrap never created one (§3) — so
// take-over CREATES one, running `claude --resume <sessionID>` so the human
// inherits the exact session the engine was driving, via whichever backend
// implements control.TerminalOpener (orca/tmux — cmux has no verified
// equivalent yet, see TerminalOpener's own doc; type-asserted the same way
// spawnCmd already type-asserts control.WorktreeSpawner).
//
// Driven is cleared (registry.MarkDriven false) ONLY after the terminal is
// confirmed opened — never before, and never in the no-backend/unsupported
// fallback branch. Ordering matters: clearing it first and then having
// OpenTerminal fail would strand the loop owned by neither the engine (no
// longer Driven) nor a human (no terminal actually opened for them).
// engine.ShouldDrive already requires driven==true (see its own doc's
// attach-preservation clause), so clearing Driven is the WHOLE pause
// mechanism — no separate "stop driving" call needed, the next scan simply
// never satisfies ShouldDrive for this session again.
//
// No backend, or a resolved backend that doesn't implement TerminalOpener
// (e.g. cmux): falls back to the SAME manual hint attach's own no-backend
// path uses ("claude --resume <id>") and leaves Driven untouched — the
// engine keeps driving it exactly as before (control.Redrive needs no
// terminal at all), since fleetops has no way to confirm a human actually
// took over by hand.
func takeOverCmd(l domain.Loop) tea.Cmd {
	return func() tea.Msg {
		command := manualResumeHint(l.SessionID)
		ctrl, ok := controlResolveFn()
		if !ok {
			return attachResultMsg{false, "no orca/tmux/cmux — take over manually: " + command}
		}
		opener, supports := ctrl.(control.TerminalOpener)
		if !supports {
			return attachResultMsg{false, "no orca/tmux/cmux — take over manually: " + command}
		}
		if err := opener.OpenTerminal(l.Cwd, command); err != nil {
			return attachResultMsg{false, fmt.Sprintf("take-over %s failed: %v", l.Project, err)}
		}
		if err := registry.MarkDriven(registryDirFn(), l.SessionID, false); err != nil {
			return attachResultMsg{false, fmt.Sprintf("take-over %s opened a terminal but could not clear Driven: %v — the engine may still drive it; kill (k) to stop it", l.Project, err)}
		}
		logActuationEvent(l, "take-over", "terminal", nil)
		return attachResultMsg{true, fmt.Sprintf("took over %s via %s — engine stopped driving it", l.Project, ctrl.Name())}
	}
}

// approveCmd accepts claude's default option at a gate (a bare Enter to the
// surface hosting the loop) via whichever multiplexer backend is available.
// On success it also best-effort deletes the loop's gate marker, so the UI
// clears the ◆ GATE state on the very next scan rather than waiting for the
// staleness check to catch up. Runs off the event loop, same non-blocking
// pattern as resumeCmd/attachCmd.
func approveCmd(l domain.Loop) tea.Cmd {
	return func() tea.Msg {
		// fix/killed-state: defense in depth — the "a" keypress guard
		// (Update) already requires StateGate, which a killed loop can
		// never be, so this is currently unreachable via the TUI; kept
		// here anyway so a future change to that guard can't accidentally
		// let a killed loop reach a real actuation attempt without an
		// explicit, sensible refusal.
		if l.State == domain.StateKilled {
			return approveResultMsg{false, "this loop was killed — nothing to approve"}
		}
		// Tier 1 only (tty → host send → cwd) — approving a gate has no headless Tier-2
		// equivalent (there's no "press Enter" over `claude --resume -p`;
		// that starts a brand new turn, not an in-place keypress).
		act, backendAvailable, found := resolveActuationTargetFn(sessionsDirFn(), l.SessionID, l.ProjectDir)
		if !backendAvailable {
			return approveResultMsg{false, "no orca/tmux/cmux — approve manually: attach and press Enter"}
		}
		if !found {
			return approveResultMsg{false, "no unambiguous claude surface — attach (↵) and act manually: press Enter"}
		}
		if err := act.Approve(); err != nil {
			logActuationEvent(l, "approve", act.Tier(), err)
			// Unknown delivery is not a failure — see unknownDeliveryText.
			if errors.Is(err, control.ErrSendDeliveryUnknown) {
				return approveResultMsg{false, unknownDeliveryText("approve", l.Project, "the Enter", "a")}
			}
			return approveResultMsg{false, fmt.Sprintf("approve %s failed: %v", l.Project, err)}
		}
		// Compare-and-swap delete: only remove the marker THIS decision was
		// based on (l.GateTS) — a plain delete-by-name could destroy a BRAND
		// NEW marker that landed between this loop's scan snapshot and this
		// approve call (see gate.DeleteMarkerIfTS).
		gate.DeleteMarkerIfTS(gate.GatesDir(), l.SessionID, l.GateTS)
		logActuationEvent(l, "approve", act.Tier(), nil)
		return approveResultMsg{true, fmt.Sprintf("approved %s via %s", l.Project, act.Backend())}
	}
}

// newWizardInput builds a fresh, focused textinput.Model for the next
// wizard step — each step starts with an empty input (the label carried in
// renderNewLoopPrompt/wizardStepLabel is what tells the human which
// question this is, not a placeholder on the input itself).
func newWizardInput() textinput.Model {
	input := textinput.New()
	input.Prompt = ""
	input.Focus()
	return input
}

// enterWizardDir opens the [c] change-dir free-text step from one of the two
// single-key steps (wizardWhere / wizardEngineDrive), remembering where to
// return on a valid entry. The input is prefilled with the current target so
// the human edits a visible path rather than typing one blind.
func (m Model) enterWizardDir(returnTo wizardStep) (tea.Model, tea.Cmd) {
	m.spawnDirReturn = returnTo
	m.spawnStep = wizardDir
	m.input = newWizardInput()
	m.input.SetValue(m.spawnCwd)
	m.input.CursorEnd()
	return m, textinput.Blink
}

// dirHostsClaudeRepo reports whether any loop in the current fleet has run
// in dir — the independent "claude has actually run here" evidence that
// makes worktree spawn wizardWhere's enter-default. Recomputed whenever the
// wizard's target dir changes (pure, no exec).
func (m Model) dirHostsClaudeRepo(dir string) bool {
	for _, l := range m.loops {
		if l.Cwd != "" && l.Cwd == dir {
			return true
		}
	}
	return false
}

// resolveSpawnDir turns wizardDir's free-text answer into an absolute
// existing directory: "~" expands to the home dir, relative paths resolve
// against fleetops' own cwd, and anything that doesn't stat as a directory
// is rejected (the wizard re-prompts rather than letting spawn fail later
// with a murkier backend error).
func resolveSpawnDir(value string) (string, error) {
	path := value
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot expand ~: %v", err)
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~"))
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("bad path %q: %v", value, err)
	}
	st, err := os.Stat(abs)
	if err != nil || !st.IsDir() {
		return "", fmt.Errorf("not a directory: %s", abs)
	}
	return abs, nil
}

// displayDir abbreviates the home-dir prefix to "~" for the wizard's
// status-line labels — pure formatting, never fed back into any path use.
func displayDir(dir string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return dir
	}
	if dir == home {
		return "~"
	}
	if strings.HasPrefix(dir, home+string(filepath.Separator)) {
		return "~" + dir[len(home):]
	}
	return dir
}

// parseMaxCycles parses the wizard's max_iteration step: "" means "use the
// default", anything else must be a positive integer. Pulled out of
// advanceSpawnWizard as its own pure function so the parsing/defaulting
// behavior (incl. the non-numeric/zero/negative rejection) is directly
// unit-testable without driving key presses through Update.
func parseMaxCycles(value string) (int, error) {
	if value == "" {
		return registry.DefaultMaxCycles, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("max_iteration must be a positive number")
	}
	return n, nil
}

// advanceSpawnWizard handles "enter" while modePrompting: validates/stores
// the current step's answer and moves to the next step, or — on the final
// step (wizardMaxCycles) — submits the spawn. Returns to modeNormal only on
// cancel (empty goal) or successful submission; a re-prompt (invalid max
// cycles) stays in modePrompting on the same step so the human can retry.
func (m Model) advanceSpawnWizard() (tea.Model, tea.Cmd) {
	value := strings.TrimSpace(m.input.Value())

	switch m.spawnStep {
	case wizardGoal:
		if value == "" {
			m.mode = modeNormal
			m.input.Blur()
			m.status, m.statusKind = "cancelled (empty goal)", statusNeutral
			return m, nil
		}
		m.spawnGoal = value
		m.spawnStep = wizardName
		m.input = newWizardInput()
		return m, textinput.Blink

	case wizardName:
		m.spawnName = value
		m.spawnStep = wizardDoneWhen
		m.input = newWizardInput()
		return m, textinput.Blink

	case wizardDoneWhen:
		m.spawnDoneWhen = value
		m.spawnStep = wizardRubric
		m.input = newWizardInput()
		return m, textinput.Blink

	case wizardRubric:
		m.spawnRubric = value
		m.spawnStep = wizardChallenger
		m.input = newWizardInput()
		return m, textinput.Blink

	case wizardChallenger:
		m.spawnChallenger = value
		m.spawnStep = wizardMaxCycles
		m.input = newWizardInput()
		return m, textinput.Blink

	case wizardMaxCycles:
		maxCycles, err := parseMaxCycles(value)
		if err != nil {
			m.status, m.statusKind = err.Error()+" — try again", statusErr
			return m, nil // re-prompt: stay in modePrompting on this same step
		}
		m.spawnMaxCycles = maxCycles

		// LoopEngine MVP: the engine-drive choice is offered ONLY when the
		// opt-in env gate is on — engineEnabledFn's zero-cost check, same
		// "don't offer a choice that doesn't exist" reasoning as the
		// worktree-eligibility skip just below. Engine disabled (the
		// common case today) → falls straight through to the EXACT same
		// worktree-eligibility check that ran here before this field
		// existed — byte-for-byte the manual path.
		if engineEnabledFn() {
			m.spawnStep = wizardEngineDrive
			return m, textinput.Blink
		}
		// ALWAYS show wizardWhere, even when no backend supports
		// worktree-isolated spawn (tmux/cmux, or no backend resolved at
		// all): it stopped being just the w/d choice — it's the one step
		// that shows the spawn target dir and offers [c]/[s] to change it,
		// and skipping it would commit the spawn to a dir the human never
		// saw. When ineligible, [w] simply isn't offered (see
		// whereStepLabel).
		m.spawnStep = wizardWhere
		return m, textinput.Blink

	case wizardDir:
		// [c]'s free-text path entry. Empty input keeps the current target
		// (a change-of-mind escape that doesn't cancel the whole wizard);
		// anything else must resolve to an existing directory or the step
		// re-prompts.
		if value != "" {
			dir, err := resolveSpawnDir(value)
			if err != nil {
				m.status, m.statusKind = err.Error()+" — try again", statusErr
				return m, nil // re-prompt: stay on wizardDir
			}
			m.spawnCwd = dir
			m.spawnHostsClaudeRepo = m.dirHostsClaudeRepo(dir)
		}
		m.spawnStep = m.spawnDirReturn
		return m, textinput.Blink

	default:
		// unreachable given wizardStep's const range, but never hang the UI
		// in prompting mode on an impossible state.
		m.mode = modeNormal
		m.input.Blur()
		m.status, m.statusKind = "cancelled (internal wizard error)", statusErr
		return m, nil
	}
}

// submitSpawnWizard finishes the wizard (from either wizardMaxCycles, when
// worktree spawn isn't eligible at all, or wizardWhere's w/d/enter) and
// dispatches spawnCmd. useWorktree is the wizard's resolved choice — spawnCmd
// re-checks WorktreeSpawner support itself before actually branching (ctrl is
// re-resolved at spawn time, not reused from the earlier eligibility check,
// in case availability changed in between).
func (m Model) submitSpawnWizard(useWorktree bool) (tea.Model, tea.Cmd) {
	m.mode = modeNormal
	m.input.Blur()
	note := ""
	if m.spawnDoneWhen == "" {
		note += " (no done condition — oracle judges against the goal only)"
	}
	if useWorktree {
		m.status, m.statusKind = fmt.Sprintf("spawning loop in a new worktree of %s...%s", m.spawnCwd, note), statusNeutral
	} else {
		m.status, m.statusKind = fmt.Sprintf("spawning loop in %s...%s", m.spawnCwd, note), statusNeutral
	}
	spec := registry.BindSpec{
		Name:          m.spawnName,
		Goal:          m.spawnGoal,
		DoneCondition: m.spawnDoneWhen,
		Rubric:        m.spawnRubric,
		Challenger:    m.spawnChallenger,
		MaxCycles:     m.spawnMaxCycles,
	}
	return m, spawnCmd(m.spawnCwd, spec, useWorktree)
}

// submitSpawnWizardEngineDrive finishes the wizard via the 'e' (engine-
// drive) choice at wizardEngineDrive — LoopEngine MVP's bootstrap path,
// sibling to submitSpawnWizard's existing manual path above. Skips
// wizardWhere entirely: the headless bootstrap (bootstrapEngineCmd) always
// runs in cwd directly — no worktree choice for an engine-driven loop yet
// (worktree isolation for engine loops is an explicit seed-spec non-goal).
func (m Model) submitSpawnWizardEngineDrive() (tea.Model, tea.Cmd) {
	m.mode = modeNormal
	m.input.Blur()
	m.status, m.statusKind = fmt.Sprintf("engine: bootstrapping %s… (cycle 1)", slugFromGoal(m.spawnGoal, 24)), statusNeutral
	spec := registry.BindSpec{
		Name:          m.spawnName,
		Goal:          m.spawnGoal,
		DoneCondition: m.spawnDoneWhen,
		Rubric:        m.spawnRubric,
		Challenger:    m.spawnChallenger,
		MaxCycles:     m.spawnMaxCycles,
		Driven:        true, // bootstrapEngineCmd asserts this too (defense in depth) — see its doc
	}
	return m, bootstrapEngineCmd(m.spawnCwd, spec)
}

// checkWorktreeEligibilityCmd resolves the current backend and reports
// whether it implements control.WorktreeSpawner — run off the event loop
// (control.Resolve/Available do real exec calls) and fired at "n" keypress
// time, well before the wizard reaches wizardWhere (see
// worktreeEligibilityMsg).
func checkWorktreeEligibilityCmd() tea.Cmd {
	return func() tea.Msg {
		ctrl, ok := control.Resolve()
		if !ok {
			return worktreeEligibilityMsg(false)
		}
		_, supports := ctrl.(control.WorktreeSpawner)
		return worktreeEligibilityMsg(supports)
	}
}

// spawnCmd starts a brand new claude loop from the wizard's full contract
// (spec), via whichever multiplexer backend is available. Controller.Spawn
// (and SpawnWorktree) have no way to report back the new session's id (they
// just start a process), so on success this writes a pending record
// (registry.WritePending) that the next scan's registry.BindPending matches
// to the new session once it starts writing its own JSONL — that's also
// what picks the loop up into the fleet in the first place; spawnCmd doesn't
// construct a domain.Loop. The prompt actually sent to the new session is
// the composed contract block (buildSpawnPrompt), not the bare goal — the
// registry still stores goal/doneWhen/oracle/challenger as separate fields.
//
// useWorktree requests the worktree-isolated branch (Part 1 of the
// worktree-spawn slice): when the resolved controller implements
// control.WorktreeSpawner, cwd is treated as the REPO to branch a fresh
// worktree from (SpawnWorktree also sends the contract prompt itself, as
// part of orca's one-shot --agent launch — no separate Resume/send step,
// verified live). If useWorktree is false, or the backend doesn't implement
// WorktreeSpawner at all (tmux/cmux — a structural fallback, not a
// preference), this falls through to the ordinary current-dir Spawn path
// unchanged.
//
// SHARED-WORKSPACE CAVEAT (verified live, see SpawnWorktree's doc): for a
// path-registered ("folder") repo, Orca doesn't isolate at all — the
// returned worktreePath comes back EQUAL to cwd. The spawn still fully
// works, so this is detected here (comparing the two paths) purely to tell
// the human the truth in the status line, not treated as any kind of
// failure.
func spawnCmd(cwd string, spec registry.BindSpec, useWorktree bool) tea.Cmd {
	return func() tea.Msg {
		ctrl, ok := control.Resolve()
		if !ok {
			return spawnResultMsg{false, "no orca/tmux/cmux — spawn manually: cd " + cwd + " && claude"}
		}
		prompt := buildSpawnPrompt(spec.Goal, spec.DoneCondition, spec.Rubric, spec.Challenger, spec.MaxCycles)

		if spawner, supports := ctrl.(control.WorktreeSpawner); useWorktree && supports {
			name := worktreeNameFromGoal(spec.Goal)
			worktreePath, err := spawner.SpawnWorktree(cwd, name, prompt)
			if err != nil {
				return spawnResultMsg{false, fmt.Sprintf("spawn worktree failed: %v", err)}
			}
			pendingCwd := worktreePath
			bindNote := ""
			if pendingCwd == "" {
				// SpawnWorktree succeeded but the (unverified) response
				// shape didn't yield a path — best-effort pending record
				// keyed by the repo cwd; BindPending's existing
				// newest-unbound-wins matching still applies, it just has
				// less certainty which session is "the" new one (same as
				// the shared-workspace case just below).
				pendingCwd = cwd
				bindNote = " (binding may miss — worktree path unknown)"
			}
			if err := registry.WritePending(registry.PendingDir(), pendingCwd, spec); err != nil {
				return spawnResultMsg{true, fmt.Sprintf("spawned loop in new worktree %s via %s (goal not recorded: %v)", name, ctrl.Name(), err)}
			}
			if worktreePath != "" && worktreePath == cwd {
				// folder repo — no isolated checkout was actually created
				// (see SpawnWorktree's shared-workspace caveat). Say so
				// plainly rather than claiming isolation that didn't happen.
				return spawnResultMsg{true, fmt.Sprintf("spawned in shared workspace %s (no isolated checkout — folder repo)", name)}
			}
			return spawnResultMsg{true, fmt.Sprintf("spawned loop in new worktree %s via %s%s", name, ctrl.Name(), bindNote)}
		}

		if err := ctrl.Spawn(cwd, prompt); err != nil {
			return spawnResultMsg{false, fmt.Sprintf("spawn failed: %v", err)}
		}
		if err := registry.WritePending(registry.PendingDir(), cwd, spec); err != nil {
			// best-effort: the loop still spawned and will show up
			// unbound — just won't get ORACLE/N-I tracking.
			return spawnResultMsg{true, fmt.Sprintf("spawned loop in %s via %s (goal not recorded: %v)", cwd, ctrl.Name(), err)}
		}
		return spawnResultMsg{true, fmt.Sprintf("spawned loop in %s via %s", cwd, ctrl.Name())}
	}
}

// ── LoopEngine: headless bootstrap ───────────────────────────────────────
//
// Bootstrap = run the wizard's contract as a HEADLESS cycle 1
// (`claude -p <contract> --output-format json`), read session_id back
// SYNCHRONOUSLY from that call's own output, and Bind DETERMINISTICALLY —
// no BindPending cwd/timestamp race at all, unlike spawnCmd's manual path
// above (we have the exact session_id, not a "probably this one" match).
// This runs cycle 1 only; triggerDrives/driveCmd take over auto-cycling
// from there. The bootstrapped loop shows up in the very next scan as an
// ordinary bound loop with Driven=true; its State derives from its
// transcript exactly like any other loop (the scanner stays the SOLE owner
// of State — this function never classifies anything itself).

// bootstrapTimeout bounds the headless claude -p bootstrap call.
// Deliberately generous compared to judgeTimeout's 2 minutes (internal/
// oracle) — cycle 1 does REAL WORK, not a cheap haiku judgment.
const bootstrapTimeout = 5 * time.Minute

// bootstrapEnvelope is claude -p --output-format json's top-level shape —
// the fields bootstrap cares about. SessionID is the only one this slice
// actually consumes; Result/IsError/Usage are captured for completeness
// (a future slice's provenance/diagnostics) but unused here.
type bootstrapEnvelope struct {
	SessionID string          `json:"session_id"`
	Result    string          `json:"result"`
	IsError   bool            `json:"is_error"`
	Usage     json.RawMessage `json:"usage,omitempty"`
}

// bootstrapSessionIDRe is parseBootstrapSessionID's lenient fallback —
// same regex-tolerance idiom as internal/oracle's fencedJSON/
// extractJSONObject: session_id is always a plain ASCII token, so
// extracting it directly is safe even when a raw control character
// elsewhere in the envelope (e.g. inside "result"'s text — live-observed)
// trips encoding/json's strict string-literal validation for the object
// as a WHOLE.
var bootstrapSessionIDRe = regexp.MustCompile(`"session_id"\s*:\s*"([^"]+)"`)

// parseBootstrapSessionID extracts session_id from claude -p
// --output-format json's raw stdout: a strict decode first (the common,
// well-formed case), falling back to a direct regex extraction if strict
// parsing fails. ok=false only when NEITHER path finds a non-empty
// session_id — bootstrapEngineCmd must never create a phantom record in
// that case.
func parseBootstrapSessionID(raw []byte) (string, bool) {
	var env bootstrapEnvelope
	if err := json.Unmarshal(raw, &env); err == nil && env.SessionID != "" {
		return env.SessionID, true
	}
	if m := bootstrapSessionIDRe.FindSubmatch(raw); m != nil {
		return string(m[1]), true
	}
	return "", false
}

// bootstrapClaudeFn runs `claude -p <prompt> --output-format json` in cwd
// and returns its raw stdout — the ONE real exec call bootstrapEngineCmd
// makes, isolated behind a func var (same seam shape as judgeFn/redriveFn:
// an entire side-effecting call, swappable, not just a directory/session
// lookup) so tests can verify Bind/event-emission/status-line behavior
// without invoking a real claude CLI.
var bootstrapClaudeFn = func(ctx context.Context, cwd, prompt string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "claude", "-p", prompt, "--output-format", "json")
	cmd.Dir = cwd
	return cmd.Output()
}

// bootstrapEngineCmd runs cwd's contract as a headless cycle 1 and, only
// on success, Binds the result. Runs off the event loop, same non-blocking
// discipline as every other tea.Cmd in this file — bootstrapTimeout's 5
// minutes must never block Update/View.
//
// spec.Driven is asserted true HERE, regardless of what the caller passed
// in — defensive, fail-closed duplication of submitSpawnWizardEngineDrive's
// own Driven:true (this function's ENTIRE reason to exist is creating an
// engine-driven loop; trusting the caller alone isn't enough — a future
// caller bug leaving Driven false here would silently create a bound-but-
// never-driven record with no way to ever be picked up by ShouldDrive).
//
// buildSpawnPrompt is reused VERBATIM — the exact same contract document
// the manual spawn path's cycle 1 sends, and what the oracle later judges
// against; one document, not two that could drift apart.
//
// Failure (no session_id in the envelope, the claude -p call itself
// errors/times out, or the subsequent Bind fails) surfaces a clear status
// and creates NO record.
func bootstrapEngineCmd(cwd string, spec registry.BindSpec) tea.Cmd {
	return func() tea.Msg {
		spec.Driven = true

		ctx, cancel := context.WithTimeout(context.Background(), bootstrapTimeout)
		defer cancel()
		prompt := buildSpawnPrompt(spec.Goal, spec.DoneCondition, spec.Rubric, spec.Challenger, spec.MaxCycles)
		out, err := bootstrapClaudeFn(ctx, cwd, prompt)
		if err != nil {
			return bootstrapResultMsg{false, fmt.Sprintf("engine bootstrap failed: %v", err), ""}
		}

		sessionID, ok := parseBootstrapSessionID(out)
		if !ok {
			return bootstrapResultMsg{false, "engine bootstrap: no session_id in response — no loop created", ""}
		}

		if err := registry.Bind(registryDirFn(), sessionID, spec); err != nil {
			return bootstrapResultMsg{false, fmt.Sprintf("engine bootstrap: cycle 1 ran (session %s) but binding failed: %v", sessionID, err), ""}
		}
		_ = events.Append(historyDirFn(), events.Event{
			TS:        time.Now().UnixNano(),
			SessionID: sessionID,
			FromState: "",
			ToState:   "",
			Trigger:   events.TriggerEngine,
			Detail:    "bootstrap: " + trunc(spec.Goal, 200),
			Actor:     events.ActorAuto,
		})
		return bootstrapResultMsg{true, fmt.Sprintf("engine loop bootstrapped (session %s, cycle 1 complete)", sessionID), sessionID}
	}
}

// worktreeNameFromGoal builds the `orca worktree create --name` value from
// the wizard's goal: "mctl-" + a lowercase [a-z0-9-] slug of the goal's
// first ~24 runes.
func worktreeNameFromGoal(goal string) string {
	return "mctl-" + slugFromGoal(goal, 24)
}

// slugFromGoal lowercases goal and keeps only [a-z0-9], collapsing every
// other run of characters to a single dash, capped to the first maxRunes
// INPUT runes (not output length) — "loop" if nothing alnum survives. Pure
// function so the slugging is directly testable. Shared core: originally
// worktreeNameFromGoal's own body (still its sole caller for the "mctl-"-
// prefixed worktree name); LoopEngine MVP's engine-drive bootstrap reuses
// it bare (no prefix) for the "engine: bootstrapping <slug>…" status line
// — same "compact, filesystem/terminal-safe label from free-text goal"
// need, not a new concept.
func slugFromGoal(goal string, maxRunes int) string {
	runes := []rune(strings.ToLower(goal))
	if len(runes) > maxRunes {
		runes = runes[:maxRunes]
	}
	var b strings.Builder
	prevDash := false
	for _, r := range runes {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		case !prevDash && b.Len() > 0:
			b.WriteByte('-')
			prevDash = true
		}
	}
	slug := strings.TrimRight(b.String(), "-")
	if slug == "" {
		slug = "loop"
	}
	return slug
}

// buildSpawnPrompt composes the LOOP CONTRACT block sent as the new
// session's very first prompt. This is the SAME contract the wizard
// collected (see the "n" key) that also becomes the oracle's judging rubric
// (internal/oracle.Judge, via doneWhen/rubricText) — what the agent is told
// and what it's judged against are one document, not two that can drift
// apart. maxCycles is always shown resolved (never 0) — the wizard applies
// registry.DefaultMaxCycles before calling this. challenger's line is
// omitted entirely when empty: there's no challenger phase yet (see
// DESIGN.md), so an empty line naming a check that never runs would be
// actively misleading.
//
// feat/panel-info (precise rename): the criteria line is labeled "rubric:"
// now, not "oracle:" — matching the wizard's renamed prompt and
// domain.Goal.Rubric. "An independent oracle will verify..." at the end is
// UNCHANGED — that sentence is genuinely about the judge, not the criteria.
func buildSpawnPrompt(goal, doneWhen, rubricText, challenger string, maxCycles int) string {
	done := doneWhen
	if done == "" {
		done = "you judge the goal fully achieved"
	}
	rubricLine := rubricText
	if rubricLine == "" {
		rubricLine = "an independent LLM judge verifies against the complete condition"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "goal: %s\n", goal)
	fmt.Fprintf(&b, "complete condition: %s\n", done)
	fmt.Fprintf(&b, "rubric: %s\n", rubricLine)
	if challenger != "" {
		fmt.Fprintf(&b, "challenger: %s\n", challenger)
	}
	fmt.Fprintf(&b, "max_iteration: %d\n", maxCycles)
	b.WriteString("\nWork in cycles toward the goal. Report progress concretely each cycle.\n")
	b.WriteString("Declare DONE only when the complete condition is met — state the evidence.\n")
	b.WriteString("An independent oracle will verify your claim against this contract.")
	return b.String()
}

// killCmd cleanly quits a loop's claude process by re-sending "/exit" +
// Enter — reuses Resume, which does exactly "type text, press Enter".
func killCmd(l domain.Loop) tea.Cmd {
	return func() tea.Msg {
		// SAFETY: same reasoning as resumeCmd's StallGone guard — if the
		// process is already gone, there's nothing to send "/exit" into
		// (and doing so anyway risks typing it into a bare shell instead).
		// fix/killed-state: belt-and-suspenders mirror of the "k" keypress
		// guard (Update) — StateKilled reaching here at all would only
		// happen via a stale dispatch, but the discipline in this file is
		// every guard gets re-checked at the actual dispatch site too.
		if l.State == domain.StateKilled || l.Stall == domain.StallGone {
			return killResultMsg{true, fmt.Sprintf("%s already killed/gone — it will age out of the window", l.Project)}
		}
		// feat/engine-provenance (design doc §4's kill adapter): a Driven
		// (engine-owned, headless) loop has no terminal surface to /exit
		// into — resolveActuationTargetFn below would just fail with a
		// misleading "no unambiguous claude surface", since there was never
		// a surface to find. Killing it instead means: clear Driven
		// (MarkDriven false) so the engine never schedules it another cycle
		// — ShouldDrive already requires driven==true, so this alone is the
		// WHOLE "no further drive fires" guarantee, same free-pause
		// mechanism take-over's own Driven-clear relies on — plus record a
		// kill event in the SAME shape mostRecentActuationIsKill looks for
		// (TriggerActuation/ActorHuman, Detail prefixed "kill ", via the
		// unchanged logActuationEvent helper) so the next scan promotes
		// StateKilled exactly as it would for an observed loop's kill.
		if l.Driven {
			if err := registry.MarkDriven(registryDirFn(), l.SessionID, false); err != nil {
				return killResultMsg{false, fmt.Sprintf("kill %s failed: could not clear Driven — %v", l.Project, err)}
			}
			logActuationEvent(l, "kill", "engine", nil)
			return killResultMsg{true, fmt.Sprintf("killed %s — Driven cleared, state updates on next scan", l.Project)}
		}
		// Tier 1 only (tty → host send → cwd) — killing has no headless Tier-2
		// equivalent (there's no live conversation left to type "/exit"
		// into via a fresh --resume -p turn).
		act, backendAvailable, found := resolveActuationTargetFn(sessionsDirFn(), l.SessionID, l.ProjectDir)
		if !backendAvailable {
			return killResultMsg{false, "no orca/tmux/cmux — kill manually: type /exit in " + l.Project}
		}
		if !found {
			return killResultMsg{false, "no unambiguous claude surface — attach (↵) and act manually: type /exit"}
		}
		if err := act.Resume("/exit"); err != nil {
			logActuationEvent(l, "kill", act.Tier(), err)
			// Unknown delivery is not a failure — see unknownDeliveryText.
			// This is the site that motivated the distinction: "kill failed"
			// reads as "press k again," and a second "/exit" into a session
			// that already got one is the double-send this guards.
			if errors.Is(err, control.ErrSendDeliveryUnknown) {
				return killResultMsg{false, unknownDeliveryText("kill", l.Project, `the "/exit"`, "k")}
			}
			return killResultMsg{false, fmt.Sprintf("kill %s failed: %v", l.Project, err)}
		}
		// fix/killed-state: the event is written HERE, immediately once
		// the "/exit" keystroke is confirmed sent — not once the process
		// has actually exited (which happens asynchronously, outside this
		// call's control). That's exactly what lets the NEXT scan's
		// mostRecentActuationIsKill see a kill on record and derive
		// StateKilled as soon as the process is confirmed gone — the
		// status line deliberately does NOT optimistically set local model
		// state itself (that would be a fake, unverified state the next
		// scan could immediately contradict).
		logActuationEvent(l, "kill", act.Tier(), nil)
		return killResultMsg{true, fmt.Sprintf("killed %s — state updates on next scan", l.Project)}
	}
}

// interruptCmd stops a loop's current turn (Esc) without killing the
// process — the loop stays alive, resumable with r.
func interruptCmd(l domain.Loop) tea.Cmd {
	return func() tea.Msg {
		// fix/killed-state: defense in depth — the "p" keypress guard
		// (Update) already requires Running/Gate, which a killed loop can
		// never be, so this is currently unreachable via the TUI; kept
		// here anyway, same reasoning as approveCmd's mirror check.
		if l.State == domain.StateKilled {
			return interruptResultMsg{false, "this loop was killed — nothing to stop"}
		}
		// Tier 1 only (tty → host send → cwd) — interrupting has no headless Tier-2
		// equivalent (there's no in-flight turn to interrupt via a fresh
		// --resume -p call; that would start a brand new turn instead).
		act, backendAvailable, found := resolveActuationTargetFn(sessionsDirFn(), l.SessionID, l.ProjectDir)
		if !backendAvailable {
			return interruptResultMsg{false, "no orca/tmux/cmux — stop manually: press Esc in " + l.Project}
		}
		if !found {
			return interruptResultMsg{false, "no unambiguous claude surface — attach (↵) and act manually: press Esc"}
		}
		if err := act.Interrupt(); err != nil {
			logActuationEvent(l, "interrupt", act.Tier(), err)
			// Unknown delivery is not a failure — see unknownDeliveryText.
			if errors.Is(err, control.ErrSendDeliveryUnknown) {
				return interruptResultMsg{false, unknownDeliveryText("stop", l.Project, "the Esc", "p")}
			}
			return interruptResultMsg{false, fmt.Sprintf("stop %s failed: %v", l.Project, err)}
		}
		logActuationEvent(l, "interrupt", act.Tier(), nil)
		return interruptResultMsg{true, fmt.Sprintf("interrupted %s — resume with r", l.Project)}
	}
}

// triggerJudgments fires one judgeCmd per bound loop that's due for
// judgment: idle (the natural checkpoint — a finished turn means its report
// is final, unlike mid-turn) and either never judged, or the loop has moved
// past the cycle it was last judged at (Cycle > Last.AtCycle). A per-session
// in-flight guard (m.judging) ensures at most one judgeCmd runs per loop at
// a time — a slow `claude -p` call can't pile up duplicate judgments across
// 3s refreshes, and a verdict already rendered for the CURRENT cycle isn't
// re-requested just because the loop is still sitting idle.
func (m *Model) triggerJudgments() tea.Cmd {
	var cmds []tea.Cmd
	for _, l := range m.loops {
		if l.Goal.Text == "" || l.State != domain.StateIdle {
			continue
		}
		if l.Last != nil && l.Cycle <= l.Last.AtCycle {
			continue
		}
		if m.judging[l.SessionID] {
			continue
		}
		if m.judging == nil {
			m.judging = make(map[string]bool)
		}
		m.judging[l.SessionID] = true
		cmds = append(cmds, judgeCmd(l))
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

// ── LoopEngine: the cycle ─────────────────────────────────────────────────
//
// The engine is a governance harness, not an agent runtime — this section
// is ONLY the drive-decision + drive-dispatch machinery (mirroring
// triggerJudgments/judgeCmd's existing shape). Deliberately no
// self-chaining: a drive fires at most once per 3s scan tick, the SAME
// cadence triggerJudgments already has.

// triggerDrives fires one driveCmd per Driven loop that engine.ShouldDrive
// decides is due for its next cycle — mirrors triggerJudgments' shape
// exactly: a per-session in-flight guard (here, the SAME m.actuating map
// manual r/i and 429 auto-redrive already share — not a separate map, see
// ShouldDrive's own doc for why joining that interlock is load-bearing),
// batched into one tea.Batch.
//
// Gated by engineEnabledFn() FIRST, before touching m.loops at all — the
// opt-in kill-switch (standing discipline, gate #2 of 2: env
// FLEETOPS_ENGINE=1 AND the per-loop Driven flag, both required). Env
// unset (or anything other than "1") means this returns nil immediately —
// no drive EVER fires, regardless of how many loops are Driven=true.
func (m *Model) triggerDrives() tea.Cmd {
	if !engineEnabledFn() {
		return nil
	}
	var cmds []tea.Cmd
	for _, l := range m.loops {
		if !engine.ShouldDrive(l, l.Driven, m.actuating[l.SessionID]) {
			continue
		}
		m.setActuating(l.SessionID)
		cmds = append(cmds, driveCmd(l))
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

// driveCmd fires ONE engine-driven cycle — a thin wrapper around redriveFn
// (control.Redrive), mirroring autoRedrive429Cmd's shape exactly: load the
// loop's contract, compose the prompt (engine.NextWorkPrompt, REUSED
// verbatim from Slice 0/1 — the pure decision core, not reimplemented
// here), emit the TriggerEngine/ActorAuto history event BEFORE dispatch
// (so a cycle that never returns — a wedged claude — still leaves a
// record it was ATTEMPTED), then send.
//
// REUSES resumeResultMsg verbatim — NOT a new message type, by
// design. The SAME Update handler that already
// clears m.actuating for a manual r/i does exactly what an engine cycle's
// completion needs: clear the interlock (so the NEXT scan tick's
// triggerDrives — or a human's r/i — can act on this session again),
// surface the result in the status line. No engine-specific handler.
//
// registry.Load happens INSIDE this closure, not synchronously in
// triggerDrives/Update — same off-the-render-path discipline this file
// established for gitStatsCmd/detailCacheCmd/fleetOracleCountsCmd (a
// registry record read is real disk I/O, same class of thing those fixes
// were about). ok=false (no record found — shouldn't happen for a Driven
// loop, since Driven implies bound, but defensive) skips the redrive
// entirely rather than composing a prompt from a zero-value contract.
func driveCmd(l domain.Loop) tea.Cmd {
	return func() tea.Msg {
		contract, ok := registry.Load(registryDirFn(), l.SessionID)
		if !ok {
			return resumeResultMsg{sessionID: l.SessionID, ok: false,
				text: fmt.Sprintf("engine: %s has no registry record — cycle skipped", l.Project)}
		}
		prompt := engine.NextWorkPrompt(l, contract)
		_ = events.Append(historyDirFn(), events.Event{
			TS:        time.Now().UnixNano(),
			SessionID: l.SessionID,
			FromState: l.StateString(),
			ToState:   l.StateString(),
			Trigger:   events.TriggerEngine,
			Detail:    fmt.Sprintf("cycle %d", l.Cycle),
			Actor:     events.ActorAuto,
		})
		err := redriveFn(l.SessionID, prompt)
		if err != nil {
			return resumeResultMsg{sessionID: l.SessionID, ok: false,
				text: fmt.Sprintf("engine: cycle %d failed — %v", l.Cycle, err)}
		}
		return resumeResultMsg{sessionID: l.SessionID, ok: true,
			text: fmt.Sprintf("engine: cycle %d — %s", l.Cycle, slugFromGoal(l.Goal.Text, 24))}
	}
}

// judgeCmd asks the oracle to verdict a bound loop's progress against its
// goal, using its full (uncapped) last report, then saves the verdict to
// the registry at the loop's current cycle. Runs off the event loop, same
// non-blocking pattern as the other *Cmd funcs.
//
// event-log-and-notify: a successful verdict also records a
// events.TriggerOracle history event (detail=outcome+atCycle, per the
// slice's spec) — best-effort, swallowed error (see internal/events
// package doc). from_state/to_state are both l.State: a verdict is a
// JUDGMENT about the loop, not itself a state transition (enrichFromRegistry
// may promote State from it on the NEXT scan, which gets its own
// scan-triggered event if it happens — see Model.detectTransitions).
func judgeCmd(l domain.Loop) tea.Cmd {
	return func() tea.Msg {
		lastText, _ := claude.LastAssistantTextFull(l.Path) // ok=false just means an empty report is judged as-is
		verdict, err := judgeFn(l.Goal.Text, l.Cwd, lastText, l.Goal.DoneWhen, l.Goal.Rubric)
		if err != nil {
			return verdictMsg{sessionID: l.SessionID, err: err}
		}
		if err := registry.SaveVerdict(registryDirFn(), l.SessionID, verdict, l.Cycle); err != nil {
			return verdictMsg{sessionID: l.SessionID, err: err}
		}
		_ = events.Append(historyDirFn(), events.Event{
			TS:        time.Now().UnixNano(),
			SessionID: l.SessionID,
			FromState: l.StateString(),
			ToState:   l.StateString(),
			Trigger:   events.TriggerOracle,
			// feat/detail-panel-v2: the reason is appended verbatim (council
			// hard rule: never paraphrased) — see events.ParseOracleDetail,
			// which the VERDICTS block uses to pull it back out. Backward
			// compatible with report.go's existing outcome-only parser
			// (still splits on " at cycle", unaffected by anything after).
			Detail: fmt.Sprintf("%s at cycle %d: %s", verdict.Outcome, l.Cycle, verdict.Reason),
			Actor:  events.ActorAuto,
		})
		return verdictMsg{sessionID: l.SessionID, verdict: verdict}
	}
}

// ── scan-triggered history events + desktop notifications ───────────────
//
// event-log-and-notify's scanner emitter: every loopsMsg, compare each
// session's state THIS scan against its state in the PREVIOUS scan (m.loops,
// still holding the prior snapshot at the point Update calls this) and
// record one history event per session whose signature actually changed —
// edge-triggered, never re-emitted just because a session is still sitting
// in the same state on the next 3s poll. The prev-state map is simply
// m.loops itself (keyed by session id) — no separate package-level map is
// needed, since Update always has last scan's Model available before
// overwriting it with the new one.

// transitionEvent pairs a computed events.Event with whether it should also
// fire a desktop notification — decided synchronously in detectTransitions
// (pure logic + the notifiedAt dedup ledger), then actually dispatched by
// emitTransitionsCmd's tea.Cmd so neither the file write nor the osascript
// exec ever blocks Update.
type transitionEvent struct {
	ev     events.Event
	notify bool
	title  string
	body   string
}

// stateSignature is the granularity detectTransitions compares scan to
// scan — domain.Loop.StateString(), which carries the Stall kind alongside
// StateStalled (see its doc). This is what lets a StallNoOutput→StallGone
// edge (both StateStalled) register as a real, notify-worthy change instead
// of looking like "no change" to a State-only comparison; the notify
// trigger policy explicitly needs exactly this resolution (its "INTO
// StallGone" requirement). Review fix (P2): this is also EXACTLY the string
// recorded as FromState/ToState in the persisted history event — the same
// signature drives both the in-memory edge-trigger decision and what gets
// written to disk, so a no-output→gone incident is no longer invisible to
// `fleetops report`'s FromState!=ToState transition counting.
func stateSignature(l domain.Loop) string {
	return l.StateString()
}

// scanTransitionDetail is the scan-triggered event's detail field: the
// stall kind when the loop landed in StateStalled (the one case the task's
// spec calls out by example), empty otherwise — GatePrompt/verdict reason
// are already carried by their own dedicated callouts/events (renderGate/
// DriftCallout, the oracle event above), so repeating them here would just
// be noise.
func scanTransitionDetail(l domain.Loop) string {
	if l.State == domain.StateStalled {
		return string(l.Stall)
	}
	return ""
}

// notifyDedupWindow: at most one desktop notification per (session, edge)
// within this long — see Model.notifiedAt's doc for the restart caveat.
const notifyDedupWindow = 10 * time.Minute

// notifyTitlePrefix: osascript's `display notification` always shows the
// generic Script Editor icon and can't be pointed at a different one
// without shipping a real .app bundle (out of scope for a CLI tool) — see
// internal/notify's package doc for the fuller writeup and future options.
// A 🚀 prefix on the title is a cheap mitigation:
// makes a fleetops notification visually identifiable in Notification
// Center at a glance, without needing an icon at all.
const notifyTitlePrefix = "🚀 "

// ── feat/auto-redrive-429: opt-in 429 auto-redrive ───────────────────────
//
// The FIRST piece of automation this codebase ships — every condition
// below is a hard gate, not a preference, per the council's "safety bar is
// maximal" constraint:
//   - opt-in only, off by default (autoRedriveEnabledFn — env
//     FLEETOPS_AUTO_REDRIVE_429=1; unset it to disable entirely, no
//     in-app toggle this slice).
//   - Tier 2 ONLY (redriveFn, the headless `claude --resume -p` path) —
//     never types into a terminal, sidestepping the wrong-surface hazard
//     entirely for an automated (unattended) action.
//   - re-checked at fire time against the CURRENT scan snapshot — a loop
//     that recovered during the 5-minute delay is silently skipped, not
//     force-redriven.
//   - a hard lifetime ceiling (3 attempts) and a per-session dedup window,
//     both enforced before ever scheduling.
//   - joins the SAME m.actuating interlock manual r/i actuations use
//     (review fix, P1): a manual resume/inject already in flight skips the
//     auto-redrive rather than racing it, and vice versa.

// autoRedriveEnabledFn checks the opt-in env var — a func var (not a bare
// os.Getenv call) so tests don't need to mutate a real process
// environment variable.
var autoRedriveEnabledFn = func() bool {
	return os.Getenv("FLEETOPS_AUTO_REDRIVE_429") == "1"
}

const (
	// autoRedriveDelay is BOTH the schedule-to-fire delay AND the
	// per-session dedup window (see maybeScheduleAutoRedrive429) — one
	// constant, since "no auto-redrive attempted in the last 5 minutes"
	// and "wait 5 minutes before firing" are the same 5 minutes: you can't
	// schedule a NEW one while the previous one's delay hasn't elapsed.
	autoRedriveDelay = 5 * time.Minute
	// autoRedriveMaxAttempts is the LIFETIME cap on auto-redrive attempts
	// per session (not per day/window) — recounted from the event log on
	// restart (autoRedriveAttemptCount), never reset.
	autoRedriveMaxAttempts = 3
)

// autoRedriveDetailPrefix is the exact detail-field prefix every
// auto-redrive attempt event carries ("auto-redrive-429 attempt N/3" — the
// task's own literal wording) — autoRedriveAttemptCount matches on this
// prefix to recount attempts from the event log.
const autoRedriveDetailPrefix = "auto-redrive-429 attempt "

// autoRedriveScheduledMsg fires autoRedriveDelay after
// maybeScheduleAutoRedrive429 schedules it — see scheduleAutoRedrive429Cmd.
type autoRedriveScheduledMsg struct {
	sessionID string
}

// scheduleAutoRedrive429Cmd is a delayed, one-shot tea.Tick — "must
// survive nothing" per the task: if the TUI quits before this fires, the
// pending retry is simply lost. Honest and safe (no on-disk "pending
// retry" record to leak or double-fire on the next launch).
func scheduleAutoRedrive429Cmd(sessionID string) tea.Cmd {
	return tea.Tick(autoRedriveDelay, func(time.Time) tea.Msg {
		return autoRedriveScheduledMsg{sessionID: sessionID}
	})
}

// maybeScheduleAutoRedrive429 is the edge-triggered policy gate — called
// from detectTransitions for every loop whose scan-detected transition
// might be "entering StallRateLimit" (enteredRateLimit), the exact same
// edge notify's gate/gone triggers hook into. Returns nil (schedule
// nothing) unless EVERY condition holds:
//  1. enteredRateLimit is true (a real edge THIS scan, not "still
//     rate-limited from before").
//  2. autoRedriveEnabledFn() — the opt-in kill switch.
//  3. l.State is neither StateFailed nor StateGate — structurally
//     unreachable here (l.State is StateStalled by construction of the
//     rate-limit edge), but checked explicitly per the task's own wording
//     (defense in depth, matching this codebase's belt-and-suspenders
//     style elsewhere).
//  4. no auto-redrive scheduled for this session within the last
//     autoRedriveDelay (dedup).
//  5. fewer than autoRedriveMaxAttempts lifetime attempts so far.
//
// On success: records the schedule time (closing the dedup window),
// updates the status line ("auto: re-driving <label> in 5m (attempt
// N/3)"), and returns scheduleAutoRedrive429Cmd's tea.Tick.
func (m *Model) maybeScheduleAutoRedrive429(l domain.Loop, enteredRateLimit bool, now time.Time) tea.Cmd {
	if !enteredRateLimit || !autoRedriveEnabledFn() {
		return nil
	}
	if l.State == domain.StateFailed || l.State == domain.StateGate {
		return nil
	}
	if last, ok := m.autoRedriveScheduledAt[l.SessionID]; ok && now.Sub(last) < autoRedriveDelay {
		return nil
	}
	attempts := m.autoRedriveAttemptCount(l.SessionID)
	if attempts >= autoRedriveMaxAttempts {
		return nil
	}
	if m.autoRedriveScheduledAt == nil {
		m.autoRedriveScheduledAt = make(map[string]time.Time)
	}
	m.autoRedriveScheduledAt[l.SessionID] = now
	m.status, m.statusKind = fmt.Sprintf("auto: re-driving %s in 5m (attempt %d/%d)", l.Project, attempts+1, autoRedriveMaxAttempts), statusNeutral
	return scheduleAutoRedrive429Cmd(l.SessionID)
}

// autoRedriveAttemptCount returns sessionID's lifetime auto-redrive
// attempt count, lazily seeding Model.autoRedriveAttempts from the event
// log (counting TriggerActuation events whose Detail has
// autoRedriveDetailPrefix) the FIRST time it's asked about a given
// session — so a fleetops restart recounts from disk instead of
// silently resetting the ceiling to 0.
func (m *Model) autoRedriveAttemptCount(sessionID string) int {
	if m.autoRedriveAttempts == nil {
		m.autoRedriveAttempts = make(map[string]int)
	}
	if n, ok := m.autoRedriveAttempts[sessionID]; ok {
		return n
	}
	evs, _ := events.Read(historyDirFn(), sessionID)
	n := 0
	for _, ev := range evs {
		if ev.Trigger == events.TriggerActuation && strings.HasPrefix(ev.Detail, autoRedriveDetailPrefix) {
			n++
		}
	}
	m.autoRedriveAttempts[sessionID] = n
	return n
}

// loopBySessionID finds sessionID in m.loops — used by
// autoRedriveScheduledMsg's handler to re-check the CURRENT (latest scan)
// state before firing a delayed auto-redrive.
func (m Model) loopBySessionID(sessionID string) (domain.Loop, bool) {
	for _, l := range m.loops {
		if l.SessionID == sessionID {
			return l, true
		}
	}
	return domain.Loop{}, false
}

// autoRedriveResultMsg reports one auto-redrive attempt's outcome.
type autoRedriveResultMsg struct {
	sessionID string
	project   string
	attempt   int
	ok        bool
}

// autoRedrive429Cmd fires attempt N's headless Tier-2 redrive (Tier 2
// ONLY — see this section's doc) and records the attempt as a history
// event regardless of outcome. actor=auto (per the task's explicit
// wording) — distinct from every OTHER actuation event in this codebase
// (always actor=human): this one really is unattended. The FINAL
// (autoRedriveMaxAttempts-th) attempt always triggers the "exhausted"
// desktop notification (see autoRedriveResultMsg's handler) regardless of
// whether THIS attempt's transport call itself errored — there's nothing
// more to schedule either way (the ceiling in maybeScheduleAutoRedrive429
// already prevents a 4th attempt), so the human needs to know automated
// retries are done, whether the last one technically "succeeded" (sent
// fine, API still says no) or not.
// Review fix (P2): the exhausted-notification DECISION moved to Update's
// autoRedriveResultMsg handler (keyed on attempt==ceiling, not on err) —
// see that handler's doc for why, and autoRedriveExhaustedNotifyCmd for the
// actual (still async) notify.Send call.
func autoRedrive429Cmd(l domain.Loop, attempt int) tea.Cmd {
	return func() tea.Msg {
		prompt, _ := claude.LastUserPrompt(l.Path) // an empty/absent prior prompt still redrives — same tolerance as resumeCmd
		err := redriveFn(l.SessionID, prompt)
		_ = events.Append(historyDirFn(), events.Event{
			TS:        time.Now().UnixNano(),
			SessionID: l.SessionID,
			FromState: l.StateString(),
			ToState:   l.StateString(),
			Trigger:   events.TriggerActuation,
			Detail:    fmt.Sprintf("%s%d/%d", autoRedriveDetailPrefix, attempt, autoRedriveMaxAttempts),
			Actor:     events.ActorAuto,
		})
		return autoRedriveResultMsg{sessionID: l.SessionID, project: l.Project, attempt: attempt, ok: err == nil}
	}
}

// autoRedriveExhaustedNotifyCmd sends the "auto-redrive exhausted" desktop
// notification off the event loop. Split out from autoRedrive429Cmd
// (review fix, P2) because the DECISION to notify needs Model.notifiedAt's
// dedup ledger (shouldNotify), which only a pointer-receiver method on
// Model can mutate — autoRedrive429Cmd itself has no Model access, matching
// this codebase's established shape for actuation cmds (they close over a
// Loop, never a Model). The actual notify.Send call stays async here, same
// discipline as every other notification in this codebase.
func autoRedriveExhaustedNotifyCmd(project string) tea.Cmd {
	return func() tea.Msg {
		_ = notifySendFn(notifyTitlePrefix+"fleetops · auto-redrive exhausted", project)
		return nil
	}
}

// ── LoopEngine: opt-in kill-switch seam ──────────────────────────────────
//
// The engine is reachable ONLY behind an explicit opt-in — this env gate
// AND the "n" wizard's engine-drive choice (see registry.BindSpec.Driven).
// No engine cycle EVER fires unless BOTH are true. Off by default.
//
// Mirrors autoRedriveEnabledFn exactly (a func var, not a bare os.Getenv
// call, so tests don't need to mutate a real process environment
// variable) — same "opt-in, off by default, no in-app toggle" shape as
// the 429 auto-redrive gate above, deliberately: this is the SAME class of
// decision (an automated action needs an explicit, boring, well-known
// escape hatch), not a new pattern invented for the engine.
var engineEnabledFn = func() bool {
	return os.Getenv("FLEETOPS_ENGINE") == "1"
}

// shouldNotify applies the dedup ledger for sessionID's edge ("gate" or
// "gone"), recording now as the edge's last-notified time whenever it
// allows a notification through — pointer receiver so the decision (and the
// ledger write) actually persists onto the Model Update returns, same
// idiom as triggerJudgments/setActuating.
func (m *Model) shouldNotify(sessionID, edge string, now time.Time) bool {
	key := sessionID + ":" + edge
	if last, ok := m.notifiedAt[key]; ok && now.Sub(last) < notifyDedupWindow {
		return false
	}
	if m.notifiedAt == nil {
		m.notifiedAt = make(map[string]time.Time)
	}
	m.notifiedAt[key] = now
	return true
}

// detectTransitions compares m.loops (the PREVIOUS scan, still held at the
// point Update calls this — see loopsMsg's handler) against newLoops (the
// scan that just arrived) and returns one transitionEvent per session whose
// stateSignature changed, PLUS (review fix, P2) a synthetic edge for a
// session's FIRST appearance (no entry in the previous scan) when it's
// ALREADY sitting in StateGate — see seedFirstAppearanceGate's doc for why.
// Every OTHER first appearance is still deliberately NOT a transition —
// there's no from_state to compare against, and treating every ordinary
// loop present at fleetops startup as a "transition" would spam the
// history log with meaningless "unknown→X" noise on every restart.
//
// Notify trigger policy (the task's exact spec): fire ONLY on transitions
// INTO StateGate or INTO StallGone, each independently dedup-gated via
// shouldNotify. Severity floor: nothing else notifies yet (done/drift/429
// are explicitly out of scope for this slice).
func (m *Model) detectTransitions(newLoops []domain.Loop, now time.Time) ([]transitionEvent, []tea.Cmd) {
	prev := make(map[string]domain.Loop, len(m.loops))
	for _, l := range m.loops {
		prev[l.SessionID] = l
	}

	var out []transitionEvent
	var cmds []tea.Cmd
	for _, l := range newLoops {
		before, ok := prev[l.SessionID]
		if !ok {
			if te, seeded := m.seedFirstAppearanceGate(l, now); seeded {
				out = append(out, te)
			}
			continue
		}
		if stateSignature(before) == stateSignature(l) {
			continue
		}

		te := transitionEvent{ev: events.Event{
			TS:        now.UnixNano(),
			SessionID: l.SessionID,
			FromState: before.StateString(),
			ToState:   l.StateString(),
			Trigger:   events.TriggerScan,
			Detail:    scanTransitionDetail(l),
			Actor:     events.ActorSystem,
		}}

		enteredGate := before.State != domain.StateGate && l.State == domain.StateGate
		enteredGone := l.State == domain.StateStalled && l.Stall == domain.StallGone &&
			!(before.State == domain.StateStalled && before.Stall == domain.StallGone)
		// feat/auto-redrive-429: the SAME edge notify's gate/gone triggers
		// hook into — see maybeScheduleAutoRedrive429's doc for the full
		// policy gate (opt-in, ceiling, dedup).
		enteredRateLimit := l.State == domain.StateStalled && l.Stall == domain.StallRateLimit &&
			!(before.State == domain.StateStalled && before.Stall == domain.StallRateLimit)
		switch {
		case enteredGate && m.shouldNotify(l.SessionID, "gate", now):
			te.notify = true
			te.title = notifyTitlePrefix + "fleetops · GATE"
			te.body = fmt.Sprintf("%s: %s", l.Project, l.GatePrompt)
		case enteredGone && m.shouldNotify(l.SessionID, "gone", now):
			te.notify = true
			te.title = notifyTitlePrefix + "fleetops · loop gone"
			te.body = l.Project
		}
		if cmd := m.maybeScheduleAutoRedrive429(l, enteredRateLimit, now); cmd != nil {
			cmds = append(cmds, cmd)
		}
		out = append(out, te)
	}
	return out, cmds
}

// seedFirstAppearanceGate is the review fix (P2) for a restart-timing gap:
// without this, a fleetops restart's first scan sees every loop as a
// "first appearance" (no previous scan to diff against — see
// detectTransitions' doc), so an ALREADY-open gate from before the restart
// would never generate an edge and would silently never notify — directly
// contradicting Model.notifiedAt's own doc comment, which claimed a restart
// re-notifies a still-open gate. This seeds exactly that: a synthetic
// ""→"gate" history event (FromState="" — same "nothing to compare against
// yet" convention as registry.BindPending's spawn event) for a
// first-appearance loop that's already gated, still subject to the normal
// dedup ledger (so a restart within the same 10-minute window as an
// already-delivered notification does NOT re-notify). Judgment call: the
// task's review comment scoped this to StateGate specifically; a
// first-appearance loop already in StallGone is not seeded the same way —
// flagged for confirmation, not implemented, since restart-time
// already-gone is a narrower, less clearly human-actionable edge (the
// human didn't just leave a decision pending, the loop simply died at some
// unknown point before this cockpit ever started watching it).
func (m *Model) seedFirstAppearanceGate(l domain.Loop, now time.Time) (transitionEvent, bool) {
	if l.State != domain.StateGate {
		return transitionEvent{}, false
	}
	te := transitionEvent{ev: events.Event{
		TS:        now.UnixNano(),
		SessionID: l.SessionID,
		FromState: "",
		ToState:   l.StateString(),
		Trigger:   events.TriggerScan,
		Actor:     events.ActorSystem,
	}}
	if m.shouldNotify(l.SessionID, "gate", now) {
		te.notify = true
		te.title = notifyTitlePrefix + "fleetops · GATE"
		te.body = fmt.Sprintf("%s: %s", l.Project, l.GatePrompt)
	}
	return te, true
}

// emitTransitionsCmd returns a tea.Cmd that performs the actual history-log
// writes and desktop-notification sends for transitions — off the event
// loop, so neither ever blocks Update. Best-effort throughout: every
// events.Append/notifySendFn error is swallowed (see internal/events and
// internal/notify package docs) — a history-log or notification failure
// must never surface in the TUI or otherwise interrupt the fleet loop. nil
// (a documented valid tea.Cmd) when there's nothing to do, so callers can
// tea.Batch it unconditionally.
func emitTransitionsCmd(transitions []transitionEvent) tea.Cmd {
	if len(transitions) == 0 {
		return nil
	}
	return func() tea.Msg {
		dir := historyDirFn()
		for _, te := range transitions {
			_ = events.Append(dir, te.ev)
			if te.notify {
				_ = notifySendFn(te.title, te.body)
			}
		}
		return nil
	}
}

// unknownDeliveryText is the operator line for a Tier 1h send whose delivery is
// UNKNOWN (control.ErrSendDeliveryUnknown — osascript was killed at the
// actuationTimeout deadline, so the write may or may not have reached the pty).
//
// It exists because k/p/a have no Tier 2 and so must format this themselves,
// and because the three lines must not drift apart — they are the same claim
// about the same uncertainty.
//
// The wording is load-bearing, not decoration. These call sites used to render
// it as "kill X failed: <err>", which asserts a definite outcome in the prefix
// while the error body says the outcome is unknown. An operator scanning the
// status line reads the prefix, and for `k` "failed" is an invitation to press
// it again — reintroducing by hand exactly the double-send that
// ErrSendDeliveryUnknown exists to prevent. So the uncertainty leads, and the
// line ends by naming the key NOT to press until they have looked.
//
// verb is the display verb ("kill"), what is the payload in the operator's own
// vocabulary ("the \"/exit\""), and key is the keypress to warn off ("k").
func unknownDeliveryText(verb, project, what, key string) string {
	return fmt.Sprintf("%s %s: delivery UNKNOWN — %s may or may not have landed (the host send timed out). Attach (↵) and check before pressing %s again", verb, project, what, key)
}

// logActuationEvent best-effort records an actor=human actuation event —
// action is what the human triggered ("resume"/"inject"/"approve"/
// "interrupt"/"kill" — "spawn" is logged separately, see
// registry.BindPending's doc, since no session_id exists yet at spawn
// time), tier is which actuation path was actually used: "tier1" (multiplexer)
// or "tier1h" (in-place host send), both reported by the Actuator itself, plus
// "tier2" for the headless re-drive and "terminal"/"engine" for the take-over
// and engine-kill paths that bypass actuation entirely.
// from_state/to_state are both l.State: an actuation attempt is a record of
// WHAT A HUMAN DID, not itself a state transition (the next scan is what
// would reclassify the loop, and that gets its own scan-triggered event if
// it happens). Only called at a point where a tier was actually dispatched
// — the early "no backend"/"ambiguous" refusal branches never reach a tier,
// so callers simply don't call this for those (nothing was "taken" to log).
//
// err is the actuation's result — nil means it was confirmed dispatched.
// It is classified into the event's STRUCTURED Outcome field (see
// events.Outcome*), which is what derivations must read; Detail keeps its
// existing human-readable "<action> <tier> ok|failed: <err>" shape for the
// EVENTS panel and `fleetops report`, and is no longer load-bearing for any
// derivation (issue #50: mostRecentActuationIsKill used to match a prefix
// that a FAILED kill's detail also satisfies).
//
// ErrSendDeliveryUnknown (the host send timed out) is classified
// OutcomeUnknown, not OutcomeFailed — the same distinction killCmd already
// makes in what it tells the human ("delivery UNKNOWN — it may or may not
// have landed"). Recording it as "failed" would assert something we did not
// observe, which is the exact class of over-claim this field exists to stop.
func logActuationEvent(l domain.Loop, action, tier string, err error) {
	outcome := actuationOutcome(err)
	detail := action + " " + tier
	if err == nil {
		detail += " ok"
	} else {
		detail += " failed: " + err.Error()
	}
	_ = events.Append(historyDirFn(), events.Event{
		TS:        time.Now().UnixNano(),
		SessionID: l.SessionID,
		FromState: l.StateString(),
		ToState:   l.StateString(),
		Trigger:   events.TriggerActuation,
		Detail:    detail,
		Actor:     events.ActorHuman,
		Outcome:   outcome,
	})
}

// actuationOutcome classifies an actuation's error into an events.Outcome*
// value — the one place the "a timeout is not a failure" rule is written.
func actuationOutcome(err error) string {
	switch {
	case err == nil:
		return events.OutcomeOK
	case errors.Is(err, control.ErrSendDeliveryUnknown):
		return events.OutcomeUnknown
	default:
		return events.OutcomeFailed
	}
}

// pagerCmd builds the argv for the "o" key's log pager: -R renders color
// codes, +G jumps to the end (most recent activity first), and a custom
// prompt tells the human how to get back — otherwise "q returns you to the
// cockpit" isn't obvious once less has taken over the whole screen.
func pagerCmd(path string) []string {
	// -M shows the LONG prompt on the bottom line at all times, and -PM sets
	// that long prompt's text. The attached -P form matters: with
	// `--prompt=log:...` less eats the first character ("l") as the
	// prompt-type selector and the hint never renders (user-reported).
	return []string{"less", "-R", "+G", "-M", "-PMfleetops log — q to return (%pB\\%)", path}
}

func (m Model) selected() (domain.Loop, bool) {
	loops := m.visibleLoops()
	if m.cursor >= 0 && m.cursor < len(loops) {
		return loops[m.cursor], true
	}
	return domain.Loop{}, false
}

// injectHeadlessFallbackEligible reports whether an ambiguous inject target
// (refuseIfAmbiguous's dead-end — N sessions share one cwd, no session-registry
// tty to disambiguate by, e.g. orca, which has no CLI tty/tab-by-session
// mapping at all) may instead fall through to the Tier 2 headless-exact
// re-drive (feat/inject-headless-exact-fallback) once the human submits a
// prompt, rather than being refused outright at "i" keypress time. This
// gates ONLY whether the keypress-time guard lets the human type a prompt in
// the first place — the actual routing mechanism is unchanged: sendPromptCmd's
// existing Tier1→Tier2 fallthrough already sends any tier-1 miss to
// `claude --resume <SessionID> -p <prompt>` by the loop's EXACT SessionID
// (control.Redrive — the same trusted, session-unique, same-transcript
// mechanism the 429/StallGone auto-redrive path already relies on), so it
// can't misroute regardless of how many sibling sessions share the cwd.
//
// Deliberately an explicit ALLOWLIST, not a denylist that only excludes
// StateRunning — so any future/unknown domain.LoopState value fails CLOSED
// to the pre-existing dead-end refusal (the ADR's own fail-closed
// discipline) instead of silently gaining a fallback nobody vetted it for:
//
//   - StateIdle: turn complete, nothing mid-flight — a headless resume turn
//     is exactly what a human would otherwise type by hand once they attach.
//   - StateStalled: silently stuck (429/no-output/token-out) — the SAME
//     class of loop StallGone already always headless-redrives
//     UNconditionally, ambiguity or not (see sendPromptCmd's Tier policy
//     doc); this extends that already-trusted mechanism to the narrower
//     "stalled AND the cwd chain is ambiguous too" case.
//   - NEVER StateRunning: a live mid-turn session. `claude --resume`
//     concurrent with an in-progress interactive turn is explicitly
//     unverified territory (docs/adr-vendor-independent-actuation.md §4:
//     "whether the open TUI re-renders it is unknown") — risks conflicting
//     with or forking the transcript. Stays the dead-end refusal: attach
//     (↵) and act manually.
//   - Everything else (StateGate, StateDrift, StatePaused, and the
//     Terminal() states StateDone/StateFailed/StateKilled) is OUT OF SCOPE
//     for this slice, not silently included — GATE/DRIFT carry their own
//     semantics (DRIFT already has its own hint-prompt re-drive flow via
//     "r", untouched by this feature) and were never requested; the
//     Terminal states are already refused earlier by their own dedicated
//     guards (StateFailed/StateKilled) before this is ever reached, or are
//     simply not valid inject targets to begin with (StateDone).
//   - StallGone loops never reach this function at all — the "i" handler's
//     own `sel.Stall != domain.StallGone` gate already routes them straight
//     to Tier 2 unconditionally, ambiguity guard skipped entirely.
func injectHeadlessFallbackEligible(state domain.LoopState) bool {
	switch state {
	case domain.StateIdle, domain.StateStalled:
		return true
	default:
		return false
	}
}

// refuseIfAmbiguous is the P0-1/P0-2 actuation guard: Locate/LocateClaude
// match a terminal surface by ProjectDir (a directory), but loops are
// SESSIONS — when more than one loop in the current fleet shares l's
// directory, a typed/destructive action (resume/kill/approve/interrupt)
// could silently land on the wrong one (freshest-tab tiering picks whichever
// surface looks healthiest, not necessarily the loop the human selected).
// Callers must check this immediately before dispatching resumeCmd/killCmd/
// approveCmd/interruptCmd — NOT attachCmd/o, which are read-only/navigation
// and safe regardless of which sibling surface they land on.
func (m Model) refuseIfAmbiguous(l domain.Loop) (msg string, ambiguous bool) {
	n := m.sameProjectDirCount(l.ProjectDir)
	if n <= 1 {
		return "", false
	}
	// The refusal itself is honest — fail-closed beats killing the wrong
	// loop. The REMEDY has to be too, which means consulting l rather than
	// emitting a constant; see ambiguityRemedy.
	return fmt.Sprintf("ambiguous: %d loops share %s's directory, none has a session-registry tty — %s", n, l.Project, ambiguityRemedy(l)), true
}

// ambiguityRemedy picks what to actually advise for the loop that hit the
// cwd-ambiguity refusal (issue #49, part 2).
//
// This used to be a compile-time constant — "attach (↵) or run `fleetops
// hooks install` so injects can target by session" — emitted identically
// for a live loop and a dead one, and for a session fleetops spawned as
// well as one a human started elsewhere. Both halves were wrong for the
// reported case (a fleetops-spawned loop, dead 40 minutes, oracle-drifted):
// attaching cannot help a process that is gone, and `hooks install` cannot
// retroactively register an ALREADY-RUNNING session, because registration
// happens at SessionStart. The operator was told to do two things that
// could not change the outcome.
//
// The branches, in precedence order, each on a fact the caller already
// handed us:
//
//   - process gone (Stall == StallGone) — nothing to reach at all, so
//     neither remedy applies. What DOES work is the headless path: "r"/"i"
//     re-drive via `claude --resume <exact session id> -p`, which needs no
//     surface and no live process, and "x" retires the row.
//   - fleetops-spawned (BoundAt non-zero ⇔ a registry.Record exists ⇔ this
//     loop came through the "n" wizard's spawn → WritePending → BindPending
//     path, which is the only way a Record is ever created). Installing
//     hooks cannot fix THIS session; only a fresh session registers. Attach
//     is genuinely available, so offer it and say what actually restores
//     session-exact targeting.
//   - otherwise: an observed session with no registry entry, alive. The
//     original advice is correct here and is kept verbatim.
//
// Deliberately NOT consulted: whether the hooks are in fact installed.
// Nothing in the TUI knows that today, and guessing would just move the
// dishonesty. The owned branch sidesteps it by not advising hooks at all.
//
// Note on the first branch: every current caller pre-filters StallGone
// before reaching refuseIfAmbiguous (and the one that did not — "r" on a
// StateDrift loop — stops producing dead loops now that drift no longer
// survives the liveness pass). It is kept because this is a helper shared
// by seven call sites each carrying its own hand-written pre-filter, and
// the invariant belongs in the one place that authors the sentence rather
// than in seven places that must remember to.
func ambiguityRemedy(l domain.Loop) string {
	if l.Stall == domain.StallGone {
		return "and this one's process is already gone, so neither reaches it — r/i re-drive it headlessly, or x to remove it"
	}
	if !l.BoundAt.IsZero() {
		return "attach (↵) to act manually; `fleetops hooks install` can't help — registration happens at session start, so only a fresh loop gets session-exact targeting"
	}
	return "attach (↵) or run `fleetops hooks install` so injects can target by session"
}

// sameProjectDirCount counts how many loops in the current fleet (not just
// the visible/filtered subset — ambiguity is a property of the WHOLE fleet
// sharing a terminal surface, filtering doesn't change that) have this exact
// encoded ProjectDir.
func (m Model) sameProjectDirCount(projectDir string) int {
	n := 0
	for _, l := range m.loops {
		if l.ProjectDir == projectDir {
			n++
		}
	}
	return n
}

// ttyPathPlausible reports whether sel has a session registry entry with a
// non-empty tty — if so, actuation will TRY the session-unique tty-dispatch
// path FIRST (control.ResolveActuationTarget's Tier 1a), so the keypress-time
// refuseIfAmbiguous check (which only protects the cwd-based Tier 1b) is
// skipped here as an optimistic UX shortcut: no sense showing a "N loops
// share this directory" refusal for a session that's about to be dispatched
// by tty, not cwd.
//
// IMPORTANT — this is NOT the safety boundary. ttyPathPlausible only checks
// that a registry entry EXISTS with a tty; it does not (and, being a
// synchronous read in Update, cannot) validate that the tty↔pid BINDING
// still holds (see F4 hardening review, and P1-1's pidTTYFn fix). The real
// guarantee lives downstream, off the event loop, inside
// control.ResolveActuationTarget: it re-validates the binding right before
// committing to Tier 1a, and falls through to the cwd chain (Tier 1b —
// Resolve()+LocateClaude) whenever that check fails. LocateClaude carries
// its OWN internal ">1 match" ambiguity refusal, so a genuinely ambiguous
// loop whose tty turned out to be stale/recycled still gets refused —
// skipping the guard here can only cost a less-specific error message
// ("no unambiguous claude surface" instead of "N loops share..."), never a
// wrong-terminal misroute. See internal/control/actuation_test.go and this
// package's TestSendPromptCmd_TTYPlausibleButBindingFails_* /
// TestApproveCmd_TierOneFailsAmbiguously_* for the end-to-end proof.
//
// This function itself is a plain local file read (internal/sessions.ReadSession),
// not an exec call, so it's safe to run synchronously in Update — unlike the
// tty-binding re-check, which only happens later, off the event loop, inside
// the tea.Cmd (see control.ResolveActuationTarget).
func (m Model) ttyPathPlausible(sel domain.Loop) bool {
	entry, err := sessions.ReadSession(sessionsDirFn(), sel.SessionID)
	return err == nil && entry.TTY != ""
}

// setActuating marks sessionID as having an in-flight resume/inject send —
// see Model.actuating's doc. Lazily inits the map, same pattern as
// triggerJudgments' m.judging.
func (m *Model) setActuating(sessionID string) {
	if m.actuating == nil {
		m.actuating = make(map[string]bool)
	}
	m.actuating[sessionID] = true
}

// withoutHidden drops every loop in the persisted hide-set — applied both at
// hide/delete-keypress time (prune m.loops in place) and to each scan's fresh
// result in the loopsMsg handler (so a rescan, or a restart, can't resurrect
// a hidden loop). See Model.hidden.
func (m Model) withoutHidden(loops []domain.Loop) []domain.Loop {
	if len(m.hidden) == 0 {
		return loops
	}
	out := make([]domain.Loop, 0, len(loops))
	for _, l := range loops {
		if !m.hidden[l.SessionID] {
			out = append(out, l)
		}
	}
	return out
}

// hideSession tombstones sessionID into the persisted hide-set, then prunes it
// out of the fleet list and clamps the cursor onto a still-visible row. Shared
// by the "d" (hide) and "x" (delete) keys — both end in exactly this. Returns
// the persistence error (nil on success): the in-memory hide + prune happen
// REGARDLESS, so even a persist failure still hides the loop for this session
// (it just won't survive a restart) — fail-open toward the human's intent.
func (m *Model) hideSession(sessionID string) error {
	set, err := hidden.Add(hiddenFileFn(), sessionID)
	if err != nil {
		if m.hidden == nil {
			m.hidden = make(map[string]bool)
		}
		m.hidden[sessionID] = true // persist failed — still hide in memory
	} else {
		m.hidden = set // disk is source of truth; adopt what it now holds
	}
	m.loops = m.withoutHidden(m.loops)
	// One visibleLoops() call, not two: under an active filter it allocates and
	// repopulates a fresh slice over every loop, and the clamp needs only its
	// length.
	if visible := len(m.visibleLoops()); m.cursor >= visible {
		m.cursor = maxInt(0, visible-1)
	}
	return err
}

// visibleLoops is what the table/cursor/actions operate on: all loops, or
// the subset matching the filter — the applied one (m.filterQuery) normally,
// or the live in-progress query while modeFiltering is actively typing (so
// the table live-filters as you type, before enter commits it).
func (m Model) visibleLoops() []domain.Loop {
	query := m.filterQuery
	if m.mode == modeFiltering {
		query = m.input.Value()
	}
	if query == "" {
		return m.loops
	}
	out := make([]domain.Loop, 0, len(m.loops))
	for _, l := range m.loops {
		if matchesFilter(l, query) {
			out = append(out, l)
		}
	}
	return out
}

// matchesFilter is the "/" filter's matching rule: a case-insensitive
// substring match against Project, SessionID, the STATE label, or the Stall
// kind — the fields a human would actually search by.
func matchesFilter(l domain.Loop, query string) bool {
	if query == "" {
		return true
	}
	q := strings.ToLower(query)
	for _, field := range []string{l.Project, l.SessionID, stateLabel(l), string(l.Stall)} {
		if strings.Contains(strings.ToLower(field), q) {
			return true
		}
	}
	return false
}

// termWidth is the usable render width, guarding against 0 before the first
// tea.WindowSizeMsg arrives.
func (m Model) termWidth() int {
	if m.w <= 0 {
		return 80
	}
	return m.w
}

// termHeight is the usable render height, guarding against 0 before the
// first tea.WindowSizeMsg arrives (matches termWidth's guard) — the default
// (24) is the traditional terminal height, a reasonable frame to size tests
// and any pre-WindowSizeMsg render against.
func (m Model) termHeight() int {
	if m.h <= 0 {
		return 24
	}
	return m.h
}

// counts tallies loop states, total spend, and oracle judgment share for the
// summary band and keybar. judged/good are over bound loops that have been
// judged at least once (Last != nil): good counts a latest outcome of
// done or progress — "the loop is fine" — vs rejected/drift.
func (m Model) counts() (total, running, stalled, idle, gated, totalTokens, judged, good int) {
	total = len(m.loops)
	for _, l := range m.loops {
		switch l.State {
		case domain.StateRunning:
			running++
		case domain.StateStalled:
			stalled++
		case domain.StateIdle:
			idle++
		case domain.StateGate:
			gated++
		}
		totalTokens += l.TokensSpent

		if l.Last != nil {
			judged++
			if l.Last.Outcome == domain.OutcomeDone || l.Last.Outcome == domain.OutcomeProgress {
				good++
			}
		}
	}
	return
}

// View composes the fleet cockpit's two-pane layout ("layout B"): a bordered
// FLEET list panel (compact identity+state — see renderListRow) and, at
// wide-enough widths, a bordered DETAIL panel showing everything about the
// SELECTED loop (renderDetail — GOAL/ORACLE/RUBRIC/BUDGET/N-I/LAST/CWD/LOG/
// TAIL/callout, i.e. every column that used to live in the old flat table's
// wide columns before this redesign). Three width-driven layouts (see
// layoutModeFor): wide (side-by-side), stacked (list above detail), and
// list-only (no detail pane at all). The whole frame is height-bounded to
// m.termHeight() — required because cmd/fleetops/main.go runs in
// tea.WithAltScreen() mode, where content beyond the terminal height is
// genuinely invisible (no scrollback), not just visually inconvenient — see
// panelHeight below and TestView_NoLineExceedsTerminalWidth's height checks.
func (m Model) View() string {
	width := m.termWidth()
	height := m.termHeight()
	var b strings.Builder

	b.WriteString(renderHeaderBlock(m, width))
	b.WriteString("\n")
	b.WriteString(renderRule(width))
	b.WriteString("\n")

	// Chrome accounted for above (the 3-line header block + rule =
	// topChromeLines) and below (just the bottom line = bottomChromeLines)
	// is a FIXED line count regardless of content — renderBottomLine always
	// returns exactly one line (even if blank), which is what makes this
	// budget a real guarantee rather than an estimate. Whatever's left goes
	// to the panel area, floored so the UI never collapses to nothing at an
	// absurdly short terminal — layoutStacked needs a taller floor than the
	// other two modes since it renders two bordered panels, not one (see
	// stackedPanelHeightFloor). feat/top-hint-grid removed the bottom
	// keybar (its keybindings moved into the header block's hint grid) and
	// the blank lines around both chrome regions, handing every freed line
	// to the panel area.
	mode := layoutModeFor(width)
	floor := panelHeightFloor
	if mode == layoutStacked {
		floor = stackedPanelHeightFloor
	}
	panelHeight := height - topChromeLines - bottomChromeLines
	if panelHeight < floor {
		panelHeight = floor
	}

	switch mode {
	case layoutWide:
		b.WriteString(m.renderWide(width, panelHeight))
	case layoutStacked:
		b.WriteString(m.renderStacked(width, panelHeight))
	default:
		b.WriteString(m.renderListOnly(width, panelHeight))
	}
	b.WriteString("\n")
	b.WriteString(m.renderBottomLine())
	return b.String()
}

// renderBottomLine is the whole bottom chrome (feat/top-hint-grid removed
// the keybar entirely — keybindings live in the header block's hint grid
// now): the active wizard/filter/inject prompt, or (in modeNormal) the last
// action's status. ALWAYS exactly one line, blank when there's nothing to
// show — unlike the pre-two-pane View, which omitted the line entirely when
// status was "", this keeps the bottom chrome's line count a fixed constant
// (bottomChromeLines) instead of one that depends on frame content.
func (m Model) renderBottomLine() string {
	switch m.mode {
	case modePrompting:
		return renderNewLoopPrompt(m)
	case modeFiltering:
		return renderFilterPrompt(m.input)
	case modeInjecting:
		return renderInjectPrompt(m)
	case modeDriftHint:
		return renderDriftHintPrompt(m)
	default:
		return renderStatusLine(m.status, m.statusKind)
	}
}

// ── header block (feat/top-hint-grid) ────────────────────────
//
// The insight: keybinding hints belong at the TOP, near
// where the eyes already live (the FLEET list), not in a bottom keybar that
// forces constant eye travel and competes with the status/wizard line. The
// single-line header (logo left, LIVE/uptime right) and the separate
// summary band are replaced by ONE 3-line block split into three
// side-by-side regions — LEFT (identity), MIDDLE (fleet stats), RIGHT (a
// keybinding hint grid) — and the bottom keybar (renderKeybar, since
// removed) is gone entirely; every keybinding it used to show now lives in
// the header's hint grid instead.

// headerLines is the header block's fixed height — every region below is
// padded/clipped to exactly this many lines (headerRegionLines) so
// lipgloss.JoinHorizontal always composes a clean 3-row grid regardless of
// how much each region actually has to say this frame.
const headerLines = 3

func renderRule(width int) string {
	return lipgloss.NewStyle().Foreground(cLine).Render(strings.Repeat("─", width))
}

// renderHeaderBlock composes the 3-line header. Width priority order
// (fix/exit-gate-ux, UX judge items 2+3 — FLIPS the priority feat/top-hint-
// grid originally shipped, which the judge caught inverted at ~80 cols:
// hint columns rendered full while the live fleet-stats band truncated to
// "fleet 10 · 1 ru…"):
//  1. the GATE/STALLED attention badge — must NEVER be ansi-truncated (the
//     sole cue in narrow/list-only mode); falls back to an abbreviated
//     form before it would ever clip (see headerMiddleContent.render).
//  2. the fleet-stats band (fleet counts + budget/oracle%) — must stay
//     FULLY legible; this live band is why the window is open at all.
//  3. hint-grid columns — dropped right-to-left first, since keybindings
//     are learnable-once (unlike the badge/stats, which are live state).
//  4. LEFT (logo/uptime) — lowest priority, shrunk last, and only once
//     MIDDLE's hard floor (stats + the badge's ABBREVIATED form) wouldn't
//     otherwise fit at all.
func renderHeaderBlock(m Model, width int) string {
	content := m.headerMiddleContent()
	middleMin := content.minWidth()
	middleIdeal := content.idealWidth()

	leftWidth := headerLeftWidth
	if leftWidth > width {
		leftWidth = width
	}
	if width-leftWidth < middleMin {
		leftWidth = width - middleMin
		if leftWidth < 0 {
			leftWidth = 0
		}
	}

	avail := width - leftWidth
	middleWidth := middleIdeal
	if middleWidth > avail {
		middleWidth = avail
	}

	remaining := avail - middleWidth
	cols := headerHintColumnCount(width, remaining)
	hintWidth := cols * headerHintColWidth
	middleWidth = avail - hintWidth // hand any width the hint grid didn't claim back to MIDDLE

	left := renderHeaderLeft(m, leftWidth)
	middle := content.render(middleWidth)
	if cols == 0 {
		return lipgloss.JoinHorizontal(lipgloss.Top, left, middle)
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, left, middle, renderHeaderHintGrid(cols, hintWidth))
}

// headerRegionLines pads/truncates lines to exactly headerLines entries
// (padLines — see the two-pane panel machinery) and fits+pads each one to
// width, so every region composes into a clean grid via JoinHorizontal
// regardless of its own content length.
func headerRegionLines(lines []string, width int) string {
	lines = padLines(lines, headerLines)
	out := make([]string, headerLines)
	for i, l := range lines {
		out[i] = padToWidth(fitWithin(l, width), width)
	}
	return strings.Join(out, "\n")
}

// headerLeftWidth is the LEFT region's width — fixed rather than
// content-fit, so it does NOT truncate before MIDDLE does (the task's
// explicit priority: "the stats column truncates before the logo column").
// 36 comfortably fits "● LIVE 00:00 · <hostname>" for a typical hostname
// (e.g. "workstation.local", 32 cols total) without truncating — a
// hostname long enough to still overflow this is the one edge this doesn't
// fully guarantee, same spirit as the two-pane layout's own acknowledged
// extreme-width edges.
const headerLeftWidth = 36

// renderHeaderLeft: logo + subtitle, the LIVE/uptime/hostname line, and a
// free third line. Judgment call: the task's spec offered "line 3 left free
// or gate badge" as alternatives without picking one — left free, since
// MIDDLE's own line 3 (renderHeaderMiddle) already carries the gate badge
// and a second copy here would just be redundant.
func renderHeaderLeft(m Model, width int) string {
	line1 := stTitle.Render("◎ FLEETOPS") + stFaint.Render("  fleet cockpit")
	line2 := stLive.Render("●") + stDim.Render(" LIVE ") +
		stDim.Bold(true).Render(formatUptime(time.Since(m.start))) +
		stDim.Render(" · "+m.hostname)
	return headerRegionLines([]string{line1, line2, ""}, width)
}

// headerMiddleContent bundles MIDDLE's three lines' content — fleet
// counts, budget+oracle, and BOTH forms of the gate/stalled badge (full
// and abbreviated) — computed ONCE per render from Model, so sizing
// (minWidth/idealWidth, which renderHeaderBlock uses to allocate LEFT vs
// MIDDLE vs the hint grid) and actual rendering (render) can never
// disagree about what MIDDLE needs.
type headerMiddleContent struct {
	fleet       string
	stats       string
	badgeFull   string
	badgeAbbrev string
}

// headerMiddleContent gathers renderHeaderMiddle's old per-render inputs —
// the old single-line renderSummaryBand's content, split across the header
// block's 3 rows instead of joined with " · " onto one line with a
// right-aligned badge.
func (m Model) headerMiddleContent() headerMiddleContent {
	total, running, stalled, idle, gated, totalTokens, judged, good := m.counts()
	// Judgment call: the old band's applied-filter indicator ("filter:
	// %q") has no dedicated line in the task's 3-line MIDDLE spec — folded
	// into line 2 alongside budget/oracle (the same "auxiliary stats"
	// line), rather than dropped. Same "don't show it while still typing
	// it" rule as before (the prompt line already shows the live query).
	filterQuery := ""
	if m.mode != modeFiltering {
		filterQuery = m.filterQuery
	}
	return headerMiddleContent{
		fleet:       headerFleetCountsLine(total, running, stalled, idle, gated),
		stats:       headerBudgetOracleLine(totalTokens, judged, good, filterQuery),
		badgeFull:   headerGateBadgeLine(gated, stalled),
		badgeAbbrev: headerGateBadgeLineAbbrev(gated, stalled),
	}
}

// minWidth is MIDDLE's hard floor: the fleet-stats lines' own full width
// (never abbreviated — UX judge item 2) plus the badge's ABBREVIATED form
// (the narrowest it's ever allowed to render — item 3, "never truncate", so
// this floor guarantees room for at least that much). renderHeaderBlock
// shrinks LEFT first whenever even this floor doesn't otherwise fit.
func (c headerMiddleContent) minWidth() int {
	return maxInt(maxInt(lipgloss.Width(c.fleet), lipgloss.Width(c.stats)), lipgloss.Width(c.badgeAbbrev))
}

// idealWidth is what MIDDLE would need to show its FULL (non-abbreviated)
// badge form — renderHeaderBlock tries to give it this much before handing
// anything to the hint grid, but never less than minWidth.
func (c headerMiddleContent) idealWidth() int {
	return maxInt(maxInt(lipgloss.Width(c.fleet), lipgloss.Width(c.stats)), lipgloss.Width(c.badgeFull))
}

// render picks the fullest badge form (full, else abbreviated) that fits
// width — the badge must NEVER be ansi-truncated by headerRegionLines'
// fitWithin, which would otherwise clip it mid-glyph.
func (c headerMiddleContent) render(width int) string {
	badge := c.badgeFull
	if badge != "" && lipgloss.Width(badge) > width {
		badge = c.badgeAbbrev
	}
	return headerRegionLines([]string{c.fleet, c.stats, badge}, width)
}

// headerFleetCountsLine: "fleet N · x run · y gate · z stalled · w idle"
// (zero-count segments omitted, fleet always shown) — unchanged from the
// old renderSummaryBand's left side, minus budget/oracle/filter (now
// headerBudgetOracleLine's line).
func headerFleetCountsLine(total, running, stalled, idle, gated int) string {
	parts := []string{stDim.Render(fmt.Sprintf("fleet %d", total))}
	if running > 0 {
		parts = append(parts, lipgloss.NewStyle().Foreground(cBlue).Render(fmt.Sprintf("%d run", running)))
	}
	if gated > 0 {
		parts = append(parts, lipgloss.NewStyle().Foreground(cAmber).Render(fmt.Sprintf("%d gate", gated)))
	}
	if stalled > 0 {
		parts = append(parts, lipgloss.NewStyle().Foreground(cAmber).Render(fmt.Sprintf("%d stalled", stalled)))
	}
	if idle > 0 {
		parts = append(parts, stDim.Render(fmt.Sprintf("%d idle", idle)))
	}
	return strings.Join(parts, stFaint.Render(" · "))
}

// headerBudgetOracleLine: "budget <spent> · oracle P% · filter \"q\""
// (each segment omitted when there's nothing to show — no spend, no judged
// loops, no applied filter). budget is total spend across the fleet, not
// spent/cap (see the old renderSummaryBand's doc for why a fleet-wide cap
// would be meaningless); oracle% is the share of judged loops whose latest
// outcome is done or progress.
func headerBudgetOracleLine(totalTokens, judged, good int, filterQuery string) string {
	var parts []string
	if totalTokens > 0 {
		parts = append(parts, stDim.Render("budget "+prettyTokens(totalTokens)))
	}
	if judged > 0 {
		pct := int(math.Round(float64(good) / float64(judged) * 100))
		parts = append(parts, stDim.Render(fmt.Sprintf("oracle %d%%", pct)))
	}
	if filterQuery != "" {
		parts = append(parts, stFaint.Render(fmt.Sprintf("filter: %q", filterQuery)))
	}
	return strings.Join(parts, stFaint.Render(" · "))
}

// headerGateBadgeLine: gates take priority ("▲ N GATE NEEDS YOU") since a
// gate is a human actively being asked something right now; otherwise
// stalls get the badge ("▲ N STALLED NEED YOU") — unchanged priority from
// the old renderSummaryBand's right-aligned badge.
func headerGateBadgeLine(gated, stalled int) string {
	switch {
	case gated > 0:
		return stBadgeStalled.Render(fmt.Sprintf("▲ %d GATE NEEDS YOU", gated))
	case stalled > 0:
		return stBadgeStalled.Render(fmt.Sprintf("▲ %d STALLED NEED YOU", stalled))
	default:
		return ""
	}
}

// headerGateBadgeLineAbbrev is headerGateBadgeLine's fallback form
// (fix/exit-gate-ux, UX judge item 3): "▲N GATE"/"▲N STALLED" — used ONLY
// when the full form doesn't fit width (see headerMiddleContent.render),
// so the badge — the sole attention cue in narrow/list-only mode — is
// NEVER ansi-truncated mid-glyph. Same gate-over-stalled priority as the
// full form.
func headerGateBadgeLineAbbrev(gated, stalled int) string {
	switch {
	case gated > 0:
		return stBadgeStalled.Render(fmt.Sprintf("▲%d GATE", gated))
	case stalled > 0:
		return stBadgeStalled.Render(fmt.Sprintf("▲%d STALLED", stalled))
	default:
		return ""
	}
}

// headerHintKeys is the RIGHT region's content — every keybinding the old
// bottom keybar used to list (minus "↑↓ select": the FLEET panel's own ▸
// cursor marker already makes selection self-evident, so it didn't earn a
// grid cell) — filled column-major into headerLines (3) rows, so this
// list's order IS the grid's visual column grouping (see
// renderHeaderHintGrid).
//
// fix/exit-gate-ux (UX judge item 7): reordered from the original
// alphabetical-ish column-major fill (which split "r resume"/"i inject" —
// both "send something into the loop" — into different columns) into
// FUNCTIONAL groups, so the grid reads by what each key DOES, not by
// coincidence of position: col0 = send-into-the-loop (r/i/a — resume,
// inject, approve all push some decision/prompt at the loop), col1 =
// lifecycle (n/k/p — start, kill, stop), col2 = nav/view (↵/o// — attach,
// view log, filter), col3 = session housekeeping (q quit, d hidden, x delete —
// appended after the pinned groups so none of them reshuffle).
var headerHintKeys = []struct{ key, action string }{
	{"r", "resume"}, {"i", "inject"}, {"a", "approve"},
	{"n", "new"}, {"k", "kill"}, {"p", "stop"},
	{"↵", "attach"}, {"o", "log"}, {"/", "filter"},
	{"q", "quit"}, {"d", "hidden"}, {"x", "delete"},
}

// headerHintColWidth is one hint grid column's width (the task's "~14
// cols"); headerHintMinWidth is the total terminal width below which the
// WHOLE grid is dropped rather than showing a single cramped column
// (keybindings are discoverable in the README at that point, per the task).
const (
	headerHintColWidth = 14
	headerHintMinWidth = 70
)

// headerHintColumnCount decides how many hint-grid columns fit: 0 below
// headerHintMinWidth (drop the whole grid — keybindings are discoverable
// in the README at that point), otherwise as many headerHintColWidth-wide
// columns fit in availForHints — the space renderHeaderBlock has ALREADY
// determined is left over after giving LEFT and MIDDLE (the fleet-stats
// band + attention badge — UX judge items 2+3, higher width priority than
// hints) everything they need. Capped at the number of columns
// headerHintKeys actually needs (no empty trailing columns). Columns drop
// right-to-left as availForHints shrinks: the LAST column (fewest,
// least-essential-by-list-order keys) runs out of room first.
func headerHintColumnCount(totalWidth, availForHints int) int {
	if totalWidth < headerHintMinWidth {
		return 0
	}
	maxCols := (len(headerHintKeys) + headerLines - 1) / headerLines // ceil
	cols := availForHints / headerHintColWidth
	if cols > maxCols {
		cols = maxCols
	}
	if cols < 0 {
		cols = 0
	}
	return cols
}

// renderHeaderHintGrid lays out cols columns × headerLines rows of
// "<key> action" cells, column-major (column c holds
// headerHintKeys[c*headerLines : c*headerLines+headerLines]) — matches the
// task's fill order exactly. A cell past the end of headerHintKeys (the
// grid's last column is usually only partially full) renders blank.
func renderHeaderHintGrid(cols, width int) string {
	colWidth := headerHintColWidth
	if cols > 0 {
		colWidth = width / cols
	}
	lines := make([]string, headerLines)
	for row := 0; row < headerLines; row++ {
		cells := make([]string, cols)
		for c := 0; c < cols; c++ {
			idx := c*headerLines + row
			cell := ""
			if idx < len(headerHintKeys) {
				k := headerHintKeys[idx]
				cell = stKey.Render("<"+k.key+">") + stDim.Render(" "+k.action)
			}
			cells[c] = padToWidth(cell, colWidth)
		}
		lines[row] = strings.Join(cells, "")
	}
	return headerRegionLines(lines, width)
}

// ── two-pane layout (layout B) ───────────────────────────────
//
// feat/two-pane-cockpit replaced the old flat single-table row (NAME+DOING+
// STATE+CYCLE+ORACLE+BUDGET+N-I+LAST+NOTE, sized by the since-removed
// columnWidths/flexNameDoing/renderTableHeader/renderRow — see git history
// if you need the F1-era column-width cascade) with a scannable
// list-with-detail layout: a compact FLEET list panel (marker+NAME+STATE[+LAST],
// see listRowWidths/renderListRow) and a DETAIL panel carrying everything
// that used to be a "wide" column (renderDetail, unchanged in content, now
// rendered inside its own bordered box instead of below a flat table).

const (
	wMarker = 2
	wState  = 12
	wLast   = 14
	// wCycle/wOracle (feat/panel-info): the FLEET row's two new optional
	// columns. wCycle fits "12/999"-style labels (cycleLabel's widest
	// realistic output) comfortably; wOracle fits "✓×99"/"✗×99" (the
	// ORACLE×N glyph+count, oracleCompactLabel) with a little breathing
	// room. See listRowWidths for the drop-order these widths feed into.
	wCycle  = 7
	wOracle = 6
)

// nameFloorWidth/nameCapWidth bound the FLEET panel's NAME column: below the
// floor a name is a noise-y fragment, so listRowWidths never shrinks it
// further (see listNameFloor for the panel's ABSOLUTE floor, used only when
// even NAME's ideal floor can't be honored); above the cap NAME stops
// growing even in a very wide left panel — extra room is just left blank
// (there's no NOTE-style column left in the list to hand the spare to).
// The cap was raised from 28 alongside the DisplayLabel change: NAME now
// usually carries a display name or a goal's first line ("what is this loop
// doing"), which earns more room than the old project-dir label did.
const (
	nameFloorWidth = 10
	nameCapWidth   = 40
)

// nameGoodWidth is the label width worth protecting: below it a goal-text
// label degrades into a useless fragment, so listRowWidths sheds the
// optional ORACLE/CYCLE columns (in their usual drop order) before letting
// NAME shrink past it. Distinct from listNameFloor (the physical last
// resort) — this is a READABILITY floor, not a fit floor.
const nameGoodWidth = 20

// listNameFloor is listRowWidths' absolute last resort — smaller than
// nameFloorWidth — so the FLEET panel keeps showing SOMETHING for NAME even
// in the narrowest panel this layout ever hands it (list-only mode's floor,
// see panelHeightFloor/layoutModeFor's thresholds).
const listNameFloor = 6

// wideMinWidth/stackedMinWidth are the layout-mode thresholds (the task's
// "essentials": <80 stacked, <50 list-only). See layoutModeFor.
const (
	wideMinWidth    = 80
	stackedMinWidth = 50
)

// topChromeLines/bottomChromeLines/panelHeightFloor: the fixed vertical
// budget View() reserves outside the panel area — see View's comment. Kept
// as named constants (not recomputed from rendered output) so the budget is
// provably constant instead of an estimate that content could quietly grow
// past.
const (
	topChromeLines = headerLines + 1 // the 3-line header block + rule (no blank line — panels gain it, see feat/top-hint-grid)
	// bottomChromeLines: just the bottom line (prompt/status) — the keybar
	// (and the blank lines that used to surround both chrome regions) is
	// gone entirely as of feat/top-hint-grid; every line it used to cost is
	// handed to the panel area instead.
	bottomChromeLines = 1
	panelHeightFloor  = 5 // border(2) + title + rule(2) + >=1 content row

	// stackedPanelHeightFloor is layoutStacked's floor: it renders TWO
	// bordered panels sharing panelHeight, each needing at least
	// panelHeightFloor of its own (see renderStacked) — so the pair needs
	// twice that to honor both without either panel silently pushing the
	// whole frame past the height budget.
	stackedPanelHeightFloor = 2 * panelHeightFloor
)

type layoutMode int

const (
	layoutWide layoutMode = iota
	layoutStacked
	layoutListOnly
)

// layoutModeFor picks the width-driven layout: wide (side-by-side FLEET +
// DETAIL), stacked (FLEET above DETAIL, both full width), or list-only (no
// DETAIL panel at all) — the task's fallback thresholds.
func layoutModeFor(width int) layoutMode {
	switch {
	case width >= wideMinWidth:
		return layoutWide
	case width >= stackedMinWidth:
		return layoutStacked
	default:
		return layoutListOnly
	}
}

// renderWide lays FLEET and DETAIL out side by side. The left (FLEET) panel
// gets a fixed-ish share of the width — just enough for its compact columns
// (see listRowWidths) — everything else goes to DETAIL, which benefits most
// from extra room (GOAL/TAIL wrap wider). Both panels are pre-sized to the
// SAME outer height so lipgloss.JoinHorizontal's own top-alignment padding
// is never needed (see renderPanel's doc — a panel shorter than its sibling
// would otherwise show its bottom border floating above blank filler).
func (m Model) renderWide(width, panelHeight int) string {
	leftWidth := width / 2
	if leftWidth < wideLeftFloor {
		leftWidth = wideLeftFloor
	}
	if leftWidth > wideLeftCap {
		leftWidth = wideLeftCap
	}
	rightWidth := width - leftWidth

	rows := panelContentRows(panelHeight)
	left := renderPanel(fleetTitle(m), padLines(m.fleetPanelLines(panelInnerWidth(leftWidth), rows), rows), leftWidth)
	right := renderPanel(detailTitle(m), padLines(m.detailPanelLines(panelInnerWidth(rightWidth), rows), rows), rightWidth)
	return lipgloss.JoinHorizontal(lipgloss.Top, left, right)
}

// wideLeftFloor/wideLeftCap bound the FLEET panel's share of the width in
// renderWide: the floor is enough room for marker+NAME(floor)+STATE+LAST
// plus the border (2+10+12+14+2=40), the cap keeps the list from wasting a
// huge share of a very wide terminal that DETAIL would put to better use.
// The share is an even half (was 2/5) and the cap 72 (was 60) since the
// DisplayLabel change: the list's NAME column now answers "what is each
// loop doing" at a glance, which is the panel's whole point — the cap is
// sized so a full nameCapWidth label plus every optional column fits
// (2+40+12+7+6+14 + border 2 = 83 is more than DETAIL can spare; 72 keeps
// ~29 label cols with all columns shown, a workable compromise).
const (
	wideLeftFloor = 40
	wideLeftCap   = 72
)

// renderStacked puts FLEET above DETAIL, both spanning the full width,
// splitting the available panel height between them (FLEET gets a smaller
// share — it's a scoped-down list, DETAIL needs more room for its many
// rows).
func (m Model) renderStacked(width, panelHeight int) string {
	// listHeight+detailHeight must sum to EXACTLY panelHeight — two
	// independent floors (each clamped up without the other giving room
	// back) would let the pair exceed the height budget View() computed.
	// View() guarantees panelHeight >= stackedPanelHeightFloor (2×
	// panelHeightFloor) precisely so both floors below can be honored
	// without needing to violate that invariant.
	//
	// FLEET gets the LARGER share (3/5, not the more even split an earlier
	// version of this used) — its whole purpose is the at-a-glance
	// multi-loop overview, so starving it down to 1-2 visible rows at a
	// common height (24) defeated that; DETAIL degrades more gracefully
	// under a tight budget since detailPanelLines clips it top-down and
	// renderDetail already orders its rows by priority (STATE/NOTE/CYCLE/
	// GOAL first, CWD/LOG/TAIL last), so losing its tail rows first is the
	// closest a clip can get to spending the pane's own priority order.
	listHeight := panelHeight * 3 / 5
	if listHeight < panelHeightFloor {
		listHeight = panelHeightFloor
	}
	detailHeight := panelHeight - listHeight
	if detailHeight < panelHeightFloor {
		detailHeight = panelHeightFloor
		listHeight = panelHeight - detailHeight
	}

	listRows := panelContentRows(listHeight)
	detailRows := panelContentRows(detailHeight)
	inner := panelInnerWidth(width)
	top := renderPanel(fleetTitle(m), padLines(m.fleetPanelLines(inner, listRows), listRows), width)
	bottom := renderPanel(detailTitle(m), padLines(m.detailPanelLines(inner, detailRows), detailRows), width)
	return lipgloss.JoinVertical(lipgloss.Left, top, bottom)
}

// renderListOnly shows just the FLEET panel, spanning the full width and the
// full panel height — no DETAIL pane at all (width < stackedMinWidth).
func (m Model) renderListOnly(width, panelHeight int) string {
	rows := panelContentRows(panelHeight)
	return renderPanel(fleetTitle(m), padLines(m.fleetPanelLines(panelInnerWidth(width), rows), rows), width)
}

// fleetTitle/detailTitle are the panels' bordered titles — FLEET carries the
// visible-loop count (post-filter, matching the mockup's "LOOPS" label this
// replaces); DETAIL names the selected loop so it's obvious which loop the
// panel describes, or a plain "DETAIL" placeholder when nothing is
// selected.
func fleetTitle(m Model) string {
	return fmt.Sprintf("FLEET (%d)", len(m.visibleLoops()))
}

func detailTitle(m Model) string {
	if sel, ok := m.selected(); ok {
		return "DETAIL ▸ " + sel.DisplayLabel()
	}
	return "DETAIL"
}

// panelInnerWidth/panelContentRows: how much CONTENT width/height a bordered
// panel of the given OUTER width/height has room for, after its rounded
// border (1 col/row each side) and its baked-in title+rule (2 lines — see
// renderPanel).
func panelInnerWidth(outerWidth int) int {
	w := outerWidth - 2
	if w < 1 {
		w = 1
	}
	return w
}

func panelContentRows(outerHeight int) int {
	h := outerHeight - 4 // border top/bottom(2) + title(1) + rule(1)
	if h < 1 {
		h = 1
	}
	return h
}

// renderPanel wraps already-height-fitted content lines (exactly
// panelContentRows(outerHeight) of them — callers pad/clip via padLines) in
// a rounded border with a bold title baked in as the box's first line and a
// thin rule underneath. lipgloss's Border()+Width() pads a SHORT line but
// does not clip a TALL block of content, so callers own sizing the line
// count themselves; renderPanel only adds the fixed title+rule+border
// chrome around whatever it's given.
func renderPanel(title string, lines []string, outerWidth int) string {
	inner := panelInnerWidth(outerWidth)
	var body strings.Builder
	body.WriteString(fitWithin(stTitle.Render(title), inner))
	body.WriteString("\n")
	body.WriteString(lipgloss.NewStyle().Foreground(cLine).Render(strings.Repeat("─", inner)))
	for _, l := range lines {
		body.WriteString("\n")
		body.WriteString(fitWithin(l, inner))
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(cLine).
		Width(inner).
		Render(body.String())
}

// visibleWindow computes [start,end) into a total-item range so index idx
// stays visible within a window of at most maxRows items, scrolling the
// minimum needed and centering idx when there's room — the FLEET panel's
// scroll behavior (no new keybindings: existing ↑/↓/g/G cursor movement
// drives idx, this just keeps it on screen). total<=maxRows returns the
// whole range unscrolled.
func visibleWindow(total, idx, maxRows int) (start, end int) {
	if maxRows <= 0 || total <= maxRows {
		return 0, total
	}
	start = idx - maxRows/2
	if start < 0 {
		start = 0
	}
	end = start + maxRows
	if end > total {
		end = total
		start = end - maxRows
	}
	return start, end
}

// padLines pads (with blank lines) or truncates lines to exactly n entries,
// so a panel's content always occupies exactly its allotted height — the
// bordered box's bottom border lands in the same place regardless of how
// few rows/detail lines are actually present this frame.
func padLines(lines []string, n int) []string {
	if len(lines) >= n {
		return lines[:n]
	}
	out := make([]string, n)
	copy(out, lines)
	return out
}

// ── feat/panel-info: FLEET panel's ORACLE×N column cache ─────────────────
//
// fleetOracleCountsMsg reports fleetOracleCountsCmd's result: sessionID ->
// total judged-verdict count, for EVERY loop in the fleet (not just the
// selected one — see Model.fleetOracleCounts' doc for why this differs
// from gitStats/detailCache's scope).
type fleetOracleCountsMsg map[string]int

// fleetOracleCountsCmd counts each loop's TriggerOracle events (its total
// judged-verdict history) off the event loop, once per scan tick, for the
// WHOLE fleet at once (one tea.Cmd, one event-log read per BOUND loop —
// not one per FLEET row per render, which is exactly the per-render-event-
// read class of bug fix/exit-gate-ux's architecture judge already
// eliminated elsewhere; this column must not reintroduce it under a new
// name). Unbound loops (no goal — never judged, never will be) are
// skipped entirely: no event-log read is even attempted for them.
func fleetOracleCountsCmd(loops []domain.Loop) tea.Cmd {
	return func() tea.Msg {
		dir := historyDirFn()
		counts := make(map[string]int, len(loops))
		for _, l := range loops {
			if l.Goal.Text == "" {
				continue
			}
			evs, _ := events.Read(dir, l.SessionID) // best-effort; nil on error just means "0 judged so far"
			n := 0
			for _, ev := range evs {
				if ev.Trigger == events.TriggerOracle {
					n++
				}
			}
			counts[l.SessionID] = n
		}
		return fleetOracleCountsMsg(counts)
	}
}

// listRowWidths computes the compact FLEET row's NAME width for a panel of
// the given inner content width, dropping the LAST column first if there
// isn't room for it — the same self-verifying "never return a layout that
// doesn't fit" cascade the old columnWidths used, scoped down to this
// layout's much smaller column set (marker+NAME+STATE[+CYCLE][+ORACLE]
// [+LAST] — BUDGET/N-I/NOTE stay DETAIL-only, see renderDetail).
// Unlike an earlier version of this function, wName is never clamped UP to
// listNameFloor when there isn't room — an unconditional floor is not a
// guarantee (the exact class of bug F1 fixed in the old columnWidths): at a
// small enough innerWidth, honoring the floor would make
// wMarker+wName+wState overflow innerWidth. The one edge this still doesn't
// claim to cover is innerWidth < wMarker+wState (marker+STATE alone already
// exceed it) — same spirit as F1's own acknowledged "not fully guaranteed
// under ~40 cols" edge in the old system.
//
// feat/panel-info: CYCLE and ORACLE (the ORACLE×N column) drop before LAST
// as width shrinks — the exact "width degradation ORACLE→CYCLE→LAST"
// order: ORACLE is the least essential of the three optional columns
// (dropped first), LAST (last-activity recency) is the most established/
// essential (dropped last, same priority LAST already had before this
// change).
//
// feat/loop-display-name: ORACLE and CYCLE now drop not just when the row
// physically can't fit (listNameFloor) but whenever they'd squeeze NAME
// below nameGoodWidth — the label carries the loop's display name/goal
// ("what is this loop doing"), the panel's primary answer, which a 7-char
// fragment can't give. LAST keeps its stricter physical-fit-only rule: at
// the narrowest panels renderWide ever produces this lands on the same
// widths as before, so only mid-width panels trade ORACLE/CYCLE for a
// readable label.
func listRowWidths(innerWidth int) (wName int, showCycle, showOracle, showLast bool) {
	showCycle, showOracle, showLast = true, true, true
	name := func() int {
		fixed := wMarker + wState
		if showLast {
			fixed += wLast
		}
		if showCycle {
			fixed += wCycle
		}
		if showOracle {
			fixed += wOracle
		}
		return innerWidth - fixed
	}
	// drop right-to-priority: ORACLE first, then CYCLE, then LAST — see
	// this function's own doc for why this specific order.
	if name() < nameGoodWidth {
		showOracle = false
	}
	if name() < nameGoodWidth {
		showCycle = false
	}
	if name() < listNameFloor {
		showLast = false
	}
	wName = name()
	if wName < 0 {
		wName = 0
	}
	if wName > nameCapWidth {
		wName = nameCapWidth
	}
	return wName, showCycle, showOracle, showLast
}

// renderListRow renders one FLEET panel row: marker+NAME+STATE[+CYCLE]
// [+ORACLE][+LAST] — no DOING/BUDGET/N-I/NOTE (see renderDetail for
// those). Selection highlight and duplicate-label disambiguation match the
// old renderRow. oracleCount is the loop's cached judged-verdict count
// (Model.fleetOracleCounts, looked up by the caller — this function does
// no I/O of its own, same discipline as everything else in this file's
// render path).
func renderListRow(l domain.Loop, sel, dup bool, wName int, showCycle, showOracle, showLast bool, oracleCount int, totalWidth int) string {
	// feat/engine-provenance: wMarker is 2 cols wide, but the cursor glyph
	// below only ever occupies 1 of them — the second was always blank
	// padding. Rather than widen the row for a Driven marker, that
	// already-reserved second column is where it goes: two independently
	// styled 1-rune cells concatenated (both ⚙/▸/◆/etc. are confirmed
	// single-width via go-runewidth, same class of glyph this file already
	// renders elsewhere — see stateLabel), so the combined string is exactly
	// wMarker runes wide by construction, no .Width() padding call needed.
	cursorGlyph, cursorColor := " ", cFaint
	if sel {
		cursorGlyph, cursorColor = "▸", cAccent
	}
	drivenGlyph, drivenColor := " ", cFaint
	if l.Driven {
		drivenGlyph = "⚙" // dim, deliberately — a compact provenance hint, not a status alert
	}
	marker := lipgloss.NewStyle().Foreground(cursorColor).Render(cursorGlyph) +
		lipgloss.NewStyle().Foreground(drivenColor).Render(drivenGlyph)
	label := l.DisplayLabel()
	if dup {
		label += "·" + shortID(l.SessionID)
	}
	cells := []string{
		marker,
		stInk.Width(wName).Render(trunc(label, wName-1)),
		stateStyle(l).Width(wState).Render(stateLabel(l)),
	}
	if showCycle {
		cells = append(cells, stDim.Width(wCycle).Render(cycleLabel(l)))
	}
	if showOracle {
		cells = append(cells, oracleStyle(l).Width(wOracle).Render(oracleCompactLabel(l, oracleCount)))
	}
	if showLast {
		cells = append(cells, stDim.Width(wLast).Render(rel(time.Since(l.LastActivity))))
	}
	row := lipgloss.JoinHorizontal(lipgloss.Top, cells...)
	if sel {
		row = stSelRow.Render(padToWidth(row, totalWidth))
	}
	return row
}

// oracleCompactLabel is the FLEET panel's ORACLE×N column value: the
// latest verdict's glyph (✓ done/progress, ✗ rejected) times the total
// judged count so far — "✓×3", "✗×2" — or "—" for a loop that's unbound
// or bound but never yet judged. count comes from the caller's
// Model.fleetOracleCounts lookup (a per-scan cache — see its doc); this
// function itself does no I/O, mirroring oracleLabel's own shape (the
// DETAIL panel's fuller "✓ verified"/"✗ rejected" sibling).
func oracleCompactLabel(l domain.Loop, count int) string {
	if l.Last == nil {
		return "—"
	}
	glyph := "✓"
	if l.Last.Outcome == domain.OutcomeRejected {
		glyph = "✗"
	}
	return fmt.Sprintf("%s×%d", glyph, count)
}

// fleetPanelLines builds the FLEET panel's content lines, scrolled (see
// visibleWindow) so the cursor row stays visible within innerHeight rows.
// Callers pad/clip the result to exactly innerHeight via padLines.
func (m Model) fleetPanelLines(innerWidth, innerHeight int) []string {
	visible := m.visibleLoops()
	switch {
	case len(m.loops) == 0:
		// fix/exit-gate-ux (UX judge item 5): the empty FLEET panel used to
		// just state the fact and dead-end a new user there — no indication
		// of what to do about it. One extra line pointing at the two most
		// likely next actions (spawn a loop, or install the hooks that make
		// gate detection work at all) turns a dead end into a next step.
		return []string{
			stFaint.Render("no active Claude Code loops in the window."),
			stFaint.Render("press n to spawn a loop · run 'fleetops hooks install' for gate detection"),
		}
	case len(visible) == 0:
		return []string{stFaint.Render(fmt.Sprintf("no loops match filter %q.", m.filterQuery))}
	}
	wName, showCycle, showOracle, showLast := listRowWidths(innerWidth)
	dupLabels := duplicateLabels(visible)
	start, end := visibleWindow(len(visible), m.cursor, innerHeight)
	rows := make([]string, 0, end-start)
	for i := start; i < end; i++ {
		l := visible[i]
		rows = append(rows, renderListRow(l, i == m.cursor, dupLabels[l.DisplayLabel()], wName, showCycle, showOracle, showLast, m.fleetOracleCounts[l.SessionID], innerWidth))
	}
	return rows
}

// detailPanelLines builds the DETAIL panel's content: the selected loop's
// full renderDetail output, clipped to innerHeight lines (simple top-down
// clipping, not a scrollable viewport — adding detail-pane scroll keys
// would be new behavior the task's "behavior unchanged" doesn't ask for; see
// the PR description's judgment-call note). A placeholder stands in when
// nothing is selected.
//
// feat/detail-panel-v2: also gathers detailData here — the session's event
// history (a couple of small local file reads via events.Read, cheap enough
// to do synchronously in View()) and the cached git stats (gitStatsCmd,
// computed asynchronously per scan tick — see Model.gitStats' doc) — so
// renderDetail itself stays a pure function over already-known data.
func (m Model) detailPanelLines(innerWidth, innerHeight int) []string {
	sel, ok := m.selected()
	if !ok {
		return []string{stFaint.Render("select a loop to see its detail.")}
	}
	cached := m.detailCache[sel.SessionID] // zero value (empty events, lastError.ok=false) when not yet cached — see detailCache's doc
	data := detailData{
		now:       time.Now(),
		events:    cached.events,
		git:       m.gitStats[sel.SessionID],
		lastError: cached.lastError,
	}
	lines := strings.Split(renderDetail(sel, innerWidth, innerHeight, data), "\n")
	if len(lines) > innerHeight {
		lines = lines[:innerHeight]
	}
	return lines
}

// duplicateLabels reports, for each display label shared by 2+ loops in the
// current fleet, whether renderListRow must disambiguate it with a
// session-id suffix — the ONLY place a session-id fragment still reaches
// the FLEET list (many loops sharing a common label like "backend" are
// otherwise indistinguishable). Keyed by DisplayLabel, not Project: two
// loops in the same repo pursuing DIFFERENT goals already read apart.
func duplicateLabels(loops []domain.Loop) map[string]bool {
	counts := make(map[string]int, len(loops))
	for _, l := range loops {
		counts[l.DisplayLabel()]++
	}
	dup := make(map[string]bool, len(counts))
	for label, n := range counts {
		dup[label] = n > 1
	}
	return dup
}

// noteForRow decides the DETAIL panel's NOTE row text and color (moved here
// from the old flat table's NOTE column — see renderDetail). A governor-set
// l.Note (internal/engine.Check via the scanner's applyGovernor) always
// wins when set — it's either an "over budget"/"max cycles reached"
// escalation (amber, State otherwise unchanged) or a "stopped: no
// improvement" note paired with StateFailed (red, matching FAILED's own
// state color) — this row is what keeps a StateFailed loop's governor note
// (the one case with no callout of its own) visible at all now that it's
// off the list.
//
// fix/exit-gate-ux (UX judge item 4): this used to ALSO fall back to a
// stall/drift-derived text ("⚠ <stall>" / "✗ <reason>") whenever l.Note was
// empty — but StateStalled/StateDrift both already have their OWN callout
// box below (renderResumeCallout/renderDriftCallout) stating the exact
// same thing, so that fallback made the same fact print twice on top of
// the ORACLE row ALSO repeating it a third time (see renderOracleDetail's
// own fix). Dropped entirely: StateFailed (via l.Note above) is the only
// state left with no callout of its own, so it's the only one that still
// needs this row for anything.
func noteForRow(l domain.Loop) (string, lipgloss.Style) {
	if l.Note != "" {
		if l.State == domain.StateFailed {
			return l.Note, lipgloss.NewStyle().Foreground(cRed)
		}
		return l.Note, lipgloss.NewStyle().Foreground(cAmber)
	}
	return "", stateStyle(l)
}

// cycleLabel: plain count ("6"), or "6/12" once a per-loop MaxCycles exists.
func cycleLabel(l domain.Loop) string {
	if l.Goal.MaxCycles > 0 {
		return fmt.Sprintf("%d/%d", l.Cycle, l.Goal.MaxCycles)
	}
	return fmt.Sprintf("%d", l.Cycle)
}

// oracleLabel: "—" for an unbound loop or a bound one never yet judged;
// otherwise the latest verdict, mockup-style ("✓ verified" done, "✓
// progress", "✗ rejected").
func oracleLabel(l domain.Loop) string {
	if l.Last == nil {
		return "—"
	}
	switch l.Last.Outcome {
	case domain.OutcomeDone:
		return "✓ verified"
	case domain.OutcomeProgress:
		return "✓ progress"
	case domain.OutcomeRejected:
		return "✗ rejected"
	default:
		return "—"
	}
}

// noImproveLabel: "—" for an unbound loop; "<n>/<limit>" for a bound one.
func noImproveLabel(l domain.Loop) string {
	if l.Goal.Text == "" {
		return "—"
	}
	return fmt.Sprintf("%d/%d", l.NoImprove, l.Goal.NoImproveLimit)
}

// shortID is the first 4 chars of a session id, for disambiguating rows
// that share a project label (e.g. "sessions·1110").
func shortID(id string) string {
	if len(id) <= 4 {
		return id
	}
	return id[:4]
}

// ── git working-tree stats (feat/detail-panel-v2's STAGE row) ───────────
//
// Computing `git diff`/`git status` is real exec work (unlike reading the
// event log, a couple of small local file reads) — doing it synchronously
// inside View() would risk blocking the whole TUI on a wedged git process.
// So it follows the SAME tea.Cmd/Msg pattern as judgeCmd/verdictMsg: fired
// once per scan (loopsMsg), only for the currently-selected loop (the only
// one ever rendered), result cached in Model.gitStats keyed by SessionID.

// gitStatsResult is one loop's working-tree diff snapshot. ok=false means
// "not a git repo, CwdVerified is false, or the git commands failed/timed
// out" — STAGE simply omits the file/± portion in that case, never an
// error shown to the human (this is a nice-to-have annotation, not fleet
// state).
type gitStatsResult struct {
	files, plus, minus int
	ok                 bool
}

// gitStatsMsg reports gitStatsCmd's result for sessionID.
type gitStatsMsg struct {
	sessionID string
	stats     gitStatsResult
}

// gitStatsTimeout bounds each of the two git subprocess calls — a wedged
// git (e.g. an NFS-mounted repo) must not hang the fleet loop.
const gitStatsTimeout = 2 * time.Second

// gitStatsCmd computes l's working-tree diff stats off the event loop.
// CwdVerified is required (same "don't trust a lossy decoded path" guard
// used elsewhere — see domain.Loop.CwdVerified's doc) — an unverified Cwd
// isn't safe to run git commands against at all.
func gitStatsCmd(l domain.Loop) tea.Cmd {
	return func() tea.Msg {
		if !l.CwdVerified {
			return gitStatsMsg{l.SessionID, gitStatsResult{}}
		}
		files, plus, minus, ok := computeGitStats(l.Cwd)
		return gitStatsMsg{l.SessionID, gitStatsResult{files: files, plus: plus, minus: minus, ok: ok}}
	}
}

// computeGitStats runs `git diff --shortstat` (tracked-file changes: file
// count + insertions/deletions) and `git status --porcelain` (adds
// untracked files to the file count — diff alone can't see them) in cwd.
// ok=false if the FIRST command fails (not a git repo, or git itself
// missing) — the second command's failure is tolerated (still returns the
// diff-only numbers) since a status failure doesn't invalidate the diff.
func computeGitStats(cwd string) (files, plus, minus int, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), gitStatsTimeout)
	defer cancel()
	diffOut, err := exec.CommandContext(ctx, "git", "-C", cwd, "diff", "--shortstat").Output()
	if err != nil {
		return 0, 0, 0, false
	}
	files, plus, minus = parseShortstat(string(diffOut))

	ctx2, cancel2 := context.WithTimeout(context.Background(), gitStatsTimeout)
	defer cancel2()
	statusOut, err := exec.CommandContext(ctx2, "git", "-C", cwd, "status", "--porcelain").Output()
	if err == nil {
		files += countUntrackedFiles(string(statusOut))
	}
	return files, plus, minus, true
}

// shortstatRe parses `git diff --shortstat`'s summary line, e.g.
// " 2 files changed, 47 insertions(+), 9 deletions(-)" — any of the three
// clauses may be absent (e.g. a diff with only deletions omits
// "insertions").
var shortstatRe = regexp.MustCompile(`(\d+) files? changed(?:, (\d+) insertions?\(\+\))?(?:, (\d+) deletions?\(-\))?`)

func parseShortstat(s string) (files, plus, minus int) {
	m := shortstatRe.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, 0
	}
	files, _ = strconv.Atoi(m[1])
	if m[2] != "" {
		plus, _ = strconv.Atoi(m[2])
	}
	if m[3] != "" {
		minus, _ = strconv.Atoi(m[3])
	}
	return files, plus, minus
}

// countUntrackedFiles counts `git status --porcelain` lines for an
// untracked file ("??" prefix) — these never show up in `git diff`, which
// only ever compares tracked content.
func countUntrackedFiles(porcelain string) int {
	n := 0
	for _, line := range strings.Split(porcelain, "\n") {
		if strings.HasPrefix(line, "??") {
			n++
		}
	}
	return n
}

// ── DETAIL panel's async event-log + LAST ERROR cache ───────────────────
//
// fix/exit-gate-ux (architecture judge, P1): reading the event log
// (events.Read) and parsing the selected loop's transcript for its last
// error (claude.LastError) both used to run synchronously inside View() —
// real disk I/O on the Update/View goroutine on EVERY keystroke and EVERY
// scan tick, contradicting this file's own off-loop discipline (gitStatsCmd
// exists precisely to avoid exactly this class of bug for the STAGE row's
// git stats). Follows the SAME tea.Cmd/Msg/Model-cache pattern as
// gitStatsCmd verbatim — see its doc.

// lastErrorResult is claude.LastError's three return values, bundled so
// detailCacheEntry can carry it as one field (mirrors gitStatsResult's own
// shape/reasoning).
type lastErrorResult struct {
	text string
	ts   time.Time
	ok   bool
}

// detailCacheEntry is one loop's cached event history + LAST ERROR
// extraction. The zero value (nil events, lastError.ok=false) means "not
// computed yet" — renderDetail already tolerates both being empty/absent
// (see detailData's doc), so there is no separate loading state to thread
// through.
type detailCacheEntry struct {
	events    []events.Event
	lastError lastErrorResult
}

// detailCacheMsg reports detailCacheCmd's result for sessionID.
type detailCacheMsg struct {
	sessionID string
	entry     detailCacheEntry
}

// detailCacheCmd gathers l's event history and transcript LAST ERROR off
// the event loop. Both are best-effort (events.Read tolerates a
// missing/empty history; claude.LastError's ok=false just means "no error
// entry found") — never an error surfaced to the human, same as gitStatsCmd.
func detailCacheCmd(l domain.Loop) tea.Cmd {
	return func() tea.Msg {
		evs, _ := events.Read(historyDirFn(), l.SessionID)
		text, ts, ok := claude.LastError(l.Path)
		return detailCacheMsg{
			sessionID: l.SessionID,
			entry: detailCacheEntry{
				events:    evs,
				lastError: lastErrorResult{text: text, ts: ts, ok: ok},
			},
		}
	}
}

// ── detail pane ──────────────────────────────────────────────

// tailMaxLines caps how many wrapped lines the detail pane's TAIL row shows.
// feat/detail-panel-v2 (council: TAIL must not grow into a transcript
// viewer — that's what ↵ attach is for) shrank this from 6 to 4 to make
// room for the new LAST ERROR/VERDICTS/EVENTS blocks; the full report lives
// in the pager / oracle / EVENTS block, not here.
const tailMaxLines = 4

// detailKeyWidth is the fixed column width of a detail row's KEY (see
// detailRow). TAIL's wrapped continuation lines indent by exactly this much so
// their text aligns under the value column instead of the label.
const detailKeyWidth = 8

// detailData bundles everything renderDetail needs beyond the Loop itself —
// data that's either expensive (git stats, transcript parsing) or requires
// "now"/the event log for time-relative computations (STAGE elapsed,
// burn-rate ETA, LAST ERROR staleness, VERDICTS, EVENTS, the STALLED
// callout's flap counter). Gathered once by the caller (detailPanelLines,
// from Model's async caches — see detailCacheCmd/gitStatsCmd) so
// renderDetail itself stays a pure rendering function over already-known
// data — directly testable without a real event-log dir, transcript file,
// or git repo.
type detailData struct {
	now       time.Time
	events    []events.Event // this session's history, oldest-first (events.Read's contract)
	git       gitStatsResult
	lastError lastErrorResult // the selected loop's transcript LAST ERROR (claude.LastError), cached async — see detailCacheCmd
}

// eventsMinRows is the EVENTS block's floor: below this many rows available
// (title line + at least 2 data rows), the whole block is omitted rather
// than rendering a cramped, barely-useful sliver.
const eventsMinRows = 3

func renderDetail(l domain.Loop, width, height int, data detailData) string {
	// leave room for the ~8-col key + its gap before truncating long values
	// (paths) so nothing overflows the terminal width.
	valueWidth := width - 10
	if valueWidth < 10 {
		valueWidth = 10
	}

	var d strings.Builder
	// fix/exit-gate-ux (UX judge item 4): this used to lead with
	// "▸ <project>  <sid>" — but the panel's own title (see detailTitle)
	// already reads "DETAIL ▸ <project>", so the project name printed a
	// SECOND time as this content block's very first thing a human's eye
	// hits. Lead with the session id alone — it's the one identifying fact
	// the panel title doesn't already carry.
	d.WriteString(stFaint.Render(l.SessionID))
	d.WriteString("\n")
	d.WriteString(detailRow("STATE", stateStyle(l).Render(stateLabel(l))))
	// NOTE: moved here from the old flat table's NOTE column (see
	// noteForRow) — the ONLY place a StateFailed loop's governor note is
	// visible now that the list no longer shows it (StateStalled/Gate/Drift
	// keep their own callout box below; StateFailed has none, so this row
	// is its sole surface).
	if note, noteSt := noteForRow(l); note != "" {
		d.WriteString(detailRow("NOTE", noteSt.Render(trunc(note, valueWidth))))
	}
	d.WriteString(detailRow("CYCLE", stInk.Render(cycleLabel(l))))
	// feat/engine-provenance: the DETAIL-panel counterpart to the FLEET
	// panel's ⚙ marker (renderListRow) — an observed loop omits this row
	// entirely (l.Driven is only ever true for an engine-owned loop; see
	// domain.Loop.Driven's doc), so a human scanning DETAIL sees ownership
	// as a presence/absence signal, not a third state to parse.
	if l.Driven {
		d.WriteString(detailRow("DRIVE", stInk.Render(trunc("⚙ engine-driven (cycle "+cycleLabel(l)+")", valueWidth))))
	}
	if l.Goal.Text != "" {
		d.WriteString(detailRow("GOAL", stInk.Render(trunc(l.Goal.Text, valueWidth))))
		d.WriteString(detailRow("ORACLE", renderOracleDetail(l)))
		// RUBRIC/CHALL: the wizard's two remaining free-text contract
		// fields (how completion is verified; the adversarial probe
		// description) — distinct from the ORACLE row above, which shows
		// the oracle's rendered VERDICT, not the criteria it judges
		// against. "CHALL" (not "CHALLENGER") is a hard requirement, not
		// styling: detailRow's KEY column is a fixed detailKeyWidth (8)
		// cols via lipgloss .Width() — verified empirically that content
		// LONGER than the fixed width WRAPS to a second line rather than
		// overflowing in place, which would break both this row's own
		// alignment and silently shift every row's height accounting.
		// Both rows are now ALWAYS shown once a loop has a
		// goal (previously RUBRIC was hidden when empty and CHALLENGER
		// wasn't shown at all) — "leave it blank if there's nothing": a
		// predictable, always-present row
		// with a "—" placeholder reads better than a contract section
		// whose row COUNT silently varies loop to loop. The challenger
		// field is still STORED ONLY (no challenger phase exists to
		// surface progress against — see DESIGN.md) — this just makes the
		// fact that none was set visible, rather than omitting the row.
		d.WriteString(detailRow("RUBRIC", stDim.Render(trunc(orDash(l.Goal.Rubric), valueWidth))))
		d.WriteString(detailRow("CHALL", stDim.Render(trunc(orDash(l.Goal.Challenger), valueWidth))))
		if stage, ok := renderStageRow(l, data); ok {
			d.WriteString(detailRow("STAGE", stInk.Render(trunc(stage, valueWidth))))
		}
	}
	d.WriteString(detailRow("BUDGET", budgetStyle(l).Render(trunc(budgetLine(l), valueWidth))))
	if l.Goal.Text != "" {
		d.WriteString(detailRow("N/I", noImproveStyle(l).Render(noImproveLabel(l))))
	}
	d.WriteString(detailRow("LAST", stInk.Render(rel(time.Since(l.LastActivity))+"  ("+l.LastActivity.Format("15:04:05")+")")))
	d.WriteString(detailRow("CWD", stDim.Render(trunc(l.Cwd, valueWidth))))
	d.WriteString(detailRow("LOG", stDim.Render(trunc(l.Path, valueWidth))))

	switch l.State {
	case domain.StateStalled:
		d.WriteString(renderResumeCallout(l, width, data.events, data.now))
	case domain.StateGate:
		d.WriteString(renderGateCallout(l, width))
	case domain.StateDrift:
		d.WriteString(renderDriftCallout(l, width))
	}

	if errText, errTS, ok := lastErrorForDetail(data); ok {
		d.WriteString(renderLastErrorBlock(errText, errTS, valueWidth))
	}

	if l.Goal.Text != "" {
		if lines := renderVerdictsBlock(data.events, valueWidth); lines != "" {
			d.WriteString(lines)
		}
	}

	top := strings.TrimRight(d.String(), "\n")

	var tail string
	if l.LastText != "" {
		wrapped := wrapTailText(l.LastText, valueWidth, tailMaxLines)
		for i := range wrapped {
			wrapped[i] = stDim.Render(wrapped[i])
		}
		tail = strings.TrimRight(detailRowMultiline("TAIL", wrapped), "\n")
	}

	// EVENTS absorbs whatever height is left after everything else
	// (including TAIL, capped at tailMaxLines, and the top block above) —
	// see eventsMinRows' doc for the floor below which it's omitted
	// entirely rather than rendered cramped.
	used := strings.Count(top, "\n") + 1
	if tail != "" {
		used += strings.Count(tail, "\n") + 1
	}
	remaining := height - used
	eventsBlock := ""
	if remaining >= eventsMinRows {
		eventsBlock = renderEventsBlock(data.events, valueWidth, remaining)
	}

	var out strings.Builder
	out.WriteString(top)
	if eventsBlock != "" {
		out.WriteString("\n")
		out.WriteString(eventsBlock)
	}
	if tail != "" {
		out.WriteString("\n")
		out.WriteString(tail)
	}
	// No border/padding here (unlike the pre-two-pane stDetail wrap this
	// used to return): renderDetail's output now lives INSIDE the DETAIL
	// panel's own bordered box (see renderPanel), which already supplies
	// the border — wrapping it again here would nest two borders.
	return out.String()
}

// budgetLine is the BUDGET row's value: the existing "<spent> / <cap>
// (P%)" plus, when computable, an inline burn rate + ETA cycle —
// "3.9M / 2.0M · ~4.1k/cyc · cap ~c483" (rate = TokensSpent/Cycle; ETA
// cycle = the cycle number at which the budget is projected to run out,
// current cycle + remaining budget / rate). The burn/ETA suffix is
// omitted before cycle 2 (not enough data for a meaningful rate), or once
// already over budget (no future ETA to report — judgment call, see PR
// body).
//
// fix/exit-gate-ux (UX judge, P1 — "most common view is broken"): an
// UNBOUND loop (Goal.BudgetTokens<=0 — most real observed sessions, which
// aren't started via the "n" wizard's contract) used to render "<spent> /
// 0 (0%)" — a fabricated cap and percentage against a budget that was
// never set. There is no "/ <cap> (P%)" to show without a real cap, and no
// burn-rate ETA either (it needs one to compute against) — just the raw
// spend.
func budgetLine(l domain.Loop) string {
	if l.Goal.BudgetTokens <= 0 {
		return prettyTokens(l.TokensSpent)
	}
	base := fmt.Sprintf("%s / %s (%d%%)",
		prettyTokens(l.TokensSpent), prettyTokens(l.Goal.BudgetTokens), int(math.Round(l.BudgetFrac()*100)))
	suffix := budgetBurnRateSuffix(l)
	return base + suffix
}

func budgetBurnRateSuffix(l domain.Loop) string {
	if l.Goal.Text == "" || l.Goal.BudgetTokens <= 0 || l.Cycle < 2 {
		return ""
	}
	rate := float64(l.TokensSpent) / float64(l.Cycle)
	if rate <= 0 {
		return ""
	}
	remaining := float64(l.Goal.BudgetTokens - l.TokensSpent)
	if remaining <= 0 {
		return ""
	}
	etaCycle := l.Cycle + int(math.Round(remaining/rate))
	return fmt.Sprintf(" · ~%s/cyc · cap ~c%d", prettyTokens(int(math.Round(rate))), etaCycle)
}

// plural: "" for n==1, "s" otherwise — the STAGE row's "N file(s)" wording.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// orDash: s unchanged if non-empty, else "—" — the RUBRIC/CHALLENGER
// contract rows: a row that's always present
// once a loop has a goal, with a placeholder rather than being hidden when
// its field is empty.
func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// renderStageRow builds the STAGE row's value — "6/12 · elapsed 04:12 ·
// 2 files +47 −9" — omitting the elapsed segment's SOURCE data entirely
// (ok=false, the whole row is skipped by the caller) when neither BoundAt
// nor the event log's first entry is available. The git file/± segment is
// independently optional (silently omitted when not a git repo /
// CwdVerified false — see gitStatsResult).
func renderStageRow(l domain.Loop, data detailData) (string, bool) {
	elapsed, ok := stageElapsed(l, data)
	if !ok {
		return "", false
	}
	line := fmt.Sprintf("%s · elapsed %s", cycleLabel(l), formatUptime(elapsed))
	if g := data.git; g.ok && (g.files > 0 || g.plus > 0 || g.minus > 0) {
		line += fmt.Sprintf(" · %d file%s +%d −%d", g.files, plural(g.files), g.plus, g.minus)
	}
	return line, true
}

// stageElapsed prefers BoundAt (when the loop is bound and its registry
// record actually carries one); falling back to the event log's earliest
// entry for this session (oldest-first, per events.Read's contract) —
// "elapsed since we first knew about this loop" either way. ok=false when
// NEITHER source is available.
func stageElapsed(l domain.Loop, data detailData) (time.Duration, bool) {
	if !l.BoundAt.IsZero() {
		return data.now.Sub(l.BoundAt), true
	}
	if len(data.events) == 0 {
		return 0, false
	}
	return data.now.Sub(time.Unix(0, data.events[0].TS)), true
}

// lastErrorForDetail applies the staleness suppression rule to data's
// already-cached LAST ERROR extraction (see detailCacheCmd — this no
// longer calls claude.LastError itself, fix/exit-gate-ux moved that off
// the render path): don't show a stale error on a loop that has since
// recovered — compare against the event log's last transition INTO a
// healthy state (running or idle). ok=false when there's no cached error,
// or it's older than that recovery.
func lastErrorForDetail(data detailData) (string, time.Time, bool) {
	if !data.lastError.ok {
		return "", time.Time{}, false
	}
	if isErrorStale(data.lastError.ts, data.events) {
		return "", time.Time{}, false
	}
	return data.lastError.text, data.lastError.ts, true
}

// isErrorStale reports whether errorTS predates the event log's most
// recent scan-triggered transition into StateRunning or StateIdle — i.e.
// the loop recovered AFTER this error was recorded, so it's stale news, not
// a live incident.
// Review fix (P2): a zero errorTS (claude.LastError couldn't parse the
// transcript entry's timestamp — see entryTimestamp's doc) must FAIL OPEN
// (treated as NOT stale, i.e. shown) rather than silently suppressing a
// possibly-live error. The old code compared errorTS.UnixNano() directly —
// the zero time.Time's UnixNano() is a huge NEGATIVE number, which is
// "less than" any real lastHealthy timestamp, so an unparseable timestamp
// was ALWAYS judged older than the last recovery and the block simply
// never showed, with no visible symptom other than "LAST ERROR never
// appears" — exactly the failure mode a "fail open" default exists to
// avoid: if the transcript's timestamp format ever drifts from what
// entryTimestamp expects, this must not become a silent, permanent blind
// spot.
func isErrorStale(errorTS time.Time, evs []events.Event) bool {
	if errorTS.IsZero() {
		return false
	}
	var lastHealthy int64
	for _, ev := range evs {
		if ev.Trigger != events.TriggerScan {
			continue
		}
		if ev.ToState != string(domain.StateRunning) && ev.ToState != string(domain.StateIdle) {
			continue
		}
		if ev.TS > lastHealthy {
			lastHealthy = ev.TS
		}
	}
	return lastHealthy > 0 && errorTS.UnixNano() < lastHealthy
}

// lastErrorMaxLines caps the LAST ERROR block's wrapped, head-truncated
// display — "max ~5 lines" per the task; content beyond it is marked with
// an ellipsis (via wrapTailText, the same word-wrap+cap machinery TAIL
// uses) rather than shown unbounded (a single giant stack trace must not
// swallow the whole panel).
const lastErrorMaxLines = 5

// renderLastErrorBlock renders the "LAST ERROR (hh:mm:ss · verbatim)"
// section: a bold red label carrying the entry's own timestamp, then the
// RAW error text (council hard rule: never paraphrased), word-wrapped and
// capped.
func renderLastErrorBlock(text string, ts time.Time, width int) string {
	var b strings.Builder
	b.WriteString("\n")
	label := fmt.Sprintf("LAST ERROR (%s · verbatim)", ts.Format("15:04:05"))
	b.WriteString(lipgloss.NewStyle().Foreground(cRed).Bold(true).Render(label))
	b.WriteString("\n")
	for _, line := range wrapTailText(text, width, lastErrorMaxLines) {
		b.WriteString(stInk.Render(line))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderVerdictsBlock renders "VERDICTS (N/I n)" — the newest 3 oracle
// verdicts from the event log, each "hh:mm ✓/✗ \"<reason verbatim>\"".
// Falls back to the loop's current registry verdict (l.Last) alone when
// the event log has none yet (e.g. history predates this feature, or was
// pruned) — but that fallback needs the Loop itself, which this function
// doesn't receive; see its caller in renderDetail, which only calls this
// when data.events actually has oracle entries, and separately renders
// l.Last via the existing ORACLE row when it doesn't. Returns "" (render
// nothing) when there are no verdicts at all.
func renderVerdictsBlock(evs []events.Event, width int) string {
	var oracleEvs []events.Event
	for _, ev := range evs {
		if ev.Trigger == events.TriggerOracle {
			oracleEvs = append(oracleEvs, ev)
		}
	}
	if len(oracleEvs) == 0 {
		return ""
	}
	// newest first, top 3 — oracleEvs is a sub-slice of evs, which
	// events.Read guarantees oldest-first.
	start := len(oracleEvs) - 3
	if start < 0 {
		start = 0
	}
	newest := oracleEvs[start:]

	var b strings.Builder
	b.WriteString("\n")
	b.WriteString(stFaint.Render(fmt.Sprintf("VERDICTS (%d)", len(oracleEvs))))
	for i := len(newest) - 1; i >= 0; i-- {
		ev := newest[i]
		outcome, reason := events.ParseOracleDetail(ev.Detail)
		icon, style := "✓", lipgloss.NewStyle().Foreground(cGreen)
		if outcome == string(domain.OutcomeRejected) {
			icon, style = "✗", lipgloss.NewStyle().Foreground(cRed)
		}
		t := time.Unix(0, ev.TS).Format("15:04")
		line := fmt.Sprintf("%s %s %q", t, icon, reason)
		b.WriteString("\n")
		b.WriteString(style.Render(trunc(line, width)))
	}
	return b.String()
}

// eventActorGlyph: ☺ human, ⎇ auto, blank (2 spaces, for column alignment)
// for system/scan-triggered events — the task's own glyph legend.
func eventActorGlyph(a events.Actor) string {
	switch a {
	case events.ActorHuman:
		return "☺ "
	case events.ActorAuto:
		return "⎇ "
	default:
		return "  "
	}
}

// eventBody renders one event's from→to/action/verdict payload — the part
// after the timestamp+glyph — varying by Trigger:
//   - scan/governor: "<from>→<to>", plus Detail if any (e.g. a stall kind).
//   - actuation: Detail verbatim (already a compact "<action> <tier>
//     ok|failed: ..." string — see logActuationEvent).
//   - oracle: "<outcome> \"<reason>\"" (verbatim reason, council hard rule).
func eventBody(ev events.Event) string {
	switch ev.Trigger {
	case events.TriggerActuation:
		return ev.Detail
	case events.TriggerOracle:
		outcome, reason := events.ParseOracleDetail(ev.Detail)
		if reason != "" {
			return fmt.Sprintf("%s %q", outcome, reason)
		}
		return outcome
	default: // scan, governor
		from := ev.FromState
		if from == "" {
			from = "—"
		}
		body := from + "→" + ev.ToState
		if ev.Detail != "" {
			body += " " + ev.Detail
		}
		return body
	}
}

// renderEventsBlock renders the EVENTS section: the loop's history
// newest-first, filling maxRows total lines (including its own "EVENTS"
// title line — so maxRows-1 actual event lines). Never coalesces repeated
// transitions (flapping IS the signal, per the task) — a session that
// bounced stalled→running→stalled 5 times shows all 5 lines.
//
// fix/detail-dedup (UX judge item 4, re-judge finding): TriggerOracle
// events are skipped entirely — VERDICTS (renderVerdictsBlock) is the
// dedicated oracle view, and it was showing the SAME verdict (same ts,
// same verbatim reason) a second time here, with only the glyph differing
// from VERDICTS' own. EVENTS is the actuation/scan/governor timeline; a
// judgment belongs in exactly one place.
func renderEventsBlock(evs []events.Event, width, maxRows int) string {
	if len(evs) == 0 || maxRows < eventsMinRows {
		return ""
	}
	dataRows := maxRows - 1 // one row spent on the title
	var b strings.Builder
	b.WriteString(stFaint.Render("EVENTS"))
	shown := 0
	for i := len(evs) - 1; i >= 0 && shown < dataRows; i-- {
		ev := evs[i]
		if ev.Trigger == events.TriggerOracle {
			continue
		}
		t := time.Unix(0, ev.TS).Format("15:04")
		line := fmt.Sprintf("%s %s%s", t, eventActorGlyph(ev.Actor), eventBody(ev))
		b.WriteString("\n")
		b.WriteString(stDim.Render(trunc(line, width)))
		shown++
	}
	if shown == 0 {
		// every entry in evs was an oracle verdict (now excluded above) —
		// an "EVENTS" header with nothing under it would be worse than no
		// block at all, same "omit rather than render empty" convention
		// renderVerdictsBlock already follows.
		return ""
	}
	return b.String()
}

// ordinal renders 1→"1st", 2→"2nd", 3→"3rd", 4→"4th", ... (English
// ordinal suffix rules — the flap counter's "3rd stall in 20m" wording).
func ordinal(n int) string {
	if n%100 >= 11 && n%100 <= 13 {
		return fmt.Sprintf("%dth", n)
	}
	switch n % 10 {
	case 1:
		return fmt.Sprintf("%dst", n)
	case 2:
		return fmt.Sprintf("%dnd", n)
	case 3:
		return fmt.Sprintf("%drd", n)
	default:
		return fmt.Sprintf("%dth", n)
	}
}

// flapCounter counts scan-triggered transitions INTO any stalled state
// within the last hour of now — the STALLED/429 callout's "(3rd stall in
// 20m)" annotation. ok=false when count<=1 (nothing to call out — nobody
// cares that a loop stalled exactly once). span is the time from the
// EARLIEST counted stall to now, matching "Nth stall in <span>" — how long
// this flapping pattern has been going on.
func flapCounter(evs []events.Event, now time.Time) (count int, span time.Duration, ok bool) {
	cutoff := now.Add(-time.Hour).UnixNano()
	var first int64
	for _, ev := range evs {
		if ev.Trigger != events.TriggerScan || ev.TS < cutoff {
			continue
		}
		if !strings.HasPrefix(ev.ToState, string(domain.StateStalled)) {
			continue
		}
		count++
		if first == 0 || ev.TS < first {
			first = ev.TS
		}
	}
	if count <= 1 {
		return count, 0, false
	}
	return count, now.Sub(time.Unix(0, first)), true
}

// renderOracleDetail is the ORACLE row's value: a COMPACT glyph + the
// cycle the verdict landed at (e.g. "✓ at cycle 4" / "✗ at cycle 6"),
// colored by outcome. "—" if never judged yet.
//
// fix/exit-gate-ux (UX judge item 4) first tightened this to compact
// glyph+cycle ONLY for StateDrift, since renderDriftCallout already prints
// l.Last.Reason as its own headline there. fix/detail-dedup (re-judge
// finding: the SAME reason still rendered 3x on a DONE loop too — ORACLE
// row + VERDICTS block, both verbatim) drops that State-specific carve-out
// entirely: EVERY outcome now gets the compact form here, full stop. The
// verbatim reason lives in exactly ONE place — VERDICTS
// (renderVerdictsBlock) — with the DRIFT/GATE/STALLED action callouts
// keeping their own single additional headline occurrence (that's "the
// problem + what to act on now", a defensible distinct purpose from
// VERDICTS' judgment history — not a verbatim triple).
func renderOracleDetail(l domain.Loop) string {
	if l.Last == nil {
		return stFaint.Render("—")
	}
	icon, style := "✓", stDim
	switch l.Last.Outcome {
	case domain.OutcomeDone:
		style = lipgloss.NewStyle().Foreground(cGreen)
	case domain.OutcomeRejected:
		icon = "✗"
		style = lipgloss.NewStyle().Foreground(cRed)
	}
	return style.Render(fmt.Sprintf("%s at cycle %d", icon, l.Last.AtCycle))
}

// detailRow is one KEY  value line in the mockup's key-value grid (faint
// uppercase key, detailKeyWidth cols wide). value is assumed single-line and
// already styled by the caller; use detailRowMultiline for wrapped values.
func detailRow(key, value string) string {
	return stFaint.Width(detailKeyWidth).Render(key) + value + "\n"
}

// detailRowMultiline renders a KEY row whose value spans multiple lines: the
// KEY sits on the first line (same shape as detailRow), and every continuation
// line is indented by detailKeyWidth so its text aligns under the value column
// — a clean continuation, not a new row and not the label repeated. Callers
// style the value lines themselves (as with detailRow). Empty lines → "".
func detailRowMultiline(key string, lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(stFaint.Width(detailKeyWidth).Render(key))
	b.WriteString(lines[0])
	b.WriteString("\n")
	indent := strings.Repeat(" ", detailKeyWidth)
	for _, line := range lines[1:] {
		b.WriteString(indent)
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// wrapTailText word-wraps s to width columns and returns at most maxLines
// plain (unstyled) lines for the detail pane's TAIL row. s is already
// single-line (newlines collapsed by internal/claude.summarizeTailText); this
// splits it across the pane's value column. When the wrapped text has MORE
// than maxLines lines (content was cut), the last returned line ends with the
// same "…" marker trunc uses, so it's visually clear there's more — otherwise
// every wrapped line is returned as-is (no padding, no wasted blank lines).
func wrapTailText(s string, width, maxLines int) []string {
	if width <= 0 || maxLines <= 0 {
		return nil
	}
	// lipgloss.Width word-wraps (hard-breaking any single word longer than
	// width) and left-pads each line to width; strip that padding so the
	// lines carry only their own text.
	wrapped := strings.Split(lipgloss.NewStyle().Width(width).Render(s), "\n")
	for i := range wrapped {
		wrapped[i] = strings.TrimRight(wrapped[i], " ")
	}
	if len(wrapped) <= maxLines {
		return wrapped
	}
	wrapped = wrapped[:maxLines]
	last := maxLines - 1
	// Force a truncation marker on the last shown line. trunc keeps it within
	// width: a full-width line drops its last rune for the "…"; a short line
	// simply gains the "…".
	wrapped[last] = trunc(wrapped[last]+"…", width)
	return wrapped
}

// renderResumeCallout is the mockup's amber gate-line, repurposed for a
// stall: "RESUME ▸ <why>   r re-send prompt   manual: claude --resume <id>".
// A 429 gets the red accent instead of amber (the turn didn't complete, it
// was rejected — a sharper signal than a generic stall). A gone process
// gets red too, but with "restart" wording instead of "resume" — since the
// ADR Phase 2 Tier 2 redrive landed, "r" still works here: sendPromptCmd
// skips the (correctly absent) terminal surface and re-drives the SAME
// session headlessly via `claude --resume <id> -p <prompt>`, which is
// exactly the restart the manual hint spells out, just without the
// copy-paste.
// renderResumeCallout's evs/now (feat/detail-panel-v2) drive the flap
// counter — "(3rd stall in 20m)" appended to the stall-kind text when this
// loop has stalled more than once in the last hour (flapCounter). Every
// existing caller in this codebase now must pass the session's event
// history; renderDetail is the only one, via data.events/data.now.
func renderResumeCallout(l domain.Loop, width int, evs []events.Event, now time.Time) string {
	gone := l.Stall == domain.StallGone
	box, accent, chip := stCalloutAmber, cAmber, stKeyChipAmber
	if l.Stall == domain.StallRateLimit || gone {
		box, accent, chip = stCalloutRed, cRed, stKeyChipRed
	}
	// border(1) + padding(1) on each side.
	contentWidth := width - 4
	if contentWidth < 20 {
		contentWidth = 20
	}

	label := "RESUME ▸"
	action := chip.Render("r") + stDim.Render(" re-send prompt") +
		"   " + stDim.Render("manual: "+manualResumeHint(l.SessionID))
	if gone {
		label = "RESTART ▸"
		action = chip.Render("r") + stDim.Render(" re-drive headlessly (tier 2)") +
			"   " + stDim.Render("manual: "+manualResumeHint(l.SessionID))
	}

	stallText := string(l.Stall)
	if count, span, ok := flapCounter(evs, now); ok {
		stallText += fmt.Sprintf("  (%s stall in %s)", ordinal(count), formatUptime(span))
	}

	line := lipgloss.NewStyle().Foreground(accent).Bold(true).Render(label) +
		" " + stInk.Render(stallText) +
		"   " + action
	return "\n" + box.Width(contentWidth).Render(line)
}

// renderGateCallout is the mockup's gate-line for a loop waiting on a human
// decision — driven by the Notification hook, not a screen-scrape guess:
// "GATE ▸ <prompt>   a approve   ↵ attach to answer".
func renderGateCallout(l domain.Loop, width int) string {
	contentWidth := width - 4
	if contentWidth < 20 {
		contentWidth = 20
	}
	prompt := l.GatePrompt
	if prompt == "" {
		prompt = "claude is asking for permission"
	}
	line := lipgloss.NewStyle().Foreground(cAmber).Bold(true).Render("GATE ▸") +
		" " + stInk.Render(prompt) +
		"   " + stKeyChipAmber.Render("a") + stDim.Render(" approve") +
		"   " + stKeyChipAmber.Render("↵") + stDim.Render(" attach to answer")
	return "\n" + stCalloutAmber.Width(contentWidth).Render(line)
}

// renderDriftCallout is the mockup's red gate-line for a loop the oracle
// rejected: "DRIFT ▸ <reason>   r re-drive with hint   k kill". "r" opens
// the one-line hint-input step (feat/drift-guided-redrive) before
// re-driving the loop with its LAST USER PROMPT plus the operator's
// optional corrective hint appended (see composeDriftPrompt) — no longer a
// blind resend of the exact prompt the oracle just rejected.
func renderDriftCallout(l domain.Loop, width int) string {
	contentWidth := width - 4
	if contentWidth < 20 {
		contentWidth = 20
	}
	reason := "oracle rejected this loop's \"done\" claim"
	if l.Last != nil && l.Last.Reason != "" {
		reason = l.Last.Reason
	}
	line := lipgloss.NewStyle().Foreground(cRed).Bold(true).Render("DRIFT ▸") +
		" " + stInk.Render(reason) +
		"   " + stKeyChipRed.Render("r") + stDim.Render(" re-drive with hint") +
		"   " + stKeyChipRed.Render("k") + stDim.Render(" kill")
	return "\n" + stCalloutRed.Width(contentWidth).Render(line)
}

// ── status line / keybar ─────────────────────────────────────

// renderStatusLine shows the last action's result on its own line above the
// keybar: green on success, red on failure, amber for a pending kill-confirm
// warning, dim otherwise.
func renderStatusLine(status string, kind statusKind) string {
	if status == "" {
		return ""
	}
	style := stDim
	switch kind {
	case statusOK:
		style = lipgloss.NewStyle().Foreground(cGreen)
	case statusErr:
		style = lipgloss.NewStyle().Foreground(cRed)
	case statusWarn:
		style = lipgloss.NewStyle().Foreground(cAmber)
	}
	return style.Render(status)
}

// renderNewLoopPrompt replaces the status line while the "n" key's
// loop-contract wizard is active: "NEW LOOP ▸ <step label> <input>". The
// final wizardWhere step is single-key (no free-text input), so it's
// special-cased to render just the label — see whereStepLabel, which needs
// Model context (fleet/eligibility) that a plain wizardStep can't carry.
func renderNewLoopPrompt(m Model) string {
	switch m.spawnStep {
	case wizardWhere:
		return lipgloss.NewStyle().Foreground(cAccent).Bold(true).Render("NEW LOOP ▸ " + m.whereStepLabel())
	case wizardEngineDrive:
		return lipgloss.NewStyle().Foreground(cAccent).Bold(true).Render("NEW LOOP ▸ " + m.engineDriveStepLabel())
	}
	return lipgloss.NewStyle().Foreground(cAccent).Bold(true).Render("NEW LOOP ▸ "+wizardStepLabel(m.spawnStep)+" ") + m.input.View()
}

// engineDriveStepLabel is wizardEngineDrive's single-key prompt (LoopEngine
// MVP) — same "no free-text input, so it needs its own label, not
// wizardStepLabel's" shape as whereStepLabel. A method (not the constant it
// used to be) because it now shows the spawn target dir: engine-drive spawns
// headless in spawnCwd without ever reaching wizardWhere, so this is the
// engine path's only chance to see — and [c]hange — where the loop will run.
func (m Model) engineDriveStepLabel() string {
	return fmt.Sprintf("drive? [e] engine-drive · [m] manual (observe only) · [c] change dir — in %s:", displayDir(m.spawnCwd))
}

// wizardStepLabel is each free-text wizard step's prompt label —
// max_iteration's carries the default inline ("[12]"), since there's no
// separate placeholder shown on the input (see newWizardInput). Does not
// cover wizardWhere (see whereStepLabel) or wizardEngineDrive (see
// engineDriveStepLabel) — both single-key steps with no free-text input.
func wizardStepLabel(step wizardStep) string {
	switch step {
	case wizardGoal:
		return "goal:"
	case wizardName:
		return "name (fleet list label, optional):"
	case wizardDoneWhen:
		return "complete condition:"
	case wizardRubric:
		return "rubric:"
	case wizardChallenger:
		return "challenger:"
	case wizardMaxCycles:
		return fmt.Sprintf("max_iteration [%d]:", registry.DefaultMaxCycles)
	case wizardDir:
		return "dir (absolute or ~ path; empty keeps current):"
	default:
		return ""
	}
}

// whereStepLabel builds the wizard's final "where to spawn" prompt. It
// always names the target directory — the spawn base must be visible BEFORE
// the human commits, never discovered from the status line after. [w] is
// offered only when the backend can actually isolate; [s] only when the
// selected loop has a verified-real cwd that differs from the current
// target (the explicit replacement for the old silent inheritance). The
// busy-directory nudge appends when the target already hosts >=1 fleet loop
// (independent of — and a stronger UX nudge than — spawnHostsClaudeRepo,
// which only gates the w/enter default).
func (m Model) whereStepLabel() string {
	label := "where?"
	if m.spawnWorktreeEligible {
		label += " [w] new worktree ·"
	}
	label += " [d] this dir · [c] change dir"
	if sel, ok := m.selected(); ok && sel.CwdVerified && sel.Cwd != "" && sel.Cwd != m.spawnCwd {
		label += fmt.Sprintf(" · [s] %s's dir", sel.Project)
	}
	label += fmt.Sprintf(" — in %s:", displayDir(m.spawnCwd))
	if m.spawnDirBusyCount() >= 1 {
		label += " (dir busy — worktree recommended)"
	}
	return label
}

// spawnDirBusyCount counts loops in the CURRENT fleet whose Cwd matches the
// wizard's spawn target directory — used only for whereStepLabel's nudge
// text (pure, no exec, safe to call on every render).
func (m Model) spawnDirBusyCount() int {
	n := 0
	for _, l := range m.loops {
		if l.Cwd == m.spawnCwd {
			n++
		}
	}
	return n
}

// renderFilterPrompt replaces the status line while the "/" key's filter
// query is active: "FILTER ▸ <input>".
func renderFilterPrompt(input textinput.Model) string {
	return lipgloss.NewStyle().Foreground(cAccent).Bold(true).Render("FILTER ▸ ") + input.View()
}

// renderInjectPrompt replaces the status line while the "i" key's arbitrary
// prompt is being typed: "INJECT ▸ <project> ◂ <input>", so the human always
// sees WHICH loop the text is bound for (it was snapshotted at keypress time
// and can't change under them). When the target is StateRunning the text will
// land mid-turn at an unpredictable point in claude's input — not a blocker
// (that would defeat the feature), but a real footgun, so an amber nudge is
// appended rather than pretending it's risk-free.
func renderInjectPrompt(m Model) string {
	head := lipgloss.NewStyle().Foreground(cAccent).Bold(true).Render("INJECT ▸ " + m.injectTarget.Project + " ◂ ")
	line := head + m.input.View()
	if m.injectTarget.State == domain.StateRunning {
		line += "  " + lipgloss.NewStyle().Foreground(cAmber).Render("(running — lands mid-turn)")
	}
	return line
}

// renderDriftHintPrompt replaces the status line while the "r" key's
// DRIFT-loop hint input is active: "RE-DRIVE ▸ <project> ◂ hint
// (enter=none): <input>" — same "always show which loop this targets"
// discipline as renderInjectPrompt.
func renderDriftHintPrompt(m Model) string {
	head := lipgloss.NewStyle().Foreground(cAccent).Bold(true).
		Render("RE-DRIVE ▸ " + m.driftHintTarget.Project + " ◂ hint (enter=none): ")
	return head + m.input.View()
}

// ── layout helpers ────────────────────────────────────────────

// padBetween left-aligns left and right-aligns right within width, joined by
// spaces, and — F1 — actually GUARANTEES the result fits within width: it
// used to just floor the gap at 1 and concatenate regardless, so a narrow
// terminal (e.g. a live-measured w=45 rendering 65 cols) never degraded at
// all. Degrades in two steps, matching the header/summary band's own
// priority (left is the identity/label, right is supplementary status):
//  1. once left+right no longer both fit, drop right entirely;
//  2. if even left alone still overflows, ANSI-aware truncate it
//     (ansi.TruncateWc) — left/right here are already lipgloss-styled, so a
//     plain rune/byte truncate would risk cutting mid-escape-sequence and
//     corrupting the terminal (unlike trunc(), which this codebase only
//     ever applies to PLAIN text, styling it afterward).
func padBetween(left, right string, width int) string {
	if right == "" {
		return fitWithin(left, width)
	}
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap >= 1 {
		return left + strings.Repeat(" ", gap) + right
	}
	return fitWithin(left, width)
}

// fitWithin returns s unchanged if it already fits width, else ANSI-aware
// truncates it — see padBetween's doc for why plain trunc() isn't safe for
// already-styled input.
func fitWithin(s string, width int) string {
	if lipgloss.Width(s) <= width {
		return s
	}
	return ansi.TruncateWc(s, width, "…")
}

// padToWidth right-pads s with spaces until it reaches width (visible
// width, ANSI-aware via lipgloss.Width), so a background fill spans evenly.
func padToWidth(s string, width int) string {
	w := lipgloss.Width(s)
	if w >= width {
		return s
	}
	return s + strings.Repeat(" ", width-w)
}

// formatUptime: mm:ss under an hour, hh:mm from an hour on.
func formatUptime(d time.Duration) string {
	if d < time.Hour {
		return fmt.Sprintf("%02d:%02d", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%02d:%02d", int(d.Hours()), int(d.Minutes())%60)
}

// budgetBar renders the mockup's budget meter: a width-char bar of █
// (filled, rounded from frac) then ░ (remainder), followed by " NN%". frac
// is clamped to [0,1] first — defensive, since BudgetFrac() already clamps,
// but this is a general-purpose pure func.
func budgetBar(frac float64, width int) string {
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	filled := int(math.Round(frac * float64(width)))
	if filled > width {
		filled = width
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
	return fmt.Sprintf("%s %d%%", bar, int(math.Round(frac*100)))
}

// prettyTokens pretty-prints a token count in the mockup's compact k/M
// style: under 1,000 → plain digits, under 1,000,000 → "<n>k" (rounded),
// otherwise → "<n.n>M" (one decimal).
func prettyTokens(n int) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 1_000_000:
		return fmt.Sprintf("%dk", int(math.Round(float64(n)/1000)))
	default:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
}

// ── misc helpers ────────────────────────────────────────────

func rel(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// trunc truncates s to at most n terminal COLUMNS — not bytes (multi-byte
// glyphs like █/⚠ would corrupt into "�") and not runes either: CJK text
// renders double-width, so a rune count under-measures by up to 2× and the
// overflowing cell wraps, shearing every column after it (observed in
// practice with Korean DOING/TAIL snippets). go-runewidth measures what the terminal
// actually draws; the ellipsis is budgeted inside n.
func trunc(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if narrowAmbiguous.StringWidth(s) <= n {
		return s
	}
	return narrowAmbiguous.Truncate(s, n, "…")
}

// narrowAmbiguous measures text the way this cockpit's own layout engine and
// the terminals it targets actually draw it: East Asian **Ambiguous**
// characters — "…", "●", "◆", the box-drawing runes — occupy ONE column.
//
// Pinned rather than inherited. go-runewidth's DefaultCondition auto-detects
// from the process locale and widens Ambiguous glyphs to 2 columns under
// ko/ja/zh, which disagrees with both sides trunc() has to line up with:
// lipgloss v1.1.0 measures via charmbracelet/x/ansi and always treats them as
// 1, and iTerm2 draws them as 1 (measured on the maintainer's machine:
// "Ambiguous Double Width" = false). Inheriting the locale therefore made
// trunc() — the ONLY runewidth caller in the tree — reserve a column the
// renderer never used, cutting text one column early on an East Asian locale
// while every other component stayed at 1. Issue #44.
//
// CJK proper is unaffected either way: Hangul/Kana/Han are Wide, not
// Ambiguous, so they measure 2 under both conditions — which is the whole
// point of measuring in columns here (see trunc's doc above).
var narrowAmbiguous = &runewidth.Condition{EastAsianWidth: false}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
