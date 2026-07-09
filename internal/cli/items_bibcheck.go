// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Add manuscript bibliography citekey checking.

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/spf13/cobra"
)

// TeX citation commands accepted by items bibcheck.
var latexCitationRE = regexp.MustCompile(`\\(?:citeauthor|citeyear|parencite|textcite|autocite|footcite|fullcite|citep|citet|cite|nocite)\*?(?:\s*\[[^\]\n]*\]){0,2}\s*\{([^}]*)\}`)

// Pandoc citekey token finder; code spans/blocks are stripped before this runs.
var pandocCitationRE = regexp.MustCompile(`(^|[^\\A-Za-z0-9_])-?@([A-Za-z0-9][A-Za-z0-9_:.#$%&+?<>~/.-]*)`)

type bibcheckSummary struct {
	Total      int `json:"total"`
	OK         int `json:"ok"`
	Unknown    int `json:"unknown"`
	Ambiguous  int `json:"ambiguous"`
	Incomplete int `json:"incomplete,omitempty"`
}

type bibcheckLocation struct {
	File string `json:"file"`
	Line int    `json:"line"`
}

type bibcheckOccurrence struct {
	CiteKey string
	File    string
	Line    int
	Format  string
}

type bibcheckMatch struct {
	ItemKey string `json:"item_key"`
	Title   string `json:"title,omitempty"`
}

type bibcheckKeyResult struct {
	CiteKey     string          `json:"cite_key"`
	Status      string          `json:"status"`
	Occurrences int             `json:"occurrences"`
	ItemKey     string          `json:"item_key,omitempty"`
	Title       string          `json:"title,omitempty"`
	Matches     []bibcheckMatch `json:"matches,omitempty"`
}

type bibcheckFileReport struct {
	File    string              `json:"file"`
	Format  string              `json:"format"`
	Summary bibcheckSummary     `json:"summary"`
	Keys    []bibcheckKeyResult `json:"keys"`
}

type bibcheckReport struct {
	Manuscript string               `json:"manuscript,omitempty"`
	Format     string               `json:"format,omitempty"`
	Summary    bibcheckSummary      `json:"summary"`
	Keys       []bibcheckKeyResult  `json:"keys"`
	Files      []bibcheckFileReport `json:"files,omitempty"`
	Findings   []Finding            `json:"findings"`
}

func newItemsBibcheckCmd(flags *rootFlags) *cobra.Command {
	var failOnUnknown bool
	var failOn string

	cmd := &cobra.Command{
		Use:   "bibcheck <manuscript...>",
		Short: "Check manuscript citation keys against the synced Better BibTeX library",
		Example: `  zotio items bibcheck paper.tex
  zotio items bibcheck manuscript.md --fail-on-unknown
  zotio items bibcheck paper.tex chapter.md --json
  zotio items bibcheck chapter.qmd --fail-on high`,
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			if dryRunOK(flags) {
				return nil
			}

			gate := strings.ToLower(strings.TrimSpace(failOn))
			switch gate {
			case "", "none", sevHigh, "any":
			default:
				return usageErr(fmt.Errorf("invalid --fail-on %q: must be high, any, or none", failOn))
			}

			occurrences := make([]bibcheckOccurrence, 0)
			formats := make(map[string]string, len(args))
			for _, path := range args {
				parsed, format, err := parseManuscriptCitationOccurrences(path)
				if err != nil {
					return err
				}
				formats[path] = format
				occurrences = append(occurrences, parsed...)
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

			items, err := loadCitekeyItems(db)
			if err != nil {
				return err
			}
			incompleteRows, err := queryCitationIncompleteItems(db, 0)
			if err != nil {
				return fmt.Errorf("querying incomplete citation items: %w", err)
			}

			var source FindingSource
			source.Kind = "local"
			if _, lastSynced, _, err := db.GetSyncState("items"); err == nil && !lastSynced.IsZero() {
				syncedAt := lastSynced
				source.SyncedAt = &syncedAt
			}

			report := buildBibcheckReportFromOccurrences(args, formats, occurrences, items, incompleteRows, source)
			if err := printBibcheckReport(cmd, flags, report); err != nil {
				return err
			}
			if failOnUnknown && (report.Summary.Unknown > 0 || report.Summary.Ambiguous > 0) {
				return gateErr(fmt.Errorf("bibcheck found %d unknown and %d ambiguous citation key(s)", report.Summary.Unknown, report.Summary.Ambiguous))
			}
			if bibcheckGateTriggered(gate, report.Findings) {
				return gateErr(fmt.Errorf("bibcheck found %d finding(s) at or above %s", len(report.Findings), gate))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&failOnUnknown, "fail-on-unknown", false, "Exit 11 when any cited key is unknown or ambiguous")
	cmd.Flags().StringVar(&failOn, "fail-on", "", "Exit 11 when findings reach this severity: high, any, or none")

	return cmd
}

// Dispatch manuscript parsing by supported extension only.
func parseManuscriptCiteKeys(path string) ([]string, string, error) {
	occurrences, format, err := parseManuscriptCitationOccurrences(path)
	if err != nil {
		return nil, "", err
	}
	return citeKeysFromOccurrences(occurrences), format, nil
}

func parseManuscriptCitationOccurrences(path string) ([]bibcheckOccurrence, string, error) {
	ext := strings.ToLower(filepath.Ext(path))
	var format string
	switch ext {
	case ".tex":
		format = "tex"
	case ".md", ".markdown", ".qmd":
		format = "pandoc-markdown"
	default:
		if ext == "" {
			ext = "<none>"
		}
		return nil, "", usageErr(fmt.Errorf("unsupported manuscript extension %q; supported formats: .tex, .md, .markdown, .qmd", ext))
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("reading manuscript %q: %w", path, err)
	}
	content := string(data)
	if format == "tex" {
		return parseLatexCitationOccurrences(path, content), format, nil
	}
	return parsePandocMarkdownCitationOccurrences(path, content), format, nil
}

func citeKeysFromOccurrences(occurrences []bibcheckOccurrence) []string {
	keys := make([]string, 0, len(occurrences))
	for _, occurrence := range occurrences {
		keys = append(keys, occurrence.CiteKey)
	}
	return keys
}

// Parse LaTeX cite/nocite command key lists, including starred and optional-argument variants.
func parseLatexCiteKeys(content string) []string {
	return citeKeysFromOccurrences(parseLatexCitationOccurrences("", content))
}

func parseLatexCitationOccurrences(path, content string) []bibcheckOccurrence {
	matches := latexCitationRE.FindAllStringSubmatchIndex(content, -1)
	lines := lineStartOffsets(content)
	out := make([]bibcheckOccurrence, 0, len(matches))
	for _, match := range matches {
		if len(match) < 4 || match[2] < 0 || match[3] < 0 {
			continue
		}
		line := lineNumberForOffset(lines, match[0])
		for _, key := range appendCiteKeyList(nil, content[match[2]:match[3]]) {
			out = append(out, bibcheckOccurrence{CiteKey: key, File: path, Line: line, Format: "tex"})
		}
	}
	return out
}

// Parse Pandoc @citekey tokens after removing fenced and inline code.
func parsePandocMarkdownCiteKeys(content string) []string {
	return citeKeysFromOccurrences(parsePandocMarkdownCitationOccurrences("", content))
}

func parsePandocMarkdownCitationOccurrences(path, content string) []bibcheckOccurrence {
	content = markdownWithoutCode(content)
	lines := strings.Split(content, "\n")
	out := make([]bibcheckOccurrence, 0)
	for i, line := range lines {
		matches := pandocCitationRE.FindAllStringSubmatch(line, -1)
		for _, match := range matches {
			if len(match) < 3 {
				continue
			}
			key := trimPandocCiteKey(match[2])
			if key == "" || key == "*" {
				continue
			}
			out = append(out, bibcheckOccurrence{CiteKey: key, File: path, Line: i + 1, Format: "pandoc-markdown"})
		}
	}
	return out
}

func lineStartOffsets(content string) []int {
	starts := []int{0}
	for i, ch := range content {
		if ch == '\n' {
			starts = append(starts, i+1)
		}
	}
	return starts
}

func lineNumberForOffset(starts []int, offset int) int {
	line := sort.Search(len(starts), func(i int) bool { return starts[i] > offset })
	if line == 0 {
		return 1
	}
	return line
}

func appendCiteKeyList(keys []string, raw string) []string {
	for _, part := range strings.Split(raw, ",") {
		key := strings.TrimSpace(part)
		if key == "" || key == "*" {
			continue
		}
		keys = append(keys, key)
	}
	return keys
}

func trimPandocCiteKey(key string) string {
	end := -1
	for i, r := range key {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			end = i + len(string(r))
		}
	}
	if end < 0 {
		return ""
	}
	return key[:end]
}

func markdownWithoutCode(content string) string {
	var b strings.Builder
	inFence := false
	var fenceChar byte
	fenceLen := 0

	for _, line := range strings.SplitAfter(content, "\n") {
		plainLine := strings.TrimRight(line, "\r\n")
		trimmed := strings.TrimSpace(plainLine)
		if !inFence {
			if ch, n, ok := bibcheckMarkdownFence(trimmed); ok {
				inFence = true
				fenceChar = ch
				fenceLen = n
				b.WriteByte('\n')
				continue
			}
			b.WriteString(stripMarkdownInlineCode(plainLine))
			if strings.HasSuffix(line, "\n") {
				b.WriteByte('\n')
			}
			continue
		}

		if ch, n, ok := bibcheckMarkdownFence(trimmed); ok && ch == fenceChar && n >= fenceLen {
			inFence = false
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func bibcheckMarkdownFence(trimmed string) (byte, int, bool) {
	if len(trimmed) < 3 || (trimmed[0] != '`' && trimmed[0] != '~') {
		return 0, 0, false
	}
	ch := trimmed[0]
	n := 0
	for n < len(trimmed) && trimmed[n] == ch {
		n++
	}
	return ch, n, n >= 3
}

func stripMarkdownInlineCode(line string) string {
	var b strings.Builder
	for i := 0; i < len(line); {
		if line[i] != '`' {
			b.WriteByte(line[i])
			i++
			continue
		}
		n := countSameByte(line, i, '`')
		end := findClosingBackticks(line, i+n, n)
		if end < 0 {
			break
		}
		i = end + n
	}
	return b.String()
}

func countSameByte(s string, start int, ch byte) int {
	n := 0
	for start+n < len(s) && s[start+n] == ch {
		n++
	}
	return n
}

func findClosingBackticks(s string, start, n int) int {
	needle := strings.Repeat("`", n)
	idx := strings.Index(s[start:], needle)
	if idx < 0 {
		return -1
	}
	return start + idx
}

// Cross-reference manuscript keys against the shared Better BibTeX citekey inventory.
func buildBibcheckReportFromOccurrences(paths []string, formats map[string]string, occurrences []bibcheckOccurrence, items []citekeyItem, incompleteRows []map[string]any, source FindingSource) bibcheckReport {
	byCiteKey := bibcheckItemsByCiteKey(items)
	order, counts, locationsByCiteKey, occurrencesByFile := summarizeBibcheckOccurrences(occurrences)
	keys, summary := buildBibcheckKeyResults(order, counts, byCiteKey)
	findings, incompleteByFile := buildBibcheckFindings(order, locationsByCiteKey, byCiteKey, incompleteRows, source)
	for _, finding := range findings {
		if finding.Kind == "incomplete_citation" {
			summary.Incomplete++
		}
	}

	report := bibcheckReport{
		Summary:  summary,
		Keys:     keys,
		Findings: findings,
	}
	if len(paths) == 1 {
		report.Manuscript = paths[0]
		report.Format = formats[paths[0]]
		return report
	}

	report.Files = make([]bibcheckFileReport, 0, len(paths))
	for _, path := range paths {
		fileOrder, fileCounts, _, _ := summarizeBibcheckOccurrences(occurrencesByFile[path])
		fileKeys, fileSummary := buildBibcheckKeyResults(fileOrder, fileCounts, byCiteKey)
		fileSummary.Incomplete = incompleteByFile[path]
		report.Files = append(report.Files, bibcheckFileReport{
			File:    path,
			Format:  formats[path],
			Summary: fileSummary,
			Keys:    fileKeys,
		})
	}
	return report
}

func bibcheckItemsByCiteKey(items []citekeyItem) map[string][]citekeyItem {
	byCiteKey := make(map[string][]citekeyItem, len(items))
	for _, item := range items {
		if item.CiteKey == "" {
			continue
		}
		byCiteKey[item.CiteKey] = append(byCiteKey[item.CiteKey], item)
	}
	for citeKey := range byCiteKey {
		matches := byCiteKey[citeKey]
		sort.Slice(matches, func(i, j int) bool { return citekeyItemLess(matches[i], matches[j]) })
		byCiteKey[citeKey] = matches
	}
	return byCiteKey
}

func summarizeBibcheckOccurrences(occurrences []bibcheckOccurrence) ([]string, map[string]int, map[string][]bibcheckLocation, map[string][]bibcheckOccurrence) {
	order := make([]string, 0, len(occurrences))
	counts := make(map[string]int, len(occurrences))
	locationsByCiteKey := make(map[string][]bibcheckLocation, len(occurrences))
	occurrencesByFile := make(map[string][]bibcheckOccurrence)
	for _, occurrence := range occurrences {
		if occurrence.CiteKey == "" {
			continue
		}
		if counts[occurrence.CiteKey] == 0 {
			order = append(order, occurrence.CiteKey)
		}
		counts[occurrence.CiteKey]++
		if occurrence.File != "" {
			locationsByCiteKey[occurrence.CiteKey] = append(locationsByCiteKey[occurrence.CiteKey], bibcheckLocation{File: occurrence.File, Line: occurrence.Line})
			occurrencesByFile[occurrence.File] = append(occurrencesByFile[occurrence.File], occurrence)
		}
	}
	return order, counts, locationsByCiteKey, occurrencesByFile
}

func buildBibcheckKeyResults(order []string, counts map[string]int, byCiteKey map[string][]citekeyItem) ([]bibcheckKeyResult, bibcheckSummary) {
	keys := make([]bibcheckKeyResult, 0, len(order))
	var summary bibcheckSummary
	for _, key := range order {
		matches := byCiteKey[key]
		result := bibcheckKeyResult{
			CiteKey:     key,
			Occurrences: counts[key],
		}
		switch len(matches) {
		case 0:
			result.Status = "unknown"
			summary.Unknown++
		case 1:
			result.Status = "ok"
			result.ItemKey = matches[0].Key
			result.Title = matches[0].Title
			summary.OK++
		default:
			result.Status = "ambiguous"
			result.Matches = make([]bibcheckMatch, 0, len(matches))
			for _, match := range matches {
				result.Matches = append(result.Matches, bibcheckMatch{ItemKey: match.Key, Title: match.Title})
			}
			summary.Ambiguous++
		}
		keys = append(keys, result)
	}
	summary.Total = len(keys)
	return keys, summary
}

func buildBibcheckFindings(order []string, locationsByCiteKey map[string][]bibcheckLocation, byCiteKey map[string][]citekeyItem, incompleteRows []map[string]any, source FindingSource) ([]Finding, map[string]int) {
	incompleteByItem := make(map[string]map[string]any, len(incompleteRows))
	for _, row := range incompleteRows {
		key := sqlStringValue(row["key"])
		if key != "" {
			incompleteByItem[key] = row
		}
	}

	undefinedAction := &RecommendedAction{Text: "Add or correct the Better BibTeX key in Zotero, or fix the manuscript citation key"}
	incompleteAction := &RecommendedAction{Text: "Add the missing core citation fields (creators, title, date, venue) in Zotero"}
	findings := make([]Finding, 0)
	incompleteByFile := make(map[string]int)
	for _, citeKey := range order {
		matches := byCiteKey[citeKey]
		locations := locationsByCiteKey[citeKey]
		if len(matches) == 0 {
			findings = append(findings, Finding{
				Kind:              "undefined_citekey",
				Severity:          sevHigh,
				Title:             fmt.Sprintf("Undefined citekey %s", citeKey),
				Evidence:          bibcheckEvidence(citeKey, locations, nil, ""),
				Source:            source,
				Autofixable:       false,
				RecommendedAction: undefinedAction,
			})
			continue
		}

		for _, item := range matches {
			row, incomplete := incompleteByItem[item.Key]
			if !incomplete {
				continue
			}
			for _, file := range bibcheckLocationFiles(locations) {
				incompleteByFile[file]++
			}
			findings = append(findings, Finding{
				Kind:              "incomplete_citation",
				Severity:          sevHigh,
				ItemKey:           item.Key,
				Title:             item.Title,
				Evidence:          bibcheckEvidence(citeKey, locations, bibcheckMissingFields(sqlStringValue(row["missing"])), sqlStringValue(row["item_type"])),
				Source:            source,
				Autofixable:       false,
				RecommendedAction: incompleteAction,
			})
		}
	}
	return findings, incompleteByFile
}

func bibcheckEvidence(citeKey string, locations []bibcheckLocation, missingFields []string, itemType string) map[string]any {
	evidence := map[string]any{
		"citekey":    citeKey,
		"locations":  locations,
		"file_lines": bibcheckFileLines(locations),
	}
	if len(locations) > 0 {
		evidence["file"] = locations[0].File
		evidence["line"] = locations[0].Line
		evidence["file_line"] = bibcheckLocationString(locations[0])
	}
	if len(missingFields) > 0 {
		evidence["missing"] = strings.Join(missingFields, ", ")
		evidence["missing_fields"] = missingFields
	}
	if itemType != "" {
		evidence["item_type"] = itemType
	}
	return evidence
}

func bibcheckMissingFields(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	fields := make([]string, 0, len(parts))
	for _, part := range parts {
		field := strings.TrimSpace(part)
		if field != "" {
			fields = append(fields, field)
		}
	}
	return fields
}

func bibcheckLocationFiles(locations []bibcheckLocation) []string {
	seen := make(map[string]bool, len(locations))
	files := make([]string, 0)
	for _, loc := range locations {
		if loc.File == "" || seen[loc.File] {
			continue
		}
		seen[loc.File] = true
		files = append(files, loc.File)
	}
	return files
}

func bibcheckFileLines(locations []bibcheckLocation) []string {
	lines := make([]string, 0, len(locations))
	for _, loc := range locations {
		lines = append(lines, bibcheckLocationString(loc))
	}
	return lines
}

func bibcheckLocationString(loc bibcheckLocation) string {
	if loc.Line <= 0 {
		return loc.File
	}
	return loc.File + ":" + strconv.Itoa(loc.Line)
}

func bibcheckGateTriggered(failOn string, findings []Finding) bool {
	threshold := failOnRank(failOn)
	if threshold == 0 {
		return false
	}
	for _, finding := range findings {
		if severityRank(finding.Severity) >= threshold {
			return true
		}
	}
	return false
}

func printBibcheckReport(cmd *cobra.Command, flags *rootFlags, report bibcheckReport) error {
	if flags.asJSON {
		return printCommandJSON(cmd.OutOrStdout(), report, flags)
	}
	if flags.quiet {
		return nil
	}

	out := cmd.OutOrStdout()
	if len(report.Files) == 0 {
		fmt.Fprintf(out, "Manuscript: %s (%s)\n", report.Manuscript, report.Format)
	} else {
		fmt.Fprintf(out, "Manuscripts: %d files\n", len(report.Files))
	}
	fmt.Fprintf(out, "Summary: %d ok, %d unknown, %d ambiguous, %d incomplete (%d cited keys)\n\n", report.Summary.OK, report.Summary.Unknown, report.Summary.Ambiguous, report.Summary.Incomplete, report.Summary.Total)
	for _, file := range report.Files {
		fmt.Fprintf(out, "  %s (%s): %d ok, %d unknown, %d ambiguous, %d incomplete (%d cited keys)\n", file.File, file.Format, file.Summary.OK, file.Summary.Unknown, file.Summary.Ambiguous, file.Summary.Incomplete, file.Summary.Total)
	}
	if len(report.Files) > 0 {
		fmt.Fprintln(out)
	}
	if len(report.Keys) == 0 {
		fmt.Fprintln(out, "No citation keys found.")
		return nil
	}

	rows := make([][]string, 0, len(report.Keys))
	for _, key := range report.Keys {
		itemKey := key.ItemKey
		title := key.Title
		if key.Status == "ambiguous" {
			itemKey, title = summarizeBibcheckMatches(key.Matches)
		}
		rows = append(rows, []string{key.CiteKey, key.Status, itemKey, title})
	}
	if err := flags.printTable(cmd, []string{"CITEKEY", "STATUS", "ITEM", "TITLE"}, rows); err != nil {
		return err
	}
	if len(report.Findings) == 0 {
		return nil
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "Findings:")
	for _, finding := range report.Findings {
		citeKey, _ := finding.Evidence["citekey"].(string)
		locationText := formatBibcheckLocations(finding.Evidence["locations"])
		switch finding.Kind {
		case "undefined_citekey":
			fmt.Fprintf(out, "  undefined_citekey high %s — %s\n", citeKey, locationText)
		case "incomplete_citation":
			missing, _ := finding.Evidence["missing"].(string)
			if missing != "" {
				fmt.Fprintf(out, "  incomplete_citation high %s (%s) missing %s — %s\n", citeKey, finding.ItemKey, missing, locationText)
			} else {
				fmt.Fprintf(out, "  incomplete_citation high %s (%s) — %s\n", citeKey, finding.ItemKey, locationText)
			}
		}
	}
	return nil
}

func formatBibcheckLocations(raw any) string {
	locations, ok := raw.([]bibcheckLocation)
	if !ok || len(locations) == 0 {
		return "no manuscript location"
	}
	return strings.Join(bibcheckFileLines(locations), ", ")
}

func summarizeBibcheckMatches(matches []bibcheckMatch) (string, string) {
	itemKeys := make([]string, 0, len(matches))
	titles := make([]string, 0, len(matches))
	for _, match := range matches {
		itemKeys = append(itemKeys, match.ItemKey)
		if match.Title != "" {
			titles = append(titles, match.Title)
		}
	}
	return strings.Join(itemKeys, ","), strings.Join(titles, " | ")
}
