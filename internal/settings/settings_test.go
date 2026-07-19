package settings

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeSettings(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	return path
}

func assertDefault(t *testing.T, got []string, why string) {
	t.Helper()
	if !reflect.DeepEqual(got, []string{"claude"}) {
		t.Fatalf("spawn command = %#v, want [claude] (%s)", got, why)
	}
}

// ── success ──────────────────────────────────────────────────────────────

func TestSpawnCommand_ReadsConfiguredArgv(t *testing.T) {
	path := writeSettings(t, `{"spawn":{"command":["claude","--agent","team","--dangerously-skip-permissions"]}}`)

	got := spawnCommandFrom(path)

	want := []string{"claude", "--agent", "team", "--dangerously-skip-permissions"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("spawn command = %#v, want %#v", got, want)
	}
}

// argv[0] stays the process name every LocateClaude implementation matches on
// (control.isClaudeComm's literal "claude"). A configuration that changed it
// would make the loop invisible to actuation — worth asserting the shape the
// documented example produces.
func TestSpawnCommand_KeepsClaudeAsArgvZero(t *testing.T) {
	path := writeSettings(t, `{"spawn":{"command":["claude","--agent","team"]}}`)

	if got := spawnCommandFrom(path)[0]; got != "claude" {
		t.Fatalf("argv[0] = %q, want claude", got)
	}
}

// Unknown keys must not invalidate the file — a future setting alongside this
// one still yields a usable spawn command.
func TestSpawnCommand_IgnoresUnknownKeys(t *testing.T) {
	path := writeSettings(t, `{"spawn":{"command":["claude","--agent","team"]},"future":{"x":1},"other":true}`)

	want := []string{"claude", "--agent", "team"}
	if got := spawnCommandFrom(path); !reflect.DeepEqual(got, want) {
		t.Fatalf("spawn command = %#v, want %#v", got, want)
	}
}

// The returned slice must not alias shared state — the Tier 2 redrive appends
// its contract flags to it.
func TestSpawnCommand_ResultIsSafeToAppendTo(t *testing.T) {
	path := writeSettings(t, `{"spawn":{"command":["claude","--agent","team"]}}`)

	first := spawnCommandFrom(path)
	_ = append(first, "--mutated") //nolint:staticcheck // deliberately exercising aliasing
	second := spawnCommandFrom(path)

	if !reflect.DeepEqual(second, []string{"claude", "--agent", "team"}) {
		t.Fatalf("a caller's append leaked into a later read: %#v", second)
	}
}

func TestDefaultSpawnCommand_IsFreshEachCall(t *testing.T) {
	first := DefaultSpawnCommand()
	first[0] = "mutated"

	if got := DefaultSpawnCommand()[0]; got != "claude" {
		t.Fatalf("DefaultSpawnCommand()[0] = %q after a caller mutated an earlier result", got)
	}
}

// ── failure / degrade ────────────────────────────────────────────────────
//
// Every one of these must degrade to ["claude"], never fail: a typo in an
// optional convenience file must not be able to stop a user spawning a loop.

func TestSpawnCommand_MissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.json")

	assertDefault(t, spawnCommandFrom(path), "a missing settings file is the common case")
}

func TestSpawnCommand_EmptyPath(t *testing.T) {
	assertDefault(t, spawnCommandFrom(""), "an undeterminable home dir means no configuration")
}

func TestSpawnCommand_MalformedJSON(t *testing.T) {
	path := writeSettings(t, `{"spawn":{"command":["claude",`)

	assertDefault(t, spawnCommandFrom(path), "truncated JSON")
}

// The single most likely human mistake with this file: writing the command as
// a shell string instead of an argv array.
func TestSpawnCommand_CommandGivenAsStringNotArray(t *testing.T) {
	path := writeSettings(t, `{"spawn":{"command":"claude --agent team"}}`)

	assertDefault(t, spawnCommandFrom(path), "a string is not an argv array")
}

func TestSpawnCommand_CommandGivenAsNumberArray(t *testing.T) {
	path := writeSettings(t, `{"spawn":{"command":[1,2,3]}}`)

	assertDefault(t, spawnCommandFrom(path), "non-string elements")
}

func TestSpawnCommand_EmptyArray(t *testing.T) {
	path := writeSettings(t, `{"spawn":{"command":[]}}`)

	assertDefault(t, spawnCommandFrom(path), "an empty argv has no command to run")
}

func TestSpawnCommand_NoSpawnKey(t *testing.T) {
	path := writeSettings(t, `{"other":{"x":1}}`)

	assertDefault(t, spawnCommandFrom(path), "no spawn setting present")
}

func TestSpawnCommand_EmptyFile(t *testing.T) {
	path := writeSettings(t, ``)

	assertDefault(t, spawnCommandFrom(path), "an empty file is not valid JSON")
}

// A blank element is a typo (a stray comma, an unfinished edit). Running the
// remainder would execute a command the human did not write, so the whole argv
// is rejected.
func TestSpawnCommand_BlankElementRejectsWholeArgv(t *testing.T) {
	path := writeSettings(t, `{"spawn":{"command":["claude","","--agent"]}}`)

	assertDefault(t, spawnCommandFrom(path), "a blank element means the argv cannot be trusted")
}

func TestSpawnCommand_WhitespaceOnlyElementRejectsWholeArgv(t *testing.T) {
	path := writeSettings(t, `{"spawn":{"command":["claude","   "]}}`)

	assertDefault(t, spawnCommandFrom(path), "a whitespace-only element is blank")
}

func TestSpawnCommand_BlankArgvZero(t *testing.T) {
	path := writeSettings(t, `{"spawn":{"command":["","--agent","team"]}}`)

	assertDefault(t, spawnCommandFrom(path), "there is no command to execute")
}

func TestSpawnCommand_NullCommand(t *testing.T) {
	path := writeSettings(t, `{"spawn":{"command":null}}`)

	assertDefault(t, spawnCommandFrom(path), "null decodes to a nil slice")
}

func TestSpawnCommand_TopLevelArray(t *testing.T) {
	path := writeSettings(t, `["claude","--agent"]`)

	assertDefault(t, spawnCommandFrom(path), "the file must be an object")
}

// A directory where the file should be — read fails, degrade rather than
// panic.
func TestSpawnCommand_PathIsADirectory(t *testing.T) {
	assertDefault(t, spawnCommandFrom(t.TempDir()), "a directory cannot be read as a file")
}

// ── argv[0] must be claude, and it is ENFORCED ───────────────────────────
//
// A launcher-style argv[0] costs the user their WHOLE multiplexer actuation
// surface: tmux's LocateByTTY (Tier 1a) and LocateClaude (Tier 1b), and cmux's
// tree walk, all filter panes through control.isClaudeComm. The loop would
// still run — it just could not be reached by a/r/i/p/k.

func TestSpawnCommand_RejectsLauncherStyleArgvZero(t *testing.T) {
	for _, argv := range [][]string{
		{"mise", "exec", "--", "claude"},
		{"env", "claude", "--agent", "team"},
		{"zsh", "-ic", "team"},
	} {
		got := validSpawnCommand(argv)
		if len(got) != 1 || got[0] != defaultSpawnCommandName {
			t.Errorf("validSpawnCommand(%v) = %v, want the default — argv[0] is not claude", argv, got)
		}
	}
}

// An absolute path to the same binary is fine: the rule is about the process
// NAME the pane reports, which is the basename.
func TestSpawnCommand_AcceptsAbsolutePathToClaude(t *testing.T) {
	argv := []string{"/opt/homebrew/bin/claude", "--agent", "team"}

	got := validSpawnCommand(argv)

	if len(got) != len(argv) || got[0] != argv[0] {
		t.Errorf("validSpawnCommand(%v) = %v, want it accepted", argv, got)
	}
}
