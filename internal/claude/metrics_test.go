package claude

import (
	"os"
	"testing"
	"time"
)

func TestSessionMetrics_CyclesCountsOnlyRealUserText(t *testing.T) {
	path := writeJSONL(t,
		`{"type":"user","message":{"content":"first prompt"}}`,
		`{"type":"assistant","message":{"content":"working","usage":{"input_tokens":10,"output_tokens":20}}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"ok"}]}}`,
		`{"type":"user","message":{"content":"second prompt"}}`,
	)

	cycles, tokens := SessionMetrics(path)
	if cycles != 2 {
		t.Errorf("cycles = %d, want 2 (the tool_result-only user entry shouldn't count)", cycles)
	}
	if tokens != 20 {
		t.Errorf("tokens = %d, want 20 (output_tokens only)", tokens)
	}
}

func TestSessionMetrics_TokensOnlyCountOutputTokens(t *testing.T) {
	// input_tokens and cache_creation_input_tokens re-count the whole
	// context on every call and must be excluded — otherwise a long
	// conversation's spend explodes (43M observed on a real session).
	path := writeJSONL(t,
		`{"type":"user","message":{"content":"go"}}`,
		`{"type":"assistant","message":{"content":"a","usage":{"input_tokens":100000,"output_tokens":50,"cache_creation_input_tokens":25000,"cache_read_input_tokens":99999}}}`,
		`{"type":"assistant","message":{"content":"b"}}`,
	)

	_, tokens := SessionMetrics(path)
	if tokens != 50 {
		t.Errorf("tokens = %d, want 50 (only output_tokens counted; missing-usage line contributes 0)", tokens)
	}
}

func TestSessionMetrics_EmptyFile(t *testing.T) {
	path := writeJSONL(t)
	cycles, tokens := SessionMetrics(path)
	if cycles != 0 || tokens != 0 {
		t.Errorf("got (%d, %d), want (0, 0) for an empty file", cycles, tokens)
	}
}

func TestSessionMetrics_MissingFile(t *testing.T) {
	cycles, tokens := SessionMetrics("/no/such/path.jsonl")
	if cycles != 0 || tokens != 0 {
		t.Errorf("got (%d, %d), want (0, 0) for a missing file", cycles, tokens)
	}
}

func TestSessionMetrics_CacheInvalidatesOnChange(t *testing.T) {
	path := writeJSONL(t, `{"type":"user","message":{"content":"one"}}`)

	cycles, _ := SessionMetrics(path)
	if cycles != 1 {
		t.Fatalf("initial cycles = %d, want 1", cycles)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	if _, err := f.WriteString(`{"type":"user","message":{"content":"two"}}` + "\n"); err != nil {
		t.Fatalf("append: %v", err)
	}
	f.Close()

	// force mtime forward in case the append landed within the same
	// filesystem timestamp tick.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	cycles, _ = SessionMetrics(path)
	if cycles != 2 {
		t.Errorf("cycles after append = %d, want 2 (cache must invalidate on size/mtime change)", cycles)
	}
}

func TestPruneMetricsCache_RemovesUntrackedPaths(t *testing.T) {
	pathA := writeJSONL(t, `{"type":"user","message":{"content":"a"}}`)
	pathB := writeJSONL(t, `{"type":"user","message":{"content":"b"}}`)

	SessionMetrics(pathA)
	SessionMetrics(pathB)
	if _, ok := metricsCache.Load(pathA); !ok {
		t.Fatal("precondition failed: pathA should be cached")
	}

	pruneMetricsCache(map[string]bool{pathB: true})

	if _, ok := metricsCache.Load(pathA); ok {
		t.Error("expected pathA's cache entry to be pruned (not in the keep set)")
	}
	if _, ok := metricsCache.Load(pathB); !ok {
		t.Error("expected pathB's cache entry to survive (in the keep set)")
	}
}
