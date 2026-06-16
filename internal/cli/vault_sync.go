// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean 49r4): vault-aware Obsidian/Logseq note sync. Materializes one
// Markdown note per item into a PKM vault, keyed on the Better BibTeX / native
// citation key, with zotero:// backlinks (item select + per-annotation
// open-pdf). Re-runs are idempotent: only the managed frontmatter keys and a
// fenced annotations block are updated; user prose and any other frontmatter
// keys are preserved verbatim. Reads the local store (run `sync` first); the
// stdout note-template generator remains for one-off scripting.

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"zotero-pp-cli/internal/store"
)

const (
	vaultAnnBegin = "<!-- zotero-pp-cli:annotations (auto-generated; edits here are overwritten on sync) -->"
	vaultAnnEnd   = "<!-- /zotero-pp-cli:annotations -->"
)

// vaultMeta is the per-item data rendered into a note.
type vaultMeta struct {
	Key         string
	CiteKey     string
	Title       string
	Authors     []string
	Year        string
	ItemType    string
	DOI         string
	URL         string
	Abstract    string
	Collections []string
}

// fmEntry is one frontmatter key and its rendered line(s).
type fmEntry struct {
	key   string
	lines []string
}

type vaultSyncResult struct {
	Key     string `json:"key"`
	CiteKey string `json:"citekey,omitempty"`
	File    string `json:"file"`
	Status  string `json:"status"` // created | updated | unchanged | skipped
}

func newVaultCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "vault",
		Short: "Sync the library into an Obsidian/Logseq Markdown vault",
	}
	cmd.AddCommand(newVaultSyncCmd(flags))
	return cmd
}

func newVaultSyncCmd(flags *rootFlags) *cobra.Command {
	var (
		flagOut        string
		flagFormat     string
		flagCollection string
		flagTag        string
		flagItemType   string
		flagLimit      int
	)

	cmd := &cobra.Command{
		Use:   "sync --out <dir>",
		Short: "Create/update one Markdown note per item in a vault (idempotent)",
		Long: `Materialize literature notes into an Obsidian or Logseq vault from the local
store (run 'sync' first). Notes are named from the citation key, carry zotero://
backlinks, and embed current annotations in a managed block.

Re-running is idempotent and non-destructive: only the managed frontmatter keys
and the fenced annotations block change; your prose and other frontmatter keys
are preserved. Use --dry-run to preview create/update/unchanged without writing.`,
		Example: `  zotero-pp-cli vault sync --out ~/vault/refs
  zotero-pp-cli vault sync --out ~/vault/refs --collection ABCD1234 --dry-run
  zotero-pp-cli vault sync --out ~/vault/refs --format logseq --tag to-read`,
		Annotations: map[string]string{"mcp:read-only": "false"},
		RunE: func(cmd *cobra.Command, args []string) error {
			format := strings.ToLower(strings.TrimSpace(flagFormat))
			if format == "" {
				format = "obsidian"
			}
			if format != "obsidian" && format != "logseq" {
				return fmt.Errorf("invalid --format value %q: must be obsidian or logseq", flagFormat)
			}
			if strings.TrimSpace(flagOut) == "" {
				return fmt.Errorf("--out <dir> is required")
			}

			rawDB, err := openStoreForRead(cmd.Context(), "zotero-pp-cli")
			if err != nil {
				return fmt.Errorf("opening database: %w", err)
			}
			if rawDB == nil {
				fmt.Fprintln(cmd.OutOrStdout(), "Run 'zotero-pp-cli sync' first.")
				return nil
			}
			defer rawDB.Close()

			items, err := rawDB.QueryItems(store.ItemQuery{
				ItemType:   flagItemType,
				Tag:        flagTag,
				Collection: flagCollection,
				TopOnly:    true,
				Sort:       "title",
				Direction:  "asc",
				Limit:      flagLimit,
			})
			if err != nil {
				return fmt.Errorf("querying items: %w", err)
			}

			results := make([]vaultSyncResult, 0, len(items))
			for _, raw := range items {
				meta := vaultItemMeta(raw)
				if !isRegularLiteratureItem(meta.ItemType) {
					continue
				}
				res, werr := syncVaultNote(rawDB, meta, format, flagOut, flags.dryRun)
				if werr != nil {
					return werr
				}
				results = append(results, res)
			}

			return printVaultSyncReport(cmd, results, flagOut, format, flags)
		},
	}

	cmd.Flags().StringVar(&flagOut, "out", "", "Vault directory to write notes into (required)")
	cmd.Flags().StringVar(&flagFormat, "format", "obsidian", "Note format: obsidian or logseq")
	cmd.Flags().StringVar(&flagCollection, "collection", "", "Only sync items in this collection key")
	cmd.Flags().StringVar(&flagTag, "tag", "", "Only sync items with this tag")
	cmd.Flags().StringVar(&flagItemType, "item-type", "", "Only sync items of this type")
	cmd.Flags().IntVar(&flagLimit, "limit", 0, "Maximum items to sync (0 = all)")

	return cmd
}

// syncVaultNote writes (or previews) a single item's note and reports the
// resulting status.
func syncVaultNote(db *store.Store, meta vaultMeta, format, outDir string, dryRun bool) (vaultSyncResult, error) {
	anns := loadItemAnnotations(db, meta.Key)
	annBlock := renderAnnotationBlock(anns)
	filename := vaultFilename(meta)
	path := filepath.Join(outDir, filename)

	existing, _ := os.ReadFile(path)
	var content string
	if len(existing) == 0 {
		content = renderVaultNote(meta, annBlock, format)
	} else {
		content = mergeVaultNote(string(existing), meta, annBlock, format)
	}

	status := "created"
	if len(existing) > 0 {
		if content == string(existing) {
			status = "unchanged"
		} else {
			status = "updated"
		}
	}

	if !dryRun && status != "unchanged" {
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			return vaultSyncResult{}, fmt.Errorf("creating vault dir: %w", err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return vaultSyncResult{}, fmt.Errorf("writing %s: %w", path, err)
		}
	}

	return vaultSyncResult{Key: meta.Key, CiteKey: meta.CiteKey, File: filename, Status: status}, nil
}

// --- metadata extraction ---

func vaultItemMeta(raw json.RawMessage) vaultMeta {
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		return vaultMeta{}
	}
	data, ok := obj["data"].(map[string]any)
	if !ok {
		data = obj
	}
	key := zoteroString(obj, "key")
	if key == "" {
		key = zoteroString(data, "key")
	}
	citekey := strings.TrimSpace(jsonStringFieldFromMap(obj, "citationKey"))
	if citekey == "" {
		extra, _ := stringValue(data["extra"])
		citekey = citationKeyFromExtra(extra)
	}
	title, _ := stringValue(data["title"])
	doi, _ := stringValue(data["DOI"])
	url, _ := stringValue(data["url"])
	abstract, _ := stringValue(data["abstractNote"])
	itemType, _ := stringValue(data["itemType"])
	date, _ := stringValue(data["date"])

	return vaultMeta{
		Key:         key,
		CiteKey:     citekey,
		Title:       title,
		Authors:     noteAuthors(data["creators"]),
		Year:        yearFromDate(date),
		ItemType:    itemType,
		DOI:         doi,
		URL:         url,
		Abstract:    strings.TrimSpace(abstract),
		Collections: stringSliceFromAny(data["collections"]),
	}
}

func stringSliceFromAny(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// isRegularLiteratureItem excludes attachments, annotations, and standalone
// notes, which do not get their own literature note.
func isRegularLiteratureItem(itemType string) bool {
	switch itemType {
	case "attachment", "annotation", "note", "":
		return false
	default:
		return true
	}
}

func loadItemAnnotations(db *store.Store, key string) []annotationSummary {
	rows, err := db.AnnotationsForItem(key)
	if err != nil {
		return nil
	}
	items := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		var o map[string]any
		if json.Unmarshal(r, &o) == nil {
			items = append(items, o)
		}
	}
	anns := annotationSummariesFromItems(items)
	sort.SliceStable(anns, func(i, j int) bool {
		pi, pj := annotationPageNum(anns[i].Page), annotationPageNum(anns[j].Page)
		if pi != pj {
			return pi < pj
		}
		return anns[i].DateAdded < anns[j].DateAdded
	})
	return anns
}

func annotationPageNum(page string) int {
	page = strings.TrimSpace(page)
	if page == "" {
		return 1 << 30 // sort unpaged annotations last
	}
	n, err := strconv.Atoi(page)
	if err != nil {
		return 1 << 30
	}
	return n
}

// --- zotero:// backlinks ---

func zoteroLibrarySegment() string {
	if activeGroupID != "" {
		return "groups/" + activeGroupID
	}
	return "library"
}

func zoteroSelectLink(key string) string {
	return "zotero://select/" + zoteroLibrarySegment() + "/items/" + key
}

func zoteroOpenPDFLink(parentKey, annKey string) string {
	return "zotero://open-pdf/" + zoteroLibrarySegment() + "/items/" + parentKey + "?annotation=" + annKey
}

// --- annotation block (format-agnostic) ---

func renderAnnotationBlock(anns []annotationSummary) string {
	var b strings.Builder
	b.WriteString(vaultAnnBegin)
	b.WriteByte('\n')
	if len(anns) == 0 {
		b.WriteString("_No annotations._\n")
	} else {
		for _, a := range anns {
			b.WriteString(renderAnnotationLine(a))
		}
	}
	b.WriteString(vaultAnnEnd)
	return b.String()
}

func renderAnnotationLine(a annotationSummary) string {
	text := strings.TrimSpace(a.Text)
	comment := strings.TrimSpace(a.Comment)
	main := text
	if main == "" {
		main = comment
		comment = ""
	}
	if main == "" {
		main = "(annotation)"
	}

	var b strings.Builder
	b.WriteString("- ")
	b.WriteString(strings.ReplaceAll(main, "\n", " "))
	if a.ParentItem != "" && a.Key != "" {
		label := "link"
		if a.Page != "" {
			label = "p. " + a.Page
		}
		b.WriteString(" ([" + label + "](" + zoteroOpenPDFLink(a.ParentItem, a.Key) + "))")
	}
	b.WriteByte('\n')
	if comment != "" {
		b.WriteString("  - Note: " + strings.ReplaceAll(comment, "\n", " ") + "\n")
	}
	return b.String()
}

// --- managed frontmatter ---

func managedObsidianFrontmatter(meta vaultMeta) []fmEntry {
	return []fmEntry{
		{"title", []string{"title: " + yamlScalar(meta.Title)}},
		{"authors", obsidianAuthorsEntry(meta.Authors)},
		{"year", []string{"year: " + meta.Year}},
		{"itemType", []string{"itemType: " + meta.ItemType}},
		{"DOI", []string{"DOI: " + meta.DOI}},
		{"url", []string{"url: " + yamlScalar(meta.URL)}},
		{"citekey", []string{"citekey: " + meta.CiteKey}},
		{"zotero", []string{"zotero: " + yamlScalar(zoteroSelectLink(meta.Key))}},
		{"collections", obsidianListEntry("collections", meta.Collections)},
	}
}

func managedLogseqProps(meta vaultMeta) []fmEntry {
	return []fmEntry{
		{"title", []string{"title:: " + meta.Title}},
		{"authors", []string{"authors:: " + strings.Join(wikilinkAuthors(meta.Authors), ", ")}},
		{"year", []string{"year:: " + meta.Year}},
		{"item-type", []string{"item-type:: " + meta.ItemType}},
		{"doi", []string{"doi:: " + meta.DOI}},
		{"url", []string{"url:: " + meta.URL}},
		{"citekey", []string{"citekey:: " + meta.CiteKey}},
		{"zotero", []string{"zotero:: " + zoteroSelectLink(meta.Key)}},
	}
}

func obsidianAuthorsEntry(authors []string) []string {
	if len(authors) == 0 {
		return []string{"authors: []"}
	}
	lines := []string{"authors:"}
	for _, a := range authors {
		lines = append(lines, "  - "+strconv.Quote("[["+a+"]]"))
	}
	return lines
}

func obsidianListEntry(key string, vals []string) []string {
	if len(vals) == 0 {
		return []string{key + ": []"}
	}
	lines := []string{key + ":"}
	for _, v := range vals {
		lines = append(lines, "  - "+v)
	}
	return lines
}

// yamlScalar renders a YAML plain scalar, double-quoting when the value would
// otherwise be ambiguous (contains ':', quotes, brackets, leading/trailing
// space, etc.).
func yamlScalar(s string) string {
	if s == "" {
		return `""`
	}
	if strings.ContainsAny(s, ":#\"'[]{}|>*&!%@`") || strings.HasPrefix(s, " ") || strings.HasSuffix(s, " ") {
		return strconv.Quote(s)
	}
	return s
}

// --- note rendering (new files) ---

func renderVaultNote(meta vaultMeta, annBlock, format string) string {
	if format == "logseq" {
		return renderLogseqNote(meta, annBlock)
	}
	return renderObsidianNote(meta, annBlock)
}

func renderObsidianNote(meta vaultMeta, annBlock string) string {
	var b strings.Builder
	b.WriteString("---\n")
	for _, e := range managedObsidianFrontmatter(meta) {
		for _, ln := range e.lines {
			b.WriteString(ln)
			b.WriteByte('\n')
		}
	}
	b.WriteString("---\n\n")
	title := meta.Title
	if title == "" {
		title = meta.CiteKey
	}
	b.WriteString("# " + title + "\n\n")
	abstract := meta.Abstract
	if abstract == "" {
		abstract = "(no abstract)"
	}
	b.WriteString("> [!abstract]\n")
	for _, ln := range strings.Split(abstract, "\n") {
		b.WriteString("> " + ln + "\n")
	}
	b.WriteString("\n## Annotations\n\n")
	b.WriteString(annBlock)
	b.WriteString("\n\n## Notes\n")
	return b.String()
}

func renderLogseqNote(meta vaultMeta, annBlock string) string {
	var b strings.Builder
	for _, e := range managedLogseqProps(meta) {
		for _, ln := range e.lines {
			b.WriteString(ln)
			b.WriteByte('\n')
		}
	}
	b.WriteString("\n## Annotations\n")
	b.WriteString(annBlock)
	b.WriteString("\n\n## Notes\n")
	return b.String()
}

// --- idempotent merge (existing files) ---

func mergeVaultNote(existing string, meta vaultMeta, annBlock, format string) string {
	if format == "logseq" {
		return mergeLogseqNote(existing, managedLogseqProps(meta), annBlock)
	}
	return mergeObsidianNote(existing, managedObsidianFrontmatter(meta), annBlock)
}

func mergeObsidianNote(existing string, managed []fmEntry, annBlock string) string {
	fmLines, body, has := splitObsidianFrontmatter(existing)
	var entries []fmEntry
	if has {
		entries = parseFrontmatterEntries(fmLines)
	} else {
		body = existing
	}
	merged := mergeFrontmatterEntries(entries, managed)

	var b strings.Builder
	b.WriteString("---\n")
	for _, e := range merged {
		for _, ln := range e.lines {
			b.WriteString(ln)
			b.WriteByte('\n')
		}
	}
	b.WriteString("---\n\n")
	b.WriteString(strings.TrimLeft(replaceManagedBlock(body, annBlock), "\n"))
	return b.String()
}

func mergeLogseqNote(existing string, managed []fmEntry, annBlock string) string {
	lines := strings.Split(existing, "\n")
	managedByKey := make(map[string]fmEntry, len(managed))
	order := make([]string, 0, len(managed))
	for _, m := range managed {
		managedByKey[m.key] = m
		order = append(order, m.key)
	}
	emitted := make(map[string]bool, len(managed))

	var out []string
	i := 0
	for i < len(lines) {
		key, ok := logseqProp(lines[i])
		if !ok {
			break
		}
		if m, isManaged := managedByKey[key]; isManaged {
			out = append(out, m.lines...)
			emitted[key] = true
		} else {
			out = append(out, lines[i])
		}
		i++
	}
	for _, k := range order {
		if !emitted[k] {
			out = append(out, managedByKey[k].lines...)
		}
	}
	body := strings.Join(lines[i:], "\n")
	body = replaceManagedBlock(body, annBlock)
	return strings.Join(out, "\n") + "\n" + body
}

// replaceManagedBlock swaps the fenced annotations block for annBlock, or
// appends a new one (with heading) when no managed block exists yet.
func replaceManagedBlock(body, annBlock string) string {
	start := strings.Index(body, vaultAnnBegin)
	if start >= 0 {
		rel := strings.Index(body[start:], vaultAnnEnd)
		if rel >= 0 {
			endAbs := start + rel + len(vaultAnnEnd)
			return body[:start] + annBlock + body[endAbs:]
		}
	}
	trimmed := strings.TrimRight(body, "\n")
	if trimmed == "" {
		return "## Annotations\n\n" + annBlock + "\n"
	}
	return trimmed + "\n\n## Annotations\n\n" + annBlock + "\n"
}

func splitObsidianFrontmatter(s string) (fmLines []string, body string, has bool) {
	lines := strings.Split(s, "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], " \t") != "---" {
		return nil, s, false
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], " \t") == "---" {
			body = strings.Join(lines[i+1:], "\n")
			return lines[1:i], strings.TrimPrefix(body, "\n"), true
		}
	}
	return nil, s, false
}

func parseFrontmatterEntries(fmLines []string) []fmEntry {
	var entries []fmEntry
	for _, ln := range fmLines {
		if key, ok := yamlTopKey(ln); ok {
			entries = append(entries, fmEntry{key: key, lines: []string{ln}})
		} else if len(entries) > 0 {
			entries[len(entries)-1].lines = append(entries[len(entries)-1].lines, ln)
		} else {
			entries = append(entries, fmEntry{key: "", lines: []string{ln}})
		}
	}
	return entries
}

func mergeFrontmatterEntries(existing, managed []fmEntry) []fmEntry {
	managedByKey := make(map[string]fmEntry, len(managed))
	order := make([]string, 0, len(managed))
	for _, m := range managed {
		managedByKey[m.key] = m
		order = append(order, m.key)
	}
	emitted := make(map[string]bool, len(managed))

	var result []fmEntry
	for _, e := range existing {
		if m, ok := managedByKey[e.key]; ok && e.key != "" {
			result = append(result, m)
			emitted[e.key] = true
		} else {
			result = append(result, e)
		}
	}
	for _, k := range order {
		if !emitted[k] {
			result = append(result, managedByKey[k])
		}
	}
	return result
}

func yamlTopKey(line string) (string, bool) {
	if line == "" {
		return "", false
	}
	switch line[0] {
	case ' ', '\t', '#', '-':
		return "", false
	}
	i := strings.IndexByte(line, ':')
	if i <= 0 {
		return "", false
	}
	return line[:i], simpleKey(line[:i])
}

func logseqProp(line string) (string, bool) {
	i := strings.Index(line, ":: ")
	if i <= 0 {
		return "", false
	}
	key := line[:i]
	return key, simpleKey(key)
}

func simpleKey(key string) bool {
	for _, r := range key {
		if r == '_' || r == '-' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return key != ""
}

func vaultFilename(meta vaultMeta) string {
	base := meta.CiteKey
	if base == "" {
		base = meta.Key
	}
	return sanitizeVaultFilename(base) + ".md"
}

func sanitizeVaultFilename(s string) string {
	mapped := strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', '\n', '\t', '\r':
			return '-'
		}
		if r < 0x20 {
			return '-'
		}
		return r
	}, s)
	mapped = strings.Trim(mapped, " .-")
	if len(mapped) > 120 {
		mapped = strings.Trim(mapped[:120], " .-")
	}
	if mapped == "" {
		return "untitled"
	}
	return mapped
}

func printVaultSyncReport(cmd *cobra.Command, results []vaultSyncResult, outDir, format string, flags *rootFlags) error {
	var created, updated, unchanged int
	for _, r := range results {
		switch r.Status {
		case "created":
			created++
		case "updated":
			updated++
		case "unchanged":
			unchanged++
		}
	}

	if flags.asJSON {
		report := map[string]any{
			"out":       outDir,
			"format":    format,
			"dry_run":   flags.dryRun,
			"created":   created,
			"updated":   updated,
			"unchanged": unchanged,
			"results":   results,
		}
		data, err := json.Marshal(report)
		if err != nil {
			return err
		}
		return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), flags)
	}

	out := cmd.OutOrStdout()
	verb := "Synced"
	if flags.dryRun {
		verb = "Would sync"
	}
	fmt.Fprintf(out, "%s %d note(s) to %s [%s]: %d created, %d updated, %d unchanged\n",
		verb, len(results), outDir, format, created, updated, unchanged)
	for _, r := range results {
		if r.Status == "unchanged" {
			continue
		}
		fmt.Fprintf(out, "  [%s] %s\n", r.Status, r.File)
	}
	return nil
}
