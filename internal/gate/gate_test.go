package gate

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestWriteMarker_PendingRoundTrip(t *testing.T) {
	dir := t.TempDir()

	if err := WriteMarker(dir, "sess-abc123", "approve merge to main?", "permission_prompt"); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}

	pending := Pending(dir)
	info, ok := pending["sess-abc123"]
	if !ok {
		t.Fatal("expected a pending entry for sess-abc123")
	}
	if info.Message != "approve merge to main?" {
		t.Errorf("Message = %q, want %q", info.Message, "approve merge to main?")
	}
	if time.Since(info.TS) > 5*time.Second {
		t.Errorf("TS = %v, want close to now", info.TS)
	}
}

func TestWriteMarker_CreatesDir(t *testing.T) {
	dir := t.TempDir() + "/nested/gates"
	if err := WriteMarker(dir, "sess-1", "hi", "permission_prompt"); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
	if len(Pending(dir)) != 1 {
		t.Error("expected mkdirAll to have created the nested dir and the marker to be readable")
	}
}

func TestPending_EmptyOrMissingDir(t *testing.T) {
	if p := Pending(t.TempDir()); len(p) != 0 {
		t.Errorf("got %+v, want empty for an empty dir", p)
	}
	if p := Pending("/no/such/dir"); len(p) != 0 {
		t.Errorf("got %+v, want empty for a missing dir", p)
	}
}

func TestPending_SkipsMalformedAndNonJSONFiles(t *testing.T) {
	dir := t.TempDir()
	if err := WriteMarker(dir, "good", "ok", "permission_prompt"); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
	writeRaw(t, dir+"/bad.json", "not json at all")
	writeRaw(t, dir+"/ignored.txt", `{"message":"x","ts":1}`)

	pending := Pending(dir)
	if len(pending) != 1 {
		t.Fatalf("got %d entries, want 1 (malformed/non-.json files skipped): %+v", len(pending), pending)
	}
	if _, ok := pending["good"]; !ok {
		t.Error("expected the well-formed marker to survive")
	}
}

func TestDeleteMarker(t *testing.T) {
	dir := t.TempDir()
	if err := WriteMarker(dir, "sess-1", "hi", "permission_prompt"); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
	DeleteMarker(dir, "sess-1")
	if len(Pending(dir)) != 0 {
		t.Error("expected marker to be gone after DeleteMarker")
	}
}

func TestDeleteMarker_MissingFileIsHarmless(t *testing.T) {
	// must not panic or otherwise fail loudly.
	DeleteMarker(t.TempDir(), "does-not-exist")
}

func TestDeleteMarkerIfTS_MatchingTS_Deletes(t *testing.T) {
	dir := t.TempDir()
	if err := WriteMarker(dir, "s1", "msg", "permission_prompt"); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
	ts := Pending(dir)["s1"].TS.UnixNano()

	if !DeleteMarkerIfTS(dir, "s1", ts) {
		t.Error("expected DeleteMarkerIfTS to succeed with the matching TS")
	}
	if len(Pending(dir)) != 0 {
		t.Error("expected the marker to be gone")
	}
}

func TestDeleteMarkerIfTS_MismatchedTS_Survives(t *testing.T) {
	dir := t.TempDir()
	if err := WriteMarker(dir, "s1", "msg", "permission_prompt"); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
	ts := Pending(dir)["s1"].TS.UnixNano()

	// simulate a FRESH marker having landed (different TS) between the
	// caller's snapshot and the delete call — the stale delete must not
	// destroy it.
	if DeleteMarkerIfTS(dir, "s1", ts-999) {
		t.Error("expected DeleteMarkerIfTS to refuse — the on-disk TS doesn't match")
	}
	if len(Pending(dir)) != 1 {
		t.Error("expected the marker to survive (a fresh marker must not be destroyed by a stale delete)")
	}
}

func TestDeleteMarkerIfTS_MissingFile(t *testing.T) {
	if DeleteMarkerIfTS(t.TempDir(), "nope", 12345) {
		t.Error("expected false for a missing marker")
	}
}

func TestDeleteMarkerIfTS_SameSecondDifferentNano_WindowClosed(t *testing.T) {
	// The whole point of the nanosecond migration: two markers landing
	// within the SAME SECOND must still be distinguishable by TS — under
	// the old seconds-scale TS they'd have been indistinguishable, and a
	// stale-delete based on the old marker's TS could have destroyed a
	// fresh one that arrived a few hundred milliseconds later in that same
	// second.
	dir := t.TempDir()
	base := time.Date(2026, 1, 1, 12, 0, 0, 100_000_000, time.UTC) // :00.100
	staleNanos := base.UnixNano()
	freshNanos := base.Add(400 * time.Millisecond).UnixNano() // :00.500 — same second, different nano

	if staleNanos/int64(time.Second) != freshNanos/int64(time.Second) {
		t.Fatal("test setup bug: the two timestamps must share the same whole second")
	}

	writeMarkerWithTS(t, dir, "s1", "fresh gate", "permission_prompt", freshNanos)

	// A caller snapshotted the OLD marker's (stale) TS and decides to
	// delete based on it — must refuse, since the on-disk TS is now the
	// fresh one landed in the same second.
	if DeleteMarkerIfTS(dir, "s1", staleNanos) {
		t.Error("expected DeleteMarkerIfTS to refuse — the fresh, same-second marker must survive")
	}
	if len(Pending(dir)) != 1 {
		t.Error("expected the fresh marker to survive the stale, same-second delete attempt")
	}
}

func TestPending_LegacySecondsScaleTS_Normalized(t *testing.T) {
	// a marker file written before the nanosecond migration has TS in unix
	// seconds — Pending must still interpret it correctly (not as a
	// nanosecond value, which would decode to a moment in 1970).
	dir := t.TempDir()
	legacySeconds := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC).Unix()
	writeMarkerWithTS(t, dir, "legacy", "old-style marker", "permission_prompt", legacySeconds)

	info, ok := Pending(dir)["legacy"]
	if !ok {
		t.Fatal("expected a pending entry")
	}
	if info.TS.Year() < 2020 {
		t.Errorf("TS = %v, want ~2026 (legacy seconds-scale TS must be upgraded to nanos, not misread as epoch-1970)", info.TS)
	}
}

func TestDeleteMarkerIfTS_LegacySecondsScale_MatchesNormalizedTS(t *testing.T) {
	dir := t.TempDir()
	legacySeconds := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC).Unix()
	writeMarkerWithTS(t, dir, "legacy", "old-style marker", "permission_prompt", legacySeconds)

	ts := Pending(dir)["legacy"].TS.UnixNano()

	if !DeleteMarkerIfTS(dir, "legacy", ts) {
		t.Error("expected DeleteMarkerIfTS to match a legacy seconds-scale marker via its normalized TS")
	}
}

// writeMarkerWithTS writes a marker file with an explicit TS (nanoseconds),
// bypassing WriteMarker's time.Now() stamp — used to construct
// same-second-different-nanosecond fixtures precisely.
func writeMarkerWithTS(t *testing.T, dir, sessionID, message, notificationType string, tsNanos int64) {
	t.Helper()
	data, err := json.Marshal(markerFile{Message: message, Type: notificationType, TS: tsNanos})
	if err != nil {
		t.Fatalf("marshal marker fixture: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir gates dir: %v", err)
	}
	if err := os.WriteFile(dir+"/"+sessionID+".json", data, 0o644); err != nil {
		t.Fatalf("write marker fixture: %v", err)
	}
}

func TestIsGateActive(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name        string
		markerTS    time.Time
		logMtime    time.Time
		wantActive  bool
		description string
	}{
		{"log written before gate fired", base, base.Add(-time.Minute), true, "nothing written after the gate — still active"},
		{"log written exactly at gate time", base, base, true, "no new writes — active"},
		{"log written within slack after gate", base, base.Add(1500 * time.Millisecond), true, "within the 2s slack — still active"},
		{"log written well after gate", base, base.Add(10 * time.Second), false, "transcript moved on — human already answered, stale"},
		{"log written exactly at the slack boundary", base, base.Add(2 * time.Second), true, "boundary itself counts as active (not After)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := IsGateActive(c.markerTS, c.logMtime)
			if got != c.wantActive {
				t.Errorf("IsGateActive(%v, %v) = %v, want %v (%s)", c.markerTS, c.logMtime, got, c.wantActive, c.description)
			}
		})
	}
}

func writeRaw(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture %s: %v", path, err)
	}
}
