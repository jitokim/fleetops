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

func (orcaController) Focus(t Target) error {
	argv := orcaFocusCmd(t.ID)
	return exec.Command(argv[0], argv[1:]...).Run()
}

// orcaFocusCmd builds the argv that brings an Orca terminal tab to the
// front: "terminal switch" (alias "terminal focus") switches to a terminal
// tab in the UI.
func orcaFocusCmd(handle string) []string {
	return []string{"orca", "terminal", "switch", "--terminal", handle, "--json"}
}

// orcaTerminalList is the `orca terminal list --json` result payload
// (RuntimeTerminalSummary, verified against Orca's src/shared/runtime-types.ts
// + src/cli/specs/core.ts). Unlike cmux's tree (unverified shape), this
// contract is typed and verified, so a plain struct decode is enough — no
// tolerant any-walking needed. visualLayouts/totalCount/truncated are ignored
// (not relevant here).
type orcaTerminalList struct {
	Terminals []orcaTerminal `json:"terminals"`
}

// orcaListEnvelope is the RPC envelope the real orca CLI wraps the payload
// in: {"id","ok","result":{terminals...},"_meta"}. Source types also show a
// bare {"terminals":[...]} shape, so decodeOrcaTerminals tries the envelope
// first and falls back to bare.
type orcaListEnvelope struct {
	OK     *bool             `json:"ok"`
	Result *orcaTerminalList `json:"result"`
}

type orcaTerminal struct {
	Handle       string `json:"handle"`
	WorktreePath string `json:"worktreePath"`
	Title        string `json:"title"` // Claude Code prefixes its tab title "✳"
	Connected    bool   `json:"connected"`
	Writable     bool   `json:"writable"`
	LastOutputAt int64  `json:"lastOutputAt"`
}

// claudeTabPrefix is the marker Claude Code puts on a terminal tab's title.
// Sending a prompt into a bare shell tab (no prefix) would execute it as a
// shell command instead of driving the agent, so a Claude Code tab is
// strongly preferred over any other tab sharing the same worktreePath.
const claudeTabPrefix = "✳"

// parseOrcaTerminals decodes `orca terminal list --json` and returns the
// best terminal whose worktreePath encodes to projectDir (same "/"→"-"
// encoding as tmux's cwd match).
func parseOrcaTerminals(jsonBytes []byte, projectDir string) (Target, bool) {
	terminals, ok := decodeOrcaTerminals(jsonBytes)
	if !ok {
		return Target{}, false
	}
	return selectOrcaTerminal(terminals, projectDir)
}

// decodeOrcaTerminals unwraps the RPC envelope's "result.terminals", falling
// back to a bare "terminals" top-level key. An explicit {"ok":false}
// envelope is treated as "no terminals" (ok=false).
func decodeOrcaTerminals(jsonBytes []byte) ([]orcaTerminal, bool) {
	var envelope orcaListEnvelope
	if err := json.Unmarshal(jsonBytes, &envelope); err != nil {
		return nil, false
	}
	if envelope.OK != nil && !*envelope.OK {
		return nil, false
	}
	if envelope.Result != nil {
		return envelope.Result.Terminals, true
	}

	var bare orcaTerminalList
	if err := json.Unmarshal(jsonBytes, &bare); err != nil {
		return nil, false
	}
	return bare.Terminals, true
}

// selectOrcaTerminal picks among terminals sharing projectDir's worktreePath.
// Multiple tabs can share a cwd (a Claude Code tab + a bare shell tab in the
// same repo, see claudeTabPrefix) — preference order:
//  1. connected + writable + Claude Code tab (title prefix "✳")
//  2. connected + writable
//  3. any match
//
// Within a tier, the most recently active terminal (highest lastOutputAt)
// wins.
func selectOrcaTerminal(terminals []orcaTerminal, projectDir string) (Target, bool) {
	var matches []orcaTerminal
	for _, t := range terminals {
		if strings.ReplaceAll(t.WorktreePath, "/", "-") == projectDir {
			matches = append(matches, t)
		}
	}
	if len(matches) == 0 {
		return Target{}, false
	}

	tiers := []func(orcaTerminal) bool{
		func(t orcaTerminal) bool {
			return t.Connected && t.Writable && strings.HasPrefix(t.Title, claudeTabPrefix)
		},
		func(t orcaTerminal) bool { return t.Connected && t.Writable },
		func(orcaTerminal) bool { return true },
	}
	for _, pred := range tiers {
		if best, ok := bestOrcaTerminal(matches, pred); ok {
			return Target{Backend: "orca", ID: best.Handle, Cwd: best.WorktreePath}, true
		}
	}
	return Target{}, false
}

// bestOrcaTerminal returns the highest-lastOutputAt terminal matching pred,
// or ok=false if none match.
func bestOrcaTerminal(terminals []orcaTerminal, pred func(orcaTerminal) bool) (orcaTerminal, bool) {
	var best orcaTerminal
	found := false
	for _, t := range terminals {
		if !pred(t) {
			continue
		}
		if !found || t.LastOutputAt > best.LastOutputAt {
			best = t
			found = true
		}
	}
	return best, found
}
