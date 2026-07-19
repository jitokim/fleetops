package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jitokim/fleetops/tools/dupdetect/internal/similarity"
)

func defaultOptions(title string) options {
	return options{number: 42, title: title, threshold: similarity.DefaultThreshold, topN: 3}
}

const openItems = `[
  {"number": 1, "title": "fleet table crashes on resize"},
  {"number": 2, "title": "add zellij backend support"},
  {"number": 3, "title": "fix: fleet table crashes on resize in tmux"}
]`

func TestRunReportsMatches(t *testing.T) {
	got := runOK(t, openItems, defaultOptions("fleet table crashes on resize"))

	if len(got.Matches) != 2 {
		t.Fatalf("got %d matches %+v, want 2", len(got.Matches), got.Matches)
	}
	if got.Matches[0].Number != 1 {
		t.Errorf("best match = #%d, want #1", got.Matches[0].Number)
	}
	if got.Matches[0].Score < got.Matches[1].Score {
		t.Errorf("matches not ordered best-first: %+v", got.Matches)
	}
}

func TestRunExcludesTheItemItself(t *testing.T) {
	opts := defaultOptions("fleet table crashes on resize")
	opts.number = 1

	for _, m := range runOK(t, openItems, opts).Matches {
		if m.Number == 1 {
			t.Errorf("item reported itself as a duplicate: %+v", m)
		}
	}
}

func TestRunRespectsTopN(t *testing.T) {
	opts := defaultOptions("fleet table crashes on resize")
	opts.topN = 1

	if got := runOK(t, openItems, opts); len(got.Matches) != 1 {
		t.Errorf("got %d matches, want 1", len(got.Matches))
	}
}

// Every no-match path must still emit valid JSON with an empty list — the
// workflow parses stdout unconditionally, so a bare newline or a nil `matches`
// would break it.
func TestRunEmitsEmptyListWhenNothingMatches(t *testing.T) {
	tests := []struct {
		name  string
		stdin string
		opts  options
	}{
		{"unrelated title", openItems, defaultOptions("update the contributing guide")},
		{"empty title", openItems, defaultOptions("")},
		{"punctuation title", openItems, defaultOptions("???")},
		{"empty candidate array", `[]`, defaultOptions("fleet table crashes on resize")},
		{"empty stdin", "", defaultOptions("fleet table crashes on resize")},
		{"whitespace only stdin", "   \n", defaultOptions("fleet table crashes on resize")},
		{"null candidate array", `null`, defaultOptions("fleet table crashes on resize")},
		{"threshold above one", openItems, options{number: 42, title: "fleet table crashes on resize", threshold: 1.1, topN: 3}},
		{"zero topN", openItems, options{number: 42, title: "fleet table crashes on resize", threshold: similarity.DefaultThreshold}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := runOK(t, tc.stdin, tc.opts)
			if got.Matches == nil {
				t.Fatal("matches is null, want an empty JSON array")
			}
			if len(got.Matches) != 0 {
				t.Errorf("got %d matches %+v, want none", len(got.Matches), got.Matches)
			}
		})
	}
}

func TestRunRejectsMalformedInput(t *testing.T) {
	tests := []struct {
		name  string
		stdin string
	}{
		{"not json", "this is not json"},
		{"truncated array", `[{"number": 1, "title": "crash"`},
		{"object instead of array", `{"number": 1, "title": "crash"}`},
		{"wrong field type", `[{"number": "one", "title": "crash"}]`},
		{"title is not a string", `[{"number": 1, "title": 12}]`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout bytes.Buffer
			err := run(strings.NewReader(tc.stdin), &stdout, defaultOptions("crash"))
			if err == nil {
				t.Fatalf("run accepted malformed input, wrote %q", stdout.String())
			}
			if !strings.Contains(err.Error(), "reading candidates from stdin") {
				t.Errorf("error %q does not say where it came from", err)
			}
		})
	}
}

// Extra fields must be tolerated so `gh ... --json number,title,url,state` can
// be piped in without a jq step in between.
func TestRunIgnoresUnknownFields(t *testing.T) {
	stdin := `[{"number": 1, "title": "fleet table crashes on resize", "url": "https://example.test/1", "state": "OPEN"}]`

	got := runOK(t, stdin, defaultOptions("fleet table crashes on resize"))
	if len(got.Matches) != 1 || got.Matches[0].Number != 1 {
		t.Fatalf("got %+v, want a single match on #1", got.Matches)
	}
	if got.Matches[0].Title != "fleet table crashes on resize" {
		t.Errorf("title = %q, want the candidate's own title", got.Matches[0].Title)
	}
}

func TestRunHandlesMultibyteTitles(t *testing.T) {
	stdin := `[{"number": 5, "title": "한글 제목 크래시 발생"}]`

	got := runOK(t, stdin, defaultOptions("한글 제목 크래시"))
	if len(got.Matches) != 1 {
		t.Fatalf("got %+v, want one match", got.Matches)
	}
}

// alwaysFailWriter is an io.Writer that always fails, so tests can drive the
// encodeReport error branch without depending on any real broken pipe or
// full-disk condition.
type alwaysFailWriter struct{ err error }

func (w alwaysFailWriter) Write([]byte) (int, error) { return 0, w.err }

// TestRunReturnsErrorWhenStdoutWriteFails exercises the encodeReport error
// branch in main.go, which no other test reaches: every other test writes to
// a bytes.Buffer that cannot fail. A stdout write can fail for real (closed
// pipe, disk full downstream), and run() must surface that as an error
// rather than silently swallowing it or panicking.
func TestRunReturnsErrorWhenStdoutWriteFails(t *testing.T) {
	writeErr := errors.New("broken pipe")
	stdout := alwaysFailWriter{err: writeErr}

	err := run(strings.NewReader(openItems), stdout, defaultOptions("fleet table crashes on resize"))

	if err == nil {
		t.Fatal("run returned nil error despite a failing stdout writer")
	}
	if !strings.Contains(err.Error(), "writing report to stdout") {
		t.Errorf("error %q does not say where it came from", err)
	}
	if !errors.Is(err, writeErr) {
		t.Errorf("error %q does not wrap the underlying write error", err)
	}
}

// TestRunReturnsErrorWhenStdoutWriteFailsWithNoMatches is the empty-matches
// variant of the above: json.Encoder still writes bytes for `{"matches":[]}`,
// so the failure path must be reachable even when there is nothing to report.
func TestRunReturnsErrorWhenStdoutWriteFailsWithNoMatches(t *testing.T) {
	stdout := alwaysFailWriter{err: errors.New("disk full")}

	err := run(strings.NewReader(`[]`), stdout, defaultOptions("fleet table crashes on resize"))

	if err == nil {
		t.Fatal("run returned nil error despite a failing stdout writer")
	}
	if !strings.Contains(err.Error(), "writing report to stdout") {
		t.Errorf("error %q does not say where it came from", err)
	}
}

// alwaysFailReader is an io.Reader that always fails with a non-EOF error, so
// tests can distinguish "stdin is syntactically invalid JSON" (already
// covered by TestRunRejectsMalformedInput) from "reading stdin itself
// failed" (a closed pipe, a killed upstream `gh` process) — both must produce
// a "reading candidates from stdin" error, but only a real I/O failure proves
// decodeCandidates' generic error branch, not just its JSON-syntax branch.
type alwaysFailReader struct{ err error }

func (r alwaysFailReader) Read([]byte) (int, error) { return 0, r.err }

func TestRunReturnsErrorWhenStdinReadFails(t *testing.T) {
	readErr := errors.New("upstream process killed")
	stdin := alwaysFailReader{err: readErr}

	var stdout bytes.Buffer
	err := run(stdin, &stdout, defaultOptions("crash"))

	if err == nil {
		t.Fatalf("run accepted a failing reader, wrote %q", stdout.String())
	}
	if !strings.Contains(err.Error(), "reading candidates from stdin") {
		t.Errorf("error %q does not say where it came from", err)
	}
	if !errors.Is(err, readErr) {
		t.Errorf("error %q does not wrap the underlying read error", err)
	}
}

func runOK(t *testing.T, stdin string, opts options) report {
	t.Helper()
	var stdout bytes.Buffer
	if err := run(strings.NewReader(stdin), &stdout, opts); err != nil {
		t.Fatalf("run returned %v", err)
	}
	var got report
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("run wrote invalid JSON %q: %v", stdout.String(), err)
	}
	return got
}
