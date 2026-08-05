// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
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

			c, err := flags.newClient()
			if err != nil {
				return err
			}

			path := replacePathParam("/items/{itemKey}", "itemKey", args[0])
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
					_, statusCode, applyErr = c.Patch(path, map[string]any{"deleted": 0})
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
