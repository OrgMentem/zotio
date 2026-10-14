// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"strings"

	"github.com/spf13/cobra"

	"zotio/internal/mutation"
)

func newCollectionsDeleteCmd(flags *rootFlags) *cobra.Command {

	cmd := &cobra.Command{
		Use:     "delete <collectionKey>",
		Short:   "Delete a collection (does not delete items)",
		Example: "  zotio collections delete ABCD2345",
		Annotations: map[string]string{
			"zotio:endpoint": "collections.delete",
			"zotio:method":   "DELETE",
			"zotio:path":     "/collections/{collectionKey}",
		},
		// One key per run. Cobra's default for a command with no subcommands is
		// ArbitraryArgs, so `collections delete K1 K2` used to delete K1 and drop
		// K2 on the floor: the path, the op id and the whole envelope are built
		// from args[0] alone, and nothing reported the ignored keys. A
		// destructive command must not silently do less than it was asked, and a
		// missing key is a usage error, not a help request that exits 0.
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := replacePathParam("/collections/{collectionKey}", "collectionKey", args[0])
			// Preview and apply run the same read from the write plane. A 404 is
			// a hard failure by default, not a no-op: the response cannot
			// distinguish a genuinely absent key from a collection created moments
			// ago that has not yet propagated to this plane, and reporting success
			// in that window would falsely tell the caller a live collection is
			// gone (see N4-2, the identical bug in items delete). --ignore-missing
			// is the caller's explicit opt-in to accept that risk for idempotent
			// retries; unlike items, a collection has no trash state to
			// distinguish "already gone" from, so this is the only no-op path
			// collections delete needs.
			c, err := flags.newWriteClient()
			if err != nil {
				return err
			}
			// Add (not Remove) and a bool, not the key string: the mirror is keyed
			// by item key, so a non-string Add guarantees applyChangeToItemData
			// refuses to replay a collection key onto an item, and a bool never
			// satisfies reverse.go's string-scalar inversion check either.
			op := mutation.Op{
				ID:          "collections.delete:" + args[0],
				Key:         args[0],
				Kind:        "collection_delete",
				Changes:     []mutation.Change{{Field: "collection", Add: true}},
				Destructive: true,
			}
			version, err := readCollectionVersion(c, flags, args[0])
			if err != nil {
				if !flags.ignoreMissing || !strings.Contains(err.Error(), "HTTP 404") {
					return err
				}
				op.Changes = nil
				op.Destructive = false
				op.NoOpReason = map[string]any{
					"code":    "already_deleted",
					"message": "collection does not exist on the write plane; --ignore-missing treats this as already done",
				}
			}
			op.ExpectedVersion = version
			var applyErr error
			op.Apply = func() (string, any, error) {
				// Zotero requires If-Unmodified-Since-Version on DELETE (HTTP 428
				// without it).
				_, _, status, detail, delErr := deleteWithVersionGuard(c, path, version)
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
			env, runErr := runMutation(cmd.Context(), flags, "collections.delete", []mutation.Op{op})
			return renderMutationResult(cmd, flags, env, preferApplyErr(runErr, applyErr), collectionWriteLine("delete", "deleted"))
		},
	}

	return cmd
}
