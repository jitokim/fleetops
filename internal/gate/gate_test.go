package gate

import (
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
