// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"

	"zotio/internal/client"

	"github.com/spf13/cobra"
)

// exportFormatJSON is the only export format the local mirror can serve: it is
// the structured Zotero item JSON the store already holds. bibtex, ris, and
// csljson are produced by Zotero's export translators, which run inside the
// desktop app and are therefore live-only.
const exportFormatJSON = "json"

func newCollectionsExportCmd(flags *rootFlags) *cobra.Command {
	var flagFormat string
	var flagOutput string
	var flagFlat bool
	var flagLimit int

	cmd := &cobra.Command{
		Use:   "export <collectionKey>",
		Short: "Export a collection (and subcollections) as BibTeX, RIS, CSL-JSON, or item JSON",
		Long: `Recursively walks a collection and all its subcollections, then emits a
single combined export in the requested format. Use --flat to export only
the top-level collection without recursing into subcollections.

bibtex, ris, and csljson are rendered by Zotero's export translators, so they
need Zotero desktop running. --format json emits the structured item JSON the
synced mirror already holds, so it also works with Zotero closed
(--data-source local, or auto once the local API stops answering).`,
		Example: `  # Export collection as BibTeX (default)
  zotio collections export ABCD1234

  # Export as RIS to a file
  zotio collections export ABCD1234 --format ris --output refs.ris

  # Export the whole subtree's item JSON from the synced mirror, Zotero closed
  zotio collections export ABCD1234 --format json --data-source local

  # Export without descending into subcollections
  zotio collections export ABCD1234 --flat`,
		// The capability registry declares live_local_api for the default
		// (translator-rendered) route, but central preflight cannot read
		// --format, so it would refuse the offline `--format json` route too.
		// Enforcement therefore happens in refuseTranslatorExport, which emits
		// the same precondition_unmet envelope and exit code, only for the
		// formats that actually need Zotero.
		Annotations: map[string]string{"mcp:read-only": "true", preflightAnnotationKey: preflightAnnotationSkip},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			collKey := args[0]

			format := flagFormat
			if format == "" {
				format = "bibtex"
			}
			switch format {
			case "bibtex", "ris", "csljson", exportFormatJSON:
			default:
				return fmt.Errorf("unknown format %q: use bibtex, ris, csljson, or json", format)
			}

			if format == exportFormatJSON {
				return runMirrorCollectionExport(cmd, flags, collKey, flagOutput, flagFlat, flagLimit)
			}
			if flags.dataSource == "local" {
				return refuseTranslatorExport(cmd, flags, format, "--data-source local cannot reach Zotero's export translators")
			}
			return runTranslatorCollectionExport(cmd, flags, collKey, format, flagOutput, flagFlat, flagLimit)
		},
	}
	cmd.Flags().StringVar(&flagFormat, "format", "bibtex", "Export format: bibtex, ris, csljson, json (json is the offline-capable format)")
	cmd.Flags().StringVar(&flagOutput, "output", "", "Write output to file instead of stdout")
	cmd.Flags().BoolVar(&flagFlat, "flat", false, "Export only the top-level collection, skip subcollections")
	cmd.Flags().IntVar(&flagLimit, "limit", zoteroPageMax, "Items fetched per API request (max 100); the export always walks every page")

	return cmd
}

// runTranslatorCollectionExport renders bibtex/ris/csljson through Zotero's
// export translators. The read has exactly one plane, so it uses the client
// directly instead of resolveRead: an unreachable desktop becomes the same
// precondition refusal rather than a mirror dump that could not carry the
// rendering anyway.
func runTranslatorCollectionExport(cmd *cobra.Command, flags *rootFlags, collKey, format, output string, flat bool, limit int) error {
	c, err := flags.newClient()
	if err != nil {
		return err
	}
	err = writeCollectionExport(cmd, output, func(w io.Writer) error {
		return exportCollection(c, w, collKey, format, flat, limit, map[string]bool{})
	})
	if isNetworkError(err) {
		return refuseTranslatorExport(cmd, flags, format, "Zotero desktop's local API is unreachable")
	}
	return err
}

// runMirrorCollectionExport emits the structured item JSON of a collection
// subtree through resolveRead, so --data-source, provenance, and the auto
// fallback to the mirror match every other list read.
func runMirrorCollectionExport(cmd *cobra.Command, flags *rootFlags, collKey, output string, flat bool, limit int) error {
	reader := &dataSourceItemReader{ctx: cmd.Context(), flags: flags}
	if flags.dataSource != "local" {
		c, err := flags.newClient()
		if err != nil {
			return err
		}
		reader.c = c
	}
	items := make([]json.RawMessage, 0)
	if err := writeCollectionExport(cmd, output, func(w io.Writer) error {
		if err := collectCollectionItems(reader, collKey, "", flat, limit, map[string]bool{}, &items); err != nil {
			return err
		}
		return json.NewEncoder(w).Encode(items)
	}); err != nil {
		return err
	}
	printProvenance(cmd, len(items), reader.prov)
	return nil
}

// writeCollectionExport publishes an export to stdout or, under --output, to a
// file under a writer lock.
func writeCollectionExport(cmd *cobra.Command, output string, emit func(io.Writer) error) error {
	if output == "" {
		// Stdout stays unbuffered: wrapping it would delay broken-pipe
		// detection and keep fetching pages after the consumer exited.
		return emit(cmd.OutOrStdout())
	}
	lockPath, canonicalTarget, err := outputWriterLockPath(output)
	if err != nil {
		return fmt.Errorf("resolving output path: %w", err)
	}
	// The lock precedes the first source read, and covers the atomic
	// publication that keeps a mid-walk failure from replacing the
	// previous artifact with a valid-looking partial bibliography.
	return withPathWriterLock(cmd, lockPath, fmt.Sprintf("collections export to %q", canonicalTarget), func() error {
		return withAtomicOutputFile(output, exportOutputFileMode, emit)
	})
}

// refuseTranslatorExport refuses a rendered export that cannot reach Zotero,
// and names the offline route.
//
// The mirror stores Zotero's item JSON, never translator output, and the gap is
// not cosmetic: Better BibTeX citation keys live in Zotero, so a locally
// invented .bib key would compile into a manuscript and break the citation
// resolution far away from this command, without ever failing here. Refusing
// loudly (exit 9) is the honest answer.
func refuseTranslatorExport(cmd *cobra.Command, flags *rootFlags, format, detail string) error {
	remediation := append([]string{
		"Export the structured item JSON the mirror can serve: 'zotio collections export <key> --format json --data-source local'.",
	}, remediationFor(cmd.Context(), flags, preconditionLiveLocalAPI)...)
	return emitPreconditionUnmetWithRemediation(cmd.OutOrStdout(), flags, "collections export", preconditionLiveLocalAPI,
		fmt.Sprintf("--format %s is rendered by Zotero's export translators, which the synced mirror cannot produce: %s", format, detail),
		remediation)
}

// zoteroPageMax is the API's hard ceiling on `limit` for a multi-object read.
// The server silently clamps anything larger, which is exactly how a
// single-shot export truncated a large collection without reporting it.
const zoteroPageMax = 100

// collectionItemReader is the read surface the countable-array walks need. It
// is deliberately narrower than collectionExportClient: a mirror-backed read
// has no response headers to hand back, so keeping the header method out of
// this interface makes that a compile error rather than a runtime surprise.
type collectionItemReader interface {
	Get(path string, params map[string]string) (json.RawMessage, error)
}

// collectionExportClient is the read surface a rendered collection export
// needs. GetWithHeader is what makes paging a text export possible at all:
// BibTeX and RIS come back as one opaque document, so the item count has to
// come from the Total-Results header rather than from the body.
type collectionExportClient interface {
	collectionItemReader
	GetWithHeader(path string, params map[string]string, header string) (json.RawMessage, string, error)
}

// dataSourceItemReader routes enumeration reads through resolveRead, so the
// export honors --data-source local|live|auto with the same policy,
// provenance, and auto fallback as every other list read.
type dataSourceItemReader struct {
	ctx   context.Context
	c     *client.Client
	flags *rootFlags
	prov  DataProvenance
}

func (r *dataSourceItemReader) Get(path string, params map[string]string) (json.RawMessage, error) {
	ctx := r.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	resource := "collections"
	if _, _, _, itemList := parseItemListPath(path); itemList {
		resource = "items"
	}
	data, prov, err := resolveRead(ctx, r.c, r.flags, resource, true, path, params, nil)
	if err != nil {
		return nil, err
	}
	r.recordProvenance(prov)
	return data, nil
}

// recordProvenance keeps the least-fresh plane the artifact was assembled from:
// one page served by the mirror makes the whole export mirror-derived, and
// reporting it as live would overstate what the operator is holding.
func (r *dataSourceItemReader) recordProvenance(prov DataProvenance) {
	if r.prov.Source == "" || (r.prov.Source == "live" && prov.Source == "local") {
		r.prov = prov
	}
}

// exportPageSize clamps the requested page size to what the API will actually
// serve. --limit is a per-request page size, not a cap on the export.
func exportPageSize(limit int) int {
	if limit <= 0 || limit > zoteroPageMax {
		return zoteroPageMax
	}
	return limit
}

// repeatedPage reports whether the API handed back the same bytes as the
// previous page, which means it ignored `start`. Without this check a paged
// export against such a server would stream page one forever.
func repeatedPage(prev, cur []byte) bool {
	return prev != nil && bytes.Equal(prev, cur)
}

func isBlankExportPage(data []byte) bool {
	content := strings.TrimSpace(string(data))
	return content == "" || content == "[]" || content == "null"
}

func subcollectionKey(sub map[string]any) string {
	if key, _ := sub["key"].(string); key != "" {
		return key
	}
	if data, ok := sub["data"].(map[string]any); ok {
		key, _ := data["key"].(string)
		return key
	}
	return ""
}

func exportCollection(c collectionExportClient, out io.Writer, collKey, format string, flat bool, limit int, visited map[string]bool) error {
	if format == "csljson" {
		items := make([]json.RawMessage, 0)
		if err := collectCollectionItems(c, collKey, format, flat, limit, visited, &items); err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(items)
	}
	return exportTextCollection(c, out, collKey, format, flat, limit, visited, make(map[string]bool))
}

func exportTextCollection(c collectionExportClient, out io.Writer, collKey, format string, flat bool, limit int, visited, citationKeys map[string]bool) error {
	if visited[collKey] {
		return nil
	}
	visited[collKey] = true

	if err := forEachCollectionItemPage(c, collKey, format, limit, func(data json.RawMessage) error {
		if format == "bibtex" {
			data = json.RawMessage(deduplicateBibTeXCitationKeys(string(data), citationKeys))
		}
		_, err := fmt.Fprintln(out, strings.TrimSpace(string(data)))
		return err
	}); err != nil {
		return err
	}

	if flat {
		return nil
	}

	subcols, err := fetchSubcollectionKeys(c, collKey)
	if err != nil {
		return err
	}
	for _, key := range subcols {
		if err := exportTextCollection(c, out, key, format, flat, limit, visited, citationKeys); err != nil {
			return fmt.Errorf("exporting subcollection %s: %w", key, err)
		}
	}
	return nil
}

// deduplicateBibTeXCitationKeys keeps every citation key unique across a
// combined export. Zotero only coordinates keys within one translator call.
func deduplicateBibTeXCitationKeys(page string, emitted map[string]bool) string {
	var out strings.Builder
	copied, depth := 0, 0
	for i := 0; i < len(page); i++ {
		switch page[i] {
		case '\\':
			if i+1 < len(page) {
				i++
			}
		case '%':
			for i < len(page) && page[i] != '\n' {
				i++
			}
		case '{':
			depth++
		case '}':
			if depth > 0 {
				depth--
			}
		case '@':
			if depth != 0 {
				continue
			}

			typeStart := i + 1
			typeEnd := typeStart
			for typeEnd < len(page) && isBibTeXEntryTypeByte(page[typeEnd]) {
				typeEnd++
			}
			if typeEnd == typeStart {
				continue
			}
			brace := typeEnd
			for brace < len(page) && isBibTeXWhitespace(page[brace]) {
				brace++
			}
			if brace == len(page) || page[brace] != '{' {
				continue
			}
			if typeName := page[typeStart:typeEnd]; strings.EqualFold(typeName, "comment") || strings.EqualFold(typeName, "preamble") || strings.EqualFold(typeName, "string") {
				continue
			}
			comma := bibTeXCitationKeyEnd(page, brace+1)
			if comma < 0 {
				continue
			}

			key := page[brace+1 : comma]
			if key == "" {
				continue
			}
			out.WriteString(page[copied : brace+1])
			if emitted[key] {
				for suffix := 1; ; suffix++ {
					candidate := fmt.Sprintf("%s-%d", key, suffix)
					if !emitted[candidate] {
						key = candidate
						break
					}
				}
			}
			emitted[key] = true
			out.WriteString(key)
			copied = comma
		}
	}
	out.WriteString(page[copied:])
	return out.String()
}

func isBibTeXEntryTypeByte(b byte) bool {
	return b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '_' || b == '-'
}

func isBibTeXWhitespace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r' || b == '\n'
}

func bibTeXCitationKeyEnd(page string, start int) int {
	for i := start; i < len(page); i++ {
		switch page[i] {
		case '\\':
			if i+1 < len(page) {
				i++
			}
		case ',':
			return i
		case '{', '}', '\r', '\n', '%':
			return -1
		}
	}
	return -1
}

// forEachCollectionItemPage walks every page of a collection's items, handing
// each non-blank page to emit.
//
// An export format returns an opaque document rather than a countable array,
// so Total-Results bounds the walk when the API sends it. Without that header,
// keys at the next offset establish whether another page exists. Keys also
// verify that a server honored start, because opaque export bytes can repeat
// for distinct, unrenderable items.
func forEachCollectionItemPage(c collectionExportClient, collKey, format string, limit int, emit func(json.RawMessage) error) error {
	pageSize := exportPageSize(limit)
	// url-encode path param to prevent segment injection.
	path := "/collections/" + url.PathEscape(collKey) + "/items"
	seenPageKeys := make(map[string]bool)
	for start := 0; ; start += pageSize {
		data, total, err := c.GetWithHeader(path, map[string]string{
			"format": format,
			"limit":  strconv.Itoa(pageSize),
			"start":  strconv.Itoa(start),
		}, "Total-Results")
		if err != nil {
			return fmt.Errorf("fetching items for collection %s: %w", collKey, err)
		}

		next := start + pageSize
		if count, cerr := strconv.Atoi(strings.TrimSpace(total)); cerr == nil && count >= 0 {
			if count == 0 {
				return nil
			}
			if start >= count {
				return fmt.Errorf("pagination for collection %s exceeded Total-Results %d", collKey, count)
			}
			key, found, err := collectionKeyAt(c, collKey, start)
			if err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("pagination for collection %s ended before Total-Results %d", collKey, count)
			}
			if seenPageKeys[key] {
				return fmt.Errorf("pagination for collection %s ignored start %d", collKey, start)
			}
			seenPageKeys[key] = true
			if !isBlankExportPage(data) {
				if err := emit(data); err != nil {
					return err
				}
			}
			if next >= count {
				return nil
			}
			continue
		}

		if !isBlankExportPage(data) {
			if err := emit(data); err != nil {
				return err
			}
		}
		key, found, err := collectionKeyAt(c, collKey, next)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		if start == 0 {
			firstKey, firstFound, err := collectionKeyAt(c, collKey, start)
			if err != nil {
				return err
			}
			if !firstFound {
				return fmt.Errorf("pagination for collection %s returned a key at %d but not %d", collKey, next, start)
			}
			seenPageKeys[firstKey] = true
		}
		if seenPageKeys[key] {
			return fmt.Errorf("pagination for collection %s ignored start %d", collKey, next)
		}
		seenPageKeys[key] = true
	}
}

// collectionKeyAt returns the first item key at offset start. Key responses
// remain countable even when an export translator omits an item entirely.
func collectionKeyAt(c collectionExportClient, collKey string, start int) (string, bool, error) {
	// url-encode path param to prevent segment injection.
	data, err := c.Get("/collections/"+url.PathEscape(collKey)+"/items", map[string]string{
		"format": "keys",
		"limit":  "1",
		"start":  strconv.Itoa(start),
	})
	if err != nil {
		return "", false, fmt.Errorf("checking for item key in collection %s: %w", collKey, err)
	}
	key := strings.TrimSpace(string(data))
	if key == "" || key == "[]" || key == "null" {
		return "", false, nil
	}
	if newline := strings.IndexByte(key, '\n'); newline >= 0 {
		key = strings.TrimSpace(key[:newline])
	}
	return key, key != "", nil
}

// collectCollectionItems walks a collection subtree's countable-array formats:
// csljson (a translator array) and, with an empty format, the structured item
// JSON both the live API and the synced mirror serve. An empty format is what
// lets this walk run through resolveRead — the local item planner deliberately
// refuses to serve a `format` it cannot render (see resolveLocalItemList).
func collectCollectionItems(c collectionItemReader, collKey, format string, flat bool, limit int, visited map[string]bool, items *[]json.RawMessage) error {
	if visited[collKey] {
		return nil
	}
	visited[collKey] = true

	pageSize := exportPageSize(limit)
	// url-encode path param to prevent segment injection.
	path := "/collections/" + url.PathEscape(collKey) + "/items"
	var prev []byte
	for start := 0; ; start += pageSize {
		params := map[string]string{
			"limit": strconv.Itoa(pageSize),
			"start": strconv.Itoa(start),
		}
		if format != "" {
			params["format"] = format
		}
		data, err := c.Get(path, params)
		if err != nil {
			return fmt.Errorf("fetching items for collection %s: %w", collKey, err)
		}
		if repeatedPage(prev, data) {
			return fmt.Errorf("pagination for collection %s ignored start %d", collKey, start)
		}
		prev = data

		var page []json.RawMessage
		if err := json.Unmarshal(data, &page); err != nil {
			return fmt.Errorf("decoding items for collection %s: %w", collKey, err)
		}
		*items = append(*items, page...)
		// These formats are countable arrays, so a short page is the last page
		// and no response header is needed to know it.
		if len(page) < pageSize {
			break
		}
	}

	if flat {
		return nil
	}
	subcols, err := fetchSubcollectionKeys(c, collKey)
	if err != nil {
		return err
	}
	for _, key := range subcols {
		if err := collectCollectionItems(c, key, format, flat, limit, visited, items); err != nil {
			return fmt.Errorf("exporting subcollection %s: %w", key, err)
		}
	}
	return nil
}

// fetchSubcollectionKeys returns every child collection key. The subcollection
// read was previously unpaginated, so the API's default page size silently
// dropped whole subtrees from a broad collection's export.
func fetchSubcollectionKeys(c collectionItemReader, collKey string) ([]string, error) {
	// url-encode path param to prevent segment injection.
	path := "/collections/" + url.PathEscape(collKey) + "/collections"
	var keys []string
	var prev []byte
	for start := 0; ; start += zoteroPageMax {
		data, err := c.Get(path, map[string]string{
			"limit": strconv.Itoa(zoteroPageMax),
			"start": strconv.Itoa(start),
		})
		if err != nil {
			return nil, fmt.Errorf("fetching subcollections for %s: %w", collKey, err)
		}
		if repeatedPage(prev, data) {
			return nil, fmt.Errorf("pagination for subcollections of %s ignored start %d", collKey, start)
		}
		prev = data

		var page []map[string]any
		if err := json.Unmarshal(data, &page); err != nil {
			return nil, fmt.Errorf("decoding subcollections for %s: %w", collKey, err)
		}
		for _, sub := range page {
			if key := subcollectionKey(sub); key != "" {
				keys = append(keys, key)
			}
		}
		if len(page) < zoteroPageMax {
			return keys, nil
		}
	}
}
