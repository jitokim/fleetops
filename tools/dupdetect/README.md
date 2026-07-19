# dupdetect

Flags likely-duplicate issues and PRs by title similarity. Used by
`.github/workflows/duplicate-detect.yml`; runs standalone too.

Pure string algorithms — no model, no embedding, no network call, no API key.
It is a **nested module** so that repo automation never widens the root
`go.mod` (see `CONTRIBUTING.md`), and it depends on **stdlib only**.

## Usage

```bash
gh issue list --state open --limit 200 --json number,title |
  go run . -number 42 -title "fleet table crashes on resize"
```

```json
{"matches":[{"number":7,"title":"crash on resize","score":0.84}]}
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `-number` | `0` | The new item's number, excluded from its own matches |
| `-title` | `""` | The new item's title |
| `-threshold` | `0.74` | Minimum score in `[0,1]` to report |
| `-top` | `3` | Maximum matches to report |

Output is always a valid `{"matches":[...]}` object, empty list included.
Unknown input fields are ignored, so extra `--json` columns are fine.

## How the score works

Titles are normalized first: lowercased, Conventional Commit prefixes
(`fix:`, `feat(tui)!:`) and issue references (`#12`, `GH-12`) removed, split
on non-alphanumeric runes, stopwords dropped, plurals crudely folded.

Two signals are then combined into `[0,1]`:

- **Damerau-Levenshtein edit ratio (weight 0.4)** — character-level drift.
  Damerau rather than plain Levenshtein because a transposition (`teh`,
  `recieve`) is one human typo but two Levenshtein edits. Computed over
  *sorted* tokens, so it measures character drift only, and leaves word order
  to the other signal.
- **Soft token-set Jaccard overlap (weight 0.6)** — order-free word overlap,
  where two tokens count as shared if they are the same word *or* one typo
  apart. The softness is what separates `instal`/`install` (a duplicate) from
  `zellij`/`wezterm` (two unrelated requests); exact-set Jaccard scores both
  0.6 and cannot tell them apart.

### The threshold, and how honest it is

`0.74` is measured, and the measurements are enforced by
`TestCorpusAccuracyMatchesDocumentedRates` (the rates) and
`TestCorpusCompositionMatchesDocumentedFigures` (the counts quoted below)
rather than restated here — see the comment on `DefaultThreshold` in
`internal/similarity/similarity.go` for the authoritative figures. Change the
corpus and those tests will name the numbers on this page that went stale.
In short: over the 30 labeled pairs it flags every
duplicate that string similarity can reach (12/12) and one non-duplicate
(1/16). Two further pairs are real duplicates no threshold can reach, so true
recall is 12/14.

The classes **overlap**, so no threshold is clean. The worst non-duplicate
scores 0.757 and the weakest duplicate 0.746:

| pair | score | truth |
| --- | --- | --- |
| `fleet table shows wrong count` / `... wrong color` | 0.757 | different bugs |
| `한글 제목 크래시` / `한글 제목 크래시 발생` | 0.750 | same bug |

Both differ by exactly one token. No algorithm that reads titles as strings
can tell them apart, so the corpus labels the first as a `knownFalsePositive`
rather than dropping it to make the numbers look better. Raising to 0.76 buys
zero false positives at the cost of missing two real duplicates; 0.74 favors
recall because the output is advisory and capped at three items.

## What it cannot do

It compares **titles only**, and only as strings.

- **True paraphrase is invisible.** "fleet table shows stale session state" vs
  "sessions display outdated status in the list" scores 0.20 — the same bug
  with almost no shared vocabulary. The corpus tracks these as `knownMiss`.
  Catching them needs semantic matching, which needs a model, which this tool
  deliberately does not have.
- **Plural folding over-strips words whose stem ends in `e`.** "test cases
  fail" vs "test case fails" scores 0.67 and is missed, even though the
  structurally identical "fleet table crashes" / "fleet table crash" scores a
  perfect 1. The `-es` rule turns `cases` into `cas` but leaves `case` alone, so
  the plural stops matching its own singular; `sizes`/`size`, `uses`/`use` and
  `releases`/`release` fail the same way. This is not fixable without a
  dictionary — `cases` (stem `case`) and `classes` (stem `class`) are
  suffix-identical — so the corpus tracks it as a `knownMiss` instead.
- **It never reads the body**, so a detailed report and a terse one about the
  same crash will not match unless their titles do.
- **One differing noun can be the whole bug** (`wrong count` vs `wrong
  color`), and this cannot see that.
- **It only sees the candidates it is given.** The workflow pipes in
  `--limit 200`, and `gh` lists newest-first, so on a tracker with more than
  200 open items the *oldest* silently fall out of range — and the oldest item
  is often the original that a new report duplicates. The tool cannot tell a
  truncated candidate list from a complete one; it reports no match either
  way. Raise the limit if this repo ever crosses that line.

Treat a silent run as "no obvious duplicate", never as "not a duplicate", and
treat a comment as a prompt to look, never as a verdict.

## Tests

```bash
cd tools/dupdetect
gofmt -l . && go vet ./... && go test ./... -count=1 -race
```

The root `make test` does **not** reach this module — nested modules are
excluded from `./...` at the root. `.github/workflows/dupdetect-test.yml` runs
these three gates in CI for exactly that reason.
