// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Approximate title matching, shared by every surface that has to answer
// "is this the paper I already have?" without an identifier. The contract is
// deliberately narrow: this package ranks candidates and never decides that two
// titles name the same work. Callers present the ranking; a person confirms it.

package cli

import (
	"math"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Scoring constants. These are the only tunable numbers in approximate title
// matching, and they live together so tuning is one edit against one corpus
// (TestTitleSimilarityCorpus, TestTitleSimilarityScoreBounds) rather than a
// scatter of literals.
const (
	// nearTitleMatchLimit caps how many near matches are reported. The cap is
	// by RANK, not by similarity, so the output cannot become a wall of weak
	// matches no matter how the score behaves.
	nearTitleMatchLimit = 5

	// nearTitleMinScore is a junk floor, NOT a "same work" threshold. Nothing
	// in this file decides sameness. Its only job is to keep a candidate that
	// shares one incidental word with the query out of a list a human reads.
	// It sits at half because two titles in the same field routinely share a
	// trailing phrase — "... for Image Recognition" — without being the same
	// paper, and that pair lands just under it.
	nearTitleMinScore = 0.5

	// titleCandidateLimit bounds how many bm25-ranked rows are pulled back for
	// re-ranking. Ten times the reported cap leaves room for bm25 and title
	// similarity to disagree about order without truncating a real match.
	titleCandidateLimit = nearTitleMatchLimit * 10

	// fuzzyTokenMinLen is the shortest token allowed to match approximately.
	// Below it, one edit is most of the word: "the"/"she" would pair up.
	fuzzyTokenMinLen = 4

	// fuzzyTokenTwoEditLen is the length from which two edits are allowed, so a
	// longer word survives a doubled typo without letting short words drift.
	fuzzyTokenTwoEditLen = 8

	// titleInformativeMinLen is the length at which a word starts carrying
	// enough signal to score on. Function words ("the", "of", "for", "and")
	// sit below it and appear in most titles, so counting them made two
	// unrelated titles look half the same.
	titleInformativeMinLen = 4

	// titleInformativeMinCount is how many informative words a title must have
	// before the function words are dropped. Below it there is nothing left to
	// compare: "War and Peace" has one, so discarding "war" and "and" would
	// score it identical to every other title ending in "Peace". The count is
	// tested against BOTH titles of a comparison; titleComparableTokens says
	// why.
	titleInformativeMinCount = 2

	// titleDistinctMaxScore is the ceiling for a pair that is not the same
	// title under normalizeExactTitle. 1.00 has to mean "the same title,
	// written differently", because the near-match list is printed under a
	// heading that says the titles DIFFER: a one-token title against a
	// one-edit typo of itself scored 1.0000, which reads as an exact hit that
	// the exact search somehow missed. The ceiling says instead "as close as
	// the tokens can tell".
	titleDistinctMaxScore = 0.99

	// nearTitleScoreScale rounds a reported score to two decimals. The score
	// is advisory, and JSON published 0.6666666666666666 for it, which invites
	// a caller to compare it against a precision it does not have.
	nearTitleScoreScale = 100

	// exactTitleTrailingPunctuation is stripped from the end of a folded
	// title. A trailing stop or separator is an artefact of the citation the
	// title was copied out of ("Title." / "Title: "), not part of the name of
	// the work.
	exactTitleTrailingPunctuation = " .,;:!?"
)

// exactTitlePunctuationFold folds the punctuation variants that say nothing
// about which work is meant: the same title typed by hand, exported from a
// publisher and copied out of a PDF differs by exactly these. Soft hyphens and
// zero-width marks are removed rather than folded to ASCII, because they are
// line-breaking artefacts that sit INSIDE a word. Being a package-level
// replacer, the table is built once rather than per title.
var exactTitlePunctuationFold = strings.NewReplacer(
	"\u2018", "'", "\u2019", "'", "\u201a", "'", "\u201b", "'", "\u2032", "'",
	"\u201c", `"`, "\u201d", `"`, "\u201e", `"`, "\u201f", `"`, "\u2033", `"`,
	"\u2010", "-", "\u2011", "-", "\u2012", "-", "\u2013", "-", "\u2014", "-",
	"\u2015", "-", "\u2212", "-",
	"\u2026", "...",
	"\u00ad", "", "\u200b", "", "\u200c", "", "\u200d", "", "\ufeff", "",
)

// titleCandidate is one item considered as a near match.
type titleCandidate struct {
	Key   string
	Title string
}

// nearTitleMatch is a ranked candidate. Score is reported so a reader can tell
// a strong match from a weak one, and so a bug report about ranking carries the
// number that produced it.
type nearTitleMatch struct {
	Key   string  `json:"key"`
	Title string  `json:"title"`
	Score float64 `json:"score"`
	// Year is published even when it is empty. A person saw "----" for an
	// undated item while a JSON caller got no key at all and had to guess
	// whether the field was missing or the date was.
	Year     string `json:"year"`
	ItemType string `json:"item_type,omitempty"`
	Trashed  bool   `json:"trashed,omitempty"`
}

// rankNearTitleMatches scores candidates against the query title and returns
// the best ones, strongest first.
//
// Ordering is (score desc, key asc). The key tiebreak is what makes two runs
// over the same library print the same list: candidate order arrives from bm25,
// which can reorder equally-scored rows, and an unstable list cannot be
// reproduced from a bug report.
func rankNearTitleMatches(query string, candidates []titleCandidate, limit int) []nearTitleMatch {
	queryTokens := titleMatchTokens(query)
	if len(queryTokens.all) == 0 {
		return nil
	}
	if limit <= 0 {
		limit = nearTitleMatchLimit
	}

	scored := make([]nearTitleMatch, 0, len(candidates))
	for _, candidate := range candidates {
		// Rounded here, where the number is produced, so the score that is
		// printed, serialised and compared is the same score the floor
		// tested. Rounding cannot manufacture 1.00 out of a near match:
		// titleTokenSimilarity has already pulled every non-identical pair
		// down to titleDistinctMaxScore.
		raw := titleTokenSimilarity(queryTokens, titleMatchTokens(candidate.Title))
		score := math.Round(raw*nearTitleScoreScale) / nearTitleScoreScale
		if score < nearTitleMinScore {
			continue
		}
		scored = append(scored, nearTitleMatch{Key: candidate.Key, Title: candidate.Title, Score: score})
	}
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].Score != scored[j].Score {
			return scored[i].Score > scored[j].Score
		}
		return scored[i].Key < scored[j].Key
	})
	if len(scored) > limit {
		scored = scored[:limit]
	}
	return scored
}

// titleTokens is one title prepared for comparison. Both token lists are kept
// because whether the function words are worth scoring on is a question about
// the PAIR, not about one title; titleComparableTokens answers it.
type titleTokens struct {
	// exact is the title under normalizeExactTitle, kept so a comparison can
	// tell "the same title written differently" from "nearly the same title"
	// without re-folding either side.
	exact string
	// all is every word, function words included.
	all []string
	// informative is all without the function words. It aliases all when the
	// title has none, so the common case allocates nothing.
	informative []string
}

// titleMatchTokens folds a title with normalizeExactTitle, splits it on
// everything that is not a letter or a digit, and reports both the whole word
// list and the words worth scoring on. Splitting that way means punctuation,
// dashes and quoting styles cannot change a score. Folding first means an NFD
// title tokenises the same as its NFC twin: NFD "Nação Brasileira" scored
// 0.3333 against the NFC form, because the combining marks split "nação" into
// three tokens, and the FTS store really does return that candidate — so the
// ranker was discarding a row the query had found.
//
// A word made only of digits is informative however short it is. "Volume 12
// Studies" against "Volume 13 Studies" scored 1.0000 while "12" was dropped
// for being under titleInformativeMinLen, and that number is the only thing
// that tells the two volumes apart.
//
// What NFC folding does NOT fix, and why neither is fixed here:
//
//   - "Theatre" against "Théâtre" still scores 0. Folding diacritics would
//     pair them, but candidate generation is FTS over the stored title
//     (titleCandidateMatchQuery in internal/store/query.go), which keeps
//     letters as they are, so the row never arrives to be scored; folding in
//     Go would change nothing a user can see. It would also merge words that a
//     diacritic separates ("schon"/"schön", "laska"/"łaska"), which changes
//     what counts as the same word. The recall gap belongs in candidate
//     generation, not in the ranker.
//   - A CJK title has no spaces, so it is one token: one changed character
//     stays inside the edit budget for a token that long, and the pair reaches
//     titleDistinctMaxScore. That score is unreachable in practice, because
//     the same one-character change returns no candidate row from FTS at all.
//     Splitting it needs a segmenter, and it would have to be the segmenter
//     the FTS tokenizer uses or the two ends would disagree again.
func titleMatchTokens(title string) titleTokens {
	exact := normalizeExactTitle(title)
	fields := strings.FieldsFunc(exact, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	informative := fields
	for _, field := range fields {
		if titleWordIsInformative(field) {
			continue
		}
		informative = make([]string, 0, len(fields)-1)
		for _, keep := range fields {
			if titleWordIsInformative(keep) {
				informative = append(informative, keep)
			}
		}
		break
	}
	return titleTokens{exact: exact, all: fields, informative: informative}
}

// titleWordIsInformative reports whether a word carries enough signal to score
// on. Length is the test, except for a numeral: "12" is short because volume
// numbers are short, never because it is a function word.
func titleWordIsInformative(word string) bool {
	return len([]rune(word)) >= titleInformativeMinLen || isTitleNumeral(word)
}

// normalizeExactTitle folds a title to the form used for EXACT title equality:
// NFC, lowercase, single spaces, ASCII punctuation, no trailing separator. It
// never drops a word, so two titles that fold together differ only in how they
// were typed or exported — "ATTENTION IS ALL YOU NEED." against "Attention Is
// All You Need", or an en dash against a hyphen.
//
// It is also the boundary between "the same title" and "a near match":
// titleTokenSimilarity may report 1.00 only for a pair that is equal here.
func normalizeExactTitle(title string) string {
	folded := exactTitlePunctuationFold.Replace(norm.NFC.String(title))
	collapsed := strings.Join(strings.Fields(strings.ToLower(folded)), " ")
	return strings.TrimRight(collapsed, exactTitleTrailingPunctuation)
}

// titleComparableTokens decides, once per comparison, which word lists the two
// titles are scored on. The decision has to be JOINT.
//
// Deciding it per title tokenised "The Art of War: Complete Edition" to
// [complete edition] and "The Art of War" to [the art of war]. Those share
// nothing, so the pair scored 0.0000 and the feature dropped the very candidate
// it exists to surface — the silence it was written to remove came straight
// back. Deciding it jointly scores that pair 0.8000, and still scores "The
// Study of Networks" against "The Theory of Games" 0.0000, which is the pair
// the function-word filter exists for.
func titleComparableTokens(a, b titleTokens) (left, right []string) {
	if len(a.informative) >= titleInformativeMinCount && len(b.informative) >= titleInformativeMinCount {
		return a.informative, b.informative
	}
	return a.all, b.all
}

// titleTokenSimilarity returns a Sørensen–Dice coefficient over tokens that are
// allowed to match approximately: 2*shared / (len(a)+len(b)), in [0,1].
//
// Token-level rather than whole-string edit distance, because the two failures
// this has to survive pull in opposite directions. A typo is local, so it must
// cost about one token, not one edit per following character. An added subtitle
// is a block of extra tokens, so it must cost proportionally rather than
// wrecking the score the way a length-normalized string distance does:
// "Attention Is All You Need" against that title plus a three-word subtitle
// scores 2*2/(2+4) = 0.67 here, and near 0.6 under plain edit distance.
//
// Each token pairs at most once, so a repeated word cannot inflate the count.
func titleTokenSimilarity(a, b titleTokens) float64 {
	left, right := titleComparableTokens(a, b)
	if len(left) == 0 || len(right) == 0 {
		return 0
	}
	score := 2 * float64(countTitleWordPairs(left, right)) / float64(len(left)+len(right))
	if a.exact == b.exact {
		return score
	}
	// Approximate pairing can make two different titles look identical: one
	// token one edit apart, or a volume whose number is the only difference.
	// A list headed "no exact match" must never print 1.00, so anything that
	// is not the same title comes back under the ceiling.
	return min(score, titleDistinctMaxScore)
}

// countTitleWordPairs returns how many words the two titles share, where two
// words are shared when titleWordsMatch calls them the same word. Each word
// pairs at most once.
//
// Identical words pair first and the leftovers go through augmenting paths, so
// the result is the MAXIMUM number of pairs. Greedy first-best pairing was both
// wrong and asymmetric: [word words] against [words world] paired "word" with
// "words", had nothing left for "words", and scored 0.5000 one way against
// 1.0000 reversed — so a ranking depended on which of the two titles was the
// query. Maximum cardinality is a property of the pair rather than of the
// order, which makes the score symmetric by construction.
func countTitleWordPairs(left, right []string) int {
	pairedWith := make([]int, len(right))
	for i := range pairedWith {
		pairedWith[i] = -1
	}
	shared := 0
	var unpaired []int
	for i, word := range left {
		paired := false
		for j, other := range right {
			if pairedWith[j] < 0 && word == other {
				pairedWith[j] = i
				shared++
				paired = true
				break
			}
		}
		if !paired {
			unpaired = append(unpaired, i)
		}
	}
	if len(unpaired) == 0 {
		return shared
	}
	// Only the words with no identical partner pay for edit distance, so two
	// identical titles never run the expensive pass at all.
	visited := make([]bool, len(right))
	for _, i := range unpaired {
		for j := range visited {
			visited[j] = false
		}
		if augmentTitleWordPairs(i, left, right, pairedWith, visited) {
			shared++
		}
	}
	return shared
}

// augmentTitleWordPairs looks for an augmenting path from left word i: either a
// free partner, or a partner whose current owner can move elsewhere. Returning
// true means the matching grew by one pair.
func augmentTitleWordPairs(i int, left, right []string, pairedWith []int, visited []bool) bool {
	for j, other := range right {
		if visited[j] || !titleWordsMatch(left[i], other) {
			continue
		}
		visited[j] = true
		if pairedWith[j] < 0 || augmentTitleWordPairs(pairedWith[j], left, right, pairedWith, visited) {
			pairedWith[j] = i
			return true
		}
	}
	return false
}

// titleWordsMatch reports whether two words are close enough to be the same
// word. The edit budget grows with length because one edit in a three-letter
// word changes which word it is, while one edit in a ten-letter word is a typo.
//
// A word made only of digits gets no budget: it matches itself or nothing.
// "World Development Report 2019" scored 1.0000 against the 2018 edition
// because four digits are long enough for one edit, and a year, volume or
// edition number is the one part of a title where a single changed character
// means a different work.
func titleWordsMatch(a, b string) bool {
	if a == b {
		return true
	}
	if isTitleNumeral(a) || isTitleNumeral(b) {
		return false
	}
	ra, rb := []rune(a), []rune(b)
	budget := 0
	if shorter := min(len(ra), len(rb)); shorter >= fuzzyTokenTwoEditLen {
		budget = 2
	} else if shorter >= fuzzyTokenMinLen {
		budget = 1
	}
	if budget == 0 || abs(len(ra)-len(rb)) > budget {
		return false
	}
	return boundedLevenshtein(ra, rb, budget) <= budget
}

// isTitleNumeral reports whether a word is nothing but digits: a year, a
// volume, an edition, a part number.
func isTitleNumeral(word string) bool {
	if word == "" {
		return false
	}
	for _, r := range word {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// boundedLevenshtein returns the edit distance between two rune slices, giving
// up as soon as every cell in a row exceeds budget. The early exit matters
// because this runs once per token pair per candidate; without it a long title
// against fifty candidates does far more work than the answer is worth.
// A return value greater than budget means only "further than budget".
func boundedLevenshtein(a, b []rune, budget int) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	previous := make([]int, len(b)+1)
	current := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		current[0] = i
		rowMin := current[0]
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			current[j] = min(min(current[j-1]+1, previous[j]+1), previous[j-1]+cost)
			rowMin = min(rowMin, current[j])
		}
		if rowMin > budget {
			return budget + 1
		}
		previous, current = current, previous
	}
	return previous[len(b)]
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
