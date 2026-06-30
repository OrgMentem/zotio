// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH: expose Zotero desktop Connector save targets for collection routing.

package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newImportTargetsCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "targets",
		Short: "List Zotero desktop Connector save targets",
		Long: `List Zotero desktop Connector save targets for connector-backed imports.
Use the id column (for example C78) with --connector-target when --collection is
ambiguous or the local Web API collection key cannot be mapped automatically.`,
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			conn, err := flags.newConnector()
			if err != nil {
				return preconditionErr(err)
			}
			selected, err := conn.SelectedCollection(cmd.Context())
			if err != nil {
				return preconditionErr(fmt.Errorf("desktop connector is not reachable: %w", err))
			}
			targets := connectorTargetPaths(selected)
			if flags.asJSON || flags.agent {
				return printJSONFiltered(cmd.OutOrStdout(), map[string]any{
					"library_id":    selected.LibraryID,
					"library_name":  selected.LibraryName,
					"selected_id":   selected.SelectedID,
					"selected_name": selected.SelectedName,
					"targets":       targets,
				}, flags)
			}
			rows := make([][]string, 0, len(targets))
			for _, target := range targets {
				rows = append(rows, []string{target.ID, target.Path, fmt.Sprintf("%t", target.FilesEditable)})
			}
			return flags.printTable(cmd, []string{"ID", "PATH", "FILES"}, rows)
		},
	}
	return cmd
}
