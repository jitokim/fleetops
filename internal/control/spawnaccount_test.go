package control

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jitokim/fleetops/internal/settings"
)

// pinAccountConfigDir replaces the account-resolver seam for one test: cwds
// listed in bound map to their config dir; everything else resolves to no
// account (ok=false), the zero-config default.
func pinAccountConfigDir(t *testing.T, bound map[string]string) {
	t.Helper()
	original := accountConfigDirFn
	t.Cleanup(func() { accountConfigDirFn = original })
	accountConfigDirFn = func(cwd string) (string, bool) {
		dir, ok := bound[cwd]
		return dir, ok
	}
}

// workConfigDir is the config dir a bound cwd resolves to in these tests.
const workConfigDir = "/abs/.claude-work"

// ── the prefix is added for a bound cwd, and only there ─────────────────────

func TestSpawnArgvForCwd_BoundCwdGetsEnvPrefix(t *testing.T) {
	pinSpawnCommand(t, teamArgv())
	pinAccountConfigDir(t, map[string]string{"/abs/work/repo": workConfigDir})

	got := spawnArgvForCwd("/abs/work/repo")

	want := []string{"env", "CLAUDE_CONFIG_DIR=" + workConfigDir,
		"claude", "--agent", "team", "--dangerously-skip-permissions"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %#v\nwant %#v", got, want)
	}
}

// The env prefix must lead the argv — env has to run BEFORE claude for the
// variable to be in claude's environment.
func TestSpawnArgvForCwd_EnvPrefixLeadsAndAssignmentFollows(t *testing.T) {
	pinSpawnCommand(t, settings.DefaultSpawnCommand())
	pinAccountConfigDir(t, map[string]string{"/abs/work": workConfigDir})

	got := spawnArgvForCwd("/abs/work")

	if len(got) < 3 || got[0] != "env" || got[1] != "CLAUDE_CONFIG_DIR="+workConfigDir || got[2] != "claude" {
		t.Fatalf("argv = %#v, want [env, CLAUDE_CONFIG_DIR=…, claude]", got)
	}
}

// The zero-config default: an unbound cwd spawns byte-identically to the base
// command — no env, no CLAUDE_CONFIG_DIR anywhere. This is the "behaves exactly
// as today" guarantee.
func TestSpawnArgvForCwd_UnboundCwdIsUnchanged(t *testing.T) {
	pinSpawnCommand(t, teamArgv())
	pinAccountConfigDir(t, map[string]string{"/abs/work/repo": workConfigDir})

	got := spawnArgvForCwd("/somewhere/else")

	if !reflect.DeepEqual(got, teamArgv()) {
		t.Fatalf("unbound argv = %#v, want the base %#v with no prefix", got, teamArgv())
	}
	for _, arg := range got {
		if arg == "env" || strings.HasPrefix(arg, "CLAUDE_CONFIG_DIR=") {
			t.Fatalf("unbound cwd leaked an account prefix: %#v", got)
		}
	}
}

// Even with NOTHING bound at all (empty resolver), spawn is the base command —
// the same inert default proven at the accounts layer for a missing file,
// proven here at the spawn layer.
func TestSpawnArgvForCwd_NoBindingsIsInert(t *testing.T) {
	pinSpawnCommand(t, settings.DefaultSpawnCommand())
	pinAccountConfigDir(t, map[string]string{})

	if got := spawnArgvForCwd("/abs/work/repo"); !reflect.DeepEqual(got, []string{"claude"}) {
		t.Fatalf("argv = %#v, want the bare [claude] default", got)
	}
}

// ── composition through each backend's spawn-site helper ────────────────────

// orca takes a command STRING: the CLAUDE_CONFIG_DIR token must survive
// shellQuoteJoin intact and unquoted (its characters are all shell-inert).
func TestSpawnArgvForCwd_OrcaStringSurvivesQuotingIntact(t *testing.T) {
	pinSpawnCommand(t, settings.DefaultSpawnCommand())
	pinAccountConfigDir(t, map[string]string{"/abs/work": workConfigDir})

	got := shellQuoteJoin(spawnArgvForCwd("/abs/work"))

	want := "env CLAUDE_CONFIG_DIR=" + workConfigDir + " claude"
	if got != want {
		t.Fatalf("orca command string = %q, want %q", got, want)
	}
}

// A config dir containing a space must be quoted as ONE token, so the
// assignment does not split into `env CLAUDE_CONFIG_DIR=/my` `dir`.
func TestSpawnArgvForCwd_OrcaStringQuotesASpacedConfigDir(t *testing.T) {
	pinSpawnCommand(t, settings.DefaultSpawnCommand())
	pinAccountConfigDir(t, map[string]string{"/abs/work": "/my dir/.claude"})

	got := shellQuoteJoin(spawnArgvForCwd("/abs/work"))

	want := "env 'CLAUDE_CONFIG_DIR=/my dir/.claude' claude"
	if got != want {
		t.Fatalf("orca command string = %q, want the spaced token quoted whole: %q", got, want)
	}
}

// tmux takes an argv: env and the assignment must arrive as SEPARATE elements,
// and the command name tmux execs must still be "claude" so LocateClaude finds
// the pane (env execs claude in place, so the pane's foreground stays claude).
func TestSpawnArgvForCwd_TmuxArgvKeepsClaudeReachable(t *testing.T) {
	pinSpawnCommand(t, teamArgv())
	pinAccountConfigDir(t, map[string]string{"/repo": workConfigDir})

	argv := tmuxNewWindowCmd("/repo", spawnArgvForCwd("/repo"))

	want := []string{"tmux", "new-window", "-d", "-c", "/repo", "-P", "-F", "#{pane_id}",
		"env", "CLAUDE_CONFIG_DIR=" + workConfigDir,
		"claude", "--agent", "team", "--dangerously-skip-permissions"}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("tmux argv = %#v\nwant %#v", argv, want)
	}
	if !containsArg(argv, "claude") || !isClaudeComm("claude") {
		t.Fatal("claude is no longer a discrete argv element — LocateClaude could not match the pane")
	}
	for _, arg := range argv {
		if strings.Contains(arg, " ") {
			t.Fatalf("argv element %q contains a space — the command was joined into a string", arg)
		}
	}
}

// iTerm2 folds the argv into a shell launch line; the assignment must be quoted
// as one word so `exec env CLAUDE_CONFIG_DIR=… claude` is well-formed.
func TestSpawnArgvForCwd_ITerm2LaunchLineCarriesTheAssignment(t *testing.T) {
	pinSpawnCommand(t, settings.DefaultSpawnCommand())
	pinAccountConfigDir(t, map[string]string{"/abs/work": workConfigDir})

	line := iterm2LaunchLine("/abs/work", spawnArgvForCwd("/abs/work"))

	want := "cd /abs/work && exec env CLAUDE_CONFIG_DIR=" + workConfigDir + " claude || exit 1"
	if line != want {
		t.Fatalf("iterm2 launch line = %q, want %q", line, want)
	}
}

// Unbound: none of the three spawn-site helpers gain a prefix.
func TestSpawnArgvForCwd_UnboundCwdAddsNoPrefixToAnyBackend(t *testing.T) {
	pinSpawnCommand(t, settings.DefaultSpawnCommand())
	pinAccountConfigDir(t, map[string]string{"/abs/work": workConfigDir})

	unbound := "/other/repo"
	if got := tmuxNewWindowCmd(unbound, spawnArgvForCwd(unbound)); containsArg(got, "env") {
		t.Fatalf("tmux gained an env prefix for an unbound cwd: %#v", got)
	}
	if got := shellQuoteJoin(spawnArgvForCwd(unbound)); got != "claude" {
		t.Fatalf("orca gained a prefix for an unbound cwd: %q", got)
	}
	if got := iterm2LaunchLine(unbound, spawnArgvForCwd(unbound)); strings.Contains(got, "CLAUDE_CONFIG_DIR") {
		t.Fatalf("iterm2 gained a prefix for an unbound cwd: %q", got)
	}
}
