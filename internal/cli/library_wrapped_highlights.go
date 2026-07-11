// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// libraryWrappedHighlights carries the year-in-review superlatives. Every
// field is computed locally from synced rows; anything unavailable stays nil
// so the renderer never implies data that does not exist.
type libraryWrappedHighlights struct {
	BusiestDay    *libraryWrappedDayHighlight `json:"busiest_day,omitempty"`
	TopWeekday    *libraryWrappedRankedCount  `json:"top_weekday,omitempty"`
	LongestStreak *libraryWrappedStreak       `json:"longest_streak,omitempty"`
	DeepCut       *libraryWrappedItemPick     `json:"deep_cut,omitempty"`
	SameYearCount int                         `json:"published_same_year,omitempty"`
	MostAnnotated *libraryWrappedItemPick     `json:"most_annotated,omitempty"`
	TopTag        *libraryWrappedRankedCount  `json:"top_tag,omitempty"`
}

type libraryWrappedDayHighlight struct {
	Date  string `json:"date"` // "Mon, Jun 29"
	Count int    `json:"count"`
}

type libraryWrappedStreak struct {
	Days  int    `json:"days"`
	Start string `json:"start"` // "Jul 4"
	End   string `json:"end"`   // "Jul 8"
}

type libraryWrappedItemPick struct {
	Key   string `json:"key,omitempty"`
	Title string `json:"title"`
	Year  int    `json:"year,omitempty"`
	Count int    `json:"count,omitempty"`
}

// wrappedItemRow is one top-level item added in the wrapped year.
type wrappedItemRow struct {
	Key     string
	Title   string
	AddedAt time.Time
	PubYear int
	Tags    []string
}

func queryLibraryWrappedItemRows(db localQueryStore, year int) ([]wrappedItemRow, error) {
	rows, err := db.QueryRaw(`
SELECT id,
	COALESCE(NULLIF(TRIM(json_extract(data,'$.data.title')),''),'(untitled)') AS title,
	COALESCE(json_extract(data,'$.data.dateAdded'),'') AS date_added,
	COALESCE(json_extract(data,'$.data.date'),'') AS pub_date,
	COALESCE(json_extract(data,'$.data.tags'),'[]') AS tags
FROM resources
WHERE resource_type='items'
	AND COALESCE(NULLIF(item_type,''), json_extract(data,'$.data.itemType'), '') NOT IN ('attachment','note','annotation')
	AND SUBSTR(COALESCE(json_extract(data,'$.data.dateAdded'),''), 1, 4) = ?`, fmt.Sprintf("%04d", year))
	if err != nil {
		return nil, err
	}
	out := make([]wrappedItemRow, 0, len(rows))
	for _, row := range rows {
		added, err := parseWrappedTimestamp(sqlStringValue(row["date_added"]))
		if err != nil {
			continue
		}
		out = append(out, wrappedItemRow{
			Key:     sqlStringValue(row["id"]),
			Title:   strings.TrimSpace(sqlStringValue(row["title"])),
			AddedAt: added,
			PubYear: extractPublicationYear(sqlStringValue(row["pub_date"])),
			Tags:    parseWrappedTags(sqlStringValue(row["tags"])),
		})
	}
	return out, nil
}

func parseWrappedTimestamp(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Parse("2006-01-02", strings.TrimSpace(s)[:min(len(strings.TrimSpace(s)), 10)])
}

var wrappedYearRe = regexp.MustCompile(`\b(1[5-9]\d\d|20\d\d)\b`)

// extractPublicationYear pulls a plausible publication year out of Zotero's
// free-form date field ("1997-11", "November 1997", "1997/11/15", …).
func extractPublicationYear(s string) int {
	m := wrappedYearRe.FindString(s)
	if m == "" {
		return 0
	}
	year, err := strconv.Atoi(m)
	if err != nil {
		return 0
	}
	return year
}

func parseWrappedTags(raw string) []string {
	var entries []struct {
		Tag string `json:"tag"`
	}
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil
	}
	tags := make([]string, 0, len(entries))
	for _, e := range entries {
		if t := strings.TrimSpace(e.Tag); t != "" {
			tags = append(tags, t)
		}
	}
	return tags
}

// computeWrappedHighlights derives the superlatives from the year's rows.
// Pure function; deterministic ties (earliest date, alphabetical name win).
func computeWrappedHighlights(rows []wrappedItemRow, year int) libraryWrappedHighlights {
	var h libraryWrappedHighlights
	if len(rows) == 0 {
		return h
	}

	dayCounts := map[string]int{} // "2026-06-29" -> count
	weekdayCounts := map[time.Weekday]int{}
	tagCounts := map[string]int{}
	for _, r := range rows {
		dayCounts[r.AddedAt.Format("2006-01-02")]++
		weekdayCounts[r.AddedAt.Weekday()]++
		for _, t := range r.Tags {
			tagCounts[t]++
		}
		if r.PubYear == year {
			h.SameYearCount++
		}
	}

	// Busiest single day (ties: earliest date).
	days := make([]string, 0, len(dayCounts))
	for d := range dayCounts {
		days = append(days, d)
	}
	sort.Strings(days)
	bestDay, bestCount := "", 0
	for _, d := range days {
		if dayCounts[d] > bestCount {
			bestDay, bestCount = d, dayCounts[d]
		}
	}
	if bestDay != "" && bestCount > 1 {
		t, _ := time.Parse("2006-01-02", bestDay)
		h.BusiestDay = &libraryWrappedDayHighlight{Date: t.Format("Mon, Jan 2"), Count: bestCount}
	}

	// Favorite weekday (ties: earliest weekday, Sunday first).
	var bestWD time.Weekday
	bestWDCount := 0
	for wd := time.Sunday; wd <= time.Saturday; wd++ {
		if weekdayCounts[wd] > bestWDCount {
			bestWD, bestWDCount = wd, weekdayCounts[wd]
		}
	}
	if bestWDCount > 1 {
		h.TopWeekday = &libraryWrappedRankedCount{Name: bestWD.String(), Count: bestWDCount}
	}

	// Longest streak of consecutive days with at least one addition.
	if streak := longestDayStreak(days); streak != nil && streak.Days > 1 {
		h.LongestStreak = streak
	}

	// Deep cut: the oldest publication added this year (gap of 10+ years).
	for _, r := range rows {
		if r.PubYear == 0 || r.PubYear > year {
			continue
		}
		if h.DeepCut == nil || r.PubYear < h.DeepCut.Year {
			h.DeepCut = &libraryWrappedItemPick{Key: r.Key, Title: r.Title, Year: r.PubYear}
		}
	}
	if h.DeepCut != nil && year-h.DeepCut.Year < 10 {
		h.DeepCut = nil
	}

	// Top tag (ties: alphabetical).
	tagNames := make([]string, 0, len(tagCounts))
	for t := range tagCounts {
		tagNames = append(tagNames, t)
	}
	sort.Strings(tagNames)
	for _, t := range tagNames {
		if h.TopTag == nil || tagCounts[t] > h.TopTag.Count {
			h.TopTag = &libraryWrappedRankedCount{Name: t, Count: tagCounts[t]}
		}
	}
	return h
}

func longestDayStreak(sortedDays []string) *libraryWrappedStreak {
	if len(sortedDays) == 0 {
		return nil
	}
	parse := func(s string) (time.Time, bool) {
		t, err := time.Parse("2006-01-02", s)
		return t, err == nil
	}
	bestLen, curLen := 0, 0
	var bestStart, bestEnd, curStart, prev time.Time
	for _, d := range sortedDays {
		t, ok := parse(d)
		if !ok {
			continue
		}
		if curLen > 0 && t.Sub(prev) == 24*time.Hour {
			curLen++
		} else {
			curLen, curStart = 1, t
		}
		prev = t
		if curLen > bestLen {
			bestLen, bestStart, bestEnd = curLen, curStart, t
		}
	}
	if bestLen == 0 {
		return nil
	}
	return &libraryWrappedStreak{Days: bestLen, Start: bestStart.Format("Jan 2"), End: bestEnd.Format("Jan 2")}
}

// queryLibraryWrappedMostAnnotated finds the item whose PDFs collected the
// most annotations this year. Zotero nests annotation -> attachment -> item,
// so the attachment hop is resolved explicitly.
func queryLibraryWrappedMostAnnotated(db localQueryStore, year int) (*libraryWrappedItemPick, error) {
	rows, err := db.QueryRaw(`
SELECT json_extract(a.data,'$.data.parentItem') AS parent, COUNT(*) AS count
FROM resources a
WHERE a.resource_type='items'
	AND COALESCE(NULLIF(a.item_type,''), json_extract(a.data,'$.data.itemType'), '') = 'annotation'
	AND SUBSTR(COALESCE(json_extract(a.data,'$.data.dateAdded'),''), 1, 4) = ?
	AND NULLIF(TRIM(json_extract(a.data,'$.data.parentItem')),'') IS NOT NULL
GROUP BY parent
ORDER BY count DESC, parent ASC
LIMIT 1`, fmt.Sprintf("%04d", year))
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	attachmentKey := sqlStringValue(rows[0]["parent"])
	count := sqlIntValue(rows[0]["count"])
	if attachmentKey == "" || count == 0 {
		return nil, nil
	}

	// attachment -> owning item (annotations on standalone attachments keep
	// the attachment itself as the pick)
	itemKey := attachmentKey
	if parentRows, err := db.QueryRaw(`
SELECT COALESCE(NULLIF(TRIM(json_extract(data,'$.data.parentItem')),''), id) AS item
FROM resources WHERE resource_type='items' AND id = ?`, attachmentKey); err == nil && len(parentRows) > 0 {
		if v := sqlStringValue(parentRows[0]["item"]); v != "" {
			itemKey = v
		}
	}

	titleRows, err := db.QueryRaw(`
SELECT COALESCE(NULLIF(TRIM(json_extract(data,'$.data.title')),''),'(untitled)') AS title
FROM resources WHERE resource_type='items' AND id = ?`, itemKey)
	if err != nil || len(titleRows) == 0 {
		return nil, err
	}
	return &libraryWrappedItemPick{Key: itemKey, Title: sqlStringValue(titleRows[0]["title"]), Count: count}, nil
}
