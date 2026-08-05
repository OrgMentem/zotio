// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
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

The item previews by default and is created only under --yes; --dry-run always
wins over --yes.`,
		Annotations: map[string]string{"zotio:method": "POST", "zotio:path": "/items"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}

			httpClient := &http.Client{Timeout: enrichTimeout(flags.timeout)}
			item, source := buildImportItemFromURL(cmd.Context(), httpClient, args[0])
			addImportCollection(item, flagCollection)

			return runSingleItemCreate(cmd, flags, singleItemCreate{
				operation: "import.url",
				key:       args[0],
				source:    source,
				item:      item,
				fetchPDF:  flagFetchPDF,
			})
		},
	}
	cmd.Flags().StringVar(&flagCollection, "collection", "", "Collection key to add the item to")
	cmd.Flags().BoolVar(&flagFetchPDF, "fetch-pdf", false, "Attach an open-access PDF via Zotero's desktop resolver (requires --via connector)")

	return cmd
}
