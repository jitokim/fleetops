package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jitokim/fleetops/internal/domain"
)

// tabLabelMax bounds the goal-label (or subagent-type) portion of a cmux tab
// title to a compact width — the tab strip is narrow, so a long goal must be
// clipped rather than pushing the state token off-screen. ~16-20 columns per
// pivot §5.1; 18 sits in the middle. Measured in terminal COLUMNS via trunc
// (the SAME go-runewidth measure the TUI's own columns use — see trunc), not
// bytes or runes, so a CJK goal clips at the width it actually draws.
const tabLabelMax = 18

// Delegation takes precedence in a tab title: a delegating loop's parent tail
// is quiet, so its state alone ("running"/"idle") is uninformative — the tab
// should instead name what the subagent is doing, mirroring v0.9.0's DETAIL
// delegation row onto the tab strip. tabDelegateGlyph/tabDelegateToken compose
// with the label (the subagent type) into the same "<glyph><token> <label>"
// shape every other state uses, e.g. "▸deleg code-reviewer" (pivot §5.1).
const (
	tabDelegateGlyph = "▸"
	tabDelegateToken = "deleg"
)

// composeTabTitle builds the compact, width-bounded cmux tab title for a loop:
// a state glyph + token (from domain.StateString, so it carries the stall slug)
// plus a short goal label. A DELEGATING loop wins — its title names the
// subagent instead of the parent's quiet state (see delegationSubagent).
//
// Pure and total: every loop maps to a title, so the driver never has to decide
// whether a loop is titleable — that decision belongs to the target-resolution
// step (ambiguity refusal), not the composer. Examples (pivot §5.1):
// "◆GATE auth-mw", "▸deleg code-reviewer", "⏸STALL rate-limit fx",
// "✗DRIFT auth-mw".
func composeTabTitle(l domain.Loop) string {
	if subType, ok := delegationSubagent(l); ok {
		return tabTitleJoin(tabDelegateGlyph, tabDelegateToken, subType)
	}
	return tabTitleJoin(tabStateGlyph(l.State), tabStateToken(l.State, l.Stall), l.DisplayLabel())
}

// tabTitleJoin assembles "<glyph><token> <label>", width-bounding the label to
// tabLabelMax columns and omitting the label section entirely when it is empty
// (e.g. a delegation carrying no subagent type ⇒ just "▸deleg", honest rather
// than a dangling separator).
func tabTitleJoin(glyph, token, label string) string {
	head := glyph + token
	label = trunc(strings.TrimSpace(label), tabLabelMax)
	if label == "" {
		return head
	}
	return head + " " + label
}

// tabStateToken renders a loop's state as the tab strip's compact token,
// derived from domain.StateString so it carries the stall slug the event
// history already encodes (e.g. "stalled:rate-limit" → "STALL rate-limit") —
// kept consistent with that persisted form rather than re-deriving the slug
// here.
func tabStateToken(state domain.LoopState, stall domain.StallKind) string {
	if base, slug, ok := strings.Cut(domain.StateString(state, stall), ":"); ok {
		return tabStateWord(domain.LoopState(base)) + " " + slug
	}
	return tabStateWord(state)
}

// tabStateWord is the short, upper-case word for a state in a tab title. The
// exact words are a presentation choice consistent with the pivot §5.1 examples
// (GATE / STALL / DRIFT) and with the fleet list's own state labels
// (stateLabel); the fallback upper-cases any state not enumerated so a new
// LoopState still renders honestly rather than blank.
func tabStateWord(state domain.LoopState) string {
	switch state {
	case domain.StateRunning:
		return "RUN"
	case domain.StateGate:
		return "GATE"
	case domain.StateStalled:
		return "STALL"
	case domain.StateIdle:
		return "IDLE"
	case domain.StateDrift:
		return "DRIFT"
	case domain.StateDone:
		return "DONE"
	case domain.StateFailed:
		return "FAIL"
	case domain.StatePaused:
		return "PAUSE"
	case domain.StateKilled:
		return "KILL"
	default:
		return strings.ToUpper(string(state))
	}
}

// tabStateGlyph is the single glyph a state leads with in a tab title — a
// presentation choice matching the pivot §5.1 examples (◆ gate, ⏸ stalled,
// ✗ drift) and the glyph vocabulary the fleet list already draws (stateLabel).
func tabStateGlyph(state domain.LoopState) string {
	switch state {
	case domain.StateGate:
		return "◆"
	case domain.StateStalled:
		return "⏸"
	case domain.StateRunning:
		return "●"
	case domain.StateIdle:
		return "·"
	case domain.StateDrift, domain.StateFailed:
		return "✗"
	case domain.StateDone:
		return "✓"
	case domain.StateKilled:
		return "☠"
	case domain.StatePaused:
		return "‖"
	default:
		return "·"
	}
}

// tabSyncEntry pairs a loop whose composed tab title CHANGED with the title to
// write — the unit renameTabsCmd's off-loop write pass consumes.
type tabSyncEntry struct {
	loop  domain.Loop
	title string
}

// tabTitlesSyncedMsg reports back which sessions' tabs were ACTUALLY renamed
// (sessionID → title written) so the handler can advance the debounce ledger —
// only confirmed writes, so a skipped/failed one is retried next tick.
type tabTitlesSyncedMsg struct {
	written map[string]string
}

// pendingTabTitleChanges is the driver's pure diff step: the loops whose freshly
// composed title differs from the last one written (debounce), or NONE when the
// opt-in gate is off. Keeping the gate + debounce here (off the event loop, no
// I/O) makes both directly unit-testable and means a steady-state tick produces
// zero work — the composer is pure and cheap, so this runs inline in the handler.
func (m Model) pendingTabTitleChanges(loops []domain.Loop) []tabSyncEntry {
	if !m.renameTabs {
		return nil // opt-in gate: observation stays pure unless --rename-tabs
	}
	var changed []tabSyncEntry
	for i := range loops {
		title := composeTabTitle(loops[i])
		if m.lastTabTitle[loops[i].SessionID] == title {
			continue // debounce: unchanged since the last successful write
		}
		changed = append(changed, tabSyncEntry{loop: loops[i], title: title})
	}
	return changed
}

// renameTabsCmd mirrors each changed loop's composed title onto its cmux tab,
// off the event loop. It resolves every target through resolveTabTitlerFn — the
// SAME fail-closed locate/ambiguity discipline typed actuation uses — and skips
// (never renames) any loop that is ambiguous, non-cmux, or whose bounded-exec
// write fails: a cmux failure degrades to "didn't rename", never a hang or a
// crash. Returns nil (no command) when the gate is off or nothing changed, so a
// steady-state tick costs a pure diff and no goroutine.
func (m Model) renameTabsCmd(loops []domain.Loop) tea.Cmd {
	changed := m.pendingTabTitleChanges(loops)
	if len(changed) == 0 {
		return nil
	}
	sessionsDir := sessionsDirFn()
	return func() tea.Msg {
		written := make(map[string]string, len(changed))
		for _, e := range changed {
			titler, target, ok := resolveTabTitlerFn(sessionsDir, e.loop.SessionID, e.loop.ProjectDir)
			if !ok {
				continue // ambiguous / non-cmux / no backend — fail-closed skip
			}
			if err := titler.SetTabTitle(target, e.title); err != nil {
				continue // bounded-exec failure/timeout — degrade to "didn't rename"
			}
			written[e.loop.SessionID] = e.title
		}
		return tabTitlesSyncedMsg{written: written}
	}
}

// delegationSubagent reports whether a loop is DELEGATING and, if so, the
// subagent type it handed off to — reusing the SAME signal the scanner writes:
// a delegating loop's LastText is the "delegating: <type> — <child line>"
// summary claude.applySubagentDelegation synthesizes (see claude.formatDelegating).
// This intentionally keys off that string form rather than a new field so tab
// titling and the DETAIL row read one signal; the coupling to formatDelegating's
// literal output is the trade (a structured domain.Loop field carried from the
// scanner would be the cleaner follow-up, but it lives in a different package).
//
// HONESTY (why this is not a bare HasPrefix): composeTabTitle runs for EVERY
// loop, and a non-delegating loop's LastText is the RAW assistant tail
// (domain.Loop.LastText). So the match must reject prose that merely begins with
// the word "delegating" (e.g. "delegating the migration to a script") — else the
// tab would fabricate a delegation. It accepts ONLY the exact boundaries
// formatDelegating ever emits after the head: end-of-string (bare), ":" (typed),
// " —" (child line, no type), or " (" (live count, no type). Anything else is
// prose ⇒ not a delegation.
//
// The type is "" when the delegation carried none (the bare/child/live-count
// forms), which composeTabTitle renders as a bare "▸deleg" — honest, not invented.
func delegationSubagent(l domain.Loop) (string, bool) {
	const prefix = "delegating"
	if !strings.HasPrefix(l.LastText, prefix) {
		return "", false
	}
	rest := l.LastText[len(prefix):]
	// Only the scanner's authoritative boundaries count (see the doc above); a
	// raw tail like " the migration…" is prose, not a delegation.
	if rest != "" && !strings.HasPrefix(rest, ":") && !strings.HasPrefix(rest, " —") && !strings.HasPrefix(rest, " (") {
		return "", false
	}
	if !strings.HasPrefix(rest, ":") {
		return "", true // the bare / child-line / live-count forms carry no type
	}
	// Typed form: strip the ": " and drop the child-line ("… — <line>") and
	// live-count ("… (N live)") suffixes formatDelegating may append.
	rest = strings.TrimPrefix(strings.TrimPrefix(rest, ":"), " ")
	if i := strings.Index(rest, " — "); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.Index(rest, " ("); i >= 0 {
		rest = rest[:i]
	}
	return strings.TrimSpace(rest), true
}
