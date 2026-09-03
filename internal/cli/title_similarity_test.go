// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"math"
	"testing"
)

// titleCorpusCase is one labelled pair. Want records whether a HUMAN would call
// these the same work. The scorer never decides that, so the assertion is about
// ordering pressure, not about a verdict: a should-match pair must outrank the
// junk floor, and a should-not pair must fall under it.
type titleCorpusCase struct {
	name  string
	query string
	other string
	want  bool
}

// titleCorpus is the tuning scoreboard. Change a constant in
// title_similarity.go and this test tells you what the change cost. Add a case
// whenever a real library turns up a pair the scorer gets wrong; never delete a
// case to make a change pass.
var titleCorpus = []titleCorpusCase{
	// Identical and trivially different.
	{"identical", "Attention Is All You Need", "Attention Is All You Need", true},
	{"case and punctuation", "Attention Is All You Need", "ATTENTION IS ALL YOU NEED.", true},
	{"unicode dash", "Sequence-to-Sequence Learning", "Sequence\u2013to\u2013Sequence Learning", true},

	// Typos: the failure the feature exists for.
	{"single typo", "Attention Is All You Need", "Attention Is All You Nead", true},
	{"transposition", "Deep Residual Learning", "Deep Residaul Learning", true},
	{"doubled typo in long word", "Convolutional Architectures", "Convolutionnal Architectures", true},

	// Structural difference, same work.
	{"added subtitle", "Attention Is All You Need", "Attention Is All You Need: A Transformer Study", true},
	{"dropped subtitle", "BERT: Pre-training of Deep Bidirectional Transformers", "BERT", false},
	{"spelling variant", "Modelling Behaviour in Networks", "Modeling Behavior in Networks", true},
	{"reordered words", "Learning Deep Representations", "Deep Representations Learning", true},

	// Generic titles: the trap. These must not pair with each other.
	{"generic identical", "Introduction", "Introduction", true},
	{"generic different", "Introduction", "Conclusion", false},
	{"short stopword overlap", "The Study of Networks", "The Theory of Games", false},

	// Genuinely unrelated.
	{"unrelated", "Attention Is All You Need", "Random Forests", false},
	{"one shared common word", "A Survey of Reinforcement Learning", "A History of Rome", false},
	{"same field different work", "Deep Residual Learning for Image Recognition", "Batch Normalization for Image Recognition", false},
}

func TestTitleSimilarityCorpus(t *testing.T) {
	for _, tc := range titleCorpus {
		t.Run(tc.name, func(t *testing.T) {
			score := titleTokenSimilarity(titleMatchTokens(tc.query), titleMatchTokens(tc.other))
			if got := score >= nearTitleMinScore; got != tc.want {
				t.Fatalf("score(%q, %q) = %.3f; reportable = %v, want %v (floor %.2f)",
					tc.query, tc.other, score, got, tc.want, nearTitleMinScore)
			}
		})
	}
}

func TestTitleSimilarityIsSymmetricAndBounded(t *testing.T) {
	for _, tc := range titleCorpus {
		forward := titleTokenSimilarity(titleMatchTokens(tc.query), titleMatchTokens(tc.other))
		reverse := titleTokenSimilarity(titleMatchTokens(tc.other), titleMatchTokens(tc.query))
		if math.Abs(forward-reverse) > 1e-9 {
			t.Fatalf("score(%q,%q) = %.6f but reversed = %.6f; ranking would depend on argument order",
				tc.query, tc.other, forward, reverse)
		}
		if forward < 0 || forward > 1 {
			t.Fatalf("score(%q,%q) = %.6f, outside [0,1]", tc.query, tc.other, forward)
		}
	}
}

// A repeated word must not pair twice. Without one-to-one pairing, a title that
// simply repeats a query word would score as though it contained the whole
// query.
func TestTitleSimilarityPairsEachTokenOnce(t *testing.T) {
	score := titleTokenSimilarity(
		titleMatchTokens("Networks Networks Networks"),
		titleMatchTokens("Networks"),
	)
	if want := 2 * 1.0 / 4.0; math.Abs(score-want) > 1e-9 {
		t.Fatalf("repeated-token score = %.4f, want %.4f (one pairing only)", score, want)
	}
}

func TestRankNearTitleMatchesOrdersByScoreThenKey(t *testing.T) {
	candidates := []titleCandidate{
		{Key: "ZZZZ", Title: "Attention Is All You Need"},
		{Key: "AAAA", Title: "Attention Is All You Need"},
		{Key: "MMMM", Title: "Attention Is All You Need: A Transformer Study"},
		{Key: "JUNK", Title: "Random Forests"},
	}
	got := rankNearTitleMatches("Attention Is All You Need", candidates, nearTitleMatchLimit)
	if len(got) != 3 {
		t.Fatalf("matches = %+v, want 3 (junk below the floor)", got)
	}
	if got[0].Key != "AAAA" || got[1].Key != "ZZZZ" {
		t.Fatalf("tied rows = %s,%s; want AAAA,ZZZZ so equal scores order by key", got[0].Key, got[1].Key)
	}
	if got[2].Key != "MMMM" {
		t.Fatalf("third = %s, want MMMM (subtitle scores below exact)", got[2].Key)
	}
	if got[0].Score < got[2].Score {
		t.Fatalf("scores not descending: %+v", got)
	}
}

func TestRankNearTitleMatchesCapsByRank(t *testing.T) {
	candidates := make([]titleCandidate, 0, nearTitleMatchLimit*3)
	for _, key := range []string{"K01", "K02", "K03", "K04", "K05", "K06", "K07", "K08"} {
		candidates = append(candidates, titleCandidate{Key: key, Title: "Attention Is All You Need"})
	}
	got := rankNearTitleMatches("Attention Is All You Need", candidates, nearTitleMatchLimit)
	if len(got) != nearTitleMatchLimit {
		t.Fatalf("len(matches) = %d, want the rank cap %d", len(got), nearTitleMatchLimit)
	}
}

func TestRankNearTitleMatchesRejectsEmptyQuery(t *testing.T) {
	got := rankNearTitleMatches("   ...  ", []titleCandidate{{Key: "AAAA", Title: "Anything"}}, nearTitleMatchLimit)
	if got != nil {
		t.Fatalf("matches for a token-free query = %+v, want none", got)
	}
}

// The bounded distance must agree with the unbounded answer whenever the answer
// is within budget; a wrong early exit would silently drop real typo matches.
func TestBoundedLevenshteinMatchesWithinBudget(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"need", "nead", 1},
		{"residual", "residaul", 2},
		{"kitten", "sitting", 3},
		{"", "abc", 3},
		{"abc", "", 3},
		{"same", "same", 0},
	}
	for _, tc := range cases {
		got := boundedLevenshtein([]rune(tc.a), []rune(tc.b), len(tc.a)+len(tc.b))
		if got != tc.want {
			t.Fatalf("boundedLevenshtein(%q,%q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
	if got := boundedLevenshtein([]rune("kitten"), []rune("sitting"), 1); got <= 1 {
		t.Fatalf("boundedLevenshtein over budget = %d, want a value above the budget", got)
	}
}

func TestFindItemYearRequiresAPlausibleYear(t *testing.T) {
	cases := map[string]string{
		"2017":            "2017",
		"March 3, 2017":   "2017",
		"2017-03-03":      "2017",
		"n.d.":            "",
		"":                "",
		"volume 12":       "",
		"circa 1899 text": "1899",
	}
	for date, want := range cases {
		if got := findItemYear(date); got != want {
			t.Fatalf("findItemYear(%q) = %q, want %q", date, got, want)
		}
	}
}
