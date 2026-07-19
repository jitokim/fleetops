package control

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// tmuxController drives a tmux pane via the tmux CLI.
type tmuxController struct{}

func (tmuxController) Name() string { return "tmux" }

func (tmuxController) Available() bool {
	if _, err := exec.LookPath("tmux"); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), availabilityTimeout)
	defer cancel()
	return exec.CommandContext(ctx, "tmux", "list-panes", "-a").Run() == nil
}

func (tmuxController) Locate(projectDir string) (Target, bool) {
	out, ok := tmuxListPanes()
	if !ok {
		return Target{}, false
	}
	for _, t := range parseTmuxPanes(out) {
		if encodeCwd(t.Cwd) == projectDir {
			return t, true
		}
	}
	return Target{}, false
}

// LocateClaude is like Locate, but returns only a pane whose foreground
// command names claude by isClaudeComm's rule (base name, ".exe" stripped —
// see its doc in control.go for the exact test and why it is not stricter) —
// typed/destructive actions must never land on a bare shell pane that merely
// happens to share the directory (see parseTmuxClaudePanes and
// selectClaudeTmuxPane).
func (tmuxController) LocateClaude(projectDir string) (Target, bool) {
	out, ok := tmuxListPanes()
	if !ok {
		return Target{}, false
	}
	return selectClaudeTmuxPane(parseTmuxClaudePanes(out), projectDir)
}

// selectClaudeTmuxPane picks the SOLE claude pane matching projectDir.
// Refuses (ok=false) when MORE THAN ONE claude pane matches — same "no way
// to tell which one was meant" reasoning as selectClaudeOrcaTerminal; the
// authoritative backstop behind the TUI's keypress-time fleet-ambiguity
// guard (see Controller.LocateClaude's doc). Pulled out of LocateClaude as
// its own pure function so the ambiguity behavior is unit-testable without a
// real tmux binary.
func selectClaudeTmuxPane(candidates []Target, projectDir string) (Target, bool) {
	var matches []Target
	for _, t := range candidates {
		if encodeCwd(t.Cwd) == projectDir {
			matches = append(matches, t)
		}
	}
	if len(matches) != 1 {
		return Target{}, false
	}
	return matches[0], true
}

// tmuxListPanes runs the shared list-panes probe behind both Locate and
// LocateClaude, extended with #{pane_current_command} (P0-3) so callers can
// tell a claude pane from a bare shell sharing the same directory.
func tmuxListPanes() (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), availabilityTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux", "list-panes", "-a", "-F", "#{pane_id}\t#{pane_current_path}\t#{pane_current_command}").Output()
	if err != nil {
		return "", false
	}
	return string(out), true
}

// LocateByTTY finds the pane whose controlling tty matches tty (as recorded
// by the session registry, e.g. "ttys012") AND whose foreground command
// names claude by isClaudeComm's rule (see its doc in control.go) — the ADR
// Phase 2 tty-dispatch path (see
// ResolveActuationTarget). tty is session-unique (unlike cwd), so this
// deliberately does NOT apply an ambiguity refusal the way
// selectClaudeTmuxPane does for the cwd path: at most one live pane can
// have a given controlling tty at any moment.
func (tmuxController) LocateByTTY(tty string) (Target, bool) {
	out, ok := tmuxListPanesTTY()
	if !ok {
		return Target{}, false
	}
	return selectTTYPane(parseTmuxTTYPaneLines(out), tty)
}

// selectTTYPane is LocateByTTY's pure selection core, pulled out so the
// tty-matching logic is directly unit-testable against a fixture without a
// real tmux binary (same pattern as selectClaudeTmuxPane for the cwd path).
// Matches the pane whose tty normalizes equal to tty AND whose foreground
// command names claude by isClaudeComm's rule (see its doc in control.go).
func selectTTYPane(lines []tmuxTTYPaneLine, tty string) (Target, bool) {
	want := normalizeTTY(tty)
	for _, l := range lines {
		if isClaudeComm(l.Command) && normalizeTTY(l.TTY) == want {
			return Target{Backend: "tmux", ID: l.ID}, true
		}
	}
	return Target{}, false
}

// tmuxListPanesTTY is LocateByTTY's own list-panes probe — a separate
// -F format from tmuxListPanes (which is cwd-oriented and doesn't need
// pane_tty at all).
func tmuxListPanesTTY() (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), availabilityTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux", "list-panes", "-a", "-F", "#{pane_tty}\t#{pane_id}\t#{pane_current_command}").Output()
	if err != nil {
		return "", false
	}
	return string(out), true
}

// tmuxTTYPaneLine is one parsed row of tmuxListPanesTTY's output.
type tmuxTTYPaneLine struct {
	TTY     string
	ID      string
	Command string
}

// parseTmuxTTYPaneLines parses `tmux list-panes -a -F
// '#{pane_tty}\t#{pane_id}\t#{pane_current_command}'` output, one pane per
// line. A line that doesn't split into exactly 3 tab-separated fields is
// skipped, not an error — same tolerance as parseTmuxPaneLines.
func parseTmuxTTYPaneLines(out string) []tmuxTTYPaneLine {
	var lines []tmuxTTYPaneLine
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		lines = append(lines, tmuxTTYPaneLine{TTY: parts[0], ID: parts[1], Command: parts[2]})
	}
	return lines
}

// normalizeTTY strips a "/dev/" prefix so a session registry entry's tty
// ("ttys012", from internal/sessions' `ps -o tty=`) and tmux's own
// "#{pane_tty}" (which reports the full device path, "/dev/ttys012")
// compare equal. Applied symmetrically to both operands in LocateByTTY, so
// either form on either side matches.
func normalizeTTY(tty string) string {
	return strings.TrimPrefix(tty, "/dev/")
}

func (tmuxController) Resume(t Target, prompt string) error {
	for _, argv := range tmuxResumeCmds(t.ID, prompt) {
		if err := runWithTimeout(argv); err != nil {
			return err
		}
	}
	return nil
}

// tmuxResumeCmds builds the argv sequence that re-sends prompt to a pane and
// submits it: send-keys the literal text, then send-keys Enter separately
// (tmux has no single-call "type + submit").
func tmuxResumeCmds(paneID, prompt string) [][]string {
	return [][]string{
		{"tmux", "send-keys", "-t", paneID, "-l", "--", prompt},
		{"tmux", "send-keys", "-t", paneID, "Enter"},
	}
}

// Approve accepts claude's default highlighted option at a gate by sending
// a bare Enter — no text typed, just the key.
func (tmuxController) Approve(t Target) error {
	return runWithTimeout(tmuxApproveCmd(t.ID))
}

// tmuxApproveCmd builds the argv for a bare Enter keypress into a pane.
func tmuxApproveCmd(paneID string) []string {
	return []string{"tmux", "send-keys", "-t", paneID, "Enter"}
}

func (tmuxController) Focus(t Target) error {
	for _, argv := range tmuxFocusCmds(t.ID) {
		if err := runWithTimeout(argv); err != nil {
			return err
		}
	}
	return nil
}

// tmuxFocusCmds builds the argv sequence that brings a pane to the front:
// select-pane makes it the active pane in its window, switch-client moves
// the attached client to that window. switch-client fails harmlessly when
// run from outside tmux (no attached client) — the TUI surfaces the error.
func tmuxFocusCmds(paneID string) [][]string {
	return [][]string{
		{"tmux", "select-pane", "-t", paneID},
		{"tmux", "switch-client", "-t", paneID},
	}
}

// spawnBootWait is a pragmatic fixed pause for claude's TUI to boot inside
// the new pane before typing the goal into it — tmux has no equivalent of
// orca's "wait --for tui-idle", so this is a flat sleep rather than a poll.
const spawnBootWait = 8 * time.Second

// Spawn opens a new tmux window running claude in cwd, waits for it to boot
// (pragmatic fixed delay, see spawnBootWait), then sends the goal + Enter.
func (tmuxController) Spawn(cwd, goal string) error {
	argv := tmuxNewWindowCmd(cwd, spawnCommandFn())
	out, err := exec.Command(argv[0], argv[1:]...).Output()
	if err != nil {
		return err
	}
	paneID := strings.TrimSpace(string(out))
	if paneID == "" {
		return fmt.Errorf("tmux new-window: empty pane id")
	}

	time.Sleep(spawnBootWait)

	for _, argv := range tmuxResumeCmds(paneID, goal) {
		if err := exec.Command(argv[0], argv[1:]...).Run(); err != nil {
			return err
		}
	}
	return nil
}

// tmuxNewWindowCmd builds the argv that opens a new tmux window running
// spawnArgv in cwd, printing just the new pane's id to stdout (-P -F) so Spawn
// can target it directly.
//
// spawnArgv is the configured spawn command (internal/settings, default
// ["claude"]) and is appended as SEPARATE argv elements, never joined into a
// string. tmux runs a multi-argument command directly with execvp instead of
// handing it to a shell, so there is no word splitting or quoting layer — and
// the pane's foreground process stays literally "claude", which is what
// LocateClaude matches `#{pane_current_command}` against (see isClaudeComm).
// Joining the argv into one string would have made every configured loop
// invisible to actuation, since the pane would report a shell instead.
//
// tmux stops parsing its own options at the first non-option argument (the
// command name), so the command's own flags — "--agent", "--dangerously-skip-
// permissions" — are passed to it rather than interpreted by tmux.
//
// -d creates the window in the BACKGROUND (detached): tmux new-window without
// it makes the new window the client's current window, so when the fleetops
// cockpit is itself running inside that same tmux client, spawning a loop
// would yank the screen off the cockpit and into the freshly-created claude
// session — the "creating a loop auto-jumps into attach" hijack. -d keeps the
// cockpit put; the loop still spawns and its goal is still sent (send-keys
// below targets the captured pane id, which -P -F still reports for a detached
// window — focus is irrelevant to it). Take-over (OpenTerminal) deliberately
// does NOT pass -d: there the human explicitly asked to jump into the session.
func tmuxNewWindowCmd(cwd string, spawnArgv []string) []string {
	return append([]string{"tmux", "new-window", "-d", "-c", cwd, "-P", "-F", "#{pane_id}"}, spawnArgv...)
}

// OpenTerminal implements control.TerminalOpener: opens a new tmux window in
// cwd running command — LoopEngine's take-over attach. Reuses
// tmuxNewWindowCmd's exact "-c cwd" shape, generalized from the hardcoded "claude" Spawn always runs
// to an arbitrary command (take-over's "claude --resume <id>"); command is
// already the complete shell invocation, and tmux interprets a single
// trailing argv element as the shell-command to run in the new window's
// pane (same convention `tmux new-window "claude --resume <id>"` documents)
// — no -P/-F pane-id capture needed here, unlike Spawn, since there is no
// follow-up send.
func (tmuxController) OpenTerminal(cwd, command string) error {
	argv := tmuxOpenTerminalCmd(cwd, command)
	return exec.Command(argv[0], argv[1:]...).Run()
}

// tmuxOpenTerminalCmd builds the argv for OpenTerminal — pulled out as its
// own pure function (same reasoning as tmuxNewWindowCmd/tmuxResumeCmds
// above: directly unit-testable without a real tmux binary).
func tmuxOpenTerminalCmd(cwd, command string) []string {
	return []string{"tmux", "new-window", "-c", cwd, command}
}

// Interrupt stops the current turn without killing claude — a bare Esc.
func (tmuxController) Interrupt(t Target) error {
	return runWithTimeout(tmuxInterruptCmd(t.ID))
}

// tmuxInterruptCmd builds the argv for an Escape keypress into a pane.
func tmuxInterruptCmd(paneID string) []string {
	return []string{"tmux", "send-keys", "-t", paneID, "Escape"}
}

// tmuxPaneLine is one parsed row of tmuxListPanes' 3-field output — the
// shared core behind parseTmuxPanes (permissive, feeds Locate/Focus) and
// parseTmuxClaudePanes (claude-only, feeds LocateClaude).
type tmuxPaneLine struct {
	ID      string
	Cwd     string
	Command string
}

// parseTmuxPaneLines parses `tmux list-panes -a -F
// '#{pane_id}\t#{pane_current_path}\t#{pane_current_command}'` output, one
// pane per line. A line that doesn't split into exactly 3 tab-separated
// fields is skipped, not an error.
func parseTmuxPaneLines(out string) []tmuxPaneLine {
	var lines []tmuxPaneLine
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		lines = append(lines, tmuxPaneLine{ID: parts[0], Cwd: parts[1], Command: parts[2]})
	}
	return lines
}

// parseTmuxPanes returns every pane regardless of its foreground command —
// used by Locate (attach/Focus must be able to jump to a bare shell pane in
// the right directory, not just a claude pane).
func parseTmuxPanes(out string) []Target {
	var targets []Target
	for _, l := range parseTmuxPaneLines(out) {
		targets = append(targets, Target{Backend: "tmux", ID: l.ID, Cwd: l.Cwd})
	}
	return targets
}

// parseTmuxClaudePanes returns only panes whose foreground command names
// claude by isClaudeComm's rule (see its doc in control.go) — used by
// LocateClaude for typed/destructive actions, which must never land on a
// bare shell pane sharing the same directory.
func parseTmuxClaudePanes(out string) []Target {
	var targets []Target
	for _, l := range parseTmuxPaneLines(out) {
		if !isClaudeComm(l.Command) {
			continue
		}
		targets = append(targets, Target{Backend: "tmux", ID: l.ID, Cwd: l.Cwd})
	}
	return targets
}
