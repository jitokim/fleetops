package control

import (
	"strings"

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
// for the one backend whose CLI takes a command STRING rather than an argv:
// orca's `terminal create --command`.
//
// Every other spawn site passes argv through untouched, which is strictly
// better (see internal/settings' package doc on why argv is the right shape).
// This exists only because orca's contract leaves no choice — and since the
// value now comes from a user-editable settings file rather than a hardcoded
// literal, joining with a bare space would break the moment any argument
// contained a space and would be a shell-injection surface besides.
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
