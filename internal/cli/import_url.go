// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH: Add hand-written URL import workflow missing from the generated CLI.

package cli

import (
	"time"

	"github.com/spf13/cobra"
)

func newImportUrlCmd(flags *rootFlags) *cobra.Command {
	var flagCollection string

	cmd := &cobra.Command{
		Use:         "url <url>",
		Short:       "Import a URL as a webpage item",
		Annotations: map[string]string{"pp:method": "POST", "pp:path": "/items"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}

			item := map[string]any{
				"itemType":   "webpage",
				"title":      args[0],
				"url":        args[0],
				"accessDate": time.Now().Format("2006-01-02"),
			}
			addImportCollection(item, flagCollection)

			c, err := flags.newClient()
			if err != nil {
				return err
			}
			data, _, err := c.Post("/items", []map[string]any{item})
			if err != nil {
				return classifyAPIError(err, flags)
			}
			return printOutputWithFlags(cmd.OutOrStdout(), data, flags)
		},
	}
	cmd.Flags().StringVar(&flagCollection, "collection", "", "Collection key to add the item to")

	return cmd
}
