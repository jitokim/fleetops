package registry

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/jitokim/missionctl/internal/domain"
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
	if rec.Oracle != "" {
		t.Errorf("Oracle = %q, want empty (not supplied)", rec.Oracle)
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
		Oracle:        "run go test ./... and check for PASS",
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
	if rec.Oracle != spec.Oracle {
		t.Errorf("Oracle = %q, want %q", rec.Oracle, spec.Oracle)
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
	// SaveVerdict re-reads then re-writes the record — DoneCondition/Oracle/
	// Challenger must survive that round trip, not just Goal/caps.
	dir := t.TempDir()
	spec := BindSpec{Goal: "goal", DoneCondition: "tests pass", Oracle: "run tests", Challenger: "adversarial probe"}
	if err := Bind(dir, "sess-1", spec); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := SaveVerdict(dir, "sess-1", domain.Verdict{Outcome: domain.OutcomeProgress}, 1); err != nil {
		t.Fatalf("SaveVerdict: %v", err)
	}
	rec, _ := Load(dir, "sess-1")
	if rec.DoneCondition != "tests pass" || rec.Oracle != "run tests" || rec.Challenger != "adversarial probe" {
		t.Errorf("got %+v, want the contract fields preserved across SaveVerdict", rec)
	}
}

func TestSaveVerdict_UnboundSessionErrors(t *testing.T) {
	if err := SaveVerdict(t.TempDir(), "never-bound", domain.Verdict{Outcome: domain.OutcomeDone}, 1); err == nil {
		t.Error("expected an error saving a verdict for a session with no registry record")
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
		Oracle:        "run tests",
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
		if p.DoneCondition != spec.DoneCondition || p.Oracle != spec.Oracle || p.Challenger != spec.Challenger || p.MaxCycles != spec.MaxCycles {
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
		{SessionID: "older", Cwd: "/x/aboard", LastActivity: spawnTS.Add(-time.Hour)}, // pre-existing, before the spawn — must NOT match
		{SessionID: "newer", Cwd: "/x/aboard", LastActivity: spawnTS.Add(time.Second)},
		{SessionID: "other-cwd", Cwd: "/x/other", LastActivity: spawnTS.Add(time.Second)},
	}

	BindPending(loopsDir, pendingDir, loops, now)

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
		{SessionID: "already-bound", Cwd: "/x/aboard", LastActivity: spawnTS.Add(time.Second)},
	}

	BindPending(loopsDir, pendingDir, loops, now)

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
	loops := []domain.Loop{{SessionID: "s1", Cwd: "/x/aboard", LastActivity: staleTS.Add(time.Second)}}
	BindPending(loopsDir, pendingDir, loops, now)

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
	BindPending(loopsDir, pendingDir, nil, now)

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
