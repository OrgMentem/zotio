// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"zotio/internal/mutation"
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
		Args:        cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateImportDestination(flagCollection); err != nil {
				return err
			}
			if reason := importURLInvalidReason(args[0]); reason != "" {
				ops := []mutation.Op{{
					ID:            "import.url",
					Key:           args[0],
					Kind:          "item_create",
					Changes:       []mutation.Change{{Field: "source", Add: args[0]}},
					InvalidReason: reason,
				}}
				env, runErr := runMutation(cmd.Context(), flags, "import.url", ops)
				return renderMutationResult(cmd, flags, env, runErr, nil)
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
	cmd.Flags().StringVar(&flagCollection, "to", "", "Destination collection key to add the item to")
	cmd.Flags().BoolVar(&flagFetchPDF, "fetch-pdf", false, "Attach an open-access PDF via Zotero's desktop resolver (requires --via connector)")

	return cmd
}

// importURLInvalidReason returns why raw cannot be imported as a URL, or ""
// when it is an absolute http(s) URL with a host.
func importURLInvalidReason(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Sprintf("invalid URL %q: %v", raw, err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return fmt.Sprintf("invalid URL %q: expected an absolute http(s) URL such as https://example.org/page", raw)
	}
	return ""
}
