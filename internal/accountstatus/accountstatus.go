// Package accountstatus is the single place that knows the shape of, and how to
// run, `claude auth status --json` — the account-identity probe for a given
// CLAUDE_CONFIG_DIR.
//
// Measured live (claude 2.1.215 — see .notes/design-multi-account.md): running
// `CLAUDE_CONFIG_DIR=<dir> claude auth status --json` reports
// {loggedIn, email, orgName, subscriptionType, authMethod}, with loggedIn:false
// when <dir> holds no credentials. There is no token in that shape, so the
// subset this package reads (Status) is safe to display and to persist.
//
// It exists as its own package because TWO callers need the identical contract:
// the SessionStart hook (cmd/fleetops), which records the running session's
// account, and the TUI's "n"-wizard account picker, which shows each alias's
// login state before a spawn. One definition of the subprocess and its JSON
// shape, not two that could drift.
package accountstatus

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
)

// configDirEnv scopes which Claude account the probe observes — the same
// variable fleetops injects at spawn (internal/control's claudeConfigDirEnv).
const configDirEnv = "CLAUDE_CONFIG_DIR"

// Status is the subset of `claude auth status --json` this project reads: is
// the config dir logged in, and — if so — which account. No token, no orgId;
// only what is safe to show a human.
type Status struct {
	LoggedIn bool   `json:"loggedIn"`
	Email    string `json:"email"`
	// Plan is the account's subscription tier (the probe's "subscriptionType").
	Plan string `json:"subscriptionType"`
}

// Query runs `claude auth status --json` with CLAUDE_CONFIG_DIR set to
// configDir (configDir=="" leaves the child's environment un-overridden — the
// default account), bounded by ctx's deadline. ok=false on ANY failure — binary
// missing, non-zero exit, timeout, or unparseable output — because every caller
// treats a failed probe identically to "nothing to show", never as an error
// worth surfacing: this is best-effort display metadata, never load-bearing.
func Query(ctx context.Context, configDir string) (Status, bool) {
	cmd := exec.CommandContext(ctx, "claude", "auth", "status", "--json")
	if configDir != "" {
		cmd.Env = append(os.Environ(), configDirEnv+"="+configDir)
	}
	out, err := cmd.Output()
	if err != nil {
		return Status{}, false
	}
	var st Status
	if err := json.Unmarshal(out, &st); err != nil {
		return Status{}, false
	}
	return st, true
}
