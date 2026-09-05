// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Approximate CITEKEY matching, shared by `items find --citekey` and
// `items bibcheck`. Like the title ranker it only ranks candidates: nothing
// here decides that two citekeys name the same entry. Callers present the
// ranking; a person confirms it.
//
// A citekey is not prose, so this is deliberately NOT titleTokenSimilarity.
// A key has no words to pair: "smith2023" is one opaque token, and a
// token-based coefficient scores it either 1 (the token matched) or 0 (it did
// not), which is the exact-equality answer the near path exists to replace.
// The measure here is therefore an edit distance over the WHOLE string.

package cli

import (
	"math"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Scoring constants for approximate citekey matching. They live together so
// tuning is one edit against one corpus (TestCiteKeySimilarityCorpus,
// TestCiteKeySimilarityScoreBounds) rather than a scatter of literals.
const (
	// nearCiteKeyMatchLimit caps how many near keys are reported. The cap is
	// by RANK, never by a user-facing distance threshold: the person who
	// mistyped the key is the last person who should be asked to reason about
	// edit distance.
	nearCiteKeyMatchLimit = 3

	// citeKeyFuzzyMinLen is the shortest key allowed to match approximately,
	// measured on the SHORTER of the two keys, because that is where one edit
	// is most of the string. Below it a single edit changes which key is
	// meant ("ab" against "cb"), so only a key that folds equal is reported.
	citeKeyFuzzyMinLen = 4

	// citeKeyEditBudgetBase and citeKeyEditBudgetPerRunes set the admission
	// bound: one edit is always allowed, and one more per eight runes of the
	// shorter key. The bound SCALES because a long key tolerates more than a
	// short one — one wrong character in "smith2023" is a typo, one wrong
	// character in a four-rune key is a different key. The numbers mirror
	// the title layer's fuzzyTokenMinLen/fuzzyTokenTwoEditLen pair (4 runes,
	// one edit; 8 runes, two) so the two rankers do not disagree about what
	// a typo costs.
	//
	// The bound is measured on the UNWEIGHTED distance. Weighting the gate
	// would reject the commonest real break this exists to catch: a
	// transposed year ("smith2023" typed "smith2032") is two digit
	// substitutions, and no proportionate digit-weighted budget admits it.
	citeKeyEditBudgetBase     = 1
	citeKeyEditBudgetPerRunes = 8

	// citeKeyDigitEditCost is what a differing digit costs in the SCORE, and
	// it is deliberately double a letter. A citekey has structure —
	// author, year, disambiguation suffix — and the year is the part where one
	// changed character means a different work, the same reason the title
	// layer requires numerals to match exactly (titleWordsMatch). The title
	// layer can refuse a digit edit outright because a title carries words
	// either side of the year to match on; a citekey does not, and refusing
	// would mean "smith2032" reported nothing at all. So a digit difference
	// is admitted and then priced: a wrong year ranks below a wrong letter,
	// and a transposed year ranks below both.
	citeKeyDigitEditCost = 2

	// citeKeyPlainEditCost is the unweighted cost model, used for the
	// admission bound. Passed to the same distance function so the gate and
	// the score cannot drift into two implementations.
	citeKeyPlainEditCost = 1

	// citeKeyDistinctMaxScore is the ceiling for a pair that is not the same
	// key after folding. 1.00 has to mean "the same key, written
	// differently", because these rows are printed under a heading that says
	// the keys DIFFER — a score of 1.00 there reads as an exact hit the exact
	// search somehow missed.
	citeKeyDistinctMaxScore = 0.99

	// nearCiteKeyScoreScale rounds a reported score to two decimals. The
	// score is advisory, and publishing 0.5555555555555556 for it invites a
	// caller to compare it against a precision it does not have.
	nearCiteKeyScoreScale = 100
)

// citeKeyCandidate is one library entry considered as a near key.
type citeKeyCandidate struct {
	CiteKey string
	ItemKey string
	Title   string
}

// nearCiteKeyMatch is a ranked candidate key. The title travels with it
// because a key alone cannot tell the reader whether the row is their source,
// and the score travels with it so a bug report about ranking carries the
// number that produced it.
type nearCiteKeyMatch struct {
	CiteKey string  `json:"cite_key"`
	ItemKey string  `json:"item_key,omitempty"`
	Title   string  `json:"title,omitempty"`
	Score   float64 `json:"score"`
}

// rankNearCiteKeys scores candidates against the query key and returns the
// best ones, strongest first.
//
// Ordering is (score desc, cite_key asc). The key tiebreak is what makes two
// runs over the same library print the same list: candidates arrive in map or
// row order, which can reorder equally-scored rows, and an unstable list
// cannot be reproduced from a bug report.
func rankNearCiteKeys(query string, candidates []citeKeyCandidate, limit int) []nearCiteKeyMatch {
	if normalizeMatchCiteKey(query) == "" {
		return nil
	}
	if limit <= 0 {
		limit = nearCiteKeyMatchLimit
	}

	scored := make([]nearCiteKeyMatch, 0, len(candidates))
	for _, candidate := range candidates {
		// Rounded here, where the number is produced, so the score that is
		// printed, serialised and compared is one number. Rounding cannot
		// manufacture 1.00 out of a near key: citeKeySimilarity has already
		// pulled every pair that does not fold equal down to
		// citeKeyDistinctMaxScore.
		raw := citeKeySimilarity(query, candidate.CiteKey)
		if raw <= 0 {
			continue
		}
		scored = append(scored, nearCiteKeyMatch{
			CiteKey: candidate.CiteKey,
			ItemKey: candidate.ItemKey,
			Title:   candidate.Title,
			Score:   math.Round(raw*nearCiteKeyScoreScale) / nearCiteKeyScoreScale,
		})
	}
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].Score != scored[j].Score {
			return scored[i].Score > scored[j].Score
		}
		return scored[i].CiteKey < scored[j].CiteKey
	})
	if len(scored) > limit {
		scored = scored[:limit]
	}
	return scored
}

// normalizeMatchCiteKey folds a citekey to the form used for approximate
// comparison: NFC, trimmed, lowercase. Case only, plus the encoding: a
// citekey is an identifier, so nothing else about it is styling. Separator
// differences ("Smith_2023" against "smith2023") are left as ordinary edits
// rather than folded away, because a dropped underscore is a real difference
// in the key the .bib file has to contain, and the distance already prices it
// as the cheap change it is.
//
// This deliberately does NOT touch how a citekey is matched exactly. Both
// exact layers compare the key as stored — `items find` in findRowMatchesExact
// and `items bibcheck` by grouping on the key itself — because that is the
// string a .bib file and LaTeX have to agree on. So this fold is WIDER than
// the exact test, and a near key scoring 1.00 says something precise: the
// library holds that key differing only in case or in unicode encoding, which
// is the most actionable suggestion this can produce.
func normalizeMatchCiteKey(key string) string {
	return strings.ToLower(norm.NFC.String(strings.TrimSpace(key)))
}

// citeKeySimilarity scores two citekeys in [0,1]. Zero means "not a near
// key", so a caller needs no separate similarity floor: the admission bound
// is the only gate, and it guarantees every non-zero score is at least 0.5
// (TestCiteKeySimilarityIsSymmetricAndNeverWeak pins the worst case).
//
// 1.00 is reserved for a pair that folds equal, which for a citekey means the
// same key in a different case or encoding. Every other pair is capped below
// it, because the rows are printed under a heading that says the keys differ.
func citeKeySimilarity(a, b string) float64 {
	foldedA := normalizeMatchCiteKey(a)
	foldedB := normalizeMatchCiteKey(b)
	if foldedA == "" || foldedB == "" {
		return 0
	}
	if foldedA == foldedB {
		return 1
	}
	runesA := []rune(foldedA)
	runesB := []rune(foldedB)
	shorter, longer := len(runesA), len(runesB)
	if shorter > longer {
		shorter, longer = longer, shorter
	}
	if shorter < citeKeyFuzzyMinLen {
		return 0
	}
	// Both the bound and the score are measured against the SHORTER key,
	// which is the key with less to lose: an edit is a bigger share of it.
	//
	// Measuring against the longer key let a candidate profit from its own
	// extra characters. For the query "smith2032", "smith2023" and
	// "smith2023a" are the same distance away — a transposed year, plus a
	// suffix that the second pays for — and dividing by each candidate's own
	// length ranked "smith2023a" ABOVE the plain key, because its extra
	// character enlarged the divisor. They now tie, and the key tiebreak in
	// rankNearCiteKeys puts the plain key first. It also matters that a
	// disambiguation suffix is how two entries by one author and year are
	// told apart, so an added character is a real difference and must not be
	// the cheapest thing in the model.
	budget := citeKeyEditBudget(shorter)
	if longer-shorter > budget {
		return 0
	}
	// Two passes, and the cheap one first: the unweighted distance is the
	// admission bound, and it rejects all but a handful of candidates with
	// its own early exit, so the weighted pass runs only for keys that are
	// already close.
	if citeKeyDistance(runesA, runesB, budget, citeKeyPlainEditCost) > budget {
		return 0
	}
	// A weighted alignment can cost at most citeKeyDigitEditCost per edit of
	// the unweighted-optimal one, so this budget can never truncate a
	// candidate the gate admitted.
	weighted := citeKeyDistance(runesA, runesB, budget*citeKeyDigitEditCost, citeKeyDigitEditCost)
	score := 1 - float64(weighted)/float64(shorter)
	if score <= 0 {
		return 0
	}
	return math.Min(score, citeKeyDistinctMaxScore)
}

// citeKeyEditBudget returns how many edits a key of this length tolerates.
func citeKeyEditBudget(length int) int {
	return citeKeyEditBudgetBase + length/citeKeyEditBudgetPerRunes
}

// citeKeyDistance returns the edit distance between two rune slices under a
// cost model where an edit involving a digit costs digitCost, giving up as
// soon as every cell in a row exceeds budget. A return value greater than
// budget means only "further than budget". An empty side needs no special
// case: the row initialisation already sums the cost of inserting or
// deleting the other side.
//
// digitCost of citeKeyPlainEditCost makes this ordinary Levenshtein, which is
// how the gate and the score share one implementation instead of drifting
// apart. This duplicates the few lines of boundedLevenshtein in
// title_similarity.go on purpose: that helper takes no cost model, and
// widening it would put a citekey concern inside the title ranker.
func citeKeyDistance(a, b []rune, budget, digitCost int) int {
	previous := make([]int, len(b)+1)
	current := make([]int, len(b)+1)
	for j := 1; j <= len(b); j++ {
		previous[j] = previous[j-1] + runeEditCost(b[j-1], digitCost)
	}
	for i := 1; i <= len(a); i++ {
		current[0] = previous[0] + runeEditCost(a[i-1], digitCost)
		rowMin := current[0]
		for j := 1; j <= len(b); j++ {
			substitution := previous[j-1]
			if a[i-1] != b[j-1] {
				substitution += runePairEditCost(a[i-1], b[j-1], digitCost)
			}
			current[j] = min(
				current[j-1]+runeEditCost(b[j-1], digitCost),
				previous[j]+runeEditCost(a[i-1], digitCost),
				substitution,
			)
			rowMin = min(rowMin, current[j])
		}
		if rowMin > budget {
			return budget + 1
		}
		previous, current = current, previous
	}
	return previous[len(b)]
}

// runeEditCost prices inserting or deleting one rune: a digit costs digitCost,
// because dropping a digit from a year is not the same accident as dropping a
// letter from an author name.
func runeEditCost(r rune, digitCost int) int {
	if unicode.IsDigit(r) {
		return digitCost
	}
	return citeKeyPlainEditCost
}

// runePairEditCost prices substituting one rune for another. A digit on either
// side makes it a digit edit: "smith2023" against "smith202x" changed the year
// just as much as "smith2024" did.
func runePairEditCost(a, b rune, digitCost int) int {
	if unicode.IsDigit(a) || unicode.IsDigit(b) {
		return digitCost
	}
	return citeKeyPlainEditCost
}
