// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

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
			if mode := resolveMutationMode(flags); !mode.Apply {
				return printJSONFiltered(cmd.OutOrStdout(), map[string]any{
					"action":         "restore",
					"resource":       "items",
					"key":            args[0],
					"status":         0,
					"success":        false,
					"dry_run":        true,
					"preview_reason": mode.PreviewReason,
				}, flags)
			}

			// Route through the write target and supply the version precondition Zotero
			// requires for key-based writes (PATCH returns HTTP 428 without
			// If-Unmodified-Since-Version). Mirrors items update/delete: the version GET
			// and the PATCH hit the same library under hybrid routing, so an item just
			// created on the web and not yet synced locally still resolves.
			c, err := flags.newWriteClient()
			if err != nil {
				return err
			}

			path := replacePathParam("/items/{itemKey}", "itemKey", args[0])

			// The version read stays outside the mutation.Op/Apply closure (as in items
			// update/delete) so a failed read aborts immediately with its own
			// classifyAPIError result rather than the engine's generic "mutation
			// incomplete". Unlike items delete, a 404 here is NOT treated as a no-op:
			// delete's target state ("gone") is already satisfied when the item is
			// missing, but restore's target state ("present and undeleted") can never be
			// reached for an item that doesn't exist, so a missing item is a genuine
			// error here.
			_, version, err := c.GetWithVersion(path, nil)
			if err != nil {
				return classifyAPIError(err, flags)
			}
			if version <= 0 {
				return apiErr(fmt.Errorf("reading item version for %s: response did not include a version", args[0]))
			}
			patchHeaders := map[string]string{"If-Unmodified-Since-Version": strconv.Itoa(version)}

			var (
				applyErr   error
				statusCode int
			)
			ops := []mutation.Op{{
				ID:   "items.restore:" + args[0],
				Key:  args[0],
				Kind: "item_restore",
				// Boolean on purpose: keeps the mirror from replaying the
				// restore and lets the field stay out of reverse.go's reversibleFields.
				Changes: []mutation.Change{{Field: "deleted", Remove: true}},
				Apply: func() (string, any, error) {
					_, statusCode, applyErr = c.PatchWithHeaders(path, map[string]any{"deleted": 0}, patchHeaders)
					if applyErr != nil {
						return "failed", nil, applyErr
					}
					return "applied", nil, nil
				},
			}}
			if _, runErr := runMutation(cmd.Context(), flags, "items.restore", ops); runErr != nil {
				if applyErr != nil {
					return classifyAPIError(applyErr, flags)
				}
				return runErr
			}

			return printJSONFiltered(cmd.OutOrStdout(), map[string]any{
				"action":   "restore",
				"resource": "items",
				"key":      args[0],
				"status":   statusCode,
				"success":  statusCode >= 200 && statusCode < 300,
			}, flags)
		},
	}

	return cmd
}
