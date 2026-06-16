// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH: Add hand-written URL import workflow missing from the generated CLI.

package cli

import (
	"net/http"

	"github.com/spf13/cobra"
)

func newImportUrlCmd(flags *rootFlags) *cobra.Command {
	var flagCollection string

	cmd := &cobra.Command{
		Use:   "url <url>",
		Short: "Import a URL as a metadata-enriched item",
		Long: `Import a URL, resolving real metadata where possible.

If the URL embeds a DOI, full metadata is fetched from CrossRef. Otherwise the
page's embedded metadata (citation_*, Open Graph, Dublin Core meta tags) is
mapped into a typed item with title, creators, abstract, and publication venue.
A bare webpage item is used only when no metadata is available.

Use --dry-run to preview the proposed item without writing it.`,
		Annotations: map[string]string{"pp:method": "POST", "pp:path": "/items"},
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
			data, _, err := c.Post("/items", []map[string]any{item})
			if err != nil {
				return classifyAPIError(err, flags)
			}
			return printOutputWithFlags(cmd.OutOrStdout(), data, flags)
		},
	}
	cmd.Flags().StringVar(&flagCollection, "collection", "", "Collection key to add the item to")

	return cmd
}
