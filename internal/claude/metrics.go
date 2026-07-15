package claude

import (
	"bufio"
	"encoding/json"
	"os"
	"sync"
	"time"
)

// DefaultBudgetTokens is the v0 default token budget applied when a loop has
// no per-loop budget configured yet (Goal.BudgetTokens == 0) — just enough
// for Loop.BudgetFrac() to render something meaningful until real per-loop
// budgets exist.
const DefaultBudgetTokens = 2_000_000

// sessionMetricsEntry is what metricsCache stores per path, keyed off file
// size+mtime so a full-file scan only re-runs when the file actually
// changed (a full scan every ~3s refresh, for every session, is too heavy).
type sessionMetricsEntry struct {
	size   int64
	mtime  time.Time
	cycles int
	tokens int
}

var metricsCache sync.Map // path (string) -> sessionMetricsEntry

// SessionMetrics returns (cycles, tokensSpent) for a session log:
//   - cycles = count of "type":"user" entries with actual user text (reuses
//     userMessageText) — tool_result-only user entries don't count as a
//     cycle, only real prompts do.
//   - tokensSpent = sum over "type":"assistant" entries of
//     message.usage.output_tokens ONLY (tolerant: missing usage → 0).
//     input_tokens and cache_creation_input_tokens are NOT counted: each
//     re-bills the whole conversation context on every single API call, so
//     summing them across a session wildly overstates spend (43M tokens
//     observed on a real session that hadn't done anywhere near that much
//     work). output_tokens ≈ the actual work generated per turn, which
//     matches the "loop budget" mental model — tokens spent DOING
//     something, not tokens re-read to remember what was already done.
func SessionMetrics(path string) (cycles int, tokensSpent int) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, 0
	}

	if cached, ok := metricsCache.Load(path); ok {
		m := cached.(sessionMetricsEntry)
		if m.size == fi.Size() && m.mtime.Equal(fi.ModTime()) {
			return m.cycles, m.tokens
		}
	}

	cycles, tokensSpent = scanSessionMetrics(path)
	metricsCache.Store(path, sessionMetricsEntry{size: fi.Size(), mtime: fi.ModTime(), cycles: cycles, tokens: tokensSpent})
	return cycles, tokensSpent
}

// scanSessionMetrics does SessionMetrics' actual full-file scan.
func scanSessionMetrics(path string) (cycles int, tokensSpent int) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)

	for scanner.Scan() {
		var entry map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		switch entry["type"] {
		case "user":
			if text, ok := userMessageText(entry); ok && text != "" {
				cycles++
			}
		case "assistant":
			tokensSpent += assistantUsageTokens(entry)
		}
	}
	return cycles, tokensSpent
}

// assistantUsageTokens returns one assistant entry's output_tokens only —
// see SessionMetrics for why input/cache tokens are excluded. Missing/
// malformed usage → 0, tolerant.
func assistantUsageTokens(entry map[string]any) int {
	msg, ok := entry["message"].(map[string]any)
	if !ok {
		return 0
	}
	usage, ok := msg["usage"].(map[string]any)
	if !ok {
		return 0
	}
	return usageInt(usage, "output_tokens")
}

// usageInt reads one usage field; encoding/json decodes JSON numbers into
// float64, so that's the type asserted here.
func usageInt(usage map[string]any, key string) int {
	v, ok := usage[key].(float64)
	if !ok {
		return 0
	}
	return int(v)
}
