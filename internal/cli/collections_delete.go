// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"

	"github.com/spf13/cobra"

	"zotio/internal/client"
	"zotio/internal/mutation"
)

func newCollectionsDeleteCmd(flags *rootFlags) *cobra.Command {

	cmd := &cobra.Command{
		Use:   "delete <collectionKey>",
		Short: "Delete a collection (does not delete items)",
		// use a collection key placeholder, not a token.
		Example: "  zotio collections delete COLLECTIONKEY",
		Annotations: map[string]string{
			"zotio:endpoint":                   "collections.delete",
			"zotio:method":                     "DELETE",
			"zotio:path":                       "/collections/{collectionKey}",
			"zotio:destructive":                "true",
			"zotio:supports-dry-run":           "true",
			"zotio:requires-allow-destructive": "true",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			path := "/collections/{collectionKey}"
			path = replacePathParam(path, "collectionKey", args[0])
			ops := []mutation.Op{{
				ID:          "collections.delete:" + args[0],
				Key:         args[0],
				Kind:        "collection_delete",
				Changes:     []mutation.Change{{Field: "collection", Remove: args[0]}},
				Destructive: true,
			}}
			// Preview unless the caller explicitly applied. MCP advertises the
			// write-safety gate flags on every mutating command, so a command
			// that writes on invocation is a false affordance: a host told the
			// gates exist would auto-approve a delete that never honored them.
			if mode := resolveMutationMode(flags); !mode.Apply {
				return printJSONFiltered(cmd.OutOrStdout(), map[string]any{
					"action":         "delete",
					"resource":       "collections",
					"key":            args[0],
					"path":           path,
					"status":         0,
					"success":        false,
					"dry_run":        true,
					"preview_reason": mode.PreviewReason,
				}, flags)
			}
			if gateFailure := mutation.CheckGates(mutationOptions(flags), ops); gateFailure != nil {
				return fmt.Errorf("%s", gateFailure.Message)
			}
			c, err := flags.newWriteClient()
			if err != nil {
				return err
			}
			// Zotero requires If-Unmodified-Since-Version on DELETE (HTTP 428
			// without it). newWriteClient points at the write target, so this
			// version GET and the DELETE hit the same library (the Web API under hybrid routing).
			delHeaders := map[string]string{}
			_, version, err := c.GetWithVersion(path, nil)
			if err != nil {
				var apiErr *client.APIError
				if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
					return writeNoop(cmd.OutOrStdout(), cmd.ErrOrStderr(), flags, "already_deleted", "already deleted (no-op)")
				}
				return classifyAPIError(err, flags)
			}
			if version <= 0 {
				return apiErr(fmt.Errorf("reading collection version for %s: response did not include a version", args[0]))
			}
			delHeaders["If-Unmodified-Since-Version"] = strconv.Itoa(version)
			data, statusCode, err := c.DeleteWithHeaders(path, delHeaders)
			if err != nil {
				return classifyDeleteError(err, flags)
			}
			if wantsHumanTable(cmd.OutOrStdout(), flags) {
				// Check if response contains an array (directly or wrapped in "data")
				var items []map[string]any
				if json.Unmarshal(data, &items) == nil && len(items) > 0 {
					if err := printAutoTable(cmd.OutOrStdout(), items); err != nil {
						fmt.Fprintf(os.Stderr, "warning: table rendering failed, falling back to JSON: %v\n", err)
					} else {
						return nil
					}
				} else {
					var wrapped struct {
						Data []map[string]any `json:"data"`
					}
					if json.Unmarshal(data, &wrapped) == nil && len(wrapped.Data) > 0 {
						if err := printAutoTable(cmd.OutOrStdout(), wrapped.Data); err != nil {
							fmt.Fprintf(os.Stderr, "warning: table rendering failed, falling back to JSON: %v\n", err)
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
					"action":   "delete",
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

	return cmd
}
