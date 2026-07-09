// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"strings"

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
			itemData, err := c.Get(itemPath, nil)
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
				collectionData, err := c.Get(collectionPath, nil)
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
	tw := newTabWriter(cmd.OutOrStdout())
	fmt.Fprintln(tw, strings.Join([]string{bold("KEY"), bold("NAME")}, "\t"))
	for _, row := range rows {
		fmt.Fprintf(tw, "%s\t%s\n", row.Key, sanitizeForTerminal(row.Name))
	}
	return tw.Flush()
}
