// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"zotio/internal/mutation"
)

func newItemsRestoreCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:         "restore <itemKey>",
		Short:       "Restore a trashed item",
		Annotations: map[string]string{"zotio:method": "PATCH", "zotio:path": "/items/{itemKey}"},
		// One key per run. Cobra's default for a command with no subcommands is
		// ArbitraryArgs, so `items restore K1 K2` used to restore K1 and drop
		// K2 on the floor: the request path, the op id and the whole envelope
		// are built from args[0] alone, and nothing reported the ignored keys.
		// A restore that reports one item as the whole result must not silently
		// do less than it was asked, and a missing key is a usage error, not a
		// help request that exits 0.
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key := args[0]
			path := replacePathParam("/items/{itemKey}", "itemKey", key)

			// Route through the write target so the version GET and the PATCH hit the
			// same library under hybrid routing: an item just created on the web and
			// not yet synced locally still resolves. Preview and apply run the same
			// read, so a missing key exits 3 in both. The route is resolved without
			// persisting or announcing it unless this run applies.
			c, clientErr := flags.newWriteClient()
			if clientErr != nil {
				return clientErr
			}
			// Zotero requires If-Unmodified-Since-Version for key-based writes
			// (PATCH returns HTTP 428 without it). Unlike items delete, a 404
			// here is NOT a no-op: delete's target state ("gone") is already
			// satisfied when the item is missing, but restore's ("present and
			// undeleted") can never be reached, so it is a genuine error.
			_, version, verErr := c.GetWithVersion(path, nil)
			if verErr != nil {
				return classifyAPIError(verErr, flags)
			}
			if version <= 0 {
				return apiErr(fmt.Errorf("reading item version for %s: response did not include a version", key))
			}

			var applyErr error
			ops := []mutation.Op{{
				ID:              "items.restore:" + key,
				Key:             key,
				Kind:            "item_restore",
				ExpectedVersion: version,
				// Boolean on purpose: keeps the mirror from replaying the
				// restore and lets the field stay out of reverse.go's reversibleFields.
				Changes: []mutation.Change{{Field: "deleted", Remove: true}},
				Apply: func() (string, any, error) {
					status, detail, patchErr := patchWithVersionGuard(c, path, map[string]any{"deleted": 0}, version)
					if patchErr != nil {
						applyErr = patchErr
					}
					return status, detail, patchErr
				},
			}}
			// Shared envelope, not a bespoke {action,resource,key,status,success}
			// shape: .result.items[0] works for every other mutation, and the
			// journal run_id was previously discarded so a restore could not be
			// traced even though the journal recorded it.
			env, runErr := runMutation(cmd.Context(), flags, "items.restore", ops)
			if runErr != nil && applyErr != nil {
				runErr = classifyAPIError(applyErr, flags)
			}
			return renderMutationResult(cmd, flags, env, runErr, nil)
		},
	}

	return cmd
}
