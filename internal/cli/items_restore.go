// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import "github.com/spf13/cobra"

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
			_, statusCode, err := c.Patch(path, map[string]any{"deleted": 0})
			if err != nil {
				return classifyAPIError(err, flags)
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
