package control

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
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
	ctx, cancel := context.WithTimeout(context.Background(), availabilityTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "cmux", "tree", "--json").Output()
	if err != nil {
		return Target{}, false
	}
	for _, t := range parseCmuxTree(out) {
		if encodeCwd(t.Cwd) == projectDir {
			return t, true
		}
	}
	return Target{}, false
}

// LocateClaude always returns not-found. cmux's `tree --json` shape is
// unverified (see parseCmuxTree's TODO) and carries no per-surface
// running-command field, so there is no way to confirm a surface is
// actually running claude (vs. a bare shell) — degrading to Locate's
// permissive cwd-only match here would reintroduce exactly the
// wrong-terminal actuation hazard LocateClaude exists to prevent. Typed
// actions on cmux fall back to the TUI's manual resume hint instead (see
// DESIGN.md's graceful-degrade path).
func (cmuxController) LocateClaude(projectDir string) (Target, bool) {
	return Target{}, false
}

func (cmuxController) Resume(t Target, prompt string) error {
	return runWithTimeout(cmuxResumeCmd(t.ID, prompt))
}

// cmuxResumeCmd builds the argv that re-sends prompt to a surface and submits
// it in one call ("\n" sends Enter): cmux send --surface <ref> -- "<prompt>\n".
func cmuxResumeCmd(surfaceRef, prompt string) []string {
	return []string{"cmux", "send", "--surface", surfaceRef, "--", prompt + "\n"}
}

// Approve accepts claude's default highlighted option at a gate by sending
// a bare Enter key (distinct from Resume's `send`, which types literal
// text) targeted at the surface.
//
// TODO: verify cmux's send-key subcommand shape on a machine with the cmux
// CLI — unverified, same caveat as parseCmuxTree.
func (cmuxController) Approve(t Target) error {
	return runWithTimeout(cmuxApproveCmd(t.ID))
}

// cmuxApproveCmd builds the argv for a bare Enter keypress into a surface.
func cmuxApproveCmd(surfaceRef string) []string {
	return []string{"cmux", "send-key", "--surface", surfaceRef, "enter"}
}

func (cmuxController) Focus(t Target) error {
	return runWithTimeout(cmuxFocusCmd(t.ID))
}

// cmuxFocusCmd builds the argv that brings a cmux surface to the front:
// focus-panel is the contract's compatibility alias over surface focus.
func cmuxFocusCmd(surfaceRef string) []string {
	return []string{"cmux", "focus-panel", "--panel", surfaceRef}
}

// Spawn is not supported on cmux yet — creating a brand new surface running
// claude hasn't been verified against the real cmux CLI (unlike the other
// actions here, which at least have a plausible/partially-verified
// contract). Fail explicitly rather than guess at a create-surface command.
func (cmuxController) Spawn(cwd, goal string) error {
	return fmt.Errorf("spawn not supported on cmux yet")
}

// Interrupt stops the current turn without killing claude — a bare Escape.
//
// TODO: verify cmux's send-key escape convention on a machine with the cmux
// CLI — unverified, same caveat as parseCmuxTree/Approve.
func (cmuxController) Interrupt(t Target) error {
	return runWithTimeout(cmuxInterruptCmd(t.ID))
}

// cmuxInterruptCmd builds the argv for an Escape keypress into a surface.
func cmuxInterruptCmd(surfaceRef string) []string {
	return []string{"cmux", "send-key", "--surface", surfaceRef, "escape"}
}

// parseCmuxTree tolerantly walks `cmux tree --json` output, collecting every
// node that looks like a surface (a surface-id-like key) paired with a
// cwd-like key. Unknown shape → empty slice, never panics.
//
// TODO: verify cmux tree --json shape on a machine with the cmux CLI; parser
// is intentionally tolerant.
func parseCmuxTree(jsonBytes []byte) []Target {
	var root any
	if err := json.Unmarshal(jsonBytes, &root); err != nil {
		return nil
	}
	var targets []Target
	walkCmuxNode(root, &targets)
	return targets
}

func walkCmuxNode(node any, out *[]Target) {
	switch v := node.(type) {
	case map[string]any:
		if t, ok := cmuxTargetFromNode(v); ok {
			*out = append(*out, t)
		}
		for _, child := range v {
			walkCmuxNode(child, out)
		}
	case []any:
		for _, child := range v {
			walkCmuxNode(child, out)
		}
	}
}

func cmuxTargetFromNode(m map[string]any) (Target, bool) {
	id, ok := cmuxSurfaceID(m)
	if !ok {
		return Target{}, false
	}
	cwd, ok := cmuxCwd(m)
	if !ok {
		return Target{}, false
	}
	return Target{Backend: "cmux", ID: id, Cwd: cwd}, true
}

// cmuxSurfaceID looks for a surface-id-like key, preferring a "surface:<n>"
// ref; falls back to any id when a sibling "kind":"surface" confirms intent.
func cmuxSurfaceID(m map[string]any) (string, bool) {
	for _, key := range []string{"surfaceId", "surface_id", "id"} {
		if s, ok := m[key].(string); ok && strings.HasPrefix(s, "surface:") {
			return s, true
		}
	}
	if kind, _ := m["kind"].(string); kind == "surface" {
		for _, key := range []string{"surfaceId", "surface_id", "id"} {
			if s, ok := m[key].(string); ok && s != "" {
				return s, true
			}
		}
	}
	return "", false
}

func cmuxCwd(m map[string]any) (string, bool) {
	for _, key := range []string{"cwd", "workingDirectory", "working_directory"} {
		if s, ok := m[key].(string); ok && s != "" {
			return s, true
		}
	}
	return "", false
}
