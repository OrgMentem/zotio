// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"github.com/spf13/cobra"
)

func newSchemaCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "schema",
		Short: "Zotero item type and field schema",
	}

	cmd.AddCommand(newSchemaDriftCmd(flags))
	cmd.AddCommand(newSchemaCreatorFieldsCmd(flags))
	cmd.AddCommand(newSchemaItemFieldsCmd(flags))
	cmd.AddCommand(newSchemaItemTypeCreatorTypesCmd(flags))
	cmd.AddCommand(newSchemaItemTypeFieldsCmd(flags))
	cmd.AddCommand(newSchemaItemTypesCmd(flags))
	cmd.AddCommand(newSchemaNewItemTemplateCmd(flags))
	return cmd
}
