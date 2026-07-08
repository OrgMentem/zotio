// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"github.com/spf13/cobra"
)

func newTagsCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tags",
		Short: "Manage tags across your Zotero library",
	}

	cmd.AddCommand(newTagsGetCmd(flags))
	cmd.AddCommand(newTagsListCmd(flags))
	cmd.AddCommand(newTagsAuditCmd(flags))
	cmd.AddCommand(newTagsInventoryCmd(flags))
	cmd.AddCommand(newTagsRenameCmd(flags))
	return cmd
}
