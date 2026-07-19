package control

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// stubITerm2Spawn pins the window-creating osascript seam and captures the
// argv it was called with.
func stubITerm2Spawn(t *testing.T, out string, err error) *[]string {
	t.Helper()
	original := iterm2SpawnFn
	t.Cleanup(func() { iterm2SpawnFn = original })
	var gotArgv []string
	iterm2SpawnFn = func(argv []string) (string, error) {
		gotArgv = argv
		return out, err
	}
	return &gotArgv
}

// stubITerm2Send pins the goal-delivery seam (the Tier 1h send path Spawn
// reuses) and captures its argv.
func stubITerm2Send(t *testing.T, out string, err error) *[]string {
	t.Helper()
	original := iterm2SendFn
	t.Cleanup(func() { iterm2SendFn = original })
	var gotArgv []string
	iterm2SendFn = func(argv []string) (string, error) {
		gotArgv = argv
		return out, err
	}
	return &gotArgv
}

// skipBootWait removes the 8s TUI-boot sleep for tests.
func skipBootWait(t *testing.T) {
	t.Helper()
	original := iterm2BootWaitFn
	t.Cleanup(func() { iterm2BootWaitFn = original })
	iterm2BootWaitFn = func() {}
}

func pinHostDetect(t *testing.T, inITerm2 bool) {
	t.Helper()
	original := iterm2HostDetectFn
	t.Cleanup(func() { iterm2HostDetectFn = original })
	iterm2HostDetectFn = func() bool { return inITerm2 }
}

const spawnTestGUID = "3AB4A804-D806-416E-854A-EAC59936774D"

func goodSpawnOutput() string { return spawnTestGUID + "\t/dev/ttys001\n" }

// ── the script itself: injection surface ─────────────────────────────────

// The strongest guarantee available on this surface: the spawn script is a
// compile-time CONSTANT. There is no function that builds it, so there is no
// place for a future refactor to introduce interpolation. iterm2SendScript has
// to interpolate a validated GUID because it must FIND an existing session;
// this one holds a direct reference to the window it just created and needs
// nothing.
func TestITerm2SpawnScript_ContainsNoInterpolationPoints(t *testing.T) {
	// The two values a caller could conceivably want to inject.
	for _, forbidden := range []string{"cd ", "claude", "/repo", "exec "} {
		if strings.Contains(iterm2SpawnScript, forbidden) {
			t.Fatalf("spawn script contains %q — the launch line must travel as argv, never as script source", forbidden)
		}
	}
	if !strings.Contains(iterm2SpawnScript, "item 1 of argv") {
		t.Fatal("spawn script does not read its payload from argv")
	}
}

// The byte-identity discipline hostsend.go established
// (TestITerm2SendScript_ByteIdenticalAcrossPayloads) extended to spawn: the
// script text must not vary with ANY input. If this fails, someone has
// reintroduced interpolation.
func TestITerm2SpawnScript_ByteIdenticalAcrossPayloads(t *testing.T) {
	payloads := []struct{ cwd string }{
		{"/repo"},
		{`/tmp/a"; do shell script "touch /tmp/pwned`},
		{"/tmp/'quoted'"},
		{"/tmp/한글경로"},
		{"/tmp/" + strings.Repeat("x", 500)},
	}
	baseline := iterm2SpawnArgv(iterm2LaunchLine(payloads[0].cwd, []string{"claude"}))[2]
	for _, p := range payloads {
		got := iterm2SpawnArgv(iterm2LaunchLine(p.cwd, []string{"claude"}))[2]
		if got != baseline {
			t.Fatalf("script text changed for cwd %q — interpolation has been reintroduced", p.cwd)
		}
	}
}

// The payload must reach osascript in an ARGUMENT slot, after the "--"
// end-of-options marker.
func TestITerm2SpawnArgv_PayloadTravelsAsArgvAfterDoubleDash(t *testing.T) {
	argv := iterm2SpawnArgv("cd '/repo' && exec claude || exit 1")

	want := []string{"osascript", "-e", iterm2SpawnScript, "--", "cd '/repo' && exec claude || exit 1"}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("argv = %#v\nwant %#v", argv, want)
	}
}

// The script must not walk windows→tabs→sessions — it holds a direct
// reference, so the iterate-and-mutate hazard focus.go documents cannot arise.
func TestITerm2SpawnScript_DoesNotTraverseSessionCollections(t *testing.T) {
	if strings.Contains(iterm2SpawnScript, "repeat with") {
		t.Fatal("spawn script traverses a collection; it should hold the new window directly")
	}
}

// ── the launch line ──────────────────────────────────────────────────────

func TestITerm2LaunchLine_Shape(t *testing.T) {
	got := iterm2LaunchLine("/repo", []string{"claude", "--agent", "team"})

	want := "cd /repo && exec claude --agent team || exit 1"
	if got != want {
		t.Fatalf("launch line = %q, want %q", got, want)
	}
}

// The false-success guard: without `|| exit 1` a failed cd or a missing claude
// leaves a live bare shell, and the goal would be typed into a shell prompt
// and reported as a spawned loop.
func TestITerm2LaunchLine_ClosesTheWindowOnFailure(t *testing.T) {
	got := iterm2LaunchLine("/repo", []string{"claude"})

	if !strings.HasSuffix(got, "|| exit 1") {
		t.Fatalf("launch line %q lacks the || exit 1 false-success guard", got)
	}
}

// exec replaces the shell, so the session's process IS claude — matching what
// `tmux new-window claude` already does.
func TestITerm2LaunchLine_ExecsSoTheProcessIsClaude(t *testing.T) {
	if !strings.Contains(iterm2LaunchLine("/repo", []string{"claude"}), "exec claude") {
		t.Fatal("launch line does not exec — the shell would remain the session's process")
	}
}

// cwd is shell-quoted: it reaches a shell, and a path with a space or a
// metacharacter must not become syntax.
func TestITerm2LaunchLine_QuotesAwkwardCwd(t *testing.T) {
	got := iterm2LaunchLine("/tmp/my repo", []string{"claude"})

	if !strings.Contains(got, `cd '/tmp/my repo'`) {
		t.Fatalf("launch line %q did not quote a cwd containing a space", got)
	}
}

func TestITerm2LaunchLine_NeutralizesCwdMetacharacters(t *testing.T) {
	got := iterm2LaunchLine("/tmp/a; touch /tmp/pwned", []string{"claude"})

	if !strings.HasPrefix(got, "cd '") {
		t.Fatalf("launch line %q did not quote a hostile cwd", got)
	}
	if strings.HasPrefix(got, "cd /tmp/a; touch") {
		t.Fatalf("launch line %q lets the cwd inject a second command", got)
	}
}

func TestITerm2LaunchLine_UsesTheConfiguredSpawnCommand(t *testing.T) {
	got := iterm2LaunchLine("/repo", teamArgv())

	if !strings.Contains(got, "exec claude --agent team --dangerously-skip-permissions") {
		t.Fatalf("launch line %q does not carry the configured spawn command", got)
	}
}

// ── parsing the script's reply ───────────────────────────────────────────

func TestParseITerm2SpawnResult_ValidReply(t *testing.T) {
	session, ok := parseITerm2SpawnResult(goodSpawnOutput())

	if !ok {
		t.Fatal("a valid GUID/tty reply was rejected")
	}
	if session.guid.String() != spawnTestGUID {
		t.Fatalf("guid = %q, want %q", session.guid.String(), spawnTestGUID)
	}
	if session.tty != "ttys001" {
		t.Fatalf("tty = %q, want the normalized ttys001", session.tty)
	}
}

// Every one of these must FAIL CLOSED. osascript exits 0 in situations where
// no usable window exists, so unrecognized output can never be success.
func TestParseITerm2SpawnResult_RejectsUnusableReplies(t *testing.T) {
	cases := []struct {
		name string
		out  string
	}{
		{"empty", ""},
		{"whitespace only", "   \n"},
		{"guid only, no tty", spawnTestGUID},
		{"tty only, no guid", "/dev/ttys001"},
		{"too many fields", spawnTestGUID + "\t/dev/ttys001\textra"},
		{"empty guid", "\t/dev/ttys001"},
		{"empty tty", spawnTestGUID + "\t"},
		{"guid fails the whitelist", "not a guid!\t/dev/ttys001"},
		{"guid with a quote", `abc"def` + "\t/dev/ttys001"},
		{"tty fails the whitelist", spawnTestGUID + "\t/dev/tty s001"},
		{"uppercase tty", spawnTestGUID + "\t/dev/TTYS001"},
		{"applescript error text", "execution error: iTerm2 got an error (-1743)"},
		{"space separated instead of tab", spawnTestGUID + " /dev/ttys001"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := parseITerm2SpawnResult(tc.out); ok {
				t.Fatalf("accepted unusable reply %q", tc.out)
			}
		})
	}
}

// ── Spawn: success ───────────────────────────────────────────────────────

func TestITerm2Spawn_CreatesWindowThenDeliversTheGoal(t *testing.T) {
	skipBootWait(t)
	pinSpawnCommand(t, []string{"claude"})
	spawnArgv := stubITerm2Spawn(t, goodSpawnOutput(), nil)
	sendArgv := stubITerm2Send(t, iterm2SendHit, nil)

	if err := (iterm2Spawner{}).Spawn("/repo", "고쳐줘"); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	if len(*spawnArgv) == 0 {
		t.Fatal("no window was created")
	}
	if (*spawnArgv)[len(*spawnArgv)-1] != "cd /repo && exec claude || exit 1" {
		t.Fatalf("launch line = %q", (*spawnArgv)[len(*spawnArgv)-1])
	}
	if len(*sendArgv) == 0 {
		t.Fatal("the goal was never delivered")
	}
	if (*sendArgv)[len(*sendArgv)-1] != "고쳐줘" {
		t.Fatalf("delivered %q, want the goal", (*sendArgv)[len(*sendArgv)-1])
	}
}

// The goal is delivered against the session we just created — addressed by its
// GUID and bound to its tty.
func TestITerm2Spawn_DeliversAgainstTheNewSessionsGUIDAndTTY(t *testing.T) {
	skipBootWait(t)
	pinSpawnCommand(t, []string{"claude"})
	stubITerm2Spawn(t, goodSpawnOutput(), nil)
	sendArgv := stubITerm2Send(t, iterm2SendHit, nil)

	if err := (iterm2Spawner{}).Spawn("/repo", "go"); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	script := (*sendArgv)[2]
	if !strings.Contains(script, spawnTestGUID) {
		t.Fatalf("send script does not address the new session's GUID:\n%s", script)
	}
	if !containsArg(*sendArgv, "/dev/ttys001") {
		t.Fatalf("send argv does not bind to the new session's tty: %#v", *sendArgv)
	}
}

func TestITerm2Spawn_UsesTheConfiguredSpawnCommand(t *testing.T) {
	skipBootWait(t)
	pinSpawnCommand(t, teamArgv())
	spawnArgv := stubITerm2Spawn(t, goodSpawnOutput(), nil)
	stubITerm2Send(t, iterm2SendHit, nil)

	if err := (iterm2Spawner{}).Spawn("/repo", "go"); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	if !strings.Contains((*spawnArgv)[len(*spawnArgv)-1], "--dangerously-skip-permissions") {
		t.Fatalf("launch line %q ignored the configured spawn command", (*spawnArgv)[len(*spawnArgv)-1])
	}
}

// ── Spawn: failure ───────────────────────────────────────────────────────

// osascript exited 0 but returned nothing usable — no window. Must fail, and
// must not attempt a delivery.
func TestITerm2Spawn_UnusableReplyFailsAndNeverDelivers(t *testing.T) {
	skipBootWait(t)
	pinSpawnCommand(t, []string{"claude"})
	stubITerm2Spawn(t, "", nil)
	sendArgv := stubITerm2Send(t, iterm2SendHit, nil)

	err := (iterm2Spawner{}).Spawn("/repo", "go")

	if !errors.Is(err, ErrITerm2SpawnNoSession) {
		t.Fatalf("err = %v, want ErrITerm2SpawnNoSession", err)
	}
	if len(*sendArgv) != 0 {
		t.Fatal("a goal was delivered even though no session was created")
	}
}

func TestITerm2Spawn_OsascriptFailureIsReported(t *testing.T) {
	skipBootWait(t)
	pinSpawnCommand(t, []string{"claude"})
	stubITerm2Spawn(t, "", errors.New("Not authorized to send Apple events to iTerm2. (-1743)"))
	sendArgv := stubITerm2Send(t, iterm2SendHit, nil)

	err := (iterm2Spawner{}).Spawn("/repo", "go")

	if err == nil {
		t.Fatal("Spawn reported success despite osascript failing")
	}
	if !strings.Contains(err.Error(), "-1743") {
		t.Fatalf("err %v does not carry osascript's own diagnosis", err)
	}
	if len(*sendArgv) != 0 {
		t.Fatal("a goal was delivered even though the window was never created")
	}
}

// A killed osascript may have created a window we can no longer see — that is
// UNKNOWN, not a clean failure, and must stay distinguishable so nobody
// retries blindly.
func TestITerm2Spawn_TimeoutIsSurfacedAsUnknownNotFailure(t *testing.T) {
	skipBootWait(t)
	pinSpawnCommand(t, []string{"claude"})
	stubITerm2Spawn(t, "", ErrSendDeliveryUnknown)
	stubITerm2Send(t, iterm2SendHit, nil)

	err := (iterm2Spawner{}).Spawn("/repo", "go")

	if !errors.Is(err, ErrSendDeliveryUnknown) {
		t.Fatalf("err = %v, want it to preserve ErrSendDeliveryUnknown", err)
	}
}

// The window came up but died before the goal landed (a missing claude binary
// closing it via `|| exit 1`). The send returns "miss" — so the delivery IS
// the liveness check, and this must be reported as a failure.
func TestITerm2Spawn_WindowDiedBeforeDeliveryIsAFailure(t *testing.T) {
	skipBootWait(t)
	pinSpawnCommand(t, []string{"claude"})
	stubITerm2Spawn(t, goodSpawnOutput(), nil)
	stubITerm2Send(t, iterm2SendMiss, nil)

	err := (iterm2Spawner{}).Spawn("/repo", "go")

	if err == nil {
		t.Fatal("Spawn reported success though the session had vanished by delivery time")
	}
	if !errors.Is(err, ErrSendNoSession) {
		t.Fatalf("err = %v, want it to preserve ErrSendNoSession", err)
	}
	if !strings.Contains(err.Error(), "ttys001") {
		t.Fatalf("err %v does not name the session it tried", err)
	}
}

func TestITerm2Spawn_TTYMismatchAtDeliveryIsAFailure(t *testing.T) {
	skipBootWait(t)
	pinSpawnCommand(t, []string{"claude"})
	stubITerm2Spawn(t, goodSpawnOutput(), nil)
	stubITerm2Send(t, iterm2SendTTYMismatch, nil)

	if err := (iterm2Spawner{}).Spawn("/repo", "go"); !errors.Is(err, ErrSendTTYMismatch) {
		t.Fatalf("err = %v, want ErrSendTTYMismatch", err)
	}
}

// An unrecognized verdict must never read as success.
func TestITerm2Spawn_UnrecognizedDeliveryVerdictIsAFailure(t *testing.T) {
	skipBootWait(t)
	pinSpawnCommand(t, []string{"claude"})
	stubITerm2Spawn(t, goodSpawnOutput(), nil)
	stubITerm2Send(t, "something unexpected", nil)

	if err := (iterm2Spawner{}).Spawn("/repo", "go"); !errors.Is(err, ErrSendUnrecognizedVerdict) {
		t.Fatalf("err = %v, want ErrSendUnrecognizedVerdict", err)
	}
}

// ── availability and resolution ──────────────────────────────────────────

func TestITerm2Spawner_AvailableOnlyInsideITerm2(t *testing.T) {
	pinHostDetect(t, true)
	if !(iterm2Spawner{}).Available() {
		t.Fatal("not available while running inside iTerm2")
	}
	pinHostDetect(t, false)
	if (iterm2Spawner{}).Available() {
		t.Fatal("claimed availability while NOT running inside iTerm2 — a spawn surface must never be claimed without evidence")
	}
}

// The pure-superset property: iTerm2 must not be in the backend list that
// feeds actuation dispatch, or it would join Tier 1b's cwd probe and its
// ambiguity counting.
func TestITerm2Spawner_IsNotInTheActuationBackendList(t *testing.T) {
	for _, c := range backends {
		if c.Name() == "iterm2" {
			t.Fatal("iterm2 is in `backends` — that changes Tier 1b actuation dispatch; spawn must stay a separate seam")
		}
	}
}

// A machine with no multiplexer but running inside iTerm2 can now spawn — the
// whole point of this stage.
func TestResolveSpawner_FallsBackToITerm2WithNoMultiplexer(t *testing.T) {
	pinNoBackends(t)
	pinHostDetect(t, true)

	spawner, ok := ResolveSpawner()
	if !ok {
		t.Fatal("no spawner resolved on an iTerm2-only machine")
	}
	if spawner.Name() != "iterm2" {
		t.Fatalf("resolved %q, want iterm2", spawner.Name())
	}
}

func TestResolveSpawner_NothingAvailableAtAll(t *testing.T) {
	pinNoBackends(t)
	pinHostDetect(t, false)

	if _, ok := ResolveSpawner(); ok {
		t.Fatal("a spawner resolved with no multiplexer and no iTerm2")
	}
}

// Multiplexers keep strict priority: a loop spawned into one gets pane-exact
// Tier 1a addressing, which a bare iTerm2 window cannot offer.
func TestResolveSpawner_PrefersAnAvailableMultiplexer(t *testing.T) {
	original := backends
	t.Cleanup(func() { backends = original })
	backends = []Controller{stubAvailableController{name: "tmux"}}
	pinHostDetect(t, true)

	spawner, ok := ResolveSpawner()
	if !ok {
		t.Fatal("no spawner resolved")
	}
	if spawner.Name() != "tmux" {
		t.Fatalf("resolved %q, want the multiplexer tmux to win over iterm2", spawner.Name())
	}
}

// pinNoBackends empties the multiplexer list so resolution reaches the host
// spawner without needing a real orca/cmux/tmux absent from the machine.
func pinNoBackends(t *testing.T) {
	t.Helper()
	original := backends
	t.Cleanup(func() { backends = original })
	backends = nil
}

// stubAvailableController is an always-available Controller for resolution
// ordering tests.
type stubAvailableController struct{ name string }

func (s stubAvailableController) Name() string                     { return s.name }
func (stubAvailableController) Available() bool                    { return true }
func (stubAvailableController) Locate(string) (Target, bool)       { return Target{}, false }
func (stubAvailableController) LocateClaude(string) (Target, bool) { return Target{}, false }
func (stubAvailableController) Resume(Target, string) error        { return nil }
func (stubAvailableController) Focus(Target) error                 { return nil }
func (stubAvailableController) Approve(Target) error               { return nil }
func (stubAvailableController) Spawn(string, string) error         { return nil }
func (stubAvailableController) Interrupt(Target) error             { return nil }

// Creating a window launches a profile AND a login shell; the keystroke-sized
// actuation budget under-serves it, and under-budgeting here is not a harmless
// retry — a deadline kill is classified ErrSendDeliveryUnknown precisely
// because the window may exist, so too short a timeout manufactures orphan
// windows out of slow-but-fine machines.
func TestITerm2SpawnTimeout_IsLargerThanTheKeystrokeBudget(t *testing.T) {
	if iterm2SpawnTimeout <= actuationTimeout {
		t.Errorf("iterm2SpawnTimeout = %v, want > actuationTimeout (%v) — window creation is not a keystroke send",
			iterm2SpawnTimeout, actuationTimeout)
	}
}
