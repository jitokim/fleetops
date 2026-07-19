package control

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"
)

// hangingSleepArgv is a real subprocess that would block for well longer than
// any timeout below is given — if runBounded/outputBounded's context deadline
// did not actually cut it off, these tests would take ~5s (or hang forever on
// a platform where "sleep" behaves oddly) instead of returning almost
// immediately. Skipped where "sleep" is not on PATH rather than faked,
// because the whole point is proving a REAL wedged process gets killed, not
// exercising a stub that was never at risk of hanging in the first place.
func hangingSleepArgv(t *testing.T) []string {
	t.Helper()
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not on PATH — cannot prove a real hang is bounded")
	}
	return []string{"sleep", "5"}
}

// maxAcceptableBoundedElapsed is how long runBounded/outputBounded are
// allowed to take against hangingSleepArgv's 5s sleep before a test concludes
// the deadline did not bite. Comfortably above the tiny timeout each test
// passes in, comfortably below the 5s the unbounded command would take.
const maxAcceptableBoundedElapsed = 2 * time.Second

func TestRunBounded_KillsAHangingCommand(t *testing.T) {
	argv := hangingSleepArgv(t)
	start := time.Now()
	err := runBounded(50*time.Millisecond, argv)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("want an error from a command killed at the deadline, got nil")
	}
	if elapsed >= maxAcceptableBoundedElapsed {
		t.Fatalf("runBounded took %v to return — the deadline did not bound the hanging command (an unbounded `sleep 5` would take ~5s)", elapsed)
	}
}

func TestOutputBounded_KillsAHangingCommand(t *testing.T) {
	argv := hangingSleepArgv(t)
	start := time.Now()
	_, err := outputBounded(50*time.Millisecond, argv)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("want an error from a command killed at the deadline, got nil")
	}
	if elapsed >= maxAcceptableBoundedElapsed {
		t.Fatalf("outputBounded took %v to return — the deadline did not bound the hanging command (an unbounded `sleep 5` would take ~5s)", elapsed)
	}
}

// TestRunWithTimeout_UsesActuationTimeoutBudget pins that runWithTimeout is
// exactly runBounded(actuationTimeout, ...) — a regression guard for the
// #76 refactor that pulled the exec-with-deadline logic out of runWithTimeout
// into the shared runBounded/outputBounded primitives. If a future edit
// changes runWithTimeout to use a different budget, this fails instead of
// silently shrinking or growing every Resume/Approve/Focus/Interrupt call's
// timeout.
func TestRunWithTimeout_UsesActuationTimeoutBudget(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not on PATH")
	}
	start := time.Now()
	err := runWithTimeout([]string{"sleep", "0.01"})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("runWithTimeout on a fast command: got %v, want nil", err)
	}
	if elapsed >= actuationTimeout {
		t.Fatalf("runWithTimeout took %v to return a fast command's result — longer than actuationTimeout (%v) itself", elapsed, actuationTimeout)
	}
}

// TestOutputBounded_DeadlineIsObservableViaContext confirms the error path a
// deadline-killed outputBounded call produces is recognizable as a context
// deadline by its caller (tmux.go's Spawn/OpenTerminal wrap it with
// fmt.Errorf("tmux new-window: %w", err) rather than a bespoke sentinel — see
// their docs for why — so callers that care can still errors.Is/As through to
// context.DeadlineExceeded).
func TestOutputBounded_DeadlineIsObservableViaContext(t *testing.T) {
	argv := hangingSleepArgv(t)
	_, err := outputBounded(50*time.Millisecond, argv)
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	// exec.CommandContext reports a deadline kill as an ordinary *exec.ExitError
	// ("signal: killed"), not as context.DeadlineExceeded directly — same
	// observation classifySendExecError's doc makes about this exact shape.
	// This assertion documents that a plain errors.Is on the returned error
	// will NOT find context.DeadlineExceeded, so a caller needing to detect
	// the deadline case specifically must check ctx.Err() itself (as
	// classifySendExecError does), not rely on the error runBounded/
	// outputBounded return.
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("did not expect the returned error to wrap context.DeadlineExceeded directly — exec.CommandContext reports a kill as an *exec.ExitError instead")
	}
}

func TestEncodeCwd_SlashesToHyphens(t *testing.T) {
	got := encodeCwd("/home/user/myproject")
	want := "-home-user-myproject"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEncodeCwd_DotsAlsoBecomeHyphens(t *testing.T) {
	// verified against the real Claude Code project-dir encoding (residual
	// #4 / internal/claude.encodeCwd's identical contract): both "/" AND "."
	// collapse to "-".
	got := encodeCwd("/home/user/.someplugin/agent-sessions")
	want := "-home-user--someplugin-agent-sessions"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEncodeCwd_NoSeparators_Unchanged(t *testing.T) {
	if got := encodeCwd("noseparators"); got != "noseparators" {
		t.Errorf("got %q, want unchanged input", got)
	}
}
