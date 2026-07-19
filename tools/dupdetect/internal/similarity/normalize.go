package similarity

import (
	"regexp"
	"strings"
	"unicode"
)

// conventionalPrefix strips a leading Conventional Commit type from a title
// ("fix(tui)!: cursor jumps" -> "cursor jumps"). Two issues describing the
// same bug are routinely filed as "fix: X" and "X", and the prefix is pure
// noise for similarity: it is metadata about the change, not the subject.
var conventionalPrefix = regexp.MustCompile(`^\s*(feat|fix|docs|chore|refactor|test|perf|build|ci|style|revert)(\([^)]*\))?!?:\s*`)

// issueRef strips cross-references ("#12", "GH-12"). Two reports of the same
// bug cite different issue numbers, so keeping them would push identical
// titles apart — the opposite of what the number is evidence for.
//
// The word boundary is inside the alternation, not in front of it: "#" is not
// a word character, so a leading \b would fail to match a title that is only
// "#12".
var issueRef = regexp.MustCompile(`(?i)(\bgh-|#)\d+\b`)

// stopwords are function words that carry no topical signal but inflate token
// overlap: "the fleet table crashes" and "the fleetops install fails" would
// share "the" for free. Kept deliberately small — an aggressive list starts
// deleting real content words.
var stopwords = map[string]struct{}{
	"a": {}, "an": {}, "the": {},
	"is": {}, "are": {}, "was": {}, "were": {}, "be": {}, "been": {},
	"to": {}, "of": {}, "in": {}, "on": {}, "at": {}, "for": {}, "with": {},
	"and": {}, "or": {}, "but": {}, "if": {}, "then": {},
	"it": {}, "its": {}, "this": {}, "that": {},
	"when": {}, "while": {}, "does": {}, "do": {}, "not": {},
}

// Normalized is a title reduced to the token list every similarity signal
// scores against, so the signals can never disagree about what the title "is".
type Normalized struct {
	Tokens []string
}

// IsEmpty reports whether normalization left nothing to compare. Callers must
// treat this as "no signal" rather than "perfectly similar": two titles that
// are both pure punctuation are not evidence of a duplicate.
func (n Normalized) IsEmpty() bool { return len(n.Tokens) == 0 }

// Normalize lowercases, drops conventional-commit prefixes and issue
// references, splits on anything that is not a letter or digit, and removes
// stopwords. Splitting on unicode categories (not ASCII ranges) keeps
// multibyte titles intact: Korean and CJK text survives as tokens instead of
// being shredded into empty strings.
func Normalize(title string) Normalized {
	// The two replacements differ on purpose: a commit prefix is dropped
	// outright, while an issue reference becomes a space so it still separates
	// the words that surrounded it.
	withoutPrefix := conventionalPrefix.ReplaceAllString(strings.ToLower(title), "")
	cleaned := issueRef.ReplaceAllString(withoutPrefix, " ")
	return Normalized{Tokens: foldPlurals(withoutStopwords(splitWords(cleaned)))}
}

// sibilantPluralSuffixes take "-es" rather than "-s" ("crashes" -> "crash",
// "boxes" -> "box"); stripping only the "s" would leave "crashe", which shares
// no token with "crash".
var sibilantPluralSuffixes = []string{"ses", "xes", "zes", "ches", "shes"}

// foldPlurals collapses the singular/plural drift that splits otherwise
// identical titles ("fleet table crashes" vs "fleet tables crash") into zero
// token overlap. This is a crude suffix rule, not a stemmer: it will mangle
// words like "less" -> "les". That is tolerable because it is applied to both
// sides of every comparison, so a consistent mangling still matches itself —
// and a real stemmer would mean a dependency, which this module cannot take.
func foldPlurals(tokens []string) []string {
	folded := make([]string, len(tokens))
	for i, tok := range tokens {
		folded[i] = singularize(tok)
	}
	return folded
}

func singularize(token string) string {
	for _, suffix := range sibilantPluralSuffixes {
		if len(token) > len(suffix) && strings.HasSuffix(token, suffix) {
			return strings.TrimSuffix(token, "es")
		}
	}
	if len(token) > 3 && strings.HasSuffix(token, "s") && !strings.HasSuffix(token, "ss") {
		return strings.TrimSuffix(token, "s")
	}
	return token
}

// splitWords tokenizes on any non-alphanumeric rune, so punctuation, emoji and
// markup separate words instead of becoming part of them.
func splitWords(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// withoutStopwords drops function words, but restores the original tokens if
// filtering emptied the title. "Is it not the one that does?" is all
// stopwords; comparing it as empty would silently match every other
// all-stopword title, so the unfiltered form is the safer fallback.
func withoutStopwords(tokens []string) []string {
	kept := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		if _, isStop := stopwords[tok]; !isStop {
			kept = append(kept, tok)
		}
	}
	if len(kept) == 0 {
		return tokens
	}
	return kept
}
