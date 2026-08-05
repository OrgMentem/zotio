// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"zotio/internal/mutation"
)

func newImportCmd(flags *rootFlags) *cobra.Command {
	var inputFile string
	var batchSize int

	cmd := &cobra.Command{
		Use:   "import <resource>",
		Short: "Import data from JSONL file via API create/upsert calls",
		Long: `Import data from a JSONL file by issuing one POST request per record.
Each line must be a valid JSON object. Failed records are logged to stderr
but do not stop processing the import.

The import previews by default and writes only under --yes; --dry-run always
wins over --yes. Every parsed record counts against --max-changes.`,
		Example: `  # Preview an import without sending anything (the default)
  zotio import <resource> --input data.jsonl

  # Apply the import
  zotio import <resource> --input data.jsonl --yes

  # Import from stdin
  cat data.jsonl | zotio import <resource> --input - --yes`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resource := args[0]
			path := "/" + resource

			var reader io.Reader
			if inputFile == "-" || inputFile == "" {
				// A stdio MCP server pipes the JSON-RPC transport over
				// os.Stdin; reading it directly here would corrupt that
				// session. cmd.InOrStdin() honors whatever the caller (a
				// real terminal, or a test/MCP-installed reader) actually set.
				reader = cmd.InOrStdin()
			} else {
				f, err := os.Open(inputFile)
				if err != nil {
					return fmt.Errorf("opening input file: %w", err)
				}
				defer f.Close()
				reader = f
			}

			scanner := bufio.NewScanner(reader)
			scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB line buffer

			// The whole input is parsed up front, before any gate check or
			// network call, so every well-formed line becomes a mutation.Op.
			// Blank/comment lines are tallied as "skipped" and never become an
			// op; a line that fails to parse is tallied as "failed" here (there
			// is nothing to gate, apply, or journal for it) without aborting
			// the scan of the remaining lines.
			var records []map[string]any
			var parseFailed, skipped int
			for scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())
				if line == "" || line[0] == '#' {
					skipped++
					continue
				}

				var body map[string]any
				if err := json.Unmarshal([]byte(line), &body); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: skipping invalid JSON line: %v\n", err)
					parseFailed++
					continue
				}
				records = append(records, body)
			}
			if err := scanner.Err(); err != nil {
				return fmt.Errorf("reading input: %w", err)
			}

			// The client is resolved only when the run will actually apply, so
			// a preview (the default) never loads config or touches the
			// hybrid local/web write-route resolver.
			var writeClient importApplyPoster
			if resolveMutationMode(flags).Apply {
				c, err := flags.newClient()
				if err != nil {
					return err
				}
				writeClient = c
			}

			ops := make([]mutation.Op, len(records))
			for i, record := range records {
				record := record
				ops[i] = mutation.Op{
					ID:      fmt.Sprintf("import.%s:%03d", resource, i+1),
					Kind:    resource + "_create",
					Changes: []mutation.Change{{Field: "record", Add: record}},
					Apply: func() (string, any, error) {
						if _, _, err := writeClient.Post(path, record); err != nil {
							return "failed", nil, classifyAPIError(err, flags)
						}
						return "applied", nil, nil
					},
				}
			}

			// Unlike import file, records here are posted one at a time (there
			// is no batch endpoint for an arbitrary resource), so a rejected
			// record can never un-submit records already sent alongside it.
			// ContinueOnError keeps the mutation run going past a failed
			// record instead of stopping at the first one, mirroring the
			// tolerant per-line semantics the parser above already applies.
			env, runErr := runMutation(cmd.Context(), flags, "import", ops, func(o *mutation.Options) {
				o.ContinueOnError = true
			})
			if env.Error != nil {
				// A gate rejection (max-changes/destructive) never touched the
				// network and has no per-record result to fold into the
				// succeeded/failed/skipped envelope; surface it directly.
				return fmt.Errorf("%s", env.Error.Message)
			}

			succeeded, failed := 0, parseFailed
			if env.Result != nil {
				succeeded = env.Result.Summary.Applied
				// Failed/Conflicts are per-record apply outcomes; NotAttempted
				// covers records left unattempted after a context cancellation
				// mid-run. All three mean the record did not succeed, so they
				// all fold into "failed" -- otherwise a canceled run could
				// under-report and exit 0 despite dropped records.
				failed += env.Result.Summary.Failed + env.Result.Summary.Conflicts + env.Result.Summary.NotAttempted
			}

			// JSON envelope: {succeeded, failed, skipped}.
			if flags.asJSON {
				if err := printJSONFiltered(cmd.OutOrStdout(), map[string]any{
					"succeeded": succeeded,
					"failed":    failed,
					"skipped":   skipped,
				}, flags); err != nil {
					return err
				}
			} else {
				fmt.Fprintf(cmd.ErrOrStderr(), "Import complete: %d succeeded, %d failed, %d skipped\n", succeeded, failed, skipped)
			}

			if failed > 0 {
				if runErr != nil {
					return fmt.Errorf("import incomplete: one or more records failed: %w", runErr)
				}
				return errors.New("import incomplete: one or more records failed")
			}
			return runErr
		},
	}

	cmd.Flags().StringVarP(&inputFile, "input", "i", "", "Input JSONL file path (use - for stdin)")
	_ = cmd.MarkFlagRequired("input")
	cmd.Flags().IntVar(&batchSize, "batch-size", 1, "Records per batch (future: batch API support)")

	cmd.AddCommand(newImportDoiCmd(flags))
	cmd.AddCommand(newImportUrlCmd(flags))
	cmd.AddCommand(newImportFileCmd(flags))
	cmd.AddCommand(newImportScanCmd(flags))
	cmd.AddCommand(newImportPmidCmd(flags))
	cmd.AddCommand(newImportArxivCmd(flags))
	cmd.AddCommand(newImportIsbnCmd(flags))
	// Reviewable-import pipeline.
	cmd.AddCommand(newImportResolveCmd(flags))
	cmd.AddCommand(newImportDiscoverCmd(flags))
	cmd.AddCommand(newImportApplyCmd(flags))
	// Connector-backed PDF recognition and diagnostics.
	cmd.AddCommand(newImportPDFCmd(flags))
	cmd.AddCommand(newImportTargetsCmd(flags))
	cmd.AddCommand(newImportTranslatorsCmd(flags))

	return cmd
}
