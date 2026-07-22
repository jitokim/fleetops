package accountstatus

import (
	"context"
	"testing"
	"time"
)

// The probe runs inside the SessionStart hook, so a wedged child that outlives
// ctx would delay session start. WaitDelay force-closes the child's pipes soon
// after the deadline instead of blocking Wait indefinitely on a grandchild that
// inherited stdout — so it MUST be set.
func TestBuildQueryCmd_SetsWaitDelay(t *testing.T) {
	cmd := buildQueryCmd(context.Background(), "")
	if cmd.WaitDelay != 2*time.Second {
		t.Fatalf("cmd.WaitDelay = %v, want 2s — a lingering child can block Wait past the ctx deadline and stall session start", cmd.WaitDelay)
	}
}

// A non-empty configDir scopes the probe to that account via CLAUDE_CONFIG_DIR,
// layered on top of the inherited environment (never replacing it).
func TestBuildQueryCmd_NonEmptyConfigDir_SetsEnv(t *testing.T) {
	cmd := buildQueryCmd(context.Background(), "/abs/.claude-work")
	want := configDirEnv + "=/abs/.claude-work"
	found := false
	for _, e := range cmd.Env {
		if e == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("cmd.Env is missing %q — the probe would observe the wrong account", want)
	}
}

// The default account leaves the environment un-overridden (nil Env = inherit).
func TestBuildQueryCmd_EmptyConfigDir_LeavesEnvInherited(t *testing.T) {
	cmd := buildQueryCmd(context.Background(), "")
	if cmd.Env != nil {
		t.Fatalf("cmd.Env = %v, want nil (inherit) for the default account", cmd.Env)
	}
}
