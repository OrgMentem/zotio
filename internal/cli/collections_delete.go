// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

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
			// Add (not Remove) and a bool, not the key string: the mirror is keyed
			// by item key, so a non-string Add guarantees applyChangeToItemData
			// refuses to replay a collection key onto an item, and a bool never
			// satisfies reverse.go's string-scalar inversion check either.
			ops := []mutation.Op{{
				ID:          "collections.delete:" + args[0],
				Key:         args[0],
				Kind:        "collection_delete",
				Changes:     []mutation.Change{{Field: "collection", Add: true}},
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
			var (
				applyErr   error
				data       json.RawMessage
				statusCode int
			)
			ops[0].Apply = func() (string, any, error) {
				// Zotero requires If-Unmodified-Since-Version on DELETE (HTTP 428
				// without it). newWriteClient points at the write target, so this
				// version GET and the DELETE hit the same library (the Web API under hybrid routing).
				//
				// A 404 here is a hard failure by default, not a no-op. The response
				// cannot distinguish a genuinely absent key from a collection created
				// moments ago that has not yet propagated to this plane, and reporting
				// success in that window would falsely tell the caller a live
				// collection is gone (see N4-2, the identical bug in items delete).
				// --ignore-missing is the caller's explicit opt-in to accept that risk
				// for idempotent retries; unlike items, a collection has no trash
				// state to distinguish "already gone" from, so this is the only
				// no-op path collections delete needs.
				_, version, verErr := c.GetWithVersion(path, nil)
				if verErr != nil {
					if flags.ignoreMissing && strings.Contains(verErr.Error(), "HTTP 404") {
						return "no_op", map[string]any{
							"code":    "already_deleted",
							"message": "collection does not exist on the write plane; --ignore-missing treats this as already done",
						}, nil
					}
					applyErr = classifyAPIError(verErr, flags)
					return "failed", nil, applyErr
				}
				if version <= 0 {
					applyErr = apiErr(fmt.Errorf("reading collection version for %s: response did not include a version", args[0]))
					return "failed", nil, applyErr
				}
				var (
					status string
					detail any
					delErr error
				)
				data, statusCode, status, detail, delErr = deleteWithVersionGuard(c, path, version)
				if delErr != nil {
					if flags.ignoreMissing && strings.Contains(delErr.Error(), "HTTP 404") {
						return "no_op", map[string]any{
							"code":    "already_deleted",
							"message": "collection was removed between the version read and the write; --ignore-missing treats this as already done",
						}, nil
					}
					applyErr = classifyAPIError(delErr, flags)
					return status, detail, delErr
				}
				return status, detail, nil
			}
			env, runErr := runMutation(cmd.Context(), flags, "collections.delete", ops)
			if runErr != nil {
				if applyErr != nil {
					return applyErr
				}
				return runErr
			}
			// The rest of this command's rendering assumes a real "applied" DELETE
			// populated data/statusCode from the wire. --ignore-missing's no_op
			// never calls DeleteWithHeaders, so those stay zero-valued, and falling
			// through produced {"status":0,"success":false} — a no-op that reads as
			// a failure. Render the standard mutation envelope for that one case,
			// which is where items delete's own no_op already lives; leave the
			// "applied" path exactly as it was; that legacy shape is unrelated to
			// this fix and untouched.
			if env.Result != nil && len(env.Result.Items) > 0 && env.Result.Items[0].Status == "no_op" {
				return renderMutation(cmd, flags, env, nil)
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
