// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// PATCH: Add hand-written URL import workflow missing from the generated CLI.

package cli

import (
	"fmt"
	"net/http"

	"github.com/spf13/cobra"
)

func newImportUrlCmd(flags *rootFlags) *cobra.Command {
	var flagCollection string
	var flagFetchPDF bool

	cmd := &cobra.Command{
		Use:   "url <url>",
		Short: "Import a URL as a metadata-enriched item",
		Long: `Import a URL, resolving real metadata where possible.

If the URL embeds a DOI, full metadata is fetched from CrossRef. Otherwise the
page's embedded metadata (citation_*, Open Graph, Dublin Core meta tags) is
mapped into a typed item with title, creators, abstract, and publication venue.
A bare webpage item is used only when no metadata is available.

Use --dry-run to preview the proposed item without writing it.`,
		Annotations: map[string]string{"zotio:method": "POST", "zotio:path": "/items"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}

			httpClient := &http.Client{Timeout: enrichTimeout(flags.timeout)}
			item, source := buildImportItemFromURL(cmd.Context(), httpClient, args[0])
			addImportCollection(item, flagCollection)

			if flags.dryRun {
				return printImportDryRun(cmd, item, source, flags)
			}

			c, err := flags.newClient()
			if err != nil {
				return err
			}
			// PATCH: route item creates through the desktop connector when available.
			res, err := routeCreateItem(cmd.Context(), flags, c, item, itemCreateSourceURI(item), cmd.Flags().Changed("collection"))
			if err != nil {
				return err
			}
			if flagFetchPDF {
				if res.Via != "connector" {
					return preconditionErr(fmt.Errorf("--fetch-pdf requires the desktop connector; use --via connector"))
				}
				attachResolverPDF(cmd.Context(), flags, &res)
			}
			if res.Via == "connector" {
				refreshItemsFromLocalAPI(cmd.Context(), flags)
			}
			return printCreateResult(cmd, flags, res, res.WebData)
		},
	}
	cmd.Flags().StringVar(&flagCollection, "collection", "", "Collection key to add the item to")
	cmd.Flags().BoolVar(&flagFetchPDF, "fetch-pdf", false, "Attach an open-access PDF via Zotero's desktop resolver (requires --via connector)")

	return cmd
}
