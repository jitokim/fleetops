// Package accounts maps a working directory to a Claude account, expressed as
// the CLAUDE_CONFIG_DIR that scopes it.
//
// The account axis is CLAUDE_CONFIG_DIR: running claude with
// CLAUDE_CONFIG_DIR=<dir> makes it act as whichever account is logged in under
// <dir> (measured live on claude 2.1.215 — an empty dir reports loggedIn:false,
// the default dir reports the real account). fleetops never touches tokens; it
// only remembers, per directory, WHICH config dir a spawned loop should run
// under, and hands that to the spawn as an environment variable.
//
// The model is alias-centric so the same account can be named once and bound to
// many directories:
//
//   - aliases: a human name → an absolute config dir ("company" → "/…/.claude-work").
//   - bindings: an absolute path (a repo or a parent dir) → an alias.
//
// A cwd resolves to an account by finding the LONGEST binding path that is a
// (component-wise) prefix of it, then mapping that binding's alias to its
// config dir. A git worktree resolves to its ORIGIN repo first (via the
// injected mainRepoDir seam), so a worktree inherits the account of the repo it
// was branched from without needing its own binding.
//
// # Purity
//
// This package is deliberately git-free and does no I/O beyond reading its own
// JSON file: ResolveForCwd takes the "worktree → main repo" step as an injected
// function so the whole package is unit-testable without a real git tree, and
// so the one place that DOES shell out to git (the spawn layer that wires the
// production seam) stays outside the pure resolution logic.
//
// # Fail closed, not open
//
// The one thing this package must never do is silently resolve to "no account"
// because of a typo. A binding that names an alias absent from "aliases" is a
// configuration ERROR surfaced by Load — not a binding quietly skipped — because
// skipping it would spawn work under the DEFAULT account (whatever the user is
// logged into globally), which is exactly the wrong-account mistake the feature
// exists to prevent. A MISSING file, by contrast, is not an error at all: the
// feature is simply inactive and spawning behaves as it did before.
package accounts

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Config is the parsed ~/.fleetops/accounts.json. The zero value is a valid,
// INACTIVE config: no aliases and no bindings, so every ResolveForCwd returns
// ok=false and spawning behaves exactly as it did before this feature.
type Config struct {
	// Aliases maps a human account name to the absolute CLAUDE_CONFIG_DIR that
	// scopes it. Holds names and paths only — never a token.
	Aliases map[string]string `json:"aliases"`
	// Bindings maps a directory (repo or parent) to an alias. Order is
	// preserved from the file but does not affect resolution: the LONGEST
	// matching path wins regardless of position (see ResolveForCwd).
	Bindings []Binding `json:"bindings"`
	// AliasGitEmails is the OPTIONAL, opt-in git-identity expectation: alias →
	// the git user.email the user DECLARES that account should commit as. Its
	// sole use is the DETAIL panel's ⚠ mismatch marker, shown only when a repo's
	// actual user.email disagrees with its alias's declared email. Absent for
	// the common user — and absence means no check, EVER: fleetops never infers
	// that a Claude-account / git-identity difference is wrong (committing a
	// company repo under a personal email is a legitimate signed-off-by
	// workflow, not a bug). Holds emails only, never a token.
	//
	// Unlike Bindings, a stale/unknown alias key here is NOT a validation error:
	// this is a cosmetic display hint, and it must never fail the config LOAD
	// that gates spawning. A typo just yields no expectation (no check), never a
	// blocked spawn.
	AliasGitEmails map[string]string `json:"alias_git_emails,omitempty"`
}

// GitEmailForAlias returns the git committer email the user has DECLARED this
// alias should commit as (via "alias_git_emails"), and ok=false when none is
// declared. This is the ONLY source of a mismatch expectation — a missing
// entry (the common case) means "no expectation, never warn". fleetops flags a
// git-identity disagreement only when the user explicitly asked it to.
func (c Config) GitEmailForAlias(alias string) (email string, ok bool) {
	if alias == "" {
		return "", false
	}
	email, ok = c.AliasGitEmails[alias]
	if !ok || email == "" {
		return "", false
	}
	return email, true
}

// Binding ties a directory to an account alias.
type Binding struct {
	Path  string `json:"path"`
	Alias string `json:"alias"`
}

// DefaultPath is ~/.fleetops/accounts.json — beside the other ~/.fleetops state
// (loops/, sessions/, settings.json), so there is no in-repo config to
// gitignore. Empty when the home directory cannot be determined, which Load
// treats as "no configuration" — the same tolerant posture as a missing file.
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".fleetops", "accounts.json")
}

// Load reads and validates the accounts config at path.
//
// A missing file is NOT an error — it returns the zero Config (feature
// inactive), because the overwhelmingly common case is "the user has not opted
// in" and that must behave exactly as before. A file that IS present but
// malformed (bad JSON) or invalid (a binding naming an unknown alias) IS an
// error: once the user has written a config, a typo in it must be surfaced, not
// silently swallowed into "no account".
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, nil
		}
		return Config{}, fmt.Errorf("accounts: reading %s: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("accounts: parsing %s: %w", path, err)
	}
	// Expand a leading "~"/"~/" to the home dir BEFORE validating, so a
	// config written the way the design doc's own example is ("~/.claude-work")
	// works: a "~" left literal would be shell-quoted verbatim at spawn into a
	// bogus RELATIVE config dir (an unauthenticated session), and a "~/work"
	// binding would never match an absolute cwd (a silent default-account
	// spawn). Both are the exact wrong-account failures this package exists to
	// prevent, so expansion is not a convenience — it is part of failing closed.
	home, _ := os.UserHomeDir()
	cfg.expandPaths(home)
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// expandPaths rewrites every alias config dir and binding path in place,
// expanding a leading "~" or "~/" to home. An empty home (UserHomeDir failed)
// leaves a "~" path untouched, which validate then rejects as non-absolute —
// failing closed rather than expanding to a bogus root.
func (c Config) expandPaths(home string) {
	for name, dir := range c.Aliases {
		c.Aliases[name] = expandTilde(dir, home)
	}
	for i := range c.Bindings {
		c.Bindings[i].Path = expandTilde(c.Bindings[i].Path, home)
	}
}

// expandTilde replaces a leading "~" (alone) or "~/" prefix with home. Any
// other form — a bare relative path, an absolute path, "~user/…" (another
// user's home, which this package does not resolve) — is returned unchanged and
// left for validate to accept (absolute) or reject (still relative).
func expandTilde(path, home string) string {
	if home == "" || path == "" {
		return path
	}
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}

// validate rejects a config whose binding references an alias that "aliases"
// does not define, OR whose alias config dir / binding path is not absolute
// after tilde expansion. Both are the fail-closed guarantee: a mistyped alias
// or a relative path must stop the config from loading rather than degrade to
// the default account. A relative alias dir would be shell-quoted verbatim at
// spawn into a bogus config dir (an unauthenticated session), and a relative
// binding path could never match an absolute cwd — exactly the wrong-account
// mistakes this package exists to prevent.
func (c Config) validate() error {
	for name, dir := range c.Aliases {
		if dir != "" && !filepath.IsAbs(dir) {
			return fmt.Errorf(
				"accounts: alias %q config dir %q is not absolute — use an absolute path or a \"~\"-prefixed one (a relative config dir spawns an unauthenticated session)",
				name, dir)
		}
	}
	for _, b := range c.Bindings {
		if _, ok := c.Aliases[b.Alias]; !ok {
			return fmt.Errorf(
				"accounts: binding for %q references unknown alias %q — add %q to \"aliases\" (a typo must not silently fall back to the default account)",
				b.Path, b.Alias, b.Alias)
		}
		if !filepath.IsAbs(b.Path) {
			return fmt.Errorf(
				"accounts: binding path %q is not absolute — use an absolute path or a \"~\"-prefixed one (a relative binding never matches an absolute cwd, silently falling back to the default account)",
				b.Path)
		}
	}
	return nil
}

// ResolveForCwd resolves the account a loop spawned in cwd should run under,
// returning the alias, its CLAUDE_CONFIG_DIR, and ok=false when no binding
// applies (spawn with no account override, today's behavior).
//
// mainRepoDir is an injected seam that maps cwd to the main repo of the git
// worktree containing it, so a linked worktree resolves to the ORIGIN repo it
// was branched from — the origin carries the binding, the fresh worktree does
// not. It may be nil (then cwd is used as-is) and may report ok=false for a
// non-repo cwd (then cwd is used as-is). Keeping it injected is what lets this
// package stay git-free and fully unit-testable.
//
// Matching is component-wise longest-prefix: "/a/b" matches "/a/b" and
// "/a/b/c" but NOT "/a/bc", and among all matching bindings the one with the
// longest path wins (the most specific binding). Equal-length matches can only
// arise from a duplicate path, where the FIRST in file order wins.
func (c Config) ResolveForCwd(cwd string, mainRepoDir func(cwd string) (string, bool)) (alias, configDir string, ok bool) {
	// No bindings ⇒ nothing can ever match, so skip mainRepoDir entirely. That
	// call shells out to git (2s budget) in production, and running it on a
	// zero-config machine would add a git subprocess to every spawn for no
	// possible gain — the whole "zero-config is byte-identical and adds no
	// subprocess" promise depends on this early exit.
	if len(c.Bindings) == 0 {
		return "", "", false
	}
	matchKey := filepath.Clean(cwd)
	if mainRepoDir != nil {
		if root, found := mainRepoDir(cwd); found && root != "" {
			matchKey = filepath.Clean(root)
		}
	}

	bestAlias := ""
	bestLen := -1
	for _, b := range c.Bindings {
		bindingPath := filepath.Clean(b.Path)
		if !isPathPrefix(bindingPath, matchKey) {
			continue
		}
		if len(bindingPath) > bestLen {
			bestLen = len(bindingPath)
			bestAlias = b.Alias
		}
	}
	if bestLen < 0 {
		return "", "", false
	}
	// Defensive even though Load's validate already guarantees the alias
	// exists: a Config constructed directly (not via Load) must still fail
	// CLOSED — an unknown or config-dir-less alias yields ok=false, never a
	// bare spawn presented as a resolved account.
	dir, defined := c.Aliases[bestAlias]
	if !defined || dir == "" {
		return "", "", false
	}
	return bestAlias, dir, true
}

// AliasForConfigDir is the reverse lookup Phase B's display needs: given a
// CLAUDE_CONFIG_DIR observed on a running session, name its alias. ok=false
// when no alias maps to that dir.
//
// Tie-break: when more than one alias points at the same config dir, the
// lexicographically FIRST alias name wins. Go map iteration is randomized, so
// the names are sorted before scanning to make the answer deterministic — a
// display badge that flickered between two names on successive scans would be a
// bug, not a cosmetic detail.
func (c Config) AliasForConfigDir(configDir string) (alias string, ok bool) {
	target := filepath.Clean(configDir)
	names := make([]string, 0, len(c.Aliases))
	for name := range c.Aliases {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if filepath.Clean(c.Aliases[name]) == target {
			return name, true
		}
	}
	return "", false
}

// isPathPrefix reports whether prefix is a component-wise path prefix of path:
// true when they are equal or when path continues past prefix at a separator
// boundary. The separator boundary is what stops "/a/b" from matching "/a/bc" —
// a raw strings.HasPrefix would wrongly treat "bc" as living under "b". Both
// arguments are expected already filepath.Clean'd.
func isPathPrefix(prefix, path string) bool {
	if prefix == path {
		return true
	}
	if prefix == string(filepath.Separator) {
		// The filesystem root is a prefix of every absolute path; appending a
		// second separator below would build "//" and never match.
		return strings.HasPrefix(path, prefix)
	}
	return strings.HasPrefix(path, prefix+string(filepath.Separator))
}
