// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
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
	Title          []string            `json:"title"`
	Author         []crossRefAuthor    `json:"author"`
	Published      crossRefDate        `json:"published"`
	DOI            string              `json:"DOI"`
	Type           string              `json:"type"`
	ContainerTitle []string            `json:"container-title"`
	Reference      []crossRefReference `json:"reference"`
	// CrossRef abstract (JATS XML) for enrichment.
	Abstract string `json:"abstract"`
}

type crossRefReference struct {
	DOI string `json:"DOI"`
}

type crossRefAuthor struct {
	Family string `json:"family"`
	Given  string `json:"given"`
	ORCID  string `json:"ORCID"`
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
		Annotations: map[string]string{"zotio:method": "POST", "zotio:path": "/items"},
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
			// Route item creates through the desktop connector when available.
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
	return fetchCrossRefItemWithCache(cmd.Context(), &http.Client{Timeout: timeout}, doi, nil)
}

func fetchCrossRefItemWithCache(ctx context.Context, httpClient *http.Client, doi string, providerCache *providerJSONCache) (map[string]any, error) {
	work, err := fetchCrossRefWork(ctx, httpClient, doi, providerCache)
	if err != nil {
		return nil, err
	}
	return crossRefItemFromWork(work, doi), nil
}

func fetchCrossRefWork(ctx context.Context, httpClient *http.Client, doi string, providerCache *providerJSONCache) (crossRefWork, error) {
	if doi == "" {
		return crossRefWork{}, fmt.Errorf("DOI is required")
	}
	var decoded crossRefWorkResponse
	rawURL := enrichCrossRefBase + "/works/" + url.PathEscape(doi)
	if err := getCappedProviderJSON(ctx, httpClient, providerCrossRef, rawURL, providerCache, &decoded); err != nil {
		return crossRefWork{}, fmt.Errorf("fetching CrossRef metadata: %w", err)
	}
	return decoded.Message, nil
}

func fetchCrossRefReferenceDOIs(ctx context.Context, httpClient *http.Client, doi string, providerCache *providerJSONCache) ([]string, error) {
	work, err := fetchCrossRefWork(ctx, httpClient, doi, providerCache)
	if err != nil {
		return nil, err
	}
	refs := make([]string, 0, len(work.Reference))
	for _, ref := range work.Reference {
		if ref.DOI != "" {
			refs = append(refs, ref.DOI)
		}
	}
	return refs, nil
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
	// Include the abstract (CrossRef returns JATS XML).
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
