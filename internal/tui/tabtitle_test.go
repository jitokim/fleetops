package tui

import (
	"errors"
	"testing"

	"github.com/jitokim/fleetops/internal/control"
	"github.com/jitokim/fleetops/internal/domain"
)

// --- composeTabTitle: one row per state + delegation precedence + width ---

func TestComposeTabTitle_Table(t *testing.T) {
	cases := []struct {
		name string
		loop domain.Loop
		want string
	}{
		{
			name: "gate",
			loop: domain.Loop{State: domain.StateGate, Goal: domain.Goal{Text: "auth-mw"}},
			want: "◆GATE auth-mw",
		},
		{
			name: "stalled rate-limit carries the slug",
			loop: domain.Loop{State: domain.StateStalled, Stall: domain.StallRateLimit, Goal: domain.Goal{Text: "fx"}},
			want: "⏸STALL rate-limit fx",
		},
		{
			name: "stalled no-output",
			loop: domain.Loop{State: domain.StateStalled, Stall: domain.StallNoOutput, Goal: domain.Goal{Text: "fx"}},
			want: "⏸STALL no-output fx",
		},
		{
			name: "stalled gone",
			loop: domain.Loop{State: domain.StateStalled, Stall: domain.StallGone, Goal: domain.Goal{Text: "fx"}},
			want: "⏸STALL gone fx",
		},
		{
			name: "running",
			loop: domain.Loop{State: domain.StateRunning, Goal: domain.Goal{Text: "fx"}},
			want: "●RUN fx",
		},
		{
			name: "idle",
			loop: domain.Loop{State: domain.StateIdle, Goal: domain.Goal{Text: "fx"}},
			want: "·IDLE fx",
		},
		{
			name: "drift",
			loop: domain.Loop{State: domain.StateDrift, Goal: domain.Goal{Text: "auth-mw"}},
			want: "✗DRIFT auth-mw",
		},
		{
			name: "done",
			loop: domain.Loop{State: domain.StateDone, Goal: domain.Goal{Text: "fx"}},
			want: "✓DONE fx",
		},
		{
			name: "explicit name wins over goal for the label",
			loop: domain.Loop{State: domain.StateGate, Name: "nice-name", Goal: domain.Goal{Text: "raw goal text"}},
			want: "◆GATE nice-name",
		},
		{
			// Delegation precedence: even when the state is Stalled, a delegating
			// LastText makes the tab name the subagent, not the parent's state.
			name: "delegating wins over state",
			loop: domain.Loop{State: domain.StateStalled, Stall: domain.StallNoOutput, Goal: domain.Goal{Text: "fx"}, LastText: "delegating: code-reviewer — reviewing the diff"},
			want: "▸deleg code-reviewer",
		},
		{
			name: "delegating drops the (N live) suffix",
			loop: domain.Loop{State: domain.StateRunning, LastText: "delegating: code-reviewer (2 live) — foo"},
			want: "▸deleg code-reviewer",
		},
		{
			name: "delegating with no subagent type omits the label",
			loop: domain.Loop{State: domain.StateRunning, LastText: "delegating"},
			want: "▸deleg",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := composeTabTitle(tc.loop); got != tc.want {
				t.Errorf("composeTabTitle = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestComposeTabTitle_LabelIsWidthBounded(t *testing.T) {
	long := "this-is-a-very-long-goal-name-that-overflows-the-tab-strip"
	title := composeTabTitle(domain.Loop{State: domain.StateRunning, Goal: domain.Goal{Text: long}})

	const head = "●RUN "
	if len(title) <= len(head) || title[:len(head)] != head {
		t.Fatalf("title %q did not start with %q", title, head)
	}
	label := title[len(head):]
	if w := narrowAmbiguous.StringWidth(label); w > tabLabelMax {
		t.Errorf("label %q width = %d, want <= %d", label, w, tabLabelMax)
	}
	// A truncated label must carry the ellipsis, not silently drop characters.
	if narrowAmbiguous.StringWidth(long) > tabLabelMax && label[len(label)-len("…"):] != "…" {
		t.Errorf("expected an ellipsis on the truncated label, got %q", label)
	}
}

func TestComposeTabTitle_UnknownState_UppercasesRatherThanBlank(t *testing.T) {
	// A LoopState not enumerated must still render an honest token, never blank.
	got := composeTabTitle(domain.Loop{State: domain.LoopState("weird"), Goal: domain.Goal{Text: "x"}})
	if got != "·WEIRD x" {
		t.Errorf("composeTabTitle for unknown state = %q, want %q", got, "·WEIRD x")
	}
}

// --- delegationSubagent signal extraction ---

func TestDelegationSubagent(t *testing.T) {
	cases := []struct {
		name     string
		lastText string
		wantType string
		wantOK   bool
	}{
		{"not delegating", "some ordinary tail line", "", false},
		{"empty", "", "", false},
		{"type only", "delegating: code-reviewer", "code-reviewer", true},
		{"type + child line", "delegating: developer — writing tests", "developer", true},
		{"type + live count + child", "delegating: developer (3 live) — writing tests", "developer", true},
		{"bare delegating, no type", "delegating", "", true},
		{"no type but child line", "delegating — writing tests", "", true},
		{"no type but live count + child", "delegating (2 live) — writing tests", "", true},
		// HONESTY guard: raw assistant prose that merely starts with the word
		// "delegating" must NOT be read as a delegation (no fabricated tab).
		{"prose false-positive rejected", "delegating the migration to a script", "", false},
		{"prose word-boundary rejected", "delegatingx: nope", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotType, gotOK := delegationSubagent(domain.Loop{LastText: tc.lastText})
			if gotOK != tc.wantOK || gotType != tc.wantType {
				t.Errorf("delegationSubagent(%q) = (%q, %v), want (%q, %v)", tc.lastText, gotType, gotOK, tc.wantType, tc.wantOK)
			}
		})
	}
}

// --- presentation-sync driver: gate, debounce, refusal ---

// fakeTabTitler records every SetTabTitle it receives and returns err.
type fakeTabTitler struct {
	titles map[string]string // ID → title
	err    error
}

func (f *fakeTabTitler) SetTabTitle(target control.Target, title string) error {
	if f.titles == nil {
		f.titles = map[string]string{}
	}
	f.titles[target.ID] = title
	return f.err
}

// withResolveTabTitler swaps the resolver seam for one test.
func withResolveTabTitler(t *testing.T, fn func(sessionsDir, sessionID, projectDir string) (control.TabTitler, control.Target, bool)) {
	t.Helper()
	orig := resolveTabTitlerFn
	t.Cleanup(func() { resolveTabTitlerFn = orig })
	resolveTabTitlerFn = fn
}

func TestPendingTabTitleChanges_OptInGateOff_NoWork(t *testing.T) {
	m := New() // renameTabs defaults to false
	l := domain.Loop{SessionID: "s1", State: domain.StateGate, Goal: domain.Goal{Text: "x"}}
	if changed := m.pendingTabTitleChanges([]domain.Loop{l}); changed != nil {
		t.Errorf("gate OFF must yield no changes, got %v", changed)
	}
	if cmd := m.renameTabsCmd([]domain.Loop{l}); cmd != nil {
		t.Error("gate OFF must yield a nil cmd (no rename attempted)")
	}
}

func TestRenameTabsCmd_GateOff_NeverResolves(t *testing.T) {
	resolved := false
	withResolveTabTitler(t, func(_, _, _ string) (control.TabTitler, control.Target, bool) {
		resolved = true
		return nil, control.Target{}, false
	})
	m := New() // gate off
	if cmd := m.renameTabsCmd([]domain.Loop{{SessionID: "s1", State: domain.StateGate}}); cmd != nil {
		cmd()
	}
	if resolved {
		t.Error("gate OFF must never reach the target resolver")
	}
}

func TestPendingTabTitleChanges_Debounce_UnchangedTitleSkipped(t *testing.T) {
	l := domain.Loop{SessionID: "s1", State: domain.StateGate, Goal: domain.Goal{Text: "x"}}
	m := New().WithRenameTabs(true)
	m.lastTabTitle = map[string]string{"s1": composeTabTitle(l)}
	if changed := m.pendingTabTitleChanges([]domain.Loop{l}); len(changed) != 0 {
		t.Errorf("unchanged title must be debounced (no work), got %v", changed)
	}
	// A state change flips the title, which must NOT be debounced.
	l.State = domain.StateDrift
	if changed := m.pendingTabTitleChanges([]domain.Loop{l}); len(changed) != 1 {
		t.Errorf("changed title must produce one entry, got %v", changed)
	}
}

func TestRenameTabsCmd_WritesChangedTitle_ThenDebouncesEndToEnd(t *testing.T) {
	titler := &fakeTabTitler{}
	withResolveTabTitler(t, func(_, sessionID, _ string) (control.TabTitler, control.Target, bool) {
		return titler, control.Target{Backend: "cmux", ID: "surface:" + sessionID}, true
	})

	l := domain.Loop{SessionID: "s1", ProjectDir: "-x-proj", State: domain.StateGate, Goal: domain.Goal{Text: "auth-mw"}}
	m := New().WithRenameTabs(true)

	cmd := m.renameTabsCmd([]domain.Loop{l})
	if cmd == nil {
		t.Fatal("expected a rename cmd for the changed title")
	}
	msg := cmd()
	synced, ok := msg.(tabTitlesSyncedMsg)
	if !ok {
		t.Fatalf("expected tabTitlesSyncedMsg, got %T", msg)
	}
	if titler.titles["surface:s1"] != "◆GATE auth-mw" {
		t.Errorf("SetTabTitle got %q, want %q", titler.titles["surface:s1"], "◆GATE auth-mw")
	}
	if synced.written["s1"] != "◆GATE auth-mw" {
		t.Errorf("written ledger = %v, want s1→◆GATE auth-mw", synced.written)
	}

	// Feed the result back: the handler advances the debounce ledger, and the
	// NEXT tick with the SAME loop must produce zero work (steady state).
	updated, _ := m.Update(msg)
	mm := updated.(Model)
	if changed := mm.pendingTabTitleChanges([]domain.Loop{l}); len(changed) != 0 {
		t.Errorf("after a confirmed write the same loop must debounce, got %v", changed)
	}
}

func TestRenameTabsCmd_AmbiguousLoop_NoRenameNoLedger(t *testing.T) {
	titler := &fakeTabTitler{}
	// Resolver refuses (ok=false) — the ambiguity/fail-closed case.
	withResolveTabTitler(t, func(_, _, _ string) (control.TabTitler, control.Target, bool) {
		return nil, control.Target{}, false
	})

	l := domain.Loop{SessionID: "s1", State: domain.StateGate, Goal: domain.Goal{Text: "x"}}
	m := New().WithRenameTabs(true)

	msg := m.renameTabsCmd([]domain.Loop{l})()
	synced := msg.(tabTitlesSyncedMsg)
	if len(synced.written) != 0 {
		t.Errorf("ambiguous loop must not be renamed, written = %v", synced.written)
	}
	if len(titler.titles) != 0 {
		t.Error("ambiguous loop must never reach SetTabTitle")
	}
	// Ledger must stay empty so the loop is retried once it disambiguates.
	updated, _ := m.Update(msg)
	if got := updated.(Model).lastTabTitle["s1"]; got != "" {
		t.Errorf("ambiguous loop must not enter the debounce ledger, got %q", got)
	}
}

func TestRenameTabsCmd_NonCmuxBackend_NoOp(t *testing.T) {
	// A non-cmux backend resolves to no TabTitler — control.ResolveTabTitler
	// returns ok=false, so the driver skips exactly as for ambiguity.
	withResolveTabTitler(t, func(_, _, _ string) (control.TabTitler, control.Target, bool) {
		return nil, control.Target{}, false
	})
	l := domain.Loop{SessionID: "s1", State: domain.StateGate, Goal: domain.Goal{Text: "x"}}
	m := New().WithRenameTabs(true)
	synced := m.renameTabsCmd([]domain.Loop{l})().(tabTitlesSyncedMsg)
	if len(synced.written) != 0 {
		t.Errorf("non-cmux backend must be a no-op, written = %v", synced.written)
	}
}

func TestRenameTabsCmd_WriteError_NotLedgered(t *testing.T) {
	// A bounded-exec failure (SetTabTitle error) must NOT advance the ledger, so
	// the title is retried next tick.
	titler := &fakeTabTitler{err: errWriteFailed}
	withResolveTabTitler(t, func(_, sessionID, _ string) (control.TabTitler, control.Target, bool) {
		return titler, control.Target{Backend: "cmux", ID: sessionID}, true
	})
	l := domain.Loop{SessionID: "s1", State: domain.StateGate, Goal: domain.Goal{Text: "x"}}
	m := New().WithRenameTabs(true)
	synced := m.renameTabsCmd([]domain.Loop{l})().(tabTitlesSyncedMsg)
	if len(synced.written) != 0 {
		t.Errorf("a failed write must not be ledgered, written = %v", synced.written)
	}
}

// TestRenameTabsCmd_MixedBatch_OnlyConfirmedLedgered drives ONE tick carrying
// three loops at once — an OK write, a failed write, and an ambiguous (refused)
// loop — and asserts only the confirmed one enters the debounce ledger. This is
// the crux invariant (skipped/failed writes must be retried, not suppressed),
// exercised for a multi-loop batch rather than one loop at a time.
func TestRenameTabsCmd_MixedBatch_OnlyConfirmedLedgered(t *testing.T) {
	okTitler := &fakeTabTitler{}
	errTitler := &fakeTabTitler{err: errWriteFailed}
	withResolveTabTitler(t, func(_, sessionID, _ string) (control.TabTitler, control.Target, bool) {
		switch sessionID {
		case "ok":
			return okTitler, control.Target{Backend: "cmux", ID: "surface:ok"}, true
		case "writeerr":
			return errTitler, control.Target{Backend: "cmux", ID: "surface:writeerr"}, true
		default: // "ambiguous"
			return nil, control.Target{}, false
		}
	})

	loops := []domain.Loop{
		{SessionID: "ok", State: domain.StateGate, Goal: domain.Goal{Text: "a"}},
		{SessionID: "writeerr", State: domain.StateGate, Goal: domain.Goal{Text: "b"}},
		{SessionID: "ambiguous", State: domain.StateGate, Goal: domain.Goal{Text: "c"}},
	}
	m := New().WithRenameTabs(true)
	synced := m.renameTabsCmd(loops)().(tabTitlesSyncedMsg)

	if len(synced.written) != 1 || synced.written["ok"] != "◆GATE a" {
		t.Fatalf("written = %v, want only {ok: ◆GATE a}", synced.written)
	}
	updated, _ := m.Update(synced)
	mm := updated.(Model)
	if mm.lastTabTitle["ok"] == "" {
		t.Error("confirmed write must be ledgered")
	}
	if mm.lastTabTitle["writeerr"] != "" || mm.lastTabTitle["ambiguous"] != "" {
		t.Errorf("failed/ambiguous loops must not be ledgered: %v", mm.lastTabTitle)
	}
}

// TestUpdate_TabTitlesSyncedMsg_AdvancesLedger confirms the handler records only
// the confirmed writes into the debounce ledger.
func TestUpdate_TabTitlesSyncedMsg_AdvancesLedger(t *testing.T) {
	m := New().WithRenameTabs(true)
	updated, cmd := m.Update(tabTitlesSyncedMsg{written: map[string]string{"s1": "◆GATE x"}})
	if cmd != nil {
		t.Error("tabTitlesSyncedMsg must not schedule a follow-up cmd")
	}
	if got := updated.(Model).lastTabTitle["s1"]; got != "◆GATE x" {
		t.Errorf("ledger[s1] = %q, want %q", got, "◆GATE x")
	}
}

var errWriteFailed = errors.New("cmux rename-tab failed")
