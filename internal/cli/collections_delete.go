// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"fmt"
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
		// One key per run. Cobra's default for a command with no subcommands is
		// ArbitraryArgs, so `collections delete K1 K2` used to delete K1 and drop
		// K2 on the floor: the path, the op id and the whole envelope are built
		// from args[0] alone, and nothing reported the ignored keys. A
		// destructive command must not silently do less than it was asked, and a
		// missing key is a usage error, not a help request that exits 0.
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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
			var applyErr error
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
				_, _, status, detail, delErr = deleteWithVersionGuard(c, path, version)
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
			return renderMutation(cmd, flags, env, nil)
		},
	}

	return cmd
}
