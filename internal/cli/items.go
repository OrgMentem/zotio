// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"github.com/spf13/cobra"
)

func newItemsCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "items",
		Short: "Manage items in your Zotero library",
	}

	cmd.AddCommand(newItemsChildrenCmd(flags))
	cmd.AddCommand(newItemsCreateCmd(flags))
	cmd.AddCommand(newItemsNewCmd(flags))
	cmd.AddCommand(newItemsDeleteCmd(flags))
	cmd.AddCommand(newItemsAnnotationsCmd(flags))
	cmd.AddCommand(newItemsAuditCmd(flags))
	cmd.AddCommand(newItemsAuthorsCmd(flags))
	cmd.AddCommand(newItemsCiteCmd(flags))
	cmd.AddCommand(newItemsBibliographyCmd(flags))
	cmd.AddCommand(newItemsCitekeyConflictsCmd(flags))
	cmd.AddCommand(newItemsBibcheckCmd(flags))
	cmd.AddCommand(newItemsCollectionsOfCmd(flags))
	cmd.AddCommand(newItemsDuplicatesCmd(flags))
	cmd.AddCommand(newItemsRelatedCmd(flags))
	cmd.AddCommand(newItemsSimilarCmd(flags))
	// Metadata enrichment/remediation pipeline.
	cmd.AddCommand(newItemsEnrichCmd(flags))
	// Attachment on-disk file-path resolver (local-API file endpoints).
	cmd.AddCommand(newItemsFileCmd(flags))
	cmd.AddCommand(newItemsFindCmd(flags))
	cmd.AddCommand(newItemsFulltextCmd(flags))
	cmd.AddCommand(newItemsMissingPdfCmd(flags))
	cmd.AddCommand(newItemsMoveCmd(flags))
	cmd.AddCommand(newItemsNoteTemplateCmd(flags))
	cmd.AddCommand(newItemsOpenCmd(flags))
	cmd.AddCommand(newItemsPreprintCheckCmd(flags))
	cmd.AddCommand(newItemsRetractCheckCmd(flags))
	cmd.AddCommand(newItemsRecentCmd(flags))
	cmd.AddCommand(newItemsRestoreCmd(flags))
	cmd.AddCommand(newItemsStaleCmd(flags))
	cmd.AddCommand(newItemsSummarizeCmd(flags))
	cmd.AddCommand(newItemsUnfiledCmd(flags))
	cmd.AddCommand(newItemsVenuesCmd(flags))
	cmd.AddCommand(newItemsGetCmd(flags))
	cmd.AddCommand(newItemsListCmd(flags))
	cmd.AddCommand(newItemsTagsCmd(flags))
	cmd.AddCommand(newItemsTopCmd(flags))
	cmd.AddCommand(newItemsTrashCmd(flags))
	cmd.AddCommand(newItemsUpdateCmd(flags))
	return cmd
}
