// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"fmt"
	"html"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type libraryWrappedReport struct {
	Year        int                         `json:"year"`
	Items       libraryWrappedItemSummary   `json:"items"`
	TopVenues   []libraryWrappedRankedCount `json:"top_venues,omitempty"`
	TopAuthors  []libraryWrappedRankedCount `json:"top_authors,omitempty"`
	Annotations *libraryWrappedAnnotations  `json:"annotations,omitempty"`
	PDFCoverage *libraryWrappedPDFCoverage  `json:"pdf_coverage,omitempty"`
	Highlights  libraryWrappedHighlights    `json:"highlights,omitzero"`
	CardPath    string                      `json:"card_path,omitempty"`
}

type libraryWrappedItemSummary struct {
	Total      int                         `json:"total"`
	ByMonth    []libraryWrappedMonthCount  `json:"by_month,omitempty"`
	ByItemType []libraryWrappedRankedCount `json:"by_item_type,omitempty"`
}

type libraryWrappedMonthCount struct {
	Month int    `json:"month"`
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type libraryWrappedRankedCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type libraryWrappedAnnotations struct {
	Count        int                       `json:"count"`
	BusiestMonth *libraryWrappedMonthCount `json:"busiest_month,omitempty"`
}

type libraryWrappedPDFCoverage struct {
	WithAttachment int `json:"with_attachment"`
	Total          int `json:"total"`
	Percent        int `json:"percent"`
}

func newLibraryWrappedCmd(flags *rootFlags) *cobra.Command {
	flagYear := time.Now().Year()
	var flagCard string
	var flagCardStyle string

	cmd := &cobra.Command{
		Use:         "wrapped",
		Short:       "Show a local year-in-review for your Zotero library",
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if flagYear < 1 || flagYear > 9999 {
				return usageErr(fmt.Errorf("--year must be between 1 and 9999"))
			}
			if strings.TrimSpace(flagCard) == "" && cmd.Flags().Changed("card") {
				return usageErr(fmt.Errorf("--card requires a non-empty path"))
			}
			if !validWrappedCardStyles[flagCardStyle] {
				return usageErr(fmt.Errorf("--card-style must be one of: overview, rhythm, picks, cycle"))
			}
			if cmd.Flags().Changed("card-style") && !cmd.Flags().Changed("card") {
				return usageErr(fmt.Errorf("--card-style requires --card"))
			}

			rawDB, err := openStoreForRead(cmd.Context(), "zotio")
			if err != nil {
				return fmt.Errorf("opening local database: %w", err)
			}
			if rawDB == nil {
				fmt.Fprintln(cmd.OutOrStdout(), "Run 'zotio sync' first.")
				return nil
			}
			defer rawDB.Close()
			db := localQueryStore{rawDB}

			totalStored, err := queryLibraryWrappedStoredItemCount(db)
			if err != nil {
				return fmt.Errorf("checking local library: %w", err)
			}
			if totalStored == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "Run 'zotio sync' first.")
				return nil
			}

			report, err := queryLibraryWrappedReport(db, flagYear)
			if err != nil {
				return fmt.Errorf("querying wrapped report: %w", err)
			}
			if flagCard != "" {
				if err := writeLibraryWrappedCard(flagCard, report, flagCardStyle); err != nil {
					return err
				}
				report.CardPath = flagCard
			}
			if flags.asJSON {
				return printCommandJSON(cmd.OutOrStdout(), report, flags)
			}
			return printLibraryWrapped(cmd, report)
		},
	}
	cmd.Flags().IntVar(&flagYear, "year", flagYear, "Year to summarize")
	cmd.Flags().StringVar(&flagCard, "card", "", "Write an 800x418 SVG share card to this path")
	cmd.Flags().StringVar(&flagCardStyle, "card-style", "overview", "Card layout: overview, rhythm, picks, or cycle (animated crossfade through all three)")
	return cmd
}

// keep all year-in-review reads local and scoped to top-level Zotero items.
func queryLibraryWrappedReport(db localQueryStore, year int) (libraryWrappedReport, error) {
	items, err := queryLibraryWrappedItems(db, year)
	if err != nil {
		return libraryWrappedReport{}, err
	}
	report := libraryWrappedReport{Year: year, Items: items}
	if items.Total == 0 {
		return report, nil
	}

	venues, err := queryLibraryWrappedTopVenues(db, year, 5)
	if err != nil {
		return libraryWrappedReport{}, err
	}
	report.TopVenues = venues

	authors, err := queryLibraryWrappedTopAuthors(db, year, 5)
	if err != nil {
		return libraryWrappedReport{}, err
	}
	report.TopAuthors = authors

	annotations, err := queryLibraryWrappedAnnotations(db, year)
	if err != nil {
		return libraryWrappedReport{}, err
	}
	report.Annotations = annotations

	coverage, err := queryLibraryWrappedPDFCoverage(db, year)
	if err != nil {
		return libraryWrappedReport{}, err
	}
	if coverage.Total > 0 {
		report.PDFCoverage = &coverage
	}

	itemRows, err := queryLibraryWrappedItemRows(db, year)
	if err != nil {
		return libraryWrappedReport{}, err
	}
	report.Highlights = computeWrappedHighlights(itemRows, year)
	if pick, err := queryLibraryWrappedMostAnnotated(db, year); err == nil && pick != nil {
		report.Highlights.MostAnnotated = pick
	}
	return report, nil
}

func queryLibraryWrappedStoredItemCount(db localQueryStore) (int, error) {
	rows, err := db.QueryRaw(`
SELECT COUNT(*) AS count
FROM resources
WHERE resource_type='items'
	AND COALESCE(NULLIF(item_type,''), json_extract(data,'$.data.itemType'), '') NOT IN ('attachment','note','annotation')`)
	if err != nil {
		return 0, err
	}
	return firstCount(rows), nil
}

func queryLibraryWrappedItems(db localQueryStore, year int) (libraryWrappedItemSummary, error) {
	monthRows, err := db.QueryRaw(`
SELECT CAST(SUBSTR(json_extract(data,'$.data.dateAdded'), 6, 2) AS INTEGER) AS month, COUNT(*) AS count
FROM resources
WHERE resource_type='items'
	AND COALESCE(NULLIF(item_type,''), json_extract(data,'$.data.itemType'), '') NOT IN ('attachment','note','annotation')
	AND SUBSTR(COALESCE(json_extract(data,'$.data.dateAdded'),''), 1, 4) = ?
GROUP BY month
ORDER BY month`, fmt.Sprintf("%04d", year))
	if err != nil {
		return libraryWrappedItemSummary{}, err
	}

	byMonth := make([]libraryWrappedMonthCount, 12)
	total := 0
	for i := range byMonth {
		byMonth[i] = libraryWrappedMonthCount{Month: i + 1, Name: shortMonthName(i + 1)}
	}
	for _, row := range monthRows {
		month := sqlIntValue(row["month"])
		if month < 1 || month > 12 {
			continue
		}
		count := sqlIntValue(row["count"])
		byMonth[month-1].Count = count
		total += count
	}

	typeRows, err := db.QueryRaw(`
SELECT COALESCE(NULLIF(item_type,''), json_extract(data,'$.data.itemType'), 'unknown') AS name, COUNT(*) AS count
FROM resources
WHERE resource_type='items'
	AND COALESCE(NULLIF(item_type,''), json_extract(data,'$.data.itemType'), '') NOT IN ('attachment','note','annotation')
	AND SUBSTR(COALESCE(json_extract(data,'$.data.dateAdded'),''), 1, 4) = ?
GROUP BY name
ORDER BY count DESC, name ASC`, fmt.Sprintf("%04d", year))
	if err != nil {
		return libraryWrappedItemSummary{}, err
	}

	return libraryWrappedItemSummary{
		Total:      total,
		ByMonth:    byMonth,
		ByItemType: rankedRows(typeRows),
	}, nil
}

func queryLibraryWrappedTopVenues(db localQueryStore, year, limit int) ([]libraryWrappedRankedCount, error) {
	rows, err := db.QueryRaw(`
SELECT TRIM(json_extract(data,'$.data.publicationTitle')) AS name, COUNT(*) AS count
FROM resources
WHERE resource_type='items'
	AND COALESCE(NULLIF(item_type,''), json_extract(data,'$.data.itemType'), '') NOT IN ('attachment','note','annotation')
	AND SUBSTR(COALESCE(json_extract(data,'$.data.dateAdded'),''), 1, 4) = ?
	AND NULLIF(TRIM(json_extract(data,'$.data.publicationTitle')), '') IS NOT NULL
GROUP BY name
ORDER BY count DESC, name ASC
LIMIT ?`, fmt.Sprintf("%04d", year), limit)
	if err != nil {
		return nil, err
	}
	return rankedRows(rows), nil
}

func queryLibraryWrappedTopAuthors(db localQueryStore, year, limit int) ([]libraryWrappedRankedCount, error) {
	rows, err := db.QueryRaw(`
SELECT COALESCE(
	NULLIF(TRIM(json_extract(data,'$.data.creators[0].lastName')), '')
		|| CASE WHEN NULLIF(TRIM(json_extract(data,'$.data.creators[0].firstName')), '') IS NULL
			THEN '' ELSE ', ' || TRIM(json_extract(data,'$.data.creators[0].firstName')) END,
	NULLIF(TRIM(json_extract(data,'$.data.creators[0].name')), '')
) AS name, COUNT(*) AS count
FROM resources
WHERE resource_type='items'
	AND COALESCE(NULLIF(item_type,''), json_extract(data,'$.data.itemType'), '') NOT IN ('attachment','note','annotation')
	AND SUBSTR(COALESCE(json_extract(data,'$.data.dateAdded'),''), 1, 4) = ?
	AND name IS NOT NULL
GROUP BY name
ORDER BY count DESC, name ASC
LIMIT ?`, fmt.Sprintf("%04d", year), limit)
	if err != nil {
		return nil, err
	}
	return rankedRows(rows), nil
}

func queryLibraryWrappedAnnotations(db localQueryStore, year int) (*libraryWrappedAnnotations, error) {
	allRows, err := db.QueryRaw(`
SELECT COUNT(*) AS count
FROM resources
WHERE resource_type='items'
	AND COALESCE(NULLIF(item_type,''), json_extract(data,'$.data.itemType'), '') = 'annotation'`)
	if err != nil {
		return nil, err
	}
	if firstCount(allRows) == 0 {
		return nil, nil
	}

	rows, err := db.QueryRaw(`
SELECT CAST(SUBSTR(json_extract(data,'$.data.dateAdded'), 6, 2) AS INTEGER) AS month, COUNT(*) AS count
FROM resources
WHERE resource_type='items'
	AND COALESCE(NULLIF(item_type,''), json_extract(data,'$.data.itemType'), '') = 'annotation'
	AND SUBSTR(COALESCE(json_extract(data,'$.data.dateAdded'),''), 1, 4) = ?
GROUP BY month
ORDER BY count DESC, month ASC`, fmt.Sprintf("%04d", year))
	if err != nil {
		return nil, err
	}

	out := &libraryWrappedAnnotations{}
	for _, row := range rows {
		month := sqlIntValue(row["month"])
		if month < 1 || month > 12 {
			continue
		}
		count := sqlIntValue(row["count"])
		out.Count += count
		if out.BusiestMonth == nil || count > out.BusiestMonth.Count {
			out.BusiestMonth = &libraryWrappedMonthCount{Month: month, Name: shortMonthName(month), Count: count}
		}
	}
	return out, nil
}

func queryLibraryWrappedPDFCoverage(db localQueryStore, year int) (libraryWrappedPDFCoverage, error) {
	rows, err := db.QueryRaw(`
SELECT
	COUNT(DISTINCT i.id) AS total_items,
	COUNT(DISTINCT CASE WHEN a.id IS NOT NULL THEN i.id END) AS items_with_pdf
FROM resources i
LEFT JOIN resources a ON
	a.resource_type='items'
	AND COALESCE(NULLIF(a.item_type,''), json_extract(a.data,'$.data.itemType'), '') = 'attachment'
	AND json_extract(a.data,'$.data.contentType') = 'application/pdf'
	AND (a.parent_key = i.id OR json_extract(a.data,'$.data.parentItem') = i.id)
WHERE i.resource_type='items'
	AND COALESCE(NULLIF(i.item_type,''), json_extract(i.data,'$.data.itemType'), '') NOT IN ('attachment','note','annotation')
	AND SUBSTR(COALESCE(json_extract(i.data,'$.data.dateAdded'),''), 1, 4) = ?`, fmt.Sprintf("%04d", year))
	if err != nil {
		return libraryWrappedPDFCoverage{}, err
	}
	if len(rows) == 0 {
		return libraryWrappedPDFCoverage{}, nil
	}
	coverage := libraryWrappedPDFCoverage{
		WithAttachment: sqlIntValue(rows[0]["items_with_pdf"]),
		Total:          sqlIntValue(rows[0]["total_items"]),
	}
	if coverage.Total > 0 {
		coverage.Percent = coverage.WithAttachment * 100 / coverage.Total
	}
	return coverage, nil
}

func rankedRows(rows []map[string]any) []libraryWrappedRankedCount {
	out := make([]libraryWrappedRankedCount, 0, len(rows))
	for _, row := range rows {
		name := strings.TrimSpace(sqlStringValue(row["name"]))
		if name == "" {
			continue
		}
		out = append(out, libraryWrappedRankedCount{Name: name, Count: sqlIntValue(row["count"])})
	}
	return out
}

// printLibraryWrapped renders the year-in-review. Every number is real and
// locally computed; sections with no data are omitted, never faked.
func printLibraryWrapped(cmd *cobra.Command, report libraryWrappedReport) error {
	w := cmd.OutOrStdout()
	fmt.Fprintln(w, bold(fmt.Sprintf("Wrapped · %d", report.Year)))
	fmt.Fprintln(w, dim("Your Zotero year in review — computed entirely from the local store."))
	fmt.Fprintln(w)

	if report.Items.Total == 0 {
		fmt.Fprintf(w, "No items were added in %d.\n", report.Year)
		if report.CardPath != "" {
			fmt.Fprintf(w, "SVG card written: %s\n", report.CardPath)
		}
		return nil
	}

	// Hero counters
	hero := []string{heroStat(report.Items.Total, "item", "items") + " added"}
	if report.Annotations != nil && report.Annotations.Count > 0 {
		hero = append(hero, heroStat(report.Annotations.Count, "annotation", "annotations"))
	}
	if s := report.Highlights.LongestStreak; s != nil {
		hero = append(hero, heroStat(s.Days, "day", "days")+" best streak")
	}
	fmt.Fprintln(w, "  "+strings.Join(hero, dim("   ·   ")))
	fmt.Fprintln(w)

	// Months, with the peak highlighted
	fmt.Fprintln(w, bold("Months"))
	maxMonth := wrappedMaxMonthCount(report.Items.ByMonth)
	for _, m := range report.Items.ByMonth {
		bar := statBar(m.Count, maxMonth)
		suffix := ""
		if m.Count == maxMonth && maxMonth > 0 {
			bar = green(strings.Repeat("▆", barCells(m.Count, maxMonth)))
			suffix = green(" ◂ peak")
		}
		fmt.Fprintf(w, "%s  %3d  %s%s\n", dim(m.Name), m.Count, bar, suffix)
	}
	fmt.Fprintln(w)

	// Type mix as one stacked ratio bar
	if len(report.Items.ByItemType) > 0 {
		fmt.Fprintln(w, bold("Type mix"))
		bar, legend := stackedRatioBar(report.Items.ByItemType, 36)
		fmt.Fprintln(w, bar)
		fmt.Fprintln(w, legend)
		fmt.Fprintln(w)
	}

	printWrappedHighlights(w, report)

	if len(report.TopVenues) > 0 {
		typed := make([]statRow, 0, len(report.TopVenues))
		for _, v := range report.TopVenues {
			typed = append(typed, statRow{label: truncate(v.Name, 42), count: v.Count})
		}
		printStatSection(w, "Top venues", typed)
	}
	if len(report.TopAuthors) > 0 {
		typed := make([]statRow, 0, len(report.TopAuthors))
		for _, a := range report.TopAuthors {
			typed = append(typed, statRow{label: truncate(a.Name, 42), count: a.Count})
		}
		printStatSection(w, "Top first authors", typed)
	}

	if c := report.PDFCoverage; c != nil {
		fmt.Fprintf(w, "%s  %d/%d (%d%%)  %s\n\n", bold("PDF coverage"), c.WithAttachment, c.Total, c.Percent, coverageBar(c.Percent))
	}

	if report.CardPath != "" {
		fmt.Fprintf(w, "SVG card written: %s\n", report.CardPath)
	} else {
		fmt.Fprintln(w, dim("Share it: zotio library wrapped --card wrapped.svg (--card-style overview|rhythm|picks|cycle)"))
	}
	return nil
}

func heroStat(n int, singular, plural string) string {
	noun := plural
	if n == 1 {
		noun = singular
	}
	return bold(cyan(fmt.Sprintf("%d", n))) + " " + noun
}

// printWrappedHighlights renders the superlatives block; rows with no data
// are skipped entirely.
func printWrappedHighlights(w io.Writer, report libraryWrappedReport) {
	h := report.Highlights
	type row struct{ label, value string }
	var rows []row
	if h.BusiestDay != nil {
		rows = append(rows, row{"Busiest day", fmt.Sprintf("%s — %d items added", h.BusiestDay.Date, h.BusiestDay.Count)})
	}
	if h.TopWeekday != nil {
		rows = append(rows, row{"Favorite weekday", fmt.Sprintf("%s (%d additions)", h.TopWeekday.Name, h.TopWeekday.Count)})
	}
	if h.LongestStreak != nil {
		rows = append(rows, row{"Longest streak", fmt.Sprintf("%d days in a row (%s – %s)", h.LongestStreak.Days, h.LongestStreak.Start, h.LongestStreak.End)})
	}
	if h.DeepCut != nil {
		rows = append(rows, row{"Deep cut", fmt.Sprintf("%q (%d) — %d years after publication", truncate(h.DeepCut.Title, 40), h.DeepCut.Year, report.Year-h.DeepCut.Year)})
	}
	if h.SameYearCount > 0 {
		rows = append(rows, row{"Hot off the press", fmt.Sprintf("%s published in %d itself", pluralCount(h.SameYearCount, "item", "items"), report.Year)})
	}
	if h.MostAnnotated != nil {
		rows = append(rows, row{"Most annotated", fmt.Sprintf("%q — %d annotations", truncate(h.MostAnnotated.Title, 40), h.MostAnnotated.Count)})
	}
	if h.TopTag != nil {
		rows = append(rows, row{"Top tag", fmt.Sprintf("%s (%d items)", h.TopTag.Name, h.TopTag.Count)})
	}
	if len(rows) == 0 {
		return
	}
	fmt.Fprintln(w, bold("Highlights"))
	labelW := 0
	for _, r := range rows {
		if l := len(r.label); l > labelW {
			labelW = l
		}
	}
	for _, r := range rows {
		fmt.Fprintf(w, "  %s  %s\n", dim(padRight(r.label, labelW)), r.value)
	}
	fmt.Fprintln(w)
}

// stackedRatioBar renders counts as one proportional multi-color bar plus a
// matching legend line. Every non-zero share gets at least one cell.
func stackedRatioBar(rows []libraryWrappedRankedCount, cells int) (string, string) {
	total := 0
	for _, r := range rows {
		total += r.Count
	}
	if total == 0 {
		return "", ""
	}
	palette := []func(string) string{cyan, green, yellow, magenta, blue, red}
	var bar, legend strings.Builder
	used := 0
	for i, r := range rows {
		n := r.Count * cells / total
		if n < 1 {
			n = 1
		}
		if i == len(rows)-1 && used+n < cells {
			n = cells - used // last share absorbs rounding remainder
		}
		used += n
		style := palette[i%len(palette)]
		bar.WriteString(style(strings.Repeat("▆", n)))
		if i > 0 {
			legend.WriteString(dim("  ·  "))
		}
		legend.WriteString(style("▆") + " " + r.Name + " " + dim(fmt.Sprintf("%d", r.Count)))
	}
	return bar.String(), legend.String()
}

// coverageBar renders a 24-cell percentage bar colored by severity.
func coverageBar(pct int) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	filled := pct * 24 / 100
	style := red
	switch {
	case pct >= 80:
		style = green
	case pct >= 50:
		style = yellow
	}
	return style(strings.Repeat("▆", filled)) + dim(strings.Repeat("░", 24-filled))
}

// barCells mirrors statBar's cell count so peak bars recolor identically.
func barCells(count, max int) int {
	if max <= 0 || count <= 0 {
		return 0
	}
	n := count * 24 / max
	if n < 1 {
		n = 1
	}
	return n
}

func wrappedMaxMonthCount(months []libraryWrappedMonthCount) int {
	maxCount := 0
	for _, month := range months {
		if month.Count > maxCount {
			maxCount = month.Count
		}
	}
	return maxCount
}

func shortMonthName(month int) string {
	if month < 1 || month > 12 {
		return ""
	}
	return time.Month(month).String()[:3]
}

var validWrappedCardStyles = map[string]bool{"overview": true, "rhythm": true, "picks": true, "cycle": true}

// generate a dependency-free SVG card with escaped local metadata.
func writeLibraryWrappedCard(path string, report libraryWrappedReport, style string) error {
	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating card directory: %w", err)
		}
	}
	svg := renderLibraryWrappedSVG(report, style)
	// #nosec G306 -- the SVG card is a user-requested shareable artifact, not a secret.
	if err := os.WriteFile(path, []byte(svg), 0o644); err != nil {
		return fmt.Errorf("writing SVG card: %w", err)
	}
	return nil
}

// Card palette; mirrors the terminal palette so the shareable artifact and
// the terminal experience read as one product.
var wrappedCardPalette = []string{"#4cc2ff", "#3ddc97", "#ffd166", "#ef6da8", "#6a8dff", "#ff6b6b"}

const wrappedCardFont = `font-family="Inter, ui-sans-serif, system-ui, sans-serif"`

// renderLibraryWrappedSVG renders one card layout, or — for style "cycle" —
// an animated card that crossfades through all three layouts with CSS
// keyframes (works inside GitHub's <img> sandbox; honors reduced motion).
func renderLibraryWrappedSVG(report libraryWrappedReport, style string) string {
	var body string
	switch style {
	case "rhythm":
		body = wrappedCardRhythmBody(report)
	case "picks":
		body = wrappedCardPicksBody(report)
	case "cycle":
		return wrappedCardShell(report, fmt.Sprintf(`  <style>
    .zw-slide{opacity:0;animation:zw-rest 18s linear infinite}
    .zw-first{animation-name:zw-lead}
    .zw-s2{animation-delay:6s}
    .zw-s3{animation-delay:12s}
    @keyframes zw-lead{0%%,33.3%%{opacity:1}36.3%%,96%%{opacity:0}100%%{opacity:1}}
    @keyframes zw-rest{0%%{opacity:0}3%%,33.3%%{opacity:1}36.3%%,100%%{opacity:0}}
    @media (prefers-reduced-motion: reduce){.zw-slide{animation:none}.zw-first{opacity:1}}
  </style>
  <g class="zw-slide zw-first">
%s  </g>
  <g class="zw-slide zw-s2">
  <rect width="800" height="418" rx="24" fill="url(#zw-bg)"/>
%s  </g>
  <g class="zw-slide zw-s3">
  <rect width="800" height="418" rx="24" fill="url(#zw-bg)"/>
%s  </g>
`, wrappedCardOverviewBody(report), wrappedCardRhythmBody(report), wrappedCardPicksBody(report)))
	default:
		body = wrappedCardOverviewBody(report)
	}
	return wrappedCardShell(report, body)
}

// wrappedCardShell wraps a card body with the SVG document, background,
// accent strip, header, and the computed-locally footer.
func wrappedCardShell(report libraryWrappedReport, body string) string {
	var b strings.Builder
	year := report.Year
	fmt.Fprintf(&b, `<?xml version="1.0" encoding="UTF-8"?>
<svg xmlns="http://www.w3.org/2000/svg" width="800" height="418" viewBox="0 0 800 418" role="img" aria-labelledby="title desc">
  <title id="title">zotio wrapped %d</title>
  <desc id="desc">A local Zotero year in review: %d items added in %d.</desc>
  <defs>
    <linearGradient id="zw-bg" x1="0" y1="0" x2="0" y2="1">
      <stop offset="0" stop-color="#0c111f"/>
      <stop offset="1" stop-color="#141b30"/>
    </linearGradient>
  </defs>
  <rect width="800" height="418" rx="24" fill="url(#zw-bg)"/>
%s  <rect x="24" y="0" width="752" height="3" rx="1.5" fill="#4cc2ff" opacity="0.85"/>
  <text x="54" y="58" fill="#e8eef7" %s font-size="24" font-weight="800">zotio</text>
  <text x="746" y="58" text-anchor="end" fill="#8fa3bd" %s font-size="17" font-weight="600" letter-spacing="4">WRAPPED %d</text>
  <text x="746" y="404" text-anchor="end" fill="#5c7089" %s font-size="12">computed locally by zotio — no data leaves your machine</text>
</svg>
`, year, report.Items.Total, year, body, wrappedCardFont, wrappedCardFont, year, wrappedCardFont)
	return b.String()
}

// wrappedCardOverviewBody is the default layout: hero counter, type mix,
// highlights, monthly rhythm, venue/author footer, coverage meter.
func wrappedCardOverviewBody(report libraryWrappedReport) string {
	var b strings.Builder
	// Hero: big count + chips
	fmt.Fprintf(&b, `  <text x="54" y="128" fill="#ffffff" %s font-size="62" font-weight="800">%d</text>
`, wrappedCardFont, report.Items.Total)
	heroLabelX := 54 + heroDigitsWidth(report.Items.Total)
	fmt.Fprintf(&b, `  <text x="%d" y="128" fill="#8fa3bd" %s font-size="21">items added</text>
`, heroLabelX, wrappedCardFont)
	chips := make([]string, 0, 2)
	if report.Annotations != nil && report.Annotations.Count > 0 {
		chips = append(chips, pluralCount(report.Annotations.Count, "annotation", "annotations"))
	}
	if s := report.Highlights.LongestStreak; s != nil {
		chips = append(chips, fmt.Sprintf("%d-day streak", s.Days))
	}
	chipX := 746
	for i := len(chips) - 1; i >= 0; i-- { // right-aligned row of pills
		w := 11*len(chips[i]) + 24
		chipX -= w
		fmt.Fprintf(&b, `  <rect x="%d" y="104" width="%d" height="30" rx="15" fill="#1b2740" stroke="#2e4566"/>
  <text x="%d" y="124" fill="#bcd2ea" %s font-size="15">%s</text>
`, chipX, w, chipX+12, wrappedCardFont, svgText(chips[i]))
		chipX -= 12
	}

	// Type mix: full-width stacked ratio bar + legend
	b.WriteString(wrappedSVGTypeMix(report.Items.ByItemType, "zw-mix-ov", 54, 156, 692, 14))

	// Left column: highlights
	b.WriteString(wrappedSVGHighlights(report, 54, 236))

	// Right column: monthly rhythm
	fmt.Fprintf(&b, `  <text x="440" y="236" fill="#e8eef7" %s font-size="16" font-weight="700">Monthly rhythm</text>
`, wrappedCardFont)
	b.WriteString(wrappedSVGMonthBars(report.Items.ByMonth, 440, 336))

	// Footer: venue/author left, coverage right, trust line
	footer := make([]string, 0, 2)
	if len(report.TopVenues) > 0 {
		footer = append(footer, fmt.Sprintf("Top venue: %s (%d)", report.TopVenues[0].Name, report.TopVenues[0].Count))
	}
	if len(report.TopAuthors) > 0 {
		footer = append(footer, fmt.Sprintf("Top author: %s (%d)", report.TopAuthors[0].Name, report.TopAuthors[0].Count))
	}
	if len(footer) > 0 {
		fmt.Fprintf(&b, `  <text x="54" y="381" fill="#9fb7d3" %s font-size="15">%s</text>
`, wrappedCardFont, svgText(strings.Join(footer, "   ·   ")))
	}
	if c := report.PDFCoverage; c != nil {
		fill := c.Percent * 120 / 100
		fmt.Fprintf(&b, `  <rect x="560" y="371" width="120" height="8" rx="4" fill="#1b2740"/>
  <rect x="560" y="371" width="%d" height="8" rx="4" fill="%s"/>
  <text x="690" y="381" fill="#9fb7d3" %s font-size="14">%d%% PDFs</text>
`, fill, coverageFill(c.Percent), wrappedCardFont, c.Percent)
	}
	return b.String()
}

// wrappedCardRhythmBody is the time-focused layout: stat blocks for streak,
// busiest day, and favorite weekday next to a large month chart.
func wrappedCardRhythmBody(report libraryWrappedReport) string {
	var b strings.Builder
	h := report.Highlights
	type stat struct{ big, label, color string }
	var stats []stat
	if s := h.LongestStreak; s != nil {
		stats = append(stats, stat{fmt.Sprintf("%d days", s.Days), fmt.Sprintf("longest streak (%s – %s)", s.Start, s.End), "#3ddc97"})
	}
	if d := h.BusiestDay; d != nil {
		stats = append(stats, stat{pluralCount(d.Count, "item", "items"), fmt.Sprintf("busiest day (%s)", d.Date), "#4cc2ff"})
	}
	if wd := h.TopWeekday; wd != nil {
		stats = append(stats, stat{wd.Name + "s", fmt.Sprintf("favorite weekday (%d additions)", wd.Count), "#ffd166"})
	}
	if len(stats) == 0 {
		stats = append(stats, stat{pluralCount(report.Items.Total, "item", "items"), fmt.Sprintf("added in %d", report.Year), "#4cc2ff"})
	}
	y := 132
	for _, s := range stats {
		fmt.Fprintf(&b, `  <text x="54" y="%d" fill="%s" %s font-size="30" font-weight="800">%s</text>
  <text x="54" y="%d" fill="#8fa3bd" %s font-size="14">%s</text>
`, y, s.color, wrappedCardFont, svgText(s.big), y+22, wrappedCardFont, svgText(s.label))
		y += 74
	}
	// Large month chart on the right
	fmt.Fprintf(&b, `  <text x="410" y="110" fill="#e8eef7" %s font-size="16" font-weight="700">Monthly rhythm</text>
`, wrappedCardFont)
	b.WriteString(wrappedSVGMonthBarsSized(report.Items.ByMonth, 410, 320, 24, 6, 160))
	if a := report.Annotations; a != nil && a.Count > 0 {
		line := pluralCount(a.Count, "annotation", "annotations")
		if a.BusiestMonth != nil {
			line += fmt.Sprintf(", busiest in %s", a.BusiestMonth.Name)
		}
		fmt.Fprintf(&b, `  <text x="410" y="374" fill="#9fb7d3" %s font-size="14">%s</text>
`, wrappedCardFont, svgText(line))
	}
	if h.SameYearCount > 0 {
		fmt.Fprintf(&b, `  <text x="54" y="374" fill="#9fb7d3" %s font-size="14">%s published in %d itself</text>
`, wrappedCardFont, svgText(pluralCount(h.SameYearCount, "item", "items")), report.Year)
	}
	return b.String()
}

// wrappedCardPicksBody is the content-focused layout: the standout papers
// and tags, plus top venues and authors as ranked mini-bars.
func wrappedCardPicksBody(report libraryWrappedReport) string {
	var b strings.Builder
	h := report.Highlights
	type pick struct{ color, label, value string }
	var picks []pick
	if h.DeepCut != nil {
		picks = append(picks, pick{"#ffd166", "Deep cut", fmt.Sprintf("%s (%d) — %d years after publication", truncate(h.DeepCut.Title, 40), h.DeepCut.Year, report.Year-h.DeepCut.Year)})
	}
	if h.MostAnnotated != nil {
		picks = append(picks, pick{"#4cc2ff", "Most annotated", fmt.Sprintf("%s (%d annotations)", truncate(h.MostAnnotated.Title, 40), h.MostAnnotated.Count)})
	}
	if h.TopTag != nil {
		picks = append(picks, pick{"#ef6da8", "Top tag", fmt.Sprintf("%s (%d items)", truncate(h.TopTag.Name, 32), h.TopTag.Count)})
	}
	y := 108
	for _, p := range picks {
		fmt.Fprintf(&b, `  <circle cx="59" cy="%d" r="5" fill="%s"/>
  <text x="78" y="%d" fill="#8fa3bd" %s font-size="14">%s</text>
  <text x="78" y="%d" fill="#e8eef7" %s font-size="17" font-weight="600">%s</text>
`, y-6, p.color, y, wrappedCardFont, svgText(p.label), y+24, wrappedCardFont, svgText(p.value))
		y += 62
	}
	if len(picks) == 0 {
		fmt.Fprintf(&b, `  <text x="54" y="120" fill="#8fa3bd" %s font-size="16">Not enough data for picks this year — see the overview card.</text>
`, wrappedCardFont)
	}
	// Ranked columns
	b.WriteString(wrappedSVGRankedColumn("Top venues", report.TopVenues, 54, 312))
	b.WriteString(wrappedSVGRankedColumn("Top authors", report.TopAuthors, 420, 312))
	return b.String()
}

// wrappedSVGRankedColumn renders up to three ranked rows with proportional
// mini-bars.
func wrappedSVGRankedColumn(title string, rows []libraryWrappedRankedCount, x, startY int) string {
	if len(rows) == 0 {
		return ""
	}
	if len(rows) > 3 {
		rows = rows[:3]
	}
	maxCount := rows[0].Count
	var b strings.Builder
	fmt.Fprintf(&b, `  <text x="%d" y="%d" fill="#e8eef7" %s font-size="15" font-weight="700">%s</text>
`, x, startY, wrappedCardFont, svgText(title))
	y := startY + 20
	for _, r := range rows {
		barW := 12
		if maxCount > 0 {
			barW = 12 + r.Count*100/maxCount
		}
		fmt.Fprintf(&b, `  <rect x="%d" y="%d" width="%d" height="8" rx="4" fill="#4cc2ff" opacity="0.8"/>
  <text x="%d" y="%d" fill="#9fb7d3" %s font-size="13">%s (%d)</text>
`, x, y-8, barW, x+barW+10, y, wrappedCardFont, svgText(truncate(r.Name, 26)), r.Count)
		y += 24
	}
	return b.String()
}

// heroDigitsWidth approximates the rendered width of the hero counter so the
// unit label sits next to it regardless of magnitude.
func heroDigitsWidth(n int) int {
	return 16 + len(strconv.Itoa(n))*38
}

func coverageFill(pct int) string {
	switch {
	case pct >= 80:
		return "#3ddc97"
	case pct >= 50:
		return "#ffd166"
	}
	return "#ff6b6b"
}

// wrappedSVGTypeMix renders the stacked ratio bar with a compact legend for
// up to four types (the rest fold into "other").
func wrappedSVGTypeMix(rows []libraryWrappedRankedCount, clipID string, x, y, w, h int) string {
	total := 0
	for _, r := range rows {
		total += r.Count
	}
	if total == 0 {
		return ""
	}
	shares := rows
	other := 0
	if len(shares) > 4 {
		for _, r := range shares[4:] {
			other += r.Count
		}
		shares = shares[:4]
	}
	var b strings.Builder
	fmt.Fprintf(&b, `  <clipPath id="%s"><rect x="%d" y="%d" width="%d" height="%d" rx="7"/></clipPath>
  <g clip-path="url(#%s)">
`, clipID, x, y, w, h, clipID)
	cx := x
	legendParts := make([]string, 0, 5)
	emit := func(name string, count, idx int) {
		sw := count * w / total
		if sw < 6 {
			sw = 6
		}
		if cx+sw > x+w || idx == len(shares) && other == 0 {
			sw = x + w - cx
		}
		color := wrappedCardPalette[idx%len(wrappedCardPalette)]
		fmt.Fprintf(&b, `    <rect x="%d" y="%d" width="%d" height="%d" fill="%s"/>
`, cx, y, sw, h, color)
		cx += sw
		legendParts = append(legendParts, fmt.Sprintf(`<tspan fill="%s">●</tspan> %s %d`, color, svgText(name), count))
	}
	for i, r := range shares {
		emit(r.Name, r.Count, i)
	}
	if other > 0 {
		emit("other", other, len(shares))
	}
	// close any rounding gap with the last color
	if cx < x+w {
		fmt.Fprintf(&b, `    <rect x="%d" y="%d" width="%d" height="%d" fill="%s"/>
`, cx, y, x+w-cx, h, wrappedCardPalette[(len(legendParts)-1)%len(wrappedCardPalette)])
	}
	b.WriteString("  </g>\n")
	fmt.Fprintf(&b, `  <text x="%d" y="%d" fill="#8fa3bd" %s font-size="14">%s</text>
`, x, y+34, wrappedCardFont, strings.Join(legendParts, "&#160;&#160;&#160;"))
	return b.String()
}

// wrappedSVGHighlights renders up to four superlative rows with colored dots;
// missing signals are skipped and remaining rows shift up.
func wrappedSVGHighlights(report libraryWrappedReport, x, startY int) string {
	h := report.Highlights
	type hl struct{ color, label, value string }
	var rows []hl
	if h.DeepCut != nil {
		rows = append(rows, hl{"#ffd166", "Deep cut", fmt.Sprintf("%s (%d)", truncate(h.DeepCut.Title, 30), h.DeepCut.Year)})
	}
	if h.MostAnnotated != nil {
		rows = append(rows, hl{"#4cc2ff", "Most annotated", fmt.Sprintf("%s (%d)", truncate(h.MostAnnotated.Title, 26), h.MostAnnotated.Count)})
	}
	if h.BusiestDay != nil {
		rows = append(rows, hl{"#3ddc97", "Busiest day", fmt.Sprintf("%s — %d items", h.BusiestDay.Date, h.BusiestDay.Count)})
	}
	if h.TopTag != nil {
		rows = append(rows, hl{"#ef6da8", "Top tag", fmt.Sprintf("%s (%d)", truncate(h.TopTag.Name, 24), h.TopTag.Count)})
	}
	if len(rows) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, `  <text x="%d" y="%d" fill="#e8eef7" %s font-size="16" font-weight="700">Highlights</text>
`, x, startY, wrappedCardFont)
	y := startY + 28
	for _, r := range rows {
		fmt.Fprintf(&b, `  <circle cx="%d" cy="%d" r="4" fill="%s"/>
  <text x="%d" y="%d" fill="#8fa3bd" %s font-size="14">%s</text>
  <text x="%d" y="%d" fill="#d5e2f2" %s font-size="14">%s</text>
`, x+5, y-5, r.color, x+20, y, wrappedCardFont, svgText(r.label), x+140, y, wrappedCardFont, svgText(r.value))
		y += 26
	}
	return b.String()
}

// wrappedSVGMonthBars renders the 12-month chart with the peak highlighted.
func wrappedSVGMonthBars(months []libraryWrappedMonthCount, x, baseline int) string {
	return wrappedSVGMonthBarsSized(months, x, baseline, 16, 10, 74)
}

// wrappedSVGMonthBarsSized renders the month chart with configurable bar
// width, gap, and maximum bar height; the peak month is highlighted.
func wrappedSVGMonthBarsSized(months []libraryWrappedMonthCount, x, baseline, barW, gap, maxH int) string {
	maxCount := wrappedMaxMonthCount(months)
	if len(months) == 0 || maxCount == 0 {
		return fmt.Sprintf(`  <text x="%d" y="%d" fill="#8fa3bd" %s font-size="14">No monthly items for this year</text>
`, x, baseline-40, wrappedCardFont)
	}
	var b strings.Builder
	for i, month := range months {
		bx := x + i*(barW+gap)
		height := 6 + int(math.Round(float64(month.Count)/float64(maxCount)*float64(maxH)))
		fill := "#4cc2ff"
		opacity := "1"
		if month.Count == 0 {
			height = 4
			opacity = "0.35"
		}
		if month.Count == maxCount {
			fill = "#3ddc97"
			// label the peak with its count
			fmt.Fprintf(&b, `  <text x="%d" y="%d" text-anchor="middle" fill="#3ddc97" %s font-size="12" font-weight="700">%d</text>
`, bx+barW/2, baseline-height-8, wrappedCardFont, month.Count)
		}
		fmt.Fprintf(&b, `  <rect x="%d" y="%d" width="%d" height="%d" rx="5" fill="%s" opacity="%s"/>
`, bx, baseline-height, barW, height, fill, opacity)
		fmt.Fprintf(&b, `  <text x="%d" y="%d" text-anchor="middle" fill="#5c7089" %s font-size="10">%s</text>
`, bx+barW/2, baseline+16, wrappedCardFont, svgText(month.Name[:1]))
	}
	return b.String()
}

func svgText(s string) string {
	return html.EscapeString(strings.TrimSpace(s))
}

func pluralCount(n int, singular, plural string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %s", n, plural)
}
