// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"zotio/internal/mutation"
)

func newCollectionsUpdateCmd(flags *rootFlags) *cobra.Command {
	var bodyName string
	var bodyParentCollection string
	var stdinBody bool

	cmd := &cobra.Command{
		Use:   "update <collectionKey>",
		Short: "Update a collection",
		// use a collection key placeholder, not a token.
		Example: "  zotio collections update COLLECTIONKEY",
		Annotations: map[string]string{
			"zotio:endpoint":                   "collections.update",
			"zotio:method":                     "PUT",
			"zotio:path":                       "/collections/{collectionKey}",
			"zotio:destructive":                "false",
			"zotio:supports-dry-run":           "true",
			"zotio:requires-allow-destructive": "false",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}

			path := "/collections/{collectionKey}"
			path = replacePathParam(path, "collectionKey", args[0])
			var body map[string]any
			if stdinBody {
				stdinData, err := io.ReadAll(cmd.InOrStdin())
				if err != nil {
					return fmt.Errorf("reading stdin: %w", err)
				}
				var jsonBody map[string]any
				if err := json.Unmarshal(stdinData, &jsonBody); err != nil {
					return fmt.Errorf("parsing stdin JSON: %w", err)
				}
				body = jsonBody
			} else {
				body = map[string]any{}
				if bodyName != "" {
					body["name"] = bodyName
				}
				if cmd.Flags().Changed("parent-collection") {
					if bodyParentCollection == "" || strings.EqualFold(bodyParentCollection, "false") {
						body["parentCollection"] = false
					} else {
						body["parentCollection"] = bodyParentCollection
					}
				}
			}
			ops := []mutation.Op{{
				ID:      "collections.update:" + args[0],
				Key:     args[0],
				Kind:    "collection_update",
				Changes: []mutation.Change{{Field: "collection", Add: body}},
			}}
			// Preview unless the caller explicitly applied. The body is planned
			// first so the preview shows exactly what apply would send.
			if mode := resolveMutationMode(flags); !mode.Apply {
				return printJSONFiltered(cmd.OutOrStdout(), map[string]any{
					"action":         "update",
					"resource":       "collections",
					"key":            args[0],
					"path":           path,
					"body":           body,
					"status":         0,
					"success":        false,
					"dry_run":        true,
					"preview_reason": mode.PreviewReason,
				}, flags)
			}
			if gateFailure := mutation.CheckGates(mutationOptions(flags), ops); gateFailure != nil {
				return fmt.Errorf("%s", gateFailure.Message)
			}
			// Read from the write target immediately before the PUT so the
			// precondition protects against concurrent collection edits. This stays
			// outside the gated Apply closure (mirrors items update): it is not a
			// change to record, and a failed read must abort with its own
			// classifyAPIError result, not the engine's generic "mutation incomplete".
			c, err := flags.newWriteClient()
			if err != nil {
				return err
			}
			_, version, err := c.GetWithVersion(path, nil)
			if err != nil {
				return classifyAPIError(err, flags)
			}
			// Only the PUT itself is the write; routing just this call through the
			// mutation engine journals it and (best-effort) mirror-replays it.
			var data json.RawMessage
			var statusCode int
			var applyErr error
			ops[0].Apply = func() (string, any, error) {
				var (
					status string
					detail any
					putErr error
				)
				data, statusCode, status, detail, putErr = putWithVersionGuard(c, path, body, version)
				if putErr != nil {
					applyErr = putErr
				}
				return status, detail, putErr
			}
			if _, runErr := runMutation(cmd.Context(), flags, "collections.update", ops); runErr != nil {
				if applyErr != nil {
					return applyErr
				}
				return runErr
			}
			if wantsHumanTable(cmd.OutOrStdout(), flags) {
				// Check if response contains an array (directly or wrapped in "data")
				var items []map[string]any
				if json.Unmarshal(data, &items) == nil && len(items) > 0 {
					if err := printAutoTable(cmd.OutOrStdout(), items); err != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "warning: table rendering failed, falling back to JSON: %v\n", err)
					} else {
						return nil
					}
				} else {
					var wrapped struct {
						Data []map[string]any `json:"data"`
					}
					if json.Unmarshal(data, &wrapped) == nil && len(wrapped.Data) > 0 {
						if err := printAutoTable(cmd.OutOrStdout(), wrapped.Data); err != nil {
							fmt.Fprintf(cmd.ErrOrStderr(), "warning: table rendering failed, falling back to JSON: %v\n", err)
						} else {
							return nil
						}
					}
				}
			}
			if flags.asJSON || !isTerminal(cmd.OutOrStdout()) {
				if flags.quiet {
					return nil
				}
				// Apply --compact and --select to the API response before wrapping.
				// --select wins when both are set: explicit field choice trumps the
				// generic high-gravity allow-list. Otherwise --compact still applies
				// when --agent is on but the user did not name fields.
				filtered := data
				if flags.selectFields != "" {
					filtered = filterFields(filtered, flags.selectFields)
				} else if flags.compact {
					filtered = compactFields(filtered)
				}
				envelope := map[string]any{
					"action":   "put",
					"resource": "collections",
					"path":     path,
					"status":   statusCode,
					"success":  statusCode >= 200 && statusCode < 300,
				}
				if flags.dryRun {
					envelope["dry_run"] = true
					envelope["status"] = 0
					envelope["success"] = false
				}
				if len(filtered) > 0 {
					var parsed any
					if err := json.Unmarshal(filtered, &parsed); err == nil {
						envelope["data"] = parsed
					}
				}
				envelopeJSON, err := json.Marshal(envelope)
				if err != nil {
					return err
				}
				return printOutput(cmd.OutOrStdout(), json.RawMessage(envelopeJSON), true)
			}
			return printOutputWithFlags(cmd.OutOrStdout(), data, flags)
		},
	}
	cmd.Flags().StringVar(&bodyName, "name", "", "New collection name")
	cmd.Flags().StringVar(&bodyParentCollection, "parent-collection", "", "New parent collection key (false or empty for top-level)")
	cmd.Flags().BoolVar(&stdinBody, "stdin", false, "Read request body as JSON from stdin")

	return cmd
}
