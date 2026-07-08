// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"github.com/spf13/cobra"
)

func newCollectionsCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "collections",
		Short: "Manage collections in your Zotero library",
	}

	cmd.AddCommand(newCollectionsCreateCmd(flags))
	cmd.AddCommand(newCollectionsDeleteCmd(flags))
	cmd.AddCommand(newCollectionsGetCmd(flags))
	cmd.AddCommand(newCollectionsItemsCmd(flags))
	cmd.AddCommand(newCollectionsListCmd(flags))
	cmd.AddCommand(newCollectionsBundleCmd(flags))
	// Register collection citation-gap discovery.
	cmd.AddCommand(newCollectionsGapsCmd(flags))
	cmd.AddCommand(newCollectionsExportCmd(flags))
	cmd.AddCommand(newCollectionsMoveCmd(flags))
	cmd.AddCommand(newCollectionsStatsCmd(flags))
	cmd.AddCommand(newCollectionsSubcollectionsCmd(flags))
	cmd.AddCommand(newCollectionsTagsCmd(flags))
	cmd.AddCommand(newCollectionsTopCmd(flags))
	cmd.AddCommand(newCollectionsUpdateCmd(flags))
	return cmd
}
