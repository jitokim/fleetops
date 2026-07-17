package notify

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestArgv_PlainTitleAndBody(t *testing.T) {
	got := argv("fleetops · GATE", "myproject: continue?")
	want := []string{"osascript", "-e", `display notification "myproject: continue?" with title "fleetops · GATE"`}
	if len(got) != len(want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestArgv_EscapesDoubleQuotesInBody(t *testing.T) {
	got := argv("title", `he said "hi"`)
	script := got[2]
	if !strings.Contains(script, `\"hi\"`) {
		t.Errorf("script = %q, want the embedded quotes escaped as \\\"", script)
	}
}

func TestArgv_EscapesBackslashBeforeQuote(t *testing.T) {
	// A body ending in a literal backslash right before the closing quote
	// must not let that backslash escape the closing quote it's adjacent
	// to — backslash must be escaped BEFORE the quote-escaping pass runs.
	got := argv("title", `path\`)
	script := got[2]
	if !strings.Contains(script, `path\\"`) {
		t.Errorf("script = %q, want a trailing backslash doubled before the closing quote", script)
	}
}

// TestArgv_EscapesNewlinesInMultilineGatePrompt is the P2 review fix's
// regression: a multi-line gate prompt (a real possibility — Claude Code's
// AskUserQuestion/permission prompts can wrap several lines) previously
// reached osascript with a literal embedded newline, which is not valid
// inside an AppleScript double-quoted string literal — `display
// notification` would fail with a syntax error and Send's error is always
// swallowed by callers, so the notification was silently lost entirely.
func TestArgv_EscapesNewlinesInMultilineGatePrompt(t *testing.T) {
	body := "continue with the deploy?\noptions:\n1. yes\n2. no"
	script := argv("fleetops · GATE", body)[2]
	if strings.ContainsRune(script, '\n') {
		t.Errorf("script = %q, must not contain a raw newline byte (invalid inside an AppleScript string literal)", script)
	}
	if !strings.Contains(script, `continue with the deploy?\noptions:\n1. yes\n2. no`) {
		t.Errorf("script = %q, want each newline escaped as the two-character \\n sequence", script)
	}
}

func TestArgv_EscapesCarriageReturns(t *testing.T) {
	script := argv("t", "line1\r\nline2")[2]
	if strings.ContainsRune(script, '\r') || strings.ContainsRune(script, '\n') {
		t.Errorf("script = %q, must not contain a raw \\r or \\n byte", script)
	}
	if !strings.Contains(script, `line1\r\nline2`) {
		t.Errorf("script = %q, want \\r and \\n both escaped as two-character sequences", script)
	}
}

// TestArgv_NewlineEscapeAppliedAfterBackslashDoubling ensures the escape
// ORDER documented in appleScriptString actually holds: a raw newline must
// still escape correctly even when the string also contains a literal
// backslash (the backslash-doubling pass must not somehow consume or
// interact with the newline escaping that runs after it).
func TestArgv_NewlineEscapeAppliedAfterBackslashDoubling(t *testing.T) {
	script := argv("t", "path\\to\\file\nnext line")[2]
	if !strings.Contains(script, `path\\to\\file\nnext line`) {
		t.Errorf("script = %q, want backslashes doubled AND the newline escaped", script)
	}
}

func TestArgv_NeverProducesAnUnbalancedQuoteCount(t *testing.T) {
	cases := []string{
		``,
		`"`,
		`\`,
		`\"`,
		`"\"quoted\" with \\backslashes\\"`,
		"multi\nline\ttext",
	}
	for _, body := range cases {
		script := argv("t", body)[2]
		// Every literal `"` in the script must be part of a `\"` escape or
		// one of the 4 literal AppleScript-string delimiters — so the count
		// of UNESCAPED quotes (not preceded by a backslash) must be exactly
		// 4: the 2 delimiters around body plus the 2 around title.
		unescaped := 0
		runes := []rune(script)
		for i, r := range runes {
			if r != '"' {
				continue
			}
			if i > 0 && runes[i-1] == '\\' {
				// count the run of preceding backslashes; an escaped quote
				// is only "consumed" by an ODD number of backslashes.
				j := i - 1
				for j >= 0 && runes[j] == '\\' {
					j--
				}
				if (i-1-j)%2 == 1 {
					continue // escaped quote, not a delimiter
				}
			}
			unescaped++
		}
		if unescaped != 4 {
			t.Errorf("body %q: script %q has %d unescaped quotes, want 4 (2 string literals)", body, script, unescaped)
		}
	}
}

func TestSend_CallsRunnerWithArgv(t *testing.T) {
	orig := runner
	defer func() { runner = orig }()

	var gotArgv []string
	runner = func(ctx context.Context, argv []string) error {
		gotArgv = argv
		return nil
	}

	if err := Send("T", "B"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	want := argv("T", "B")
	if len(gotArgv) != len(want) {
		t.Fatalf("got %#v, want %#v", gotArgv, want)
	}
	for i := range want {
		if gotArgv[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q", i, gotArgv[i], want[i])
		}
	}
}

func TestSend_PropagatesRunnerError(t *testing.T) {
	orig := runner
	defer func() { runner = orig }()

	wantErr := errors.New("osascript not found")
	runner = func(ctx context.Context, argv []string) error { return wantErr }

	if err := Send("T", "B"); err != wantErr {
		t.Errorf("Send err = %v, want %v", err, wantErr)
	}
}

func TestSend_PassesADeadlineContext(t *testing.T) {
	orig := runner
	defer func() { runner = orig }()

	var sawDeadline bool
	runner = func(ctx context.Context, argv []string) error {
		_, sawDeadline = ctx.Deadline()
		return nil
	}
	if err := Send("T", "B"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !sawDeadline {
		t.Error("Send must pass a context with a deadline (the 3s timeout)")
	}
}
