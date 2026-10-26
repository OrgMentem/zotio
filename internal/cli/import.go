// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"zotio/internal/mutation"
)

// importFailure attributes one failed record to its input line.
type importFailure struct {
	Record int    `json:"record,omitempty"`
	Line   int    `json:"line"`
	Reason string `json:"reason"`
}

func reasonString(r any) string {
	switch v := r.(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		data, _ := json.Marshal(v)
		return string(data)
	}
}

// importRejection reads a Zotero write response and returns the message for
// a record listed under "failed" (HTTP 200 can still reject objects).
func importRejection(raw json.RawMessage) string {
	var resp struct {
		Failed map[string]struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"failed"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &resp) != nil || len(resp.Failed) == 0 {
		return ""
	}
	keys := make([]string, 0, len(resp.Failed))
	for k := range resp.Failed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	f := resp.Failed[keys[0]]
	return fmt.Sprintf("zotero rejected the record (code %d): %s", f.Code, f.Message)
}

func newImportCmd(flags *rootFlags) *cobra.Command {
	var inputFile string

	cmd := &cobra.Command{
		Use:   "import <resource>",
		Short: "Import data from JSONL file via API create/upsert calls",
		Long: `Import data from a JSONL file by issuing one POST request per record.
Each line must be a valid JSON object. Failed records are logged to stderr
but do not stop processing the import.
Supported resources: items, collections, and searches.

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
			switch resource {
			case "items", "collections", "searches":
			default:
				return usageErr(fmt.Errorf("unsupported import resource %q; valid resources: items, collections, searches", resource))
			}
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
					return usageErr(fmt.Errorf("opening input file: %w", err))
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
			var recordLines []int
			var parseFailures []importFailure
			var skipped, lineNo int
			for scanner.Scan() {
				lineNo++
				line := strings.TrimSpace(scanner.Text())
				if line == "" || line[0] == '#' {
					skipped++
					continue
				}

				var body map[string]any
				if err := json.Unmarshal([]byte(line), &body); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "line %d: skipping invalid JSON: %v\n", lineNo, err)
					parseFailures = append(parseFailures, importFailure{Line: lineNo, Reason: "invalid JSON: " + err.Error()})
					continue
				}
				// JSON null decodes into a nil map without an error, but it is
				// not a JSON object and must not become a create op.
				if body == nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "line %d: skipping invalid JSON: not a JSON object\n", lineNo)
					parseFailures = append(parseFailures, importFailure{Line: lineNo, Reason: "invalid JSON: not a JSON object"})
					continue
				}
				records = append(records, body)
				recordLines = append(recordLines, lineNo)
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
				position := fmt.Sprintf("%d", i)
				ops[i] = mutation.Op{
					ID:   fmt.Sprintf("import.%s:%03d", resource, i+1),
					Kind: resource + "_create",
					Changes: []mutation.Change{
						{Field: "endpoint", Add: "POST " + path},
						{Field: "record", Add: record},
					},
					Apply: func() (string, any, error) {
						// Zotero write endpoints take an array of objects.
						raw, _, err := writeClient.PostWithHeaders(path, []any{record}, map[string]string{
							"Zotero-Write-Token": createBatchWriteToken("import."+resource, position, record),
						})
						if err != nil {
							if isCommittedCreateConflict(err) {
								return "conflict", map[string]any{
									"committed": true, "outcome": "unknown",
									"message": ambiguousCreateRefusal("import."+resource, "record", err).Error(),
								}, err
							}
							return "failed", nil, classifyAPIError(err, flags)
						}
						if reason := importRejection(raw); reason != "" {
							return "failed", nil, errors.New(reason)
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
				return renderMutationResult(cmd, flags, env, runErr, nil)
			}

			if env.Result == nil {
				// Preview: show the plan, never an "Import complete" line.
				if err := renderImportPreview(cmd, flags, env, runErr, path, records, recordLines); err != nil {
					return err
				}
				// Unparseable lines were already attributed on stderr; the
				// preview must not exit 0 for input apply would reject.
				if len(parseFailures) > 0 {
					return fmt.Errorf("import preview: %d input line(s) are not valid JSON", len(parseFailures))
				}
				return nil
			}

			succeeded := env.Result.Summary.Applied
			failures := append([]importFailure(nil), parseFailures...)
			opIndex := make(map[string]int, len(ops))
			for i := range ops {
				opIndex[ops[i].ID] = i
			}
			for _, item := range env.Result.Items {
				if item.Status == "applied" || item.Status == "no_op" {
					continue
				}
				i, ok := opIndex[item.OpID]
				if !ok {
					continue
				}
				reason := reasonString(item.Reason)
				if reason == "" {
					reason = item.Status
				}
				failures = append(failures, importFailure{Record: i + 1, Line: recordLines[i], Reason: reason})
			}
			// Failed/Conflicts/NotAttempted all mean the record did not
			// succeed; counting them keeps a canceled run from exiting 0.
			failed := len(failures)
			for _, f := range failures {
				if f.Record > 0 {
					fmt.Fprintf(cmd.ErrOrStderr(), "record %d (line %d): %s\n", f.Record, f.Line, SanitizeForTerminal(f.Reason))
				}
			}

			if failed > 0 && runErr == nil {
				runErr = errors.New("import incomplete: one or more records failed")
			}
			if len(parseFailures) > 0 {
				env.OK = false
			}
			if flags.asJSON || !isTerminal(cmd.OutOrStdout()) {
				if failures == nil {
					failures = []importFailure{}
				}
				// Retain record counters as additive fields on the standard
				// mutation envelope, including its durable journal reference.
				output := struct {
					mutation.Envelope
					Succeeded int             `json:"succeeded"`
					Failed    int             `json:"failed"`
					Skipped   int             `json:"skipped"`
					Failures  []importFailure `json:"failures"`
				}{env, succeeded, failed, skipped, failures}
				if !flags.quiet {
					if err := printJSONFiltered(cmd.OutOrStdout(), output, flags); err != nil {
						return err
					}
				}
				if runErr != nil {
					return markRendered(runErr)
				}
				return nil
			}
			return renderMutationResult(cmd, flags, env, runErr, nil)
		},
	}

	cmd.Flags().StringVarP(&inputFile, "input", "i", "", "Input JSONL file path (use - for stdin)")
	_ = cmd.MarkFlagRequired("input")

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
	cmd.AddCommand(newImportMonitorCmd(flags))
	cmd.AddCommand(newImportApplyCmd(flags))
	// Connector-backed PDF recognition and diagnostics.
	cmd.AddCommand(newImportPDFCmd(flags))
	cmd.AddCommand(newImportTargetsCmd(flags))
	cmd.AddCommand(newImportTranslatorsCmd(flags))

	return cmd
}

// renderImportPreview prints the plan for `import <resource>` without --yes:
// the mutation envelope for JSON or piped output, otherwise one row per
// record so the operator can review exactly what --yes would POST.
func renderImportPreview(cmd *cobra.Command, flags *rootFlags, env mutation.Envelope, runErr error, path string, records []map[string]any, recordLines []int) error {
	if flags.asJSON || !isTerminal(cmd.OutOrStdout()) {
		return renderMutationResult(cmd, flags, env, runErr, nil)
	}
	if flags.quiet {
		return runErr
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Plan: %d planned · POST %s (preview; pass --yes to apply)\n", env.Plan.Summary.Planned, path)
	for i, record := range records {
		if i == 50 {
			fmt.Fprintf(out, "… %d more\n", len(records)-50)
			break
		}
		data, _ := json.Marshal(record)
		text := string(data)
		if len(text) > 100 {
			text = text[:100] + "…"
		}
		fmt.Fprintln(out, SanitizeForTerminal(fmt.Sprintf("  [planned] record %d (line %d): %s", i+1, recordLines[i], text)))
	}
	return runErr
}
