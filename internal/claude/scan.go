// Package claude turns Claude Code's own session logs into fleet state — the
// observation core (seed spec §Observe). Each session is a JSONL file under
// ~/.claude/projects/<proj>/<session>.jsonl; we read file mtime (last activity)
// and tail the last few KB for stall markers. No screen scraping.
package claude

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jitokim/fleetops/internal/domain"
	"github.com/jitokim/fleetops/internal/engine"
	"github.com/jitokim/fleetops/internal/events"
	"github.com/jitokim/fleetops/internal/gate"
	"github.com/jitokim/fleetops/internal/registry"
)

// IdleThreshold: no log write for this long ⇒ the loop is considered stuck.
var IdleThreshold = 4 * time.Minute

const tailBytes = 24 * 1024

// ProjectsDir is ~/.claude/projects (override for tests).
func ProjectsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

// ActiveWindow: only sessions written within this window are part of "the fleet".
// Long-running loops keep writing (so they stay in); old finished sessions fall out.
var ActiveWindow = 24 * time.Hour

// IncludeHidden: when false (default), sessions whose project dir encodes a
// hidden (dot-prefixed) path segment are filtered out. Claude Code encodes
// both "/" and "." as "-", so a dot-dir doubles up a dash, e.g.
// "/home/user/.someplugin/agent/sessions" → "-home-user--someplugin-agent-sessions".
// Those are headless/infra sessions (agent tooling like claude-mem's
// observer), not a human's loop, and otherwise drown out the real fleet.
// A future flag can flip this to see them.
var IncludeHidden = false

// DiscoverLoops scans session logs and derives current fleet state, keeping only
// sessions active within `within` (0 = keep all). Seed spec AC-1 + filter decision:
// "recent activity + not cleanly ended" — the window drops days-old noise.
func DiscoverLoops(now time.Time, within time.Duration) ([]domain.Loop, error) {
	root := ProjectsDir()
	matches, err := filepath.Glob(filepath.Join(root, "*", "*.jsonl"))
	if err != nil {
		return nil, err
	}
	gatesDir := gate.GatesDir()
	pending := gate.Pending(gatesDir)
	loops := make([]domain.Loop, 0, len(matches))
	for _, path := range matches {
		fi, err := os.Stat(path)
		if err != nil || fi.Size() == 0 {
			continue
		}
		if within > 0 && now.Sub(fi.ModTime()) > within {
			continue
		}
		if !IncludeHidden && isHiddenProjectDir(filepath.Base(filepath.Dir(path))) {
			continue
		}
		loops = append(loops, loopFromLog(path, fi, now, gatesDir, pending))
	}
	sort.Slice(loops, func(i, j int) bool {
		return loops[i].LastActivity.After(loops[j].LastActivity)
	})

	historyDir := events.HistoryDir()
	loopsDir := registry.LoopsDir()
	registry.BindPending(loopsDir, registry.PendingDir(), loops, now, historyDir)
	loops = enrichFromRegistry(loops, loopsDir, historyDir)

	live, liveOK := LiveClaudeCwds()
	loops = applyLiveness(loops, live, liveOK, historyDir, now, within)

	// Keep metricsCache bounded to sessions actually present in this scan —
	// otherwise it grows forever as old sessions age out of the window or
	// get deleted, over a long-running fleetops process.
	keep := make(map[string]bool, len(matches))
	for _, m := range matches {
		keep[m] = true
	}
	pruneMetricsCache(keep)

	return loops, nil
}

// enrichFromRegistry attaches goal-bound metadata (Name, Goal.Text/MaxCycles/
// NoImproveLimit, Last verdict, NoImprove, Driven) from the registry to
// each loop that has a record — observed (non-spawned) sessions have none
// and are left untouched (Goal.Text stays "", which the TUI treats as
// "unbound"; Driven stays false, since an unbound loop was never handed to
// the engine).
//
// A bound loop whose latest verdict was rendered AT this exact cycle
// (Verdict.AtCycle == Cycle — i.e. nothing has happened since it was
// judged) gets its State promoted to the oracle's conclusion: done →
// StateDone, rejected → StateDrift. "progress" leaves State as already
// classified (idle/running) — real work is happening, there's no terminal
// call to make yet. A verdict from an EARLIER cycle (AtCycle < Cycle) is
// still shown (Last stays populated for the ORACLE column) but does not
// override State — the loop has moved on since that judgment, and it's due
// to be judged again (see the TUI's judge trigger policy).
func enrichFromRegistry(loops []domain.Loop, loopsDir, historyDir string) []domain.Loop {
	for i := range loops {
		rec, ok := registry.Load(loopsDir, loops[i].SessionID)
		if !ok {
			continue
		}
		loops[i].Name = rec.Name
		loops[i].Goal.Text = rec.Goal
		loops[i].Goal.DoneWhen = rec.DoneCondition
		loops[i].Goal.Rubric = rec.Rubric
		loops[i].Goal.Challenger = rec.Challenger
		loops[i].Goal.MaxCycles = rec.MaxCycles
		loops[i].Goal.NoImproveLimit = rec.NoImproveLimit
		loops[i].NoImprove = rec.NoImprove
		loops[i].Last = rec.Verdict
		loops[i].BoundAt = rec.BoundAt
		// Driven is copied onto the in-memory Loop the SAME way BoundAt already is —
		// registry stays the durable source of truth, Loop.Driven is a
		// per-scan PROJECTION of it, never a second place engine-ownership
		// can independently drift. An unbound loop (the `!ok` continue
		// above) never reaches here, so it keeps Loop.Driven's zero value
		// (false) — exactly right, since it was never handed to the engine.
		loops[i].Driven = rec.Driven

		// A live gate always wins over a stale verdict: the loop is blocked
		// on a human decision RIGHT NOW, which is more urgent and more
		// current than a judgment rendered against an earlier cycle's
		// output. Without this guard, a bound loop that hit a fresh
		// permission prompt after being judged done/rejected would show
		// DONE/DRIFT instead of the ◆ GATE it's actually sitting in.
		if rec.Verdict != nil && rec.Verdict.AtCycle == loops[i].Cycle && loops[i].State != domain.StateGate {
			switch rec.Verdict.Outcome {
			case domain.OutcomeDone:
				loops[i].State = domain.StateDone
				loops[i].Stall = domain.StallNone
			case domain.OutcomeRejected:
				loops[i].State = domain.StateDrift
				loops[i].Stall = domain.StallNone
			}
		}

		applyGovernor(&loops[i], historyDir)
	}
	return loops
}

// applyGovernor enforces a bound loop's hard ceilings via
// internal/engine.Check — DESIGN.md §3: budget/max-cycles/no-improve must
// live in the runtime, not just as advisory numbers a human has to notice.
// Runs AFTER the verdict mapping above, so Check sees this cycle's final
// State/NoImprove.
//
// A live gate always wins (same reasoning as the verdict-vs-gate guard just
// above: a human decision pending RIGHT NOW outranks a governor verdict) and
// an already-terminal loop (done/failed/killed) is left alone — there's
// nothing left to enforce once a loop is conclusively finished. Neither
// carve-out is explicitly spelled out by the governor spec, but both mirror
// an already-established precedent in this file; flagged in the slice
// report as a judgment call.
//
// event-log-and-notify: the Stop branch (a real promotion to StateFailed)
// also records a events.TriggerGovernor history event — best-effort,
// swallowed error (see internal/events package doc). This is naturally
// edge-triggered with NO extra bookkeeping: the guard clause above already
// makes Stop unreachable on every SUBSEQUENT scan once State is
// StateFailed (Terminal() is true), so this fires exactly once per loop's
// lifetime. Escalate does NOT log an event — it doesn't change State (see
// its own case below), and would otherwise re-fire on every 3s poll for as
// long as the annotation persists, which is exactly the re-emit-every-poll
// noise the scanner's own transition detector (internal/tui) is designed to
// avoid.
func applyGovernor(l *domain.Loop, historyDir string) {
	if l.State == domain.StateGate || l.State.Terminal() {
		return
	}
	switch d := engine.Check(*l); d.Action {
	case engine.Stop:
		// fromState is captured via domain.StateString (not the bare
		// string(l.State) this used before the P2 review fix) since l.Stall
		// is untouched by this branch — a Stop out of a StateStalled loop
		// must still record which STALL KIND it was leaving, the same
		// "encode the kind into the persisted state" fix applied to every
		// other emitter (see domain.Loop.StateString's doc).
		fromStateStr := domain.StateString(l.State, l.Stall)
		l.State = domain.StateFailed
		l.Note = fmt.Sprintf("stopped: no improvement %d/%d", l.NoImprove, l.Goal.NoImproveLimit)
		_ = events.Append(historyDir, events.Event{
			TS:        time.Now().UnixNano(),
			SessionID: l.SessionID,
			FromState: fromStateStr,
			ToState:   l.StateString(),
			Trigger:   events.TriggerGovernor,
			Detail:    l.Note,
			Actor:     events.ActorSystem,
		})
	case engine.Escalate:
		switch d.Reason {
		case "budget exhausted":
			l.Note = "⚠ over budget"
		case "max cycles reached":
			l.Note = "⚠ max cycles reached"
		default:
			l.Note = "⚠ " + d.Reason
		}
	}
}

// drivenDormantStale bounds applyLiveness's LoopEngine dormancy exception
// (see its doc): a Driven+Idle loop with no live process is held as
// dormant for up to this long since its last activity — past it, presumed
// genuinely dead (not merely resting between cycles) and surfaces as
// StateStalled/StallGone like any other presumed-dead loop, so a truly
// stuck engine loop is never silently invisible forever.
const drivenDormantStale = 15 * time.Minute

// applyLiveness cross-checks each loop against live `claude` CLI processes
// in its cwd — the JSONL alone can't tell "waiting for human" (idle) from
// "process dead" (terminal closed/crashed): both just stop writing. loops
// must already be sorted by LastActivity desc (as DiscoverLoops does), so
// within any cwd the earliest-indexed entries are the most recently active
// ones — no extra sort needed here.
//
// live is keyed by REAL (unencoded) lsof cwd paths (see LiveClaudeCwds), not
// by a loop's lossily-decoded Cwd — decodeCwd can't tell a "-" that was
// originally "/" from one that was originally in the directory name itself
// (e.g. "my-app"), so matching against it would silently miss real
// directories. Instead each live real path is re-encoded with encodeCwd
// (Claude Code's own "/" and "." → "-" scheme) and matched against the
// loop's ProjectDir, which IS that raw encoded string — lossless in this
// direction. ok=false (the ps/lsof probe itself failed) short-circuits to
// "leave the fleet exactly as classified" — see LiveClaudeCwds: a probe
// failure is not evidence of anything, and must never be treated as "0 live
// processes", which would wrongly mark the entire fleet StallGone/dropped.
//
// Per ProjectDir, the live count of most-recently-active loops are left
// untouched (there's a real process behind them). The rest are presumed
// dead, and — fix/killed-state — FIRST checked for a human kill decision
// (mostRecentActuationIsKill) before any of the rules below apply: a
// killed loop becomes StateKilled regardless of what it would otherwise
// have been, since a human's kill is definitive and must win over even the
// settled-verdict exemption (case in point — the bug this fixes: killing a
// StateDrift loop left it showing ✗ DRIFT forever, because the exemption
// below meant nothing ever re-examined it once "settled"). Absent a kill,
// the pre-existing rules apply:
//   - StateIdle (finished its turn, then the process went away) → dropped
//     entirely: the loop ended cleanly, it's not part of the fleet anymore.
//     EXCEPTION: a Driven loop's headless bootstrap/cycle has NO live process BETWEEN
//     turns by design (claude -p exits once each cycle finishes) — this
//     drop rule would otherwise make an engine loop vanish from the fleet
//     the instant each cycle's process exits. A Driven+Idle loop is held
//     as dormant (State stays Idle, NOT dropped) instead, UNLESS it's gone
//     quiet longer than drivenDormantStale — then it's presumed genuinely
//     dead, not merely resting between cycles, and falls through to the
//     SAME StateStalled/StallGone treatment as any other presumed-dead
//     loop (never dropped either way, once Driven: dropping would let a
//     truly stuck engine loop silently vanish instead of surfacing it).
//   - any LoopState.Terminal() state — StateDone (the oracle converged
//     it), StateFailed (the governor stopped it), StateKilled (a human
//     ended it) → left alone, dropped or demoted by neither rule: that's
//     the FINAL record of a judgment, not an incident, and the process
//     exiting afterwards is the expected epilogue.
//     StateDrift is NOT covered by this (it used to share an arm with
//     StateDone): drift is non-final, it asks for a re-drive, and a dead
//     process can't be re-driven in place — so it takes the
//     reclassification below. See the guard's own comment and
//     design-loop-state-model.md §4.
//   - anything else (StateDrift, StateStalled, or StateRunning past the live count —
//     e.g. a process that just died mid-turn) → kept, reclassified
//     StateStalled/StallGone: a mid-work death IS an incident. Applies to
//     Driven loops exactly the same as observed ones — the dormancy
//     exception above is StateIdle-specific.
//
// Bonus: whenever a ProjectDir has ANY live process backing it (regardless
// of which specific loop in the group that process belongs to), every loop
// sharing that ProjectDir gets its Cwd healed to the confirmed-real lsof
// path (overwriting the lossy decode) and CwdVerified set — the directory
// itself is confirmed real, independent of which loop instance is live.
//
// Collision guard: encodeCwd is many-to-one (both "/" and "." collapse to
// "-"), so two DISTINCT real directories — e.g. /x/foo-bar and /x/foo.bar —
// can map to the SAME ProjectDir. When that happens for a given ProjectDir,
// which real path is "the" real one is genuinely ambiguous, so healing is
// skipped entirely for it: Cwd stays the lossy decode and CwdVerified stays
// false, rather than risk silently healing to the WRONG one of the two.
//
// historyDir/now/within are threaded through purely for
// mostRecentActuationIsKill — only consulted for loops that reach this
// function's "presumed dead" branch (idxs[k:] below), never for the whole
// fleet every scan (the review's efficiency ask): a loop with a live
// process backing it never needs its history checked at all.
func applyLiveness(loops []domain.Loop, live map[string]int, ok bool, historyDir string, now time.Time, within time.Duration) []domain.Loop {
	if !ok {
		return loops // probe failed — do not reclassify the fleet on no data (P1-2)
	}

	liveCountByProjectDir := make(map[string]int)
	realPathByProjectDir := make(map[string]string)
	collidedProjectDir := make(map[string]bool) // ProjectDir reached from >1 distinct real path
	for realPath, count := range live {
		pd := encodeCwd(realPath)
		liveCountByProjectDir[pd] += count
		if existing, seen := realPathByProjectDir[pd]; seen && existing != realPath {
			collidedProjectDir[pd] = true
		}
		realPathByProjectDir[pd] = realPath
	}

	byProjectDir := make(map[string][]int)
	for i, l := range loops {
		byProjectDir[l.ProjectDir] = append(byProjectDir[l.ProjectDir], i)
	}

	drop := make(map[int]bool, len(loops))
	for pd, idxs := range byProjectDir {
		if realPath, matched := realPathByProjectDir[pd]; matched && !collidedProjectDir[pd] {
			for _, i := range idxs {
				loops[i].Cwd = realPath
				loops[i].CwdVerified = true
			}
		}

		k := liveCountByProjectDir[pd]
		// fix/exit-gate-ux (architecture judge item D): a collided
		// ProjectDir's summed live count is unattributable — encodeCwd
		// collapsed TWO DISTINCT real directories into this one pd, so we
		// have no way to tell which of the group's loop entries the
		// counted live processes actually back (same ambiguity the
		// healing guard above already refuses to resolve — see its doc).
		// Trusting the raw sum here would let a genuinely-dead loop in
		// one colliding dir "borrow" aliveness from an UNRELATED live
		// process in the OTHER colliding dir, escaping StallGone purely
		// by coincidence of arithmetic. Treat it as zero evidence instead:
		// every loop sharing this ProjectDir goes through the SAME
		// "presumed dead" scrutiny below (kill-check, idle-drop,
		// terminal-state exemption, else Gone) that an under-backed group
		// already gets — never silently exempted.
		if collidedProjectDir[pd] {
			k = 0
		}
		if k >= len(idxs) {
			continue // enough live processes for every loop sharing this dir
		}
		for _, i := range idxs[k:] {
			if mostRecentActuationIsKill(historyDir, loops[i].SessionID, now, within) {
				loops[i].State = domain.StateKilled
				loops[i].Stall = domain.StallNone
				continue // wins over every rule below — see doc
			}
			// Rung 1 of the precedence ladder
			// (design-loop-state-model.md §4): A SETTLED ENDING IS
			// FINAL. done/failed/killed are LoopState.Terminal(), and
			// for all three the process subsequently exiting is the
			// EXPECTED epilogue, not new information — the oracle
			// converged it, the governor stopped it, or a human ended
			// it, and none of those judgments becomes less true because
			// the OS reaped the process afterwards. So the observed
			// death adds nothing and the ending stands.
			//
			// Expressed as Terminal() rather than as a list of case
			// arms deliberately: the rule IS "final beats non-final",
			// and a second hand-written enumeration of which states are
			// final is exactly how the two lists drift apart.
			//
			// StateDrift deliberately does NOT get this treatment (it
			// used to share an arm with StateDone — defect #1). Drift is
			// NOT final. It means "the oracle rejected the agent's
			// claim, re-drive this loop", which is a statement about a
			// loop still supposed to be workable, and a dead process
			// cannot be re-driven in place. So the second half of the
			// rule applies — among NON-final states, observed beats
			// inferred — and drift falls through to the demotion below.
			//
			// The rejected verdict itself is not lost, though it is
			// demoted: enrichFromRegistry already copied it onto
			// Loop.Last, and the ORACLE row and the VERDICTS block are
			// state-independent, so both still render it. What DOES
			// change is the callout, which dispatches on State — the
			// drift callout headlining the oracle's reason is replaced
			// by the gone/restart callout, so the reason moves from
			// headline to detail. That is the intended trade: "this
			// process is dead" is the more actionable headline, and the
			// reason stays one glance away.
			if loops[i].State.Terminal() {
				continue
			}
			switch loops[i].State {
			case domain.StateIdle:
				if loops[i].Driven {
					if now.Sub(loops[i].LastActivity) <= drivenDormantStale {
						continue // dormant — held as Idle, awaiting the engine's next drive
					}
					loops[i].State = domain.StateStalled
					loops[i].Stall = domain.StallGone
					continue
				}
				drop[i] = true
			default:
				loops[i].State = domain.StateStalled
				loops[i].Stall = domain.StallGone
			}
		}
	}
	if len(drop) == 0 {
		return loops
	}

	out := make([]domain.Loop, 0, len(loops)-len(drop))
	for i, l := range loops {
		if !drop[i] {
			out = append(out, l)
		}
	}
	return out
}

// mostRecentActuationIsKill reports whether sessionID's most recent
// actor=human actuation event within `within` of now was a kill that was
// CONFIRMED DISPATCHED.
//
// Issue #50: this used to be strings.HasPrefix(Detail, "kill ") with no
// success filter, and logActuationEvent writes a failed kill's Detail as
// "kill <tier> failed: <err>" — which that prefix also matches. So a kill
// the human was explicitly told had FAILED still promoted the loop to
// StateKilled once its process was later observed gone (for any reason),
// after which `k` refused it as already killed. "The human pressed k" is
// not "the kill landed."
//
// The fix is structural, not a longer string match: the outcome now
// travels in Event.Outcome, and only events.OutcomeOK counts. Two
// consequences, both deliberate:
//
//   - events.OutcomeUnknown (ErrSendDeliveryUnknown — the host send timed
//     out) does NOT count. It is genuinely neither confirmed-sent nor
//     confirmed-failed, and StateKilled is an ASSERTION that a human ended
//     this loop; an unconfirmed send does not license it. Declining to
//     assert costs only that the loop reads "gone" rather than "killed"
//     once its process exits, and it keeps `k` available for the human the
//     TUI just told to "attach and check before pressing k again" — whereas
//     counting it would make the loop unkillable on the strength of
//     something we did not observe.
//   - events written before Outcome existed carry "", so they do not count
//     either. A kill recorded by an older build stops being replayed as
//     StateKilled; the loop shows the ordinary gone treatment instead. That
//     is a bounded, one-time, fail-toward-not-claiming regression (the
//     window is `within`, 24h by default) and is the correct direction for
//     a field whose whole purpose is to stop claiming more than we know.
//
// Residual, named rather than hidden: WHICH ACTION the event records is
// still read out of Detail's prefix. That is a weaker dependency (the
// action word is chosen by logActuationEvent's callers from a fixed set and
// is always first) and is not what let the bug through, but structuring it
// too would be the right follow-up.
//
// Deliberately the MOST RECENT one, not "was there ever a
// kill": if a kill was followed by a later successful resume/inject
// actuation (reviving the session before this fix landed, or via a race),
// that later actuation — not the stale kill — is what should win. events.Read
// is scoped to just this one session (not internal/events.ReadAll's
// whole-directory scan), so this stays cheap even called once per
// process-gone candidate loop per scan.
func mostRecentActuationIsKill(historyDir, sessionID string, now time.Time, within time.Duration) bool {
	evs, err := events.Read(historyDir, sessionID)
	if err != nil {
		return false
	}
	// within<=0 means "no window" (DiscoverLoops' own "0 = keep all"
	// convention) — MinInt64 so every event passes rather than every event
	// being (wrongly) treated as expired.
	cutoff := int64(math.MinInt64)
	if within > 0 {
		cutoff = now.Add(-within).UnixNano()
	}
	var lastActuation *events.Event
	for i := range evs {
		ev := &evs[i]
		if ev.Trigger != events.TriggerActuation || ev.Actor != events.ActorHuman || ev.TS < cutoff {
			continue
		}
		if lastActuation == nil || ev.TS > lastActuation.TS {
			lastActuation = ev
		}
	}
	if lastActuation == nil {
		return false
	}
	return lastActuation.Outcome == events.OutcomeOK && strings.HasPrefix(lastActuation.Detail, "kill ")
}

// isHiddenProjectDir reports whether an encoded project dir contains a
// dot-prefixed path segment (see IncludeHidden): "/" and "." both encode to
// "-", so a hidden dir shows up as a double dash.
func isHiddenProjectDir(dir string) bool {
	return strings.Contains(dir, "--")
}

func loopFromLog(path string, fi os.FileInfo, now time.Time, gatesDir string, pending map[string]gate.Info) domain.Loop {
	projectDir := filepath.Base(filepath.Dir(path))
	proj := projectLabel(projectDir)
	session := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	last := fi.ModTime()
	idleFor := now.Sub(last)

	l := domain.Loop{
		ID:           session,
		Project:      proj,
		ProjectDir:   projectDir,
		Cwd:          decodeCwd(projectDir),
		SessionID:    session,
		Path:         path,
		LastActivity: last,
		State:        domain.StateRunning, // fallback if the tail can't be read at all
	}

	l.Cycle, l.TokensSpent = SessionMetrics(path)
	if l.Goal.BudgetTokens == 0 {
		l.Goal.BudgetTokens = DefaultBudgetTokens // v0 default until per-loop budgets exist
	}

	// One shared tail read serves classification, the detail pane's TAIL row
	// (LastText), AND the AskUserQuestion gate check further below — avoid
	// reading the file more than once. Classification always runs (not just
	// once "idle"): "running" means a turn is genuinely in flight, not merely
	// "wrote recently" (see classifyLoop) — a loop that finished its turn a
	// second ago is idle, not running.
	buf, haveTail := readTail(path, tailBytes)
	if haveTail {
		if text, ok := lastAssistantTextFromTail(buf); ok {
			l.LastText = text
		} else if biggerBuf, ok := readTail(path, lastTextTailBytes); ok {
			// F2: the standard tail had NO assistant text at all — a busy
			// loop's large tool_use/tool_result payloads can fill the
			// whole small window without a single text block, even though
			// a perfectly good "what it was doing" report sits just
			// outside it. Widen the search specifically for LastText;
			// classification below still runs on the cheaper, smaller buf
			// (it doesn't need this — see classifyLoop).
			if text, ok := lastAssistantTextFromTail(biggerBuf); ok {
				l.LastText = text
			}
		}
		l.State, l.Stall = classifyLoop(buf, idleFor)
	} else if idleFor >= IdleThreshold {
		l.State, l.Stall = domain.StateStalled, domain.StallNoOutput
	}

	// A pending Notification-hook marker beats any tail heuristic above —
	// but only when it's actually asking for a decision. Claude Code fires
	// the SAME hook for the 60s "Claude is waiting for your input" idle
	// notification, which is NOT a gate (verified live). The official
	// notification_type field is the authoritative signal
	// (permission_prompt/elicitation_dialog/agent_needs_input all mean
	// "blocked on a human"; idle_prompt and anything else don't); older
	// claude versions that omit it (Type == "") fall back to the
	// message-contains-"permission" heuristic. Anything that isn't a gate
	// falls through to the normal tail classification above (→ Idle) and
	// the marker is best-effort deleted so it doesn't linger.
	if info, ok := pending[session]; ok {
		if gate.IsGateActive(info.TS, last) && isGateNotification(info) {
			l.State = domain.StateGate
			l.Stall = domain.StallNone
			l.GatePrompt = info.Message
			l.GateTS = info.TS.UnixNano() // lets approveCmd compare-and-swap delete only the marker this decision was based on
		} else {
			// Compare-and-swap: only delete the marker this scan actually
			// judged stale/non-gate. A plain delete-by-name could destroy a
			// BRAND NEW marker that landed between the Pending() snapshot
			// above and this delete (e.g. the human answered, then a fresh
			// permission prompt fired moments later) — see gate.DeleteMarkerIfTS.
			gate.DeleteMarkerIfTS(gatesDir, session, info.TS.UnixNano())
		}
	}

	// A pending AskUserQuestion is a gate the Notification-hook path above
	// structurally cannot see: AskUserQuestion never fires a Notification hook
	// (confirmed upstream gap, anthropics/claude-code#59908), so no marker ever
	// lands for it, and its tool_use turn otherwise falls through to
	// StateStalled/no-output — indistinguishable from a genuinely hung session.
	// Detect it straight from the tail instead. Like a hook gate it's
	// unambiguously "blocked on a human" the moment it appears, so it applies
	// IMMEDIATELY — not gated behind the idle stall timeout (same reasoning as
	// the marker check's IsGateActive: a pending human decision is not a
	// recency question), which is why this overrides an otherwise-Running tail
	// too. A real hook-marker gate set just above still wins (don't clobber an
	// already-set StateGate); a genuinely finished turn (StateIdle) has no
	// pending question by construction. No GateTS is set — there's no marker
	// file to compare-and-swap delete, and approveCmd treats GateTS==0 as a
	// no-op delete (it still sends the approve keystroke to the surface).
	if haveTail && l.State != domain.StateGate && l.State != domain.StateIdle {
		if question, ok := pendingAskUserQuestion(buf); ok {
			l.State = domain.StateGate
			l.Stall = domain.StallNone
			l.GatePrompt = question
		}
	}
	return l
}

// gateNotificationTypes are Claude Code's notification_type values that mean
// "blocked on a human decision" — the rest (idle_prompt, auth_success, etc.)
// are informational, not a gate.
var gateNotificationTypes = map[string]bool{
	"permission_prompt":  true,
	"elicitation_dialog": true,
	"agent_needs_input":  true,
}

// isGateNotification decides whether a marker represents a real gate.
// Type is authoritative when present; when empty (older claude versions
// that predate notification_type), falls back to a message-text heuristic.
func isGateNotification(info gate.Info) bool {
	if info.Type != "" {
		return gateNotificationTypes[info.Type]
	}
	return strings.Contains(strings.ToLower(info.Message), "permission")
}

// tailState reads the tail of the session log and classifies it given how
// long it's been since the last write (see classifyLoop). Exposed for
// tests; loopFromLog itself calls classifyLoop directly since it already
// holds the tail buffer from the LastText read (avoids a second file read).
func tailState(path string, idleFor time.Duration) (domain.LoopState, domain.StallKind) {
	buf, ok := readTail(path, tailBytes)
	if !ok {
		return domain.StateStalled, domain.StallNoOutput
	}
	return classifyLoop(buf, idleFor)
}

// backgroundLaunchToolName is the tool a session uses to start work that
// outlives its own turn.
const backgroundLaunchToolName = "Agent"

// toolUseIDPattern extracts the tool-use id a completion notification carries,
// which is what lets a launch be PAIRED with its completion rather than merely
// counted. Counting would misread two launches and one completion as "all
// done" the moment the totals happened to line up.
var toolUseIDPattern = regexp.MustCompile(`<tool-use-id>(toolu_[A-Za-z0-9]+)</tool-use-id>`)

// outstandingBackgroundWork reports whether the tail shows background work
// that was launched and has not reported back.
//
// A background launch is an assistant tool_use for backgroundLaunchToolName
// whose input sets run_in_background; its completion arrives later as a
// notification carrying the SAME tool-use id. Unmatched launch ⇒ the session
// is waiting on itself, not on a human.
//
// # Tail truncation, and which way it fails
//
// This sees only the tail readTail kept, so a launch older than that window is
// invisible and the session reads as idle — exactly today's behaviour, so the
// degradation is graceful rather than a new wrong answer. The opposite error
// (a launch visible but its completion scrolled past) would report work that
// is actually finished, but it is close to impossible by construction: the
// completion is always LATER in the file than its launch, so any window
// holding the launch holds the completion too.
//
// Deliberately tolerant of shape: unparseable lines are skipped rather than
// failing the whole scan, matching lastTurnEnded's posture on a tail whose
// first line is usually cut mid-record.
func outstandingBackgroundWork(buf []byte) bool {
	launched := map[string]bool{}
	notified := map[string]bool{}

	for _, line := range strings.Split(string(buf), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// The completion notification is matched against the RAW line: it
		// arrives as injected prose whose exact envelope is not ours to
		// depend on, so pattern-matching the id is more durable than
		// decoding a structure that may change shape.
		for _, m := range toolUseIDPattern.FindAllStringSubmatch(line, -1) {
			notified[m[1]] = true
		}

		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if t, _ := entry["type"].(string); t != "assistant" {
			continue
		}
		msg, ok := entry["message"].(map[string]any)
		if !ok {
			continue
		}
		blocks, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, b := range blocks {
			block, ok := b.(map[string]any)
			if !ok {
				continue
			}
			if bt, _ := block["type"].(string); bt != "tool_use" {
				continue
			}
			if name, _ := block["name"].(string); name != backgroundLaunchToolName {
				continue
			}
			input, ok := block["input"].(map[string]any)
			if !ok {
				continue
			}
			if bg, _ := input["run_in_background"].(bool); !bg {
				continue
			}
			if id, _ := block["id"].(string); id != "" {
				launched[id] = true
			}
		}
	}

	for id := range launched {
		if !notified[id] {
			return true
		}
	}
	return false
}

// classifyLoop is tailState's buffer-only core. "Running" means "a turn is
// in flight", not just "the log was touched recently", so a finished turn
// is idle regardless of how long ago that was:
//   - the last meaningful (user/assistant) entry is an assistant message
//     whose turn finished (stop_reason "end_turn"), AND no background work
//     it launched is still outstanding ⇒ StateIdle: waiting on a human, not
//     stuck — not an incident, no matter the recency.
//   - otherwise (mid-turn: last entry is user/tool_result, or an assistant
//     message that hasn't finished, e.g. tool_use, or a finished turn with
//     background work still outstanding):
//   - idleFor < IdleThreshold ⇒ StateRunning: genuinely still working.
//   - idleFor >= IdleThreshold ⇒ StateStalled (a rate-limit marker
//     anywhere in the tail ⇒ StallRateLimit, else StallNoOutput).
//
// # Why a finished turn is not always idle
//
// StateIdle asserts something specific: "waiting on a human." When a session
// launches a background agent it ENDS ITS TURN and then waits for that agent,
// so the transcript looks exactly like an idle session — but the human has
// nothing to do, and the session will wake itself. Reported live: a session
// sat on the fleet list as idle for many minutes while a background agent
// worked, and the operator reasonably read that as "it's my move."
//
// Treating an outstanding launch as "the turn hasn't really ended" puts it
// back on the existing ladder, which gets both cases right without a new
// state: still-recent reads as running (true — work is in flight), and a long
// silence reads as StalledNoOutput (also true, and the more useful reading,
// because a background agent that dies leaves its launcher waiting forever —
// which happened twice in the same session that reported this).
func classifyLoop(buf []byte, idleFor time.Duration) (domain.LoopState, domain.StallKind) {
	if lastTurnEnded(buf) && !outstandingBackgroundWork(buf) {
		return domain.StateIdle, domain.StallNone
	}
	if idleFor < IdleThreshold {
		return domain.StateRunning, domain.StallNone
	}
	if hasRateLimitMarker(buf) {
		return domain.StateStalled, domain.StallRateLimit
	}
	return domain.StateStalled, domain.StallNoOutput
}

// readTail reads the last n bytes of path (or the whole file if smaller).
func readTail(path string, n int64) ([]byte, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, false
	}
	start := int64(0)
	if fi.Size() > n {
		start = fi.Size() - n
	}
	buf := make([]byte, fi.Size()-start)
	if _, err := f.ReadAt(buf, start); err != nil {
		return nil, false
	}
	return buf, true
}

// hasRateLimitMarker looks for a recent rate-limit marker in the tail.
//
// fix/exit-gate-ux (architecture judge item C): this used to match
// "rate limit"/"rate-limit"/"429 "/"usage limit" as BARE substrings
// anywhere in the tail — the SAME false-positive class internal/claude.
// LastError was fixed for (see apiErrorMarker's doc): ordinary conversation
// can mention a status code or discuss rate limiting without any actual
// error occurring at all (this repo's own transcripts are a real example —
// a status update mentioning the "429 auto-redrive" FEATURE by name). A
// transcript merely discussing rate limits must not get StallRateLimit.
//
// Two independent, tightened signals now gate a match:
//  1. a genuinely structured API error shape — `"status":429` or
//     `"type":"rate_limit_error"` (Anthropic's actual error-type slug) —
//     safe as bare substrings since this exact key:value JSON shape
//     essentially never occurs in ordinary prose.
//  2. Claude Code's own synthesized "API Error" marker (apiErrorMarker)
//     PLUS a rate-limit-specific word in the SAME tail — i.e. a bare
//     "429"/"rate limit"/"usage limit" mention only counts when there's
//     also a genuine synthesized error entry present, never on its own.
func hasRateLimitMarker(buf []byte) bool {
	s := strings.ToLower(string(buf))
	if strings.Contains(s, "\"status\":429") || strings.Contains(s, "\"type\":\"rate_limit_error\"") {
		return true
	}
	if !strings.Contains(s, apiErrorMarker) {
		return false
	}
	return strings.Contains(s, "rate limit") ||
		strings.Contains(s, "rate-limit") ||
		strings.Contains(s, "429") ||
		strings.Contains(s, "usage limit")
}

// lastTurnEnded reports whether the last parseable user/assistant entry in
// the tail is an assistant message whose turn finished (stop_reason
// "end_turn"). A possibly-truncated first line in the tail buffer simply
// fails to parse and is skipped, same tolerance as LastUserPrompt.
func lastTurnEnded(buf []byte) bool {
	var last map[string]any
	for _, line := range strings.Split(string(buf), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if t, _ := entry["type"].(string); t == "user" || t == "assistant" {
			last = entry
		}
	}
	if last == nil || last["type"] != "assistant" {
		return false
	}
	msg, ok := last["message"].(map[string]any)
	if !ok {
		return false
	}
	stopReason, _ := msg["stop_reason"].(string)
	return stopReason == "end_turn"
}

// pendingAskUserQuestion reports whether the tail's last user/assistant entry
// is an unanswered AskUserQuestion tool_use, and if so the first question's
// text (already bounded for GatePrompt). AskUserQuestion — Claude Code's
// interactive numbered-choice prompt for a structured human decision — never
// fires a Notification hook (confirmed upstream gap, anthropics/claude-code
// #59908), so the gate.Pending marker path can't catch it; its tool_use turn
// (stop_reason "tool_use", not "end_turn") otherwise classifies as
// StateStalled/no-output, indistinguishable from a genuinely hung session.
//
// Same tolerant tail scan as lastTurnEnded: a possibly-truncated first line
// simply fails to parse and is skipped, and only user/assistant entries are
// kept, so the non-turn system/attachment noise Claude Code appends AFTER a
// pending question (e.g. periodic task_reminder attachments) is ignored rather
// than mistaken for the last turn. If this assistant AskUserQuestion is
// genuinely the last user/assistant entry, no later user tool_result answered
// it (an answer would BE the last user entry, so last["type"] would be "user"
// and this returns false). Any missing/malformed shape yields ("", false) —
// never a panic, matching this file's tolerant-parse discipline.
func pendingAskUserQuestion(buf []byte) (string, bool) {
	var last map[string]any
	for _, line := range strings.Split(string(buf), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if t, _ := entry["type"].(string); t == "user" || t == "assistant" {
			last = entry
		}
	}
	if last == nil || last["type"] != "assistant" {
		return "", false
	}
	msg, ok := last["message"].(map[string]any)
	if !ok {
		return "", false
	}
	content, ok := msg["content"].([]any)
	if !ok {
		return "", false
	}
	for _, block := range content {
		b, ok := block.(map[string]any)
		if !ok {
			continue
		}
		if b["type"] != "tool_use" || b["name"] != "AskUserQuestion" {
			continue
		}
		if question, ok := firstAskUserQuestionText(b); ok {
			return summarizeTailText(question, tailTextCap), true
		}
	}
	return "", false
}

// firstAskUserQuestionText pulls input.questions[0].question out of an
// AskUserQuestion tool_use block, tolerating any missing/wrong-typed shape (a
// malformed block is treated as "no pending question", never a panic).
func firstAskUserQuestionText(block map[string]any) (string, bool) {
	input, ok := block["input"].(map[string]any)
	if !ok {
		return "", false
	}
	questions, ok := input["questions"].([]any)
	if !ok || len(questions) == 0 {
		return "", false
	}
	first, ok := questions[0].(map[string]any)
	if !ok {
		return "", false
	}
	text, ok := first["question"].(string)
	if !ok || text == "" {
		return "", false
	}
	return text, true
}

// projectLabel turns "-home-user-myproject" into "myproject".
func projectLabel(dir string) string {
	parts := strings.Split(strings.Trim(dir, "-"), "-")
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i] != "" {
			return parts[i]
		}
	}
	return dir
}

// decodeCwd best-effort reverses the "/" → "-" project-dir encoding, for
// display only. Lossy when a path segment itself contains "-"; ProjectDir
// (the raw encoded string) is the source of truth for matching, see
// internal/control.
func decodeCwd(dir string) string {
	return "/" + strings.ReplaceAll(strings.TrimPrefix(dir, "-"), "-", "/")
}

// encodeCwd applies Claude Code's own project-dir encoding to a real
// (unencoded) absolute path — both "/" AND "." become "-" (verified:
// "/home/user/.someplugin/agent-sessions" →
// "-home-user--someplugin-agent-sessions"). This is the lossless
// direction (unlike decodeCwd): encoding a known-real path can be compared
// exactly against a loop's ProjectDir, which is why applyLiveness uses this
// instead of decoding ProjectDir and fuzzy-matching against a live path.
func encodeCwd(realPath string) string {
	return domain.EncodeCwd(realPath)
}

// LastUserPrompt returns the text of the last user message in a Claude Code
// session log, for re-sending on resume (DESIGN.md: resume re-drives the
// loop rather than restarting it). ok is false if the file has no user
// message (or can't be read).
func LastUserPrompt(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)

	last := ""
	found := false
	for scanner.Scan() {
		var entry map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if entry["type"] != "user" {
			continue
		}
		if text, ok := userMessageText(entry); ok && text != "" {
			last = text
			found = true
		}
	}
	return last, found
}

// userMessageText extracts the text of a user transcript entry's
// message.content, which is either a plain string or an array of content
// blocks (text blocks have "type":"text").
func userMessageText(entry map[string]any) (string, bool) {
	msg, ok := entry["message"].(map[string]any)
	if !ok {
		return "", false
	}
	switch content := msg["content"].(type) {
	case string:
		return content, content != ""
	case []any:
		for _, block := range content {
			b, ok := block.(map[string]any)
			if !ok {
				continue
			}
			if b["type"] != "text" {
				continue
			}
			if text, ok := b["text"].(string); ok && text != "" {
				return text, true
			}
		}
	}
	return "", false
}

// LastAssistantText returns the last assistant message's text (first
// tailTextCap chars, newlines collapsed to spaces) from the tail of the
// session log — "what was it last doing", shown in the detail pane's TAIL
// row. ok is false only when EVEN the widened search (see widenedTailRead)
// finds no assistant text at all. Thin path-based wrapper around
// lastAssistantTextFromTail, which loopFromLog calls directly against a
// tail buffer it already read (see readTail).
func LastAssistantText(path string) (string, bool) {
	return widenedTailRead(path, lastAssistantTextFromTail)
}

// lastTextTailBytes is a much larger fallback tail window, tried ONLY when
// the standard tailBytes window (shared with classification) contains no
// assistant text at all. A busy loop's large tool_use/tool_result payloads
// (e.g. a big file read or grep) can fill the entire small tail without a
// single text block, even though a perfectly good "what it was doing"
// report sits just outside that window — this is F2's "DOING/TAIL empty
// for the busiest loops" bug. Widening only on an actual miss keeps the
// common case (small tail already has text) exactly as cheap as before.
const lastTextTailBytes = 256 * 1024

// widenedTailRead reads path's standard tailBytes-sized tail and applies
// extract; if that finds nothing, it retries with the much larger
// lastTextTailBytes window before giving up. Shared by
// LastAssistantText/LastAssistantTextFull (both callers of readTail — see
// each's doc) so busy loops' DOING/TAIL AND the oracle's judged report
// (internal/oracle.Judge, via LastAssistantTextFull) both get the same
// fallback — the same root cause, one fix.
func widenedTailRead(path string, extract func([]byte) (string, bool)) (string, bool) {
	if buf, ok := readTail(path, tailBytes); ok {
		if text, ok := extract(buf); ok {
			return text, true
		}
	}
	buf, ok := readTail(path, lastTextTailBytes)
	if !ok {
		return "", false
	}
	return extract(buf)
}

// tailTextCap bounds LastText, the summarized last-assistant message. It's
// sized to fill several wrapped lines in the detail pane's TAIL row
// (internal/tui renders up to tailMaxLines of it) at typical terminal widths,
// while staying bounded — it is deliberately NOT the full uncapped report,
// which LastAssistantTextFull already serves separately for the oracle. Bumped
// from 120 (a single hard-truncated line) once the TAIL row learned to wrap.
const tailTextCap = 800

// lastAssistantTextFromTail is LastAssistantText's buffer-only core: finds
// the raw text, then caps it to tailTextCap chars for the TAIL row.
func lastAssistantTextFromTail(buf []byte) (string, bool) {
	text, ok := lastAssistantTextRawFromTail(buf)
	if !ok {
		return "", false
	}
	return summarizeTailText(text, tailTextCap), true
}

// LastAssistantTextFull returns the last assistant message's RAW text from
// the tail of the session log — uncapped, unlike LastAssistantText (which
// caps at tailTextCap chars for the TUI's TAIL row). The oracle
// (internal/oracle) needs the full report to judge accurately; an
// 800-char summary would throw away exactly the evidence it's supposed to
// check. ok is false only when EVEN the widened search (see
// widenedTailRead) finds no assistant text at all — otherwise a busy
// loop's judged report would go missing for the same reason F2 fixed
// DOING/TAIL.
func LastAssistantTextFull(path string) (string, bool) {
	return widenedTailRead(path, lastAssistantTextRawFromTail)
}

// lastAssistantTextRawFromTail is the shared, uncapped core of both
// lastAssistantTextFromTail and LastAssistantTextFull.
func lastAssistantTextRawFromTail(buf []byte) (string, bool) {
	last := ""
	found := false
	for _, line := range strings.Split(string(buf), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if t, _ := entry["type"].(string); t != "assistant" {
			continue
		}
		if text, ok := assistantMessageText(entry); ok && text != "" {
			last = text
			found = true
		}
	}
	if !found {
		return "", false
	}
	return last, true
}

// assistantMessageText mirrors userMessageText for an assistant entry:
// message.content is either a plain string or an array of blocks (text
// blocks have "type":"text"; tool_use blocks are skipped — not useful as a
// one-line summary of "what it was doing").
func assistantMessageText(entry map[string]any) (string, bool) {
	msg, ok := entry["message"].(map[string]any)
	if !ok {
		return "", false
	}
	switch content := msg["content"].(type) {
	case string:
		return content, content != ""
	case []any:
		for _, block := range content {
			b, ok := block.(map[string]any)
			if !ok {
				continue
			}
			if b["type"] != "text" {
				continue
			}
			if text, ok := b["text"].(string); ok && text != "" {
				return text, true
			}
		}
	}
	return "", false
}

// LastError scans path's tail (widened if needed — same fallback as
// LastAssistantText/LastAssistantTextFull) for the most recent GENUINE
// error entry: an assistant text block carrying Claude Code's own literal
// "API Error" marker (see apiErrorMarker's doc — never a bare "429"/"rate
// limit" substring match, which false-positives on ordinary conversation),
// or a tool_result with is_error=true. Returns the RAW error text (council
// hard rule for feat/detail-panel-v2's LAST ERROR block: rendered
// verbatim, never paraphrased) and the entry's own timestamp (parsed from
// its "timestamp" field, RFC3339 — zero time if missing/unparseable).
// ok=false when no error entry is found at all (even in the widened tail)
// — there is NO fallback to plain assistant text; a healthy loop with no
// real error must always get ok=false.
func LastError(path string) (text string, ts time.Time, ok bool) {
	if buf, readOK := readTail(path, tailBytes); readOK {
		if e, found := extractLastError(buf); found {
			return e.text, e.ts, true
		}
	}
	buf, readOK := readTail(path, lastTextTailBytes)
	if !readOK {
		return "", time.Time{}, false
	}
	e, found := extractLastError(buf)
	return e.text, e.ts, found
}

// errorEntry is extractLastError's intermediate result — kept as a small
// struct (rather than two bare returns threaded through every helper) so
// the "keep scanning, remember the LAST match" loop in extractLastError
// reads as one assignment, not a pair of parallel variables that could
// drift out of sync.
type errorEntry struct {
	text string
	ts   time.Time
}

// apiErrorMarker is the ONLY signal that makes an assistant text block a
// genuine error: the literal "API Error" prefix Claude Code itself
// synthesizes into the transcript when a real API call fails (e.g. "API
// Error: 429 Too Many Requests — retry after 30s" — the exact shape this
// package's own fixtures use).
//
// fix/last-error-false-positive (P1, live repro): the OLD check matched
// on ANY of {"api error", "429", "rate limit"} as a bare substring,
// anywhere in ordinary assistant conversation — not just Claude Code's own
// synthesized error entries. Caught live against this repo's own real
// transcript: a healthy loop's normal status-update message, which
// happened to mention this very codebase's "429 auto-redrive" FEATURE BY
// NAME, matched the "429" substring and got rendered as a "verbatim
// error" in the DETAIL panel — on a loop with zero actual API errors. That
// destroys the block's entire purpose (fire ONLY on real errors) and an
// operator's trust in it. "429"/"rate limit" alone are commonplace words
// in any conversation ABOUT rate limits or this feature; only the "API
// Error" marker is Claude-Code-synthesized and never occurs in organic
// text. A plain assistant text block without this marker is NEVER an
// error, full stop — no substring fallback.
const apiErrorMarker = "api error"

// extractLastError is LastError's buffer-only core: walks every parseable
// JSONL line (tolerating a truncated first line, same as every other tail
// scanner in this file) and keeps the LAST matching error entry — either an
// assistant text block carrying apiErrorMarker, or a user tool_result
// block with is_error=true.
func extractLastError(buf []byte) (errorEntry, bool) {
	var last errorEntry
	found := false
	for _, line := range strings.Split(string(buf), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		switch entry["type"] {
		case "assistant":
			if text, isErr := assistantErrorText(entry); isErr {
				last = errorEntry{text, entryTimestamp(entry)}
				found = true
			}
		case "user":
			if text, isErr := userToolResultError(entry); isErr {
				last = errorEntry{text, entryTimestamp(entry)}
				found = true
			}
		}
	}
	return last, found
}

// assistantErrorText reports whether an assistant entry's text content is
// a genuine Claude-Code-synthesized API error (case-insensitive match
// against the literal apiErrorMarker — see its doc for why this is the
// ONLY acceptable signal), returning that raw text if so.
func assistantErrorText(entry map[string]any) (string, bool) {
	text, ok := assistantMessageText(entry)
	if !ok {
		return "", false
	}
	if !strings.Contains(strings.ToLower(text), apiErrorMarker) {
		return "", false
	}
	return text, true
}

// userToolResultError reports whether a user entry's message.content
// carries a tool_result block with is_error=true, returning that block's
// raw content text if so (the LAST such block in the entry, mirroring
// userMessageText's "last text block wins" tolerance).
func userToolResultError(entry map[string]any) (string, bool) {
	msg, ok := entry["message"].(map[string]any)
	if !ok {
		return "", false
	}
	content, ok := msg["content"].([]any)
	if !ok {
		return "", false
	}
	text, found := "", false
	for _, block := range content {
		b, ok := block.(map[string]any)
		if !ok || b["type"] != "tool_result" {
			continue
		}
		isErr, _ := b["is_error"].(bool)
		if !isErr {
			continue
		}
		if t, ok := toolResultText(b["content"]); ok {
			text, found = t, true
		}
	}
	return text, found
}

// toolResultText extracts a tool_result block's content as plain text —
// either a bare string, or (per the Messages API) an array of content
// blocks with "type":"text".
func toolResultText(content any) (string, bool) {
	switch c := content.(type) {
	case string:
		return c, c != ""
	case []any:
		for _, block := range c {
			b, ok := block.(map[string]any)
			if !ok || b["type"] != "text" {
				continue
			}
			if text, ok := b["text"].(string); ok && text != "" {
				return text, true
			}
		}
	}
	return "", false
}

// entryTimestamp parses a transcript entry's "timestamp" field — RFC3339
// with fractional seconds, e.g. "2026-07-16T14:15:54.953Z". VERIFIED
// (review fix, P2) against a real session file on this machine
// (~/.claude/projects/-home-user-fleetops/*.jsonl,
// 2026-07-17): both "user" and "assistant" entries carry exactly this
// shape, and time.Parse(time.RFC3339, s) parses it correctly despite
// time.RFC3339's layout constant not showing fractional digits — Go's
// RFC3339 parsing special-cases optional fractional seconds in the input
// even though the layout string itself doesn't spell them out. Zero time
// for anything missing/unparseable, never a panic — and see
// internal/tui.isErrorStale's doc for why callers must treat that zero
// value as "fail open" (NOT stale / show it), not "infinitely old".
func entryTimestamp(entry map[string]any) time.Time {
	s, ok := entry["timestamp"].(string)
	if !ok {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// summarizeTailText collapses newlines to spaces and caps length, yielding a
// single-line, bounded string (LastText). The detail pane's TAIL row re-wraps
// it across up to tailMaxLines lines at the pane's width; the DOING column
// hard-truncates it to its own narrow column. Keeping LastText itself
// single-line lets both callers wrap/truncate as they see fit.
//
// max is a RUNE count, not a byte count — cut by rune (not internal/tui's
// trunc, to avoid a tui->claude dependency; same "byte-index slice can land
// mid-character" hazard trunc's own doc comment warns about). Session
// transcripts routinely contain multi-byte text (e.g. Korean, 3 bytes/rune in
// UTF-8); a byte-index cut at the max boundary can slice a rune in half,
// rendering as a stray "�" right before the "…" marker.
func summarizeTailText(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
