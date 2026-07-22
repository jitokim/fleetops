package control

import (
	"reflect"
	"testing"
)

// ── spawnArgvWithConfigDir: explicit-account sibling of spawnArgvForCwd ──────

// A non-empty configDir must front the base command with the env prefix — the
// wizard's explicit choice for an unbound dir, which spawnArgvForCwd (cwd
// re-resolution) would have discarded.
func TestSpawnArgvWithConfigDir_NonEmptyGetsEnvPrefix(t *testing.T) {
	pinSpawnCommand(t, teamArgv())

	got := spawnArgvWithConfigDir("/abs/.claude-work")

	want := []string{"env", "CLAUDE_CONFIG_DIR=/abs/.claude-work",
		"claude", "--agent", "team", "--dangerously-skip-permissions"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %#v\nwant %#v", got, want)
	}
}

// The empty configDir is the load-bearing default case: an EXPLICIT default
// pick (or an absent account) must render byte-identically to the base command,
// exactly as an unbound cwd does — no env, no CLAUDE_CONFIG_DIR anywhere.
func TestSpawnArgvWithConfigDir_EmptyIsUnchanged(t *testing.T) {
	pinSpawnCommand(t, teamArgv())

	got := spawnArgvWithConfigDir("")

	want := teamArgv()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %#v\nwant %#v (empty configDir must not add any prefix)", got, want)
	}
}

// ── SpawnWithAccount: the fail-closed dispatcher ────────────────────────────

// fakeAccountSpawner records the argv it was asked to spawn under, and
// implements AccountSpawner.
type fakeAccountSpawner struct {
	spawnCwd            string
	spawnGoal           string
	withConfigDirCwd    string
	withConfigDirGoal   string
	withConfigDirConfig string
}

func (f *fakeAccountSpawner) Name() string    { return "fake-account" }
func (f *fakeAccountSpawner) Available() bool { return true }
func (f *fakeAccountSpawner) Spawn(cwd, goal string) error {
	f.spawnCwd, f.spawnGoal = cwd, goal
	return nil
}
func (f *fakeAccountSpawner) SpawnWithConfigDir(cwd, goal, configDir string) error {
	f.withConfigDirCwd, f.withConfigDirGoal, f.withConfigDirConfig = cwd, goal, configDir
	return nil
}

// fakePlainSpawner can spawn but CANNOT pin an account (no AccountSpawner).
type fakePlainSpawner struct{ spawned bool }

func (f *fakePlainSpawner) Name() string    { return "fake-plain" }
func (f *fakePlainSpawner) Available() bool { return true }
func (f *fakePlainSpawner) Spawn(cwd, goal string) error {
	f.spawned = true
	return nil
}

// configDir=="" must fall straight through to plain Spawn — the zero-config
// path, byte-for-byte unchanged, never routed through SpawnWithConfigDir.
func TestSpawnWithAccount_EmptyConfigDir_UsesPlainSpawn(t *testing.T) {
	f := &fakeAccountSpawner{}

	if err := SpawnWithAccount(f, "/repo", "do it", ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.spawnCwd != "/repo" || f.spawnGoal != "do it" {
		t.Errorf("plain Spawn not called with (cwd, goal); got (%q, %q)", f.spawnCwd, f.spawnGoal)
	}
	if f.withConfigDirConfig != "" {
		t.Errorf("SpawnWithConfigDir must NOT be reached for an empty configDir; got config %q", f.withConfigDirConfig)
	}
}

// A non-empty configDir on an AccountSpawner must pin it via SpawnWithConfigDir.
func TestSpawnWithAccount_NonEmpty_PinsViaAccountSpawner(t *testing.T) {
	f := &fakeAccountSpawner{}

	if err := SpawnWithAccount(f, "/repo", "do it", "/abs/.claude-work"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.withConfigDirCwd != "/repo" || f.withConfigDirGoal != "do it" || f.withConfigDirConfig != "/abs/.claude-work" {
		t.Errorf("SpawnWithConfigDir got (%q, %q, %q), want (/repo, do it, /abs/.claude-work)",
			f.withConfigDirCwd, f.withConfigDirGoal, f.withConfigDirConfig)
	}
	if f.spawnCwd != "" {
		t.Errorf("plain Spawn must NOT be reached when an account is pinned; got cwd %q", f.spawnCwd)
	}
}

// The fail-CLOSED guarantee: a non-empty configDir on a backend that cannot pin
// an account must ERROR, never silently spawn under the wrong account.
func TestSpawnWithAccount_NonEmpty_BackendCannotPin_FailsClosed(t *testing.T) {
	f := &fakePlainSpawner{}

	err := SpawnWithAccount(f, "/repo", "do it", "/abs/.claude-work")

	if err == nil {
		t.Fatal("expected an error — a backend that cannot pin an account must refuse, not spawn under the wrong one")
	}
	if f.spawned {
		t.Error("plain Spawn was called despite the account being unpinnable — that is the exact wrong-account spawn this must prevent")
	}
}

// ── LoginArgv: the ONE login invocation both consumers share ────────────────

// The account-scoped form: `env CLAUDE_CONFIG_DIR=<dir> claude login` as an
// argv, exactly what the `fleetops accounts` CLI runs directly.
func TestLoginArgv_NonEmpty_PrefixesConfigDir(t *testing.T) {
	got := LoginArgv("/abs/.claude-work")
	want := []string{"env", "CLAUDE_CONFIG_DIR=/abs/.claude-work", "claude", "login"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("arg %d = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

// The default account carries NO env prefix — a bare ["claude", "login"].
func TestLoginArgv_Empty_IsBareLogin(t *testing.T) {
	got := LoginArgv("")
	if len(got) != 2 || got[0] != "claude" || got[1] != "login" {
		t.Fatalf("got %v, want [claude login] (default account must not carry an env prefix)", got)
	}
}

// ── LoginTerminalCommand: the D2 login invocation ───────────────────────────

func TestLoginTerminalCommand_NonEmpty_PrefixesConfigDir(t *testing.T) {
	got := LoginTerminalCommand("/abs/.claude-work")
	want := "env CLAUDE_CONFIG_DIR=/abs/.claude-work claude login"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestLoginTerminalCommand_Empty_IsBareLogin(t *testing.T) {
	got := LoginTerminalCommand("")
	want := "claude login"
	if got != want {
		t.Errorf("got %q, want %q (default account must not carry an env prefix)", got, want)
	}
}

// A config dir containing a space must be shell-quoted as ONE word, so the
// command string a TerminalOpener runs cannot break apart.
func TestLoginTerminalCommand_SpaceInDir_IsQuoted(t *testing.T) {
	got := LoginTerminalCommand("/Users/x/Application Support/.claude")
	want := "env 'CLAUDE_CONFIG_DIR=/Users/x/Application Support/.claude' claude login"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
