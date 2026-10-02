// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Add manuscript bibliography citekey checking.

package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	// Suggestions are the closest citekeys in the library to a key that
	// matched nothing. They are advisory and deliberately NOT Matches:
	// Matches means the library really holds those items under this key, and
	// folding a guess into it would report a broken citation as resolved.
	// Status stays "unknown" for the same reason — the key really is unknown.
	Suggestions []nearCiteKeyMatch `json:"suggestions,omitempty"`
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
	Includes   []bibcheckInclude    `json:"includes,omitempty"`
	Findings   []Finding            `json:"findings"`
}

// bibcheckInclude is one file traversed through \input{} or \include{}
// under --follow-includes, with the file and line that pulled it in.
type bibcheckInclude struct {
	File         string `json:"file"`
	IncludedFrom string `json:"included_from"`
	Line         int    `json:"line"`
}

// TeX include commands followed by --follow-includes: only the braced forms.
var latexIncludeRE = regexp.MustCompile(`\\(?:input|include)\s*\{([^}\n]*)\}`)

func newItemsBibcheckCmd(flags *rootFlags) *cobra.Command {
	var failOnUnknown bool
	var failOn string
	var followIncludes bool

	cmd := &cobra.Command{
		Use:   "bibcheck <manuscript...>",
		Short: "Check manuscript citation keys against the synced Better BibTeX library",
		Example: `  zotio items bibcheck paper.tex
  zotio items bibcheck manuscript.md --fail-on-unknown
  zotio items bibcheck paper.tex chapter.md --json
  zotio items bibcheck chapter.qmd --fail-on high`,
		Annotations: map[string]string{"mcp:read-only": "true"},
		Args:        cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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
			var tree *bibcheckIncludeWalker
			if followIncludes {
				walker, err := newBibcheckIncludeWalker(args)
				if err != nil {
					return err
				}
				tree = walker
			}
			for _, path := range args {
				if tree != nil && strings.ToLower(filepath.Ext(path)) == ".tex" {
					parsed, err := tree.walkRoot(path)
					if err != nil {
						return err
					}
					formats[path] = "tex"
					occurrences = append(occurrences, parsed...)
					continue
				}
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
			incompleteRows, err := queryCitationIncompleteItems(db, 0, "")
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
			if tree != nil {
				report.Includes = tree.includes
				for i := range tree.findings {
					tree.findings[i].Source = source
				}
				report.Findings = append(report.Findings, tree.findings...)
			}
			if err := printBibcheckReport(cmd, flags, report); err != nil {
				return err
			}
			if tree != nil && len(tree.findings) > 0 {
				return gateErr(fmt.Errorf("bibcheck found %d include error(s)", len(tree.findings)))
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
	cmd.Flags().BoolVar(&followIncludes, "follow-includes", false, "Also check .tex files pulled in by \\input{PATH} and \\include{PATH}, resolved from the named file's directory; a missing include or a cycle exits 11. Only the braced forms are followed: not \\input PATH, \\subfile, or \\import")

	return cmd
}

// Dispatch manuscript parsing by supported extension only.
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

// bibcheckIncludeWalker follows \input{}/\include{} across every named root.
// TeX resolves an include against the compile directory, so the same file can
// pull in different chapters under roots in different directories: each root
// directory gets its own traversal, keyed by cleaned absolute path, and roots
// that share a directory share it. A file reached from more than one
// traversal contributes its citations once.
type bibcheckIncludeWalker struct {
	// visited maps a root's absolute directory to the files already read
	// in that directory's traversal.
	visited map[string]map[string]bool
	// named maps the absolute path of each .tex file named on the command
	// line to the path as given, so a named file reached through an include
	// still reports under its own per-file entry.
	named map[string]string
	// cited holds the absolute paths whose citations are already counted.
	cited       map[string]bool
	seenInclude map[bibcheckInclude]bool
	seenFinding map[bibcheckSeenFinding]bool
	includes    []bibcheckInclude
	findings    []Finding
	// sourceOrder interleaves citations with includes for bibliography selection.
	// Bibcheck retains its root-first report order.
	sourceOrder bool
}

type bibcheckSeenFinding struct {
	kind, file, target string
	line               int
}

func newBibcheckIncludeWalker(paths []string) (*bibcheckIncludeWalker, error) {
	w := &bibcheckIncludeWalker{
		visited:     make(map[string]map[string]bool),
		named:       make(map[string]string),
		cited:       make(map[string]bool),
		seenInclude: make(map[bibcheckInclude]bool),
		seenFinding: make(map[bibcheckSeenFinding]bool),
	}
	for _, path := range paths {
		if strings.ToLower(filepath.Ext(path)) != ".tex" {
			continue
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("resolving manuscript %q: %w", path, err)
		}
		if _, ok := w.named[abs]; !ok {
			w.named[abs] = path
		}
	}
	return w, nil
}

func (w *bibcheckIncludeWalker) walkRoot(root string) ([]bibcheckOccurrence, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolving manuscript %q: %w", root, err)
	}
	// TeX resolves \input paths against the compile directory, which is the
	// root file's directory, not the directory of the including file.
	rootDir := filepath.Dir(root)
	rootAbsDir := filepath.Dir(abs)
	visited := w.visited[rootAbsDir]
	if visited == nil {
		visited = make(map[string]bool)
		w.visited[rootAbsDir] = visited
	}
	if visited[abs] {
		return nil, nil
	}
	data, err := os.ReadFile(root)
	if err != nil {
		return nil, fmt.Errorf("reading manuscript %q: %w", root, err)
	}
	visited[abs] = true
	label := w.label(abs, root)
	stack := map[string]string{abs: label}
	return w.walk(label, abs, string(data), rootDir, rootAbsDir, visited, stack), nil
}

// label is the path a file reports under: the path as given on the command
// line for a named file, else its path relative to the root.
func (w *bibcheckIncludeWalker) label(abs, display string) string {
	if name, ok := w.named[abs]; ok {
		return name
	}
	return display
}

func (w *bibcheckIncludeWalker) walk(path, pathAbs, content, rootDir, rootAbsDir string, visited map[string]bool, stack map[string]string) []bibcheckOccurrence {
	var out []bibcheckOccurrence
	cite := !w.cited[pathAbs]
	w.cited[pathAbs] = true
	citationStart, citationLine := 0, 0
	appendCitations := func(end int) {
		if cite && w.sourceOrder {
			occurrences := parseLatexCitationOccurrences(path, content[citationStart:end])
			for i := range occurrences {
				occurrences[i].Line += citationLine
			}
			out = append(out, occurrences...)
			citationLine += strings.Count(content[citationStart:end], "\n")
			citationStart = end
		}
	}
	if cite && !w.sourceOrder {
		out = parseLatexCitationOccurrences(path, content)
	}
	for _, inc := range latexIncludeTargets(content) {
		appendCitations(inc.offset)
		target := strings.TrimSpace(inc.path)
		if filepath.Ext(target) == "" {
			target += ".tex"
		}
		abs := filepath.Clean(target)
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(rootAbsDir, target)
		}
		display := abs
		if rel, err := filepath.Rel(rootAbsDir, abs); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			display = filepath.Join(rootDir, rel)
		}
		display = w.label(abs, display)
		if from, onStack := stack[abs]; onStack {
			w.addFinding("include_cycle", fmt.Sprintf("Include cycle %s -> %s", path, from), path, inc.line, from)
			continue
		}
		if visited[abs] {
			continue
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			w.addFinding("include_missing", fmt.Sprintf("Missing include %s", display), path, inc.line, display)
			continue
		}
		visited[abs] = true
		if include := (bibcheckInclude{File: display, IncludedFrom: path, Line: inc.line}); !w.seenInclude[include] {
			w.seenInclude[include] = true
			w.includes = append(w.includes, include)
		}
		stack[abs] = display
		out = append(out, w.walk(display, abs, string(data), rootDir, rootAbsDir, visited, stack)...)
		delete(stack, abs)
	}
	appendCitations(len(content))
	return out
}

// addFinding records an include error once, however many root traversals
// reach the same file and line.
func (w *bibcheckIncludeWalker) addFinding(kind, title, file string, line int, target string) {
	key := bibcheckSeenFinding{kind: kind, file: file, target: target, line: line}
	if w.seenFinding[key] {
		return
	}
	w.seenFinding[key] = true
	w.findings = append(w.findings, bibcheckIncludeFinding(kind, title, file, line, target))
}

func bibcheckIncludeFinding(kind, title, file string, line int, target string) Finding {
	return Finding{
		Kind:     kind,
		Severity: sevHigh,
		Title:    title,
		Evidence: map[string]any{
			"file":      file,
			"line":      line,
			"file_line": bibcheckLocationString(bibcheckLocation{File: file, Line: line}),
			"include":   target,
		},
		RecommendedAction: &RecommendedAction{Text: "Fix the \\input/\\include path in the manuscript"},
	}
}

type latexInclude struct {
	path   string
	line   int
	offset int
}

// Environments whose body TeX reads as literal text: an \input inside one is
// an example, not an include.
var latexLiteralBeginRE = regexp.MustCompile(`\\begin\s*\{(verbatim\*?|Verbatim\*?|lstlisting|minted|comment)\}`)

// latexIncludeTargets finds braced \input/\include commands outside TeX
// comments and literal environments. It matches over the whole cleaned
// content, so a command split from its braced argument by a newline or a
// comment is still found, at the line of the command.
func latexIncludeTargets(content string) []latexInclude {
	text := latexIncludeSearchText(content)
	lines := lineStartOffsets(text)
	var out []latexInclude
	for _, m := range latexIncludeRE.FindAllStringSubmatchIndex(text, -1) {
		out = append(out, latexInclude{path: text[m[2]:m[3]], line: lineNumberForOffset(lines, m[0]), offset: m[0]})
	}
	return out
}

// latexIncludeSearchText masks comments and literal environment bodies from
// content, preserving byte offsets and line numbers.
func latexIncludeSearchText(content string) string {
	var b strings.Builder
	b.Grow(len(content))
	literalEnd := ""
	mask := func(n int) {
		for range n {
			b.WriteByte(' ')
		}
	}
	for i, line := range strings.Split(content, "\n") {
		if i > 0 {
			b.WriteByte('\n')
		}
		for line != "" {
			if literalEnd != "" {
				end := strings.Index(line, literalEnd)
				if end < 0 {
					mask(len(line))
					break
				}
				mask(end + len(literalEnd))
				line = line[end+len(literalEnd):]
				literalEnd = ""
				continue
			}
			code := stripTexComment(line)
			loc := latexLiteralBeginRE.FindStringSubmatchIndex(code)
			if loc == nil {
				b.WriteString(code)
				mask(len(line) - len(code))
				break
			}
			b.WriteString(code[:loc[0]])
			literalEnd = `\end{` + code[loc[2]:loc[3]] + `}`
			mask(loc[1] - loc[0])
			// code is a prefix of line, so the offsets carry over; a % after
			// the \begin is literal text, not a comment.
			line = line[loc[1]:]
		}
	}
	return b.String()
}

func stripTexComment(line string) string {
	for i := range len(line) {
		if line[i] != '%' {
			continue
		}
		backslashes := 0
		for j := i - 1; j >= 0 && line[j] == '\\'; j-- {
			backslashes++
		}
		if backslashes%2 == 0 {
			return line[:i]
		}
	}
	return line
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
			b.WriteString(line[i:])
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

// buildBibcheckKeyResults resolves each cited key against the library
// inventory. A key that resolves to nothing is where this command is read
// most and used to answer least: bibcheck exists to catch a broken citation
// before a LaTeX build fails, and a key that is one character off is the
// commonest such break. So an unknown key also carries the closest keys in
// the library, ranked; the near-key data was already in the store.
func buildBibcheckKeyResults(order []string, counts map[string]int, byCiteKey map[string][]citekeyItem) ([]bibcheckKeyResult, bibcheckSummary) {
	keys := make([]bibcheckKeyResult, 0, len(order))
	var summary bibcheckSummary
	// Built at most once per call, and only when a key is actually unknown: a
	// manuscript whose citations all resolve must not pay for a walk over the
	// whole library, and this runs once per file as well as once overall.
	candidates := sync.OnceValue(func() []citeKeyCandidate {
		return bibcheckCiteKeyCandidates(byCiteKey)
	})
	for _, key := range order {
		matches := byCiteKey[key]
		result := bibcheckKeyResult{
			CiteKey:     key,
			Occurrences: counts[key],
		}
		switch len(matches) {
		case 0:
			result.Status = "unknown"
			result.Suggestions = rankNearCiteKeys(key, candidates(), nearCiteKeyMatchLimit)
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

// manuscriptCitationResolution is the outcome of resolving manuscript
// citation occurrences to library items: the unambiguous item keys in
// first-citation order, and every cited key that resolved to zero or several
// items, with the file and line of its first citation.
type manuscriptCitationResolution struct {
	ItemKeys   []string
	Unresolved []manuscriptUnresolvedCitation
}

type manuscriptUnresolvedCitation struct {
	Key      bibcheckKeyResult
	Location bibcheckLocation
}

// resolveManuscriptCitations resolves occurrences with the same unambiguous
// citekey rules bibcheck reports: a key held by exactly one item resolves; a
// key held by none is unknown; a key held by several is ambiguous.
func resolveManuscriptCitations(occurrences []bibcheckOccurrence, items []citekeyItem) manuscriptCitationResolution {
	byCiteKey := bibcheckItemsByCiteKey(items)
	order, counts, locationsByCiteKey, _ := summarizeBibcheckOccurrences(occurrences)
	keys, _ := buildBibcheckKeyResults(order, counts, byCiteKey)
	var out manuscriptCitationResolution
	seen := make(map[string]bool, len(keys))
	for _, key := range keys {
		if key.Status != "ok" {
			var loc bibcheckLocation
			if locations := locationsByCiteKey[key.CiteKey]; len(locations) > 0 {
				loc = locations[0]
			}
			out.Unresolved = append(out.Unresolved, manuscriptUnresolvedCitation{Key: key, Location: loc})
			continue
		}
		if !seen[key.ItemKey] {
			seen[key.ItemKey] = true
			out.ItemKeys = append(out.ItemKeys, key.ItemKey)
		}
	}
	return out
}

// parseManuscriptFiles parses each manuscript in order with the bibcheck
// parser, concatenating occurrences so first-citation order spans files.
// With followIncludes, .tex files are walked through the bibcheck include
// traversal, and its missing-include and cycle findings are returned.
func parseManuscriptFiles(paths []string, followIncludes bool) ([]bibcheckOccurrence, []Finding, error) {
	var tree *bibcheckIncludeWalker
	if followIncludes {
		walker, err := newBibcheckIncludeWalker(paths)
		if err != nil {
			return nil, nil, err
		}
		tree = walker
		tree.sourceOrder = true
	}
	var occurrences []bibcheckOccurrence
	for _, path := range paths {
		if tree != nil && strings.ToLower(filepath.Ext(path)) == ".tex" {
			parsed, err := tree.walkRoot(path)
			if err != nil {
				return nil, nil, err
			}
			occurrences = append(occurrences, parsed...)
			continue
		}
		parsed, _, err := parseManuscriptCitationOccurrences(path)
		if err != nil {
			return nil, nil, err
		}
		occurrences = append(occurrences, parsed...)
	}
	if tree == nil {
		return occurrences, nil, nil
	}
	return occurrences, tree.findings, nil
}

// bibcheckCiteKeyCandidates flattens the citekey inventory to one candidate
// per key. Where a key is held by several items the first is used, which is
// deterministic because bibcheckItemsByCiteKey has already sorted them: a
// suggestion names the key to type, and an ambiguous key is a separate
// problem this command already reports on its own row.
func bibcheckCiteKeyCandidates(byCiteKey map[string][]citekeyItem) []citeKeyCandidate {
	candidates := make([]citeKeyCandidate, 0, len(byCiteKey))
	for citeKey, items := range byCiteKey {
		if len(items) == 0 {
			continue
		}
		candidates = append(candidates, citeKeyCandidate{
			CiteKey: citeKey,
			ItemKey: items[0].Key,
			Title:   items[0].Title,
		})
	}
	return candidates
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
	incompleteAction := &RecommendedAction{Command: "zotio items enrich --missing-citation --keys-from -"}
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
				Autofixable:       true,
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
		fmt.Fprintf(out, "Manuscript: %s (%s)\n", sanitizeForTerminal(report.Manuscript), report.Format)
	} else {
		fmt.Fprintf(out, "Manuscripts: %d files\n", len(report.Files))
	}
	fmt.Fprintf(out, "Summary: %d ok, %d unknown, %d ambiguous, %d incomplete (%d cited keys)\n\n", report.Summary.OK, report.Summary.Unknown, report.Summary.Ambiguous, report.Summary.Incomplete, report.Summary.Total)
	for _, file := range report.Files {
		fmt.Fprintf(out, "  %s (%s): %d ok, %d unknown, %d ambiguous, %d incomplete (%d cited keys)\n", sanitizeForTerminal(file.File), file.Format, file.Summary.OK, file.Summary.Unknown, file.Summary.Ambiguous, file.Summary.Incomplete, file.Summary.Total)
	}
	if len(report.Files) > 0 {
		fmt.Fprintln(out)
	}
	// An include finding can stand without any cited key, so "no keys" is
	// not the end of the report.
	if len(report.Keys) == 0 {
		fmt.Fprintln(out, "No citation keys found.")
	} else {
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
		if err := printBibcheckKeySuggestions(out, report.Keys); err != nil {
			return err
		}
	}
	if len(report.Findings) == 0 {
		return nil
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "Findings:")
	for _, finding := range report.Findings {
		// Prose, not a table: sanitized so a manuscript cannot forge a
		// finding line or recolour the terminal, and never truncated,
		// because a shortened key or path is a wrong report. latexCitationRE
		// captures everything between the braces, so a \cite{} argument
		// reaches here with whatever bytes the file holds; the path comes
		// from the filesystem. The key table above gets the same treatment
		// through flags.printTable (root.go).
		citeKey, _ := finding.Evidence["citekey"].(string)
		citeKey = sanitizeForTerminal(citeKey)
		locationText := sanitizeForTerminal(formatBibcheckLocations(finding.Evidence["locations"]))
		switch finding.Kind {
		case "undefined_citekey":
			fmt.Fprintf(out, "  undefined_citekey high %s — %s\n", citeKey, locationText)
		case "include_missing", "include_cycle":
			fileLine, _ := finding.Evidence["file_line"].(string)
			fmt.Fprintf(out, "  %s high %s — %s\n", finding.Kind, sanitizeForTerminal(finding.Title), sanitizeForTerminal(fileLine))
		case "incomplete_citation":
			missing, _ := finding.Evidence["missing"].(string)
			if missing != "" {
				fmt.Fprintf(out, "  incomplete_citation high %s (%s) missing %s — %s\n", citeKey, sanitizeForTerminal(finding.ItemKey), missing, locationText)
			} else {
				fmt.Fprintf(out, "  incomplete_citation high %s (%s) — %s\n", citeKey, sanitizeForTerminal(finding.ItemKey), locationText)
			}
		}
	}
	return nil
}

// printBibcheckKeySuggestions renders the closest library keys for the keys
// that resolved to nothing. It is a block of its own rather than a cell on
// the unknown row, because the table's ITEM and TITLE columns mean "the
// library entry this citation resolves to": a guess printed there would
// report a broken citation as resolved. The heading says outright that these
// are not matches, and the row keeps the cited key beside the suggestion so
// the reader can see the one character that differs.
func printBibcheckKeySuggestions(out io.Writer, keys []bibcheckKeyResult) error {
	printed := 0
	tw := newTabWriter(out)
	for _, key := range keys {
		if key.Status != "unknown" {
			continue
		}
		for _, suggestion := range key.Suggestions {
			if printed == 0 {
				fmt.Fprint(out, "\nUnknown keys — closest keys in your library (NOT matches; confirm before editing):\n")
				fmt.Fprintln(tw, "CITED\tSUGGESTION\tITEM\tTITLE\tSCORE")
			}
			// Every cell here is text zotio did not write: the cited key came
			// out of the manuscript, the rest out of the library. advisoryCell
			// (items_find.go) is the same treatment flags.printTable gives the
			// key table above, plus the tab this tabwriter reads as a column
			// break.
			title := advisoryCell(suggestion.Title)
			if title == "" {
				title = nearMatchMissingField
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%.2f\n", advisoryCell(key.CiteKey), advisoryCell(suggestion.CiteKey), advisoryCell(suggestion.ItemKey), title, suggestion.Score)
			printed++
		}
	}
	if printed == 0 {
		return nil
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprint(out, "Fix the citation in the manuscript, or pin that key in Zotero. To read one: zotio items get <ITEM>\n")
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
