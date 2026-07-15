package control

import "testing"

func TestOrcaResumeCmd(t *testing.T) {
	got := orcaResumeCmd("term_abc123", "hello world")
	want := []string{"orca", "terminal", "send", "--terminal", "term_abc123", "--text", "hello world", "--enter", "--json"}

	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	hasEnter := false
	for _, a := range got {
		if a == "--enter" {
			hasEnter = true
		}
	}
	if !hasEnter {
		t.Error("argv missing --enter")
	}
}

func TestOrcaResumeCmd_EmptyPromptFallback(t *testing.T) {
	got := orcaResumeCmd("term_abc123", "")
	want := []string{"orca", "terminal", "send", "--terminal", "term_abc123", "--text", "", "--enter", "--json"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestParseOrcaTerminals_PicksConnectedWritable(t *testing.T) {
	fixture := []byte(`{
		"terminals": [
			{"handle": "term_wrong", "worktreePath": "/Users/imac/IdeaProjects/other", "connected": true, "writable": true},
			{"handle": "term_stale", "worktreePath": "/Users/imac/IdeaProjects/aboard", "connected": false, "writable": false},
			{"handle": "term_live", "worktreePath": "/Users/imac/IdeaProjects/aboard", "connected": true, "writable": true}
		],
		"totalCount": 3,
		"truncated": false
	}`)

	target, ok := parseOrcaTerminals(fixture, "-Users-imac-IdeaProjects-aboard")
	if !ok {
		t.Fatal("expected ok=true")
	}
	want := Target{Backend: "orca", ID: "term_live", Cwd: "/Users/imac/IdeaProjects/aboard"}
	if target != want {
		t.Errorf("got %+v, want %+v", target, want)
	}
}

func TestParseOrcaTerminals_FallsBackToFirstMatchWhenNoneWritable(t *testing.T) {
	fixture := []byte(`{
		"terminals": [
			{"handle": "term_a", "worktreePath": "/Users/imac/IdeaProjects/aboard", "connected": false, "writable": false},
			{"handle": "term_b", "worktreePath": "/Users/imac/IdeaProjects/aboard", "connected": false, "writable": false}
		]
	}`)

	target, ok := parseOrcaTerminals(fixture, "-Users-imac-IdeaProjects-aboard")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if target.ID != "term_a" {
		t.Errorf("got ID %q, want first match term_a", target.ID)
	}
}

func TestParseOrcaTerminals_NoMatch(t *testing.T) {
	fixture := []byte(`{"terminals": [{"handle": "term_a", "worktreePath": "/Users/imac/IdeaProjects/other", "connected": true, "writable": true}]}`)

	if _, ok := parseOrcaTerminals(fixture, "-Users-imac-IdeaProjects-aboard"); ok {
		t.Error("expected ok=false when no worktreePath matches projectDir")
	}
}

func TestParseOrcaTerminals_EmptyTerminals(t *testing.T) {
	if _, ok := parseOrcaTerminals([]byte(`{"terminals": []}`), "-Users-imac-IdeaProjects-aboard"); ok {
		t.Error("expected ok=false for empty terminals list")
	}
}

func TestParseOrcaTerminals_GarbageJSON(t *testing.T) {
	if _, ok := parseOrcaTerminals([]byte(`not json`), "-Users-imac-IdeaProjects-aboard"); ok {
		t.Error("expected ok=false for unparseable JSON")
	}
}
