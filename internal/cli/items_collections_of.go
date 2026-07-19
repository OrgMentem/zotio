// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"

	"github.com/spf13/cobra"
)

type itemCollectionRow struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}

func newItemsCollectionsOfCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:         "collections-of <itemKey>",
		Short:       "Show collections containing an item",
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			c, err := flags.newClient()
			if err != nil {
				return err
			}

			itemPath := replacePathParam("/items/{itemKey}", "itemKey", args[0])
			itemData, _, err := resolveRead(cmd.Context(), c, flags, "items", false, itemPath, nil, nil)
			if err != nil {
				return classifyAPIError(err, flags)
			}
			collectionKeys, err := itemCollections(itemData)
			if err != nil {
				return err
			}

			rows := make([]itemCollectionRow, 0, len(collectionKeys))
			for _, key := range collectionKeys {
				collectionPath := replacePathParam("/collections/{collectionKey}", "collectionKey", key)
				collectionData, _, err := resolveRead(cmd.Context(), c, flags, "collections", false, collectionPath, nil, nil)
				if err != nil {
					return classifyAPIError(err, flags)
				}
				rows = append(rows, itemCollectionRow{Key: key, Name: jsonStringField(collectionData, "name")})
			}

			if flags.asJSON || !isTerminal(cmd.OutOrStdout()) {
				data, err := json.Marshal(rows)
				if err != nil {
					return err
				}
				return printOutput(cmd.OutOrStdout(), json.RawMessage(data), true)
			}
			return printItemCollectionsTable(cmd, rows)
		},
	}

	return cmd
}

func printItemCollectionsTable(cmd *cobra.Command, rows []itemCollectionRow) error {
	tableRows := make([][]string, 0, len(rows))
	for _, row := range rows {
		tableRows = append(tableRows, []string{row.Key, sanitizeForTerminal(row.Name)})
	}
	return renderColumns(cmd.OutOrStdout(), []string{"key", "name"}, tableRows)
}
