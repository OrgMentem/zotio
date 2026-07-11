// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"strings"
	"testing"
	"time"
)

func wrappedRow(day string, pubYear int, tags ...string) wrappedItemRow {
	t, err := time.Parse("2006-01-02", day)
	if err != nil {
		panic(err)
	}
	return wrappedItemRow{Key: "K" + day, Title: "Item " + day, AddedAt: t, PubYear: pubYear, Tags: tags}
}

func TestComputeWrappedHighlights(t *testing.T) {
	rows := []wrappedItemRow{
		// Three-day streak Jul 4-6; Jul 4 is busiest (2 items); two Fridays
		wrappedRow("2026-07-03", 2026, "ml"),         // Fri
		wrappedRow("2026-07-04", 1997, "ml", "lstm"), // Sat
		wrappedRow("2026-07-04", 2020),               // Sat (busiest day)
		wrappedRow("2026-07-05", 2025, "ml"),         // Sun
		wrappedRow("2026-07-06", 0),                  // Mon (no pub year)
		wrappedRow("2026-07-10", 2026),               // Fri
	}
	h := computeWrappedHighlights(rows, 2026)

	if h.BusiestDay == nil || h.BusiestDay.Count != 2 || !strings.Contains(h.BusiestDay.Date, "Jul 4") {
		t.Errorf("busiest day = %+v, want Jul 4 with 2", h.BusiestDay)
	}
	if h.TopWeekday == nil || h.TopWeekday.Name != "Friday" || h.TopWeekday.Count != 2 {
		t.Errorf("top weekday = %+v, want Friday 2", h.TopWeekday)
	}
	if h.LongestStreak == nil || h.LongestStreak.Days != 4 {
		t.Errorf("streak = %+v, want 4 days (Jul 3-6)", h.LongestStreak)
	}
	if h.DeepCut == nil || h.DeepCut.Year != 1997 {
		t.Errorf("deep cut = %+v, want 1997", h.DeepCut)
	}
	if h.SameYearCount != 2 {
		t.Errorf("same-year count = %d, want 2", h.SameYearCount)
	}
	if h.TopTag == nil || h.TopTag.Name != "ml" || h.TopTag.Count != 3 {
		t.Errorf("top tag = %+v, want ml 3", h.TopTag)
	}
}

func TestComputeWrappedHighlightsSuppressesWeakSignals(t *testing.T) {
	// One item: no busiest day, no weekday habit, no streak, no deep cut
	// (recent pub), nothing implied that the data cannot support.
	h := computeWrappedHighlights([]wrappedItemRow{wrappedRow("2026-03-01", 2024)}, 2026)
	if h.BusiestDay != nil || h.TopWeekday != nil || h.LongestStreak != nil || h.DeepCut != nil {
		t.Errorf("weak signals should be suppressed, got %+v", h)
	}
}

func TestExtractPublicationYear(t *testing.T) {
	tests := map[string]int{
		"1997-11":         1997,
		"November 1997":   1997,
		"2020/06/17":      2020,
		"":                0,
		"n.d.":            0,
		"circa 1850":      1850,
		"12 January 2026": 2026,
	}
	for in, want := range tests {
		if got := extractPublicationYear(in); got != want {
			t.Errorf("extractPublicationYear(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestStackedRatioBar(t *testing.T) {
	rows := []libraryWrappedRankedCount{
		{Name: "journalArticle", Count: 19},
		{Name: "book", Count: 2},
		{Name: "report", Count: 1},
	}
	bar, legend := stackedRatioBar(rows, 36)
	// Colors are off in tests (no TTY): the bar must still total exactly 36
	// cells and the legend must name every type with its count.
	if got := strings.Count(bar, "▆"); got != 36 {
		t.Errorf("bar has %d cells, want 36: %q", got, bar)
	}
	for _, want := range []string{"journalArticle 19", "book 2", "report 1"} {
		if !strings.Contains(legend, want) {
			t.Errorf("legend missing %q: %q", want, legend)
		}
	}
}

func TestCoverageBar(t *testing.T) {
	for pct, wantFilled := range map[int]int{0: 0, 34: 8, 50: 12, 100: 24} {
		bar := coverageBar(pct)
		if got := strings.Count(bar, "▆"); got != wantFilled {
			t.Errorf("coverageBar(%d) filled = %d, want %d", pct, got, wantFilled)
		}
		if got := strings.Count(bar, "░"); got != 24-wantFilled {
			t.Errorf("coverageBar(%d) empty = %d, want %d", pct, got, 24-wantFilled)
		}
	}
}
