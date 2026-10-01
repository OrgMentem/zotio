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

// exportOutputFileMode is the mode an export artifact is published with. It
// matches what openPrivateOutputFile forced for the direct writes this
// replaced, so the published permissions are unchanged.
const exportOutputFileMode = 0o600

// exportFileResult reports an `export --output` run. Replaced says whether a
// file already at Output was replaced; under --dry-run it says whether one
// would be, and nothing was written.
type exportFileResult struct {
	Output   string `json:"output"`
	Format   string `json:"format"`
	Records  int    `json:"records"`
	Replaced bool   `json:"replaced"`
	DryRun   bool   `json:"dry_run,omitempty"`
}

func (r exportFileResult) message() string {
	switch {
	case r.DryRun && r.Replaced:
		return fmt.Sprintf("Dry run: would export %d records to %s, replacing the existing file; nothing was written", r.Records, r.Output)
	case r.DryRun:
		return fmt.Sprintf("Dry run: would export %d records to new file %s; nothing was written", r.Records, r.Output)
	case r.Replaced:
		return fmt.Sprintf("Exported %d records to %s (replaced the existing file)", r.Records, r.Output)
	default:
		return fmt.Sprintf("Exported %d records to %s", r.Records, r.Output)
	}
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
		Long: `Export paginated API data to a local file. Output defaults to JSONL
(one JSON object per line, streaming-friendly); JSON output is available for
backwards-compatible resource exports.`,
		Example: `  # Export all items as JSONL (streaming, recommended for large datasets)
  zotio export items --output data.jsonl

  # Export with limit
  zotio export items --limit 1000

  # Pipe to another tool
  zotio export items | jq '.id'`,
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

			// Generation must not know whether it is writing to stdout or to a
			// file: only publication differs between them.
			runExport := func(writer *bufio.Writer) (int, error) {
				if flags.dataSource == "local" {
					params := map[string]string(nil)
					if len(args) == 1 && limit > 0 {
						params = map[string]string{"limit": strconv.Itoa(limit)}
					}
					data, _, getErr := resolveRead(cmd.Context(), nil, flags, resource, len(args) == 1, path, params, nil)
					if getErr != nil {
						return 0, classifyAPIError(getErr, flags)
					}
					return writeExport(writer, format, data, limit)
				}
				if len(args) > 1 {
					data, getErr := c.Get(path, nil)
					if getErr != nil {
						return 0, classifyAPIError(getErr, flags)
					}
					return writeExport(writer, format, data, limit)
				}
				items := make([]json.RawMessage, 0)
				fetched, fetchErr := resumablePaginatedFetch(cmd.Context(), c, path, nil, 100, limit, "", flags.profileName, func(page []json.RawMessage) error {
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
				}, "jsonl", nil)
				if fetchErr != nil {
					return fetched, fetchErr
				}
				if format != "jsonl" {
					data, marshalErr := json.Marshal(items)
					if marshalErr != nil {
						return fetched, marshalErr
					}
					// The reported count stays the fetched count; writeExport
					// would re-count the same records it is rendering.
					if _, writeErr := writeExport(writer, format, data, 0); writeErr != nil {
						return fetched, writeErr
					}
				}
				return fetched, nil
			}

			var count int
			var replaced bool
			if outputFile == "" {
				writer := bufio.NewWriter(cmd.OutOrStdout())
				count, err = runExport(writer)
				// Stdout keeps its historical behaviour: bytes generated before a
				// failure are still flushed, because a stream consumer has already
				// been handed everything up to that point.
				flushErr := writer.Flush()
				if err != nil {
					return err
				}
				if flushErr != nil {
					return fmt.Errorf("flushing export: %w", flushErr)
				}
				return nil
			}
			if flags.dryRun {
				// --dry-run still reads the source so the preview reports a real
				// record count, but it creates no file: no output, no temporary
				// and no <target>.lock sibling.
				if replaced, err = checkAtomicOutputTarget(outputFile); err != nil {
					return err
				}
				if count, err = runExport(bufio.NewWriter(io.Discard)); err != nil {
					return err
				}
			} else {
				lockPath, canonicalTarget, lockErr := outputWriterLockPath(outputFile)
				if lockErr != nil {
					return fmt.Errorf("resolving output path: %w", lockErr)
				}
				// The lock is taken before the first source read so a busy target
				// costs no API traffic, and it covers the atomic publication.
				if err := withPathWriterLock(cmd, lockPath, fmt.Sprintf("export to %q", canonicalTarget), func() error {
					// Re-running an export to the same path is a normal scripted
					// workflow, so an existing file is replaced, never refused; the
					// result says so instead of hiding it.
					if replaced, err = checkAtomicOutputTarget(outputFile); err != nil {
						return err
					}
					// A file is published atomically: a failure leaves whatever
					// artifact was already there instead of truncating it, and nothing
					// is flushed into a temporary file that is about to be discarded.
					return withAtomicOutputFile(outputFile, exportOutputFileMode, func(w io.Writer) error {
						writer := bufio.NewWriter(w)
						count, err = runExport(writer)
						if err != nil {
							return err
						}
						if flushErr := writer.Flush(); flushErr != nil {
							return fmt.Errorf("flushing export: %w", flushErr)
						}
						return nil
					})
				}); err != nil {
					return err
				}
			}
			result := exportFileResult{Output: outputFile, Format: format, Records: count, Replaced: replaced, DryRun: flags.dryRun}
			if flags.asJSON {
				return printCommandJSON(cmd.OutOrStdout(), result, flags)
			}
			fmt.Fprintln(cmd.ErrOrStderr(), result.message())
			return nil
		},
	}

	cmd.Flags().StringVar(&format, "format", "jsonl", "Output format: jsonl or json")
	cmd.Flags().StringVarP(&outputFile, "output", "o", "", "Output file path (default: stdout)")
	cmd.Flags().IntVar(&limit, "limit", 0, "Maximum records to export (0 = unlimited)")
	cmd.Flags().BoolVar(&noCache, "no-cache", false, "Bypass response cache for fresh data")
	// These flags belong to the legacy resource-export form. Keep accepting
	// them for existing scripts, but do not advertise them on the parent
	// command: `export snapshot` has its own, JSONL-only interface.
	_ = cmd.Flags().MarkHidden("format")
	_ = cmd.Flags().MarkHidden("no-cache")

	// paginated/resumable snapshot subcommand.
	cmd.AddCommand(newExportSnapshotCmd(flags))

	return cmd
}
