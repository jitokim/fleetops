package similarity

// damerauLevenshtein returns the optimal string alignment (OSA) distance
// between two rune slices: insertions, deletions, substitutions, and
// transpositions of adjacent runes each cost 1.
//
// Damerau rather than plain Levenshtein because human-typed issue titles carry
// transposition typos ("teh", "recieve", "fleetpos"), which plain Levenshtein
// charges 2 edits for — enough to push a genuine duplicate under the
// threshold on a short title. OSA is the restricted Damerau variant: it does
// not allow an already-transposed pair to be edited again. That restriction is
// irrelevant for titles (it only differs on pathological inputs) and it keeps
// the implementation to one extra row instead of an alphabet-sized table.
//
// Operates on runes, not bytes, so a multibyte title is measured in
// characters — byte-wise distance would report three edits for one changed
// Korean syllable.
func damerauLevenshtein(a, b []rune) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}

	prev2 := make([]int, len(b)+1) // row i-2, read only for transpositions
	prev := make([]int, len(b)+1)  // row i-1
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}

	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			curr[j] = cheapestEdit(a, b, i, j, prev2, prev, curr)
		}
		prev2, prev, curr = prev, curr, prev2
	}
	return prev[len(b)]
}

// cheapestEdit picks the lowest-cost operation for cell (i,j), including the
// adjacent-rune transposition that distinguishes Damerau from Levenshtein.
func cheapestEdit(a, b []rune, i, j int, prev2, prev, curr []int) int {
	substitution := 1
	if a[i-1] == b[j-1] {
		substitution = 0
	}
	best := min(curr[j-1]+1, prev[j]+1, prev[j-1]+substitution)

	if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
		best = min(best, prev2[j-2]+1)
	}
	return best
}
