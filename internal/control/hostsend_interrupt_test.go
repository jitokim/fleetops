package control

import (
	"errors"
	"strings"
	"testing"

	"github.com/jitokim/fleetops/internal/sessions"
)

// TestITerm2InterruptArgv_Shape: no payload argument at all — the Esc is a
// constant in the script, so the only argument is the expected tty.
func TestITerm2InterruptArgv_Shape(t *testing.T) {
	argv := iterm2InterruptArgv(testGUID(t, "ABC-123"), "ttys006")

	if len(argv) != 5 {
		t.Fatalf("argv = %v, want 5 elements [osascript -e <script> -- <tty>]", argv)
	}
	if argv[0] != "osascript" || argv[1] != "-e" || argv[3] != "--" {
		t.Fatalf("argv shape = %v, want [osascript -e <script> -- <tty>]", argv)
	}
	if argv[4] != "/dev/ttys006" {
		t.Errorf("argv[4] = %q, want /dev/ttys006", argv[4])
	}
}

// TestITerm2InterruptScript_SendsRawEscWithoutNewline is the correctness pin
// for the one verb whose payload is a single invisible byte. Two properties,
// both essential:
//
//   - the Esc is `character id 27` — a literal, so no byte-fidelity assumption
//     about the Apple Event boundary is needed;
//   - `newline no` — a trailing CR here would SUBMIT whatever is sitting in the
//     prompt instead of aborting the turn, i.e. the opposite of an interrupt.
func TestITerm2InterruptScript_SendsRawEscWithoutNewline(t *testing.T) {
	script := iterm2InterruptArgv(testGUID(t, "ABC-123"), "ttys006")[2]

	if !strings.Contains(script, "write aSession text (character id 27) newline no") {
		t.Errorf("interrupt script does not send a raw Esc without a newline:\n%s", script)
	}
	if strings.Contains(script, "newline yes") {
		t.Errorf("interrupt script submits — a trailing CR would send the prompt instead of aborting the turn:\n%s", script)
	}
	// It must not carry a text payload slot at all.
	if strings.Contains(script, "item 2 of argv") {
		t.Errorf("interrupt script references a payload argument it is never given:\n%s", script)
	}
}

// TestITerm2InterruptScript_KeepsTheTTYGuard: the interrupt path must not be a
// shortcut around the wrong-pane protection. It shares the text path's skeleton
// precisely so this cannot drift.
func TestITerm2InterruptScript_KeepsTheTTYGuard(t *testing.T) {
	script := iterm2InterruptArgv(testGUID(t, "ABC-123"), "ttys006")[2]

	if !strings.Contains(script, "tty of aSession is not (item 1 of argv)") {
		t.Errorf("interrupt script dropped the tty binding guard:\n%s", script)
	}
	guardAt := strings.Index(script, "tty of aSession is not")
	writeAt := strings.Index(script, "write aSession")
	if guardAt < 0 || writeAt < 0 || guardAt > writeAt {
		t.Errorf("the tty guard must precede the Esc write:\n%s", script)
	}
	for _, verdict := range []string{iterm2SendHit, iterm2SendMiss, iterm2SendTTYMismatch} {
		if !strings.Contains(script, `return "`+verdict+`"`) {
			t.Errorf("interrupt script never reports the %q verdict:\n%s", verdict, script)
		}
	}
	for _, forbidden := range []string{"activate", "select "} {
		if strings.Contains(script, forbidden) {
			t.Errorf("interrupt script contains %q — it must never raise the window:\n%s", forbidden, script)
		}
	}
}

// TestITerm2InterruptScript_DiffersFromTextPathOnlyInTheWriteStatement pins
// that the two paths share one skeleton: everything but the write statement is
// identical, so a future fix to the guard cannot land on one path and miss the
// other.
func TestITerm2InterruptScript_DiffersFromTextPathOnlyInTheWriteStatement(t *testing.T) {
	interruptScript := iterm2InterruptArgv(testGUID(t, "ABC-123"), "ttys006")[2]
	textScript := iterm2SendTextArgv(testGUID(t, "ABC-123"), "ttys006", "whatever")[2]

	normalized := strings.Replace(interruptScript, iterm2EscStmt, iterm2WriteTextStmt, 1)
	if normalized != textScript {
		t.Errorf("interrupt and text scripts diverge beyond the write statement:\ninterrupt(normalized):\n%s\n\ntext:\n%s", normalized, textScript)
	}
}

// TestITerm2Interrupt_UntrustedIdentifiers_RefusesNoExec: the interrupt path
// gets the SAME zero-exec refusal as the text path. A separate code path is
// exactly where a validation gap would hide.
func TestITerm2Interrupt_UntrustedIdentifiers_RefusesNoExec(t *testing.T) {
	cases := map[string]sessions.SessionEntry{
		"hostile guid": func() sessions.SessionEntry {
			e := sendEntry()
			e.WindowID = `w0t0p0:X" & (do shell script "touch /tmp/pwned") & "`
			return e
		}(),
		"empty window id": func() sessions.SessionEntry {
			e := sendEntry()
			e.WindowID = ""
			return e
		}(),
		"hostile tty": func() sessions.SessionEntry {
			e := sendEntry()
			e.TTY = "ttys006\nactivate"
			return e
		}(),
	}
	for name, entry := range cases {
		t.Run(name, func(t *testing.T) {
			captured := expectNoExec(t, "for untrusted identifiers")

			err := iterm2SendAdapter{}.Interrupt(entry)

			if !errors.Is(err, ErrNoSendSurface) {
				t.Errorf("Interrupt = %v, want ErrNoSendSurface", err)
			}
			if *captured != nil {
				t.Errorf("execed %v, want ZERO exec", *captured)
			}
		})
	}
}

// TestITerm2Interrupt_NoRecordedTTY_RefusesWithItsOwnReason: the interrupt path
// shares iterm2SendTarget, so it must give the absent-tty case the same
// distinct refusal the text path does — re-asserted here because a separate
// entry point is exactly where such a gap would hide.
func TestITerm2Interrupt_NoRecordedTTY_RefusesWithItsOwnReason(t *testing.T) {
	captured := expectNoExec(t, "without a recorded tty")

	entry := sendEntry()
	entry.TTY = ""
	err := iterm2SendAdapter{}.Interrupt(entry)

	if !errors.Is(err, ErrSendNoRecordedTTY) {
		t.Errorf("Interrupt = %v, want ErrSendNoRecordedTTY", err)
	}
	if *captured != nil {
		t.Errorf("execed %v, want ZERO exec", *captured)
	}
}

// TestITerm2Interrupt_Verdicts: same fail-closed verdict handling as the text
// path, re-asserted here because this is a separate entry point.
func TestITerm2Interrupt_Verdicts(t *testing.T) {
	cases := map[string]struct {
		stdout  string
		wantErr error
	}{
		"hit":               {iterm2SendHit + "\n", nil},
		"miss":              {iterm2SendMiss, ErrSendNoSession},
		"tty mismatch":      {iterm2SendTTYMismatch, ErrSendTTYMismatch},
		"empty":             {"", ErrSendUnrecognizedVerdict},
		"unexpected string": {"huh", ErrSendUnrecognizedVerdict},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			withFakeITerm2Send(t, func([]string) (string, error) { return tc.stdout, nil })

			err := iterm2SendAdapter{}.Interrupt(sendEntry())

			if tc.wantErr == nil {
				if err != nil {
					t.Errorf("Interrupt with stdout %q = %v, want nil", tc.stdout, err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("Interrupt with stdout %q = %v, want %v", tc.stdout, err, tc.wantErr)
			}
		})
	}
}

// TestITerm2Interrupt_ExecError_Propagates: a revoked Automation grant or a
// timeout is a real error here too, never laundered into a degrade sentinel.
func TestITerm2Interrupt_ExecError_Propagates(t *testing.T) {
	boom := errors.New("exit status 1: not authorized to send Apple events (-1743)")
	withFakeITerm2Send(t, func([]string) (string, error) { return "", boom })

	err := iterm2SendAdapter{}.Interrupt(sendEntry())

	if !errors.Is(err, boom) {
		t.Errorf("Interrupt = %v, want %v", err, boom)
	}
	if errors.Is(err, ErrSendNoSession) || errors.Is(err, ErrNoSendSurface) || errors.Is(err, ErrSendTTYMismatch) {
		t.Error("an exec failure must not masquerade as a degrade/refusal sentinel")
	}
}

// TestITerm2Interrupt_ExecsBuiltArgv confirms Interrupt dispatches the pure
// builder's argv, from the extracted GUID and normalized tty.
func TestITerm2Interrupt_ExecsBuiltArgv(t *testing.T) {
	captured := withFakeITerm2Send(t, func([]string) (string, error) { return iterm2SendHit, nil })

	entry := sendEntry()
	entry.TTY = "/dev/ttys006"
	if err := (iterm2SendAdapter{}).Interrupt(entry); err != nil {
		t.Fatalf("Interrupt = %v, want nil", err)
	}

	want := iterm2InterruptArgv(testGUID(t, "C3C73A44-B7A5-4798-8730-4F68B2A6A15E"), "ttys006")
	if !equalArgv(*captured, want) {
		t.Errorf("execed argv = %v,\nwant %v", *captured, want)
	}
}

// TestBoundSendAdapter_InterruptRoutesToInterrupt: the binding must send `p`
// down the Esc path, NOT through SendText with some escape payload.
func TestBoundSendAdapter_InterruptRoutesToInterrupt(t *testing.T) {
	f := &fakeSendAdapter{}
	act := boundSendAdapter{adapter: f, entry: sendEntry()}

	if err := act.Interrupt(); err != nil {
		t.Fatalf("Interrupt = %v, want nil", err)
	}
	if !f.interruptCalled {
		t.Error("Interrupt did not reach the adapter's Interrupt")
	}
	if len(f.sentTexts) != 0 {
		t.Errorf("Interrupt went through SendText (%v) — it must use the Esc path", f.sentTexts)
	}
}
