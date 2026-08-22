// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// `import scan` — a read-only, library-aware triage of a PDF
// folder. It deliberately does NOT re-implement Zotero's "Retrieve Metadata for
// PDF" (recognizer + file upload). Instead it answers the question the Zotero GUI
// cannot: which of these PDFs are duplicates of items I already have, which match
// an item that is missing its PDF, and which are genuinely new — so the user can
// decide what to import. DOI extraction is dependency-free (filename + the PDF's
// uncompressed embedded metadata); no PDF parser, no writes.

package cli

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"zotio/internal/store"
)

// doiScanRE matches a DOI in a filename or raw PDF bytes; tighter than the URL
// variant so it stops at whitespace/binary rather than over-capturing.
var doiScanRE = regexp.MustCompile(`10\.\d{4,9}/[A-Za-z0-9._;()/:\-]+`)

// arxivFilenamePattern matches an arXiv ID in a staged filename such as
// "arxiv-2301.08745.pdf". The literal "arxiv" token is REQUIRED: a bare
// "2301.08745" in a filename is not evidence of an arXiv paper, and inventing
// an identifier is worse than reporting none. Accepts the separators that
// appear in practice (space, ':', '.', '_', '-') and both ID generations, the
// same two forms arxivURLPattern and arxivExtraPattern already accept.
var arxivFilenamePattern = regexp.MustCompile(`(?i)arxiv[\s:._-]*([a-z-]+/[0-9]{7}|[0-9]{4}\.[0-9]{4,5})(?:v[0-9]+)?`)

const (
	scanHeadBytes = 2 << 20   // bytes scanned from the head of each PDF for an embedded DOI
	scanTailBytes = 512 << 10 // bytes scanned from the tail (XMP/Info often live near the trailer)

	// Decompression is deliberately bounded: import scan must remain a cheap
	// preflight even when handed a hostile or unusually large PDF.
	pdfCompressedScanBytes = 64 << 20
	pdfMaxFlateStreams     = 128
	pdfMaxFlateStreamBytes = 4 << 20
	pdfMaxFlateOutputBytes = 8 << 20
)

// test seams for inflatePDFStream: allow tests to observe construction and Close
// calls without changing behavior when not overridden.
var inflateZlibReader = func(r io.Reader) (io.ReadCloser, error) {
	return zlib.NewReader(r)
}

var inflateFlateReader = func(r io.Reader) io.ReadCloser {
	return flate.NewReader(r)
}

type scanResult struct {
	File      string `json:"file"`
	DOI       string `json:"doi,omitempty"`
	DOISource string `json:"doi_source"` // filename | content | none
	Status    string `json:"status"`     // new | duplicate | attach_candidate | unidentified
	ItemKey   string `json:"item_key,omitempty"`
	Title     string `json:"title,omitempty"`
}

func newImportScanCmd(flags *rootFlags) *cobra.Command {
	var (
		flagResolve bool
		flagLimit   int
	)
	cmd := &cobra.Command{
		Use:   "scan <dir>",
		Short: "Triage a folder of PDFs against your library (read-only): new vs duplicate vs attach-candidate",
		Long: `Scan a directory of PDFs and classify each against your synced library WITHOUT
importing anything. For each PDF it extracts a DOI (from the filename, then the
file's embedded metadata) and reports:

  duplicate         the DOI already belongs to an item in your library
  attach_candidate  matches an item you have that is missing its PDF
  new               not in your library (use 'import doi <DOI>' to add it)
  unidentified      no DOI found (rename the file with its DOI, or add it in Zotero)

This complements Zotero's "Retrieve Metadata for PDF": it makes the library-aware
decision the GUI does not — which PDFs to import, which are duplicates, and which
complete items you already have. It never writes; hand the actual import and file
attach to Zotero.

DOI extraction is dependency-free: it reads the filename and the PDF's uncompressed
embedded metadata (Info/XMP). It does NOT decode compressed page text, so scanned
or text-only PDFs may report "unidentified".`,
		Example: `  zotio import scan ~/Downloads/papers
  zotio import scan ~/Downloads/papers --resolve --json`,
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			pdfs, err := listPDFs(args[0], flagLimit)
			if err != nil {
				return err
			}

			db, err := openStoreForRead(cmd.Context(), "zotio")
			if err != nil {
				return fmt.Errorf("opening local database: %w", err)
			}
			if db == nil {
				fmt.Fprintln(cmd.OutOrStdout(), "Run 'zotio sync' first.")
				return nil
			}
			if err := db.DB().PingContext(cmd.Context()); err != nil {
				_ = db.Close()
				return fmt.Errorf("opening local database: %w", err)
			}
			defer db.Close()

			idx, err := buildLibraryDOIIndex(cmd.Context(), db)
			if err != nil {
				return finishScanReport(cmd, nil, 0, args[0], []string{fmt.Sprintf("indexing library DOI and PDF attachments: %v", err)}, flags)
			}

			var httpClient *http.Client
			if flagResolve {
				httpClient = &http.Client{Timeout: 15 * time.Second}
			}

			results := make([]scanResult, 0, len(pdfs))
			var warnings []string
			for _, path := range pdfs {
				result, err := classifyPDFWithErr(cmd.Context(), path, idx, httpClient)
				if err != nil {
					warnings = append(warnings, fmt.Sprintf("reading PDF %s: %v", path, err))
					continue
				}
				results = append(results, result)
			}
			return finishScanReport(cmd, results, len(pdfs), args[0], warnings, flags)
		},
	}
	cmd.Flags().BoolVar(&flagResolve, "resolve", false, "Fetch titles for 'new' PDFs from CrossRef, then DataCite (network)")
	cmd.Flags().IntVar(&flagLimit, "limit", 0, "Scan at most N PDFs (0 = all)")
	return cmd
}

func listPDFs(dir string, limit int) ([]string, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", dir, err)
	}
	if info.Mode().IsRegular() {
		return nil, fmt.Errorf("import scan expects a directory; for a single file use `zotio import pdf %s`", dir)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", dir, err)
	}
	var pdfs []string
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".pdf") {
			continue
		}
		pdfs = append(pdfs, filepath.Join(dir, e.Name()))
		if limit > 0 && len(pdfs) >= limit {
			break
		}
	}
	return pdfs, nil
}

type libItem struct {
	key    string
	title  string
	hasPDF bool
}

type libraryDOIIndex struct {
	byDOI map[string]libItem // key: lowercased DOI
}

func buildLibraryDOIIndex(ctx context.Context, db *store.Store) (libraryDOIIndex, error) {
	idx := libraryDOIIndex{byDOI: map[string]libItem{}}
	items, err := db.QueryItemsContext(ctx, store.ItemQuery{TopOnly: true})
	if err != nil {
		return idx, err
	}
	withPDF, err := itemsWithPDFSet(ctx, db)
	if err != nil {
		return idx, fmt.Errorf("indexing PDF attachments: %w", err)
	}
	for _, raw := range items {
		var obj map[string]any
		if json.Unmarshal(raw, &obj) != nil {
			continue
		}
		data, ok := obj["data"].(map[string]any)
		if !ok {
			continue
		}
		rawDOI, _ := stringValue(data["DOI"])
		doi := normalizeDOI(rawDOI)
		if doi == "" {
			continue
		}
		key, _ := stringValue(data["key"])
		if key == "" {
			key, _ = stringValue(obj["key"])
		}
		title, _ := stringValue(data["title"])
		idx.byDOI[strings.ToLower(doi)] = libItem{key: key, title: title, hasPDF: withPDF[key]}
	}
	return idx, nil
}

// itemsWithPDFSet returns the set of parent item keys that have a live PDF attachment.
func itemsWithPDFSet(ctx context.Context, db *store.Store) (map[string]bool, error) {
	set := map[string]bool{}
	atts, err := db.QueryItemsContext(ctx, store.ItemQuery{ItemType: "attachment"})
	if err != nil {
		return nil, err
	}
	for _, raw := range atts {
		var obj map[string]any
		if json.Unmarshal(raw, &obj) != nil {
			continue
		}
		data, ok := obj["data"].(map[string]any)
		if !ok {
			continue
		}
		if ct, _ := stringValue(data["contentType"]); ct != "application/pdf" {
			continue
		}
		if parent, _ := stringValue(data["parentItem"]); parent != "" {
			set[parent] = true
		}
	}
	return set, nil
}

func classifyPDF(ctx context.Context, path string, idx libraryDOIIndex, httpClient *http.Client) scanResult {
	res, err := classifyPDFWithErr(ctx, path, idx, httpClient)
	if err != nil {
		return scanResult{File: filepath.Base(path), DOISource: "none", Status: "unidentified"}
	}
	return res
}

func classifyPDFWithErr(ctx context.Context, path string, idx libraryDOIIndex, httpClient *http.Client) (scanResult, error) {
	res := scanResult{File: filepath.Base(path)}
	var err error
	res.DOI, res.DOISource, err = extractPDFDOI(path)
	if err != nil {
		return res, err
	}
	if res.DOI == "" {
		res.Status = "unidentified"
		return res, nil
	}
	if li, ok := idx.byDOI[strings.ToLower(res.DOI)]; ok {
		res.ItemKey = li.key
		res.Title = li.title
		if li.hasPDF {
			res.Status = "duplicate"
		} else {
			res.Status = "attach_candidate"
		}
		return res, nil
	}
	res.Status = "new"
	if httpClient != nil {
		// Same registry-agnostic resolution the manifest uses, so a directory
		// scan and the manifest it produces agree on an arXiv PDF's title
		// instead of resolving the item but showing no title for it.
		if item, err := fetchDOIItemWithCache(ctx, httpClient, res.DOI, nil); err == nil {
			// crossRefItemFromWork falls back to the DOI when a record carries
			// no title. A DOI is not a title, so do not display one as if it
			// were.
			if t, _ := stringValue(item["title"]); t != "" && !strings.EqualFold(t, res.DOI) {
				res.Title = t
			}
		}
	}
	return res, nil
}

// extractPDFDOI finds a DOI in the filename, then in the PDF's metadata and
// compressed content streams. It intentionally does not attempt full PDF text
// extraction: bounded Flate decoding is enough for DOI-bearing streams produced
// by ordinary scholarly PDFs and keeps this preflight dependency-free.
func extractPDFDOI(path string) (doi, source string, err error) {
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	decoded := decodeIdentifierFilename(base)
	if d := doiFromBytes([]byte(decoded)); d != "" {
		return d, "filename", nil
	}
	// No DOI in the name, but an arXiv ID in it names a paper just as
	// precisely, and every arXiv paper has a DataCite self-DOI. Deriving that
	// DOI hands the rest to the existing pipeline rather than adding a second
	// resolution path for the same paper.
	if d := arxivSelfDOIFromFilename(decoded); d != "" {
		return d, "filename", nil
	}
	raw, err := pdfScanBytes(path)
	if err != nil {
		return "", "none", err
	}
	if d := doiFromBytes(raw); d != "" {
		return d, "content", nil
	}
	if d, err := pdfScanFlateDOI(path); err != nil {
		return "", "none", err
	} else if d != "" {
		return d, "content", nil
	}
	return "", "none", nil
}

// decodeIdentifierFilename turns a staged filename back into the identifier it
// encodes. A filename cannot contain '/', so tools that stage papers by
// identifier percent-encode it (papio writes
// "10.47205%2Fjdss.2023%284-ii%2934.pdf").
//
// Decoding only %2F was not enough: '%' is outside doiScanRE's character class,
// so a surviving "%28" ended the match early and
// "10.47205/jdss.2023(4-ii)34" was looked up as "10.47205/jdss.2023", which
// 404s at both registries even though the full DOI resolves. Parentheses are
// legal in a DOI suffix. See dev/field-report-2026-08-22-papio-round2.md.
//
// An invalid escape means the name was not percent-encoded after all, so fall
// back to the narrow slash decoding rather than returning the name unusable.
// The Unicode division slashes are the other convention for the same problem.
func decodeIdentifierFilename(base string) string {
	base = strings.NewReplacer("\u2044", "/", "\u2215", "/").Replace(base)
	if unescaped, err := url.PathUnescape(base); err == nil {
		return unescaped
	}
	return strings.NewReplacer("%2F", "/", "%2f", "/").Replace(base)
}

// arxivSelfDOIFromFilename derives an arXiv paper's DataCite self-DOI from a
// staged filename such as "arxiv-2301.08745.pdf". Returning a DOI rather than a
// bare arXiv ID lets DataCite resolution and the arXiv field mapping in
// import_datacite.go handle it unchanged.
func arxivSelfDOIFromFilename(name string) string {
	m := arxivFilenamePattern.FindStringSubmatch(name)
	if len(m) < 2 {
		return ""
	}
	id := normalizeArxivID(m[1])
	if id == "" {
		return ""
	}
	return arxivSelfDOICanonicalPrefix + id
}

// trimUnbalancedClosers drops trailing brackets that belong to the surrounding
// prose rather than to the DOI, as in "as reported (see 10.1000/foo)". A closer
// with a matching opener inside the match is part of the DOI itself
// ("10.1000/ends(x)"), so it is kept: dropping it produced an unbalanced,
// non-existent DOI.
func trimUnbalancedClosers(doi string) string {
	for len(doi) > 0 {
		last := doi[len(doi)-1]
		var open byte
		switch last {
		case ')':
			open = '('
		case ']':
			open = '['
		case '}':
			open = '{'
		default:
			return doi
		}
		if strings.Count(doi, string(open)) >= strings.Count(doi, string(last)) {
			return doi
		}
		doi = doi[:len(doi)-1]
	}
	return doi
}

func doiFromBytes(data []byte) string {
	m := doiScanRE.Find(data)
	if len(m) == 0 {
		return ""
	}
	doi := trimUnbalancedClosers(normalizeDOI(string(m)))
	if slash := strings.IndexByte(doi, '/'); slash >= 0 {
		doi = doi[:slash+1] + strings.TrimLeft(doi[slash+1:], "/")
	}
	return doi
}

func pdfScanFlateDOI(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("opening: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, pdfCompressedScanBytes+1))
	if err != nil {
		return "", fmt.Errorf("reading compressed streams: %w", err)
	}
	if len(data) > pdfCompressedScanBytes {
		data = data[:pdfCompressedScanBytes]
	}
	streams := 0
	for searchFrom := 0; searchFrom < len(data) && streams < pdfMaxFlateStreams; {
		rel := bytes.Index(data[searchFrom:], []byte("stream"))
		if rel < 0 {
			break
		}
		streamStart := searchFrom + rel + len("stream")
		dictStart := bytes.LastIndex(data[searchFrom:searchFrom+rel], []byte("<<"))
		if dictStart < 0 {
			searchFrom = streamStart
			continue
		}
		dict := data[searchFrom+dictStart : searchFrom+rel]
		if !bytes.Contains(dict, []byte("FlateDecode")) {
			searchFrom = streamStart
			continue
		}
		if streamStart < len(data) && data[streamStart] == '\r' {
			streamStart++
		}
		if streamStart < len(data) && data[streamStart] == '\n' {
			streamStart++
		}
		endRel := bytes.Index(data[streamStart:], []byte("endstream"))
		if endRel < 0 {
			break
		}
		streamEnd := streamStart + endRel
		if streamEnd-streamStart > pdfMaxFlateStreamBytes {
			searchFrom = streamEnd
			continue
		}
		streams++
		if decoded := inflatePDFStream(data[streamStart:streamEnd]); len(decoded) > 0 {
			if d := doiFromBytes(decoded); d != "" {
				return d, nil
			}
		}
		searchFrom = streamEnd + len("endstream")
	}
	return "", nil
}

func inflatePDFStream(data []byte) []byte {
	// Try zlib first and close its reader before attempting flate, so a
	// successful zlib decompression does not abandon an unclosed flate reader.
	// Both readers wrap a bytes.Reader (heap buffers only), but closing
	// eagerly preserves the io.Closer contract.
	if zr, err := inflateZlibReader(bytes.NewReader(data)); err == nil {
		out, err := io.ReadAll(io.LimitReader(zr, pdfMaxFlateOutputBytes+1))
		_ = zr.Close()
		if err == nil && len(out) <= pdfMaxFlateOutputBytes {
			return out
		}
	}
	fr := inflateFlateReader(bytes.NewReader(data))
	out, err := io.ReadAll(io.LimitReader(fr, pdfMaxFlateOutputBytes+1))
	_ = fr.Close()
	if err == nil && len(out) <= pdfMaxFlateOutputBytes {
		return out
	}
	return nil
}

// pdfScanBytes returns the head and tail bytes of a file (where PDF XMP/Info
// metadata typically lives) without reading a potentially huge body into memory.
func pdfScanBytes(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stating: %w", err)
	}
	if fi.Size() <= scanHeadBytes+scanTailBytes {
		// Even the small-file path
		// reads through an explicit cap instead of unbounded io.ReadAll.
		data, err := io.ReadAll(io.LimitReader(f, scanHeadBytes+scanTailBytes+1))
		if err != nil {
			return nil, fmt.Errorf("reading: %w", err)
		}
		if int64(len(data)) > scanHeadBytes+scanTailBytes {
			return nil, fmt.Errorf("reading: file grew beyond scan cap")
		}
		return data, nil
	}
	head := make([]byte, scanHeadBytes)
	n, err := io.ReadFull(f, head)
	if err != nil {
		return nil, fmt.Errorf("reading head: %w", err)
	}
	out := head[:n]
	tail := make([]byte, scanTailBytes)
	if _, err := f.Seek(-scanTailBytes, io.SeekEnd); err != nil {
		return nil, fmt.Errorf("seeking tail: %w", err)
	}
	m, err := io.ReadFull(f, tail)
	if err != nil {
		return nil, fmt.Errorf("reading tail: %w", err)
	}
	return append(out, tail[:m]...), nil
}

type scanReport struct {
	Dir      string         `json:"dir"`
	Scanned  int            `json:"scanned"`
	Counts   map[string]int `json:"counts"`
	Results  []scanResult   `json:"results"`
	Warnings []string       `json:"warnings,omitempty"`
}

func finishScanReport(cmd *cobra.Command, results []scanResult, scanned int, dir string, warnings []string, flags *rootFlags) error {
	if err := printScanReport(cmd, results, scanned, dir, warnings, flags); err != nil {
		return err
	}
	if len(warnings) == 0 {
		return nil
	}
	if !flags.asJSON {
		for _, warning := range warnings {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", warning)
		}
	}
	return degradedErr(fmt.Errorf("import scan: %d warnings; results incomplete", len(warnings)))
}

func printScanReport(cmd *cobra.Command, results []scanResult, scanned int, dir string, warnings []string, flags *rootFlags) error {
	counts := map[string]int{}
	for _, r := range results {
		counts[r.Status]++
	}
	if flags.asJSON {
		data, err := json.Marshal(scanReport{
			Dir:      dir,
			Scanned:  scanned,
			Counts:   counts,
			Results:  results,
			Warnings: warnings,
		})
		if err != nil {
			return err
		}
		return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), flags)
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Scanned %d PDF(s) in %s: %s\n", scanned, dir, summarizeCounts(counts))
	for _, r := range results {
		line := fmt.Sprintf("  [%s] %s", r.Status, r.File)
		switch r.Status {
		case "duplicate":
			line += fmt.Sprintf("  %s -> %s %q", r.DOI, r.ItemKey, r.Title)
		case "attach_candidate":
			line += fmt.Sprintf("  %s -> %s %q (item missing its PDF)", r.DOI, r.ItemKey, r.Title)
		case "new":
			line += "  " + r.DOI
			if r.Title != "" {
				line += fmt.Sprintf(" %q", r.Title)
			}
		case "unidentified":
			line += "  (no DOI in filename or embedded metadata)"
		}
		fmt.Fprintln(out, line)
	}
	return nil
}
