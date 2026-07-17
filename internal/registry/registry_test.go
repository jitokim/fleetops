package registry

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jitokim/missionctl/internal/domain"
	"github.com/jitokim/missionctl/internal/events"
)

func TestBind_LoadRoundTrip(t *testing.T) {
	dir := t.TempDir()

	if err := Bind(dir, "sess-1", BindSpec{Goal: "fix the flaky test"}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	rec, ok := Load(dir, "sess-1")
	if !ok {
		t.Fatal("expected a record to load")
	}
	if rec.Goal != "fix the flaky test" {
		t.Errorf("Goal = %q, want %q", rec.Goal, "fix the flaky test")
	}
	if rec.DoneCondition != "" {
		t.Errorf("DoneCondition = %q, want empty (not supplied)", rec.DoneCondition)
	}
	if rec.Rubric != "" {
		t.Errorf("Rubric = %q, want empty (not supplied)", rec.Rubric)
	}
	if rec.Challenger != "" {
		t.Errorf("Challenger = %q, want empty (not supplied)", rec.Challenger)
	}
	if rec.MaxCycles != DefaultMaxCycles {
		t.Errorf("MaxCycles = %d, want %d (zero-value spec.MaxCycles falls back to the default)", rec.MaxCycles, DefaultMaxCycles)
	}
	if rec.NoImproveLimit != DefaultNoImproveLimit {
		t.Errorf("NoImproveLimit = %d, want %d", rec.NoImproveLimit, DefaultNoImproveLimit)
	}
	if rec.Verdict != nil {
		t.Errorf("Verdict = %+v, want nil (never judged)", rec.Verdict)
	}
	if time.Since(rec.BoundAt) > 5*time.Second {
		t.Errorf("BoundAt = %v, want close to now", rec.BoundAt)
	}
}

func TestBind_LoadRoundTrip_FullContract(t *testing.T) {
	dir := t.TempDir()
	spec := BindSpec{
		Goal:          "fix the flaky test",
		DoneCondition: "go test ./... passes 5 times in a row",
		Rubric:        "run go test ./... and check for PASS",
		Challenger:    "try to break it with -race",
		MaxCycles:     20,
	}

	if err := Bind(dir, "sess-1", spec); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	rec, ok := Load(dir, "sess-1")
	if !ok {
		t.Fatal("expected a record to load")
	}
	if rec.DoneCondition != spec.DoneCondition {
		t.Errorf("DoneCondition = %q, want %q", rec.DoneCondition, spec.DoneCondition)
	}
	if rec.Rubric != spec.Rubric {
		t.Errorf("Rubric = %q, want %q", rec.Rubric, spec.Rubric)
	}
	if rec.Challenger != spec.Challenger {
		t.Errorf("Challenger = %q, want %q", rec.Challenger, spec.Challenger)
	}
	if rec.MaxCycles != 20 {
		t.Errorf("MaxCycles = %d, want 20 (explicit spec value must NOT be overridden by the default)", rec.MaxCycles)
	}
}

func TestLoad_Unbound(t *testing.T) {
	if _, ok := Load(t.TempDir(), "no-such-session"); ok {
		t.Error("expected ok=false for an unbound session")
	}
}

// TestLoad_OracleJSONKeyCompat is feat/panel-info's precise-rename load-
// compat test: Record.Oracle became Record.Rubric (see Record's doc), but
// the on-disk JSON key deliberately stayed "oracle" so an ALREADY-
// PERSISTED record (written by any version of missionctl before this
// rename) keeps loading its rubric text correctly — hand-writes a raw
// on-disk record file using the OLD "oracle" key, bypassing Bind/
// writeRecordFile entirely, to prove the read path alone honors it.
func TestLoad_OracleJSONKeyCompat(t *testing.T) {
	dir := t.TempDir()
	raw := `{"goal":"fix the bug","doneCondition":"tests pass","oracle":"run go test ./...","challenger":"","boundAt":1700000000,"maxCycles":12,"noImproveLimit":3,"noImprove":0}`
	if err := os.WriteFile(filepath.Join(dir, "sess-1.json"), []byte(raw), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	rec, ok := Load(dir, "sess-1")
	if !ok {
		t.Fatal("expected the pre-rename record to load")
	}
	if rec.Rubric != "run go test ./..." {
		t.Errorf("Rubric = %q, want %q (loaded from the old \"oracle\" JSON key)", rec.Rubric, "run go test ./...")
	}
	if rec.Goal != "fix the bug" || rec.DoneCondition != "tests pass" {
		t.Errorf("got %+v, want the rest of the pre-rename record intact too", rec)
	}
}

// TestSaveVerdict_WritesRubricUnderOracleJSONKey is the write-side half of
// the same compat seam: a record saved by TODAY's code must still be
// readable by anything (a human `cat`, an older missionctl binary) that
// expects the "oracle" JSON key — Rubric must never leak onto a NEW key.
func TestSaveVerdict_WritesRubricUnderOracleJSONKey(t *testing.T) {
	dir := t.TempDir()
	if err := Bind(dir, "sess-1", BindSpec{Goal: "goal", Rubric: "run go test ./..."}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "sess-1.json"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(raw), `"oracle":"run go test ./..."`) {
		t.Errorf("on-disk record = %s, want the rubric text under the \"oracle\" JSON key", raw)
	}
	if strings.Contains(string(raw), `"rubric"`) {
		t.Errorf("on-disk record = %s, want NO \"rubric\" JSON key at all — the field stays \"oracle\" on disk", raw)
	}
}

func TestSaveVerdict_RejectedIncrementsNoImprove(t *testing.T) {
	dir := t.TempDir()
	if err := Bind(dir, "sess-1", BindSpec{Goal: "goal"}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	if err := SaveVerdict(dir, "sess-1", domain.Verdict{Outcome: domain.OutcomeRejected, Reason: "no evidence"}, 3); err != nil {
		t.Fatalf("SaveVerdict: %v", err)
	}
	rec, _ := Load(dir, "sess-1")
	if rec.NoImprove != 1 {
		t.Errorf("NoImprove = %d, want 1", rec.NoImprove)
	}
	if rec.Verdict == nil || rec.Verdict.Outcome != domain.OutcomeRejected || rec.Verdict.AtCycle != 3 {
		t.Errorf("Verdict = %+v, want {rejected, atCycle 3}", rec.Verdict)
	}

	// a second rejection accumulates.
	if err := SaveVerdict(dir, "sess-1", domain.Verdict{Outcome: domain.OutcomeRejected, Reason: "still no evidence"}, 4); err != nil {
		t.Fatalf("SaveVerdict: %v", err)
	}
	rec, _ = Load(dir, "sess-1")
	if rec.NoImprove != 2 {
		t.Errorf("NoImprove = %d, want 2 after a second rejection", rec.NoImprove)
	}
}

func TestSaveVerdict_DoneOrProgressResetsNoImprove(t *testing.T) {
	dir := t.TempDir()
	if err := Bind(dir, "sess-1", BindSpec{Goal: "goal"}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := SaveVerdict(dir, "sess-1", domain.Verdict{Outcome: domain.OutcomeRejected}, 1); err != nil {
		t.Fatalf("SaveVerdict: %v", err)
	}
	if rec, _ := Load(dir, "sess-1"); rec.NoImprove != 1 {
		t.Fatalf("precondition failed: NoImprove = %d, want 1", rec.NoImprove)
	}

	if err := SaveVerdict(dir, "sess-1", domain.Verdict{Outcome: domain.OutcomeProgress}, 2); err != nil {
		t.Fatalf("SaveVerdict: %v", err)
	}
	if rec, _ := Load(dir, "sess-1"); rec.NoImprove != 0 {
		t.Errorf("NoImprove = %d, want 0 (progress resets the streak)", rec.NoImprove)
	}

	if err := SaveVerdict(dir, "sess-1", domain.Verdict{Outcome: domain.OutcomeRejected}, 3); err != nil {
		t.Fatalf("SaveVerdict: %v", err)
	}
	if err := SaveVerdict(dir, "sess-1", domain.Verdict{Outcome: domain.OutcomeDone}, 4); err != nil {
		t.Fatalf("SaveVerdict: %v", err)
	}
	if rec, _ := Load(dir, "sess-1"); rec.NoImprove != 0 {
		t.Errorf("NoImprove = %d, want 0 (done also resets the streak)", rec.NoImprove)
	}
}

func TestSaveVerdict_PreservesContractFields(t *testing.T) {
	// SaveVerdict re-reads then re-writes the record — DoneCondition/Rubric/
	// Challenger must survive that round trip, not just Goal/caps.
	dir := t.TempDir()
	spec := BindSpec{Goal: "goal", DoneCondition: "tests pass", Rubric: "run tests", Challenger: "adversarial probe"}
	if err := Bind(dir, "sess-1", spec); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := SaveVerdict(dir, "sess-1", domain.Verdict{Outcome: domain.OutcomeProgress}, 1); err != nil {
		t.Fatalf("SaveVerdict: %v", err)
	}
	rec, _ := Load(dir, "sess-1")
	if rec.DoneCondition != "tests pass" || rec.Rubric != "run tests" || rec.Challenger != "adversarial probe" {
		t.Errorf("got %+v, want the contract fields preserved across SaveVerdict", rec)
	}
}

func TestSaveVerdict_UnboundSessionErrors(t *testing.T) {
	if err := SaveVerdict(t.TempDir(), "never-bound", domain.Verdict{Outcome: domain.OutcomeDone}, 1); err == nil {
		t.Error("expected an error saving a verdict for a session with no registry record")
	}
}

// ── LoopEngine MVP: Driven durability ────────────────────────────────────

func TestBind_DrivenTrue_RoundTrips(t *testing.T) {
	dir := t.TempDir()
	if err := Bind(dir, "sess-1", BindSpec{Goal: "goal", Driven: true}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	rec, ok := Load(dir, "sess-1")
	if !ok {
		t.Fatal("expected a record to load")
	}
	if !rec.Driven {
		t.Error("Driven = false, want true (BindSpec.Driven must round-trip)")
	}
}

// TestBind_DrivenOmitted_DefaultsFalse is the opt-in-spike's off-by-default
// contract: a BindSpec that never mentions Driven at all (the zero value —
// every EXISTING caller today, since none of them offer an engine-drive
// choice yet) must produce a NOT-driven record. No engine cycle may ever
// fire for a loop unless it was explicitly created engine-driven.
func TestBind_DrivenOmitted_DefaultsFalse(t *testing.T) {
	dir := t.TempDir()
	if err := Bind(dir, "sess-1", BindSpec{Goal: "goal"}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	rec, _ := Load(dir, "sess-1")
	if rec.Driven {
		t.Error("Driven = true, want false — a BindSpec that never opts in must not be driven")
	}
}

// TestSaveVerdict_PreservesDriven is the captain's exact ask: a driven
// record must survive SaveVerdict's load-mutate-write round trip, not
// silently un-drive itself the first time the oracle judges a cycle.
func TestSaveVerdict_PreservesDriven(t *testing.T) {
	dir := t.TempDir()
	if err := Bind(dir, "sess-1", BindSpec{Goal: "goal", Driven: true}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := SaveVerdict(dir, "sess-1", domain.Verdict{Outcome: domain.OutcomeProgress}, 1); err != nil {
		t.Fatalf("SaveVerdict: %v", err)
	}
	rec, _ := Load(dir, "sess-1")
	if !rec.Driven {
		t.Error("Driven = false after SaveVerdict, want true (must survive the round trip)")
	}
}

func TestMarkDriven_SetsAndClearsFlag(t *testing.T) {
	dir := t.TempDir()
	if err := Bind(dir, "sess-1", BindSpec{Goal: "goal"}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if rec, _ := Load(dir, "sess-1"); rec.Driven {
		t.Fatal("precondition failed: expected Driven=false right after an un-driven Bind")
	}

	if err := MarkDriven(dir, "sess-1", true); err != nil {
		t.Fatalf("MarkDriven(true): %v", err)
	}
	if rec, _ := Load(dir, "sess-1"); !rec.Driven {
		t.Error("Driven = false after MarkDriven(true), want true")
	}

	if err := MarkDriven(dir, "sess-1", false); err != nil {
		t.Fatalf("MarkDriven(false): %v", err)
	}
	if rec, _ := Load(dir, "sess-1"); rec.Driven {
		t.Error("Driven = true after MarkDriven(false), want false — the take-over pause path")
	}
}

// TestMarkDriven_PreservesEveryOtherField: flipping Driven must not
// disturb the goal/contract/verdict/no-improve state already on the
// record — MarkDriven is a single-field flip, not a rebuild.
func TestMarkDriven_PreservesEveryOtherField(t *testing.T) {
	dir := t.TempDir()
	spec := BindSpec{Goal: "goal", DoneCondition: "tests pass", Rubric: "run tests", Challenger: "adversarial probe", MaxCycles: 20}
	if err := Bind(dir, "sess-1", spec); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := SaveVerdict(dir, "sess-1", domain.Verdict{Outcome: domain.OutcomeRejected, Reason: "no evidence"}, 3); err != nil {
		t.Fatalf("SaveVerdict: %v", err)
	}

	if err := MarkDriven(dir, "sess-1", true); err != nil {
		t.Fatalf("MarkDriven: %v", err)
	}

	rec, _ := Load(dir, "sess-1")
	if rec.DoneCondition != "tests pass" || rec.Rubric != "run tests" || rec.Challenger != "adversarial probe" || rec.MaxCycles != 20 {
		t.Errorf("got %+v, want the contract fields untouched by MarkDriven", rec)
	}
	if rec.Verdict == nil || rec.Verdict.Outcome != domain.OutcomeRejected || rec.Verdict.Reason != "no evidence" || rec.Verdict.AtCycle != 3 {
		t.Errorf("Verdict = %+v, want the existing verdict untouched by MarkDriven", rec.Verdict)
	}
	if rec.NoImprove != 1 {
		t.Errorf("NoImprove = %d, want 1 (unaffected by MarkDriven)", rec.NoImprove)
	}
}

func TestMarkDriven_UnboundSessionErrors(t *testing.T) {
	if err := MarkDriven(t.TempDir(), "never-bound", true); err == nil {
		t.Error("expected an error marking Driven on a session with no registry record")
	}
}

// TestWritePending_BindPending_DrivenSurvivesRoundTrip is the pending-spawn
// plumbing check: a spawn request created with the engine-drive choice
// must still be driven once its session is matched and Bound — not lost
// in the WritePending → BindPending → Bind hop.
func TestWritePending_BindPending_DrivenSurvivesRoundTrip(t *testing.T) {
	loopsDir, pendingDir, historyDir := t.TempDir(), t.TempDir(), t.TempDir()
	now := time.Now()
	if err := WritePending(pendingDir, "/x/aboard", BindSpec{Goal: "fix it", Driven: true}); err != nil {
		t.Fatalf("WritePending: %v", err)
	}
	loops := []domain.Loop{
		// LastActivity must be safely AFTER the pending's own internal
		// time.Now() TS (WritePending stamps it, not this test) for
		// BindPending's match window — same margin
		// TestBindPending_HyphenatedWorktreePathBinds uses.
		{SessionID: "sess-1", ProjectDir: domain.EncodeCwd("/x/aboard"), LastActivity: now.Add(time.Minute)},
	}
	BindPending(loopsDir, pendingDir, loops, now, historyDir)

	rec, ok := Load(loopsDir, "sess-1")
	if !ok {
		t.Fatal("expected sess-1 to be bound")
	}
	if !rec.Driven {
		t.Error("Driven = false after BindPending, want true — the engine-drive choice must survive the pending round trip")
	}
}

func TestWritePending_ListPendingRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := WritePending(dir, "/x/aboard", BindSpec{Goal: "fix the bug"}); err != nil {
		t.Fatalf("WritePending: %v", err)
	}

	pending := listPending(dir)
	if len(pending) != 1 {
		t.Fatalf("got %d pending, want 1", len(pending))
	}
	for _, p := range pending {
		if p.Cwd != "/x/aboard" || p.Goal != "fix the bug" {
			t.Errorf("got %+v, want {cwd:/x/aboard goal:fix the bug}", p)
		}
	}
}

func TestWritePending_ListPendingRoundTrip_FullContract(t *testing.T) {
	dir := t.TempDir()
	spec := BindSpec{
		Goal:          "fix the bug",
		DoneCondition: "tests pass",
		Rubric:        "run tests",
		Challenger:    "try to break it",
		MaxCycles:     7,
	}
	if err := WritePending(dir, "/x/aboard", spec); err != nil {
		t.Fatalf("WritePending: %v", err)
	}

	pending := listPending(dir)
	if len(pending) != 1 {
		t.Fatalf("got %d pending, want 1", len(pending))
	}
	for _, p := range pending {
		if p.DoneCondition != spec.DoneCondition || p.Rubric != spec.Rubric || p.Challenger != spec.Challenger || p.MaxCycles != spec.MaxCycles {
			t.Errorf("got %+v, want the full contract carried through pending", p)
		}
	}
}

func TestBindPending_MatchesNewestUnboundSessionInCwd(t *testing.T) {
	loopsDir, pendingDir := t.TempDir(), t.TempDir()
	now := time.Now()
	spawnTS := now.Add(-time.Minute)

	if err := WritePending(pendingDir, "/x/aboard", BindSpec{Goal: "fix the bug"}); err != nil {
		t.Fatalf("WritePending: %v", err)
	}
	// backdate the pending file's ts to spawnTS by rewriting it directly
	// (WritePending always stamps "now" — reuse the round trip, but we need
	// a specific ts for the match window below).
	for name := range listPending(pendingDir) {
		if err := writePendingRaw(pendingDir, name, "/x/aboard", "fix the bug", spawnTS); err != nil {
			t.Fatalf("backdate pending: %v", err)
		}
	}

	loops := []domain.Loop{
		{SessionID: "older", Cwd: "/x/aboard", ProjectDir: "-x-aboard", LastActivity: spawnTS.Add(-time.Hour)}, // pre-existing, before the spawn — must NOT match
		{SessionID: "newer", Cwd: "/x/aboard", ProjectDir: "-x-aboard", LastActivity: spawnTS.Add(time.Second)},
		{SessionID: "other-cwd", Cwd: "/x/other", ProjectDir: "-x-other", LastActivity: spawnTS.Add(time.Second)},
	}

	BindPending(loopsDir, pendingDir, loops, now, t.TempDir())

	rec, ok := Load(loopsDir, "newer")
	if !ok {
		t.Fatal("expected \"newer\" to be bound")
	}
	if rec.Goal != "fix the bug" {
		t.Errorf("Goal = %q, want %q", rec.Goal, "fix the bug")
	}
	if _, ok := Load(loopsDir, "older"); ok {
		t.Error("expected \"older\" (predates the spawn) to NOT be bound")
	}
	if _, ok := Load(loopsDir, "other-cwd"); ok {
		t.Error("expected \"other-cwd\" (different cwd) to NOT be bound")
	}
	if len(listPending(pendingDir)) != 0 {
		t.Error("expected the matched pending file to be removed")
	}
}

func TestBindPending_SkipsAlreadyBoundSessions(t *testing.T) {
	loopsDir, pendingDir := t.TempDir(), t.TempDir()
	now := time.Now()
	spawnTS := now.Add(-time.Minute)

	if err := Bind(loopsDir, "already-bound", BindSpec{Goal: "an earlier goal"}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := WritePending(pendingDir, "/x/aboard", BindSpec{Goal: "new goal"}); err != nil {
		t.Fatalf("WritePending: %v", err)
	}
	for name := range listPending(pendingDir) {
		if err := writePendingRaw(pendingDir, name, "/x/aboard", "new goal", spawnTS); err != nil {
			t.Fatalf("backdate pending: %v", err)
		}
	}

	loops := []domain.Loop{
		{SessionID: "already-bound", Cwd: "/x/aboard", ProjectDir: "-x-aboard", LastActivity: spawnTS.Add(time.Second)},
	}

	BindPending(loopsDir, pendingDir, loops, now, t.TempDir())

	rec, _ := Load(loopsDir, "already-bound")
	if rec.Goal != "an earlier goal" {
		t.Errorf("Goal = %q, want unchanged %q (already-bound sessions must not be rebound)", rec.Goal, "an earlier goal")
	}
	if len(listPending(pendingDir)) != 1 {
		t.Error("expected the pending file to survive (no unbound match found)")
	}
}

func TestBindPending_StalePendingDropped(t *testing.T) {
	loopsDir, pendingDir := t.TempDir(), t.TempDir()
	now := time.Now()
	staleTS := now.Add(-pendingStaleAfter - time.Minute)

	if err := WritePending(pendingDir, "/x/aboard", BindSpec{Goal: "goal"}); err != nil {
		t.Fatalf("WritePending: %v", err)
	}
	for name := range listPending(pendingDir) {
		if err := writePendingRaw(pendingDir, name, "/x/aboard", "goal", staleTS); err != nil {
			t.Fatalf("backdate pending: %v", err)
		}
	}

	// even a perfectly matching session must not be bound once stale.
	loops := []domain.Loop{{SessionID: "s1", Cwd: "/x/aboard", ProjectDir: "-x-aboard", LastActivity: staleTS.Add(time.Second)}}
	BindPending(loopsDir, pendingDir, loops, now, t.TempDir())

	if _, ok := Load(loopsDir, "s1"); ok {
		t.Error("expected no binding once the pending spawn is stale")
	}
	if len(listPending(pendingDir)) != 0 {
		t.Error("expected the stale pending file to be removed")
	}
}

func TestBindPending_NoMatchYet_PendingSurvives(t *testing.T) {
	loopsDir, pendingDir := t.TempDir(), t.TempDir()
	now := time.Now()
	spawnTS := now.Add(-time.Second)

	if err := WritePending(pendingDir, "/x/aboard", BindSpec{Goal: "goal"}); err != nil {
		t.Fatalf("WritePending: %v", err)
	}
	for name := range listPending(pendingDir) {
		if err := writePendingRaw(pendingDir, name, "/x/aboard", "goal", spawnTS); err != nil {
			t.Fatalf("backdate pending: %v", err)
		}
	}

	// no loops in that cwd yet (claude hasn't written its first log line).
	BindPending(loopsDir, pendingDir, nil, now, t.TempDir())

	if len(listPending(pendingDir)) != 1 {
		t.Error("expected the pending file to survive when nothing matches yet")
	}
}

// writePendingRaw overwrites a pending file with an explicit ts, for tests
// that need to control the match window precisely (WritePending always
// stamps time.Now()).
func writePendingRaw(dir, name, cwd, goal string, ts time.Time) error {
	data := []byte(`{"cwd":"` + cwd + `","goal":"` + goal + `","ts":` + strconv.FormatInt(ts.Unix(), 10) + `}`)
	return os.WriteFile(filepath.Join(dir, name), data, 0o644)
}

// Regression: a hyphenated worktree path (mctl-...) must bind even though
// its decoded Cwd is lossy — matching goes real-path→encoded vs ProjectDir
// (the live worktree e2e caught pending records never binding).
func TestBindPending_HyphenatedWorktreePathBinds(t *testing.T) {
	loopsDir, pendingDir := t.TempDir(), t.TempDir()
	now := time.Now()
	real := "/x/orca/workspaces/asre/mctl-reply-with-exactly"
	if err := WritePending(pendingDir, real, BindSpec{Goal: "g", DoneCondition: "d", MaxCycles: 2}); err != nil {
		t.Fatalf("WritePending: %v", err)
	}
	loops := []domain.Loop{{
		SessionID:    "wt",
		ProjectDir:   "-x-orca-workspaces-asre-mctl-reply-with-exactly",
		Cwd:          "/x/orca/workspaces/asre/mctl/reply/with/exactly", // lossy decode — must not matter
		LastActivity: now.Add(time.Minute),
	}}
	BindPending(loopsDir, pendingDir, loops, now, t.TempDir())
	rec, ok := Load(loopsDir, "wt")
	if !ok {
		t.Fatal("hyphenated worktree loop was not bound")
	}
	if rec.DoneCondition != "d" || rec.MaxCycles != 2 {
		t.Fatalf("contract fields lost: %+v", rec)
	}
}

// ── spawn actuation history event (event-log-and-notify) ────────────────

func TestBindPending_SuccessfulMatch_RecordsSpawnActuationEvent(t *testing.T) {
	loopsDir, pendingDir, historyDir := t.TempDir(), t.TempDir(), t.TempDir()
	now := time.Now()
	spawnTS := now.Add(-time.Minute)

	if err := WritePending(pendingDir, "/x/aboard", BindSpec{Goal: "fix the bug"}); err != nil {
		t.Fatalf("WritePending: %v", err)
	}
	for name := range listPending(pendingDir) {
		if err := writePendingRaw(pendingDir, name, "/x/aboard", "fix the bug", spawnTS); err != nil {
			t.Fatalf("backdate pending: %v", err)
		}
	}
	loops := []domain.Loop{{SessionID: "newer", Cwd: "/x/aboard", ProjectDir: "-x-aboard", State: domain.StateRunning, LastActivity: spawnTS.Add(time.Second)}}

	BindPending(loopsDir, pendingDir, loops, now, historyDir)

	got, err := events.ReadAll(historyDir)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	evs := got["newer"]
	if len(evs) != 1 {
		t.Fatalf("got %d events, want exactly 1: %#v", len(evs), evs)
	}
	ev := evs[0]
	if ev.Trigger != events.TriggerActuation {
		t.Errorf("Trigger = %v, want TriggerActuation", ev.Trigger)
	}
	if ev.Actor != events.ActorHuman {
		t.Errorf("Actor = %v, want ActorHuman (a spawn always originates from the \"n\" key)", ev.Actor)
	}
	if ev.FromState != "" {
		t.Errorf("FromState = %q, want empty (brand new session, nothing to transition from)", ev.FromState)
	}
	if ev.ToState != string(domain.StateRunning) {
		t.Errorf("ToState = %q, want %q", ev.ToState, domain.StateRunning)
	}
	if !strings.Contains(ev.Detail, "fix the bug") {
		t.Errorf("Detail = %q, want it to carry the spawned goal", ev.Detail)
	}
}

func TestBindPending_NoMatchYet_NoSpawnEventRecorded(t *testing.T) {
	loopsDir, pendingDir, historyDir := t.TempDir(), t.TempDir(), t.TempDir()
	now := time.Now()
	spawnTS := now.Add(-time.Second)

	if err := WritePending(pendingDir, "/x/aboard", BindSpec{Goal: "goal"}); err != nil {
		t.Fatalf("WritePending: %v", err)
	}
	for name := range listPending(pendingDir) {
		if err := writePendingRaw(pendingDir, name, "/x/aboard", "goal", spawnTS); err != nil {
			t.Fatalf("backdate pending: %v", err)
		}
	}

	BindPending(loopsDir, pendingDir, nil, now, historyDir)

	got, err := events.ReadAll(historyDir)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d sessions with events, want 0 (nothing was actually bound yet)", len(got))
	}
}

func TestCapDetail_TruncatesLongGoal_RuneSafe(t *testing.T) {
	long := strings.Repeat("a", detailCap+50)
	got := capDetail(long)
	runeCount := len([]rune(got))
	if runeCount != detailCap+len([]rune("…")) {
		t.Errorf("got %d runes, want %d (cap + ellipsis)", runeCount, detailCap+1)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("got %q, want a trailing ellipsis marker", got)
	}
}

func TestCapDetail_ShortGoal_Unchanged(t *testing.T) {
	if got := capDetail("short goal"); got != "short goal" {
		t.Errorf("got %q, want unchanged %q", got, "short goal")
	}
}

func TestCapDetail_MultibyteText_NeverSplitsARune(t *testing.T) {
	// a Korean goal longer than detailCap runes — a byte-index cut would
	// slice a multi-byte character in half and corrupt the tail.
	long := strings.Repeat("이", detailCap+10)
	got := capDetail(long)
	if !utf8.ValidString(got) {
		t.Errorf("capDetail produced invalid UTF-8: %q", got)
	}
}
