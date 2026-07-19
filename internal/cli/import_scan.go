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

			idx, err := buildLibraryDOIIndex(db)
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
	withPDF, err := itemsWithPDFSet(db)
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
func itemsWithPDFSet(db *store.Store) (map[string]bool, error) {
	set := map[string]bool{}
	atts, err := db.QueryItems(store.ItemQuery{ItemType: "attachment"})
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
		if item, err := crossRefItemFromDOI(ctx, httpClient, res.DOI); err == nil {
			if t, _ := stringValue(item["title"]); t != "" {
				res.Title = t
			}
		}
	}
	return res, nil
}

// extractPDFDOI finds a DOI in the filename, then in the PDF's uncompressed
// embedded metadata. It never decodes compressed page text (no PDF parser).
func extractPDFDOI(path string) (doi, source string, err error) {
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	// A filename cannot contain '/', so a DOI saved into one is slash-encoded.
	// Decode the common, unambiguous encodings before matching (skip '_' — DOIs
	// legitimately contain underscores, so that substitution is too risky).
	decoded := strings.NewReplacer("%2F", "/", "%2f", "/", "\u2044", "/", "\u2215", "/").Replace(base)
	if m := doiScanRE.FindString(decoded); m != "" {
		if d := normalizeDOI(m); d != "" {
			return d, "filename", nil
		}
	}
	bytes, err := pdfScanBytes(path)
	if err != nil {
		return "", "none", err
	}
	if m := doiScanRE.FindString(string(bytes)); m != "" {
		if d := normalizeDOI(m); d != "" {
			return d, "content", nil
		}
	}
	return "", "none", nil
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
