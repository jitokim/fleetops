package control

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
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

// LocateClaude is like Locate, but returns only a tier-1 terminal (✳-titled,
// connected, writable) — a confirmed Claude Code surface. Typed/destructive
// actions must never fall back to tier-2/3 (a bare shell tab sharing the
// same worktreePath), which Locate's 3-tier fallback exists to hand back for
// attach — see selectClaudeOrcaTerminal.
func (orcaController) LocateClaude(projectDir string) (Target, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), availabilityTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "orca", "terminal", "list", "--json").Output()
	if err != nil {
		return Target{}, false
	}
	terminals, ok := decodeOrcaTerminals(out)
	if !ok {
		return Target{}, false
	}
	return selectClaudeOrcaTerminal(terminals, projectDir)
}

func (orcaController) Resume(t Target, prompt string) error {
	return runWithTimeout(orcaResumeCmd(t.ID, prompt))
}

// orcaResumeCmd builds the argv that re-sends prompt to an Orca terminal and
// submits it in one call: --enter submits, so no trailing "\n" is needed
// (unlike cmux's send).
func orcaResumeCmd(handle, prompt string) []string {
	return []string{"orca", "terminal", "send", "--terminal", handle, "--text", prompt, "--enter", "--json"}
}

// Approve accepts claude's default highlighted option at a gate (e.g. a
// permission prompt) by sending a bare Enter — reuses orcaResumeCmd with an
// empty prompt, since an empty --text plus --enter is exactly that.
func (orcaController) Approve(t Target) error {
	return runWithTimeout(orcaResumeCmd(t.ID, ""))
}

func (orcaController) Focus(t Target) error {
	return runWithTimeout(orcaFocusCmd(t.ID))
}

// orcaFocusCmd builds the argv that brings an Orca terminal tab to the
// front: "terminal switch" (alias "terminal focus") switches to a terminal
// tab in the UI.
func orcaFocusCmd(handle string) []string {
	return []string{"orca", "terminal", "switch", "--terminal", handle, "--json"}
}

// Interrupt stops the current turn without killing claude, via orca's
// verified --interrupt flag on `terminal send`.
func (orcaController) Interrupt(t Target) error {
	return runWithTimeout(orcaInterruptCmd(t.ID))
}

// orcaInterruptCmd builds the argv for orca's verified interrupt flag.
func orcaInterruptCmd(handle string) []string {
	return []string{"orca", "terminal", "send", "--terminal", handle, "--interrupt", "--json"}
}

const (
	spawnTitle           = "mctl loop" // the --title Spawn gives its created terminal, used to re-find it (see selectSpawnedOrcaTerminal)
	spawnCreateTimeout   = 5 * time.Second
	spawnWaitTimeout     = 35 * time.Second // covers orca's own --timeout-ms 30000 plus process-exec overhead
	spawnBootTimeoutMs   = "30000"
	spawnLocateTimeout   = 5 * time.Second
	spawnSendTextTimeout = 5 * time.Second
)

// Spawn starts a brand new claude loop: creates an orca terminal running
// claude in cwd, waits for its TUI to finish booting, then sends the goal.
//
// create's returned handle can go STALE once orca's UI adopts the pane
// (verified live) — so after waiting, Spawn re-locates the terminal by
// worktreePath + title (spawnTitle, or a Claude-Code-prefixed title if the
// TUI already relabeled it) via a fresh `terminal list`, rather than
// trusting the handle create returned.
func (orcaController) Spawn(cwd, goal string) error {
	ctxCreate, cancelCreate := context.WithTimeout(context.Background(), spawnCreateTimeout)
	defer cancelCreate()
	createOut, err := exec.CommandContext(ctxCreate, "orca", "terminal", "create",
		"--worktree", "path:"+cwd, "--command", "claude", "--title", spawnTitle, "--json").Output()
	if err != nil {
		return fmt.Errorf("orca terminal create: %w", err)
	}
	handle, ok := parseOrcaCreateHandle(createOut)
	if !ok {
		return fmt.Errorf("orca terminal create: could not parse a terminal handle from the output")
	}

	ctxWait, cancelWait := context.WithTimeout(context.Background(), spawnWaitTimeout)
	defer cancelWait()
	// best-effort: even if the wait itself errors or times out, still try
	// to locate + send below — the terminal may already be usable.
	_ = exec.CommandContext(ctxWait, "orca", "terminal", "wait",
		"--terminal", handle, "--for", "tui-idle", "--timeout-ms", spawnBootTimeoutMs, "--json").Run()

	ctxLocate, cancelLocate := context.WithTimeout(context.Background(), spawnLocateTimeout)
	defer cancelLocate()
	listOut, err := exec.CommandContext(ctxLocate, "orca", "terminal", "list", "--json").Output()
	if err != nil {
		return fmt.Errorf("orca terminal list: %w", err)
	}
	target := Target{Backend: "orca", ID: handle, Cwd: cwd} // fallback: the create handle, in case re-locate misses
	if terminals, ok := decodeOrcaTerminals(listOut); ok {
		if t, ok := selectSpawnedOrcaTerminal(terminals, cwd); ok {
			target = t
		}
	}

	argv := orcaResumeCmd(target.ID, goal)
	ctxSend, cancelSend := context.WithTimeout(context.Background(), spawnSendTextTimeout)
	defer cancelSend()
	return exec.CommandContext(ctxSend, argv[0], argv[1:]...).Run()
}

// orcaCreateEnvelope is `orca terminal create --json`'s response shape —
// same RPC envelope convention as `terminal list`.
type orcaCreateEnvelope struct {
	OK     *bool `json:"ok"`
	Result *struct {
		Terminal struct {
			Handle string `json:"handle"`
		} `json:"terminal"`
	} `json:"result"`
}

// parseOrcaCreateHandle extracts result.terminal.handle from `orca terminal
// create --json`'s output. ok=false on any decode failure, an explicit
// {"ok":false}, or a missing/empty handle.
func parseOrcaCreateHandle(jsonBytes []byte) (string, bool) {
	var envelope orcaCreateEnvelope
	if err := json.Unmarshal(jsonBytes, &envelope); err != nil {
		return "", false
	}
	if envelope.OK != nil && !*envelope.OK {
		return "", false
	}
	if envelope.Result == nil || envelope.Result.Terminal.Handle == "" {
		return "", false
	}
	return envelope.Result.Terminal.Handle, true
}

// selectSpawnedOrcaTerminal finds the freshest (highest lastOutputAt)
// terminal at cwd whose title is spawnTitle or Claude-Code-prefixed
// (claudeTabPrefix) — i.e. the terminal Spawn just created, re-found by
// cwd+title since its create-time handle can go stale (see Spawn's doc).
func selectSpawnedOrcaTerminal(terminals []orcaTerminal, cwd string) (Target, bool) {
	var matches []orcaTerminal
	for _, t := range terminals {
		if t.WorktreePath != cwd {
			continue
		}
		if t.Title == spawnTitle || strings.HasPrefix(t.Title, claudeTabPrefix) {
			matches = append(matches, t)
		}
	}
	best, ok := bestOrcaTerminal(matches, func(orcaTerminal) bool { return true })
	if !ok {
		return Target{}, false
	}
	return Target{Backend: "orca", ID: best.Handle, Cwd: best.WorktreePath}, true
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

// selectClaudeOrcaTerminal picks the freshest CONFIRMED Claude Code terminal
// (✳-titled, connected, writable) sharing projectDir's worktreePath — tier-1
// only, no fallback to a bare shell tab even if it's the only match (see
// LocateClaude). Returns ok=false rather than degrade, unlike
// selectOrcaTerminal's 3-tier fallback for attach.
func selectClaudeOrcaTerminal(terminals []orcaTerminal, projectDir string) (Target, bool) {
	var matches []orcaTerminal
	for _, t := range terminals {
		if strings.ReplaceAll(t.WorktreePath, "/", "-") == projectDir {
			matches = append(matches, t)
		}
	}
	best, ok := bestOrcaTerminal(matches, func(t orcaTerminal) bool {
		return t.Connected && t.Writable && strings.HasPrefix(t.Title, claudeTabPrefix)
	})
	if !ok {
		return Target{}, false
	}
	return Target{Backend: "orca", ID: best.Handle, Cwd: best.WorktreePath}, true
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
