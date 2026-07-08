// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"github.com/spf13/cobra"
)

func newSearchesCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "searches",
		Short: "Manage saved searches in your Zotero library",
	}

	cmd.AddCommand(newSearchesGetCmd(flags))
	cmd.AddCommand(newSearchesListCmd(flags))
	cmd.AddCommand(newSearchesRunCmd(flags))
	cmd.AddCommand(newSearchesMaterializeCmd(flags))
	return cmd
}
