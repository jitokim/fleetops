package control

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/jitokim/fleetops/internal/sessions"
)

// testGUID builds a CHECKED sessionGUID for tests that call the argv/script
// builders directly. Going through the real constructor is deliberate: it is
// the same door production code uses, so a test cannot accidentally demonstrate
// a shape production could never produce.
func testGUID(t *testing.T, raw string) sessionGUID {
	t.Helper()
	guid, ok := newSessionGUID(raw)
	if !ok {
		t.Fatalf("newSessionGUID(%q) refused a value this test needs", raw)
	}
	return guid
}

// withFakeITerm2Send swaps the injectable exec seam for a spy and restores it.
// Mirrors withFakeITerm2Focus — no real iTerm2, no osascript, no macOS.
func withFakeITerm2Send(t *testing.T, fn func(argv []string) (string, error)) *[]string {
	t.Helper()
	var captured []string
	orig := iterm2SendFn
	t.Cleanup(func() { iterm2SendFn = orig })
	iterm2SendFn = func(argv []string) (string, error) {
		captured = argv
		return fn(argv)
	}
	return &captured
}

// expectNoExec installs a send seam that fails the test the moment it is
// reached, and returns the same capture pointer withFakeITerm2Send does so the
// caller can still assert zero exec afterwards. The zero-exec property is the
// point of every refusal test on this surface: a value that reaches osascript
// at all has already left the whitelist's protection. why names the refusal
// being pinned, so a firing spy says which guard leaked.
func expectNoExec(t *testing.T, why string) *[]string {
	t.Helper()
	return withFakeITerm2Send(t, func(argv []string) (string, error) {
		t.Fatalf("iterm2SendFn must NEVER be called %s — execed %v", why, argv)
		return "", nil
	})
}

// sendEntry is a registry entry whose GUID and tty both pass their whitelists —
// the baseline these tests vary one field at a time from.
func sendEntry() sessions.SessionEntry {
	return sessions.SessionEntry{
		HostApp:  iterm2HostApp,
		WindowID: "w0t1p0:C3C73A44-B7A5-4798-8730-4F68B2A6A15E",
		TTY:      "ttys006",
		PID:      42,
	}
}

func TestResolveSendAdapter_ITerm2_Resolves(t *testing.T) {
	adapter, ok := ResolveSendAdapter(iterm2HostApp)
	if !ok {
		t.Fatalf("ResolveSendAdapter(%q) ok=false, want an adapter", iterm2HostApp)
	}
	if _, isITerm := adapter.(iterm2SendAdapter); !isITerm {
		t.Errorf("adapter = %T, want iterm2SendAdapter", adapter)
	}
	if adapter.Name() != "iterm2" {
		t.Errorf("Name() = %q, want iterm2", adapter.Name())
	}
}

func TestResolveSendAdapter_UnknownAndEmptyHostApp_Degrade(t *testing.T) {
	for _, hostApp := range []string{"Apple_Terminal", "tmux", "", "iTerm.app.evil"} {
		if adapter, ok := ResolveSendAdapter(hostApp); ok || adapter != nil {
			t.Errorf("ResolveSendAdapter(%q) = (%v,%v), want (nil,false) to degrade", hostApp, adapter, ok)
		}
	}
}

// ── injection safety ──────────────────────────────────────────────────────
//
// These are the highest-value tests in this file. The rule they enforce: no
// untrusted value is ever concatenated into AppleScript source. A payload that
// could close a string literal and append statements reaches `do shell script`,
// i.e. arbitrary local code execution — a prior review found exactly that
// Critical defect on this surface.

// hostilePayloads is the corpus every injection assertion runs over: quoting
// metacharacters, shell metacharacters, AppleScript fragments, control bytes,
// and non-ASCII. Each must round-trip VERBATIM in an argv slot and must never
// appear in the script.
var hostilePayloads = map[string]string{
	"double quote":           `say "hi"`,
	"backslash":              `C:\path\to\thing`,
	"double backslash":       `a\\b`,
	"backtick":               "run `id` now",
	"dollar paren":           `$(id)`,
	"shell semicolon":        `x; touch /tmp/pwned`,
	"applescript escape":     `x" & (do shell script "touch /tmp/pwned") & "`,
	"applescript statement":  "\" \nactivate\ntell application \"Finder\" to quit\n\"",
	"embedded newline":       "line1\nline2\nline3",
	"carriage return":        "line1\rline2",
	"korean":                 "테스트 프롬프트를 보냅니다",
	"emoji":                  "ship it 🚀",
	"raw esc byte":           "\x1b[200~payload\x1b[201~",
	"raw etx byte":           "\x03",
	"nul-adjacent controls":  "\x01\x02\x04\x05",
	"looks like a guid":      "C3C73A44-B7A5-4798-8730-4F68B2A6A15E",
	"end of options marker":  "--",
	"osascript flag shaped":  "-e",
	"tab and mixed controls": "a\tb\x0bc",
	"empty":                  "",
	"only whitespace":        "   ",
	"verdict word ok":        "ok",
	"verdict word miss":      "miss",
}

// TestITerm2SendTextArgv_PayloadOnlyInArgvNeverInScript is the structural
// injection pin. For every hostile payload the text must appear ONLY as the
// trailing argv element and NEVER as a substring of the script element. It
// fails the moment anyone reintroduces interpolation — which is exactly the
// regression that must never recur.
func TestITerm2SendTextArgv_PayloadOnlyInArgvNeverInScript(t *testing.T) {
	for name, payload := range hostilePayloads {
		t.Run(name, func(t *testing.T) {
			argv := iterm2SendTextArgv(testGUID(t, "ABC-123"), "ttys006", payload)

			if len(argv) != 6 {
				t.Fatalf("argv = %v, want 6 elements [osascript -e <script> -- <tty> <text>]", argv)
			}
			if argv[0] != "osascript" || argv[1] != "-e" || argv[3] != "--" {
				t.Fatalf("argv shape = %v, want [osascript -e <script> -- <tty> <text>]", argv)
			}
			if argv[4] != "/dev/ttys006" {
				t.Errorf("argv[4] = %q, want the expected tty /dev/ttys006", argv[4])
			}
			// The payload arrives byte-identical in its own slot.
			if argv[5] != payload {
				t.Errorf("payload round-trip: argv[5] = %q, want %q", argv[5], payload)
			}
			// ...and NOWHERE in the script.
			//
			// Checked against a baseline built with an unrelated payload, so a
			// payload that merely COINCIDES with fixed script text (the corpus
			// deliberately includes the verdict words "ok" and "miss") is not
			// mistaken for a leak. The rigorous form of this property is
			// TestITerm2SendScript_ByteIdenticalAcrossPayloads; this assertion
			// is the one that names the offending payload when it fails.
			script := argv[2]
			baseline := iterm2SendTextArgv(testGUID(t, "ABC-123"), "ttys006", "\x00unlikely-baseline\x00")[2]
			if payload != "" && strings.Contains(script, payload) && !strings.Contains(baseline, payload) {
				t.Errorf("payload %q leaked into the AppleScript source — this is the injection defect:\n%s", payload, script)
			}
		})
	}
}

// TestITerm2SendScript_ByteIdenticalAcrossPayloads encodes the entire safety
// argument in one assertion: the script is fixed at Go-compile time and does
// not vary with the payload at all. If two wildly different payloads ever
// produce different script text, a value crossed from a parameter slot into a
// syntax slot.
func TestITerm2SendScript_ByteIdenticalAcrossPayloads(t *testing.T) {
	baseline := iterm2SendTextArgv(testGUID(t, "ABC-123"), "ttys006", "benign")[2]
	for name, payload := range hostilePayloads {
		t.Run(name, func(t *testing.T) {
			if got := iterm2SendTextArgv(testGUID(t, "ABC-123"), "ttys006", payload)[2]; got != baseline {
				t.Errorf("script varied with the payload %q — interpolation has been reintroduced:\ngot:  %s\nwant: %s", payload, got, baseline)
			}
		})
	}
}

// TestITerm2SendScript_TTYIsArgvNotInterpolated: the expected tty is registry
// data too, and it likewise travels in an argument slot. The script compares
// against `item 1 of argv`, never against a baked-in literal.
func TestITerm2SendScript_TTYIsArgvNotInterpolated(t *testing.T) {
	script := iterm2SendTextArgv(testGUID(t, "ABC-123"), "ttys006", "x")[2]

	if !strings.Contains(script, "tty of aSession is not (item 1 of argv)") {
		t.Errorf("script does not compare tty against argv:\n%s", script)
	}
	if strings.Contains(script, "ttys006") {
		t.Errorf("the tty was interpolated into the script instead of passed as argv:\n%s", script)
	}
}

// TestITerm2SendText_UntrustedWindowID_RefusesNoExec: anything failing the GUID
// whitelist must refuse BEFORE exec. Reuses focus_test.go's hostile corpus —
// the same untrusted $ITERM_SESSION_ID reaches both surfaces.
func TestITerm2SendText_UntrustedWindowID_RefusesNoExec(t *testing.T) {
	hostile := map[string]string{
		"closes the quoted literal": `w0t0p0:X" & (do shell script "touch /tmp/pwned") & "`,
		"bare quote":                `w0t0p0:a"b`,
		"embedded newline":          "w0t0p0:a\nactivate",
		"backslash escape":          `w0t0p0:a\"b`,
		"whitespace":                "w0t0p0:a b",
		"empty guid after colon":    "w0t0p0:",
		"ampersand concat":          `w0t0p0:a&b`,
		"paren call":                `w0t0p0:a(b)`,
		"entirely empty":            "",
	}
	for name, windowID := range hostile {
		t.Run(name, func(t *testing.T) {
			captured := expectNoExec(t, "for an untrusted WindowID")

			entry := sendEntry()
			entry.WindowID = windowID
			err := iterm2SendAdapter{}.SendText(entry, "hello")

			if !errors.Is(err, ErrNoSendSurface) {
				t.Errorf("SendText(WindowID=%q) = %v, want ErrNoSendSurface", windowID, err)
			}
			if *captured != nil {
				t.Errorf("execed %v, want ZERO exec for %q", *captured, windowID)
			}
		})
	}
}

// TestITerm2SendText_UntrustedTTY_RefusesNoExec: the tty is untrusted registry
// data on the same footing as the WindowID. It travels as argv, so it cannot
// inject — but a value that is not a device name cannot address a real session
// either, and refusing early is the house style (defense in depth).
func TestITerm2SendText_UntrustedTTY_RefusesNoExec(t *testing.T) {
	// An empty/"/dev/"-only tty is NOT hostile, just absent — it gets its own
	// refusal, see TestITerm2SendText_NoRecordedTTY_RefusesWithItsOwnReason.
	hostile := map[string]string{
		"quote":            `ttys006"`,
		"space":            "ttys006 x",
		"newline":          "ttys006\nactivate",
		"path traversal":   "../../dev/ttys006",
		"slash":            "dev/ttys006",
		"applescript frag": `x" & (do shell script "id") & "`,
		// AppleScript's `is` is case-insensitive, so an uppercase variant would
		// otherwise satisfy the binding guard against the real lowercase device.
		"uppercase":  "TTYS006",
		"mixed case": "TtyS006",
	}
	for name, tty := range hostile {
		t.Run(name, func(t *testing.T) {
			captured := expectNoExec(t, "for an untrusted tty")

			entry := sendEntry()
			entry.TTY = tty
			err := iterm2SendAdapter{}.SendText(entry, "hello")

			if !errors.Is(err, ErrNoSendSurface) {
				t.Errorf("SendText(TTY=%q) = %v, want ErrNoSendSurface", tty, err)
			}
			if *captured != nil {
				t.Errorf("execed %v, want ZERO exec for tty %q", *captured, tty)
			}
		})
	}
}

// TestITerm2SendText_NoRecordedTTY_RefusesWithItsOwnReason: registry entries
// with "tty":"" exist in the wild. That is an ABSENT record, not a malformed
// one, and not a moved tab — Tier 1h has nothing to verify the host session
// against, so it refuses before any exec and says exactly that. Reported as a
// generic refusal it would eventually surface as a tty MISMATCH and blame a
// moved tab for a record that never had a tty at all.
func TestITerm2SendText_NoRecordedTTY_RefusesWithItsOwnReason(t *testing.T) {
	for name, tty := range map[string]string{"empty": "", "only dev prefix": "/dev/"} {
		t.Run(name, func(t *testing.T) {
			captured := expectNoExec(t, "without a recorded tty")

			entry := sendEntry()
			entry.TTY = tty
			err := iterm2SendAdapter{}.SendText(entry, "hello")

			if !errors.Is(err, ErrSendNoRecordedTTY) {
				t.Errorf("SendText(TTY=%q) = %v, want ErrSendNoRecordedTTY", tty, err)
			}
			if errors.Is(err, ErrSendTTYMismatch) {
				t.Error("an absent tty must not be reported as a MISMATCH — nothing moved")
			}
			if *captured != nil {
				t.Errorf("execed %v, want ZERO exec", *captured)
			}
		})
	}
}

// TestNewSessionGUID_RefusesHostileWindowIDs: the constructor is the ONLY door
// to a sessionGUID, and it is the door the whitelist stands in. Every value
// that could close the AppleScript string literal must be refused here — after
// this point the type system, not a caller's discipline, is what keeps the
// script source fixed.
func TestNewSessionGUID_RefusesHostileWindowIDs(t *testing.T) {
	hostile := map[string]string{
		"do shell script":   `w0t0p0:X" & (do shell script "touch /tmp/pwned") & "`,
		"bare quote":        `w0t0p0:X"`,
		"backslash":         `w0t0p0:X\\`,
		"newline":           "w0t0p0:X\nactivate",
		"space":             "w0t0p0:X Y",
		"empty":             "",
		"empty after colon": "w0t0p0:",
	}
	for name, windowID := range hostile {
		t.Run(name, func(t *testing.T) {
			guid, ok := newSessionGUID(windowID)
			if ok {
				t.Fatalf("newSessionGUID(%q) accepted a hostile value as %q", windowID, guid)
			}
			if guid.String() != "" {
				t.Errorf("a refused sessionGUID carries %q, want the inert zero value", guid)
			}
		})
	}
}

// TestSessionGUID_ZeroValueIsInjectionInert: the zero value is the one
// sessionGUID obtainable without the constructor, so it has to be harmless on
// its own. Baked into the script it produces a GUID nothing can match — a
// guaranteed miss, never a syntax break.
func TestSessionGUID_ZeroValueIsInjectionInert(t *testing.T) {
	script := iterm2SendScript(sessionGUID{}, iterm2WriteTextStmt)

	if strings.Count(script, `"`) != strings.Count(iterm2SendScript(testGUID(t, "ABC-123"), iterm2WriteTextStmt), `"`) {
		t.Error("the zero value changed the script's quote structure")
	}
	if !strings.Contains(script, `is "" then`) {
		t.Errorf("script did not interpolate the zero value as an empty literal:\n%s", script)
	}
}

// TestITerm2SendTarget_AcceptsRealValues guards against whitelists so strict
// they reject what iTerm2 and the registry actually emit (which would silently
// disable Tier 1h altogether).
func TestITerm2SendTarget_AcceptsRealValues(t *testing.T) {
	entry := sendEntry()
	entry.TTY = "/dev/ttys006" // the registry may carry either form; normalizeTTY handles it

	guid, tty, err := iterm2SendTarget(entry)
	if err != nil {
		t.Fatalf("iterm2SendTarget on a real entry = %v, want nil", err)
	}
	if guid.String() != "C3C73A44-B7A5-4798-8730-4F68B2A6A15E" {
		t.Errorf("guid = %q, want the extracted GUID", guid)
	}
	if tty != "ttys006" {
		t.Errorf("tty = %q, want the normalized ttys006", tty)
	}
}

// ── verdict handling: hit / miss / mismatch / failure ─────────────────────

// TestITerm2SendText_Verdicts is the false-success pin. osascript exits 0 for
// every one of these, so ONLY the stdout verdict distinguishes them — and
// anything unrecognized must fail closed, never read as success.
func TestITerm2SendText_Verdicts(t *testing.T) {
	cases := map[string]struct {
		stdout  string
		wantErr error
	}{
		"hit":               {iterm2SendHit, nil},
		"hit with newline":  {iterm2SendHit + "\n", nil},
		"hit padded":        {"  " + iterm2SendHit + "  \n", nil},
		"miss":              {iterm2SendMiss, ErrSendNoSession},
		"miss with newline": {iterm2SendMiss + "\n", ErrSendNoSession},
		"tty mismatch":      {iterm2SendTTYMismatch, ErrSendTTYMismatch},
		"mismatch padded":   {" " + iterm2SendTTYMismatch + "\n", ErrSendTTYMismatch},
		// Unrecognized output fails closed like a miss, but is NOT a miss:
		// claiming "the tab was closed" here would be a fabricated diagnosis.
		"empty":               {"", ErrSendUnrecognizedVerdict},
		"whitespace only":     {"   \n", ErrSendUnrecognizedVerdict},
		"unexpected string":   {"something else entirely", ErrSendUnrecognizedVerdict},
		"partial hit prefix":  {"okay", ErrSendUnrecognizedVerdict},
		"hit inside sentence": {"it is ok now", ErrSendUnrecognizedVerdict},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			withFakeITerm2Send(t, func([]string) (string, error) { return tc.stdout, nil })

			err := iterm2SendAdapter{}.SendText(sendEntry(), "hello")

			if tc.wantErr == nil {
				if err != nil {
					t.Errorf("SendText with stdout %q = %v, want nil", tc.stdout, err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("SendText with stdout %q = %v, want %v", tc.stdout, err, tc.wantErr)
			}
		})
	}
}

// TestITerm2SendText_ExecError_Propagates: a non-zero osascript (revoked
// Automation grant, iTerm2 not running, timeout) is a REAL error, distinct from
// both a miss and a refusal. It must never be laundered into one of the
// degrade sentinels.
func TestITerm2SendText_ExecError_Propagates(t *testing.T) {
	boom := errors.New("exit status 1: not authorized to send Apple events (-1743)")
	withFakeITerm2Send(t, func([]string) (string, error) { return "", boom })

	err := iterm2SendAdapter{}.SendText(sendEntry(), "hello")

	if !errors.Is(err, boom) {
		t.Errorf("SendText = %v, want the exec error %v", err, boom)
	}
	if errors.Is(err, ErrSendNoSession) || errors.Is(err, ErrNoSendSurface) || errors.Is(err, ErrSendTTYMismatch) {
		t.Error("an exec failure must not masquerade as a degrade/refusal sentinel")
	}
}

// TestSendErrors_AreDistinct: the three failure modes must stay separable, so
// the operator can be told WHICH thing went wrong. Collapsing the tty refusal
// into a generic miss would hide a genuine wrong-terminal refusal.
func TestSendErrors_AreDistinct(t *testing.T) {
	if errors.Is(ErrSendTTYMismatch, ErrSendNoSession) || errors.Is(ErrSendNoSession, ErrSendTTYMismatch) {
		t.Error("ErrSendTTYMismatch and ErrSendNoSession must be distinguishable")
	}
	if errors.Is(ErrNoSendSurface, ErrSendNoSession) || errors.Is(ErrSendNoSession, ErrNoSendSurface) {
		t.Error("ErrNoSendSurface (refused, no exec) and ErrSendNoSession (execed, no match) must be distinguishable")
	}
	// The mismatch message is surfaced verbatim to the operator, so it must
	// actually say what happened and what to do about it.
	if !strings.Contains(ErrSendTTYMismatch.Error(), "tty") || !strings.Contains(ErrSendTTYMismatch.Error(), "attach") {
		t.Errorf("ErrSendTTYMismatch message %q must name the tty problem and the manual next step", ErrSendTTYMismatch)
	}
}

// TestErrSendUnrecognizedVerdict_DoesNotClaimTheTabWasClosed: failing closed on
// an output nobody anticipated is required; DIAGNOSING it as a closed tab is
// not. ErrSendNoSession's message asserts the session was closed, which is a
// claim an unrecognized verdict does not support — and it points the operator
// at the wrong thing when the real cause is a changed iTerm2/osascript
// contract.
func TestErrSendUnrecognizedVerdict_DoesNotClaimTheTabWasClosed(t *testing.T) {
	if errors.Is(ErrSendUnrecognizedVerdict, ErrSendNoSession) || errors.Is(ErrSendNoSession, ErrSendUnrecognizedVerdict) {
		t.Error("ErrSendUnrecognizedVerdict and ErrSendNoSession must be distinguishable")
	}
	if strings.Contains(ErrSendUnrecognizedVerdict.Error(), "closed") {
		t.Errorf("message %q claims the session was closed — nothing observed supports that", ErrSendUnrecognizedVerdict)
	}
	// It must still be unmistakably a FAILURE: the send did not happen.
	if !strings.Contains(ErrSendUnrecognizedVerdict.Error(), "NOT") {
		t.Errorf("message %q must state that the send did not happen", ErrSendUnrecognizedVerdict)
	}
}

// TestITerm2SendText_ExecsBuiltArgv confirms SendText dispatches the pure
// builder's argv, built from the EXTRACTED guid and NORMALIZED tty — pinning
// that the checked values are the used values.
func TestITerm2SendText_ExecsBuiltArgv(t *testing.T) {
	captured := withFakeITerm2Send(t, func([]string) (string, error) { return iterm2SendHit, nil })

	entry := sendEntry()
	entry.TTY = "/dev/ttys006"
	if err := (iterm2SendAdapter{}).SendText(entry, "do the thing"); err != nil {
		t.Fatalf("SendText = %v, want nil", err)
	}

	want := iterm2SendTextArgv(testGUID(t, "C3C73A44-B7A5-4798-8730-4F68B2A6A15E"), "ttys006", "do the thing")
	if !equalArgv(*captured, want) {
		t.Errorf("execed argv = %v,\nwant %v", *captured, want)
	}
}

// TestITerm2SendScript_NeverRaisesTheWindow: delivery to a BACKGROUND,
// non-frontmost window is the property that makes this the right shape for
// fleet actuation. A stray select/activate would yank iTerm2 forward on every
// keypress — the exact behaviour the ADR rejected keystroke simulation for.
func TestITerm2SendScript_NeverRaisesTheWindow(t *testing.T) {
	script := iterm2SendTextArgv(testGUID(t, "ABC-123"), "ttys006", "x")[2]

	for _, forbidden := range []string{"activate", "select "} {
		if strings.Contains(script, forbidden) {
			t.Errorf("send script contains %q — it must never raise the window:\n%s", forbidden, script)
		}
	}
}

// TestITerm2SendScript_ReportsAllThreeVerdicts: the script must be able to say
// which of the three outcomes happened. A script that can only fall off the end
// makes every send indistinguishable from a success.
func TestITerm2SendScript_ReportsAllThreeVerdicts(t *testing.T) {
	script := iterm2SendTextArgv(testGUID(t, "ABC-123"), "ttys006", "x")[2]

	for _, verdict := range []string{iterm2SendHit, iterm2SendMiss, iterm2SendTTYMismatch} {
		if !strings.Contains(script, `return "`+verdict+`"`) {
			t.Errorf("script never reports the %q verdict:\n%s", verdict, script)
		}
	}
	// The tty guard must come BEFORE the write: checking after would have
	// already typed into the wrong pane.
	guardAt := strings.Index(script, "tty of aSession is not")
	writeAt := strings.Index(script, "write aSession")
	if guardAt < 0 || writeAt < 0 || guardAt > writeAt {
		t.Errorf("the tty guard must precede the write — otherwise the wrong pane is already typed into:\n%s", script)
	}
	// And the GUID match must gate both.
	if matchAt := strings.Index(script, "id of aSession is"); matchAt < 0 || matchAt > guardAt {
		t.Errorf("the GUID match must gate the tty guard and the write:\n%s", script)
	}
}

// TestITerm2SendScript_ReadOnlyTraversal: the traversal must not mutate the
// collections it iterates (closing a session mid-loop invalidates the index).
func TestITerm2SendScript_ReadOnlyTraversal(t *testing.T) {
	script := iterm2SendTextArgv(testGUID(t, "ABC-123"), "ttys006", "x")[2]

	for _, mutator := range []string{"close ", "delete ", "create ", "set id of", "set tty of"} {
		if strings.Contains(script, mutator) {
			t.Errorf("send script mutates while iterating (%q) — invalidates the index:\n%s", mutator, script)
		}
	}
}

// ── boundSendAdapter: the verb → mechanism mapping ────────────────────────

// fakeSendAdapter records what the binding asked of it — which mechanism, and
// with which payload.
type fakeSendAdapter struct {
	sentTexts       []string
	interruptCalled bool
	sendErr         error
}

func (f *fakeSendAdapter) Name() string { return "fakehost" }
func (f *fakeSendAdapter) SendText(_ sessions.SessionEntry, text string) error {
	f.sentTexts = append(f.sentTexts, text)
	return f.sendErr
}
func (f *fakeSendAdapter) Interrupt(sessions.SessionEntry) error {
	f.interruptCalled = true
	return f.sendErr
}

// TestBoundSendAdapter_VerbMapping pins the verb table: resume/inject send the
// prompt, kill rides Resume with the literal "/exit" (it is NOT a control
// character here), and approve is a bare submit — an EMPTY string, which must
// still reach the adapter as a real call rather than being optimized away.
func TestBoundSendAdapter_VerbMapping(t *testing.T) {
	f := &fakeSendAdapter{}
	act := boundSendAdapter{adapter: f, entry: sendEntry()}

	if err := act.Resume("fix the flaky test"); err != nil {
		t.Fatalf("Resume = %v, want nil", err)
	}
	if err := act.Resume("/exit"); err != nil { // kill's path
		t.Fatalf("Resume(/exit) = %v, want nil", err)
	}
	if err := act.Approve(); err != nil {
		t.Fatalf("Approve = %v, want nil", err)
	}

	want := []string{"fix the flaky test", "/exit", ""}
	if len(f.sentTexts) != len(want) {
		t.Fatalf("sent %v, want %v", f.sentTexts, want)
	}
	for i := range want {
		if f.sentTexts[i] != want[i] {
			t.Errorf("send %d = %q, want %q", i, f.sentTexts[i], want[i])
		}
	}
}

// TestBoundSendAdapter_PropagatesErrorAndReportsTier: failures pass through
// untouched, and the binding reports the Tier 1h label so the actuation log can
// tell an in-place host write from a multiplexer send.
func TestBoundSendAdapter_PropagatesErrorAndReportsTier(t *testing.T) {
	boom := errors.New("nope")
	act := boundSendAdapter{adapter: &fakeSendAdapter{sendErr: boom}, entry: sendEntry()}

	if err := act.Resume("x"); !errors.Is(err, boom) {
		t.Errorf("Resume = %v, want %v", err, boom)
	}
	if err := act.Approve(); !errors.Is(err, boom) {
		t.Errorf("Approve = %v, want %v", err, boom)
	}
	if act.Backend() != "fakehost" {
		t.Errorf("Backend() = %q, want fakehost", act.Backend())
	}
	if act.Tier() != actuationTierHostSend {
		t.Errorf("Tier() = %q, want %q", act.Tier(), actuationTierHostSend)
	}
}

// TestClassifySendExecError_DeadlineIsUnknownDelivery: a deadline kill is the
// ONE exec failure that may have landed — osascript was interrupted while
// running, not prevented from starting. It must be separated from the
// provably-undelivered failures so callers do not retry it blindly.
//
// The classification reads ctx.Err(), not err, because CommandContext surfaces
// a deadline kill as an ordinary "signal: killed" ExitError — indistinguishable
// from a script that died on its own.
func TestClassifySendExecError_DeadlineIsUnknownDelivery(t *testing.T) {
	killed := errors.New("signal: killed")

	err := classifySendExecError(context.DeadlineExceeded, killed)

	if !errors.Is(err, ErrSendDeliveryUnknown) {
		t.Errorf("classifySendExecError = %v, want ErrSendDeliveryUnknown", err)
	}
	if !strings.Contains(err.Error(), "UNKNOWN") {
		t.Errorf("message %q must say the delivery outcome is unknown", err)
	}
	// Neither a clean success nor a clean failure: it must not be mistakable
	// for one of the safe-to-retry sentinels.
	if errors.Is(err, ErrSendNoSession) || errors.Is(err, ErrSendUnrecognizedVerdict) || errors.Is(err, ErrNoSendSurface) {
		t.Error("a timeout must not masquerade as a provably-undelivered failure")
	}
}

// TestClassifySendExecError_NonDeadlineFailuresStayPlain: a denied Automation
// grant, iTerm2 not running, a launch failure — all happen BEFORE the script
// can write, so they provably delivered nothing and must keep their existing
// (degradable) shape, stderr diagnosis included.
func TestClassifySendExecError_NonDeadlineFailuresStayPlain(t *testing.T) {
	cmd := exec.Command("sh", "-c", "echo 'Not authorized to send Apple events to iTerm2. (-1743)' >&2; exit 1")
	_, execErr := cmd.Output()
	if execErr == nil {
		t.Fatal("expected the probe command to fail")
	}

	// No deadline: ctx.Err() is nil, which is the live code's normal case.
	err := classifySendExecError(nil, execErr)

	if errors.Is(err, ErrSendDeliveryUnknown) {
		t.Errorf("error = %v, want it NOT flagged as unknown-delivery", err)
	}
	if !strings.Contains(err.Error(), "-1743") {
		t.Errorf("error = %q, want the stderr diagnosis preserved", err)
	}
	if got := classifySendExecError(nil, nil); got != nil {
		t.Errorf("classifySendExecError(nil, nil) = %v, want nil", got)
	}
	// A cancelled (not timed-out) context is also not a deadline.
	if err := classifySendExecError(context.Canceled, execErr); errors.Is(err, ErrSendDeliveryUnknown) {
		t.Error("only a DEADLINE means unknown delivery; a plain cancellation does not")
	}
}

// TestWithCommandStderr_IncludesStderr: design §5.3 claims a denied Automation
// grant reaches the operator as an honest diagnosis. That is only true if the
// -1743 text — which osascript writes to STDERR, and which .Output() stashes in
// an ignored field — makes it into the error. Bare "exit status 1" tells the
// operator nothing and hides the one action that fixes it.
func TestWithCommandStderr_IncludesStderr(t *testing.T) {
	cmd := exec.Command("sh", "-c", "echo 'Not authorized to send Apple events to iTerm2. (-1743)' >&2; exit 1")
	_, execErr := cmd.Output()
	if execErr == nil {
		t.Fatal("expected the probe command to fail")
	}

	err := withCommandStderr(execErr)

	if !strings.Contains(err.Error(), "-1743") {
		t.Errorf("error = %q, want it to carry the stderr diagnosis", err)
	}
	if !errors.Is(err, execErr) {
		t.Error("the original exec error must stay unwrappable")
	}
}

// TestWithCommandStderr_PassesThroughNonExitErrors: a nil error, or a failure
// that already carries its own text (LookPath, a context deadline), must be
// returned untouched rather than dressed up.
func TestWithCommandStderr_PassesThroughNonExitErrors(t *testing.T) {
	if got := withCommandStderr(nil); got != nil {
		t.Errorf("withCommandStderr(nil) = %v, want nil", got)
	}
	plain := errors.New("exec: \"osascript\": executable file not found in $PATH")
	if got := withCommandStderr(plain); got != plain {
		t.Errorf("withCommandStderr(%v) = %v, want it unchanged", plain, got)
	}
}
