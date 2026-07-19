package similarity

import "testing"

func TestDamerauLevenshtein(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want int
	}{
		{"identical", "crash", "crash", 0},
		{"both empty", "", "", 0},
		{"empty against text costs its length", "", "crash", 5},
		{"text against empty costs its length", "crash", "", 5},
		{"single substitution", "crash", "crush", 1},
		{"single insertion", "crash", "crashs", 1},
		{"single deletion", "crash", "cash", 1},
		// The reason this package uses Damerau over plain Levenshtein: a
		// transposition is one typo, and Levenshtein would charge 2 here.
		{"adjacent transposition costs one", "the", "teh", 1},
		// Minimal case: the transposition branch in cheapestEdit only fires
		// when i>1 && j>1, so a two-character swap is the smallest input that
		// can exercise it at all rather than falling back to substitutions.
		{"minimal two-character transposition", "ab", "ba", 1},
		{"transposition inside a word", "receive", "recieve", 1},
		{"nothing in common", "abc", "xyz", 3},
		{"multibyte counts characters not bytes", "제목", "제몫", 1},
		{"multibyte transposition", "한글", "글한", 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := damerauLevenshtein([]rune(tc.a), []rune(tc.b)); got != tc.want {
				t.Errorf("damerauLevenshtein(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// TestDamerauLevenshteinAgainstIndependentOracle cross-checks the DP
// implementation against distances published outside this codebase (the
// Wikipedia Levenshtein-distance and Damerau-Levenshtein-distance articles),
// not against values this package's own author derived by re-running the
// same algorithm. None of these pairs contain a beneficial adjacent
// transposition, so the well-known Levenshtein numbers and this OSA
// implementation must agree exactly — a mismatch here would mean the DP
// recurrence itself is wrong, not just the transposition extension.
func TestDamerauLevenshteinAgainstIndependentOracle(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want int
	}{
		{"wikipedia kitten/sitting", "kitten", "sitting", 3},
		{"wikipedia saturday/sunday", "saturday", "sunday", 3},
		{"wikipedia gumbo/gambol", "gumbo", "gambol", 2},
		{"flaw/lawn classic example", "flaw", "lawn", 2},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := damerauLevenshtein([]rune(tc.a), []rune(tc.b)); got != tc.want {
				t.Errorf("damerauLevenshtein(%q, %q) = %d, want %d (published oracle value)", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// TestDamerauLevenshteinIsRestrictedOSANotFullDamerau pins the implementation
// to the specific variant the package doc comment promises. "ca" vs "abc" is
// the textbook example that separates the two algorithms: the unrestricted
// Damerau-Levenshtein distance is 2 (transpose "ca" -> "ac", then insert "b"
// into the very pair that was just transposed), but OSA forbids editing an
// already-transposed pair again, so its distance is 3. If a future change to
// cheapestEdit accidentally implemented full Damerau, this test — not just
// the doc comment — would catch the behavior change.
func TestDamerauLevenshteinIsRestrictedOSANotFullDamerau(t *testing.T) {
	const a, b = "ca", "abc"
	const osaDistance = 3
	if got := damerauLevenshtein([]rune(a), []rune(b)); got != osaDistance {
		t.Errorf("damerauLevenshtein(%q, %q) = %d, want %d (OSA distance; full Damerau-Levenshtein would be 2)", a, b, got, osaDistance)
	}
}

// TestDamerauLevenshteinTriangleInequalityOnTitleLikeTriples checks
// d(a,c) <= d(a,b) + d(b,c) — the strongest sanity property beyond identity
// and symmetry — over triples shaped like the mutations this package
// actually sees (single typo, transposition, extra trailing word),
// computing all three distances at runtime so the assertion doesn't depend
// on a hand-derived expected number. Scoped to realistic triples on purpose:
// OSA distance is a known non-metric in the general case (that's exactly
// what TestDamerauLevenshteinIsRestrictedOSANotFullDamerau demonstrates), so
// this is "triangle-inequality-ish sanity" for inputs like actual titles,
// not a universal metric proof.
func TestDamerauLevenshteinTriangleInequalityOnTitleLikeTriples(t *testing.T) {
	triples := [][3]string{
		{"crash", "crush", "brush"},
		{"fleet table crashes", "fleet tables crash", "fleet table crash"},
		{"teh quick fox", "the quick fox", "the quick box"},
		{"한글 제목 크래시", "한글 제목 크래쉬", "한글 제목 크래쉬 발생"},
	}

	for _, tr := range triples {
		a, b, c := tr[0], tr[1], tr[2]
		t.Run(a+"/"+b+"/"+c, func(t *testing.T) {
			ab := damerauLevenshtein([]rune(a), []rune(b))
			bc := damerauLevenshtein([]rune(b), []rune(c))
			ac := damerauLevenshtein([]rune(a), []rune(c))
			if ac > ab+bc {
				t.Errorf("triangle inequality violated: d(a,c)=%d > d(a,b)=%d + d(b,c)=%d for %q/%q/%q", ac, ab, bc, a, b, c)
			}
		})
	}
}

func TestDamerauLevenshteinIsSymmetric(t *testing.T) {
	pairs := [][2]string{
		{"fleet table crashes", "fleet tables crash"},
		{"", "anything"},
		{"한글 제목", "한글 제몫"},
		{"teh quick fox", "the quick fox"},
	}

	for _, p := range pairs {
		forward := damerauLevenshtein([]rune(p[0]), []rune(p[1]))
		backward := damerauLevenshtein([]rune(p[1]), []rune(p[0]))
		if forward != backward {
			t.Errorf("asymmetric for %q/%q: %d vs %d", p[0], p[1], forward, backward)
		}
	}
}
