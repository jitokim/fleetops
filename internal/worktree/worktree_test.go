package worktree

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ── pure helpers ─────────────────────────────────────────────────────────

func TestBranchName_IsTimestampConvention(t *testing.T) {
	at := time.Date(2026, 7, 19, 1, 16, 12, 0, time.UTC)
	if got := BranchName(at); got != "wt-20260719-011612" {
		t.Fatalf("BranchName = %q, want wt-20260719-011612", got)
	}
}

// The naming convention must be language-independent — this is the whole
// reason it is a timestamp and not a goal slug (see the package doc). Two
// Korean goals that slug identically under internal/tui's slugFromGoal must
// still produce distinct worktrees, which they trivially do here because the
// goal never enters the name at all.
func TestBranchName_DoesNotDependOnGoalText(t *testing.T) {
	first := BranchName(time.Date(2026, 7, 19, 1, 16, 12, 0, time.UTC))
	second := BranchName(time.Date(2026, 7, 19, 1, 16, 13, 0, time.UTC))
	if first == second {
		t.Fatalf("distinct instants produced the same branch name %q", first)
	}
}

func TestSiblingPath_PlacesWorktreeNextToRepoRoot(t *testing.T) {
	got := SiblingPath("/home/u/proj/fleetops", "wt-20260719-011612")
	want := "/home/u/proj/fleetops-wt-20260719-011612"
	if got != want {
		t.Fatalf("SiblingPath = %q, want %q", got, want)
	}
}

// A worktree nested inside its own repo is the failure this guards (see
// SiblingPath's doc): it would be swept by `git clean` and walked by every
// tree scanner, including this project's own.
func TestSiblingPath_IsNotInsideTheRepo(t *testing.T) {
	root := "/home/u/proj/fleetops"
	got := SiblingPath(root, "wt-20260719-011612")
	if strings.HasPrefix(got, root+string(filepath.Separator)) {
		t.Fatalf("SiblingPath %q is nested inside the repo root %q", got, root)
	}
}

func TestParseSymrefHead_ExtractsBranch(t *testing.T) {
	out := "ref: refs/heads/main\tHEAD\nabc123\tHEAD"
	branch, ok := parseSymrefHead(out)
	if !ok || branch != "main" {
		t.Fatalf("parseSymrefHead = (%q, %v), want (main, true)", branch, ok)
	}
}

// The default branch is resolved, never assumed to be "main" — a repo whose
// default is "master" (or anything else) must resolve to its own branch.
func TestParseSymrefHead_NonMainDefaultBranch(t *testing.T) {
	branch, ok := parseSymrefHead("ref: refs/heads/master\tHEAD")
	if !ok || branch != "master" {
		t.Fatalf("parseSymrefHead = (%q, %v), want (master, true)", branch, ok)
	}
}

// git prepends warnings/progress chatter to this output under several ordinary
// configurations; an index-based read would hand back the warning text as if
// it were a branch name.
func TestParseSymrefHead_SkipsLeadingChatter(t *testing.T) {
	out := "warning: redirecting to https://example.test/repo\nref: refs/heads/trunk\tHEAD"
	branch, ok := parseSymrefHead(out)
	if !ok || branch != "trunk" {
		t.Fatalf("parseSymrefHead = (%q, %v), want (trunk, true)", branch, ok)
	}
}

func TestParseSymrefHead_NoMarkerLine(t *testing.T) {
	if _, ok := parseSymrefHead("abc123\tHEAD"); ok {
		t.Fatal("parseSymrefHead accepted output with no symref marker")
	}
}

func TestParseSymrefHead_Empty(t *testing.T) {
	if _, ok := parseSymrefHead(""); ok {
		t.Fatal("parseSymrefHead accepted empty output")
	}
}

func TestParseSymrefHead_MarkerWithEmptyBranch(t *testing.T) {
	if _, ok := parseSymrefHead("ref: refs/heads/\tHEAD"); ok {
		t.Fatal("parseSymrefHead accepted a marker line with no branch name")
	}
}

// ── real-git fixtures ────────────────────────────────────────────────────
//
// These exercise the REAL git binary against a REAL repo in a temp dir, with
// a local bare repo standing in for "origin". Nothing touches the network and
// nothing touches the developer's own repos. A seam fake would let this
// package's central claim — that the branch is cut from origin's default
// branch and NOT from local HEAD — pass while being false of real git, so the
// success paths are deliberately not faked.

// isolateGitEnv points git at a scratch HOME and disables system/global
// config, so a developer's own git configuration (a global hooksPath,
// init.defaultBranch, commit.gpgsign, …) can neither leak into these tests nor
// make them pass or fail for reasons unrelated to the code under test. Applies
// to the production gitFn too, since it inherits the test process's env.
func isolateGitEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_AUTHOR_NAME", "fleetops test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@fleetops.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "fleetops test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@fleetops.invalid")
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newRepoWithOrigin builds a clone whose origin is a local bare repo whose
// default branch is defaultBranch. defaultBranch is a PARAMETER, and the tests
// pass "trunk": if anything in this package quietly assumed "main", a
// main-named fixture would hide it.
func newRepoWithOrigin(t *testing.T, defaultBranch string) (clone, originPath string) {
	t.Helper()
	base := t.TempDir()
	originPath = filepath.Join(base, "origin.git")
	seed := filepath.Join(base, "seed")

	if out, err := exec.Command("git", "init", "--bare", "-b", defaultBranch, originPath).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "init", "-b", defaultBranch, seed).CombinedOutput(); err != nil {
		t.Fatalf("git init seed: %v\n%s", err, out)
	}
	writeFile(t, filepath.Join(seed, "README.md"), "seed\n")
	git(t, seed, "add", ".")
	git(t, seed, "commit", "-m", "seed")
	git(t, seed, "remote", "add", "origin", originPath)
	git(t, seed, "push", "origin", defaultBranch)

	clone = filepath.Join(base, "work")
	if out, err := exec.Command("git", "clone", originPath, clone).CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v\n%s", err, out)
	}
	return clone, originPath
}

// repoRootOf reports the repo root as GIT itself resolves it. On macOS
// t.TempDir() hands back a path under /var, which is a symlink to
// /private/var, and `rev-parse --show-toplevel` returns the resolved form —
// so a test that compared against the raw t.TempDir() path would fail on a
// difference the production code is right about.
func repoRootOf(t *testing.T, dir string) string {
	t.Helper()
	return git(t, dir, "rev-parse", "--show-toplevel")
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// ── Create: success ──────────────────────────────────────────────────────

func TestCreate_CreatesWorktreeAtSiblingPathOnResolvedBase(t *testing.T) {
	isolateGitEnv(t)
	clone, _ := newRepoWithOrigin(t, "trunk")

	got, err := Create(clone)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got.Base != "origin/trunk" {
		t.Fatalf("Base = %q, want origin/trunk (the default branch must be resolved, not assumed to be main)", got.Base)
	}
	if !strings.HasPrefix(got.Branch, "wt-") {
		t.Fatalf("Branch = %q, want a wt-<timestamp> name", got.Branch)
	}
	if want := SiblingPath(repoRootOf(t, clone), got.Branch); got.Path != want {
		t.Fatalf("Path = %q, want %q", got.Path, want)
	}
	if st, err := os.Stat(got.Path); err != nil || !st.IsDir() {
		t.Fatalf("worktree dir %q was not created: %v", got.Path, err)
	}
	if branch := git(t, got.Path, "rev-parse", "--abbrev-ref", "HEAD"); branch != got.Branch {
		t.Fatalf("worktree is on branch %q, want %q", branch, got.Branch)
	}
}

// The load-bearing assertion of this whole package: the new branch is cut from
// origin/<default>, NOT from whatever the local checkout happens to have
// checked out. This is the PR #48 failure — a branch cut from a stale base —
// reproduced as a test. Without the explicit base argument, `git worktree add`
// would use local HEAD and this test would fail.
func TestCreate_BranchesFromOriginNotStaleLocalHead(t *testing.T) {
	isolateGitEnv(t)
	clone, _ := newRepoWithOrigin(t, "trunk")

	// Diverge the local checkout: a commit that exists ONLY locally.
	writeFile(t, filepath.Join(clone, "local-only.txt"), "not on origin\n")
	git(t, clone, "add", ".")
	git(t, clone, "commit", "-m", "local only commit")

	originHead := git(t, clone, "rev-parse", "origin/trunk")
	localHead := git(t, clone, "rev-parse", "HEAD")
	if originHead == localHead {
		t.Fatal("fixture is wrong: local HEAD did not diverge from origin/trunk")
	}

	got, err := Create(clone)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	worktreeHead := git(t, got.Path, "rev-parse", "HEAD")
	if worktreeHead != originHead {
		t.Fatalf("worktree HEAD = %s, want origin/trunk %s (it was cut from local HEAD %s — the stale-base bug)",
			worktreeHead, originHead, localHead)
	}
	if _, err := os.Stat(filepath.Join(got.Path, "local-only.txt")); err == nil {
		t.Fatal("worktree contains the local-only commit's file — it was branched from local HEAD, not origin")
	}
}

// A dirty working tree must NOT block worktree creation. Leaving the dirty
// checkout untouched while working elsewhere is the entire point of a
// worktree, and `git worktree add` is designed to permit it. Asserted
// explicitly so nobody later "hardens" this into a refusal.
func TestCreate_DirtyRepoStillSucceeds(t *testing.T) {
	isolateGitEnv(t)
	clone, _ := newRepoWithOrigin(t, "trunk")

	writeFile(t, filepath.Join(clone, "README.md"), "uncommitted edit\n")
	writeFile(t, filepath.Join(clone, "untracked.txt"), "untracked\n")

	got, err := Create(clone)
	if err != nil {
		t.Fatalf("Create on a dirty repo: %v (a dirty tree must not block worktree creation)", err)
	}
	if st, err := os.Stat(got.Path); err != nil || !st.IsDir() {
		t.Fatalf("worktree dir %q was not created: %v", got.Path, err)
	}
	// The dirty state stays where it was — untouched, not carried over.
	if dirty := git(t, clone, "status", "--porcelain"); dirty == "" {
		t.Fatal("the original checkout's dirty state was cleaned as a side effect")
	}
}

// A subdirectory of the repo must resolve to the same repo root, so the
// worktree still lands beside the ROOT rather than beside the subdirectory.
func TestCreate_FromSubdirectoryUsesRepoRoot(t *testing.T) {
	isolateGitEnv(t)
	clone, _ := newRepoWithOrigin(t, "trunk")
	sub := filepath.Join(clone, "internal", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	got, err := Create(sub)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if want := SiblingPath(repoRootOf(t, clone), got.Branch); got.Path != want {
		t.Fatalf("Path = %q, want %q (sibling of the repo ROOT, not of the subdirectory)", got.Path, want)
	}
}

// origin exists but its local origin/HEAD was never set (a plain init +
// remote add + fetch, no clone) — the ls-remote fallback must resolve it.
func TestCreate_ResolvesBaseViaLsRemoteWhenOriginHeadUnset(t *testing.T) {
	isolateGitEnv(t)
	clone, _ := newRepoWithOrigin(t, "trunk")

	// Clone sets refs/remotes/origin/HEAD; delete it to force the fallback.
	git(t, clone, "remote", "set-head", "origin", "--delete")

	got, err := Create(clone)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got.Base != "origin/trunk" {
		t.Fatalf("Base = %q, want origin/trunk via the ls-remote fallback", got.Base)
	}
}

// ── Create: failure ──────────────────────────────────────────────────────

func TestCreate_NotARepo(t *testing.T) {
	isolateGitEnv(t)
	dir := t.TempDir()

	_, err := Create(dir)
	if !errors.Is(err, ErrNotARepo) {
		t.Fatalf("Create in a non-repo = %v, want ErrNotARepo", err)
	}
}

func TestCreate_MissingDirectory(t *testing.T) {
	isolateGitEnv(t)

	_, err := Create(filepath.Join(t.TempDir(), "does-not-exist"))
	if !errors.Is(err, ErrNotARepo) {
		t.Fatalf("Create on a missing dir = %v, want ErrNotARepo", err)
	}
}

// A repo with no origin REFUSES rather than silently falling back to HEAD —
// the fallback would reintroduce exactly the implicit-stale-base behaviour the
// explicit base exists to prevent (see ErrNoRemote's doc).
func TestCreate_NoOriginRemoteRefuses(t *testing.T) {
	isolateGitEnv(t)
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-b", "trunk", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	writeFile(t, filepath.Join(dir, "f.txt"), "x\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "only commit")

	_, err := Create(dir)
	if !errors.Is(err, ErrNoRemote) {
		t.Fatalf("Create with no origin = %v, want ErrNoRemote", err)
	}
}

// A remote named "origin-mirror" is not origin. A substring match would treat
// it as one and resolve the base against the wrong repository.
func TestCreate_SimilarlyNamedRemoteIsNotOrigin(t *testing.T) {
	isolateGitEnv(t)
	clone, originPath := newRepoWithOrigin(t, "trunk")
	git(t, clone, "remote", "rename", "origin", "origin-mirror")
	_ = originPath

	_, err := Create(clone)
	if !errors.Is(err, ErrNoRemote) {
		t.Fatalf("Create with only an 'origin-mirror' remote = %v, want ErrNoRemote", err)
	}
}

// Two spawns inside the same clock second collide on the timestamped path.
// Checked before git runs, so the error names the path.
func TestCreate_PathCollisionRefuses(t *testing.T) {
	isolateGitEnv(t)
	clone, _ := newRepoWithOrigin(t, "trunk")
	at := time.Date(2026, 7, 19, 1, 16, 12, 0, time.UTC)

	first, err := create(clone, at)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err = create(clone, at)
	if !errors.Is(err, ErrPathExists) {
		t.Fatalf("second create at the same instant = %v, want ErrPathExists", err)
	}
	if !strings.Contains(fmt.Sprint(err), first.Path) {
		t.Fatalf("error %v does not name the colliding path %q", err, first.Path)
	}
}

// An occupied path that is not a real directory (a dangling symlink) is still
// occupied — Lstat, not Stat.
func TestCreate_DanglingSymlinkAtTargetRefuses(t *testing.T) {
	isolateGitEnv(t)
	clone, _ := newRepoWithOrigin(t, "trunk")
	at := time.Date(2026, 7, 19, 1, 16, 12, 0, time.UTC)

	target := SiblingPath(repoRootOf(t, clone), BranchName(at))
	if err := os.Symlink(filepath.Join(t.TempDir(), "nowhere"), target); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if _, err := create(clone, at); !errors.Is(err, ErrPathExists) {
		t.Fatalf("create over a dangling symlink = %v, want ErrPathExists", err)
	}
}

// ── seam-injected failures ───────────────────────────────────────────────
//
// These use the gitFn seam for states that cannot be staged with a real git:
// a remote that exists but resolves no default branch, and a `worktree add`
// that fails after every precondition passed.

// fakeGit replaces gitFn for the duration of a test with a table keyed by the
// first git argument, restoring the real one afterwards.
func fakeGit(t *testing.T, responses map[string]func(args []string) (string, error)) {
	t.Helper()
	original := gitFn
	t.Cleanup(func() { gitFn = original })
	gitFn = func(dir string, args ...string) (string, error) {
		if len(args) == 0 {
			return "", errors.New("fakeGit: no args")
		}
		fn, ok := responses[args[0]]
		if !ok {
			return "", fmt.Errorf("fakeGit: unexpected git %s", strings.Join(args, " "))
		}
		return fn(args)
	}
}

func TestCreate_OriginPresentButNoResolvableDefaultBranch(t *testing.T) {
	fakeGit(t, map[string]func([]string) (string, error){
		"rev-parse":    func([]string) (string, error) { return "/repo", nil },
		"remote":       func([]string) (string, error) { return "origin", nil },
		"symbolic-ref": func([]string) (string, error) { return "", errors.New("exit status 128") },
		// ls-remote succeeds but emits no symref marker line.
		"ls-remote": func([]string) (string, error) { return "abc123\tHEAD", nil },
	})

	_, err := Create("/repo")
	if !errors.Is(err, ErrNoDefaultBranch) {
		t.Fatalf("Create = %v, want ErrNoDefaultBranch", err)
	}
}

func TestCreate_LsRemoteFailureIsNoDefaultBranch(t *testing.T) {
	fakeGit(t, map[string]func([]string) (string, error){
		"rev-parse":    func([]string) (string, error) { return "/repo", nil },
		"remote":       func([]string) (string, error) { return "origin", nil },
		"symbolic-ref": func([]string) (string, error) { return "", errors.New("exit status 128") },
		"ls-remote":    func([]string) (string, error) { return "", errors.New("could not read from remote repository") },
	})

	_, err := Create("/repo")
	if !errors.Is(err, ErrNoDefaultBranch) {
		t.Fatalf("Create = %v, want ErrNoDefaultBranch", err)
	}
}

// `git worktree add` failing after every precondition passed must surface
// git's own message, not a bare exit status — that text is the diagnosis.
func TestCreate_WorktreeAddFailureSurfacesGitMessage(t *testing.T) {
	fakeGit(t, map[string]func([]string) (string, error){
		"rev-parse":    func([]string) (string, error) { return filepath.Join(t.TempDir(), "repo"), nil },
		"remote":       func([]string) (string, error) { return "origin", nil },
		"symbolic-ref": func([]string) (string, error) { return "origin/main", nil },
		"worktree":     func([]string) (string, error) { return "", errors.New("fatal: invalid reference: origin/main") },
	})

	_, err := Create("/repo")
	if err == nil {
		t.Fatal("Create succeeded despite git worktree add failing")
	}
	if !strings.Contains(err.Error(), "invalid reference") {
		t.Fatalf("error %v does not carry git's own message", err)
	}
}

// A repo-root probe that "succeeds" with empty output must not be treated as
// a valid root — joining onto "" would place the worktree at the filesystem
// root's neighbour.
func TestCreate_EmptyRepoRootIsNotARepo(t *testing.T) {
	fakeGit(t, map[string]func([]string) (string, error){
		"rev-parse": func([]string) (string, error) { return "", nil },
	})

	if _, err := Create("/repo"); !errors.Is(err, ErrNotARepo) {
		t.Fatalf("Create = %v, want ErrNotARepo", err)
	}
}
