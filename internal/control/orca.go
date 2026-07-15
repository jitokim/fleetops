package control

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
)

// orcaController drives an Orca (stablyai/orca) terminal via the orca CLI —
// the captain's own environment, so it's the preferred backend (see Resolve).
type orcaController struct{}

func (orcaController) Name() string { return "orca" }

func (orcaController) Available() bool {
	if _, err := exec.LookPath("orca"); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), availabilityTimeout)
	defer cancel()
	return exec.CommandContext(ctx, "orca", "terminal", "list", "--json").Run() == nil
}

func (orcaController) Locate(projectDir string) (Target, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), availabilityTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "orca", "terminal", "list", "--json").Output()
	if err != nil {
		return Target{}, false
	}
	return parseOrcaTerminals(out, projectDir)
}

func (orcaController) Resume(t Target, prompt string) error {
	argv := orcaResumeCmd(t.ID, prompt)
	return exec.Command(argv[0], argv[1:]...).Run()
}

// orcaResumeCmd builds the argv that re-sends prompt to an Orca terminal and
// submits it in one call: --enter submits, so no trailing "\n" is needed
// (unlike cmux's send).
func orcaResumeCmd(handle, prompt string) []string {
	return []string{"orca", "terminal", "send", "--terminal", handle, "--text", prompt, "--enter", "--json"}
}

// orcaTerminalList is the `orca terminal list --json` response shape
// (RuntimeTerminalSummary, verified against Orca's src/shared/runtime-types.ts
// + src/cli/specs/core.ts). Unlike cmux's tree (unverified shape), this
// contract is typed and verified, so a plain struct decode is enough — no
// tolerant any-walking needed. visualLayouts is ignored (not relevant here).
type orcaTerminalList struct {
	Terminals []orcaTerminal `json:"terminals"`
}

type orcaTerminal struct {
	Handle       string `json:"handle"`
	WorktreePath string `json:"worktreePath"`
	Connected    bool   `json:"connected"`
	Writable     bool   `json:"writable"`
}

// parseOrcaTerminals decodes `orca terminal list --json` and returns the
// terminal whose worktreePath encodes to projectDir (same "/"→"-" encoding
// as tmux's cwd match). Among matches, a connected+writable terminal wins;
// otherwise the first match is used.
func parseOrcaTerminals(jsonBytes []byte, projectDir string) (Target, bool) {
	var list orcaTerminalList
	if err := json.Unmarshal(jsonBytes, &list); err != nil {
		return Target{}, false
	}

	var best *orcaTerminal
	for i := range list.Terminals {
		term := &list.Terminals[i]
		if strings.ReplaceAll(term.WorktreePath, "/", "-") != projectDir {
			continue
		}
		if best == nil {
			best = term
		}
		if term.Connected && term.Writable {
			best = term
			break
		}
	}
	if best == nil {
		return Target{}, false
	}
	return Target{Backend: "orca", ID: best.Handle, Cwd: best.WorktreePath}, true
}
