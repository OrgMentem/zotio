// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

func newCollectionsExportCmd(flags *rootFlags) *cobra.Command {
	var flagFormat string
	var flagOutput string
	var flagFlat bool
	var flagLimit int

	cmd := &cobra.Command{
		Use:   "export <collectionKey>",
		Short: "Export a collection (and subcollections) as BibTeX, RIS, or CSL-JSON",
		Long: `Recursively walks a collection and all its subcollections, then emits a
single combined export in the requested format. Use --flat to export only
the top-level collection without recursing into subcollections.`,
		Example: `  # Export collection as BibTeX (default)
  zotio collections export ABCD1234

  # Export as RIS to a file
  zotio collections export ABCD1234 --format ris --output refs.ris

  # Export without descending into subcollections
  zotio collections export ABCD1234 --flat`,
		Annotations: map[string]string{"mcp:read-only": "true"},
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
			case "bibtex", "ris", "csljson":
			default:
				return fmt.Errorf("unknown format %q: use bibtex, ris, or csljson", format)
			}

			c, err := flags.newClient()
			if err != nil {
				return err
			}

			var out = cmd.OutOrStdout()
			if flagOutput != "" {
				f, err := openPrivateOutputFile(flagOutput, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
				if err != nil {
					return fmt.Errorf("creating output file: %w", err)
				}
				defer f.Close()
				out = f
			}

			visited := map[string]bool{}
			return exportCollection(c, out, collKey, format, flagFlat, flagLimit, visited)
		},
	}
	cmd.Flags().StringVar(&flagFormat, "format", "bibtex", "Export format: bibtex, ris, csljson")
	cmd.Flags().StringVar(&flagOutput, "output", "", "Write output to file instead of stdout")
	cmd.Flags().BoolVar(&flagFlat, "flat", false, "Export only the top-level collection, skip subcollections")
	cmd.Flags().IntVar(&flagLimit, "limit", zoteroPageMax, "Items fetched per API request (max 100); the export always walks every page")

	return cmd
}

// zoteroPageMax is the API's hard ceiling on `limit` for a multi-object read.
// The server silently clamps anything larger, which is exactly how a
// single-shot export truncated a large collection without reporting it.
const zoteroPageMax = 100

// collectionExportClient is the read surface a collection export needs.
// GetWithHeader is what makes paging a text export possible at all: BibTeX and
// RIS come back as one opaque document, so the item count has to come from the
// Total-Results header rather than from the body.
type collectionExportClient interface {
	Get(path string, params map[string]string) (json.RawMessage, error)
	GetWithHeader(path string, params map[string]string, header string) (json.RawMessage, string, error)
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
		if err := collectCollectionCSLJSON(c, collKey, flat, limit, visited, &items); err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(items)
	}

	if visited[collKey] {
		return nil
	}
	visited[collKey] = true

	if err := forEachCollectionItemPage(c, collKey, format, limit, func(data json.RawMessage) error {
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
		if err := exportCollection(c, out, key, format, flat, limit, visited); err != nil {
			return fmt.Errorf("exporting subcollection %s: %w", key, err)
		}
	}
	return nil
}

// forEachCollectionItemPage walks every page of a collection's items, handing
// each non-blank page to emit.
//
// An export format returns an opaque document rather than a countable array,
// so the page count comes from Total-Results, which the API sends on every
// multi-object read. A server that omits the header falls back to paging until
// a page comes back blank: one wasted request at the end, but a first page can
// no longer be mistaken for the whole collection.
func forEachCollectionItemPage(c collectionExportClient, collKey, format string, limit int, emit func(json.RawMessage) error) error {
	pageSize := exportPageSize(limit)
	// url-encode path param to prevent segment injection.
	path := "/collections/" + url.PathEscape(collKey) + "/items"
	var prev []byte
	for start := 0; ; start += pageSize {
		data, total, err := c.GetWithHeader(path, map[string]string{
			"format": format,
			"limit":  strconv.Itoa(pageSize),
			"start":  strconv.Itoa(start),
		}, "Total-Results")
		if err != nil {
			return fmt.Errorf("fetching items for collection %s: %w", collKey, err)
		}
		if repeatedPage(prev, data) {
			return nil
		}
		prev = data

		blank := isBlankExportPage(data)
		if !blank {
			if err := emit(data); err != nil {
				return err
			}
		}
		if count, cerr := strconv.Atoi(strings.TrimSpace(total)); cerr == nil {
			if start+pageSize >= count {
				return nil
			}
			continue
		}
		if blank {
			return nil
		}
	}
}

func collectCollectionCSLJSON(c collectionExportClient, collKey string, flat bool, limit int, visited map[string]bool, items *[]json.RawMessage) error {
	if visited[collKey] {
		return nil
	}
	visited[collKey] = true

	pageSize := exportPageSize(limit)
	// url-encode path param to prevent segment injection.
	path := "/collections/" + url.PathEscape(collKey) + "/items"
	var prev []byte
	for start := 0; ; start += pageSize {
		data, err := c.Get(path, map[string]string{
			"format": "csljson",
			"limit":  strconv.Itoa(pageSize),
			"start":  strconv.Itoa(start),
		})
		if err != nil {
			return fmt.Errorf("fetching items for collection %s: %w", collKey, err)
		}
		if repeatedPage(prev, data) {
			break
		}
		prev = data

		var page []json.RawMessage
		if err := json.Unmarshal(data, &page); err != nil {
			return fmt.Errorf("decoding CSL-JSON items for collection %s: %w", collKey, err)
		}
		*items = append(*items, page...)
		// CSL-JSON is a countable array, so a short page is the last page and
		// no response header is needed to know it.
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
		if err := collectCollectionCSLJSON(c, key, flat, limit, visited, items); err != nil {
			return fmt.Errorf("exporting subcollection %s: %w", key, err)
		}
	}
	return nil
}

// fetchSubcollectionKeys returns every child collection key. The subcollection
// read was previously unpaginated, so the API's default page size silently
// dropped whole subtrees from a broad collection's export.
func fetchSubcollectionKeys(c collectionExportClient, collKey string) ([]string, error) {
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
			return keys, nil
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
