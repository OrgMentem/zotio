// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean q1ia): `import scan` — a read-only, library-aware triage of a PDF
// folder. It deliberately does NOT re-implement Zotero's "Retrieve Metadata for
// PDF" (recognizer + file upload). Instead it answers the question the Zotero GUI
// cannot: which of these PDFs are duplicates of items I already have, which match
// an item that is missing its PDF, and which are genuinely new — so the user can
// decide what to import. DOI extraction is dependency-free (filename + the PDF's
// uncompressed embedded metadata); no PDF parser, no writes.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

const (
	scanHeadBytes = 2 << 20   // bytes scanned from the head of each PDF for an embedded DOI
	scanTailBytes = 512 << 10 // bytes scanned from the tail (XMP/Info often live near the trailer)
)

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

			db, _ := openStoreForRead(cmd.Context(), "zotio")
			if db == nil {
				fmt.Fprintln(cmd.OutOrStdout(), "Run 'zotio sync' first.")
				return nil
			}
			defer db.Close()

			idx, err := buildLibraryDOIIndex(db)
			if err != nil {
				return fmt.Errorf("indexing library DOIs: %w", err)
			}

			var httpClient *http.Client
			if flagResolve {
				httpClient = &http.Client{Timeout: 15 * time.Second}
			}

			results := make([]scanResult, 0, len(pdfs))
			for _, path := range pdfs {
				results = append(results, classifyPDF(cmd.Context(), path, idx, httpClient))
			}
			return printScanReport(cmd, results, args[0], flags)
		},
	}
	cmd.Flags().BoolVar(&flagResolve, "resolve", false, "Fetch CrossRef titles for 'new' PDFs (network)")
	cmd.Flags().IntVar(&flagLimit, "limit", 0, "Scan at most N PDFs (0 = all)")
	return cmd
}

func listPDFs(dir string, limit int) ([]string, error) {
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

func buildLibraryDOIIndex(db *store.Store) (libraryDOIIndex, error) {
	idx := libraryDOIIndex{byDOI: map[string]libItem{}}
	items, err := db.QueryItems(store.ItemQuery{TopOnly: true})
	if err != nil {
		return idx, err
	}
	withPDF := itemsWithPDFSet(db)
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

// itemsWithPDFSet returns the set of parent item keys that have a PDF attachment.
func itemsWithPDFSet(db *store.Store) map[string]bool {
	set := map[string]bool{}
	atts, err := db.ItemsByType("attachment", 0)
	if err != nil {
		return set
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
	return set
}

func classifyPDF(ctx context.Context, path string, idx libraryDOIIndex, httpClient *http.Client) scanResult {
	res := scanResult{File: filepath.Base(path)}
	res.DOI, res.DOISource = extractPDFDOI(path)
	if res.DOI == "" {
		res.Status = "unidentified"
		return res
	}
	if li, ok := idx.byDOI[strings.ToLower(res.DOI)]; ok {
		res.ItemKey = li.key
		res.Title = li.title
		if li.hasPDF {
			res.Status = "duplicate"
		} else {
			res.Status = "attach_candidate"
		}
		return res
	}
	res.Status = "new"
	if httpClient != nil {
		if item, ok := crossRefItemFromDOI(ctx, httpClient, res.DOI); ok {
			if t, _ := stringValue(item["title"]); t != "" {
				res.Title = t
			}
		}
	}
	return res
}

// extractPDFDOI finds a DOI in the filename, then in the PDF's uncompressed
// embedded metadata. It never decodes compressed page text (no PDF parser).
func extractPDFDOI(path string) (doi, source string) {
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	// A filename cannot contain '/', so a DOI saved into one is slash-encoded.
	// Decode the common, unambiguous encodings before matching (skip '_' — DOIs
	// legitimately contain underscores, so that substitution is too risky).
	decoded := strings.NewReplacer("%2F", "/", "%2f", "/", "\u2044", "/", "\u2215", "/").Replace(base)
	if m := doiScanRE.FindString(decoded); m != "" {
		if d := normalizeDOI(m); d != "" {
			return d, "filename"
		}
	}
	if bytes := pdfScanBytes(path); len(bytes) > 0 {
		if m := doiScanRE.FindString(string(bytes)); m != "" {
			if d := normalizeDOI(m); d != "" {
				return d, "content"
			}
		}
	}
	return "", "none"
}

// pdfScanBytes returns the head and tail bytes of a file (where PDF XMP/Info
// metadata typically lives) without reading a potentially huge body into memory.
func pdfScanBytes(path string) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil
	}
	if fi.Size() <= scanHeadBytes+scanTailBytes {
		// PATCH(glean zotero-pp-cli-2d2f9f266e5fedac): even the small-file path
		// reads through an explicit cap instead of unbounded io.ReadAll.
		data, _ := io.ReadAll(io.LimitReader(f, scanHeadBytes+scanTailBytes+1))
		if int64(len(data)) > scanHeadBytes+scanTailBytes {
			return nil
		}
		return data
	}
	head := make([]byte, scanHeadBytes)
	n, _ := io.ReadFull(f, head)
	out := head[:n]
	tail := make([]byte, scanTailBytes)
	if _, err := f.Seek(-scanTailBytes, io.SeekEnd); err == nil {
		m, _ := io.ReadFull(f, tail)
		out = append(out, tail[:m]...)
	}
	return out
}

func printScanReport(cmd *cobra.Command, results []scanResult, dir string, flags *rootFlags) error {
	counts := map[string]int{}
	for _, r := range results {
		counts[r.Status]++
	}
	if flags.asJSON {
		data, err := json.Marshal(map[string]any{
			"dir":     dir,
			"scanned": len(results),
			"counts":  counts,
			"results": results,
		})
		if err != nil {
			return err
		}
		return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), flags)
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Scanned %d PDF(s) in %s: %s\n", len(results), dir, summarizeCounts(counts))
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
