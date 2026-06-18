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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"zotero-pp-cli/internal/config"
	"zotero-pp-cli/internal/store"
)

const (
	vaultAnnBegin = "<!-- zotero-pp-cli:annotations (auto-generated; edits here are overwritten on sync) -->"
	vaultAnnEnd   = "<!-- /zotero-pp-cli:annotations -->"

	// PATCH(glean 15e0): managed-content fences (title/abstract) are swapped
	// wholesale each sync; the notes fence delimits the user-owned region that
	// commit 3 will push to a Zotero child note. Markers are matched as whole
	// trimmed lines; keep them stable — they are persisted inside user files.
	vaultTitleBegin    = "<!-- zotero-pp-cli:title -->"
	vaultTitleEnd      = "<!-- /zotero-pp-cli:title -->"
	vaultAbstractBegin = "<!-- zotero-pp-cli:abstract -->"
	vaultAbstractEnd   = "<!-- /zotero-pp-cli:abstract -->"
	vaultNotesBegin    = "<!-- zotero-pp-cli:notes-begin -->"
	vaultNotesEnd      = "<!-- zotero-pp-cli:notes-end -->"
)

// vaultMeta is the per-item data rendered into a note.
type vaultMeta struct {
	Key             string
	CiteKey         string
	Title           string
	Authors         []string
	Year            string
	ItemType        string
	DOI             string
	URL             string
	Abstract        string
	Collections     []string
	Library         string // PATCH(glean 61a2a8a9): synthetic ids. API library segment, e.g. "users/99999" or "groups/123".
	CollectionNames []string
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
	Status  string `json:"status"` // created | updated | unchanged | file_busy | needs_notes_boundary
	Note    string `json:"note,omitempty"`
}

func newVaultCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "vault",
		Short: "Sync the library into an Obsidian/Logseq Markdown vault",
	}
	cmd.AddCommand(newVaultSyncCmd(flags))
	cmd.AddCommand(newVaultPushCmd(flags)) // PATCH(glean 15e0): Obsidian -> Zotero write-back
	cmd.AddCommand(newVaultConflictsCmd(flags))
	cmd.AddCommand(newVaultResolveCmd(flags))
	cmd.AddCommand(newVaultPullCmd(flags))
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
		Use:   "sync [--out <dir>]",
		Short: "Create/update one Markdown note per item in a vault (idempotent)",
		Long: `Materialize literature notes into an Obsidian or Logseq vault from the local
store (run 'sync' first). Notes are named from the citation key, carry zotero://
backlinks, and embed current annotations in a managed block.

Re-running is idempotent and non-destructive: only the managed frontmatter keys
and the fenced annotations block change; your prose and other frontmatter keys
are preserved. Use --dry-run to preview create/update/unchanged without writing.`,
		Example: `  zotero-pp-cli vault sync                 # uses [vault] root/notes_dir from config
  zotero-pp-cli vault sync --out ~/vault/refs
  zotero-pp-cli vault sync --out ~/vault/refs --collection ABCD1234 --dry-run`,
		Annotations: map[string]string{"mcp:read-only": "false"},
		RunE: func(cmd *cobra.Command, args []string) error {
			outDir := strings.TrimSpace(flagOut)
			format := strings.ToLower(strings.TrimSpace(flagFormat))
			// PATCH(glean 15e0): fall back to [vault] config so --out/--format are
			// optional once configured; explicit flags always win.
			if vc := vaultConfig(flags); vc != nil {
				if outDir == "" {
					outDir = vaultResolveOut(vc)
				}
				if !cmd.Flags().Changed("format") && strings.TrimSpace(vc.Format) != "" {
					format = strings.ToLower(strings.TrimSpace(vc.Format))
				}
			}
			if format == "" {
				format = "obsidian"
			}
			if format != "obsidian" && format != "logseq" {
				return fmt.Errorf("invalid format %q: must be obsidian or logseq", format)
			}
			if outDir == "" {
				return fmt.Errorf("no output directory: pass --out <dir> or set [vault].root in config")
			}
			flagOut = outDir

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

			// PATCH(glean perf-audit rj6r/mhib): select literature items first,
			// then batch-load annotations for all of them in one query (was one
			// AnnotationsForItem call per item), and create the vault dir once
			// instead of inside the per-note loop.
			metas := make([]vaultMeta, 0, len(items))
			keys := make([]string, 0, len(items))
			libraryID := vaultLibraryID(flags)
			collNames := loadCollectionNames(rawDB)
			for _, raw := range items {
				meta := vaultItemMeta(raw)
				if !isRegularLiteratureItem(meta.ItemType) {
					continue
				}
				meta.Library = libraryID
				meta.CollectionNames = resolveCollectionNames(meta.Collections, collNames)
				metas = append(metas, meta)
				keys = append(keys, meta.Key)
			}

			annByKey, err := rawDB.AnnotationsForItems(keys)
			if err != nil {
				return fmt.Errorf("querying annotations: %w", err)
			}

			if !flags.dryRun {
				if err := os.MkdirAll(flagOut, 0o755); err != nil {
					return fmt.Errorf("creating vault dir: %w", err)
				}
			}

			// PATCH(glean 15e0): index existing managed notes by Zotero key so a
			// re-sync updates the same file even when the citation key (and thus
			// the default filename) changed, and new notes avoid colliding with an
			// existing managed or foreign file.
			idx := scanVaultIndex(flagOut)
			claimed := make(map[string]bool, len(metas))
			results := make([]vaultSyncResult, 0, len(metas))
			for _, meta := range metas {
				anns := annotationSummariesSorted(annByKey[meta.Key])
				filename := resolveNoteFilename(meta, flagOut, idx, claimed)
				res, werr := syncVaultNote(meta, anns, format, flagOut, filename, flags.dryRun)
				if werr != nil {
					return werr
				}
				results = append(results, res)
			}

			return printVaultSyncReport(cmd, results, flagOut, format, flags)
		},
	}

	cmd.Flags().StringVar(&flagOut, "out", "", "Vault directory (overrides [vault].root + notes_dir from config)")
	cmd.Flags().StringVar(&flagFormat, "format", "obsidian", "Note format: obsidian or logseq")
	cmd.Flags().StringVar(&flagCollection, "collection", "", "Only sync items in this collection key")
	cmd.Flags().StringVar(&flagTag, "tag", "", "Only sync items with this tag")
	cmd.Flags().StringVar(&flagItemType, "item-type", "", "Only sync items of this type")
	cmd.Flags().IntVar(&flagLimit, "limit", 0, "Maximum items to sync (0 = all)")

	return cmd
}

// syncVaultNote writes (or previews) a single item's note and reports the
// resulting status. PATCH(glean 15e0): a genuine read error is no longer
// swallowed (the old `existing, _ := os.ReadFile` treated a permission/IO
// failure as a fresh create and would truncate the file); writes go through an
// atomic temp-file + rename that refuses to clobber a concurrent Obsidian/iCloud
// edit, re-merging once before reporting file_busy.
func syncVaultNote(meta vaultMeta, anns []annotationSummary, format, outDir, filename string, dryRun bool) (vaultSyncResult, error) {
	annBlock := renderAnnotationBlock(anns)
	path := filepath.Join(outDir, filename)

	existing, readErr := readVaultFile(path)
	if readErr != nil {
		return vaultSyncResult{Key: meta.Key, CiteKey: meta.CiteKey, File: filename, Status: "error", Note: readErr.Error()}, nil
	}

	content, boundary := buildVaultNote(existing, meta, annBlock, format)
	result := vaultSyncResult{Key: meta.Key, CiteKey: meta.CiteKey, File: filename, Status: noteStatus(existing, content)}
	if boundary {
		result.Note = "needs_notes_boundary: add a single '## Notes' heading or notes markers to enable note sync"
	}

	if dryRun || result.Status == "unchanged" {
		return result, nil
	}

	if err := atomicReplace(path, existing, []byte(content)); err != nil {
		if !errors.Is(err, errVaultFileBusy) {
			return vaultSyncResult{}, fmt.Errorf("writing %s: %w", path, err)
		}
		// The file changed under us (Obsidian/iCloud). Re-read, re-merge once.
		existing2, rErr := readVaultFile(path)
		if rErr == nil {
			content2, boundary2 := buildVaultNote(existing2, meta, annBlock, format)
			if err2 := atomicReplace(path, existing2, []byte(content2)); err2 == nil {
				result.Status = noteStatus(existing2, content2)
				if boundary2 {
					result.Note = "needs_notes_boundary: add a single '## Notes' heading or notes markers to enable note sync"
				}
				return result, nil
			}
		}
		result.Status = "file_busy"
		result.Note = "file changed during sync; left unmodified"
	}
	return result, nil
}

// noteStatus classifies the outcome of (re)building a note's content.
func noteStatus(existing []byte, content string) string {
	if len(existing) == 0 {
		return "created"
	}
	if content == string(existing) {
		return "unchanged"
	}
	return "updated"
}

// buildVaultNote renders a fresh note (no existing file) or merges managed
// regions into an existing one, returning the content and whether the user notes
// region could not be unambiguously established (needs_notes_boundary).
func buildVaultNote(existing []byte, meta vaultMeta, annBlock, format string) (string, bool) {
	if len(existing) == 0 {
		return renderVaultNote(meta, annBlock, format), false
	}
	return mergeVaultNote(string(existing), meta, annBlock, format)
}

// readVaultFile returns nil,nil when the file does not exist, and a real error
// for any other failure (permission, IO, a directory at the path). PATCH(glean 15e0).
func readVaultFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return data, nil
}

var errVaultFileBusy = errors.New("vault file changed during sync")

// atomicReplace writes newContent via a temp file + rename, but only when the
// file's current bytes still equal expected (compare-before-replace). This keeps
// a concurrent Obsidian/iCloud write from being silently clobbered. PATCH(glean 15e0).
func atomicReplace(path string, expected, newContent []byte) error {
	cur, err := readVaultFile(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(cur, expected) {
		return errVaultFileBusy
	}
	mode := os.FileMode(0o644)
	if fi, statErr := os.Stat(path); statErr == nil {
		mode = fi.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".zpp-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(newContent); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// vaultIndex maps a Zotero item key to the basename of its existing managed
// note (and the reverse) so re-syncs update the same file and new notes avoid
// collisions. PATCH(glean 15e0).
type vaultIndex struct {
	byKey  map[string]string
	byFile map[string]string
}

func scanVaultIndex(outDir string) vaultIndex {
	idx := vaultIndex{byKey: map[string]string{}, byFile: map[string]string{}}
	entries, err := os.ReadDir(outDir)
	if err != nil {
		return idx
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(outDir, e.Name()))
		if err != nil {
			continue
		}
		// PATCH(glean 15e0): prefer the explicit identity key, but fall back to the
		// item key embedded in the zotero:// select link so notes created before
		// zotero_key existed are still recognized as managed (updated in place,
		// gaining zotero_key) instead of being duplicated as a "foreign" file.
		key := frontmatterKeyValue(string(data), "zotero_key")
		if key == "" {
			key = keyFromZoteroSelect(frontmatterKeyValue(string(data), "zotero"))
		}
		if key == "" {
			continue
		}
		idx.byFile[e.Name()] = key
		if _, dup := idx.byKey[key]; !dup {
			idx.byKey[key] = e.Name() // ReadDir is sorted: first file wins, deterministically
		}
	}
	return idx
}

// frontmatterKeyValue returns the unquoted value of a top-level frontmatter key.
func frontmatterKeyValue(content, key string) string {
	fmLines, _, has := splitObsidianFrontmatter(content)
	if !has {
		return ""
	}
	prefix := key + ":"
	for _, ln := range fmLines {
		if strings.HasPrefix(ln, prefix) {
			return strings.Trim(strings.TrimSpace(ln[len(prefix):]), `"'`)
		}
	}
	return ""
}

// keyFromZoteroSelect extracts the item key from a zotero://select/.../items/<KEY>
// link (ignoring any trailing query/fragment), or "" when absent.
func keyFromZoteroSelect(link string) string {
	const marker = "/items/"
	i := strings.LastIndex(link, marker)
	if i < 0 {
		return ""
	}
	key := link[i+len(marker):]
	if j := strings.IndexAny(key, "?#/\"' "); j >= 0 {
		key = key[:j]
	}
	return key
}

// resolveNoteFilename picks the target filename for an item: the existing
// managed note for its key when present, otherwise the citation-key filename,
// disambiguated when that name is taken by a different item or a foreign file.
func resolveNoteFilename(meta vaultMeta, outDir string, idx vaultIndex, claimed map[string]bool) string {
	if fn, ok := idx.byKey[meta.Key]; ok {
		claimed[fn] = true
		return fn
	}
	fn := vaultFilename(meta)
	if filenameTaken(fn, meta.Key, outDir, idx, claimed) {
		fn = sanitizeVaultFilename(strings.TrimSuffix(fn, ".md")+"--"+meta.Key) + ".md"
	}
	claimed[fn] = true
	return fn
}

func filenameTaken(fn, key, outDir string, idx vaultIndex, claimed map[string]bool) bool {
	if claimed[fn] {
		return true
	}
	if owner, ok := idx.byFile[fn]; ok {
		return owner != key
	}
	if _, err := os.Stat(filepath.Join(outDir, fn)); err == nil {
		return true // an unmanaged/foreign file already holds this name
	}
	return false
}

// vaultLibraryID returns the API library segment for identity frontmatter
// ("groups/<id>" or "users/<id>"), resolved locally with no network call. Empty
// when the personal user ID has not been cached yet.
func vaultLibraryID(flags *rootFlags) string {
	if activeGroupID != "" {
		return "groups/" + activeGroupID
	}
	if cfg, err := config.Load(flags.configPath); err == nil && cfg.UserID != "" {
		return "users/" + cfg.UserID
	}
	return ""
}

// --- managed/user content blocks ---

func managedTitleBlock(meta vaultMeta) string {
	title := meta.Title
	if title == "" {
		title = meta.CiteKey
	}
	return vaultTitleBegin + "\n# " + title + "\n" + vaultTitleEnd
}

func managedAbstractBlock(meta vaultMeta) string {
	abstract := meta.Abstract
	if abstract == "" {
		abstract = "(no abstract)"
	}
	var b strings.Builder
	b.WriteString(vaultAbstractBegin)
	b.WriteString("\n> [!abstract]\n")
	for _, ln := range strings.Split(abstract, "\n") {
		b.WriteString("> " + ln + "\n")
	}
	b.WriteString(vaultAbstractEnd)
	return b.String()
}

func emptyNotesRegion() string {
	return vaultNotesBegin + "\n\n" + vaultNotesEnd
}

// replaceFencedIfPresent swaps the content between begin..end markers for
// replacement, returning whether the fence was found. Unlike replaceManagedBlock
// it never injects a missing fence — legacy notes that predate a managed fence
// are left untouched rather than retrofitted.
func replaceFencedIfPresent(body, begin, end, replacement string) (string, bool) {
	start := strings.Index(body, begin)
	if start < 0 {
		return body, false
	}
	rel := strings.Index(body[start:], end)
	if rel < 0 {
		return body, false
	}
	endAbs := start + rel + len(end)
	return body[:start] + replacement + body[endAbs:], true
}

func wholeLineCount(s, marker string) int {
	n := 0
	for _, ln := range strings.Split(s, "\n") {
		if strings.TrimSpace(ln) == marker {
			n++
		}
	}
	return n
}

// ensureNotesRegion guarantees a single, well-formed user notes region. A
// well-formed existing region is left exactly as the user wrote it. A legacy
// note is migrated only when it has exactly one unambiguous "## Notes" heading;
// anything ambiguous (duplicate/partial markers, zero or multiple headings) is
// left untouched and reported via the boolean (needs_notes_boundary).
func ensureNotesRegion(body string) (string, bool) {
	begin := wholeLineCount(body, vaultNotesBegin)
	end := wholeLineCount(body, vaultNotesEnd)
	if begin == 1 && end == 1 {
		if strings.Index(body, vaultNotesBegin) < strings.Index(body, vaultNotesEnd) {
			return body, false
		}
		return body, true
	}
	if begin != 0 || end != 0 {
		return body, true
	}
	lines := strings.Split(body, "\n")
	notesAt, count := -1, 0
	for i, ln := range lines {
		if strings.TrimSpace(ln) == "## Notes" {
			count++
			notesAt = i
		}
	}
	if count != 1 {
		return body, true
	}
	head := lines[:notesAt+1]
	inner := strings.Trim(strings.Join(lines[notesAt+1:], "\n"), "\n")
	region := []string{vaultNotesBegin}
	if inner != "" {
		region = append(region, inner)
	}
	region = append(region, vaultNotesEnd)
	return strings.Join(append(head, region...), "\n") + "\n", false
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

// annotationSummariesSorted converts stored annotation payloads into summaries
// sorted by page then date added. PATCH(glean perf-audit rj6r): split out of the
// old per-item loadItemAnnotations so the caller can batch the annotation query.
func annotationSummariesSorted(rows []json.RawMessage) []annotationSummary {
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
	entries := []fmEntry{
		{"title", []string{"title: " + yamlScalar(meta.Title)}},
		{"authors", obsidianAuthorsEntry(meta.Authors)},
		{"year", []string{"year: " + meta.Year}},
		{"itemType", []string{"itemType: " + meta.ItemType}},
		{"DOI", []string{"DOI: " + meta.DOI}},
		{"url", []string{"url: " + yamlScalar(meta.URL)}},
		{"citekey", []string{"citekey: " + meta.CiteKey}},
		{"zotero_key", []string{"zotero_key: " + meta.Key}},
		{"zotero", []string{"zotero: " + yamlScalar(zoteroSelectLink(meta.Key))}},
		{"collections", obsidianListEntry("collections", meta.Collections)},
		{"collection_names", obsidianListEntry("collection_names", meta.CollectionNames)},
	}
	// PATCH(glean 15e0): zotero_key is the stable identity for re-sync lookup;
	// zotero_library scopes it and is the write target for commit 3. Library is
	// omitted when the personal user ID has not been cached yet.
	if meta.Library != "" {
		entries = append(entries, fmEntry{"zotero_library", []string{"zotero_library: " + yamlScalar(meta.Library)}})
	}
	return entries
}

func managedLogseqProps(meta vaultMeta) []fmEntry {
	props := []fmEntry{
		{"title", []string{"title:: " + meta.Title}},
		{"authors", []string{"authors:: " + strings.Join(wikilinkAuthors(meta.Authors), ", ")}},
		{"year", []string{"year:: " + meta.Year}},
		{"item-type", []string{"item-type:: " + meta.ItemType}},
		{"doi", []string{"doi:: " + meta.DOI}},
		{"url", []string{"url:: " + meta.URL}},
		{"citekey", []string{"citekey:: " + meta.CiteKey}},
		{"zotero-key", []string{"zotero-key:: " + meta.Key}},
		{"zotero", []string{"zotero:: " + zoteroSelectLink(meta.Key)}},
		{"collection-names", []string{"collection-names:: " + strings.Join(meta.CollectionNames, ", ")}},
	}
	if meta.Library != "" {
		props = append(props, fmEntry{"zotero-library", []string{"zotero-library:: " + meta.Library}})
	}
	return props
}

// vaultConfig returns the [vault] config section, or nil when unset. PATCH(glean 15e0).
func vaultConfig(flags *rootFlags) *config.VaultConfig {
	cfg, err := config.Load(flags.configPath)
	if err != nil {
		return nil
	}
	return cfg.Vault
}

// vaultResolveOut builds the output dir from [vault].root (+ notes_dir), with ~
// expansion. Empty when root is unset.
func vaultResolveOut(vc *config.VaultConfig) string {
	root := expandHome(strings.TrimSpace(vc.Root))
	if root == "" {
		return ""
	}
	if nd := strings.TrimSpace(vc.NotesDir); nd != "" {
		return filepath.Join(root, nd)
	}
	return root
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// loadCollectionNames maps collection key -> display name from the local store.
func loadCollectionNames(db *store.Store) map[string]string {
	rows, err := db.List("collections", -1)
	if err != nil {
		return nil
	}
	m := make(map[string]string, len(rows))
	for _, r := range rows {
		var o map[string]any
		if json.Unmarshal(r, &o) != nil {
			continue
		}
		key := zoteroString(o, "key")
		name := ""
		if data, ok := o["data"].(map[string]any); ok {
			name, _ = stringValue(data["name"])
		}
		if key != "" && name != "" {
			m[key] = name
		}
	}
	return m
}

// resolveCollectionNames maps collection keys to names, falling back to the key
// when the collection is not synced locally.
func resolveCollectionNames(keys []string, names map[string]string) []string {
	if len(keys) == 0 {
		return nil
	}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if n := names[k]; n != "" {
			out = append(out, n)
		} else {
			out = append(out, k)
		}
	}
	return out
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
		lines = append(lines, "  - "+yamlScalar(v))
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
	b.WriteString(managedTitleBlock(meta))
	b.WriteString("\n\n")
	b.WriteString(managedAbstractBlock(meta))
	b.WriteString("\n\n## Annotations\n\n")
	b.WriteString(annBlock)
	b.WriteString("\n\n## Notes\n")
	b.WriteString(emptyNotesRegion())
	b.WriteString("\n")
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
	b.WriteString(emptyNotesRegion())
	b.WriteString("\n")
	return b.String()
}

// --- idempotent merge (existing files) ---

func mergeVaultNote(existing string, meta vaultMeta, annBlock, format string) (string, bool) {
	if format == "logseq" {
		return mergeLogseqNote(existing, managedLogseqProps(meta), annBlock)
	}
	return mergeObsidianNote(existing, meta, managedObsidianFrontmatter(meta), annBlock)
}

func mergeObsidianNote(existing string, meta vaultMeta, managed []fmEntry, annBlock string) (string, bool) {
	fmLines, body, has := splitObsidianFrontmatter(existing)
	var entries []fmEntry
	if has {
		entries = parseFrontmatterEntries(fmLines)
	} else {
		body = existing
	}
	merged := mergeFrontmatterEntries(entries, managed)

	// Swap managed-content fences in place when present; legacy notes that
	// predate these fences are left untouched (no retrofit). Establish the user
	// notes region before the annotation swap so an append (foreign note with no
	// annotation fence) lands after the region, never inside it. PATCH(glean 15e0).
	body, _ = replaceFencedIfPresent(body, vaultTitleBegin, vaultTitleEnd, managedTitleBlock(meta))
	body, _ = replaceFencedIfPresent(body, vaultAbstractBegin, vaultAbstractEnd, managedAbstractBlock(meta))
	body, boundary := ensureNotesRegion(body)
	body = replaceManagedBlock(body, annBlock)

	var b strings.Builder
	b.WriteString("---\n")
	for _, e := range merged {
		for _, ln := range e.lines {
			b.WriteString(ln)
			b.WriteByte('\n')
		}
	}
	b.WriteString("---\n\n")
	b.WriteString(strings.TrimLeft(body, "\n"))
	return b.String(), boundary
}

func mergeLogseqNote(existing string, managed []fmEntry, annBlock string) (string, bool) {
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
	body, boundary := ensureNotesRegion(body)
	body = replaceManagedBlock(body, annBlock)
	return strings.Join(out, "\n") + "\n" + body, boundary
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
		// PATCH(glean 15e0): truncate on a UTF-8 rune boundary, not a byte
		// boundary — mapped[:120] could split a multibyte rune and yield an
		// invalid filename for non-ASCII citation keys.
		b := []byte(mapped)
		i := 120
		for i > 0 && !utf8.RuneStart(b[i]) {
			i--
		}
		mapped = strings.Trim(string(b[:i]), " .-")
	}
	if mapped == "" {
		return "untitled"
	}
	return mapped
}

func printVaultSyncReport(cmd *cobra.Command, results []vaultSyncResult, outDir, format string, flags *rootFlags) error {
	var created, updated, unchanged, issues int
	for _, r := range results {
		switch r.Status {
		case "created":
			created++
		case "updated":
			updated++
		case "unchanged":
			unchanged++
		default:
			issues++ // file_busy, error
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
			"issues":    issues,
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
	summary := fmt.Sprintf("%s %d note(s) to %s [%s]: %d created, %d updated, %d unchanged",
		verb, len(results), outDir, format, created, updated, unchanged)
	if issues > 0 {
		summary += fmt.Sprintf(", %d issue(s)", issues)
	}
	fmt.Fprintln(out, summary)
	for _, r := range results {
		if r.Status == "unchanged" && r.Note == "" {
			continue
		}
		line := fmt.Sprintf("  [%s] %s", r.Status, r.File)
		if r.Note != "" {
			line += " — " + r.Note
		}
		fmt.Fprintln(out, line)
	}
	return nil
}
