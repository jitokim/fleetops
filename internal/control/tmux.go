package control

import (
	"context"
	"os/exec"
	"strings"
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
	ctx, cancel := context.WithTimeout(context.Background(), availabilityTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux", "list-panes", "-a", "-F", "#{pane_id}\t#{pane_current_path}").Output()
	if err != nil {
		return Target{}, false
	}
	for _, t := range parseTmuxPanes(string(out)) {
		if strings.ReplaceAll(t.Cwd, "/", "-") == projectDir {
			return t, true
		}
	}
	return Target{}, false
}

func (tmuxController) Resume(t Target, prompt string) error {
	for _, argv := range tmuxResumeCmds(t.ID, prompt) {
		if err := exec.Command(argv[0], argv[1:]...).Run(); err != nil {
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

// parseTmuxPanes parses `tmux list-panes -a -F '#{pane_id}\t#{pane_current_path}'`
// output, one pane per line.
func parseTmuxPanes(out string) []Target {
	var targets []Target
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		targets = append(targets, Target{Backend: "tmux", ID: parts[0], Cwd: parts[1]})
	}
	return targets
}
