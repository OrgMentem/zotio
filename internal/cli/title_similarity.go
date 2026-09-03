// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Approximate title matching, shared by every surface that has to answer
// "is this the paper I already have?" without an identifier. The contract is
// deliberately narrow: this package ranks candidates and never decides that two
// titles name the same work. Callers present the ranking; a person confirms it.

package cli

import (
	"sort"
	"strings"
	"unicode"
)

// Scoring constants. These are the only tunable numbers in approximate title
// matching, and they live together so tuning is one edit against one corpus
// (TestTitleSimilarityCorpus) rather than a scatter of literals.
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
	// score it identical to every other title ending in "Peace".
	titleInformativeMinCount = 2
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
	Year  string  `json:"year,omitempty"`
}

// rankNearTitleMatches scores candidates against the query title and returns
// the best ones, strongest first.
//
// Ordering is (score desc, key asc). The key tiebreak is what makes two runs
// over the same library print the same list: candidate order arrives from bm25,
// which can reorder equally-scored rows, and an unstable list cannot be
// reproduced from a bug report.
func rankNearTitleMatches(query string, candidates []titleCandidate, limit int) []nearTitleMatch {
	normalizedQuery := titleMatchTokens(query)
	if len(normalizedQuery) == 0 {
		return nil
	}
	if limit <= 0 {
		limit = nearTitleMatchLimit
	}

	scored := make([]nearTitleMatch, 0, len(candidates))
	for _, candidate := range candidates {
		score := titleTokenSimilarity(normalizedQuery, titleMatchTokens(candidate.Title))
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

// titleMatchTokens lowercases a title, splits it on everything that is not a
// letter or a digit, and keeps the words worth scoring on. Splitting that way
// means punctuation, dashes and quoting styles cannot change a score. It is
// deliberately more aggressive than normalizeDuplicateTitle, which backs an
// EXACT equality contract and must not fold two distinct titles together; here
// folding is the point.
//
// Function words are dropped once the title has titleInformativeMinCount real
// words left. Counting them was measurably wrong, not merely noisy: "The Study
// of Networks" and "The Theory of Games" share "the" and "of" and scored 0.50
// with nothing else in common. Titles too short to spare them keep everything,
// because a title whose only long word is "Peace" has no other signal.
func titleMatchTokens(title string) []string {
	fields := strings.FieldsFunc(strings.ToLower(title), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	informative := make([]string, 0, len(fields))
	for _, field := range fields {
		if len([]rune(field)) >= titleInformativeMinLen {
			informative = append(informative, field)
		}
	}
	if len(informative) >= titleInformativeMinCount {
		return informative
	}
	return fields
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
func titleTokenSimilarity(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	used := make([]bool, len(b))
	shared := 0
	for _, left := range a {
		best := -1
		bestDistance := 0
		for i, right := range b {
			if used[i] {
				continue
			}
			distance, ok := fuzzyTokenDistance(left, right)
			if !ok {
				continue
			}
			if best < 0 || distance < bestDistance {
				best, bestDistance = i, distance
				if distance == 0 {
					break
				}
			}
		}
		if best >= 0 {
			used[best] = true
			shared++
		}
	}
	return 2 * float64(shared) / float64(len(a)+len(b))
}

// fuzzyTokenDistance reports whether two tokens are close enough to be the same
// word, and how close. The edit budget grows with length because one edit in a
// three-letter word changes which word it is, while one edit in a ten-letter
// word is a typo.
func fuzzyTokenDistance(a, b string) (int, bool) {
	if a == b {
		return 0, true
	}
	ra, rb := []rune(a), []rune(b)
	budget := 0
	if shorter := min(len(ra), len(rb)); shorter >= fuzzyTokenTwoEditLen {
		budget = 2
	} else if shorter >= fuzzyTokenMinLen {
		budget = 1
	}
	if budget == 0 {
		return 0, false
	}
	if abs(len(ra)-len(rb)) > budget {
		return 0, false
	}
	distance := boundedLevenshtein(ra, rb, budget)
	if distance > budget {
		return 0, false
	}
	return distance, true
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
