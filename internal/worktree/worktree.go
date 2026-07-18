// Package worktree creates a fresh, isolated git worktree for a new loop
// using plain `git` — fleetops's OWN worktree capability, independent of any
// terminal backend.
//
// It exists because worktree isolation used to be reachable only through
// `orca worktree create` (internal/control's orcaController.SpawnWorktree),
// which made a plain git feature look like a vendor capability: a user on
// tmux, on iTerm2, or on nothing at all could not get an isolated checkout,
// for no reason other than which terminal they had installed. Nothing in the
// tree called `git worktree` directly before this package.
//
// It is deliberately NOT in internal/control. control is the actuation layer —
// "drive a terminal surface" — and creating a checkout drives no terminal at
// all. Keeping it separate is what lets any spawn path (orca, tmux, iTerm2,
// or a future one) ask for a worktree without going through a backend, and
// keeps control's dependency surface where its own package doc puts it.
//
// # Naming and base
//
// The convention is the maintainer's own, encoded here rather than left to
// habit:
//
//	git worktree add -b wt-<YYYYMMDD-HHMMSS> ../<repo>-wt-<YYYYMMDD-HHMMSS> origin/<default-branch>
//
// Two deliberate choices:
//
//   - The name is a TIMESTAMP, never derived from the loop's goal. Goal-derived
//     slugging (internal/tui's slugFromGoal, which feeds orca's --name) keeps
//     only [a-z0-9], so every pure-Korean goal collapses to the same "loop"
//     fallback — "마케팅 전략 수립" and "상태관리 설계" both slug to "mctl-loop".
//     The primary user writes Korean goals, so a goal-derived name is a
//     collision generator for exactly the person it is meant to serve. A
//     timestamp is language-independent. The FLEET list already carries the
//     wizard's display name/goal, so the directory name owes the human nothing.
//
//   - The base is EXPLICIT and resolved to origin/<default-branch>, never left
//     implicit. `git worktree add` with no base silently uses current HEAD —
//     whatever branch happened to be checked out, however stale. That is the
//     gap that nearly reverted a version bump in this repo once (PR #48, a
//     branch cut from a stale base). Passing the base explicitly is the whole
//     point; see defaultBase for why the branch is resolved from the remote
//     rather than hardcoded to "main".
package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ErrNotARepo reports that the directory a spawn was asked to branch from is
// not inside a git repository at all. Its own sentinel rather than a wrapped
// git error because the operator's fix is specific and nothing else here can
// proceed: there is no repo to make a worktree of.
var ErrNotARepo = errors.New("worktree: not inside a git repository")

// ErrNoRemote reports that the repo has no "origin" remote, so the
// origin/<default-branch> base this package insists on cannot be resolved.
//
// This REFUSES rather than falling back to HEAD, and that is the design, not
// an oversight. Falling back would silently reintroduce exactly the
// implicit-stale-base behaviour the explicit base exists to prevent — and it
// would do so invisibly, at the one moment the human has least reason to
// suspect it. A loud refusal costs a local-only repo a spawn; a silent
// fallback costs everyone the guarantee.
var ErrNoRemote = errors.New("worktree: no 'origin' remote — cannot resolve an origin/<default-branch> base to branch from")

// ErrNoDefaultBranch reports that "origin" exists but its default branch could
// not be determined by either probe (see defaultBase). Distinct from
// ErrNoRemote because the operator's fix differs: a missing remote is
// "add one", an unresolvable default branch is usually "run
// `git remote set-head origin -a`".
var ErrNoDefaultBranch = errors.New("worktree: could not resolve origin's default branch (try: git remote set-head origin -a)")

// ErrPathExists reports that the sibling directory the new worktree would
// occupy is already taken. Checked BEFORE invoking git so the failure names
// the path and cannot be confused with git's own less specific complaint.
// Reachable in practice by two spawns inside the same clock second.
var ErrPathExists = errors.New("worktree: target worktree directory already exists")

// defaultRemote is the remote the base is resolved against. Hardcoded to
// "origin" deliberately: the convention this package encodes is literally
// "origin/<default-branch>", and a configurable remote would be a knob for a
// choice nobody has asked to make.
const defaultRemote = "origin"

// nameTimestampLayout formats the shared <YYYYMMDD-HHMMSS> stamp that names
// BOTH the branch and the directory — one layout constant so the two can
// never drift into disagreeing about what a worktree is called.
const nameTimestampLayout = "20060102-150405"

// gitTimeout bounds a single git invocation. Generous enough for ls-remote,
// which may contact the network (see defaultBase), and bounded so a hung
// remote can never wedge the caller — the same never-hang discipline
// internal/control applies to every exec it makes.
const gitTimeout = 30 * time.Second

// Result describes the worktree that was created. Base is carried so callers
// can TELL THE HUMAN what the branch was actually cut from — the explicit base
// is this package's whole reason to exist, and a guarantee nobody can see is
// one nobody can trust.
type Result struct {
	Path   string // absolute path of the new worktree directory
	Branch string // the branch created, e.g. "wt-20260719-011612"
	Base   string // the explicit base it was cut from, e.g. "origin/main"
}

// gitFn runs `git -C dir args...` and returns its trimmed stdout. An
// injectable package var (same seam discipline as internal/control's
// iterm2SendFn/pidTTYFn) so failure paths that are awkward to stage with a
// real repo can be tested directly — the success paths are tested against a
// REAL git and a real temp repo, which is the stronger test and the default
// here.
var gitFn = func(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).Output()
	return strings.TrimSpace(string(out)), withGitStderr(err)
}

// withGitStderr folds a failed git command's stderr into its error.
// exec.ExitError stringifies to a bare "exit status 128" while carrying the
// only text that says WHY in an ignored field — on this path that text IS the
// diagnosis ("fatal: 'wt-...' is already checked out at ..."). Mirrors
// internal/control's withCommandStderr, which exists for the same reason on
// the osascript path.
func withGitStderr(err error) error {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return err
	}
	stderr := strings.TrimSpace(string(exitErr.Stderr))
	if stderr == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, stderr)
}

// Create makes a fresh worktree branched from origin/<default-branch> for the
// repo containing repoDir, named by the current time.
func Create(repoDir string) (Result, error) {
	return create(repoDir, time.Now())
}

// create is Create with the clock injected, so the naming convention is
// testable against a fixed instant instead of whatever second the suite
// happens to run in.
func create(repoDir string, now time.Time) (Result, error) {
	root, err := repoRoot(repoDir)
	if err != nil {
		return Result{}, err
	}
	base, err := defaultBase(root)
	if err != nil {
		return Result{}, err
	}
	name := BranchName(now)
	path := SiblingPath(root, name)
	if err := ensureFree(path); err != nil {
		return Result{}, err
	}
	if _, err := gitFn(root, "worktree", "add", "-b", name, path, base); err != nil {
		return Result{}, fmt.Errorf("worktree: git worktree add: %w", err)
	}
	return Result{Path: path, Branch: name, Base: base}, nil
}

// ensureFree refuses when path is already taken. Lstat, not Stat, so a
// DANGLING SYMLINK counts as occupied too: git would fail on it anyway, and
// reporting "already exists" for something that visibly exists is more honest
// than reporting nothing and letting git's message describe a symlink the
// human did not know was there.
func ensureFree(path string) error {
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("%w: %s", ErrPathExists, path)
	}
	return nil
}

// repoRoot resolves the top level of the repo containing dir. Any failure is
// ErrNotARepo: `rev-parse --show-toplevel` fails for a non-repo, a missing
// directory, and a missing git binary alike, and all three mean the same thing
// to the caller — there is no repo here to branch from.
func repoRoot(dir string) (string, error) {
	root, err := gitFn(dir, "rev-parse", "--show-toplevel")
	if err != nil || root == "" {
		return "", fmt.Errorf("%w: %s", ErrNotARepo, dir)
	}
	return root, nil
}

// defaultBase resolves the explicit base ref: "origin/<default-branch>".
//
// The default branch is RESOLVED, never hardcoded to "main" — "master" repos
// exist, and so do repos whose default is something else entirely; a
// hardcoded guess would either fail loudly on those or, worse, branch from a
// stale ref that happens to exist. Two probes, cheapest first:
//
//  1. `symbolic-ref refs/remotes/origin/HEAD` — purely local and instant, set
//     by clone and by `git remote set-head`. This is the common case.
//  2. `ls-remote --symref origin HEAD` — asks the remote itself. Slower and
//     may touch the network, so it is the fallback rather than the primary,
//     but it is authoritative and it works on repos whose local origin/HEAD
//     was never set (a plain `git init` + `git remote add`).
//
// The remote's existence is checked FIRST so a repo with no origin gets
// ErrNoRemote — the accurate fact — instead of ErrNoDefaultBranch after two
// probes fail for a reason that was knowable up front.
func defaultBase(root string) (string, error) {
	if !hasOriginRemote(root) {
		return "", fmt.Errorf("%w: %s", ErrNoRemote, root)
	}
	if ref, err := gitFn(root, "symbolic-ref", "--short", "refs/remotes/"+defaultRemote+"/HEAD"); err == nil && ref != "" {
		return ref, nil
	}
	out, err := gitFn(root, "ls-remote", "--symref", defaultRemote, "HEAD")
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrNoDefaultBranch, root)
	}
	branch, ok := parseSymrefHead(out)
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrNoDefaultBranch, root)
	}
	return defaultRemote + "/" + branch, nil
}

// hasOriginRemote reports whether `git remote` lists defaultRemote. An exact
// line match, never a substring: a remote named "origin-mirror" is not origin,
// and treating it as one would resolve a base against the wrong repository.
func hasOriginRemote(root string) bool {
	out, err := gitFn(root, "remote")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == defaultRemote {
			return true
		}
	}
	return false
}

// symrefHeadPrefix is the marker `ls-remote --symref` puts on the symbolic-ref
// line it emits before the ordinary sha/HEAD line.
const symrefHeadPrefix = "ref: refs/heads/"

// parseSymrefHead extracts the branch name from `git ls-remote --symref origin
// HEAD` output, whose first line reads "ref: refs/heads/main\tHEAD". It scans
// for the marker line rather than assuming a line index — git prepends
// warnings and progress chatter to this output under several ordinary
// configurations, and an index-based read would silently pick one of those up
// and hand back a "branch" that is really a warning message.
func parseSymrefHead(out string) (string, bool) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, symrefHeadPrefix) {
			continue
		}
		branch := strings.TrimSpace(strings.SplitN(strings.TrimPrefix(line, symrefHeadPrefix), "\t", 2)[0])
		if branch != "" {
			return branch, true
		}
	}
	return "", false
}

// BranchName builds the branch (and directory suffix) for a worktree created
// at now: "wt-<YYYYMMDD-HHMMSS>". Exported so callers can display or predict
// the name without re-deriving the convention — one definition, not two.
func BranchName(now time.Time) string {
	return "wt-" + now.Format(nameTimestampLayout)
}

// SiblingPath places the worktree directory NEXT TO the repo root, named
// "<repo>-<branch>" — the "../<repo>-wt-<stamp>" half of the convention.
//
// A sibling, never a child: a worktree nested inside its own repo shows up in
// that repo's status as an untracked directory, gets swept by `git clean`, and
// is walked by every tool that scans the tree — including this project's own
// scanner. Placing it outside keeps the two checkouts fully independent, which
// is the entire point of asking for one.
func SiblingPath(root, branch string) string {
	return filepath.Join(filepath.Dir(root), filepath.Base(root)+"-"+branch)
}
