// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"

	"zotio/internal/client"

	"github.com/spf13/cobra"
)

// openPrivateOutputFile opens an export artifact with owner-only permissions.
// Chmod also repairs an existing output file created by an earlier zotio
// version with a less restrictive mode.
func openPrivateOutputFile(path string, flags int) (*os.File, error) {
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func finishExport(writer *bufio.Writer, file *os.File, primary error) error {
	flushErr := writer.Flush()
	var closeErr error
	if file != nil {
		closeErr = file.Close()
	}
	if primary != nil {
		return primary
	}
	if flushErr != nil {
		return fmt.Errorf("flushing export: %w", flushErr)
	}
	if closeErr != nil {
		return fmt.Errorf("closing export: %w", closeErr)
	}
	return nil
}

func writeExport(writer io.Writer, format string, data []byte, limit int) (int, error) {
	switch format {
	case "jsonl":
		var items []json.RawMessage
		if err := json.Unmarshal(data, &items); err != nil {
			_, err := fmt.Fprintln(writer, string(data))
			return 0, err
		}
		count := 0
		for _, item := range items {
			if limit > 0 && count >= limit {
				break
			}
			if _, err := fmt.Fprintln(writer, string(item)); err != nil {
				return count, err
			}
			count++
		}
		return count, nil
	default:
		var parsed any
		if err := json.Unmarshal(data, &parsed); err != nil {
			return 0, err
		}
		enc := json.NewEncoder(writer)
		enc.SetIndent("", "  ")
		return 0, enc.Encode(parsed)
	}
}

func newExportCmd(flags *rootFlags) *cobra.Command {
	var format string
	var outputFile string
	var limit int
	var noCache bool

	cmd := &cobra.Command{
		Use:   "export <resource> [id]",
		Short: "Export data to JSONL or JSON for backup, migration, or analysis",
		Long: `Export paginated API data to a local file. Supports JSONL (one JSON object
per line, streaming-friendly) and JSON (array). JSONL is recommended for
large datasets as it has no memory pressure.`,
		Example: `  # Export all items as JSONL (streaming, recommended for large datasets)
  zotio export <resource> --format jsonl --output data.jsonl

  # Export with limit
  zotio export <resource> --format jsonl --limit 1000

  # Pipe to another tool
  zotio export <resource> --format jsonl | jq '.id'`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			validResources := map[string]bool{
				"collections": true,
				"items":       true,
				"searches":    true,
				"tags":        true,
			}
			validResourceList := []string{
				"collections",
				"items",
				"searches",
				"tags",
			}
			resource := args[0]
			if !validResources[resource] {
				return usageErr(fmt.Errorf("unknown resource %q; valid: %s", resource, strings.Join(validResourceList, ", ")))
			}

			var err error

			// encode the optional resource id as one path segment.
			path := "/" + resource
			if len(args) > 1 {
				path += "/" + url.PathEscape(args[1])
			}

			var c *client.Client
			if flags.dataSource != "local" {
				c, err = flags.newClient()
				if err != nil {
					return err
				}
				if noCache {
					c.NoCache = true
				}
			}

			var file *os.File
			output := cmd.OutOrStdout()
			if outputFile != "" {
				file, err = openPrivateOutputFile(outputFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
				if err != nil {
					return fmt.Errorf("creating output file: %w", err)
				}
				output = file
			}
			writer := bufio.NewWriter(output)

			var count int
			var exportErr error
			if flags.dataSource == "local" {
				params := map[string]string(nil)
				if len(args) == 1 && limit > 0 {
					params = map[string]string{"limit": strconv.Itoa(limit)}
				}
				data, _, getErr := resolveRead(cmd.Context(), nil, flags, resource, len(args) == 1, path, params, nil)
				if getErr != nil {
					return finishExport(writer, file, classifyAPIError(getErr, flags))
				}
				count, exportErr = writeExport(writer, format, data, limit)
			} else {
				if len(args) > 1 {
					data, getErr := c.Get(path, nil)
					if getErr != nil {
						return finishExport(writer, file, classifyAPIError(getErr, flags))
					}
					count, exportErr = writeExport(writer, format, data, limit)
				} else {
					items := make([]json.RawMessage, 0)
					count, exportErr = resumablePaginatedFetch(cmd.Context(), c, path, nil, 100, limit, "", func(page []json.RawMessage) error {
						if format != "jsonl" {
							items = append(items, page...)
							return nil
						}
						for _, item := range page {
							if _, err := fmt.Fprintln(writer, string(item)); err != nil {
								return err
							}
						}
						return nil
					})
					if exportErr == nil && format != "jsonl" {
						data, marshalErr := json.Marshal(items)
						if marshalErr != nil {
							exportErr = marshalErr
						} else {
							_, exportErr = writeExport(writer, format, data, 0)
						}
					}
				}
			}
			if err := finishExport(writer, file, exportErr); err != nil {
				return err
			}
			if outputFile != "" && format == "jsonl" {
				fmt.Fprintf(cmd.ErrOrStderr(), "Exported %d records to %s\n", count, outputFile)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&format, "format", "jsonl", "Output format: jsonl or json")
	cmd.Flags().StringVarP(&outputFile, "output", "o", "", "Output file path (default: stdout)")
	cmd.Flags().IntVar(&limit, "limit", 0, "Maximum records to export (0 = unlimited)")
	cmd.Flags().BoolVar(&noCache, "no-cache", false, "Bypass response cache for fresh data")

	// paginated/resumable snapshot subcommand.
	cmd.AddCommand(newExportSnapshotCmd(flags))

	return cmd
}
