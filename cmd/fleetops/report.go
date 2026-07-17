package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jitokim/fleetops/internal/domain"
	"github.com/jitokim/fleetops/internal/events"
)

// runReportCmd implements `fleetops report --since 24h`: an OFFLINE,
// plain-text projection over the append-only event history
// (internal/events) — no TUI, no live scan, just a summary of what's
// already recorded. --since accepts time.ParseDuration syntax (h/m are the
// intended everyday units — "24h", "90m" — though any valid Go duration
// string works); defaults to 24h.
func runReportCmd(args []string) {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	since := fs.String("since", "24h", "how far back to summarize (time.ParseDuration syntax, e.g. 24h, 90m)")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	window, err := time.ParseDuration(*since)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fleetops report: invalid --since %q: %v\n", *since, err)
		os.Exit(1)
	}
	if err := writeReport(os.Stdout, events.HistoryDir(), *since, window, time.Now()); err != nil {
		fmt.Fprintln(os.Stderr, "fleetops report:", err)
		os.Exit(1)
	}
}

// loopSummary is one session's aggregated history over the report window.
type loopSummary struct {
	sessionID         string
	lastTS            int64
	lastState         string
	transitions       int
	gatesOpened       int
	gatesAnswered     int
	actuationsByActor map[string]int
	verdictsByOutcome map[string]int
}

// writeReport is runReportCmd's testable core: reads dir's full event
// history (events.ReadAll — tolerant of a missing dir/malformed lines, see
// its doc), keeps only events at or after now-window, and prints a compact
// per-loop summary (most-recently-active loop first) followed by fleet
// totals. sinceLabel is the raw --since string, shown verbatim in the
// header — what the human asked for, not a re-derived duration string.
//
// Judgment call: a loop here is identified by its raw session_id, not a
// human-readable project label — the event log itself carries no project
// name (see events.Event), and cross-referencing internal/registry or
// internal/sessions to resolve one is out of scope for this slice (the task
// asks for a projection over the history log alone).
func writeReport(w io.Writer, dir, sinceLabel string, window time.Duration, now time.Time) error {
	all, err := events.ReadAll(dir)
	if err != nil {
		return err
	}
	cutoff := now.Add(-window).UnixNano()

	summaries := make([]loopSummary, 0, len(all))
	for sessionID, evs := range all {
		var filtered []events.Event
		for _, ev := range evs {
			if ev.TS >= cutoff {
				filtered = append(filtered, ev)
			}
		}
		if len(filtered) == 0 {
			continue
		}
		summaries = append(summaries, summarizeLoop(sessionID, filtered))
	}
	// Most-recently-active loop first — the report is for a human skimming
	// "what's been happening," and recent activity is what they care about
	// most. Ties broken by session id for determinism (sort.Slice is not
	// stable across equal keys otherwise).
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].lastTS != summaries[j].lastTS {
			return summaries[i].lastTS > summaries[j].lastTS
		}
		return summaries[i].sessionID < summaries[j].sessionID
	})

	fmt.Fprintf(w, "fleetops report — since %s (%d loop%s)\n", sinceLabel, len(summaries), plural(len(summaries)))
	fmt.Fprintln(w, strings.Repeat("─", 60))
	if len(summaries) == 0 {
		fmt.Fprintln(w, "no history recorded in this window.")
		return nil
	}
	for _, s := range summaries {
		writeLoopSummary(w, s)
	}
	fmt.Fprintln(w, strings.Repeat("─", 60))
	writeFleetTotals(w, summaries)
	return nil
}

// summarizeLoop aggregates one session's (already time-filtered, still
// oldest-first per events.ReadAll's contract) events into a loopSummary.
//
// transitions counts every event whose FromState != ToState, REGARDLESS of
// trigger — this naturally covers scan-detected state changes, the
// governor's Stop promotion, AND a spawn's "" → <state> birth record, all of
// which are genuine state changes, without needing to enumerate Trigger
// values one by one. Oracle and actuation events always carry
// FromState==ToState (see their emitters' docs) and so never count here —
// they're judgments/actions, not transitions themselves.
func summarizeLoop(sessionID string, evs []events.Event) loopSummary {
	s := loopSummary{
		sessionID:         sessionID,
		actuationsByActor: map[string]int{},
		verdictsByOutcome: map[string]int{},
	}
	for _, ev := range evs {
		s.lastTS = ev.TS
		s.lastState = ev.ToState

		if ev.FromState != ev.ToState {
			s.transitions++
			if ev.ToState == string(domain.StateGate) && ev.FromState != string(domain.StateGate) {
				s.gatesOpened++
			}
			if ev.FromState == string(domain.StateGate) && ev.ToState != string(domain.StateGate) {
				s.gatesAnswered++
			}
		}

		switch ev.Trigger {
		case events.TriggerActuation:
			s.actuationsByActor[string(ev.Actor)]++
		case events.TriggerOracle:
			if outcome := verdictOutcome(ev.Detail); outcome != "" {
				s.verdictsByOutcome[outcome]++
			}
		}
	}
	return s
}

// verdictOutcome pulls the outcome word out of an oracle event's Detail
// field ("<outcome> at cycle <n>" — see judgeCmd's emitter), tolerating any
// unexpected shape by just returning Detail unsplit rather than dropping
// the count entirely.
func verdictOutcome(detail string) string {
	if i := strings.Index(detail, " at cycle"); i >= 0 {
		return detail[:i]
	}
	return detail
}

func writeLoopSummary(w io.Writer, s loopSummary) {
	fmt.Fprintf(w, "%s\n", s.sessionID)
	fmt.Fprintf(w, "  last state:   %s\n", orDash(s.lastState))
	fmt.Fprintf(w, "  transitions:  %d\n", s.transitions)
	fmt.Fprintf(w, "  gates:        %d opened, %d answered\n", s.gatesOpened, s.gatesAnswered)
	fmt.Fprintf(w, "  actuations:   %s\n", formatByKey(s.actuationsByActor))
	fmt.Fprintf(w, "  verdicts:     %s\n", formatByKey(s.verdictsByOutcome))
}

func writeFleetTotals(w io.Writer, summaries []loopSummary) {
	var transitions, gatesOpened, gatesAnswered int
	actuations := map[string]int{}
	verdicts := map[string]int{}
	for _, s := range summaries {
		transitions += s.transitions
		gatesOpened += s.gatesOpened
		gatesAnswered += s.gatesAnswered
		for k, n := range s.actuationsByActor {
			actuations[k] += n
		}
		for k, n := range s.verdictsByOutcome {
			verdicts[k] += n
		}
	}
	fmt.Fprintln(w, "FLEET TOTALS")
	fmt.Fprintf(w, "  loops:        %d\n", len(summaries))
	fmt.Fprintf(w, "  transitions:  %d\n", transitions)
	fmt.Fprintf(w, "  gates:        %d opened, %d answered\n", gatesOpened, gatesAnswered)
	fmt.Fprintf(w, "  actuations:   %s\n", formatByKey(actuations))
	fmt.Fprintf(w, "  verdicts:     %s\n", formatByKey(verdicts))
}

// formatByKey renders a label→count map as "<total> (<k>: <n>, ...)", keys
// sorted alphabetically for deterministic output — map iteration order is
// otherwise random, which would make both the report's real output and any
// test asserting on it flaky.
func formatByKey(counts map[string]int) string {
	if len(counts) == 0 {
		return "0"
	}
	keys := make([]string, 0, len(counts))
	total := 0
	for k, n := range counts {
		keys = append(keys, k)
		total += n
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s: %d", k, counts[k]))
	}
	return fmt.Sprintf("%d (%s)", total, strings.Join(parts, ", "))
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
