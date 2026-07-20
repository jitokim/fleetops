package hooks

import (
	"reflect"
	"sort"
	"testing"
)

// hookEntry builds one settings.hooks[event] entry wrapping a single command,
// matching Claude Code's schema shape.
func hookEntry(command string) map[string]any {
	return map[string]any{
		"matcher": "",
		"hooks":   []any{map[string]any{"type": "command", "command": command}},
	}
}

// settingsWith builds a settings map whose hooks section maps each event to a
// single command line (event -> command).
func settingsWith(eventCommands map[string]string) map[string]any {
	hooks := map[string]any{}
	for event, command := range eventCommands {
		hooks[event] = []any{hookEntry(command)}
	}
	return map[string]any{"hooks": hooks}
}

// allInstalled is settings with every managed event pointing at the same
// fleetops binary — the fully-healthy baseline.
func allInstalled(binary string) map[string]any {
	ec := map[string]string{}
	for _, s := range Specs() {
		ec[s.EventName] = binary + " " + s.SubcommandSuffix
	}
	return settingsWith(ec)
}

// existsOnly returns a probe that reports only the listed paths as existing.
func existsOnly(paths ...string) func(string) bool {
	set := map[string]bool{}
	for _, p := range paths {
		set[p] = true
	}
	return func(p string) bool { return set[p] }
}

// stateOf pulls one event's State out of a Report for assertions.
func stateOf(r Report, event string) (EventState, bool) {
	for _, e := range r.Events {
		if e.Event == event {
			return e.State, true
		}
	}
	return StateMissing, false
}

const testBin = "/usr/local/bin/fleetops"

func TestHealth_AllPresentAndBinaryExists_OK(t *testing.T) {
	report := Health(allInstalled(testBin), existsOnly(testBin))

	if !report.OK {
		t.Fatalf("report.OK = false, want true; events=%+v", report.Events)
	}
	if len(report.Events) != len(Specs()) {
		t.Errorf("got %d events, want %d", len(report.Events), len(Specs()))
	}
	for _, e := range report.Events {
		if e.State != StateOK {
			t.Errorf("event %s State = %v, want StateOK", e.Event, e.State)
		}
	}
	if report.HasStalePath() {
		t.Error("HasStalePath() = true, want false for a fully-healthy install")
	}
}

func TestHealth_OneEventMissing_ReportedMissing(t *testing.T) {
	settings := allInstalled(testBin)
	// Drop SessionStart — the exact event whose absence means new sessions are
	// observed but never registered.
	delete(settings["hooks"].(map[string]any), "SessionStart")

	report := Health(settings, existsOnly(testBin))

	if report.OK {
		t.Fatal("report.OK = true, want false with SessionStart missing")
	}
	if state, _ := stateOf(report, "SessionStart"); state != StateMissing {
		t.Errorf("SessionStart State = %v, want StateMissing", state)
	}
	if got := report.Missing(); !reflect.DeepEqual(got, []string{"SessionStart"}) {
		t.Errorf("Missing() = %v, want [SessionStart]", got)
	}
	// Every OTHER event must still read OK — one missing event doesn't taint
	// the rest.
	for _, e := range report.Events {
		if e.Event != "SessionStart" && e.State != StateOK {
			t.Errorf("event %s State = %v, want StateOK (only SessionStart is missing)", e.Event, e.State)
		}
	}
}

// TestHealth_EntryPresentButBinaryGone_StalePath is the missionctl regression:
// an entry that LOOKS installed but points at a binary that no longer exists
// must read as StalePath, NOT healthy and NOT plain missing.
func TestHealth_EntryPresentButBinaryGone_StalePath(t *testing.T) {
	// The dead binary is still recognizably a fleetops command (the matcher
	// keys on "fleetops") — the real stale-path case is a fleetops binary that
	// was moved or removed, e.g. a `go install`ed path that got cleaned, or the
	// historical rename that left hooks pointing at a now-dead location.
	deadBin := "/old/removed/fleetops"
	settings := allInstalled(deadBin)

	// Probe says NOTHING exists — the binary every entry points at is gone.
	report := Health(settings, existsOnly())

	if report.OK {
		t.Fatal("report.OK = true, want false when the referenced binary is gone")
	}
	if !report.HasStalePath() {
		t.Fatal("HasStalePath() = false, want true — a dead-path install must be its own scarier state")
	}
	for _, e := range report.Events {
		if e.State != StateStalePath {
			t.Errorf("event %s State = %v, want StateStalePath", e.Event, e.State)
		}
		if e.Binary != deadBin {
			t.Errorf("event %s Binary = %q, want %q (so the message can name the dead binary)", e.Event, e.Binary, deadBin)
		}
	}
	// StalePath is NOT Missing — the whole point is that they're distinct.
	if len(report.Missing()) != 0 {
		t.Errorf("Missing() = %v, want empty (present-but-dead is StalePath, not Missing)", report.Missing())
	}
	want := []string{"Notification", "PermissionRequest", "SessionEnd", "SessionStart"}
	got := report.StalePaths()
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("StalePaths() = %v, want %v", got, want)
	}
}

// TestHealth_DevBuildDiffersFromInstalledPath_StillHealthy pins the explicit
// constraint: StalePath means the binary does NOT exist — not that it differs
// from the running process. A dev build whose hooks point at an installed
// binary that DOES exist is healthy.
func TestHealth_DevBuildDiffersFromInstalledPath_StillHealthy(t *testing.T) {
	installedBin := "/home/user/go/bin/fleetops" // what the hooks point at
	// The running process is "./fleetops" (a dev build), but the probe only
	// cares whether the REFERENCED binary exists — and it does.
	report := Health(allInstalled(installedBin), existsOnly(installedBin))

	if !report.OK {
		t.Fatalf("report.OK = false, want true — hooks pointing at an existing (if different) binary are healthy; events=%+v", report.Events)
	}
}

// TestHealth_ForeignHookOnSameEvent_Ignored proves another tool's hook under a
// managed event does not count as ours — that event still reads Missing.
func TestHealth_ForeignHookOnSameEvent_Ignored(t *testing.T) {
	settings := allInstalled(testBin)
	// Add a foreign hook ALONGSIDE ours under SessionStart, then remove ours,
	// leaving only the foreign entry.
	// The foreign command even ends with the same "hook session-start" suffix,
	// so only the "fleetops"-substring clause keeps it from being counted as
	// ours — a strict test of the matcher, not just the suffix.
	settings["hooks"].(map[string]any)["SessionStart"] = []any{
		hookEntry("/opt/other-tool hook session-start"),
	}

	report := Health(settings, existsOnly(testBin))

	if state, _ := stateOf(report, "SessionStart"); state != StateMissing {
		t.Errorf("SessionStart State = %v, want StateMissing (a foreign hook is not ours)", state)
	}
	if report.OK {
		t.Error("report.OK = true, want false — a foreign-only event is not our install")
	}
}

func TestHealth_NilSettings_DegradedNoPanic(t *testing.T) {
	report := Health(nil, existsOnly(testBin))

	if report.OK {
		t.Fatal("report.OK = true for nil settings, want false (honest 'cannot confirm')")
	}
	if len(report.Events) != len(Specs()) {
		t.Errorf("got %d events, want %d even for nil settings", len(report.Events), len(Specs()))
	}
	for _, e := range report.Events {
		if e.State != StateMissing {
			t.Errorf("event %s State = %v, want StateMissing for nil settings", e.Event, e.State)
		}
	}
}

// TestHealth_MalformedShapes_NoPanic feeds wrong-typed junk in every position
// the traversal touches — Health must skip it, never panic.
func TestHealth_MalformedShapes_NoPanic(t *testing.T) {
	malformed := map[string]any{
		"hooks": map[string]any{
			"SessionStart": "not-an-array",
			"Notification": []any{"not-a-map", 42, map[string]any{"hooks": "not-an-array"}},
			"SessionEnd":   []any{map[string]any{"hooks": []any{"not-a-map", map[string]any{"command": 99}}}},
		},
	}

	report := Health(malformed, existsOnly(testBin)) // must not panic

	if report.OK {
		t.Error("report.OK = true, want false for malformed settings")
	}
	for _, e := range report.Events {
		if e.State != StateMissing {
			t.Errorf("event %s State = %v, want StateMissing (malformed shapes yield no valid entry)", e.Event, e.State)
		}
	}
}

func TestIsFleetopsHookCommand(t *testing.T) {
	cases := []struct {
		cmd    string
		suffix string
		want   bool
	}{
		{"/usr/local/bin/fleetops hook notify", "hook notify", true},
		{"fleetops hook notify", "hook notify", true},
		{"some-other-tool notify", "hook notify", false},
		// Ends with the EXACT suffix but isn't fleetops — must not match. This
		// case is what isolates the "fleetops"-substring clause from the suffix
		// clause: another tool wiring a "hook notify"-suffixed command must
		// never be mistaken for ours.
		{"/opt/other-tool hook notify", "hook notify", false},
		{"fleetops hook resume", "hook notify", false}, // doesn't end in "hook notify"
		{"/usr/local/bin/fleetops hook session-start", "hook session-start", true},
		// wrong suffix for the command: notify must NOT match session-start's
		// suffix, so the events never cross-classify each other.
		{"/usr/local/bin/fleetops hook notify", "hook session-start", false},
	}
	for _, c := range cases {
		if got := IsFleetopsHookCommand(c.cmd, c.suffix); got != c.want {
			t.Errorf("IsFleetopsHookCommand(%q, %q) = %v, want %v", c.cmd, c.suffix, got, c.want)
		}
	}
}

// TestSpecs pins the canonical managed-event set so a future edit that drops
// or renames one is caught here (cmd/fleetops derives its own list from this).
func TestSpecs(t *testing.T) {
	want := map[string]string{
		"Notification":      "hook notify",
		"SessionStart":      "hook session-start",
		"SessionEnd":        "hook session-end",
		"PermissionRequest": "hook permission",
	}
	specs := Specs()
	if len(specs) != len(want) {
		t.Fatalf("got %d specs, want %d: %+v", len(specs), len(want), specs)
	}
	for _, s := range specs {
		if w, ok := want[s.EventName]; !ok || w != s.SubcommandSuffix {
			t.Errorf("spec %+v not in expected set %+v", s, want)
		}
	}
}

func TestBinaryExists_EmptyPath_False(t *testing.T) {
	if BinaryExists("") {
		t.Error("BinaryExists(\"\") = true, want false")
	}
}
