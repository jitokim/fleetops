package control

import (
	"errors"
	"reflect"
	"testing"
)

// --- cmux tab-rename argv shape (feat/cmux-tab-rename) ---
//
// The title is positional AFTER the `--` terminator (a title that begins with a
// dash must never be read as a flag), and v1 passes NO --window (same-workspace
// only), unlike the other cmux actuation builders.

func TestCmuxRenameTabCmd_TitleIsPositionalAfterTerminator(t *testing.T) {
	got := cmuxRenameTabCmd("surface:2", "◆GATE auth-mw")
	want := []string{"cmux", "rename-tab", "--surface", "surface:2", "--", "◆GATE auth-mw"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestCmuxRenameTabCmd_DashLeadingTitle_NotParsedAsFlag(t *testing.T) {
	// A title starting with "-" must sit after "--" so cmux treats it as the
	// name, not an option.
	got := cmuxRenameTabCmd("surface:9", "-idle")
	want := []string{"cmux", "rename-tab", "--surface", "surface:9", "--", "-idle"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	// The "--" terminator must precede the title.
	if got[len(got)-2] != "--" {
		t.Errorf("expected %q before the title, got argv %v", "--", got)
	}
}

// --- SetTabTitle routes through the injectable cmuxTabRunner seam ---

// withCmuxTabRunner swaps the exec seam for one test, restoring it after.
func withCmuxTabRunner(t *testing.T, fn func([]string) error) {
	t.Helper()
	orig := cmuxTabRunner
	t.Cleanup(func() { cmuxTabRunner = orig })
	cmuxTabRunner = fn
}

func TestCmuxSetTabTitle_PassesExactArgvThroughSeam(t *testing.T) {
	var gotArgv []string
	withCmuxTabRunner(t, func(argv []string) error {
		gotArgv = argv
		return nil
	})

	err := cmuxController{}.SetTabTitle(Target{Backend: "cmux", ID: "surface:3"}, "⏸STALL rate-limit fx")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []string{"cmux", "rename-tab", "--surface", "surface:3", "--", "⏸STALL rate-limit fx"}
	if !reflect.DeepEqual(gotArgv, want) {
		t.Errorf("runner received %v, want %v", gotArgv, want)
	}
}

func TestCmuxSetTabTitle_PropagatesRunnerError(t *testing.T) {
	// A bounded-exec failure/timeout (the runner returning an error) must
	// propagate so the caller degrades to "didn't rename" rather than assuming
	// success.
	sentinel := errors.New("cmux wedged / timed out")
	withCmuxTabRunner(t, func([]string) error { return sentinel })

	err := cmuxController{}.SetTabTitle(Target{Backend: "cmux", ID: "surface:3"}, "◆GATE x")
	if !errors.Is(err, sentinel) {
		t.Errorf("SetTabTitle error = %v, want it to wrap/return %v", err, sentinel)
	}
}

// TestCmuxSetTabTitle_DefaultSeamIsBoundedExec pins that the PRODUCTION seam is
// the shared bounded-exec path (runWithTimeout), the same never-hang discipline
// every other cmux actuation uses — so a wedged cmux cannot hang a scan tick.
// The func identity is what guarantees the bound; runWithTimeout's own kill
// behavior is covered by TestRunBounded_KillsAHangingCommand.
func TestCmuxSetTabTitle_DefaultSeamIsBoundedExec(t *testing.T) {
	if reflect.ValueOf(cmuxTabRunner).Pointer() != reflect.ValueOf(runWithTimeout).Pointer() {
		t.Error("cmuxTabRunner must default to runWithTimeout (bounded exec) so a wedged cmux never hangs a tick")
	}
}
