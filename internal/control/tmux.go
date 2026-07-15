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
		if strings.ReplaceAll(t.Cwd, "/", "-") == projectDir {
			return t, true
		}
	}
	return Target{}, false
}

// LocateClaude is like Locate, but returns only a pane whose foreground
// command is literally "claude" — typed/destructive actions must never land
// on a bare shell pane that merely happens to share the directory (see
// parseTmuxClaudePanes).
func (tmuxController) LocateClaude(projectDir string) (Target, bool) {
	out, ok := tmuxListPanes()
	if !ok {
		return Target{}, false
	}
	for _, t := range parseTmuxClaudePanes(out) {
		if strings.ReplaceAll(t.Cwd, "/", "-") == projectDir {
			return t, true
		}
	}
	return Target{}, false
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
	argv := tmuxNewWindowCmd(cwd)
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
// claude in cwd, printing just the new pane's id to stdout (-P -F) so Spawn
// can target it directly.
func tmuxNewWindowCmd(cwd string) []string {
	return []string{"tmux", "new-window", "-c", cwd, "-P", "-F", "#{pane_id}", "claude"}
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

// parseTmuxClaudePanes returns only panes whose foreground command is
// literally "claude" — used by LocateClaude for typed/destructive actions,
// which must never land on a bare shell pane sharing the same directory.
func parseTmuxClaudePanes(out string) []Target {
	var targets []Target
	for _, l := range parseTmuxPaneLines(out) {
		if l.Command != "claude" {
			continue
		}
		targets = append(targets, Target{Backend: "tmux", ID: l.ID, Cwd: l.Cwd})
	}
	return targets
}
