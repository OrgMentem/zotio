// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"github.com/spf13/cobra"
)

func newCollectionsCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "collections",
		Short: "Manage collections in your Zotero library",
		Long: `Manage collections in your Zotero library.

This group covers the collections themselves (create, update, move, delete,
list, export); it does not add items to a collection. To add an item, use
'zotio items move --to <collectionKey>' (by key, also --from and --keys-from
for many items at once) or 'zotio items add-to-collection <itemKey>
--collection-name <name>' (by name, creating the collection if it does not
exist yet).`,
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
