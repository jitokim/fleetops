// Package similarity scores how likely two issue/PR titles describe the same
// thing, using only string algorithms — no model, no embedding, no network.
// It is deliberately pure: no I/O of any kind lives here, so the whole
// package is testable as plain functions and the tool around it can be
// replaced without touching the scoring.
package similarity

import (
	"sort"
	"strings"
)

// Signal weights. The token-set signal is weighted higher because issue
// authors pad and reword the same complaint, which token overlap absorbs and
// character-level distance does not. Edit distance still earns its share: it
// is length-aware, so it separates "one word changed in a five-word title"
// from "one word changed in a fifteen-word title", which Jaccard alone
// flattens.
const (
	editWeight  = 0.4
	tokenWeight = 0.6
)

// DefaultThreshold is the score at or above which a pair is worth showing to a
// human.
//
// Measured, not guessed: over the 29 labeled pairs in similarity_test.go,
// 0.74 flags every duplicate that string similarity can reach (12/12) and one
// non-duplicate (1/16). A 13th pair is a real duplicate too, but a pure
// paraphrase — it is labeled knownMiss and scores 0.200, far out of reach of
// any threshold, so true recall is 12/13.
//
// The two classes genuinely overlap — the worst non-duplicate scores 0.757
// and the weakest duplicate 0.746 — so NO threshold separates them cleanly.
// That overlap is a property of comparing titles as strings, not a tuning
// failure: "fleet table shows wrong count" vs "... wrong color" (different
// bugs) and "한글 제목 크래시" vs "한글 제목 크래시 발생" (same bug) differ
// by exactly one token either way, and no string algorithm can tell which is
// which.
//
// The alternative operating point is 0.76: it reaches 0 false positives by
// giving up 2 real duplicates. 0.74 is chosen over it because the output is
// advisory and capped at a few items — a false positive costs one glance at a
// comment that closes nothing, while a miss costs the silence this tool
// exists to break.
//
// TestCorpusAccuracyMatchesDocumentedRates enforces every number above,
// including the 0.76 counterfactual. Numbers in prose rot; that test is what
// makes this comment trustworthy, so extend it rather than editing figures by
// hand here.
const DefaultThreshold = 0.74

// Candidate is an existing open item a new title is compared against. Number
// identifies it to the caller; this package never interprets it.
type Candidate struct {
	Number int
	Title  string
}

// Match is a Candidate that scored at or above the caller's threshold.
type Match struct {
	Candidate
	Score float64
}

// Score returns how similar two raw titles are, in [0,1]: 1 means identical
// after normalization, 0 means no shared signal. Titles that normalize to
// nothing (empty, pure punctuation, bare "fix:") score 0 — absence of
// content is not evidence of duplication.
func Score(a, b string) float64 {
	left, right := Normalize(a), Normalize(b)
	if left.IsEmpty() || right.IsEmpty() {
		return 0
	}
	return editWeight*editRatio(canonical(left), canonical(right)) + tokenWeight*jaccard(left.Tokens, right.Tokens)
}

// canonical renders a title's tokens in sorted order for the edit-distance
// signal. Sorting is what makes the two signals measure different things:
// against raw word order, "crash on window resize" vs "window resize crash"
// scores 0.37 on edit distance despite being the same sentence, so the edit
// signal spends its weight re-penalizing word order that Jaccard has already
// deliberately ignored. Sorted, that pair scores 1.0 and the edit signal is
// left doing only the job it is good at — catching character-level drift.
func canonical(n Normalized) string {
	return strings.Join(distinctSorted(n.Tokens), " ")
}

// Rank returns the candidates scoring at or above threshold, best first,
// capped at topN. A zero or negative topN returns no matches, and candidates
// whose Number equals excludeNumber are skipped so an item never reports
// itself as its own duplicate.
func Rank(title string, candidates []Candidate, excludeNumber int, threshold float64, topN int) []Match {
	matches := make([]Match, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Number == excludeNumber {
			continue
		}
		if score := Score(title, candidate.Title); score >= threshold {
			matches = append(matches, Match{Candidate: candidate, Score: score})
		}
	}
	sortByScoreDesc(matches)
	if topN < 0 {
		topN = 0
	}
	if len(matches) > topN {
		matches = matches[:topN]
	}
	return matches
}

// sortByScoreDesc orders matches best-first, breaking ties by descending
// number so output is stable regardless of input order — an unstable ranking
// would make the tool's own tests flaky.
func sortByScoreDesc(matches []Match) {
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].Score != matches[j].Score {
			return matches[i].Score > matches[j].Score
		}
		return matches[i].Number > matches[j].Number
	})
}

// editRatio converts edit distance into a [0,1] similarity, normalized by the
// longer string so a short title cannot score high just by being short.
func editRatio(a, b string) float64 {
	ra, rb := []rune(a), []rune(b)
	longest := max(len(ra), len(rb))
	if longest == 0 {
		return 0
	}
	return 1 - float64(damerauLevenshtein(ra, rb))/float64(longest)
}

// tokenMatchRatio is how alike two tokens must be to count as the same word.
// Strict on purpose: at 0.80 a one-character typo in a 5+ character word still
// matches ("instal"/"install" = 0.86), but genuinely different words of
// similar shape do not ("zellij"/"wezterm" = 0.14, "pane"/"session" = 0.14).
// Lowering it collapses that distinction, which is the whole point of the
// signal.
const tokenMatchRatio = 0.80

// jaccard is the token-set overlap |A∩B| / |A∪B|, ignoring order and
// repetition — the signal that catches the same complaint written in a
// different word order.
//
// The overlap is *soft*: a token pair counts as shared when it is the same
// word or one typo away from it. Exact-set Jaccard cannot tell a misspelling
// from a different word — "fleetops instal fails" vs "fleetops install fails"
// and "add zellij backend" vs "add wezterm backend" both lose exactly one
// token, so both score 0.6, even though the first pair is a duplicate and the
// second is two unrelated feature requests. Matching near-identical tokens
// lifts the duplicate without lifting the non-duplicate, which is what makes
// the threshold separable at all.
func jaccard(a, b []string) float64 {
	setA, setB := distinctSorted(a), distinctSorted(b)
	if len(setA) == 0 || len(setB) == 0 {
		return 0
	}
	shared := countSharedTokens(setA, setB)
	return float64(shared) / float64(len(setA)+len(setB)-shared)
}

// countSharedTokens pairs up tokens across the two sets, taking exact matches
// before near-matches. Exact-first matters: a greedy pass alone could spend a
// token on a fuzzy partner that another token needed exactly, making the
// result depend on input order — and Score must be symmetric.
func countSharedTokens(setA, setB []string) int {
	takenA, takenB := make([]bool, len(setA)), make([]bool, len(setB))
	exact := claimPairs(setA, setB, takenA, takenB, func(ta, tb string) bool { return ta == tb })
	near := claimPairs(setA, setB, takenA, takenB, func(ta, tb string) bool { return editRatio(ta, tb) >= tokenMatchRatio })
	return exact + near
}

// claimPairs greedily pairs unclaimed tokens that satisfy isMatch, marking
// both sides as taken so no token is ever counted twice.
//
// Symmetry — Score(a,b) must equal Score(b,a) — is not merely a consequence of
// the sets being sorted, which would only make the walk deterministic in each
// direction separately. It holds because isMatch is symmetric, so the pairing
// graph is undirected, and index-ordered greedy on such a graph yields the
// unique stable matching (every token prefers the lowest-index partner, a
// preference order consistent with a single master list). Both directions
// therefore produce the same matching, not just the same count. A different
// tie-break — "best match first" rather than "first match" — would stay
// deterministic and break this, so keep the walk index-ordered.
func claimPairs(setA, setB []string, takenA, takenB []bool, isMatch func(ta, tb string) bool) int {
	shared := 0
	for i, ta := range setA {
		if takenA[i] {
			continue
		}
		for j, tb := range setB {
			if takenB[j] || !isMatch(ta, tb) {
				continue
			}
			takenA[i], takenB[j] = true, true
			shared++
			break
		}
	}
	return shared
}

// distinctSorted returns the unique tokens in a stable order, so token pairing
// never depends on which title was passed first.
func distinctSorted(tokens []string) []string {
	seen := make(map[string]struct{}, len(tokens))
	unique := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		if _, ok := seen[tok]; ok {
			continue
		}
		seen[tok] = struct{}{}
		unique = append(unique, tok)
	}
	sort.Strings(unique)
	return unique
}
