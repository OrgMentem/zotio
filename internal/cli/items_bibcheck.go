// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Add manuscript bibliography citekey checking.

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/spf13/cobra"
)

// TeX citation commands accepted by items bibcheck.
var latexCitationRE = regexp.MustCompile(`\\(?:citeauthor|citeyear|parencite|textcite|autocite|citep|citet|cite|nocite)\*?(?:\s*\[[^\]\n]*\]){0,2}\s*\{([^}]*)\}`)

// Pandoc citekey token finder; code spans/blocks are stripped before this runs.
var pandocCitationRE = regexp.MustCompile(`(^|[^\\A-Za-z0-9_])-?@([A-Za-z0-9_][A-Za-z0-9_:.#$%&+?<>~/.-]*)`)

type bibcheckSummary struct {
	Total     int `json:"total"`
	OK        int `json:"ok"`
	Unknown   int `json:"unknown"`
	Ambiguous int `json:"ambiguous"`
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

type bibcheckReport struct {
	Manuscript string              `json:"manuscript"`
	Format     string              `json:"format"`
	Summary    bibcheckSummary     `json:"summary"`
	Keys       []bibcheckKeyResult `json:"keys"`
}

func newItemsBibcheckCmd(flags *rootFlags) *cobra.Command {
	var failOnUnknown bool

	cmd := &cobra.Command{
		Use:   "bibcheck <manuscript>",
		Short: "Check manuscript citation keys against the synced Better BibTeX library",
		Example: `  zotio items bibcheck paper.tex
  zotio items bibcheck manuscript.md --fail-on-unknown
  zotio items bibcheck chapter.qmd --json`,
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			if len(args) != 1 {
				return usageErr(fmt.Errorf("items bibcheck expects exactly one manuscript path"))
			}
			if dryRunOK(flags) {
				return nil
			}

			keys, format, err := parseManuscriptCiteKeys(args[0])
			if err != nil {
				return err
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

			report := buildBibcheckReport(args[0], format, keys, items)
			if err := printBibcheckReport(cmd, flags, report); err != nil {
				return err
			}
			if failOnUnknown && (report.Summary.Unknown > 0 || report.Summary.Ambiguous > 0) {
				return gateErr(fmt.Errorf("bibcheck found %d unknown and %d ambiguous citation key(s)", report.Summary.Unknown, report.Summary.Ambiguous))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&failOnUnknown, "fail-on-unknown", false, "Exit 11 when any cited key is unknown or ambiguous")

	return cmd
}

// Dispatch manuscript parsing by supported extension only.
func parseManuscriptCiteKeys(path string) ([]string, string, error) {
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
	if format == "tex" {
		return parseLatexCiteKeys(string(data)), format, nil
	}
	return parsePandocMarkdownCiteKeys(string(data)), format, nil
}

// Parse LaTeX cite/nocite command key lists, including starred and optional-argument variants.
func parseLatexCiteKeys(content string) []string {
	matches := latexCitationRE.FindAllStringSubmatch(content, -1)
	keys := make([]string, 0, len(matches))
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		keys = appendCiteKeyList(keys, match[1])
	}
	return keys
}

// Parse Pandoc @citekey tokens after removing fenced and inline code.
func parsePandocMarkdownCiteKeys(content string) []string {
	content = markdownWithoutCode(content)
	matches := pandocCitationRE.FindAllStringSubmatch(content, -1)
	keys := make([]string, 0, len(matches))
	for _, match := range matches {
		if len(match) < 3 {
			continue
		}
		key := trimPandocCiteKey(match[2])
		if key != "" && key != "*" {
			keys = append(keys, key)
		}
	}
	return keys
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
func buildBibcheckReport(manuscript, format string, keys []string, items []citekeyItem) bibcheckReport {
	order := make([]string, 0, len(keys))
	counts := make(map[string]int, len(keys))
	for _, key := range keys {
		if key == "" {
			continue
		}
		if counts[key] == 0 {
			order = append(order, key)
		}
		counts[key]++
	}

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

	report := bibcheckReport{
		Manuscript: manuscript,
		Format:     format,
		Keys:       make([]bibcheckKeyResult, 0, len(order)),
	}
	for _, key := range order {
		matches := byCiteKey[key]
		result := bibcheckKeyResult{
			CiteKey:     key,
			Occurrences: counts[key],
		}
		switch len(matches) {
		case 0:
			result.Status = "unknown"
			report.Summary.Unknown++
		case 1:
			result.Status = "ok"
			result.ItemKey = matches[0].Key
			result.Title = matches[0].Title
			report.Summary.OK++
		default:
			result.Status = "ambiguous"
			result.Matches = make([]bibcheckMatch, 0, len(matches))
			for _, match := range matches {
				result.Matches = append(result.Matches, bibcheckMatch{ItemKey: match.Key, Title: match.Title})
			}
			report.Summary.Ambiguous++
		}
		report.Keys = append(report.Keys, result)
	}
	report.Summary.Total = len(report.Keys)
	return report
}

func printBibcheckReport(cmd *cobra.Command, flags *rootFlags, report bibcheckReport) error {
	if flags.asJSON {
		return printCommandJSON(cmd.OutOrStdout(), report, flags)
	}
	if flags.quiet {
		return nil
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Manuscript: %s (%s)\n", report.Manuscript, report.Format)
	fmt.Fprintf(cmd.OutOrStdout(), "Summary: %d ok, %d unknown, %d ambiguous (%d cited keys)\n\n", report.Summary.OK, report.Summary.Unknown, report.Summary.Ambiguous, report.Summary.Total)
	if len(report.Keys) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No citation keys found.")
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
	return flags.printTable(cmd, []string{"CITEKEY", "STATUS", "ITEM", "TITLE"}, rows)
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
