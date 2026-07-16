// Package tui is the fleet cockpit (Bubble Tea): aggregate list + right-pane
// detail + one-key action, refreshed from the Claude Code logs (seed spec §UX).
// Visual language matches the approved mockup (html-artifacts/mission-control-tui.html).
package tui

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/jitokim/missionctl/internal/claude"
	"github.com/jitokim/missionctl/internal/control"
	"github.com/jitokim/missionctl/internal/domain"
	"github.com/jitokim/missionctl/internal/gate"
	"github.com/jitokim/missionctl/internal/oracle"
	"github.com/jitokim/missionctl/internal/registry"
)

// judgeFn/registryDirFn are oracle.Judge/registry.LoopsDir by default,
// overridable in tests so the judge trigger-policy state machine (and
// judgeCmd's registry write) can be verified without exec or touching the
// real ~/.missionctl/loops.
var (
	judgeFn       = oracle.Judge
	registryDirFn = registry.LoopsDir
)

type loopsMsg []domain.Loop
type tickMsg time.Time

// resumeResultMsg reports the outcome of a resume (r key) attempt, computed
// off the event loop by resumeCmd so the TUI never blocks on exec.
type resumeResultMsg struct {
	ok   bool
	text string
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

// scan is a tea.Cmd: rediscover the fleet from the logs.
func scan() tea.Msg {
	loops, _ := claude.DiscoverLoops(time.Now(), claude.ActiveWindow)
	return loopsMsg(loops)
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
// free-text goal prompt and the "/" key's filter query, so arrow/letter
// keys route to the text input instead of moving the cursor or triggering
// actions while typing.
type mode int

const (
	modeNormal mode = iota
	modePrompting
	modeFiltering
)

// wizardStep is which question of the "n" key's loop-contract wizard is
// currently active. The wizard collects the full contract the wizard's
// caller (spawnCmd/buildSpawnPrompt) injects into the new session AND the
// same contract the oracle later judges against (internal/oracle.Judge) —
// one document, told to the agent and used to verify it.
type wizardStep int

const (
	wizardGoal       wizardStep = iota // required; empty cancels
	wizardDoneWhen                     // optional; completion condition
	wizardOracle                       // optional; verification rubric
	wizardChallenger                   // optional; adversarial probe description, STORED ONLY
	wizardMaxCycles                    // optional; empty = registry.DefaultMaxCycles
	wizardWhere                        // single-key w/d/enter; only reached when the backend supports worktree spawn — see advanceSpawnWizard
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

	mode      mode
	input     textinput.Model
	spawnCwd  string // captured when "n" is pressed: target loop's Cwd, or os.Getwd()
	spawnNote string // captured alongside spawnCwd: non-empty when the selected loop's cwd wasn't verified-real and spawn fell back to os.Getwd() (see the "n" handler and P1-3's CwdVerified gating)

	// spawnStep/spawnGoal/spawnDoneWhen/spawnOracle/spawnChallenger/
	// spawnMaxCycles hold the "n" key wizard's in-progress answers across
	// its steps (see wizardStep) — spawnCwd/spawnNote above are captured
	// once, before step 1, and don't change as the wizard advances.
	spawnStep       wizardStep
	spawnGoal       string
	spawnDoneWhen   string
	spawnOracle     string
	spawnChallenger string
	spawnMaxCycles  int

	// spawnWorktreeEligible/spawnHostsClaudeRepo drive the final wizardWhere
	// step's default and whether it's shown at all:
	//   - spawnWorktreeEligible: does the resolved backend implement
	//     control.WorktreeSpawner (orca only)? Computed OFF the event loop
	//     (control.Resolve does real exec calls) by checkWorktreeEligibilityCmd,
	//     fired at "n" keypress time — by the time a human types through 4-5
	//     wizard steps the result has almost always arrived, but the
	//     zero-value (false) is a safe fallback if it hasn't.
	//   - spawnHostsClaudeRepo: true when "n" was pressed with a loop
	//     selected (independent evidence claude has actually run in
	//     spawnCwd) — see the "n" handler.
	spawnWorktreeEligible bool
	spawnHostsClaudeRepo  bool

	filterQuery string // the APPLIED "/" filter (post-enter); "" means no filter

	pendingKillSession string // non-empty while awaiting the confirming second "k"
	pendingKillAt      time.Time

	judging map[string]bool // sessionID -> a judgeCmd is in flight for it (in-flight guard, see triggerJudgments)
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
	}
}

// killConfirmWindow: the second "k" must land within this long of the first
// to actually kill — otherwise it starts a fresh confirm cycle instead.
const killConfirmWindow = 3 * time.Second

func (m Model) Init() tea.Cmd { return tea.Batch(scan, tick()) }

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
	case loopsMsg:
		m.loops = []domain.Loop(msg)
		if m.cursor >= len(m.visibleLoops()) {
			m.cursor = maxInt(0, len(m.visibleLoops())-1)
		}
		m.lastScan = time.Now()
		return m, m.triggerJudgments()
	case tickMsg:
		return m, tea.Batch(scan, tick())
	case tea.KeyMsg:
		key := msg.String()
		if key != "k" {
			m.pendingKillSession = "" // any key other than a repeat "k" cancels a pending kill-confirm
		}

		if m.mode == modePrompting {
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
					return m.submitSpawnWizard(true) // explicit: attempt worktree if the backend can do it at all
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
			if msg, ambiguous := m.refuseIfAmbiguous(sel); ambiguous {
				m.status, m.statusKind = msg, statusErr
				return m, nil
			}
			m.status, m.statusKind = fmt.Sprintf("resuming %s...", sel.Project), statusNeutral
			return m, resumeCmd(sel)
		case "a":
			sel, ok := m.selected()
			if !ok || sel.State != domain.StateGate {
				m.status, m.statusKind = "select a gated loop", statusNeutral
				return m, nil
			}
			if msg, ambiguous := m.refuseIfAmbiguous(sel); ambiguous {
				m.status, m.statusKind = msg, statusErr
				return m, nil
			}
			m.status, m.statusKind = fmt.Sprintf("approving %s...", sel.Project), statusNeutral
			return m, approveCmd(sel)
		case "enter":
			sel, ok := m.selected()
			if !ok {
				m.status, m.statusKind = "select a loop to attach", statusNeutral
				return m, nil
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
			cwd, err := os.Getwd()
			if err != nil {
				cwd = "."
			}
			spawnNote := ""
			sel, selOK := m.selected()
			hostsClaudeRepo := selOK && sel.Cwd != "" // independent evidence claude has run in sel.Cwd — see wizardWhere's default
			if selOK && sel.Cwd != "" {
				// Only spawn into a loop's cwd once it's been confirmed
				// against a live process's real lsof path (see
				// applyLiveness/CwdVerified) — a dead loop's Cwd is at best
				// a lossy decode of ProjectDir (ambiguous when the real
				// directory name itself contains "-") and could point
				// spawn at the wrong directory entirely (P1-3).
				if sel.CwdVerified {
					cwd = sel.Cwd
				} else {
					spawnNote = fmt.Sprintf(" (%s's cwd wasn't verified — using %s instead)", sel.Project, cwd)
				}
			}
			m.spawnCwd = cwd
			m.spawnNote = spawnNote
			m.spawnStep = wizardGoal
			m.spawnGoal = ""
			m.spawnDoneWhen = ""
			m.spawnOracle = ""
			m.spawnChallenger = ""
			m.spawnMaxCycles = 0
			m.spawnWorktreeEligible = false // set once checkWorktreeEligibilityCmd's result arrives
			m.spawnHostsClaudeRepo = hostsClaudeRepo
			m.mode = modePrompting
			m.input = newWizardInput()
			return m, tea.Batch(textinput.Blink, checkWorktreeEligibilityCmd())
		case "k":
			sel, ok := m.selected()
			if !ok {
				m.status, m.statusKind = "select a loop to kill", statusNeutral
				return m, nil
			}
			now := time.Now()
			if m.pendingKillSession == sel.SessionID && now.Sub(m.pendingKillAt) <= killConfirmWindow {
				m.pendingKillSession = ""
				if msg, ambiguous := m.refuseIfAmbiguous(sel); ambiguous {
					m.status, m.statusKind = msg, statusErr
					return m, nil
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
			if msg, ambiguous := m.refuseIfAmbiguous(sel); ambiguous {
				m.status, m.statusKind = msg, statusErr
				return m, nil
			}
			m.status, m.statusKind = fmt.Sprintf("stopping %s...", sel.Project), statusNeutral
			return m, interruptCmd(sel)
		}
	case resumeResultMsg:
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

// resumeCmd re-sends a stalled loop's last prompt to the terminal surface
// hosting it, via whichever multiplexer backend (orca/cmux/tmux) is
// available. Runs off the event loop — exec calls belong in a tea.Cmd, never
// in Update.
func resumeCmd(l domain.Loop) tea.Cmd {
	return func() tea.Msg {
		// SAFETY: a StallGone loop's claude process is gone — the terminal
		// surface (if any) is now a bare shell. Sending the prompt into it
		// would type the prompt as a shell command instead of resuming
		// anything. Refuse before even resolving a backend.
		if l.Stall == domain.StallGone {
			return resumeResultMsg{false, "process gone — restart with: " + manualResumeHint(l.SessionID)}
		}
		// SAFETY: the governor stopped this loop (internal/engine.Check via
		// applyGovernor, no-improve limit reached) — StateFailed is
		// deliberately terminal (domain.LoopState.Terminal()). Resuming it
		// would silently re-drive a loop the runtime already decided to
		// fail closed on; the human must make a new decision (kill, or a
		// fresh contract), not have "r" quietly override the governor.
		if l.State == domain.StateFailed {
			return resumeResultMsg{false, "governor stopped this loop (no improvement) — k kill or start a new contract"}
		}
		ctrl, ok := control.Resolve()
		if !ok {
			return resumeResultMsg{false, "no orca/tmux/cmux — resume manually: " + manualResumeHint(l.SessionID)}
		}
		// LocateClaude (not Locate): resuming must land on a CONFIRMED claude
		// surface, never a bare shell sharing the same directory — see
		// Controller.LocateClaude and the P0-3 hardening rationale.
		target, ok := ctrl.LocateClaude(l.ProjectDir)
		if !ok {
			return resumeResultMsg{false, "no unambiguous claude surface — attach (↵) and act manually: " + manualResumeHint(l.SessionID)}
		}
		prompt, ok := claude.LastUserPrompt(l.Path)
		note := ""
		if !ok {
			note = " (no prior prompt found — sent Enter only)"
		}
		if err := ctrl.Resume(target, prompt); err != nil {
			return resumeResultMsg{false, fmt.Sprintf("resume %s failed: %v", l.Project, err)}
		}
		return resumeResultMsg{true, fmt.Sprintf("resumed %s via %s%s", l.Project, ctrl.Name(), note)}
	}
}

// manualResumeHint is the copy-pasteable fallback for bare terminals (no
// orca/cmux/tmux to actuate into) — observation still works everywhere, but
// actuation degrades to "tell the human what to type".
func manualResumeHint(sessionID string) string {
	return "claude --resume " + sessionID
}

// attachCmd brings the terminal surface hosting l to the front, via
// whichever multiplexer backend is available. Works for any loop state (not
// just stalled) — "jump to it" is useful for a running loop too. Runs off
// the event loop, same non-blocking pattern as resumeCmd.
func attachCmd(l domain.Loop) tea.Cmd {
	return func() tea.Msg {
		ctrl, ok := control.Resolve()
		if !ok {
			return attachResultMsg{false, "no orca/tmux/cmux — attach manually: " + manualAttachHint(l.Cwd)}
		}
		target, ok := ctrl.Locate(l.ProjectDir)
		if !ok {
			return attachResultMsg{false, "surface not found — attach manually: " + manualAttachHint(l.Cwd)}
		}
		if err := ctrl.Focus(target); err != nil {
			return attachResultMsg{false, fmt.Sprintf("attach %s failed: %v", l.Project, err)}
		}
		return attachResultMsg{true, fmt.Sprintf("attached %s via %s", l.Project, ctrl.Name())}
	}
}

// manualAttachHint is the copy-pasteable fallback for bare terminals (no
// orca/cmux/tmux to focus) — at least point the human at where the loop lives.
func manualAttachHint(cwd string) string {
	return "cd " + cwd
}

// approveCmd accepts claude's default option at a gate (a bare Enter to the
// surface hosting the loop) via whichever multiplexer backend is available.
// On success it also best-effort deletes the loop's gate marker, so the UI
// clears the ◆ GATE state on the very next scan rather than waiting for the
// staleness check to catch up. Runs off the event loop, same non-blocking
// pattern as resumeCmd/attachCmd.
func approveCmd(l domain.Loop) tea.Cmd {
	return func() tea.Msg {
		ctrl, ok := control.Resolve()
		if !ok {
			return approveResultMsg{false, "no orca/tmux/cmux — approve manually: attach and press Enter"}
		}
		// LocateClaude: approving must land on a confirmed claude surface,
		// never a bare shell (see resumeCmd's identical rationale).
		target, ok := ctrl.LocateClaude(l.ProjectDir)
		if !ok {
			return approveResultMsg{false, "no unambiguous claude surface — attach (↵) and act manually: press Enter"}
		}
		if err := ctrl.Approve(target); err != nil {
			return approveResultMsg{false, fmt.Sprintf("approve %s failed: %v", l.Project, err)}
		}
		// Compare-and-swap delete: only remove the marker THIS decision was
		// based on (l.GateTS) — a plain delete-by-name could destroy a BRAND
		// NEW marker that landed between this loop's scan snapshot and this
		// approve call (see gate.DeleteMarkerIfTS).
		gate.DeleteMarkerIfTS(gate.GatesDir(), l.SessionID, l.GateTS)
		return approveResultMsg{true, fmt.Sprintf("approved %s via %s", l.Project, ctrl.Name())}
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
		m.spawnStep = wizardDoneWhen
		m.input = newWizardInput()
		return m, textinput.Blink

	case wizardDoneWhen:
		m.spawnDoneWhen = value
		m.spawnStep = wizardOracle
		m.input = newWizardInput()
		return m, textinput.Blink

	case wizardOracle:
		m.spawnOracle = value
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

		if !m.spawnWorktreeEligible {
			// no backend supports worktree-isolated spawn (tmux/cmux, or no
			// backend resolved at all) — offering a choice that always
			// degrades to the same outcome would just be confusing, so
			// skip straight to a current-dir spawn.
			return m.submitSpawnWizard(false)
		}
		m.spawnStep = wizardWhere
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
	note := m.spawnNote
	if m.spawnDoneWhen == "" {
		note += " (no done condition — oracle judges against the goal only)"
	}
	if useWorktree {
		m.status, m.statusKind = fmt.Sprintf("spawning loop in a new worktree...%s", note), statusNeutral
	} else {
		m.status, m.statusKind = fmt.Sprintf("spawning loop in %s...%s", m.spawnCwd, note), statusNeutral
	}
	spec := registry.BindSpec{
		Goal:          m.spawnGoal,
		DoneCondition: m.spawnDoneWhen,
		Oracle:        m.spawnOracle,
		Challenger:    m.spawnChallenger,
		MaxCycles:     m.spawnMaxCycles,
	}
	return m, spawnCmd(m.spawnCwd, spec, useWorktree)
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
		prompt := buildSpawnPrompt(spec.Goal, spec.DoneCondition, spec.Oracle, spec.Challenger, spec.MaxCycles)

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

// worktreeNameFromGoal builds the `orca worktree create --name` value from
// the wizard's goal: "mctl-" + a lowercase [a-z0-9-] slug of the goal's
// first ~24 runes. Pure function so the slugging is directly testable.
func worktreeNameFromGoal(goal string) string {
	const maxRunes = 24
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
	return "mctl-" + slug
}

// buildSpawnPrompt composes the LOOP CONTRACT block sent as the new
// session's very first prompt. This is the SAME contract the wizard
// collected (see the "n" key) that also becomes the oracle's judging rubric
// (internal/oracle.Judge, via doneWhen/oracleText) — what the agent is told
// and what it's judged against are one document, not two that can drift
// apart. maxCycles is always shown resolved (never 0) — the wizard applies
// registry.DefaultMaxCycles before calling this. challenger's line is
// omitted entirely when empty: there's no challenger phase yet (see
// DESIGN.md), so an empty line naming a check that never runs would be
// actively misleading.
func buildSpawnPrompt(goal, doneWhen, oracleText, challenger string, maxCycles int) string {
	done := doneWhen
	if done == "" {
		done = "you judge the goal fully achieved"
	}
	oracleLine := oracleText
	if oracleLine == "" {
		oracleLine = "an independent LLM judge verifies against the complete condition"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "goal: %s\n", goal)
	fmt.Fprintf(&b, "complete condition: %s\n", done)
	fmt.Fprintf(&b, "oracle: %s\n", oracleLine)
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
		if l.Stall == domain.StallGone {
			return killResultMsg{true, fmt.Sprintf("%s already gone — nothing to kill", l.Project)}
		}
		ctrl, ok := control.Resolve()
		if !ok {
			return killResultMsg{false, "no orca/tmux/cmux — kill manually: type /exit in " + l.Project}
		}
		// LocateClaude: killing must land on a confirmed claude surface,
		// never a bare shell (see resumeCmd's identical rationale) — typing
		// "/exit" into an unrelated shell pane would be a real hazard.
		target, ok := ctrl.LocateClaude(l.ProjectDir)
		if !ok {
			return killResultMsg{false, "no unambiguous claude surface — attach (↵) and act manually: type /exit"}
		}
		if err := ctrl.Resume(target, "/exit"); err != nil {
			return killResultMsg{false, fmt.Sprintf("kill %s failed: %v", l.Project, err)}
		}
		return killResultMsg{true, fmt.Sprintf("killed %s", l.Project)}
	}
}

// interruptCmd stops a loop's current turn (Esc) without killing the
// process — the loop stays alive, resumable with r.
func interruptCmd(l domain.Loop) tea.Cmd {
	return func() tea.Msg {
		ctrl, ok := control.Resolve()
		if !ok {
			return interruptResultMsg{false, "no orca/tmux/cmux — stop manually: press Esc in " + l.Project}
		}
		// LocateClaude: interrupting must land on a confirmed claude surface,
		// never a bare shell (see resumeCmd's identical rationale).
		target, ok := ctrl.LocateClaude(l.ProjectDir)
		if !ok {
			return interruptResultMsg{false, "no unambiguous claude surface — attach (↵) and act manually: press Esc"}
		}
		if err := ctrl.Interrupt(target); err != nil {
			return interruptResultMsg{false, fmt.Sprintf("stop %s failed: %v", l.Project, err)}
		}
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

// judgeCmd asks the oracle to verdict a bound loop's progress against its
// goal, using its full (uncapped) last report, then saves the verdict to
// the registry at the loop's current cycle. Runs off the event loop, same
// non-blocking pattern as the other *Cmd funcs.
func judgeCmd(l domain.Loop) tea.Cmd {
	return func() tea.Msg {
		lastText, _ := claude.LastAssistantTextFull(l.Path) // ok=false just means an empty report is judged as-is
		verdict, err := judgeFn(l.Goal.Text, l.Cwd, lastText, l.Goal.DoneWhen, l.Goal.Oracle)
		if err != nil {
			return verdictMsg{sessionID: l.SessionID, err: err}
		}
		if err := registry.SaveVerdict(registryDirFn(), l.SessionID, verdict, l.Cycle); err != nil {
			return verdictMsg{sessionID: l.SessionID, err: err}
		}
		return verdictMsg{sessionID: l.SessionID, verdict: verdict}
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
	// prompt-type selector and the hint never renders (captain-reported).
	return []string{"less", "-R", "+G", "-M", "-PMmissionctl log — q to return (%pB\\%)", path}
}

func (m Model) selected() (domain.Loop, bool) {
	loops := m.visibleLoops()
	if m.cursor >= 0 && m.cursor < len(loops) {
		return loops[m.cursor], true
	}
	return domain.Loop{}, false
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
	return fmt.Sprintf("ambiguous: %d loops share %s's directory — attach (↵) and act manually", n, l.Project), true
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

func (m Model) View() string {
	width := m.termWidth()
	var b strings.Builder

	b.WriteString(renderHeaderRow(m, width))
	b.WriteString("\n")
	b.WriteString(renderRule(width))
	b.WriteString("\n")

	total, running, stalled, idle, gated, totalTokens, judged, good := m.counts()
	// The summary band's counts always describe the whole fleet, not the
	// filtered view — a filter narrows what you're looking AT, not the
	// facts. Only show the applied-filter indicator once it's committed
	// (not while still typing it — the prompt line below already shows the
	// live query, showing both would be redundant).
	bandFilter := ""
	if m.mode != modeFiltering {
		bandFilter = m.filterQuery
	}
	b.WriteString(renderSummaryBand(total, running, stalled, idle, gated, totalTokens, judged, good, bandFilter, width))
	b.WriteString("\n\n")

	b.WriteString(stFaint.Render("LOOPS"))
	b.WriteString("\n")

	wName, wDoing, wCycle, wOracle, wBudget, wNI, wNote := columnWidths(width)
	b.WriteString(renderTableHeader(wName, wDoing, wCycle, wOracle, wBudget, wNI, wNote))
	b.WriteString("\n")
	visible := m.visibleLoops()
	switch {
	case len(m.loops) == 0:
		b.WriteString(stFaint.Render("  no active Claude Code loops in the window.\n"))
	case len(visible) == 0:
		b.WriteString(stFaint.Render(fmt.Sprintf("  no loops match filter %q.\n", m.filterQuery)))
	}
	dupLabels := duplicateLabels(visible)
	for i, l := range visible {
		b.WriteString(renderRow(l, i == m.cursor, dupLabels[l.Project], wName, wDoing, wCycle, wOracle, wBudget, wNI, wNote, width))
		b.WriteString("\n")
	}

	// detail
	if sel, ok := m.selected(); ok {
		b.WriteString(renderDetail(sel, width))
	}

	// status line (its own line, above the keybar) — replaced by the
	// new-loop / filter prompt while in modePrompting/modeFiltering — + keybar.
	b.WriteString("\n")
	switch {
	case m.mode == modePrompting:
		b.WriteString(renderNewLoopPrompt(m))
		b.WriteString("\n")
	case m.mode == modeFiltering:
		b.WriteString(renderFilterPrompt(m.input))
		b.WriteString("\n")
	default:
		if line := renderStatusLine(m.status, m.statusKind); line != "" {
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	b.WriteString(renderKeybar(len(m.loops), width))
	return b.String()
}

// ── header / band / rule ────────────────────────────────────

// renderHeaderRow: left "◎ MISSIONCTL  fleet cockpit", right-aligned
// "● LIVE · <uptime> up · <hostname>".
func renderHeaderRow(m Model, width int) string {
	left := stTitle.Render("◎ MISSIONCTL") + stFaint.Render("  fleet cockpit")
	right := stLive.Render("●") + stDim.Render(" LIVE · ") +
		stDim.Bold(true).Render(formatUptime(time.Since(m.start))) +
		stDim.Render(" up · "+m.hostname)
	return padBetween(left, right, width)
}

func renderRule(width int) string {
	return lipgloss.NewStyle().Foreground(cLine).Render(strings.Repeat("─", width))
}

// renderSummaryBand: "fleet N · x run · y gate · z stalled · w idle ·
// budget <spent> · oracle P% · filter "q"" (zero-count segments omitted,
// fleet always shown; budget/oracle/filter omitted when there's no
// spend/no judged loops/no applied filter) with a right-aligned amber
// badge: gates take priority ("▲ N GATE NEEDS YOU") since a gate is a human
// actively being asked something right now; otherwise stalls get the badge
// ("▲ N STALLED NEED YOU") — the mockup's gate badge, honestly repurposed
// for stalls when there's no gate. budget is total spend across the fleet,
// not spent/cap — per-loop caps are all the same v0 default
// (DefaultBudgetTokens), so a fleet-wide cap would be a meaningless sum.
// oracle% is the share of judged (bound + verdict-rendered at least once)
// loops whose latest outcome is done or progress — i.e. "not currently
// drifting". These counts always describe the WHOLE fleet, not a filtered
// subset — see View's bandFilter comment.
func renderSummaryBand(total, running, stalled, idle, gated, totalTokens, judged, good int, filterQuery string, width int) string {
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
	left := strings.Join(parts, stFaint.Render(" · "))

	right := ""
	switch {
	case gated > 0:
		right = stBadgeStalled.Render(fmt.Sprintf("▲ %d GATE NEEDS YOU", gated))
	case stalled > 0:
		right = stBadgeStalled.Render(fmt.Sprintf("▲ %d STALLED NEED YOU", stalled))
	}
	return padBetween(left, right, width)
}

// ── row rendering ──────────────────────────────────────────

const (
	wMarker = 2
	wState  = 12
	wLast   = 14
)

const (
	cycleColWidth  = 6
	oracleColWidth = 12
	budgetColWidth = 13
	niColWidth     = 5
)

// NAME and DOING are the two flexible text columns: they SHARE whatever width
// is left after the fixed columns, each growing from a floor toward a cap.
//   - floor: below it the text is a noise-y fragment (a 10-char DOING snippet
//     is closer to noise than signal), so the column is hidden entirely rather
//     than shown squeezed — the same all-or-nothing a fixed column does at its
//     threshold, but here the trigger is "the leftover can't seat both floors".
//   - cap: so neither column runs away on a very wide terminal (the mockup
//     keeps the table compact); spare beyond both caps goes to NOTE.
//
// Keeping NAME+DOING inside the leftover budget is exactly what guarantees the
// row never exceeds the terminal width — and so never soft-wraps onto a second
// physical line — whenever DOING is shown. (An earlier fixed-width DOING broke
// this: a 30-wide column that "survived" down to a 55-col threshold couldn't
// actually fit until ~130 cols, so mainstream widths like 120 wrapped.)
const (
	nameFloorWidth  = 10
	nameCapWidth    = 28
	doingFloorWidth = 16
	doingCapWidth   = 30
)

// minWidthForNote/NI/Oracle/Budget/Cycle: below these terminal widths the
// corresponding fixed column is dropped entirely (not just truncated), in this
// degradation order as width shrinks: NOTE first (least essential — the state
// label already hints at "why"), then N/I, then ORACLE, then BUDGET, then
// CYCLE (most essential — kept the longest). Each threshold is strictly less
// than the last so that order actually holds. NAME and DOING are NOT in this
// list: they don't hard-drop at a fixed width, they flex to share the leftover
// budget (see the nameFloorWidth/doingFloorWidth block and columnWidths), so
// DOING fades by shrinking-then-hiding rather than snapping off at a threshold.
const (
	minWidthForNote   = 70
	minWidthForNI     = 68
	minWidthForOracle = 64
	minWidthForBudget = 60
	minWidthForCycle  = 50
)

// columnWidths sizes NAME/DOING/CYCLE/ORACLE/BUDGET/N-I/NOTE from the terminal
// width. CYCLE/ORACLE/BUDGET/N-I/NOTE are fixed-width, dropped below their
// thresholds (see minWidthForNote/NI/Oracle/Budget/Cycle). NAME and DOING are
// the two flexible text columns: they SHARE whatever width is left after the
// fixed columns (see flexNameDoing), each bounded by a floor and a cap, with
// any width beyond both caps handed to NOTE (as it was before DOING existed).
//
// Because NAME+DOING are always kept within that leftover budget, the row's
// total width never exceeds the terminal width whenever DOING is shown — so it
// can't soft-wrap onto a second line (there is no viewport/clip in this TUI;
// padToWidth only pads, never truncates the whole row). When the leftover
// can't seat both floors, DOING is hidden (wDoing == 0) and NAME flexes alone,
// exactly the pre-DOING behaviour; only then, at genuinely narrow widths, can
// the fixed columns alone still overflow — the rough edge the table always had
// below ~100 cols, which DOING neither introduces nor worsens.
func columnWidths(width int) (wName, wDoing, wCycle, wOracle, wBudget, wNI, wNote int) {
	if width >= minWidthForCycle {
		wCycle = cycleColWidth
	}
	if width >= minWidthForOracle {
		wOracle = oracleColWidth
	}
	if width >= minWidthForBudget {
		wBudget = budgetColWidth
	}
	if width >= minWidthForNI {
		wNI = niColWidth
	}
	if width >= minWidthForNote {
		wNote = 24
	}

	const gaps = 4 // the leading "  " indent plus cell boundaries need slack.
	fixed := wMarker + wState + wLast + wCycle + wOracle + wBudget + wNI + wNote + gaps
	remaining := width - fixed // the width budget NAME and DOING share

	if remaining >= nameFloorWidth+doingFloorWidth {
		var spare int
		wName, wDoing, spare = flexNameDoing(remaining)
		if spare > 0 && wNote > 0 {
			wNote += spare
		}
		return wName, wDoing, wCycle, wOracle, wBudget, wNI, wNote
	}

	// Not enough room for both floors: DOING steps aside (wDoing stays 0) and
	// NAME flexes alone. remaining < nameFloorWidth+doingFloorWidth < nameCap
	// here, so NAME only ever hits its floor in this branch, never its cap.
	wName = remaining
	if wName < nameFloorWidth {
		wName = nameFloorWidth
	}
	return wName, wDoing, wCycle, wOracle, wBudget, wNI, wNote
}

// flexNameDoing splits the leftover width budget shared by NAME and DOING,
// given it's already known both floors fit (remaining >= nameFloorWidth +
// doingFloorWidth). Each column grows from its floor toward its cap in
// proportion to its headroom; whatever is left once both are capped is
// returned as spare for the caller to hand to NOTE. The invariant callers rely
// on for the no-overflow guarantee: wName + wDoing + spare == remaining, so the
// two flex columns plus NOTE's bonus never exceed the budget they were given.
func flexNameDoing(remaining int) (wName, wDoing, spare int) {
	pool := remaining - nameFloorWidth - doingFloorWidth
	nameHeadroom := nameCapWidth - nameFloorWidth
	doingHeadroom := doingCapWidth - doingFloorWidth
	nameGain := pool * nameHeadroom / (nameHeadroom + doingHeadroom)
	if nameGain > nameHeadroom {
		nameGain = nameHeadroom
	}
	doingGain := pool - nameGain
	if doingGain > doingHeadroom {
		doingGain = doingHeadroom
	}
	return nameFloorWidth + nameGain, doingFloorWidth + doingGain, pool - nameGain - doingGain
}

func renderTableHeader(wName, wDoing, wCycle, wOracle, wBudget, wNI, wNote int) string {
	cells := []string{
		stHeader.Width(wMarker).Render(""),
		stHeader.Width(wName).Render("NAME"),
	}
	if wDoing > 0 {
		cells = append(cells, stHeader.Width(wDoing).Render("DOING"))
	}
	cells = append(cells, stHeader.Width(wState).Render("STATE"))
	if wCycle > 0 {
		cells = append(cells, stHeader.Width(wCycle).Render("CYCLE"))
	}
	if wOracle > 0 {
		cells = append(cells, stHeader.Width(wOracle).Render("ORACLE"))
	}
	if wBudget > 0 {
		cells = append(cells, stHeader.Width(wBudget).Render("BUDGET"))
	}
	if wNI > 0 {
		cells = append(cells, stHeader.Width(wNI).Render("N/I"))
	}
	cells = append(cells, stHeader.Width(wLast).Render("LAST"))
	if wNote > 0 {
		cells = append(cells, stHeader.Width(wNote).Render("NOTE"))
	}
	return "  " + lipgloss.JoinHorizontal(lipgloss.Top, cells...)
}

// duplicateLabels reports, for each project label shared by 2+ loops in the
// current fleet, whether renderRow must disambiguate it with a session-id
// suffix (many loops sharing "sessions"/"IdeaProjects" are otherwise
// indistinguishable in the table).
func duplicateLabels(loops []domain.Loop) map[string]bool {
	counts := make(map[string]int, len(loops))
	for _, l := range loops {
		counts[l.Project]++
	}
	dup := make(map[string]bool, len(counts))
	for label, n := range counts {
		dup[label] = n > 1
	}
	return dup
}

func renderRow(l domain.Loop, sel bool, dup bool, wName, wDoing, wCycle, wOracle, wBudget, wNI, wNote, totalWidth int) string {
	marker := " "
	markerStyle := lipgloss.NewStyle().Foreground(cFaint)
	if sel {
		marker = "▸"
		markerStyle = lipgloss.NewStyle().Foreground(cAccent)
	}
	st := stateStyle(l)
	note, noteSt := noteForRow(l)
	label := l.Project
	if dup {
		label += "·" + shortID(l.SessionID)
	}
	cells := []string{
		markerStyle.Width(wMarker).Render(marker),
		stInk.Width(wName).Render(trunc(label, wName-1)),
	}
	if wDoing > 0 {
		// dim (stDim): DOING is background context, kept visually secondary to
		// NOTE's warning colors (cRed/cAmber).
		cells = append(cells, stDim.Width(wDoing).Render(trunc(doingForRow(l), wDoing-1)))
	}
	cells = append(cells, st.Width(wState).Render(stateLabel(l)))
	if wCycle > 0 {
		cells = append(cells, stDim.Width(wCycle).Render(cycleLabel(l)))
	}
	if wOracle > 0 {
		cells = append(cells, oracleStyle(l).Width(wOracle).Render(trunc(oracleLabel(l), wOracle-1)))
	}
	if wBudget > 0 {
		bar := budgetBar(l.BudgetFrac(), 7)
		cells = append(cells, budgetStyle(l).Width(wBudget).Render(trunc(bar, wBudget-1)))
	}
	if wNI > 0 {
		cells = append(cells, noImproveStyle(l).Width(wNI).Render(noImproveLabel(l)))
	}
	cells = append(cells, stDim.Width(wLast).Render(rel(time.Since(l.LastActivity))))
	if wNote > 0 {
		cells = append(cells, noteSt.Width(wNote).Render(trunc(note, wNote-1)))
	}
	row := "  " + lipgloss.JoinHorizontal(lipgloss.Top, cells...)
	if sel {
		// pad to the full table width first so the selection background
		// spans the whole row, like the mockup's .tr.sel.
		row = stSelRow.Render(padToWidth(row, totalWidth))
	}
	return row
}

// noteForRow decides the NOTE column's text and color. A governor-set
// l.Note (internal/engine.Check via the scanner's applyGovernor) always
// wins when set — it's either an "over budget"/"max cycles reached"
// escalation (amber, State otherwise unchanged) or a "stopped: no
// improvement" note paired with StateFailed (red, matching FAILED's own
// state color) — over the older stall/drift-derived text, which falls back
// to matching the row's overall state color (st) as before.
func noteForRow(l domain.Loop) (string, lipgloss.Style) {
	if l.Note != "" {
		if l.State == domain.StateFailed {
			return l.Note, lipgloss.NewStyle().Foreground(cRed)
		}
		return l.Note, lipgloss.NewStyle().Foreground(cAmber)
	}
	switch {
	case l.Stall != domain.StallNone:
		return "⚠ " + string(l.Stall), stateStyle(l)
	case l.State == domain.StateDrift && l.Last != nil:
		return "✗ " + l.Last.Reason, stateStyle(l)
	}
	return "", stateStyle(l)
}

// doingForRow decides the DOING column's text — a background/context column
// answering "what is this loop actually working on?", distinct from NOTE's
// alert channel (which stays untouched). A goal-bound loop (spawned via the
// tui's "n" key) carries the human-written Goal.Text, the ideal answer; loops
// missionctl merely observes (the majority — plain claude sessions) fall back
// to LastText, the last assistant message's tail, already single-line and
// 120-char-capped by internal/claude.summarizeTailText (the same text feeding
// the detail pane's TAIL row). "" when a loop has neither yet (e.g. a
// just-started unbound loop with no assistant output). Unlike noteForRow the
// style is invariant (always dim, applied by the caller), so only the text is
// returned. The caller truncates it to the column width.
func doingForRow(l domain.Loop) string {
	if l.Goal.Text != "" {
		return l.Goal.Text
	}
	return l.LastText
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

// ── detail pane ──────────────────────────────────────────────

func renderDetail(l domain.Loop, width int) string {
	// leave room for the ~8-col key + its gap before truncating long values
	// (paths) so nothing overflows the terminal width.
	valueWidth := width - 10
	if valueWidth < 10 {
		valueWidth = 10
	}

	var d strings.Builder
	d.WriteString(stTitle.Render("▸ " + l.Project))
	d.WriteString("  " + stFaint.Render(l.SessionID))
	d.WriteString("\n")
	d.WriteString(detailRow("STATE", stateStyle(l).Render(stateLabel(l))))
	d.WriteString(detailRow("CYCLE", stInk.Render(cycleLabel(l))))
	if l.Goal.Text != "" {
		d.WriteString(detailRow("GOAL", stInk.Render(trunc(l.Goal.Text, valueWidth))))
		d.WriteString(detailRow("ORACLE", renderOracleDetail(l, valueWidth)))
		// RUBRIC: the wizard's "oracle:" contract field (how completion is
		// verified) — distinct from the ORACLE row above, which shows the
		// oracle's rendered VERDICT, not its rubric. Abbreviated from
		// "ORACLE-RUBRIC" to fit the pane's fixed ~8-col key width
		// (detailRow) without breaking column alignment. Challenger is
		// intentionally not shown yet (no challenger phase exists to
		// surface progress against — see DESIGN.md).
		if l.Goal.Oracle != "" {
			d.WriteString(detailRow("RUBRIC", stDim.Render(trunc(l.Goal.Oracle, valueWidth))))
		}
	}
	d.WriteString(detailRow("BUDGET", budgetStyle(l).Render(fmt.Sprintf("%s / %s (%d%%)",
		prettyTokens(l.TokensSpent), prettyTokens(l.Goal.BudgetTokens), int(math.Round(l.BudgetFrac()*100))))))
	if l.Goal.Text != "" {
		d.WriteString(detailRow("N/I", noImproveStyle(l).Render(noImproveLabel(l))))
	}
	d.WriteString(detailRow("LAST", stInk.Render(rel(time.Since(l.LastActivity))+"  ("+l.LastActivity.Format("15:04:05")+")")))
	d.WriteString(detailRow("CWD", stDim.Render(trunc(l.Cwd, valueWidth))))
	d.WriteString(detailRow("LOG", stDim.Render(trunc(l.Path, valueWidth))))
	if l.LastText != "" {
		d.WriteString(detailRow("TAIL", stDim.Render(trunc(l.LastText, valueWidth))))
	}

	switch l.State {
	case domain.StateStalled:
		d.WriteString(renderResumeCallout(l, width))
	case domain.StateGate:
		d.WriteString(renderGateCallout(l, width))
	case domain.StateDrift:
		d.WriteString(renderDriftCallout(l, width))
	}
	return stDetail.Width(width).Render(strings.TrimRight(d.String(), "\n"))
}

// renderOracleDetail is the ORACLE row's value: icon + the verdict's actual
// reason (not just the short table-cell label), colored by outcome. "—" if
// never judged yet.
func renderOracleDetail(l domain.Loop, valueWidth int) string {
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
	return style.Render(icon + " " + trunc(l.Last.Reason, valueWidth-2))
}

// detailRow is one KEY  value line in the mockup's key-value grid (faint
// uppercase key, ~8 cols wide).
func detailRow(key, value string) string {
	return stFaint.Width(8).Render(key) + value + "\n"
}

// renderResumeCallout is the mockup's amber gate-line, repurposed for a
// stall: "RESUME ▸ <why>   r re-send prompt   manual: claude --resume <id>".
// A 429 gets the red accent instead of amber (the turn didn't complete, it
// was rejected — a sharper signal than a generic stall). A gone process
// gets red too, but with "restart" wording instead of "resume" — there's no
// claude process left to re-send a prompt into (see resumeCmd's guard).
func renderResumeCallout(l domain.Loop, width int) string {
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
		action = stDim.Render("restart: " + manualResumeHint(l.SessionID))
	}

	line := lipgloss.NewStyle().Foreground(accent).Bold(true).Render(label) +
		" " + stInk.Render(string(l.Stall)) +
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
// rejected: "DRIFT ▸ <reason>   r re-drive   k kill". "r" re-drives the
// loop by re-sending its LAST USER PROMPT (resumeCmd already allows
// StateDrift, same send path as a stalled loop's resume).
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
		"   " + stKeyChipRed.Render("r") + stDim.Render(" re-drive") +
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
	if m.spawnStep == wizardWhere {
		return lipgloss.NewStyle().Foreground(cAccent).Bold(true).Render("NEW LOOP ▸ " + m.whereStepLabel())
	}
	return lipgloss.NewStyle().Foreground(cAccent).Bold(true).Render("NEW LOOP ▸ "+wizardStepLabel(m.spawnStep)+" ") + m.input.View()
}

// wizardStepLabel is each free-text wizard step's prompt label —
// max_iteration's carries the default inline ("[12]"), since there's no
// separate placeholder shown on the input (see newWizardInput). Does not
// cover wizardWhere (see whereStepLabel).
func wizardStepLabel(step wizardStep) string {
	switch step {
	case wizardGoal:
		return "goal:"
	case wizardDoneWhen:
		return "complete condition:"
	case wizardOracle:
		return "oracle:"
	case wizardChallenger:
		return "challenger:"
	case wizardMaxCycles:
		return fmt.Sprintf("max_iteration [%d]:", registry.DefaultMaxCycles)
	default:
		return ""
	}
}

// whereStepLabel builds the wizard's final "where to spawn" prompt, with a
// busy-directory nudge appended when the target directory already hosts
// >=1 fleet loop (independent of — and a stronger UX nudge than —
// spawnHostsClaudeRepo, which only gates the w/enter default).
func (m Model) whereStepLabel() string {
	label := "where? [w] new worktree · [d] this dir:"
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

// renderKeybar: only keys that actually do something today.
func renderKeybar(loopCount int, width int) string {
	keys := []string{
		stKey.Render("↑↓") + stDim.Render(" select"),
		stKey.Render("/") + stDim.Render(" filter"),
		stKey.Render("↵") + stDim.Render(" attach"),
		stKey.Render("a") + stDim.Render(" approve"),
		stKey.Render("r") + stDim.Render(" resume"),
		stKey.Render("p") + stDim.Render(" stop"),
		stKey.Render("k") + stDim.Render(" kill"),
		stKey.Render("n") + stDim.Render(" new"),
		stKey.Render("o") + stDim.Render(" log"),
		stKey.Render("q") + stDim.Render(" quit"),
	}
	sep := stFaint.Render("  ·  ")
	left := strings.Join(keys, sep)
	right := stFaint.Render(fmt.Sprintf("missionctl v0.1 · %d loops · ⧗ %s", loopCount, refreshEvery))
	// Degrade instead of wrapping: drop the right-side info when the bar is
	// tight, then tighten the key separators — a wrapped keybar reads broken.
	if lipgloss.Width(left)+lipgloss.Width(right)+1 > width {
		right = ""
	}
	if lipgloss.Width(left) > width {
		left = strings.Join(keys, stFaint.Render(" · "))
	}
	return stKeybar.Width(width).Render(padBetween(left, right, width))
}

// ── layout helpers ────────────────────────────────────────────

// padBetween left-aligns left and right-aligns right within width, joined by
// spaces. If right is empty, left is returned as-is (no trailing padding).
func padBetween(left, right string, width int) string {
	if right == "" {
		return left
	}
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
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

// trunc truncates s to at most n runes — NOT bytes: multi-byte glyphs (█/░
// in the budget bar, ⚠/✗/◆ elsewhere) are 3 bytes each in UTF-8, and a
// byte-index slice can land mid-character, corrupting the output (seen as
// stray "�" replacement glyphs). Truncating replaces the last kept rune
// with an ellipsis.
func trunc(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
