// Package settings reads fleetops's user configuration from
// ~/.fleetops/settings.json.
//
// It lives OUTSIDE the repo on purpose — beside the other ~/.fleetops state
// (loops/, sessions/, gates/, history/) — so there is no in-repo config file
// to gitignore, and a user's local choices can never be committed by accident.
//
// Today it carries exactly one setting, spawn.command. The file shape is:
//
//	{ "spawn": { "command": ["claude", "--agent", "team", "--dangerously-skip-permissions"] } }
//
// # Why an argv ARRAY and not a command string
//
// This is measured, not stylistic. The maintainer's `team` is a zsh FUNCTION:
//
//	team() { claude --agent team --dangerously-skip-permissions; }
//
// which exists only inside an interactive shell — `zsh -c team` and
// `zsh -lc team` both report "not found", and neither tmux nor orca spawns an
// interactive shell. So a command-NAME setting would silently fail for exactly
// the user it is for. An argv array sidesteps the shell entirely: os/exec
// calls execve directly, so there is no word splitting, no quoting layer to
// get wrong, and no shell-injection surface — the same argv discipline
// internal/control's hostsend.go already enforces on the osascript path.
//
// It also keeps the PROCESS NAME "claude", which is load-bearing: tmux's
// LocateByTTY (Tier 1a) and LocateClaude (Tier 1b), and cmux's tree walk, all
// filter panes through control.isClaudeComm. argv[0] is therefore REQUIRED to
// be claude (see defaultSpawnCommandName) and the flags are its arguments, so that match
// is unaffected. A shell-string setting, or a launcher-style argv[0] such as
// ["mise","exec","--","claude"], would leave the pane's foreground command
// reading something else and make every configured loop invisible to
// a/r/i/p/k — the loop would still run, it just could not be reached.
//
// # Never a hard failure
//
// An absent file, unreadable file, malformed JSON, a wrong-typed value, an
// empty array, or an array containing an empty element ALL fall back to
// ["claude"] — today's behaviour. Configuration is a convenience; a typo in it
// must not be able to stop a user spawning a loop, and there is a correct
// default to fall back to. Errors are therefore not returned at all: there is
// no decision a caller could make with one.
package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// defaultSpawnCommandName is the command fleetops spawns with when nothing is
// configured — the behaviour that predates this package — and, by basename,
// the ONLY accepted argv[0] (an absolute path to the same binary is fine).
//
// That second role is ENFORCED rather than merely documented, because a
// launcher-style argv[0] silently costs the user their entire multiplexer
// actuation surface, not just one tier: tmux's LocateByTTY (Tier 1a) and
// LocateClaude (Tier 1b), and cmux's tree walk, all filter panes through
// control.isClaudeComm. A configured ["mise","exec","--","claude"] leaves the
// pane's foreground command reading "mise", so every configured loop becomes
// invisible to a/r/i/p/k — the loop still runs, it just cannot be reached,
// which is the worst of the available failures.
//
// KNOWN GAP: rejection falls back to the default SILENTLY, so a user whose
// config was refused learns only by noticing it had no effect. Surfacing it
// needs a warning channel this package does not have (SpawnCommand returns
// []string and nothing else).
const defaultSpawnCommandName = "claude"

// DefaultSpawnCommand returns the built-in spawn argv, ["claude"].
//
// Exported because it is not merely an internal fallback: `fleetops --demo`
// must ignore the user's configuration ENTIRELY and always use this, so the
// demo has an explicit, greppable thing to ask for rather than a magic string
// spelled a second time (see control.UseDefaultSpawnCommand).
//
// Returns a fresh slice each call so a caller that appends to the result — as
// the Tier 2 redrive does — cannot mutate shared state.
func DefaultSpawnCommand() []string {
	return []string{defaultSpawnCommandName}
}

// Path is ~/.fleetops/settings.json. Empty when the home directory cannot be
// determined, which SpawnCommand treats as "no configuration" — the same
// tolerant posture as a missing file.
func Path() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".fleetops", "settings.json")
}

// file is settings.json's on-disk shape. Unknown keys are ignored by
// encoding/json, so a file carrying future settings still yields a usable
// spawn command instead of being rejected wholesale.
type file struct {
	Spawn struct {
		Command []string `json:"command"`
	} `json:"spawn"`
}

// SpawnCommand returns the configured spawn argv, or DefaultSpawnCommand when
// there is no usable configuration.
func SpawnCommand() []string {
	return spawnCommandFrom(Path())
}

// spawnCommandFrom is SpawnCommand with the path injected, so every fallback
// branch is testable against a temp file instead of the caller's real home
// directory.
func spawnCommandFrom(path string) []string {
	if path == "" {
		return DefaultSpawnCommand()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return DefaultSpawnCommand()
	}
	var decoded file
	if err := json.Unmarshal(data, &decoded); err != nil {
		// Malformed JSON, or "command" given as a bare string rather than an
		// array — the most likely single mistake a human makes with this file.
		// Both degrade to the default rather than failing the spawn.
		return DefaultSpawnCommand()
	}
	return validSpawnCommand(decoded.Spawn.Command)
}

// validSpawnCommand accepts an argv only if it is non-empty and every element
// is non-blank; anything else falls back to the default.
//
// It rejects the WHOLE argv rather than dropping blank elements. A blank
// element is a typo — a stray comma, an unfinished edit — and silently
// executing the remainder would run a command the human did not write. Falling
// back to a known-good default is the honest response to "I cannot tell what
// you meant."
func validSpawnCommand(argv []string) []string {
	if len(argv) == 0 {
		return DefaultSpawnCommand()
	}
	for _, arg := range argv {
		if strings.TrimSpace(arg) == "" {
			return DefaultSpawnCommand()
		}
	}
	if filepath.Base(argv[0]) != defaultSpawnCommandName {
		return DefaultSpawnCommand()
	}
	return append([]string(nil), argv...) // copy: callers append to this
}
