package control

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jitokim/fleetops/internal/settings"
)

// pinSpawnCommand replaces the configured-spawn-command seam for one test.
func pinSpawnCommand(t *testing.T, argv []string) {
	t.Helper()
	original := spawnCommandFn
	t.Cleanup(func() { spawnCommandFn = original })
	spawnCommandFn = func() []string { return append([]string(nil), argv...) }
}

// teamArgv is the maintainer's real configuration — the case this whole
// feature exists for, and the one a zsh-function-name setting would have
// silently failed on.
func teamArgv() []string {
	return []string{"claude", "--agent", "team", "--dangerously-skip-permissions"}
}

// ── tmux: argv stays argv ────────────────────────────────────────────────

func TestTmuxNewWindowCmd_AppendsConfiguredArgvAsSeparateArguments(t *testing.T) {
	got := tmuxNewWindowCmd("/repo", teamArgv())

	want := []string{"tmux", "new-window", "-d", "-c", "/repo", "-P", "-F", "#{pane_id}",
		"claude", "--agent", "team", "--dangerously-skip-permissions"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %#v\nwant %#v", got, want)
	}
}

// The load-bearing property: the command reaches tmux as separate arguments,
// so tmux execs it directly and the pane's foreground process stays literally
// "claude" — which is what LocateClaude matches on. If this ever collapsed
// into one string, tmux would run it through a shell and every configured loop
// would become invisible to actuation.
func TestTmuxNewWindowCmd_ProcessNameStaysClaudeForLocateClaude(t *testing.T) {
	argv := tmuxNewWindowCmd("/repo", teamArgv())

	commandName := argv[len(argv)-len(teamArgv())]
	if commandName != "claude" {
		t.Fatalf("tmux would exec %q, want claude", commandName)
	}
	if !isClaudeComm(commandName) {
		t.Fatalf("isClaudeComm(%q) is false — LocateClaude could not find this pane", commandName)
	}
	for _, arg := range argv {
		if strings.Contains(arg, " ") {
			t.Fatalf("argv element %q contains a space — the command was joined into a string", arg)
		}
	}
}

// Unconfigured, the argv must be byte-identical to what shipped before this
// feature existed.
func TestTmuxNewWindowCmd_DefaultIsUnchangedFromBefore(t *testing.T) {
	got := tmuxNewWindowCmd("/repo", settings.DefaultSpawnCommand())

	want := []string{"tmux", "new-window", "-d", "-c", "/repo", "-P", "-F", "#{pane_id}", "claude"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %#v\nwant the pre-existing %#v", got, want)
	}
}

// -d must survive the change — spawning a loop must still not yank the
// cockpit's tmux client into the new session.
func TestTmuxNewWindowCmd_StaysDetachedWithConfiguredCommand(t *testing.T) {
	if !containsArg(tmuxNewWindowCmd("/repo", teamArgv()), "-d") {
		t.Error("Spawn's tmux new-window lost -d")
	}
}

// ── orca: the one site that needs a command STRING ───────────────────────

func TestShellQuoteJoin_PlainArgvNeedsNoQuoting(t *testing.T) {
	if got := shellQuoteJoin(teamArgv()); got != "claude --agent team --dangerously-skip-permissions" {
		t.Fatalf("shellQuoteJoin = %q", got)
	}
}

func TestShellQuoteJoin_DefaultIsBareClaude(t *testing.T) {
	if got := shellQuoteJoin(settings.DefaultSpawnCommand()); got != "claude" {
		t.Fatalf("shellQuoteJoin = %q, want claude (byte-identical to the old literal)", got)
	}
}

func TestShellQuoteJoin_QuotesArgumentsWithSpaces(t *testing.T) {
	got := shellQuoteJoin([]string{"claude", "--agent", "my team"})

	if got != `claude --agent 'my team'` {
		t.Fatalf("shellQuoteJoin = %q, want the spaced argument quoted", got)
	}
}

// Shell metacharacters must not survive into the command string as syntax —
// this value now comes from a user-editable file, so it is no longer a
// hardcoded literal.
func TestShellQuoteJoin_NeutralizesShellMetacharacters(t *testing.T) {
	for _, arg := range []string{"a; rm -rf /", "a && b", "a | b", "$(whoami)", "`whoami`", "a\nb", "a>b"} {
		got := shellQuoteJoin([]string{"claude", arg})
		if !strings.HasPrefix(got, "claude '") {
			t.Errorf("argument %q was not quoted: %s", arg, got)
		}
	}
}

// The single quote is the one character the quoting idiom has to handle
// itself, and getting it wrong is precisely how quoting becomes injection.
func TestShellQuoteJoin_EscapesEmbeddedSingleQuote(t *testing.T) {
	got := shellQuoteJoin([]string{"claude", "it's"})

	if got != `claude 'it'\''s'` {
		t.Fatalf("shellQuoteJoin = %q, want the '\\'' idiom", got)
	}
}

// A quote-closing payload must not be able to append a second command.
func TestShellQuoteJoin_QuoteBreakoutAttemptStaysOneArgument(t *testing.T) {
	got := shellQuoteJoin([]string{"claude", "'; touch /tmp/pwned; '"})

	if strings.Contains(got, "; touch /tmp/pwned; ") && !strings.Contains(got, `'\''`) {
		t.Fatalf("shellQuoteJoin = %q — the payload escaped its quotes", got)
	}
}

func TestShellQuoteJoin_EmptyArgumentIsQuoted(t *testing.T) {
	if got := shellQuoteJoin([]string{"claude", ""}); got != "claude ''" {
		t.Fatalf("shellQuoteJoin = %q, want an empty argument to survive as ''", got)
	}
}

// ── Tier 2 redrive composition ───────────────────────────────────────────

func TestRedriveArgv_AppendsContractFlagsAfterConfiguredCommand(t *testing.T) {
	got, err := redriveArgv(teamArgv(), "sess-abc", "carry on")
	if err != nil {
		t.Fatalf("redriveArgv: %v", err)
	}

	want := []string{"claude", "--agent", "team", "--dangerously-skip-permissions",
		"--resume", "sess-abc", "-p", "carry on", "--output-format", "json"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %#v\nwant %#v", got, want)
	}
}

// The contract flags are Redrive's own and must survive any configuration —
// dropping or reordering them breaks Tier 2 silently for everyone who sets
// spawn.command.
func TestRedriveArgv_ContractFlagsAlwaysPresentAndInOrder(t *testing.T) {
	got, err := redriveArgv(teamArgv(), "sess-abc", "carry on")
	if err != nil {
		t.Fatalf("redriveArgv: %v", err)
	}

	tail := got[len(got)-6:]
	want := []string{"--resume", "sess-abc", "-p", "carry on", "--output-format", "json"}
	if !reflect.DeepEqual(tail, want) {
		t.Fatalf("contract tail = %#v, want %#v", tail, want)
	}
}

func TestRedriveArgv_DefaultIsUnchangedFromBefore(t *testing.T) {
	got, err := redriveArgv(settings.DefaultSpawnCommand(), "sess-abc123", "do the thing")
	if err != nil {
		t.Fatalf("redriveArgv: %v", err)
	}

	want := []string{"claude", "--resume", "sess-abc123", "-p", "do the thing", "--output-format", "json"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %#v\nwant the pre-existing %#v", got, want)
	}
}

// The configured argv must not be mutated by composition — spawnCommandFn's
// result is appended to on every re-drive.
func TestRedriveArgv_DoesNotMutateTheConfiguredArgv(t *testing.T) {
	configured := teamArgv()
	if _, err := redriveArgv(configured, "sess-abc", "x"); err != nil {
		t.Fatalf("redriveArgv: %v", err)
	}

	if !reflect.DeepEqual(configured, teamArgv()) {
		t.Fatalf("configured argv was mutated to %#v", configured)
	}
}

// ── Tier 2 redrive: conflict refusal ─────────────────────────────────────
//
// A configured flag that collides with the contract is refused LOUDLY rather
// than merged. A merged "--resume <other-id>" would produce a command with two
// session ids, and one of the two possible outcomes is re-driving a DIFFERENT
// session than the operator selected.

func TestRedriveArgv_RefusesConfiguredResume(t *testing.T) {
	_, err := redriveArgv([]string{"claude", "--resume", "some-other-session"}, "sess-abc", "x")

	if !errors.Is(err, ErrRedriveCommandConflict) {
		t.Fatalf("err = %v, want ErrRedriveCommandConflict", err)
	}
	if !strings.Contains(err.Error(), "--resume") {
		t.Fatalf("err %v does not name the offending flag", err)
	}
}

func TestRedriveArgv_RefusesConfiguredPrintShorthand(t *testing.T) {
	if _, err := redriveArgv([]string{"claude", "-p", "hello"}, "sess-abc", "x"); !errors.Is(err, ErrRedriveCommandConflict) {
		t.Fatalf("err = %v, want ErrRedriveCommandConflict", err)
	}
}

func TestRedriveArgv_RefusesConfiguredPrintLongForm(t *testing.T) {
	if _, err := redriveArgv([]string{"claude", "--print"}, "sess-abc", "x"); !errors.Is(err, ErrRedriveCommandConflict) {
		t.Fatalf("err = %v, want ErrRedriveCommandConflict", err)
	}
}

func TestRedriveArgv_RefusesConfiguredOutputFormat(t *testing.T) {
	if _, err := redriveArgv([]string{"claude", "--output-format", "text"}, "sess-abc", "x"); !errors.Is(err, ErrRedriveCommandConflict) {
		t.Fatalf("err = %v, want ErrRedriveCommandConflict", err)
	}
}

// The joined spelling must be caught too — a check that only matched separate
// arguments would let this through into the malformed invocation the guard
// exists to prevent.
func TestRedriveArgv_RefusesJoinedFlagSpelling(t *testing.T) {
	if _, err := redriveArgv([]string{"claude", "--output-format=text"}, "sess-abc", "x"); !errors.Is(err, ErrRedriveCommandConflict) {
		t.Fatalf("err = %v, want ErrRedriveCommandConflict for the --flag=value spelling", err)
	}
}

func TestRedriveArgv_ConflictReturnsNoArgv(t *testing.T) {
	argv, err := redriveArgv([]string{"claude", "--resume", "other"}, "sess-abc", "x")

	if err == nil {
		t.Fatal("expected a refusal")
	}
	if argv != nil {
		t.Fatalf("argv = %#v, want nil — a refused composition must not hand back a runnable command", argv)
	}
}

// A flag that merely CONTAINS a contract flag's name is not a conflict —
// refusing it would block legitimate configurations.
func TestRedriveArgv_SimilarlyNamedFlagIsNotAConflict(t *testing.T) {
	if _, err := redriveArgv([]string{"claude", "--resume-on-error", "--print-width", "80"}, "sess-abc", "x"); err != nil {
		t.Fatalf("refused a non-conflicting configuration: %v", err)
	}
}

// Redrive itself must surface the refusal rather than exec anything.
func TestRedrive_RefusesConflictingConfigurationWithoutExec(t *testing.T) {
	pinSpawnCommand(t, []string{"claude", "--resume", "other-session"})

	if err := Redrive("sess-abc", "x"); !errors.Is(err, ErrRedriveCommandConflict) {
		t.Fatalf("Redrive err = %v, want ErrRedriveCommandConflict", err)
	}
}

// ── --demo ───────────────────────────────────────────────────────────────

// `fleetops --demo` must ignore ~/.fleetops/settings.json entirely.
func TestUseDefaultSpawnCommand_IgnoresConfiguration(t *testing.T) {
	original := spawnCommandFn
	t.Cleanup(func() { spawnCommandFn = original })
	spawnCommandFn = teamArgv

	UseDefaultSpawnCommand()

	if got := spawnCommandFn(); !reflect.DeepEqual(got, []string{"claude"}) {
		t.Fatalf("spawn command after UseDefaultSpawnCommand = %#v, want [claude]", got)
	}
}

// And the demo default must reach the actual spawn sites, not just the seam.
func TestUseDefaultSpawnCommand_ReachesTheSpawnSites(t *testing.T) {
	original := spawnCommandFn
	t.Cleanup(func() { spawnCommandFn = original })
	spawnCommandFn = teamArgv

	UseDefaultSpawnCommand()

	if got := tmuxNewWindowCmd("/repo", spawnCommandFn()); containsArg(got, "--dangerously-skip-permissions") {
		t.Fatalf("demo mode leaked the configured command into tmux spawn: %#v", got)
	}
	if got := shellQuoteJoin(spawnCommandFn()); got != "claude" {
		t.Fatalf("demo mode leaked the configured command into orca spawn: %q", got)
	}
}
