// Command dupdetect reports which existing issues/PRs look like duplicates of
// a new one, using string similarity only — no model, no network, no API key.
// It is a pure filter: candidates in on stdin, matches out on stdout, so it
// can be tested and run by hand without GitHub involved at all.
//
//	gh issue list --state open --json number,title |
//	  dupdetect -number 42 -title "fleet table crashes on resize"
//
// Output is always valid JSON, including when nothing matches:
//
//	{"matches":[{"number":7,"title":"crash on resize","score":0.84}]}
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/jitokim/fleetops/tools/dupdetect/internal/similarity"
)

// candidate is one existing open item. Unknown fields in the input are
// ignored, so `gh ... --json number,title,url` can be piped in unchanged.
type candidate struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
}

// match is one reported duplicate. The number alone is enough for the caller:
// GitHub renders "#7" as a link, so no URL plumbing is needed.
type match struct {
	Number int     `json:"number"`
	Title  string  `json:"title"`
	Score  float64 `json:"score"`
}

type report struct {
	Matches []match `json:"matches"`
}

// options are the new item and the tuning knobs, kept together so run() takes
// one argument instead of four positional ones.
type options struct {
	number    int
	title     string
	threshold float64
	topN      int
}

func main() {
	opts := parseFlags()
	if err := run(os.Stdin, os.Stdout, opts); err != nil {
		fmt.Fprintf(os.Stderr, "dupdetect: %v\n", err)
		os.Exit(1)
	}
}

func parseFlags() options {
	var opts options
	flag.IntVar(&opts.number, "number", 0, "number of the new issue/PR, excluded from its own matches")
	flag.StringVar(&opts.title, "title", "", "title of the new issue/PR")
	flag.Float64Var(&opts.threshold, "threshold", similarity.DefaultThreshold, "minimum score in [0,1] to report a match")
	flag.IntVar(&opts.topN, "top", 3, "maximum number of matches to report")
	flag.Parse()
	return opts
}

// run is the whole program minus process wiring, so tests drive it with plain
// buffers. It always writes a well-formed report on success — an empty match
// list is the normal, expected outcome.
func run(stdin io.Reader, stdout io.Writer, opts options) error {
	candidates, err := decodeCandidates(stdin)
	if err != nil {
		return err
	}
	matches := similarity.Rank(opts.title, toSimilarityCandidates(candidates), opts.number, opts.threshold, opts.topN)
	return encodeReport(stdout, matches)
}

// decodeCandidates reads the candidate array. Empty input is not an error: a
// repo with no open issues is a legitimate state, and failing there would turn
// a quiet no-op into a red workflow run.
func decodeCandidates(stdin io.Reader) ([]candidate, error) {
	var candidates []candidate
	err := json.NewDecoder(stdin).Decode(&candidates)
	if errors.Is(err, io.EOF) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading candidates from stdin: %w", err)
	}
	return candidates, nil
}

func toSimilarityCandidates(candidates []candidate) []similarity.Candidate {
	converted := make([]similarity.Candidate, len(candidates))
	for i, c := range candidates {
		converted[i] = similarity.Candidate{Number: c.Number, Title: c.Title}
	}
	return converted
}

func encodeReport(stdout io.Writer, matches []similarity.Match) error {
	out := report{Matches: make([]match, len(matches))}
	for i, m := range matches {
		out.Matches[i] = match{Number: m.Number, Title: m.Title, Score: m.Score}
	}
	if err := json.NewEncoder(stdout).Encode(out); err != nil {
		return fmt.Errorf("writing report to stdout: %w", err)
	}
	return nil
}
