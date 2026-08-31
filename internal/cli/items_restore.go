// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"zotio/internal/client"
	"zotio/internal/mutation"
)

func newItemsRestoreCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:         "restore <itemKey>",
		Short:       "Restore a trashed item",
		Annotations: map[string]string{"zotio:method": "PATCH", "zotio:path": "/items/{itemKey}"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}

			path := replacePathParam("/items/{itemKey}", "itemKey", args[0])

			// Route through the write target so the version GET and the PATCH hit the
			// same library under hybrid routing: an item just created on the web and
			// not yet synced locally still resolves. Built only in apply mode, so a
			// preview needs no credentials and a config error keeps its own exit code.
			var writeClient *client.Client
			if resolveMutationMode(flags).Apply {
				var clientErr error
				writeClient, clientErr = flags.newWriteClient()
				if clientErr != nil {
					return clientErr
				}
			}

			var applyErr error
			ops := []mutation.Op{{
				ID:   "items.restore:" + args[0],
				Key:  args[0],
				Kind: "item_restore",
				// Boolean on purpose: keeps the mirror from replaying the
				// restore and lets the field stay out of reverse.go's reversibleFields.
				Changes: []mutation.Change{{Field: "deleted", Remove: true}},
				Apply: func() (string, any, error) {
					// Zotero requires If-Unmodified-Since-Version for key-based writes
					// (PATCH returns HTTP 428 without it). Unlike items delete, a 404
					// here is NOT a no-op: delete's target state ("gone") is already
					// satisfied when the item is missing, but restore's ("present and
					// undeleted") can never be reached, so it is a genuine error.
					_, version, verErr := writeClient.GetWithVersion(path, nil)
					if verErr != nil {
						applyErr = verErr
						return "failed", nil, verErr
					}
					if version <= 0 {
						applyErr = apiErr(fmt.Errorf("reading item version for %s: response did not include a version", args[0]))
						return "failed", nil, applyErr
					}
					status, detail, patchErr := patchWithVersionGuard(writeClient, path, map[string]any{"deleted": 0}, version)
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
			if runErr != nil {
				if applyErr != nil {
					return classifyAPIError(applyErr, flags)
				}
				return runErr
			}
			return renderMutation(cmd, flags, env, nil)
		},
	}

	return cmd
}
