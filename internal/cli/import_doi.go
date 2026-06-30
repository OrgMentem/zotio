// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH: Add hand-written DOI import workflow missing from the generated CLI.

package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/spf13/cobra"
)

type crossRefWorkResponse struct {
	Message crossRefWork `json:"message"`
}

type crossRefWork struct {
	Title          []string         `json:"title"`
	Author         []crossRefAuthor `json:"author"`
	Published      crossRefDate     `json:"published"`
	DOI            string           `json:"DOI"`
	Type           string           `json:"type"`
	ContainerTitle []string         `json:"container-title"`
	// PATCH(glean dk33): CrossRef abstract (JATS XML) for enrichment.
	Abstract string `json:"abstract"`
}

type crossRefAuthor struct {
	Family string `json:"family"`
	Given  string `json:"given"`
}

type crossRefDate struct {
	DateParts [][]int `json:"date-parts"`
}

func newImportDoiCmd(flags *rootFlags) *cobra.Command {
	var flagCollection string
	var flagFetchPDF bool

	cmd := &cobra.Command{
		Use:         "doi <doi>",
		Short:       "Import an item from CrossRef DOI metadata",
		Annotations: map[string]string{"pp:method": "POST", "pp:path": "/items"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}

			item, err := fetchCrossRefItem(cmd, flags.timeout, args[0])
			if err != nil {
				return err
			}
			addImportCollection(item, flagCollection)

			if flags.dryRun {
				return printImportDryRun(cmd, item, "CrossRef (DOI "+args[0]+")", flags)
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

func fetchCrossRefItem(cmd *cobra.Command, timeout time.Duration, doi string) (map[string]any, error) {
	if doi == "" {
		return nil, fmt.Errorf("DOI is required")
	}

	// PATCH(glean 37jv): use the overridable CrossRef base (testable seam).
	req, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, enrichCrossRefBase+"/works/"+url.PathEscape(doi), nil)
	if err != nil {
		return nil, fmt.Errorf("creating CrossRef request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "zotero-pp-cli/1.0.0")

	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching CrossRef metadata: %w", err)
	}
	defer resp.Body.Close()

	// PATCH(glean zotero-pp-cli-43513a119010f6e1): cap CrossRef responses on
	// this ad-hoc import path instead of reading an unbounded body into memory.
	body, err := readCappedExternalBody(resp.Body, 1<<20)
	if err != nil {
		return nil, fmt.Errorf("reading CrossRef response: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("DOI not found: %s", doi)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("CrossRef request failed: HTTP %d", resp.StatusCode)
	}

	var decoded crossRefWorkResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("parsing CrossRef response: %w", err)
	}
	return crossRefItemFromWork(decoded.Message, doi), nil
}

func crossRefItemFromWork(work crossRefWork, fallbackDOI string) map[string]any {
	item := map[string]any{
		"itemType": crossRefItemType(work.Type),
		"title":    firstCrossRefString(work.Title, fallbackDOI),
	}
	if creators := crossRefCreators(work.Author); len(creators) > 0 {
		item["creators"] = creators
	}
	if year := crossRefYear(work.Published); year != "" {
		item["date"] = year
	}
	if work.DOI != "" {
		item["DOI"] = work.DOI
	} else {
		item["DOI"] = fallbackDOI
	}
	if publicationTitle := firstCrossRefString(work.ContainerTitle, ""); publicationTitle != "" {
		item["publicationTitle"] = publicationTitle
	}
	// PATCH(glean 37jv): include the abstract (CrossRef returns JATS XML).
	if abstract := stripJATS(work.Abstract); abstract != "" {
		item["abstractNote"] = abstract
	}
	return item
}

func crossRefItemType(crossRefType string) string {
	switch crossRefType {
	case "journal-article":
		return "journalArticle"
	case "book":
		return "book"
	default:
		return "document"
	}
}

func crossRefCreators(authors []crossRefAuthor) []map[string]any {
	creators := make([]map[string]any, 0, len(authors))
	for _, author := range authors {
		creator := map[string]any{"creatorType": "author"}
		if author.Given != "" {
			creator["firstName"] = author.Given
		}
		if author.Family != "" {
			creator["lastName"] = author.Family
		}
		if len(creator) > 1 {
			creators = append(creators, creator)
		}
	}
	return creators
}

func crossRefYear(published crossRefDate) string {
	if len(published.DateParts) == 0 || len(published.DateParts[0]) == 0 {
		return ""
	}
	return fmt.Sprintf("%d", published.DateParts[0][0])
}

func firstCrossRefString(values []string, fallback string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return fallback
}
