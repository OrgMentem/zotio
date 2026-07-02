// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
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
				f, err := os.Create(flagOutput)
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
	cmd.Flags().IntVar(&flagLimit, "limit", 200, "Maximum items per collection request")

	return cmd
}

func exportCollection(c interface {
	Get(path string, params map[string]string) (json.RawMessage, error)
}, out io.Writer, collKey, format string, flat bool, limit int, visited map[string]bool) error {
	if visited[collKey] {
		return nil
	}
	visited[collKey] = true

	params := map[string]string{
		"format": format,
		"limit":  fmt.Sprintf("%d", limit),
	}
	// PATCH(glean pathenc-2): url-encode path param to prevent segment injection.
	data, err := c.Get("/collections/"+url.PathEscape(collKey)+"/items", params)
	if err != nil {
		return fmt.Errorf("fetching items for collection %s: %w", collKey, err)
	}

	content := strings.TrimSpace(string(data))
	if content != "" && content != "[]" && content != "null" {
		if _, err := fmt.Fprintln(out, content); err != nil {
			return err
		}
	}

	if flat {
		return nil
	}

	// PATCH(glean pathenc-2): url-encode path param to prevent segment injection.
	subData, err := c.Get("/collections/"+url.PathEscape(collKey)+"/collections", nil)
	if err != nil {
		return fmt.Errorf("fetching subcollections for %s: %w", collKey, err)
	}

	var subcols []map[string]any
	if err := json.Unmarshal(subData, &subcols); err != nil {
		return nil
	}
	for _, sub := range subcols {
		key, _ := sub["key"].(string)
		if key == "" {
			if d, ok := sub["data"].(map[string]any); ok {
				key, _ = d["key"].(string)
			}
		}
		if key == "" {
			continue
		}
		if err := exportCollection(c, out, key, format, flat, limit, visited); err != nil {
			fmt.Fprintf(os.Stderr, "warning: error exporting subcollection %s: %v\n", key, err)
		}
	}
	return nil
}
