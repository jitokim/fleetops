package similarity

import (
	"strings"
	"testing"
)

// label is the hand-assigned truth for a corpus pair.
type label int

const (
	// duplicate: a human would call these the same report, and the scorer is
	// expected to agree.
	duplicate label = iota
	// distinct: different reports, which must stay below the threshold.
	distinct
	// knownMiss: genuinely the same report, but beyond what string similarity
	// can see. Asserted to score BELOW the threshold so the limitation is
	// tracked rather than forgotten — if a scoring change ever lifts one of
	// these above the line, this test says so and the label should move to
	// duplicate.
	knownMiss
	// knownFalsePositive: different reports that this scorer flags anyway.
	// Asserted to be ABOVE the threshold for the same reason knownMiss is
	// asserted below it — the failure is recorded as a fact about the tool
	// rather than quietly dropped from the corpus to make the numbers look
	// better.
	knownFalsePositive
)

// corpusPair is one hand-labeled title pair. The corpus is the only evidence
// behind DefaultThreshold, so it lives in the test file, where a threshold
// change is forced to confront it.
type corpusPair struct {
	name  string
	a, b  string
	label label
}

// corpus is drawn from fleetops-shaped titles (backends, TUI, install). The
// non-duplicates are deliberately adversarial: over half of them are SHORT
// titles differing by exactly one token, which is the shape that actually
// breaks title similarity. An earlier version of this corpus contained only
// long, obviously-unrelated non-duplicates, and it certified a threshold that
// flagged 5 of 10 realistic non-duplicate pairs. Do not add a non-duplicate
// here unless it is hard.
var corpus = []corpusPair{
	{"identical titles", "Fleet table crashes on resize", "Fleet table crashes on resize", duplicate},
	{"commit prefix only difference", "fix: fleetops install fails on macOS", "fleetops install fails on macOS", duplicate},
	{"issue reference only difference", "crash on window resize #12", "crash on window resize GH-88", duplicate},
	{"reordered words", "crash on window resize", "window resize crash", duplicate},
	{"typo in one word", "fleetops instal fails on macos", "fleetops install fails on macos", duplicate},
	{"missing apostrophe", "fleetops does not build on windows", "fleetops doesnt build on windows", duplicate},
	{"one extra word", "tmux backend sends keys to the wrong pane", "tmux backend sends keys to wrong pane", duplicate},
	{"dropped leading verb", "add support for zellij backend", "support for zellij backend", duplicate},
	{"singular plural drift", "fleet table crashes", "fleet tables crash", duplicate},
	{"punctuation and casing drift", "cmux backend: attach fails silently", "cmux backend attach fails silently", duplicate},
	{"korean duplicate with extra word", "한글 제목 크래시", "한글 제목 크래시 발생", duplicate},

	{"unrelated subjects", "fleet table crashes on resize", "add zellij backend support", distinct},
	{"unrelated docs vs bug", "docs: fix typo in README", "orca backend fails to attach", distinct},
	{"same subsystem different bug", "tmux backend sends keys to the wrong pane", "tmux backend fails to attach to session", distinct},
	{"same verb different platform", "install fails on macos", "install fails on linux", distinct},
	{"shared stopwords only", "the it is not that", "the one that is it", distinct},
	// Every pair below differs by ONE token in a short title — the exact shape
	// that a naive scorer flags and a maintainer would not.
	{"different backend same request", "add zellij backend support", "add wezterm backend support", distinct},
	{"different backend same failure", "tmux backend fails to attach", "orca backend fails to attach", distinct},
	{"different feature same component", "fleet table sorting is wrong", "fleet table filtering is wrong", distinct},
	{"different version same request", "support go 1.24", "support go 1.25", distinct},
	{"different target same action", "kill action kills wrong session", "kill action kills wrong pane", distinct},
	{"opposite lifecycle events", "crash on startup", "crash on shutdown", distinct},
	{"opposite directions", "increase the poll interval", "decrease the poll interval", distinct},
	{"different arch same failure", "cannot build on arm64", "cannot build on windows", distinct},
	{"different tui feature requests", "add dark mode to the tui", "add vim keybindings to the tui", distinct},
	{"different session symptoms", "session state is stale", "session list is empty", distinct},

	// Reordered with filler words. This was a knownMiss under the original
	// order-sensitive edit signal; sorting the tokens before the edit
	// comparison promoted it to a catch, which is the whole reason that change
	// was made.
	{"reordered with filler words", "crash on window resize", "window resize causes a crash", duplicate},

	// True paraphrase remains the ceiling of a no-model approach: these two
	// share almost no vocabulary, so there is no string for any string
	// algorithm to match on. Only a semantic model would catch it.
	{"same bug described in different words", "fleet table shows stale session state", "sessions display outdated status in the list", knownMiss},

	// Not a limit of string matching in general — a self-inflicted one. This
	// pair is structurally identical to the "singular plural drift" duplicate
	// above, which scores a perfect 1, but here the plural's stem ends in "e",
	// so foldPlurals strips "cases" to "cas" while leaving "case" alone and the
	// two stop sharing a token. See the note on foldPlurals for why no
	// dictionary-free rule fixes it ("cases" and "classes" are suffix-identical)
	// and why it is therefore tracked here rather than patched.
	{"sibilant plural over-strips an e-stem", "test cases fail", "test case fails", knownMiss},

	// The overlap that proves no threshold separates these classes cleanly.
	// Structurally identical to the "korean duplicate with extra word"
	// duplicate above — four shared tokens and one that differs — but here the
	// differing token is the whole bug. Nothing that reads titles as strings
	// can tell these two situations apart.
	{"one noun apart but a different bug", "fleet table shows wrong count", "fleet table shows wrong color", knownFalsePositive},
}

// TestDefaultThresholdSeparatesCorpus is the calibration test: it is what
// makes DefaultThreshold a measured number rather than a guess. Changing the
// weights or the threshold without re-labeling the corpus breaks here.
func TestDefaultThresholdSeparatesCorpus(t *testing.T) {
	for _, tc := range corpus {
		t.Run(tc.name, func(t *testing.T) {
			got := Score(tc.a, tc.b)
			t.Logf("score(%q, %q) = %.3f", tc.a, tc.b, got)
			if tc.label == duplicate && got < DefaultThreshold {
				t.Errorf("labeled duplicate scored %.3f, below threshold %.2f", got, DefaultThreshold)
			}
			if tc.label == distinct && got >= DefaultThreshold {
				t.Errorf("labeled non-duplicate scored %.3f, at or above threshold %.2f", got, DefaultThreshold)
			}
			if tc.label == knownMiss && got >= DefaultThreshold {
				t.Errorf("known miss scored %.3f, now above threshold %.2f — relabel it as a duplicate", got, DefaultThreshold)
			}
			if tc.label == knownFalsePositive && got < DefaultThreshold {
				t.Errorf("known false positive scored %.3f, now below threshold %.2f — relabel it as distinct", got, DefaultThreshold)
			}
		})
	}
}

// TestCorpusAccuracyMatchesDocumentedRates pins the numbers DefaultThreshold's
// doc comment claims. Without this, that comment could drift into fiction the
// moment anyone touches the weights — which is exactly what happened to its
// previous version.
// TestCorpusCompositionMatchesDocumentedFigures pins the corpus's SHAPE, which
// the accuracy test above deliberately does not.
//
// That test asserts RATIOS — "every labeled duplicate is caught", "exactly one
// false positive" — and both stay true no matter how the corpus grows. The
// absolute figures quoted in prose (30 pairs, 12/12, 1/16, true recall 12/14)
// therefore had nothing enforcing them, so adding a single corpus entry
// silently falsified two files while every test stayed green. That already
// happened once: a knownMiss added on 2026-07-19 required 29→30 and 12/13→12/14
// by hand, and the next one would have been missed.
//
// DefaultThreshold's comment claims "TestCorpusAccuracyMatchesDocumentedRates
// enforces every number above." This test is what makes that claim true.
//
// If you changed the corpus on purpose, update the numbers here AND in the two
// places that quote them: DefaultThreshold's doc comment in similarity.go, and
// README.md's accuracy section.
func TestCorpusCompositionMatchesDocumentedFigures(t *testing.T) {
	var duplicates, distincts, knownMisses, knownFalsePositives int
	for _, tc := range corpus {
		switch tc.label {
		case duplicate:
			duplicates++
		case distinct:
			distincts++
		case knownMiss:
			knownMisses++
		case knownFalsePositive:
			knownFalsePositives++
		}
	}

	// Non-duplicates are the denominator the false-positive rate is quoted
	// against: everything a human would NOT call the same report.
	nonDuplicates := distincts + knownFalsePositives
	// True recall counts the duplicates no threshold can reach, which the
	// accuracy test excludes by design.
	reachableAndUnreachableDuplicates := duplicates + knownMisses

	for _, want := range []struct {
		name  string
		got   int
		want  int
		prose string
	}{
		{"corpus size", len(corpus), 30, "\"over the 30 labeled pairs\""},
		{"duplicates", duplicates, 12, "\"(12/12)\""},
		{"non-duplicates", nonDuplicates, 16, "\"one non-duplicate (1/16)\""},
		{"knownMiss", knownMisses, 2, "\"true recall is 12/14\""},
		{"true-recall denominator", reachableAndUnreachableDuplicates, 14, "\"true recall is 12/14\""},
	} {
		if want.got != want.want {
			t.Errorf("%s = %d, want %d — the prose %s in similarity.go and README.md is now wrong; update both or fix the corpus",
				want.name, want.got, want.want, want.prose)
		}
	}
}

func TestCorpusAccuracyMatchesDocumentedRates(t *testing.T) {
	var duplicates, flaggedDuplicates, nonDuplicates, flaggedNonDuplicates int
	for _, tc := range corpus {
		flagged := Score(tc.a, tc.b) >= DefaultThreshold
		switch tc.label {
		case duplicate:
			duplicates++
			if flagged {
				flaggedDuplicates++
			}
		case distinct, knownFalsePositive:
			nonDuplicates++
			if flagged {
				flaggedNonDuplicates++
			}
		}
	}

	t.Logf("recall %d/%d, false positives %d/%d", flaggedDuplicates, duplicates, flaggedNonDuplicates, nonDuplicates)
	if flaggedDuplicates != duplicates {
		t.Errorf("recall is %d/%d, but the threshold is documented as catching every labeled duplicate", flaggedDuplicates, duplicates)
	}
	if flaggedNonDuplicates != 1 {
		t.Errorf("false positives = %d, want exactly 1 (the labeled knownFalsePositive)", flaggedNonDuplicates)
	}
}

// TestZeroFalsePositiveThresholdCostsTwoDuplicates pins the counterfactual
// DefaultThreshold's comment uses to justify choosing 0.74. That sentence
// silently went stale once before — a corpus relabel changed the cost without
// anyone re-deriving the prose — so the trade-off is asserted here instead of
// merely asserted in a comment.
func TestZeroFalsePositiveThresholdCostsTwoDuplicates(t *testing.T) {
	const zeroFalsePositiveThreshold = 0.76

	missed, falsePositives := 0, 0
	for _, tc := range corpus {
		score := Score(tc.a, tc.b)
		switch tc.label {
		case duplicate:
			if score < zeroFalsePositiveThreshold {
				missed++
			}
		case distinct, knownFalsePositive:
			if score >= zeroFalsePositiveThreshold {
				falsePositives++
			}
		}
	}

	if falsePositives != 0 {
		t.Errorf("threshold %.2f has %d false positives, but is documented as the zero-false-positive point", zeroFalsePositiveThreshold, falsePositives)
	}
	if missed != 2 {
		t.Errorf("threshold %.2f misses %d duplicates, but the comment on DefaultThreshold says it costs 2", zeroFalsePositiveThreshold, missed)
	}
}

func TestScoreBounds(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want float64
	}{
		{"identical scores one", "fleet table crashes", "fleet table crashes", 1},
		{"identical after normalization scores one", "fix: Crash on resize #4", "crash on resize", 1},
		{"empty against empty scores zero", "", "", 0},
		{"empty against text scores zero", "", "fleet table crashes", 0},
		{"text against empty scores zero", "fleet table crashes", "", 0},
		{"punctuation only scores zero", "!!!", "???", 0},
		{"bare commit prefixes score zero", "fix:", "fix:", 0},
		{"issue reference only scores zero", "#12", "#12", 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Score(tc.a, tc.b); got != tc.want {
				t.Errorf("Score(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestScoreStaysInUnitInterval(t *testing.T) {
	for _, tc := range corpus {
		got := Score(tc.a, tc.b)
		if got < 0 || got > 1 {
			t.Errorf("Score(%q, %q) = %v, outside [0,1]", tc.a, tc.b, got)
		}
	}
}

// TestScoreOfTotallyDissimilarTitlesIsExactlyZero pins the zero value itself,
// not just "below threshold": "abc" and "xyz" share no character and no
// token, so both the edit-distance and token-set signals contribute nothing,
// and editWeight*0 + tokenWeight*0 must be exactly 0 — not a small positive
// float that happens to round under the threshold.
func TestScoreOfTotallyDissimilarTitlesIsExactlyZero(t *testing.T) {
	if got := Score("abc", "xyz"); got != 0 {
		t.Errorf("Score(%q, %q) = %v, want exactly 0", "abc", "xyz", got)
	}
}

// TestScoreAllStopwordTitleAgainstNormalContentStaysBelowThreshold exercises
// the interaction normalize_test.go cannot see on its own: Normalize's
// stopword fallback ("it is not the that" keeps every token because
// filtering would empty the title) must not make that title collide with an
// unrelated real title just because the fallback token set is non-empty.
func TestScoreAllStopwordTitleAgainstNormalContentStaysBelowThreshold(t *testing.T) {
	got := Score("it is not the that", "fleet table crashes on resize")
	if got >= DefaultThreshold {
		t.Errorf("Score(all-stopword title, unrelated content) = %v, want below threshold %v", got, DefaultThreshold)
	}
}

// TestScoreHandlesVeryLongTitlesWithinUnitInterval guards the DP-table sizing
// in damerauLevenshtein against a title far longer than anything in the
// corpus: identical long titles must still score 1, and the combined score
// must stay in [0,1] for a long title against something short and unrelated.
func TestScoreHandlesVeryLongTitlesWithinUnitInterval(t *testing.T) {
	long := strings.Repeat("fleet table crashes on resize when tmux backend attaches ", 50)

	if got := Score(long, long); got != 1 {
		t.Errorf("Score(long, long) = %v, want 1", got)
	}

	got := Score(long, "add zellij backend support")
	if got < 0 || got > 1 {
		t.Errorf("Score(long, short unrelated) = %v, outside [0,1]", got)
	}
}

func TestScoreIsSymmetric(t *testing.T) {
	for _, tc := range corpus {
		forward, backward := Score(tc.a, tc.b), Score(tc.b, tc.a)
		if forward != backward {
			t.Errorf("Score asymmetric for %q/%q: %v vs %v", tc.a, tc.b, forward, backward)
		}
	}
}

// FuzzScoreIsSymmetric establishes the invariant the greedy token pairing
// rests on. TestScoreIsSymmetric only walks the 29 corpus pairs — the very
// examples the scorer was tuned against — which is the weakest possible
// evidence for a property that must hold universally. The seeds aim at the
// hard case: short tokens from a tiny alphabet, so near-matches are dense and
// ambiguous pairings are common.
func FuzzScoreIsSymmetric(f *testing.F) {
	f.Add("crash on resize", "resize crash")
	f.Add("aab aba abb", "abb aab aba")
	f.Add("fix: a b c #1", "c b a")
	f.Add("", "")
	f.Add("한글 제목", "제목 한글")
	f.Add("ab ba ab", "ba ab ba")

	f.Fuzz(func(t *testing.T, a, b string) {
		forward, backward := Score(a, b), Score(b, a)
		if forward != backward {
			t.Errorf("Score(%q, %q) = %v but Score(%q, %q) = %v", a, b, forward, b, a, backward)
		}
		if forward < 0 || forward > 1 {
			t.Errorf("Score(%q, %q) = %v, outside [0,1]", a, b, forward)
		}
	})
}

func TestRank(t *testing.T) {
	candidates := []Candidate{
		{Number: 1, Title: "fleet table crashes on resize"},
		{Number: 2, Title: "fix: crash on window resize"},
		{Number: 3, Title: "add zellij backend support"},
		{Number: 4, Title: "crash on window resize"},
	}

	tests := []struct {
		name      string
		title     string
		exclude   int
		threshold float64
		topN      int
		want      []int
	}{
		// #2 and #4 tie at a perfect 1 (see the note below), so the tie-break
		// decides the order: oldest first, hence #2 ahead of #4.
		{"ranks best first", "crash on window resize", 0, DefaultThreshold, 5, []int{2, 4}},
		{"excludes itself", "crash on window resize", 4, DefaultThreshold, 5, []int{2}},
		{"topN caps results", "crash on window resize", 0, DefaultThreshold, 1, []int{2}},
		{"zero topN returns nothing", "crash on window resize", 0, DefaultThreshold, 0, nil},
		{"negative topN returns nothing", "crash on window resize", 0, DefaultThreshold, -3, nil},
		// #2 differs only by a "fix:" prefix, so it normalizes to the same
		// title and legitimately scores a perfect 1 alongside #4.
		{"threshold of one keeps only normalization-identical matches", "crash on window resize", 0, 1, 5, []int{2, 4}},
		{"threshold above one keeps nothing", "crash on window resize", 0, 1.1, 5, nil},
		{"unrelated title matches nothing", "update the contributing guide", 0, DefaultThreshold, 5, nil},
		{"empty title matches nothing", "", 0, DefaultThreshold, 5, nil},
		{"punctuation title matches nothing", "???", 0, DefaultThreshold, 5, nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Rank(tc.title, candidates, tc.exclude, tc.threshold, tc.topN)
			assertNumbers(t, got, tc.want)
		})
	}
}

// TestRankOnlyReturnsScoresAtOrAboveThreshold asserts the invariant every
// other Rank test only checks indirectly (by number, at one fixed
// threshold): for any threshold, every returned Match.Score must satisfy
// Score >= threshold. A regression that let Rank filter with a stale or
// off-by-one comparison would slip past tests that only check which numbers
// came back.
func TestRankOnlyReturnsScoresAtOrAboveThreshold(t *testing.T) {
	candidates := []Candidate{
		{Number: 1, Title: "fleet table crashes on resize"},
		{Number: 2, Title: "fix: crash on window resize"},
		{Number: 3, Title: "add zellij backend support"},
		{Number: 4, Title: "crash on window resize"},
	}
	thresholds := []float64{0, 0.1, 0.3, DefaultThreshold, 0.9, 1}

	for _, threshold := range thresholds {
		matches := Rank("crash on window resize", candidates, 0, threshold, len(candidates))
		for _, m := range matches {
			if m.Score < threshold {
				t.Errorf("threshold %v: match #%d scored %v, below the threshold that admitted it", threshold, m.Number, m.Score)
			}
		}
	}
}

// TestRankZeroThresholdReturnsEveryNonExcludedCandidate checks the inclusive
// lower boundary: Score is always in [0,1], so a threshold of exactly 0 must
// admit every candidate except the excluded one — including ones with no
// similarity at all. This is the mirror of the already-covered "threshold
// above one keeps nothing" upper boundary.
func TestRankZeroThresholdReturnsEveryNonExcludedCandidate(t *testing.T) {
	candidates := []Candidate{
		{Number: 1, Title: "fleet table crashes on resize"},
		{Number: 2, Title: "add zellij backend support"},
		{Number: 3, Title: "completely unrelated topic about coffee"},
	}

	got := Rank("crash on window resize", candidates, 2, 0, len(candidates))
	assertNumbers(t, got, []int{1, 3})
}

// TestRankExcludesAllCandidatesSharingExcludedNumber guards against a
// narrower exclude check (e.g. one that only skips the first match) by
// feeding Rank two distinct candidates that both carry the excluded number —
// a caller bug upstream, but Rank must not surface either of them as a
// result if it means an item matching itself.
func TestRankExcludesAllCandidatesSharingExcludedNumber(t *testing.T) {
	candidates := []Candidate{
		{Number: 4, Title: "crash on window resize"},
		{Number: 4, Title: "crash on window resize, duplicate entry"},
		{Number: 5, Title: "crash on window resize too"},
	}

	got := Rank("crash on window resize", candidates, 4, DefaultThreshold, 5)
	assertNumbers(t, got, []int{5})
}

func TestRankWithNoCandidates(t *testing.T) {
	if got := Rank("crash on resize", nil, 0, DefaultThreshold, 5); len(got) != 0 {
		t.Errorf("Rank over no candidates = %v, want empty", got)
	}
}

// A candidate whose title normalizes to nothing must never match, even when
// the incoming title is also empty — otherwise a "fix:" title would flag
// every other malformed title in the repo.
func TestRankIgnoresEmptyCandidateTitles(t *testing.T) {
	candidates := []Candidate{{Number: 1, Title: "fix:"}, {Number: 2, Title: "  "}}
	if got := Rank("fix:", candidates, 0, DefaultThreshold, 5); len(got) != 0 {
		t.Errorf("Rank over empty-titled candidates = %v, want empty", got)
	}
}

// TestRankOrdersTiesOldestFirst pins the tie-break DIRECTION, not merely that
// one exists. Determinism alone would be satisfied by either direction; the
// direction is a product decision, so it is asserted rather than left to
// whatever the comparator happens to do.
//
// The topN case is the one that actually bites: with a descending tie-break
// the lowest number sorts last among equals, so `-top N` drops the ORIGINAL —
// the item a maintainer is looking for — and reports only the later reports
// that duplicate it. That is invisible in the uncapped case, which is why the
// cap is asserted separately here.
func TestRankOrdersTiesOldestFirst(t *testing.T) {
	candidates := []Candidate{
		{Number: 30, Title: "crash on resize"},
		{Number: 10, Title: "crash on resize"},
		{Number: 40, Title: "crash on resize"},
		{Number: 20, Title: "crash on resize"},
	}

	t.Run("uncapped returns every tie oldest first", func(t *testing.T) {
		got := Rank("crash on resize", candidates, 0, DefaultThreshold, len(candidates))
		assertNumbers(t, got, []int{10, 20, 30, 40})
	})

	t.Run("topN keeps the oldest and drops the later duplicates", func(t *testing.T) {
		got := Rank("crash on resize", candidates, 0, DefaultThreshold, 3)
		assertNumbers(t, got, []int{10, 20, 30})
	})
}

func assertNumbers(t *testing.T, got []Match, want []int) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d matches %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i].Number != want[i] {
			t.Errorf("match %d = #%d (%.3f), want #%d", i, got[i].Number, got[i].Score, want[i])
		}
	}
}
