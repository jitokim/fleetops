package control

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jitokim/fleetops/internal/accounts"
	"github.com/jitokim/fleetops/internal/settings"
)

// spawnCommandFn resolves the argv fleetops spawns a loop with —
// settings.SpawnCommand (~/.fleetops/settings.json) by default, falling back
// to ["claude"] whenever there is no usable configuration.
//
// A package var for two reasons: tests need to pin the argv without writing to
// the developer's real home directory, and `fleetops --demo` replaces it
// outright (see UseDefaultSpawnCommand).
//
// Read at CALL time, never cached at init: the point of a settings file is
// that editing it takes effect, and a long-running cockpit that read it once
// at startup would keep spawning yesterday's command until restarted.
var spawnCommandFn = settings.SpawnCommand

// UseDefaultSpawnCommand pins the spawn command to the built-in ["claude"],
// ignoring ~/.fleetops/settings.json entirely. Called by `fleetops --demo`.
//
// Demo mode's contract is "nothing real" — it must behave identically on every
// machine regardless of what the person running it has configured, or a
// screenshot/recording made in demo mode would leak their local setup. The TUI
// already refuses every spawning key in demo mode (isDemoBlockedKey), so this
// is defence in depth rather than the only guard: it makes the guarantee true
// of the mechanism, not just of the current keymap, so a future key that
// forgets the demo check still cannot reach a user's configured command.
func UseDefaultSpawnCommand() {
	spawnCommandFn = settings.DefaultSpawnCommand
}

// shellQuoteJoin renders an argv as a single POSIX-shell-safe command string,
// for the spawn sites that cannot pass an argv and must hand a shell (or a
// CLI that takes a command STRING) one flat word list. They are enumerated
// rather than counted — a count in prose goes stale the moment a third is
// added, while a list simply gains a bullet:
//
//   - orca's `terminal create --command`, whose contract takes a string (orca.go).
//   - iTerm2's launch line, which must be a shell line because `create window`
//     starts a login shell and setting a working directory is a shell
//     operation (iterm2LaunchLine).
//
// Every other spawn site — tmux's new-window, the Tier 2 redrive — passes argv
// through untouched, which is strictly better (see internal/settings' package
// doc on why argv is the right shape). This exists only where that is
// impossible; and since the value now comes from a user-editable settings file
// rather than a hardcoded literal, joining with a bare space would break the
// moment any argument contained a space and would be a shell-injection surface
// besides.
//
// Single quotes, because inside them the shell interprets nothing at all; the
// only character needing care is the single quote itself, which is closed,
// backslash-escaped and re-opened via the standard POSIX idiom (see
// shellQuote's ReplaceAll). Arguments made purely of characters that are
// already shell-inert are passed through unquoted, purely so the common case
// stays readable in logs and error messages.
func shellQuoteJoin(argv []string) string {
	quoted := make([]string, 0, len(argv))
	for _, arg := range argv {
		quoted = append(quoted, shellQuote(arg))
	}
	return strings.Join(quoted, " ")
}

// shellSafeChars are the characters that need no quoting in any POSIX shell
// word. A deliberately CONSERVATIVE whitelist — anything not listed gets
// quoted, so the failure mode of a wrong guess here is an unnecessary pair of
// quotes rather than an unquoted metacharacter.
const shellSafeChars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-./=:+@,"

func shellQuote(arg string) string {
	if arg != "" && strings.IndexFunc(arg, func(r rune) bool {
		return !strings.ContainsRune(shellSafeChars, r)
	}) < 0 {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
}

// claudeConfigDirEnv is the environment variable that scopes which Claude
// account a spawned loop runs as (see internal/accounts' package doc). fleetops
// injects it, per spawn, as the account bound to the spawn's cwd — never a
// token, only the config-dir PATH the user already logged into.
const claudeConfigDirEnv = "CLAUDE_CONFIG_DIR"

// accountConfigDirFn resolves the CLAUDE_CONFIG_DIR a spawn in cwd should run
// under, ok=false meaning "no account bound to cwd" — the zero-config default,
// where spawn behaves exactly as it did before this feature.
//
// A package var for the same reason spawnCommandFn is one: it is the seam that
// lets spawnArgvForCwd (and thus every spawn site routed through it) be tested
// with a fake resolver, asserting the env prefix is added for a bound cwd and
// NOT added for an unbound one, without writing a real ~/.fleetops/accounts.json.
var accountConfigDirFn = defaultAccountConfigDir

// defaultAccountConfigDir is accountConfigDirFn's production implementation:
// load ~/.fleetops/accounts.json and resolve cwd through it, with git-based
// worktree→origin resolution wired in via GitMainRepoDir.
//
// A Load ERROR (malformed JSON, or a binding naming an unknown alias) is
// treated as INACTIVE here — the spawn proceeds with no account prefix, exactly
// as if no config existed. This is a deliberate Phase A limitation, not the
// package's own posture: internal/accounts.Load fails CLOSED (it returns the
// error rather than silently resolving to "no account"), but blocking every
// spawn tool-wide on a single JSON typo is too hostile a failure mode for the
// only Phase A consumer, which is this optional prefix. Surfacing that
// misconfiguration to the human belongs to the Phase C "n"-wizard account step
// (internal/tui's proceedFromWhere), which DOES surface a Load error as a
// one-line "accounts.json invalid — spawning under the default account" warning
// before it spawns — the warning channel this spawn path does not have.
func defaultAccountConfigDir(cwd string) (configDir string, ok bool) {
	cfg, err := accounts.Load(accounts.DefaultPath())
	if err != nil {
		return "", false
	}
	_, configDir, ok = cfg.ResolveForCwd(cwd, GitMainRepoDir)
	return configDir, ok
}

// GitMainRepoDir maps cwd to the MAIN repo root of the git repository that
// contains it, so a linked worktree resolves to the repo it was branched from
// (its account binding lives on the origin, not on the freshly-created
// worktree). ok=false on any failure — not a repo, no git binary — in which
// case the caller matches on cwd itself, today's behavior.
//
// It uses `git rev-parse --git-common-dir`, NOT --show-toplevel, on purpose:
// --show-toplevel returns a worktree's OWN root (the not-yet-bound path we must
// avoid), whereas --git-common-dir names the single shared .git directory of
// the origin for BOTH a main checkout and a linked worktree — and its parent is
// the origin repo root. git may report that path relative to cwd, so a
// non-absolute result is joined onto cwd before its parent is taken.
//
// This is the one git-touching seam the accounts feature needs; it lives HERE,
// outside internal/accounts, so that package stays pure and git-free. The
// worktree→origin resolution LOGIC it feeds is unit-tested in internal/accounts
// via an injected fake; this thin production glue is exercised at spawn time.
//
// Exported so the TUI's "n"-wizard account picker can resolve the SAME
// worktree→origin binding this spawn path resolves — one git helper, not two
// (see internal/tui's resolveAccountCmd, which feeds it to
// accounts.Config.ResolveForCwd exactly as defaultAccountConfigDir does above).
func GitMainRepoDir(cwd string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), availabilityTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", cwd, "rev-parse", "--git-common-dir").Output()
	if err != nil {
		return "", false
	}
	commonDir := strings.TrimSpace(string(out))
	if commonDir == "" {
		return "", false
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(cwd, commonDir)
	}
	return filepath.Dir(filepath.Clean(commonDir)), true
}

// spawnArgvForCwd is the cwd-aware spawn argv: the configured base command
// (spawnCommandFn), with an `env CLAUDE_CONFIG_DIR=<dir>` prefix layered on
// when — and only when — accountConfigDirFn resolves an account for cwd. When
// no account is bound it returns the base argv UNCHANGED, so a machine with no
// accounts.json spawns byte-identically to before.
//
// # Why `env VAR=val cmd` as the uniform mechanism
//
// It composes across every backend regardless of whether the backend takes an
// argv (tmux new-window, iTerm2's launch line) or a shell-quoted STRING (orca's
// --command): `env VAR=val cmd` is valid in all of them, and shellQuoteJoin
// passes the "CLAUDE_CONFIG_DIR=<dir>" token through intact (its characters are
// shell-inert unless the dir contains a space, in which case the whole token is
// quoted as one word — still correct).
//
// # Why argv[0] becoming "env" does NOT break actuation
//
// /usr/bin/env is not a launcher that lingers: it sets the variable and
// execve's claude in place, REPLACING itself, so the running process image —
// and therefore the pane's foreground command (tmux's #{pane_current_command},
// ps comm) — is "claude", exactly what control.isClaudeComm matches on. This is
// the crucial difference from a wrapper like ["mise","exec","--","claude"],
// which internal/settings rejects precisely because mise would STAY the
// foreground process. env does not stay, so LocateClaude/LocateByTTY still find
// the loop. (A vanishingly short window exists between exec of env and exec of
// claude; every spawn site already waits for the TUI to boot long after it, so
// no locate ever observes "env".)
func spawnArgvForCwd(cwd string) []string {
	configDir, ok := accountConfigDirFn(cwd)
	if !ok {
		// Unbound cwd — the zero-config default. Return the base command
		// UNCHANGED (no env, no CLAUDE_CONFIG_DIR): a machine with no
		// accounts.json spawns byte-identically to before this feature.
		return spawnCommandFn()
	}
	return spawnArgvWithConfigDir(configDir)
}

// spawnArgvWithConfigDir is spawnArgvForCwd's EXPLICIT-account sibling: it
// fronts the configured base command with `env CLAUDE_CONFIG_DIR=<configDir>`
// for a configDir supplied by the CALLER rather than re-resolved from a cwd.
//
// The "n"-wizard's account picker is the caller: once the human has chosen an
// account for an UNBOUND spawn dir, re-resolving by cwd (spawnArgvForCwd) would
// resolve to nothing (no binding) and silently discard their choice — so the
// wizard threads the chosen configDir here instead. A "" configDir means the
// DEFAULT account (the picker's explicit default choice, or a bound-but-empty
// alias that ResolveForCwd already fails closed on): return the base command
// unchanged, exactly as an unbound cwd does, so an explicit default and an
// absent config land on the identical byte-for-byte spawn.
//
// The mechanism (why `env VAR=val cmd` composes across every backend, and why
// argv[0] becoming "env" does not break actuation) is spawnArgvForCwd's — see
// its doc; this only changes WHERE the configDir comes from, never how it is
// injected.
func spawnArgvWithConfigDir(configDir string) []string {
	argv := spawnCommandFn()
	if configDir == "" {
		return argv
	}
	return append([]string{"env", claudeConfigDirEnv + "=" + configDir}, argv...)
}

// LoginArgv is the ONE definition of the `claude login` invocation that
// authenticates the account scoped by configDir: `env
// CLAUDE_CONFIG_DIR=<configDir> claude login`, or a bare `["claude", "login"]`
// for the default account (configDir==""). It is the argv form both consumers
// share — the TUI wizard, which flattens it to a shell string for a
// TerminalOpener (LoginTerminalCommand), and the `fleetops accounts` CLI, which
// runs it directly with inherited stdio. Keeping a single builder is why the
// browser flow the two trigger can never drift on WHICH command scopes the
// account. No token passes through here: it only names the login subcommand and
// the config-dir path.
func LoginArgv(configDir string) []string {
	if configDir == "" {
		return []string{"claude", "login"}
	}
	return []string{"env", claudeConfigDirEnv + "=" + configDir, "claude", "login"}
}

// LoginTerminalCommand renders LoginArgv as a shell command string for the
// "n"-wizard's account picker, which hands it to a TerminalOpener so the human
// can complete the browser OAuth for an alias that is not yet logged in.
//
// A STRING (not an argv) because that is the shape TerminalOpener.OpenTerminal
// consumes — orca's `terminal create --command` takes a command string and
// tmux's `new-window` takes a single trailing shell word; both interpret this
// through a shell, so it is shell-quoted (shellQuoteJoin) exactly as the spawn
// sites that must flatten an argv are. fleetops only LAUNCHES the flow; the
// browser half, and the credential write into configDir, are claude's and the
// human's — no token ever passes through here.
func LoginTerminalCommand(configDir string) string {
	return shellQuoteJoin(LoginArgv(configDir))
}
