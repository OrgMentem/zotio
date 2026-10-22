// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// Values Zotero documents for the items read endpoints.
var (
	itemsSortValues      = []string{"dateAdded", "dateModified", "title", "creator", "itemType", "date", "publisher", "publicationTitle", "journalAbbreviation", "language", "accessDate", "libraryCatalog", "callNumber", "rights", "addedBy", "numItems"}
	itemsDirectionValues = []string{"asc", "desc"}
	itemsFormatValues    = []string{"json", "atom", "bib", "bibtex", "biblatex", "bookmarks", "coins", "csljson", "csv", "mods", "refer", "rdf_bibliontology", "rdf_dc", "rdf_zotero", "ris", "tei", "wikipedia", "keys", "versions"}
)

// validateEnumFlag rejects a non-empty value outside allowed with a usage error.
func validateEnumFlag(flag, value string, allowed []string) error {
	if value == "" {
		return nil
	}
	for _, a := range allowed {
		if a == value {
			return nil
		}
	}
	return usageErr(fmt.Errorf("invalid %s %q: must be one of %s", flag, value, strings.Join(allowed, ", ")))
}

func newItemsListCmd(flags *rootFlags) *cobra.Command {
	var flagLimit int
	var flagStart int
	var flagSort string
	var flagDirection string
	var flagItemType string
	var flagQ string
	var flagTag string
	var flagSinceVersion int
	var flagFormat string

	cmd := &cobra.Command{
		Use:         "list",
		Short:       "List all items in the library",
		Example:     "  zotio items list --limit 20 --item-type journalArticle\n  zotio items list --query \"deep learning\" --sort dateModified --direction desc",
		Annotations: map[string]string{"zotio:endpoint": "items.list", "zotio:method": "GET", "zotio:path": "/items", "mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateLimitFlag("--limit", flagLimit); err != nil {
				return err
			}
			if err := validateEnumFlag("--sort", flagSort, itemsSortValues); err != nil {
				return err
			}
			if err := validateEnumFlag("--direction", flagDirection, itemsDirectionValues); err != nil {
				return err
			}
			if err := validateEnumFlag("--format", flagFormat, itemsFormatValues); err != nil {
				return err
			}
			c, err := flags.newClient()
			if err != nil {
				return err
			}

			path := "/items"
			params := map[string]string{}
			if flagLimit != 0 {
				params["limit"] = fmt.Sprintf("%v", flagLimit)
			}
			if flagStart != 0 {
				params["start"] = fmt.Sprintf("%v", flagStart)
			}
			if flagSort != "" {
				params["sort"] = fmt.Sprintf("%v", flagSort)
			}
			if flagDirection != "" {
				params["direction"] = fmt.Sprintf("%v", flagDirection)
			}
			if flagItemType != "" {
				params["itemType"] = fmt.Sprintf("%v", flagItemType)
			}
			if flagQ != "" {
				params["q"] = fmt.Sprintf("%v", flagQ)
			}
			if flagTag != "" {
				params["tag"] = fmt.Sprintf("%v", flagTag)
			}
			if flagSinceVersion != 0 {
				params["since"] = fmt.Sprintf("%v", flagSinceVersion)
			}
			if flagFormat != "" {
				params["format"] = fmt.Sprintf("%v", flagFormat)
			}
			data, prov, err := resolveRead(cmd.Context(), c, flags, "items", false, path, params, nil)
			if err != nil {
				return classifyAPIError(err, flags)
			}
			// Honor --limit when the API accepts but ignores ?limit=N.
			data = truncateJSONArray(data, flagLimit)
			// Print provenance to stderr for human-facing output
			printProvenance(cmd, countResultItems(data), prov)
			// For JSON output, wrap with provenance envelope before passing through flags.
			// --select wins over --compact when both are set; --compact only runs when
			// no explicit fields were requested.
			if wantsJSONEnvelope(cmd.OutOrStdout(), flags) {
				if flags.quiet {
					return nil
				}
				filtered, err := applyFieldFlags(data, flags)
				if err != nil {
					return err
				}
				wrapped, wrapErr := wrapWithProvenance(filtered, prov)
				if wrapErr != nil {
					return wrapErr
				}
				return printOutput(cmd.OutOrStdout(), wrapped, true)
			}
			// For all other output modes (table, csv, plain, quiet), use the standard pipeline
			if wantsHumanTable(cmd.OutOrStdout(), flags) {
				var items []map[string]any
				if json.Unmarshal(data, &items) == nil && len(items) > 0 {
					if err := printAutoTable(cmd.OutOrStdout(), items); err != nil {
						return err
					}
					if len(items) >= 25 {
						fmt.Fprintf(os.Stderr, "\nShowing %d results. To narrow: add --limit, --json --select, or filter flags.\n", len(items))
					}
					return nil
				}
			}
			return printOutputWithFlags(cmd.OutOrStdout(), data, flags)
		},
	}
	cmd.Flags().IntVar(&flagLimit, "limit", 0, "Maximum number of items to return (0 = no limit)")
	cmd.Flags().IntVar(&flagStart, "start", 0, "Pagination offset (zero-based)")
	cmd.Flags().StringVar(&flagSort, "sort", "", "Sort field (dateAdded, dateModified, title, creator, type, date, publisher, publicationTitle, journalAbbreviation,...")
	cmd.Flags().StringVar(&flagDirection, "direction", "", "Sort direction (asc or desc)")
	cmd.Flags().StringVar(&flagItemType, "item-type", "", "Filter by item type, for example journalArticle or book (not validated client-side; see 'zotio schema item-types')")
	cmd.Flags().StringVar(&flagQ, "query", "", "Quick search across title, creator, and year; quote multi-word queries")
	cmd.Flags().StringVar(&flagTag, "tag", "", "Filter by tag name")
	cmd.Flags().IntVar(&flagSinceVersion, "since-version", 0, "Return only items modified after this library version number")
	cmd.Flags().StringVar(&flagFormat, "format", "", "Response format (json, atom, bib, bibtex, bookmarks, coins, csljson, mods, refer, rdf_bibliontology, rdf_dc,...")

	return cmd
}
