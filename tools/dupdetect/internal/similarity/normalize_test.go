package similarity

import (
	"reflect"
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	tests := []struct {
		name  string
		title string
		want  []string
	}{
		{"plain title lowercases", "Fleet Table Crashes", []string{"fleet", "table", "crash"}},
		{"strips conventional commit prefix", "fix: cursor jumps", []string{"cursor", "jump"}},
		{"strips scoped breaking prefix", "feat(tui)!: add pane split", []string{"add", "pane", "split"}},
		{"strips docs prefix", "docs: update README", []string{"update", "readme"}},
		{"keeps colon words that are not commit types", "warning: disk full", []string{"warning", "disk", "full"}},
		{"strips hash issue reference", "crash on resize #123", []string{"crash", "resize"}},
		{"strips gh issue reference", "crash on resize GH-42", []string{"crash", "resize"}},
		{"strips punctuation", "tmux: keys go to the *wrong* pane!", []string{"tmux", "key", "go", "wrong", "pane"}},
		{"drops stopwords", "the crash is in the fleet table", []string{"crash", "fleet", "table"}},
		{"keeps digits as tokens", "go 1.25 build fails", []string{"go", "1", "25", "build", "fail"}},
		{"empty input yields no tokens", "", nil},
		{"whitespace only yields no tokens", "   \t\n ", nil},
		{"punctuation only yields no tokens", "!!! ??? ---", nil},
		{"bare commit prefix yields no tokens", "fix:", nil},
		{"all-stopword title falls back to unfiltered tokens", "it is not the that",
			[]string{"it", "is", "not", "the", "that"}},
		{"korean multibyte survives", "한글 제목 크래시", []string{"한글", "제목", "크래시"}},
		{"cjk without spaces stays one token", "窗口崩溃", []string{"窗口崩溃"}},
		{"emoji is a separator not a token", "crash 🔥 on resize", []string{"crash", "resize"}},
		{"digit-only title keeps digits as tokens", "404 500", []string{"404", "500"}},
		// A digit and a Korean letter with no separator between them form one
		// token, because splitWords groups any unbroken run of
		// unicode.IsLetter || unicode.IsDigit runes — the same rule that
		// keeps CJK text unshredded also fuses "2차" into a single token
		// rather than splitting it into "2" and "차".
		{"mixed korean/english/digit title", "TUI 크래시 API 오류 2차 on resize",
			[]string{"tui", "크래시", "api", "오류", "2차", "resize"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Normalize(tc.title)
			if len(got.Tokens) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got.Tokens, tc.want) {
				t.Errorf("Normalize(%q).Tokens = %q, want %q", tc.title, got.Tokens, tc.want)
			}
		})
	}
}

// "all-stopword title falls back" above proves the fallback keeps tokens; this
// proves the fallback does not also disable emptiness for genuinely empty
// input, which would make every punctuation-only title match every other.
func TestNormalizeIsEmpty(t *testing.T) {
	tests := []struct {
		title string
		want  bool
	}{
		{"", true},
		{"   ", true},
		{"###", true},
		{"fix:", true},
		{"#42", true},
		{"the", false},
		{"crash", false},
	}

	for _, tc := range tests {
		t.Run(tc.title, func(t *testing.T) {
			if got := Normalize(tc.title).IsEmpty(); got != tc.want {
				t.Errorf("Normalize(%q).IsEmpty() = %v, want %v", tc.title, got, tc.want)
			}
		})
	}
}

// One title exercising every normalization step at once: scoped commit
// prefix, casing, stopword, plural fold, and trailing issue reference.
func TestNormalizeAppliesEveryStep(t *testing.T) {
	got := Normalize("fix(tui): the fleet table CRASHES on resize #7").Tokens
	want := []string{"fleet", "table", "crash", "resize"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Tokens = %q, want %q", got, want)
	}
}

// TestNormalizeHandlesVeryLongTitleWithoutLosingTokens guards against an
// off-by-one or truncation that only shows up past whatever short length
// every other fixture happens to use. It repeats a plural/singular pair many
// times so the assertion can check both volume (token count scales exactly
// with repetition) and that folding stays correct all the way through a long
// input, not just in the first few tokens.
func TestNormalizeHandlesVeryLongTitleWithoutLosingTokens(t *testing.T) {
	const repeats = 200
	title := strings.Repeat("fleet tables crash ", repeats)

	got := Normalize(title).Tokens

	wantCount := 3 * repeats
	if len(got) != wantCount {
		t.Fatalf("got %d tokens, want %d", len(got), wantCount)
	}
	if got[0] != "fleet" || got[1] != "table" || got[2] != "crash" {
		t.Errorf("first triple = %v, want [fleet table crash]", got[:3])
	}
	last := got[len(got)-3:]
	if last[0] != "fleet" || last[1] != "table" || last[2] != "crash" {
		t.Errorf("last triple = %v, want [fleet table crash]", last)
	}
}

func TestSingularize(t *testing.T) {
	tests := []struct {
		token string
		want  string
	}{
		{"crashes", "crash"},
		{"boxes", "box"},
		{"tables", "table"},
		{"keys", "key"},
		{"crash", "crash"},
		{"is", "is"},     // too short to strip
		{"less", "less"}, // double-s is not a plural
		// Accepted mangling: not a plural, but folded consistently on both
		// sides of a comparison, so it still matches itself.
		{"status", "statu"},
		{"", ""},
		{"한글", "한글"},
	}

	for _, tc := range tests {
		t.Run(tc.token, func(t *testing.T) {
			if got := singularize(tc.token); got != tc.want {
				t.Errorf("singularize(%q) = %q, want %q", tc.token, got, tc.want)
			}
		})
	}
}
