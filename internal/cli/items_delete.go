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

func newItemsDeleteCmd(flags *rootFlags) *cobra.Command {

	cmd := &cobra.Command{
		Use:   "delete <itemKey>",
		Short: "Delete an item (moves to trash)",
		// Use an item key placeholder, not a token.
		Example:     "  zotio items delete ABC12345",
		Annotations: map[string]string{"zotio:endpoint": "items.delete", "zotio:method": "DELETE", "zotio:path": "/items/{itemKey}"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			path := "/items/{itemKey}"
			path = replacePathParam(path, "itemKey", args[0])
			if mode := resolveMutationMode(flags); !mode.Apply {
				return printJSONFiltered(cmd.OutOrStdout(), map[string]any{
					"action":         "delete",
					"resource":       "items",
					"key":            args[0],
					"path":           path,
					"status":         0,
					"success":        false,
					"dry_run":        true,
					"preview_reason": mode.PreviewReason,
				}, flags)
			}
			c, err := flags.newWriteClient()
			if err != nil {
				return err
			}
			var (
				applyErr    error
				alreadyGone bool
				data        json.RawMessage
				statusCode  int
			)
			ops := []mutation.Op{{
				ID:   "items.delete:" + args[0],
				Key:  args[0],
				Kind: "item_delete",
				// Boolean on purpose: the mirror must never replay a trash, and
				// "deleted" isn't in reverse.go's reversibleFields, so undo correctly refuses it.
				Changes: []mutation.Change{{Field: "deleted", Add: true}},
				Apply: func() (string, any, error) {
					// Zotero requires If-Unmodified-Since-Version on DELETE (HTTP 428
					// without it). newWriteClient points at the write target, so this version
					// GET and the DELETE hit the same library (the Web API under hybrid routing)
					// — correct even for an item just created on the web and not yet synced local.
					delHeaders := map[string]string{}
					_, version, verErr := c.GetWithVersion(path, nil)
					if verErr != nil {
						var apiErr *client.APIError
						if errors.As(verErr, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
							alreadyGone = true
							return "no_op", nil, nil
						}
						applyErr = verErr
						return "failed", nil, verErr
					}
					if version <= 0 {
						applyErr = apiErr(fmt.Errorf("reading item version for %s: response did not include a version", args[0]))
						return "failed", nil, applyErr
					}
					delHeaders["If-Unmodified-Since-Version"] = strconv.Itoa(version)
					var delErr error
					data, statusCode, delErr = c.DeleteWithHeaders(path, delHeaders)
					if delErr != nil {
						applyErr = delErr
						return "failed", nil, delErr
					}
					return "applied", nil, nil
				},
			}}
			if _, runErr := runMutation(cmd.Context(), flags, "items.delete", ops); runErr != nil {
				if applyErr != nil {
					return classifyDeleteError(applyErr, flags)
				}
				return runErr
			}
			if alreadyGone {
				return writeNoop(cmd.OutOrStdout(), cmd.ErrOrStderr(), flags, "already_deleted", "already deleted (no-op)")
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
					"resource": "items",
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
