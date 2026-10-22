// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"zotio/internal/mutation"
)

func newItemsRestoreCmd(flags *rootFlags) *cobra.Command {
	var keysFrom string

	cmd := &cobra.Command{
		Use:         "restore <itemKey...> | --keys-from <file|->",
		Short:       "Restore one or more trashed items",
		Long:        "Restore trashed items. Pass one or more item keys, or --keys-from (a file or '-' for stdin). Each key is its own version-guarded write inside one plan and one journal run. Positional keys and --keys-from cannot be combined.",
		Example:     "  zotio items restore ABC12345\n  zotio items restore --keys-from keys.txt --yes",
		Annotations: map[string]string{"zotio:method": "PATCH", "zotio:path": "/items/{itemKey}"},
		// Positional keys or --keys-from, never both and never none: resolveKeys
		// refuses the mix and an empty list as usage errors, so a restore cannot
		// silently do less than it was asked.
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			keys, keysErr := resolveTrashKeys(args, keysFrom, cmd.InOrStdin())
			if keysErr != nil {
				return keysErr
			}

			// Route through the write target so the version GET and the PATCH hit the
			// same library under hybrid routing: an item just created on the web and
			// not yet synced locally still resolves. Preview and apply run the same
			// read, so a missing key exits 3 in both. The route is resolved without
			// persisting or announcing it unless this run applies.
			c, clientErr := flags.newWriteClient()
			if clientErr != nil {
				return clientErr
			}

			var applyErr error
			ops := make([]mutation.Op, 0, len(keys))
			for _, key := range keys {
				path := replacePathParam("/items/{itemKey}", "itemKey", key)
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
				ops = append(ops, mutation.Op{
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
				})
			}
			// Shared envelope, not a bespoke {action,resource,key,status,success}
			// shape: .result.items[i] works for every other mutation.
			env, runErr := runMutation(cmd.Context(), flags, "items.restore", ops)
			if runErr != nil && applyErr != nil {
				runErr = classifyAPIError(applyErr, flags)
			}
			return renderMutationResult(cmd, flags, env, runErr, nil)
		},
	}
	cmd.Flags().StringVar(&keysFrom, "keys-from", "", "Read item keys from a file or '-' for stdin (lines or JSON), instead of positional keys")

	return cmd
}
