// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// The corpus for approximate citekey matching. A citekey is an identifier
// typed by hand out of a manuscript, so the cases are the ways a hand
// mistypes one: a wrong letter, two letters swapped, a swapped year, a
// disambiguation suffix, a dropped separator — against the keys that merely
// look alike and must never be offered.

package cli

import (
	"math"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
)

type citeKeyCorpusCase struct {
	name  string
	query string
	other string
	// want is whether the pair is reportable at all. There is no similarity
	// floor to compare against: the admission bound inside citeKeySimilarity
	// is the only gate, and a rejected pair scores exactly zero.
	want bool
}

var citeKeyCorpus = []citeKeyCorpusCase{
	// The same key, written differently.
	{"identical", "smith2023", "smith2023", true},
	{"case only", "Smith2023", "smith2023", true},
	{"surrounding whitespace", " smith2023 ", "smith2023", true},

	// Typos: the failure this exists for.
	{"one wrong letter", "smyth2023", "smith2023", true},
	{"letter transposition", "smtih2023", "smith2023", true},
	{"transposed year", "smith2032", "smith2023", true},
	{"one wrong digit", "smith2024", "smith2023", true},
	{"truncated year", "smith202", "smith2023", true},
	{"doubled digit", "smith20233", "smith2023", true},
	{"typo in a long key", "deng2009imagnet", "deng2009imagenet", true},

	// Structural difference, same author and year.
	{"disambiguation suffix", "smith2023a", "smith2023", true},
	{"dropped separators", "smith2023attention", "Smith_2023_Attention", true},

	// The trap: keys that share their shape and nothing else. A citekey is
	// mostly author plus year, so an unrelated key from the same year differs
	// only in the author part, and that is a different source.
	{"unrelated author same year", "jones2023", "smith2023", false},
	{"unrelated author and year", "einstein1905", "smith2023", false},
	{"unrelated long keys", "vaswani2017", "kingma2015", false},

	// Short keys, where one edit is most of the string and changes which key
	// is meant. Only a fold can report them.
	{"short key one edit", "abc", "abd", false},
	{"short key case only", "abc", "ABC", true},
}

func TestCiteKeySimilarityCorpus(t *testing.T) {
	for _, tc := range citeKeyCorpus {
		t.Run(tc.name, func(t *testing.T) {
			score := citeKeySimilarity(tc.query, tc.other)
			if got := score > 0; got != tc.want {
				t.Fatalf("citeKeySimilarity(%q, %q) = %.4f; reportable = %v, want %v",
					tc.query, tc.other, score, got, tc.want)
			}
		})
	}
}

// The exact numbers the ranking is built on. They are pinned because the
// ORDER between them is the design decision: a citekey is author plus year,
// and the year is the part where one changed character means a different
// work, so a digit edit costs double a letter edit.
func TestCiteKeySimilarityScoreBounds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
		other string
		want  float64
	}{
		{"identical", "smith2023", "smith2023", 1},
		{"case only folds equal", "SMITH2023", "smith2023", 1},
		{"one wrong letter", "smyth2023", "smith2023", 1 - 1.0/9},
		{"one wrong digit", "smith2024", "smith2023", 1 - 2.0/9},
		{"transposed year", "smith2032", "smith2023", 1 - 4.0/9},
		{"disambiguation suffix", "smith2023a", "smith2023", 1 - 1.0/9},
		// An all-digit key at the minimum length is the worst pair the bound
		// admits: every edit in it is a digit edit, priced double, over the
		// fewest runes that may match approximately at all. It lands exactly
		// on the 0.5 floor the whole model promises, so this row is where a
		// change to citeKeyDigitEditCost or citeKeyFuzzyMinLen shows up as a
		// broken promise rather than as a slightly different number.
		{"all-digit key at the minimum length", "2023", "2024", 0.5},
		{"rejected", "jones2023", "smith2023", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if score := citeKeySimilarity(tc.query, tc.other); math.Abs(score-tc.want) > 1e-9 {
				t.Fatalf("citeKeySimilarity(%q, %q) = %.4f, want %.4f", tc.query, tc.other, score, tc.want)
			}
		})
	}
}

// The digit decision, asserted as an ordering rather than as two numbers: a
// wrong year must rank below a wrong letter, and a transposed year below
// both. With one cost for every rune all three collapse to the same score and
// the list stops telling the reader which suggestion is the safer one.
func TestCiteKeyDigitEditCostsMoreThanALetterEdit(t *testing.T) {
	letter := citeKeySimilarity("smith2023", "smyth2023")
	digit := citeKeySimilarity("smith2023", "smith2024")
	transposedYear := citeKeySimilarity("smith2023", "smith2032")
	if !(letter > digit && digit > transposedYear) {
		t.Fatalf("letter typo %.4f, wrong year %.4f, transposed year %.4f: want strictly decreasing, so a differing year ranks below a differing letter",
			letter, digit, transposedYear)
	}
	if transposedYear <= 0 {
		t.Fatalf("transposed year scored %.4f: pricing digits must not exclude the commonest real break", transposedYear)
	}
}

// The bound scales with length, which is the whole reason it is a bound and
// not a constant: two edits in a four-rune key is a different key, while
// three edits in a sixteen-rune key is a hand that slipped.
func TestCiteKeyEditBudgetScalesWithLength(t *testing.T) {
	if got := citeKeySimilarity("abcd", "abzd"); got <= 0 {
		t.Fatalf("one edit in a four-rune key scored %.4f, want it reported", got)
	}
	if got := citeKeySimilarity("abcd", "abzz"); got != 0 {
		t.Fatalf("two edits in a four-rune key scored %.4f, want 0: that is a different key", got)
	}
	long := "abcdefghijklmnop"
	if got := citeKeySimilarity(long, "abzdefghijzlmnzp"); got <= 0 {
		t.Fatalf("three edits in a %d-rune key scored %.4f, want it reported: a long key tolerates more than a short one",
			len(long), got)
	}
	if got := citeKeySimilarity(long, "abzdefghzjzlmnzp"); got != 0 {
		t.Fatalf("four edits in a %d-rune key scored %.4f, want 0", len(long), got)
	}
}

// 1.00 has to mean "the same key, written differently", because these rows are
// printed under a heading that says the keys DIFFER. A long key one letter
// off is the pair that reaches the ceiling arithmetically.
func TestCiteKeySimilarityReservesOneForAFoldedEqualKey(t *testing.T) {
	long := strings.Repeat("ab", 60)
	nearlyLong := "z" + long[1:]
	score := citeKeySimilarity(long, nearlyLong)
	if math.Abs(score-citeKeyDistinctMaxScore) > 1e-9 {
		t.Fatalf("citeKeySimilarity over a %d-rune key with one wrong letter = %.6f, want the ceiling %.2f",
			len(long), score, citeKeyDistinctMaxScore)
	}
	if got := citeKeySimilarity(long, strings.ToUpper(long)); got != 1 {
		t.Fatalf("a key differing only in case scored %.4f, want exactly 1", got)
	}
}

// The other half of the 1.00 promise, and the only one nothing pinned: the
// comment on normalizeMatchCiteKey says a folded-equal pair may differ "in
// case or in unicode encoding", and a key with an accented author name is
// stored either way depending on which client wrote it. A composed key and
// its decomposed spelling are the same key, and a reader told they differ
// would go looking for a second entry that does not exist.
func TestCiteKeySimilarityFoldsAUnicodeEncodingDifference(t *testing.T) {
	composed := "müller2023"         // NFC: U+00FC
	decomposed := "mu\u0308ller2023" // NFD: u + combining diaeresis
	if composed == decomposed {
		t.Fatal("the fixture is not two spellings; the test would prove nothing")
	}
	if got := citeKeySimilarity(composed, decomposed); got != 1 {
		t.Fatalf("citeKeySimilarity over one key in two encodings = %.4f, want exactly 1", got)
	}
	// And the accent itself is still a difference, not noise the fold eats:
	// "muller2023" is a key someone else may hold.
	if got := citeKeySimilarity(composed, "muller2023"); got >= 1 {
		t.Fatalf("dropping the diaeresis scored %.4f, want a near key below 1", got)
	}
}

// Two invariants over the whole input space rather than over one table.
// Symmetry, because which key is the query must not change a ranking. And a
// non-zero score of at least 0.5, which is what makes the admission bound the
// only gate: a caller needs no similarity floor of its own, and no reported
// row can be arithmetic noise.
func TestCiteKeySimilarityIsSymmetricAndNeverWeak(t *testing.T) {
	alphabet := []rune("abcdefgz0123456789_")
	random := rand.New(rand.NewPCG(7, 11))
	makeKey := func() string {
		out := make([]rune, 1+random.IntN(14))
		for i := range out {
			out[i] = alphabet[random.IntN(len(alphabet))]
		}
		return string(out)
	}
	pairs := make([][2]string, 0, len(citeKeyCorpus)+20000)
	for _, tc := range citeKeyCorpus {
		pairs = append(pairs, [2]string{tc.query, tc.other})
	}
	for range 20000 {
		pairs = append(pairs, [2]string{makeKey(), makeKey()})
	}
	for _, pair := range pairs {
		forward := citeKeySimilarity(pair[0], pair[1])
		reverse := citeKeySimilarity(pair[1], pair[0])
		if math.Abs(forward-reverse) > 1e-9 {
			t.Fatalf("citeKeySimilarity(%q,%q) = %.6f but reversed = %.6f; ranking would depend on argument order",
				pair[0], pair[1], forward, reverse)
		}
		if forward < 0 || forward > 1 {
			t.Fatalf("citeKeySimilarity(%q,%q) = %.6f, outside [0,1]", pair[0], pair[1], forward)
		}
		if forward > 0 && forward < 0.5 {
			t.Fatalf("citeKeySimilarity(%q,%q) = %.6f: the bound admitted a pair too weak to show, so the gate no longer implies a usable score",
				pair[0], pair[1], forward)
		}
	}
}

// The fixture puts the two equally-scored suffix keys in REVERSE key order,
// and gives them item keys that disagree with key order too. Both are
// deliberate. sort.Slice over five rows runs an insertion sort, which happens
// to preserve input order, so a fixture already sorted by key proved nothing:
// deleting the key comparison left the test passing. Item keys sorted the
// same way hid it just as well, since the later tiebreak then produced the
// same list. Only the key tiebreak orders this fixture.
func TestRankNearCiteKeysRanksCapsAndRounds(t *testing.T) {
	candidates := []citeKeyCandidate{
		{CiteKey: "smith2024", ItemKey: "Y2024", Title: "Attention, Later"},
		{CiteKey: "jones2023", ItemKey: "JONE", Title: "Unrelated Work"},
		{CiteKey: "smith2023b", ItemKey: "AB3N9P5B", Title: "Attention, Once More"},
		{CiteKey: "smith2023", ItemKey: "SMIT", Title: "Attention Is All You Need"},
		{CiteKey: "smith2023a", ItemKey: "ZQ7K2M4A", Title: "Attention, Again"},
	}
	ranked := rankNearCiteKeys("smith2023", candidates, nearCiteKeyMatchLimit)
	if len(ranked) != nearCiteKeyMatchLimit {
		t.Fatalf("ranked %d rows, want the rank cap %d: %+v", len(ranked), nearCiteKeyMatchLimit, ranked)
	}
	if ranked[0].CiteKey != "smith2023" || ranked[0].Score != 1 {
		t.Fatalf("top row = %+v, want the exact key at 1.00", ranked[0])
	}
	// Equal scores tiebreak on the key, so two runs over one library print
	// one list; without it the map order of the candidate walk leaks out.
	if ranked[1].CiteKey != "smith2023a" || ranked[2].CiteKey != "smith2023b" {
		t.Fatalf("rows 2 and 3 = %q, %q; want the two equally-scored suffixes in key order",
			ranked[1].CiteKey, ranked[2].CiteKey)
	}
	if ranked[1].ItemKey != "ZQ7K2M4A" || ranked[1].Title != "Attention, Again" {
		t.Fatalf("row = %+v, want the item key and title a reader confirms the row with", ranked[1])
	}
	for _, row := range ranked {
		if row.CiteKey == "jones2023" {
			t.Fatalf("an unrelated key was ranked: %+v", ranked)
		}
		if rounded := math.Round(row.Score*nearCiteKeyScoreScale) / nearCiteKeyScoreScale; row.Score != rounded {
			t.Fatalf("score %v is published unrounded; an advisory number must not claim sixteen digits", row.Score)
		}
	}
}

// Two items holding ONE citekey is the conflict `items citekey-conflicts`
// exists for, and it is the case where this ranker had no answer: the rows
// score identically, tie on the key, and citekeyAuditQuery has no ORDER BY,
// so which item and title were shown moved with row order — and three
// duplicate rows filled a cap of three, hiding every other candidate behind
// one key repeated. A suggestion list answers "which key should I have
// typed", so the key is listed once, and the item tiebreak decides which
// row's title travels with it.
func TestRankNearCiteKeysListsADuplicateKeyOnceAndCapsByDistinctKey(t *testing.T) {
	// Three items share smith2023, deliberately not in item-key order, and
	// two further keys are close enough to be admitted behind them.
	candidates := []citeKeyCandidate{
		{CiteKey: "smith2023", ItemKey: "DUP3", Title: "Third Copy"},
		{CiteKey: "smith2024", ItemKey: "Y2024", Title: "Attention, Later"},
		{CiteKey: "smith2023", ItemKey: "DUP1", Title: "First Copy"},
		{CiteKey: "smith2023a", ItemKey: "SMITA", Title: "Attention, Again"},
		{CiteKey: "smith2023", ItemKey: "DUP2", Title: "Second Copy"},
	}
	ranked := rankNearCiteKeys("Smith2023", candidates, nearCiteKeyMatchLimit)

	gotKeys := make([]string, 0, len(ranked))
	for _, row := range ranked {
		gotKeys = append(gotKeys, row.CiteKey)
	}
	wantKeys := []string{"smith2023", "smith2023a", "smith2024"}
	if !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Fatalf("ranked keys = %#v, want %#v: the cap counts distinct keys, so one key held by three items must not fill it",
			gotKeys, wantKeys)
	}
	if ranked[0].ItemKey != "DUP1" || ranked[0].Title != "First Copy" {
		t.Fatalf("duplicate-key row = %+v, want the lowest item key so the row is the same on every run", ranked[0])
	}

	// The same library read in another row order prints the same list. This
	// is the property the store cannot supply: the query has no ORDER BY.
	reversed := make([]citeKeyCandidate, 0, len(candidates))
	for i := len(candidates) - 1; i >= 0; i-- {
		reversed = append(reversed, candidates[i])
	}
	if again := rankNearCiteKeys("Smith2023", reversed, nearCiteKeyMatchLimit); !reflect.DeepEqual(again, ranked) {
		t.Fatalf("reversed candidate order ranked %+v, want the same list as %+v", again, ranked)
	}
}

func TestRankNearCiteKeysRefusesAnEmptyQuery(t *testing.T) {
	candidates := []citeKeyCandidate{{CiteKey: "smith2023", ItemKey: "SMIT"}}
	if got := rankNearCiteKeys("   ", candidates, nearCiteKeyMatchLimit); got != nil {
		t.Fatalf("rankNearCiteKeys with a blank query = %+v, want nil", got)
	}
}
