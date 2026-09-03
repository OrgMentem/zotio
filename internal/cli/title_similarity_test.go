// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"math"
	"math/rand/v2"
	"strings"
	"testing"
)

// titleCorpusCase is one labelled pair. Want records whether the pair must
// REACH a reader, which is not the same question as "is this the same work":
// the 2018 edition of a report is a different work and still has to be
// reported when the query names the 2019 one. The scorer never decides
// sameness, so the assertion is about ordering pressure only — a pair that
// must be seen outranks the junk floor, and a pair that must not falls under
// it. What a score may MEAN once it is reported is pinned in
// titleScoreBounds; what the scorer still gets wrong is recorded in
// titleKnownGaps.
type titleCorpusCase struct {
	name  string
	query string
	other string
	want  bool
}

// titleCorpus is the tuning scoreboard. Change a constant in
// title_similarity.go and this test tells you what the change cost. Add a case
// whenever a real library turns up a pair the scorer handles correctly; never
// delete a case to make a change pass. A pair the scorer gets WRONG cannot
// live here, because every case asserts the current result — put it in
// titleKnownGaps, which is built to hold exactly those.
var titleCorpus = []titleCorpusCase{
	// Identical and trivially different.
	{"identical", "Attention Is All You Need", "Attention Is All You Need", true},
	{"case and punctuation", "Attention Is All You Need", "ATTENTION IS ALL YOU NEED.", true},
	{"unicode dash", "Sequence-to-Sequence Learning", "Sequence\u2013to\u2013Sequence Learning", true},

	// Typos: the failure the feature exists for.
	{"single typo", "Attention Is All You Need", "Attention Is All You Nead", true},
	{"transposition", "Deep Residual Learning", "Deep Residaul Learning", true},
	{"single insertion in long word", "Convolutional Architectures", "Convolutionnal Architectures", true},
	{"two edits in long word", "Convolutional Architectures", "Convulutionnal Architectures", true},

	// Structural difference, same work.
	{"added subtitle", "Attention Is All You Need", "Attention Is All You Need: A Transformer Study", true},
	{"added edition subtitle to short title", "The Art of War: Complete Edition", "The Art of War", true},
	{"long subtitle", "Deep Learning", "Deep Learning: Foundations and Applications for Practitioners", true},
	{"dropped subtitle", "BERT: Pre-training of Deep Bidirectional Transformers", "BERT", false},
	{"spelling variant", "Modelling Behaviour in Networks", "Modeling Behavior in Networks", true},
	{"reordered words", "Learning Deep Representations", "Deep Representations Learning", true},

	// Neighbouring editions: a different work that still has to be offered,
	// because the library holds the edition the reader did not ask for.
	{"different year in a report series", "World Development Report 2019", "World Development Report 2018", true},
	{"different volume in a series", "Volume 12 Studies", "Volume 13 Studies", true},

	// Generic titles: the trap. These must not pair with each other.
	{"generic identical", "Introduction", "Introduction", true},
	{"generic different", "Introduction", "Conclusion", false},
	{"short stopword overlap", "The Study of Networks", "The Theory of Games", false},

	// Genuinely unrelated.
	{"unrelated", "Attention Is All You Need", "Random Forests", false},
	// One side has a single informative word, so the joint switch keeps the
	// function words for BOTH sides. This is the pair that says the fallback
	// cannot be carried by a shared "of".
	{"function words shared under the joint fallback", "Of Mice and Men", "A Survey of Reinforcement Learning", false},
	{"same field different work", "Deep Residual Learning for Image Recognition", "Batch Normalization for Image Recognition", false},
}

// titleScoreBoundCase pins an exact score rather than a side of the floor.
// The boolean corpus cannot defend a constant on its own: broad mutations of
// fuzzyTokenMinLen and fuzzyTokenTwoEditLen left every corpus case green,
// because a score can move a long way without crossing the floor. Each case
// below names the constant or rule it holds down.
type titleScoreBoundCase struct {
	name  string
	query string
	other string
	want  float64
}

var titleScoreBounds = []titleScoreBoundCase{
	// The joint informative-token switch. Deciding it per title scored the
	// first pair 0.0000 and dropped the right candidate; deciding it with OR
	// instead of AND would score the second pair 0.5000 and report two
	// unrelated titles.
	{"short title against an added subtitle", "The Art of War: Complete Edition", "The Art of War", 8.0 / 10.0},
	{"stopword overlap stays at zero", "The Study of Networks", "The Theory of Games", 0},

	// Numerals match exactly or not at all.
	{"different year", "World Development Report 2019", "World Development Report 2018", 6.0 / 8.0},
	{"different volume", "Volume 12 Studies", "Volume 13 Studies", 4.0 / 6.0},
	{"equal numeral still pairs", "Report 2019 Findings", "Report 2019 Findigs", titleDistinctMaxScore},

	// Unicode: NFD tokenises like NFC, so the ranker keeps a row the FTS
	// query returned. This pair is equal under normalizeExactTitle, so it is
	// also the case that proves the ceiling does not cap a real identity.
	{"nfd equals nfc", "Nação Brasileira", "Nac\u0327a\u0303o Brasileira", 1},

	// Maximum-cardinality pairing. Greedy first-best scored this 0.5000 one
	// way and 1.0000 reversed.
	{"inflection cluster pairs optimally", "Word Words", "Words World", titleDistinctMaxScore},

	// The edit budget, at both ends of both constants.
	{"four rune word absorbs one edit", "Rome", "Home", titleDistinctMaxScore},
	{"three rune word absorbs nothing", "War", "Wax", 0},
	{"two edits in a thirteen rune word", "Convolutional Architectures", "Convulutionnal Architectures", titleDistinctMaxScore},
	{"three edits in a thirteen rune word", "Convolutional Architectures", "Convulutionnall Architectures", 2.0 / 4.0},
	{"two edits in a seven rune word", "Network Studies", "Netwerks Studies", 2.0 / 4.0},

	// The ceiling: a typo is a near match, never an identity.
	{"one token typo cannot claim identity", "Introduction", "Introducton", titleDistinctMaxScore},

	// The documented subtitle penalty, quoted in titleTokenSimilarity.
	{"added subtitle costs proportionally", "Attention Is All You Need", "Attention Is All You Need: A Transformer Study", 4.0 / 6.0},
}

// titleGapCase is a pair the scorer gets WRONG, recorded with the score it
// actually produces today. titleCorpus cannot hold these: every corpus case
// asserts the current result, so a known failure written there would either be
// red or be quietly relabelled as correct. Asserting the wrong answer keeps the
// case visible and makes the fix loud — close a gap and this test fails, which
// is the prompt to move the case into titleCorpus and delete it from here.
type titleGapCase struct {
	name  string
	query string
	other string
	score float64
	why   string
}

var titleKnownGaps = []titleGapCase{
	{
		name: "unaccented query", query: "Theatre", other: "Théâtre", score: 0,
		why: "diacritics are deliberately not folded: candidate generation is FTS over the stored letters, so this row never arrives to be scored, and folding would merge words a diacritic separates",
	},
	{
		name: "one character change in a cjk title", query: "机器学习导论", other: "机器学习概论", score: titleDistinctMaxScore,
		why: "a CJK title has no spaces, so it is one token inside the edit budget; unreachable in practice because the FTS candidate query returns no row for the changed form",
	},
	{
		name: "two word titles sharing one word", query: "Machine Learning", other: "Machine Vision", score: 2.0 / 4.0,
		why: "lands exactly on nearTitleMinScore, and the floor is inclusive, so any two two-word titles sharing one word are reported; tightening the floor to strictly-above would also drop pairs a reader wants",
	},
	{
		name: "three letter words sharing one word", query: "Big Sur", other: "Big Sky", score: 2.0 / 4.0,
		why: "neither title has an informative word, so the joint switch keeps everything and 'big' alone is half the score",
	},
	{
		name: "short titles differing in one short word", query: "The Art of War", other: "The Art of Zen", score: 6.0 / 8.0,
		why: "no informative word on either side, so the function words are all the signal there is and three of four words match",
	},
	{
		name: "very long subtitle", query: "Deep Learning", other: "Deep Learning: A Practical Introduction to Modern Neural Network Architectures", score: 4.0 / 10.0,
		why: "the proportional subtitle penalty pushes a possible same-work pair under the floor; the same penalty is what keeps 'same field different work' out, so it cannot simply be loosened",
	},
}

// titleScore is the whole pipeline a caller sees, minus the rounding that
// rankNearTitleMatches applies.
func titleScore(query, other string) float64 {
	return titleTokenSimilarity(titleMatchTokens(query), titleMatchTokens(other))
}

func TestTitleSimilarityCorpus(t *testing.T) {
	for _, tc := range titleCorpus {
		t.Run(tc.name, func(t *testing.T) {
			score := titleScore(tc.query, tc.other)
			if got := score >= nearTitleMinScore; got != tc.want {
				t.Fatalf("score(%q, %q) = %.4f; reportable = %v, want %v (floor %.2f)",
					tc.query, tc.other, score, got, tc.want, nearTitleMinScore)
			}
		})
	}
}

func TestTitleSimilarityScoreBounds(t *testing.T) {
	for _, tc := range titleScoreBounds {
		t.Run(tc.name, func(t *testing.T) {
			if score := titleScore(tc.query, tc.other); math.Abs(score-tc.want) > 1e-9 {
				t.Fatalf("score(%q, %q) = %.4f, want %.4f", tc.query, tc.other, score, tc.want)
			}
		})
	}
}

func TestTitleSimilarityKnownGaps(t *testing.T) {
	for _, tc := range titleKnownGaps {
		t.Run(tc.name, func(t *testing.T) {
			score := titleScore(tc.query, tc.other)
			if math.Abs(score-tc.score) > 1e-9 {
				t.Fatalf("known gap score(%q, %q) = %.4f, recorded as %.4f.\nThe gap was: %s\nIf you fixed it, move the case into titleCorpus and delete it here.",
					tc.query, tc.other, score, tc.score, tc.why)
			}
		})
	}
}

// Symmetry is a real invariant, not a property of one table: countTitleWordPairs
// returns a maximum-cardinality pairing, and maximum cardinality belongs to the
// pair rather than to the argument order. Greedy first-best pairing broke it —
// "Word Words" against "Words World" scored 0.5000 forward and 1.0000 reversed,
// so which title was the query changed the ranking.
func TestTitleSimilarityIsSymmetricAndBounded(t *testing.T) {
	pairs := make([][2]string, 0, len(titleCorpus)+len(titleScoreBounds)+len(titleKnownGaps)+1)
	for _, tc := range titleCorpus {
		pairs = append(pairs, [2]string{tc.query, tc.other})
	}
	for _, tc := range titleScoreBounds {
		pairs = append(pairs, [2]string{tc.query, tc.other})
	}
	for _, tc := range titleKnownGaps {
		pairs = append(pairs, [2]string{tc.query, tc.other})
	}
	pairs = append(pairs, [2]string{"Word Words", "Words World"})

	// Inflections and spelling neighbours in one title are what made greedy
	// pairing order-dependent, so the sweep is drawn from exactly those
	// clusters, plus the numerals and function words that switch code paths.
	vocabulary := []string{
		"word", "words", "world", "worlds", "form", "forms", "norm", "norms",
		"data", "date", "dates", "need", "nead", "learning", "learnings",
		"the", "of", "a", "2019", "2018", "12",
	}
	random := rand.New(rand.NewPCG(1, 2))
	for range 4000 {
		pairs = append(pairs, [2]string{
			randomTitle(random, vocabulary),
			randomTitle(random, vocabulary),
		})
	}

	for _, pair := range pairs {
		forward := titleScore(pair[0], pair[1])
		reverse := titleScore(pair[1], pair[0])
		if math.Abs(forward-reverse) > 1e-9 {
			t.Fatalf("score(%q,%q) = %.6f but reversed = %.6f; ranking would depend on argument order",
				pair[0], pair[1], forward, reverse)
		}
		if forward < 0 || forward > 1 {
			t.Fatalf("score(%q,%q) = %.6f, outside [0,1]", pair[0], pair[1], forward)
		}
	}
}

func randomTitle(random *rand.Rand, vocabulary []string) string {
	words := make([]string, 1+random.IntN(4))
	for i := range words {
		words[i] = vocabulary[random.IntN(len(vocabulary))]
	}
	return strings.Join(words, " ")
}

// A score of 1.00 is the one thing in this file that means "the same title".
// It has to survive every way the same title gets written, and it must not
// leak to a pair that is merely close: the near-match list is printed under a
// heading that says no exact match was found.
func TestTitleSimilarityReservesOneForTheSameTitle(t *testing.T) {
	same := [][2]string{
		{"Attention Is All You Need", "ATTENTION IS ALL YOU NEED."},
		{"Sequence-to-Sequence Learning", "Sequence\u2013to\u2013Sequence Learning"},
		{"Nação Brasileira", "Nac\u0327a\u0303o Brasileira"},
		{"The  Art   of War", "the art of war"},
	}
	for _, pair := range same {
		if score := titleScore(pair[0], pair[1]); math.Abs(score-1) > 1e-9 {
			t.Fatalf("score(%q,%q) = %.4f, want 1 (equal under normalizeExactTitle)", pair[0], pair[1], score)
		}
	}
	distinct := [][2]string{
		{"Introduction", "Introducton"},
		{"Volume 12 Studies", "Volume 13 Studies"},
		{"Learning Deep Representations", "Deep Representations Learning"},
		{"Modelling Behaviour in Networks", "Modeling Behavior in Networks"},
	}
	for _, pair := range distinct {
		score := titleScore(pair[0], pair[1])
		if score >= 1 {
			t.Fatalf("score(%q,%q) = %.4f, want below 1: the titles are not the same string under normalizeExactTitle",
				pair[0], pair[1], score)
		}
		if score > titleDistinctMaxScore {
			t.Fatalf("score(%q,%q) = %.6f, above the ceiling %.2f", pair[0], pair[1], score, titleDistinctMaxScore)
		}
	}
}

func TestNormalizeExactTitle(t *testing.T) {
	cases := map[string]string{
		"Attention Is All You Need":               "attention is all you need",
		"ATTENTION IS ALL YOU NEED.":              "attention is all you need",
		"  The   Art  of War  ":                   "the art of war",
		"Sequence\u2013to\u2013Sequence Learning": "sequence-to-sequence learning",
		"Minus\u2212Sign":                         "minus-sign",
		"\u201cQuoted\u201d Title":                `"quoted" title`,
		"Don\u2019t Look Back":                    "don't look back",
		"Nac\u0327a\u0303o Brasileira":            "nação brasileira",
		"Wait\u2026 What?":                        "wait... what",
		"Soft\u00adhyphen":                        "softhyphen",
		"Zero\u200bwidth":                         "zerowidth",
		"Vol. 2:":                                 "vol. 2",
		"What Is Life?":                           "what is life",
		"   ...   ":                               "",
		"":                                        "",
		"Non\u00a0breaking\u00a0space":            "non breaking space",
		"A Study of the Art of War and Peace of Kings": "a study of the art of war and peace of kings",
	}
	for title, want := range cases {
		if got := normalizeExactTitle(title); got != want {
			t.Fatalf("normalizeExactTitle(%q) = %q, want %q", title, got, want)
		}
	}
	// It folds how a title was written, never what it says: every word
	// survives, so two different titles cannot collapse into one.
	const wordy = "A Study of the Art of War and Peace of Kings"
	if got, want := len(strings.Fields(normalizeExactTitle(wordy))), len(strings.Fields(wordy)); got != want {
		t.Fatalf("normalizeExactTitle dropped words: %d of %d left", got, want)
	}
}

// A repeated word must not pair twice. Without one-to-one pairing, a title that
// simply repeats a query word would score as though it contained the whole
// query.
func TestTitleSimilarityPairsEachTokenOnce(t *testing.T) {
	score := titleScore("Networks Networks Networks", "Networks")
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

// The reported score is advisory, so it is published at the precision it has.
// JSON used to carry 0.6666666666666666, which invites an agent to compare it
// against a threshold like 0.66 and act on digits the score does not own.
func TestRankNearTitleMatchesRoundsScoreToTwoDecimals(t *testing.T) {
	got := rankNearTitleMatches(
		"Attention Is All You Need",
		[]titleCandidate{{Key: "MMMM", Title: "Attention Is All You Need: A Transformer Study"}},
		nearTitleMatchLimit,
	)
	if len(got) != 1 {
		t.Fatalf("matches = %+v, want 1", got)
	}
	if got[0].Score != 0.67 {
		t.Fatalf("score = %.17g, want exactly 0.67", got[0].Score)
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

// A numeral is the one token where a single changed character means a different
// work, so it carries no edit budget at all.
func TestTitleWordsMatchRequiresExactNumerals(t *testing.T) {
	if titleWordsMatch("2019", "2018") {
		t.Fatal(`titleWordsMatch("2019","2018") = true; a year must match exactly`)
	}
	if titleWordsMatch("12", "13") {
		t.Fatal(`titleWordsMatch("12","13") = true; a volume number must match exactly`)
	}
	if !titleWordsMatch("2019", "2019") {
		t.Fatal(`titleWordsMatch("2019","2019") = false; the same year must still pair`)
	}
	if titleWordsMatch("2019", "2019a") {
		t.Fatal(`titleWordsMatch("2019","2019a") = true; a numeral must not pair with a mixed token`)
	}
	if !titleWordsMatch("networks", "netwerks") {
		t.Fatal(`titleWordsMatch("networks","netwerks") = false; the numeral rule must not touch words`)
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
