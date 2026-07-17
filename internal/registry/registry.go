// Package registry persists goal-bound loop metadata — the "we spawned this
// with a goal" record that lets the oracle (internal/oracle) judge whether
// a loop is done, making progress, or drifting. Observed sessions that
// weren't spawned via missionctl's "n" key have no record here, and render
// as "—" (unbound) in the TUI's ORACLE/N-I columns: verdicts only make
// sense against a known goal.
package registry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jitokim/missionctl/internal/domain"
	"github.com/jitokim/missionctl/internal/events"
)

// LoopsDir is ~/.missionctl/loops (override for tests by passing an
// explicit dir to the funcs below instead of calling this).
func LoopsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".missionctl", "loops")
}

// PendingDir is ~/.missionctl/pending (same override pattern as LoopsDir).
func PendingDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".missionctl", "pending")
}

const (
	DefaultMaxCycles      = 12
	DefaultNoImproveLimit = 3

	// pendingStaleAfter: a pending spawn nobody could match to a session
	// within this long is presumed failed (or landed somewhere BindPending
	// can't see) and is dropped rather than kept around forever.
	pendingStaleAfter = 10 * time.Minute
)

// Record is one goal-bound loop's persisted state.
//
// feat/panel-info (precise rename): Rubric was named Oracle until this
// slice — see domain.Goal's doc for the full rationale. The JSON tag on
// recordFile below stays "oracle" deliberately: an already-persisted
// record on someone's disk must keep loading correctly after this rename,
// not silently lose its rubric (see TestLoad_OracleJSONKeyCompat).
type Record struct {
	Goal           string
	DoneCondition  string // completion condition from the wizard's "n" key; "" = oracle judges against Goal alone
	Rubric         string // verification rubric (free text); "" = default "independent LLM judge against the complete condition"
	Challenger     string // adversarial probe description — STORED ONLY, never executed (no challenger phase yet)
	BoundAt        time.Time
	MaxCycles      int
	NoImproveLimit int
	Verdict        *domain.Verdict // nil if never judged
	NoImprove      int
	// Driven is LoopEngine's durable "this session is engine-owned" flag
	// (docs/design-loop-engine-mvp.md §1; docs/specs/seed-loop-engine-mvp-
	// 2026-07-17.md). false for every record created before this field
	// existed, and false by default even for a freshly-Bound record unless
	// the caller explicitly opts in (see BindSpec.Driven) — the captain's
	// opt-in-spike decision: no engine cycle EVER fires unless the loop was
	// explicitly created engine-driven, off by default. Copied onto
	// domain.Loop by claude.enrichFromRegistry, mirroring BoundAt's own
	// copy-in. MarkDriven flips it later (e.g. a take-over clearing it back
	// to false — a later slice, see the seed spec's attach-preservation AC).
	Driven bool
}

// recordFile is Record's on-disk JSON shape. The "oracle" JSON tag on
// Rubric is INTENTIONALLY unchanged from before the Go field rename (see
// Record's doc) — this is the on-disk compat seam.
type recordFile struct {
	Goal           string       `json:"goal"`
	DoneCondition  string       `json:"doneCondition,omitempty"`
	Rubric         string       `json:"oracle,omitempty"`
	Challenger     string       `json:"challenger,omitempty"`
	BoundAt        int64        `json:"boundAt"`
	MaxCycles      int          `json:"maxCycles"`
	NoImproveLimit int          `json:"noImproveLimit"`
	Verdict        *verdictFile `json:"verdict,omitempty"`
	NoImprove      int          `json:"noImprove"`
	Driven         bool         `json:"driven,omitempty"`
}

// BindSpec is a loop's goal-bound contract, as collected by the wizard (the
// tui's "n" key): what to do, how completion is verified, and the cycle
// ceiling. A small value object rather than more positional string params
// on Bind/WritePending — Goal/DoneCondition/Rubric/Challenger are all plain
// strings, easy to silently transpose if passed positionally.
type BindSpec struct {
	Goal          string
	DoneCondition string
	Rubric        string
	Challenger    string
	MaxCycles     int // 0 = DefaultMaxCycles
	// Driven opts this loop into LoopEngine at creation time — false (the
	// zero value) for every EXISTING caller of BindSpec today, since none
	// of them offer an engine-drive choice yet (that's a later slice's "n"
	// wizard step). Kept on BindSpec rather than a separate Bind parameter:
	// one value object for "everything the wizard decided about this
	// loop's contract," matching this struct's own doc above, and it
	// threads through WritePending/BindPending's existing plumbing for
	// free without a second parallel parameter list.
	Driven bool
}

type verdictFile struct {
	Outcome string `json:"outcome"`
	Reason  string `json:"reason"`
	AtCycle int    `json:"atCycle"`
	TS      int64  `json:"ts"`
}

// Bind creates a new registry record for sessionID from spec — called by
// BindPending once it matches a pending spawn to its session. spec.MaxCycles
// <= 0 falls back to DefaultMaxCycles (lets callers pass a zero-value spec
// field to mean "use the default" rather than requiring every caller to know
// the constant).
func Bind(dir, sessionID string, spec BindSpec) error {
	maxCycles := spec.MaxCycles
	if maxCycles <= 0 {
		maxCycles = DefaultMaxCycles
	}
	return writeRecordFile(dir, sessionID, recordFile{
		Goal:           spec.Goal,
		DoneCondition:  spec.DoneCondition,
		Rubric:         spec.Rubric,
		Challenger:     spec.Challenger,
		BoundAt:        time.Now().Unix(),
		MaxCycles:      maxCycles,
		NoImproveLimit: DefaultNoImproveLimit,
		Driven:         spec.Driven,
	})
}

// Load reads sessionID's record. ok=false if unbound (no record) or the
// file is unreadable/malformed.
func Load(dir, sessionID string) (Record, bool) {
	data, err := os.ReadFile(filepath.Join(dir, sessionID+".json"))
	if err != nil {
		return Record{}, false
	}
	var rf recordFile
	if err := json.Unmarshal(data, &rf); err != nil {
		return Record{}, false
	}
	return recordFromFile(rf), true
}

func recordFromFile(rf recordFile) Record {
	r := Record{
		Goal:           rf.Goal,
		DoneCondition:  rf.DoneCondition,
		Rubric:         rf.Rubric,
		Challenger:     rf.Challenger,
		BoundAt:        time.Unix(rf.BoundAt, 0),
		MaxCycles:      rf.MaxCycles,
		NoImproveLimit: rf.NoImproveLimit,
		NoImprove:      rf.NoImprove,
		Driven:         rf.Driven,
	}
	if rf.Verdict != nil {
		r.Verdict = &domain.Verdict{
			Outcome: domain.Outcome(rf.Verdict.Outcome),
			Reason:  rf.Verdict.Reason,
			AtCycle: rf.Verdict.AtCycle,
		}
	}
	return r
}

// SaveVerdict records the oracle's judgment for sessionID at atCycle, and
// updates the no-improve streak: a rejected verdict increments it (the
// agent claimed done but wasn't — no real step forward); done/progress
// resets it to 0 (either it's finished, or genuine progress happened,
// either way the stall streak is broken). Loads the existing record first
// so Goal/caps survive — returns an error if sessionID isn't bound yet
// (SaveVerdict never creates a record; Bind does that).
func SaveVerdict(dir, sessionID string, verdict domain.Verdict, atCycle int) error {
	rec, ok := Load(dir, sessionID)
	if !ok {
		return fmt.Errorf("registry: no record for session %s to attach a verdict to", sessionID)
	}
	rec.Verdict = &domain.Verdict{Outcome: verdict.Outcome, Reason: verdict.Reason, AtCycle: atCycle}
	if verdict.Outcome == domain.OutcomeRejected {
		rec.NoImprove++
	} else {
		rec.NoImprove = 0
	}
	return writeRecordFile(dir, sessionID, recordFile{
		Goal:           rec.Goal,
		DoneCondition:  rec.DoneCondition,
		Rubric:         rec.Rubric,
		Challenger:     rec.Challenger,
		BoundAt:        rec.BoundAt.Unix(),
		MaxCycles:      rec.MaxCycles,
		NoImproveLimit: rec.NoImproveLimit,
		NoImprove:      rec.NoImprove,
		Driven:         rec.Driven, // LoopEngine MVP: a verdict save must not silently un-drive a loop
		Verdict: &verdictFile{
			Outcome: string(rec.Verdict.Outcome),
			Reason:  rec.Verdict.Reason,
			AtCycle: rec.Verdict.AtCycle,
			TS:      time.Now().Unix(),
		},
	})
}

// MarkDriven flips sessionID's Driven flag — the LoopEngine MVP's
// engine-ownership toggle (set true at engine-bootstrap; cleared false by
// a later slice's take-over action, see the seed spec's attach-
// preservation AC). Operates on the raw on-disk shape directly (read-
// mutate-write, like SaveVerdict's Load-then-writeRecordFile shape, but at
// the recordFile level rather than round-tripping through Record) so
// every OTHER field — notably the verdict's on-disk TS, which
// Record/recordFromFile doesn't carry into memory at all today (TS is
// write-only in this format; nothing reads it back) — survives byte-for-
// byte untouched, not silently reset to "now" as a side effect of a
// completely unrelated flag flip. Returns an error if sessionID isn't
// bound yet (MarkDriven never creates a record, same contract as
// SaveVerdict).
func MarkDriven(dir, sessionID string, driven bool) error {
	data, err := os.ReadFile(filepath.Join(dir, sessionID+".json"))
	if err != nil {
		return fmt.Errorf("registry: no record for session %s to mark driven", sessionID)
	}
	var rf recordFile
	if err := json.Unmarshal(data, &rf); err != nil {
		return fmt.Errorf("registry: session %s's record is malformed: %w", sessionID, err)
	}
	rf.Driven = driven
	return writeRecordFile(dir, sessionID, rf)
}

func writeRecordFile(dir, sessionID string, rf recordFile) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(rf)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, sessionID+".json"), data, 0o644)
}

// PendingSpawn is a not-yet-matched spawn request.
type PendingSpawn struct {
	Cwd           string
	Goal          string
	DoneCondition string
	Rubric        string
	Challenger    string
	MaxCycles     int
	TS            time.Time
	// Driven carries BindSpec.Driven through the pending→bound round trip
	// (WritePending → BindPending → Bind) — without this, a loop spawned
	// with the engine-drive choice would silently lose it the moment its
	// session got matched, since BindPending rebuilds a fresh BindSpec from
	// PendingSpawn's fields, not from the original spec.
	Driven bool
}

// pendingFile is PendingSpawn's on-disk JSON shape — same "oracle" JSON
// tag compat seam as recordFile (see Record's doc).
type pendingFile struct {
	Cwd           string `json:"cwd"`
	Goal          string `json:"goal"`
	DoneCondition string `json:"doneCondition,omitempty"`
	Rubric        string `json:"oracle,omitempty"`
	Challenger    string `json:"challenger,omitempty"`
	MaxCycles     int    `json:"maxCycles,omitempty"`
	TS            int64  `json:"ts"`
	Driven        bool   `json:"driven,omitempty"`
}

// WritePending records a just-spawned loop's full contract (spec) under
// cwd, to be matched to its session id by the next scan (BindPending).
// Controller.Spawn's caller (tui's spawnCmd) has no way to know the new
// session id directly — Spawn just starts a process — so binding happens
// out-of-band here.
func WritePending(dir, cwd string, spec BindSpec) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(pendingFile{
		Cwd:           cwd,
		Goal:          spec.Goal,
		DoneCondition: spec.DoneCondition,
		Rubric:        spec.Rubric,
		Challenger:    spec.Challenger,
		MaxCycles:     spec.MaxCycles,
		TS:            time.Now().Unix(),
		Driven:        spec.Driven,
	})
	if err != nil {
		return err
	}
	name := strconv.FormatInt(time.Now().UnixNano(), 10) + ".json"
	return os.WriteFile(filepath.Join(dir, name), data, 0o644)
}

// listPending reads every pending file in dir, keyed by filename (so
// BindPending can delete the exact file it matched or judged stale).
func listPending(dir string) map[string]PendingSpawn {
	out := make(map[string]PendingSpawn)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var pf pendingFile
		if err := json.Unmarshal(data, &pf); err != nil {
			continue
		}
		out[e.Name()] = PendingSpawn{
			Cwd:           pf.Cwd,
			Goal:          pf.Goal,
			DoneCondition: pf.DoneCondition,
			Rubric:        pf.Rubric,
			Challenger:    pf.Challenger,
			MaxCycles:     pf.MaxCycles,
			TS:            time.Unix(pf.TS, 0),
			Driven:        pf.Driven,
		}
	}
	return out
}

// BindPending matches unmatched pending spawns to the newest not-yet-bound
// session sharing the pending spawn's cwd, whose log was written after the
// spawn fired (LastActivity > pending.TS — a scan-time proxy for "born from
// this spawn": not a birth-time proof, but robust in practice, since an
// unrelated pre-existing session in the same cwd won't happen to write to
// its own log in the same instant a brand new claude boots up there).
// Matched pendings are bound (see Bind) and their pending file removed.
// Pendings older than pendingStaleAfter are dropped unmatched — the spawn
// presumably failed, or landed somewhere BindPending can't see.
//
// event-log-and-notify: a successful match is also the natural, and only,
// place to record the "spawn" actuation event (events.TriggerActuation,
// actor=human — a human triggered the "n" key wizard that created this
// pending spawn) — at spawn TIME (tui's spawnCmd) no session_id exists yet
// to key an events.Append call on; this is the first moment one does. Best-
// effort, swallowed error (see internal/events package doc).
func BindPending(loopsDir, pendingDir string, loops []domain.Loop, now time.Time, historyDir string) {
	for name, p := range listPending(pendingDir) {
		if now.Sub(p.TS) > pendingStaleAfter {
			os.Remove(filepath.Join(pendingDir, name))
			continue
		}

		var best *domain.Loop
		for i := range loops {
			l := &loops[i]
			// Match in the lossless direction (real path → encoded), never
			// against l.Cwd: the decoded Cwd is lossy and may not be healed
			// yet when BindPending runs (scan order: bind → enrich →
			// liveness). A hyphenated worktree dir (mctl-...) never
			// string-matches its decode — this exact bug shipped once (live
			// worktree e2e caught it).
			if domain.EncodeCwd(p.Cwd) != l.ProjectDir {
				continue
			}
			if !l.LastActivity.After(p.TS) {
				continue
			}
			if _, bound := Load(loopsDir, l.SessionID); bound {
				continue
			}
			if best == nil || l.LastActivity.After(best.LastActivity) {
				best = l
			}
		}
		if best == nil {
			continue // no match yet; retry next scan (until stale)
		}
		spec := BindSpec{
			Goal:          p.Goal,
			DoneCondition: p.DoneCondition,
			Rubric:        p.Rubric,
			Challenger:    p.Challenger,
			MaxCycles:     p.MaxCycles,
			Driven:        p.Driven,
		}
		if err := Bind(loopsDir, best.SessionID, spec); err != nil {
			continue // best-effort; retry next scan
		}
		os.Remove(filepath.Join(pendingDir, name))
		_ = events.Append(historyDir, events.Event{
			TS:        now.UnixNano(),
			SessionID: best.SessionID,
			FromState: "", // brand new — nothing to transition FROM
			ToState:   best.StateString(),
			Trigger:   events.TriggerActuation,
			Detail:    "spawn: " + capDetail(p.Goal),
			Actor:     events.ActorHuman,
		})
	}
}

// capDetail bounds a free-text detail field's length (in RUNES, not bytes —
// a byte-index cut can slice a multi-byte character in half, e.g. Korean
// goal text, same hazard internal/tui.trunc's doc warns about) so one long
// goal string can't blow up a history line unreasonably.
const detailCap = 200

func capDetail(s string) string {
	r := []rune(s)
	if len(r) <= detailCap {
		return s
	}
	return string(r[:detailCap]) + "…"
}
