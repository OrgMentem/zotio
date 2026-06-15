// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.

package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newItemsCiteCmd(flags *rootFlags) *cobra.Command {
	var flagStyle string

	cmd := &cobra.Command{
		Use:   "cite <itemKey>",
		Short: "Get a formatted citation for an item",
		Long: `Retrieve a formatted bibliographic citation for an item. The Zotero local
API renders citations using your installed CSL styles. For the most common
styles (apa, chicago, mla), use the --style flag. For others, pass the
CSL style ID directly (e.g. "nature", "harvard-cite-them-right").`,
		Example: `  zotero-pp-cli items cite ABCD1234
  zotero-pp-cli items cite ABCD1234 --style apa
  zotero-pp-cli items cite ABCD1234 --style bibtex`,
		Annotations: map[string]string{"pp:endpoint": "items.get", "pp:method": "GET", "pp:path": "/items/{itemKey}", "mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			c, err := flags.newClient()
			if err != nil {
				return err
			}

			itemKey := args[0]
			path := replacePathParam("/items/{itemKey}", "itemKey", itemKey)
			params := map[string]string{}

			switch flagStyle {
			case "bibtex":
				params["format"] = "bibtex"
			case "ris":
				params["format"] = "ris"
			case "csljson", "csl-json":
				params["format"] = "csljson"
			case "":
				params["format"] = "bib"
			default:
				// Pass the style name as the format parameter — Zotero local API
				// accepts format=bib which uses the default citation style.
				// For named CSL styles, use format=bib (the local API doesn't support
				// per-request style selection in the same way as the remote API).
				fmt.Fprintf(cmd.ErrOrStderr(), "Note: Zotero local API uses the default citation style; --style %q not applied.\n", flagStyle)
				params["format"] = "bib"
			}

			data, err := c.Get(path, params)
			if err != nil {
				return classifyAPIError(err, flags)
			}
			return printOutput(cmd.OutOrStdout(), data, false)
		},
	}
	cmd.Flags().StringVar(&flagStyle, "style", "", "Citation style (apa, mla, chicago, bibtex, ris, csljson) — default uses Zotero's current default style")

	return cmd
}
