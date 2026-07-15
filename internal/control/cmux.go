package control

import (
	"context"
	"encoding/json"
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
		if strings.ReplaceAll(t.Cwd, "/", "-") == projectDir {
			return t, true
		}
	}
	return Target{}, false
}

func (cmuxController) Resume(t Target, prompt string) error {
	argv := cmuxResumeCmd(t.ID, prompt)
	return exec.Command(argv[0], argv[1:]...).Run()
}

// cmuxResumeCmd builds the argv that re-sends prompt to a surface and submits
// it in one call ("\n" sends Enter): cmux send --surface <ref> -- "<prompt>\n".
func cmuxResumeCmd(surfaceRef, prompt string) []string {
	return []string{"cmux", "send", "--surface", surfaceRef, "--", prompt + "\n"}
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
