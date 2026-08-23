// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"zotio/internal/mutation"
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
	// Publisher backs the `book` arm of the citation-core check, whose
	// predicate requires a publisher that no other CrossRef field carries.
	Publisher string `json:"publisher"`
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
		Use:   "doi <doi>",
		Short: "Import an item from DOI metadata (CrossRef, then DataCite)",
		Long: `Import an item from DOI metadata.

The DOI's registration agency is resolved automatically. CrossRef is asked
first; if it reports no such record, DataCite is asked. That covers registrants
CrossRef does not hold, including arXiv's own 10.48550/arXiv.* preprint DOIs,
Zenodo, Dryad, and figshare. A CrossRef outage is not treated as "no record",
so a transient failure is reported rather than being retried at a registry that
does not own the DOI.

The item previews by default and is created only under --yes; --dry-run always
wins over --yes.`,
		Annotations: map[string]string{"zotio:method": "POST", "zotio:path": "/items"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			// The DOI import plan is known before fetching metadata: one create,
			// plus the resolver attachment when requested. In apply mode, reject
			// an over-cap plan before constructing clients or making network calls.
			if resolveMutationMode(flags).Apply {
				if gateFailure := mutation.CheckGates(mutationOptions(flags), doiImportPreflightOps(args[0], flagFetchPDF)); gateFailure != nil {
					return fmt.Errorf("%s", gateFailure.Message)
				}
			}

			item, err := fetchDOIItem(cmd, flags.timeout, args[0])
			if err != nil {
				return err
			}
			addImportCollection(item, flagCollection)

			var res itemCreateResult
			ops := []mutation.Op{{
				ID:   "import.doi",
				Key:  args[0],
				Kind: "item_create",
				Changes: []mutation.Change{
					{Field: "doi", Add: args[0]},
					{Field: "item", Add: item},
				},
				Apply: func() (string, any, error) {
					var (
						via string
						err error
					)
					if flagFetchPDF {
						via, err = flags.resolveCreateVia(cmd.Context(), cmd.Flags().Changed("collection"))
						if err != nil {
							return "failed", nil, err
						}
						if via != "connector" {
							return "failed", nil, preconditionErr(fmt.Errorf("--fetch-pdf requires the desktop connector; use --via connector"))
						}
					}
					c, err := flags.newClient()
					if err != nil {
						return "failed", nil, err
					}
					if flagFetchPDF {
						res, err = routeCreateItemVia(cmd.Context(), flags, via, c, item, itemCreateSourceURI(item), cmd.Flags().Changed("collection"))
					} else {
						res, err = routeCreateItem(cmd.Context(), flags, c, item, itemCreateSourceURI(item), cmd.Flags().Changed("collection"))
					}
					if err != nil {
						return "failed", nil, err
					}
					return "applied", map[string]any{"via": res.Via}, nil
				},
			}}
			if flagFetchPDF {
				ops = append(ops, mutation.Op{
					ID:   "import.doi:resolver-pdf",
					Key:  args[0],
					Kind: "attachment_create",
					Changes: []mutation.Change{{
						Field: "attachment",
						Add: map[string]any{
							"source":    "resolver",
							"condition": "when an open-access PDF resolver is available",
						},
					}},
					Apply: func() (string, any, error) {
						attachResolverPDF(cmd.Context(), flags, &res)
						detail := map[string]any{
							"status": res.OAPDFStatus,
							"title":  res.OAPDFTitle,
							"error":  res.OAPDFError,
						}
						if res.OAPDFStatus == "attached" {
							return "applied", detail, nil
						}
						return "no_op", detail, nil
					},
				})
			}
			env, runErr := runMutation(cmd.Context(), flags, "import.doi", ops)
			if renderErr := renderMutation(cmd, flags, env, nil); renderErr != nil {
				return renderErr
			}
			if runErr == nil && res.Via == "connector" {
				refreshItemsFromLocalAPI(cmd.Context(), flags)
			}
			return runErr
		},
	}
	cmd.Flags().StringVar(&flagCollection, "collection", "", "Collection key to add the item to")
	cmd.Flags().BoolVar(&flagFetchPDF, "fetch-pdf", false, "Attach an open-access PDF via Zotero's desktop resolver (requires --via connector)")

	return cmd
}

// doiImportPreflightOps describes the countable writes before CrossRef metadata
// is available. The fully detailed operations are still built and run later.
func doiImportPreflightOps(doi string, fetchPDF bool) []mutation.Op {
	ops := []mutation.Op{{
		ID:      "import.doi",
		Key:     doi,
		Kind:    "item_create",
		Changes: []mutation.Change{{Field: "doi"}},
	}}
	if fetchPDF {
		ops = append(ops, mutation.Op{
			ID:      "import.doi:resolver-pdf",
			Key:     doi,
			Kind:    "attachment_create",
			Changes: []mutation.Change{{Field: "attachment"}},
		})
	}
	return ops
}

// fetchDOIItem resolves a DOI to a Zotero item without assuming which registry
// owns it. See import_datacite.go for why this is a fallback and not a prefix
// table.
func fetchDOIItem(cmd *cobra.Command, timeout time.Duration, doi string) (map[string]any, error) {
	return fetchDOIItemWithCache(cmd.Context(), &http.Client{Timeout: timeout}, doi, nil)
}

// fetchDOIItemWithCache tries CrossRef, then DataCite. Only a "no such record"
// answer from CrossRef triggers the fallback: a timeout, a transport failure or
// a 5xx means CrossRef may well own the DOI and is simply unavailable, so
// retrying the same question at a registry that definitely does not own it
// would trade a truthful transient error for a misleading permanent one.
//
// When both miss, the error names both attempts. The single-registry message
// ("fetching CrossRef metadata: HTTP 404") is what sent a downstream consumer
// hunting for a malformed DOI when the DOI was fine and the registry was wrong.
func fetchDOIItemWithCache(ctx context.Context, httpClient *http.Client, doi string, providerCache *providerJSONCache) (map[string]any, error) {
	work, crossRefErr := fetchCrossRefWork(ctx, httpClient, doi, providerCache)
	if crossRefErr == nil {
		return crossRefItemFromWork(work, doi), nil
	}
	if !isRegistryRecordAbsent(crossRefErr) {
		return nil, crossRefErr
	}

	attrs, dataCiteErr := fetchDataCiteAttributes(ctx, httpClient, doi, providerCache)
	if dataCiteErr == nil {
		return dataCiteItemFromAttributes(attrs, doi), nil
	}
	// crossRefErr already self-identifies ("fetching CrossRef metadata: ..."),
	// so naming CrossRef again here would stutter.
	return nil, fmt.Errorf("resolving DOI metadata: %w; DataCite: %w", crossRefErr, dataCiteErr)
}

// isRegistryRecordAbsent reports whether an error means "this registry has no
// such DOI" rather than "this registry could not answer". getCappedProviderJSON
// renders a non-2xx as "HTTP <code>: <body>", so the status is matched on that
// rendering; 404 and 410 are the two ways a DOI registry says it has no record.
func isRegistryRecordAbsent(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "HTTP 404") || strings.Contains(msg, "HTTP 410")
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
	if containerTitle := firstCrossRefString(work.ContainerTitle, ""); containerTitle != "" {
		setCrossRefContainerTitle(item, containerTitle)
	}
	// Include the abstract (CrossRef returns JATS XML).
	if abstract := stripJATS(work.Abstract); abstract != "" {
		item["abstractNote"] = abstract
	}
	return item
}

// setCrossRefContainerTitle places CrossRef's container title in the field
// supported by the resolved Zotero item type. A CrossRef container is not
// necessarily a thesis university, so thesis items deliberately omit it.
// Types without a matching container field retain it in Extra rather than
// making the import invalid.
func setCrossRefContainerTitle(item map[string]any, containerTitle string) {
	itemType, _ := item["itemType"].(string)
	switch itemType {
	case "journalArticle", "magazineArticle", "newspaperArticle":
		item["publicationTitle"] = containerTitle
	case "conferencePaper":
		item["proceedingsTitle"] = containerTitle
	case "bookSection":
		item["bookTitle"] = containerTitle
	case "thesis":
		// CrossRef's container title does not establish a university.
	default:
		item["extra"] = "Container: " + containerTitle
	}
}

func crossRefItemType(crossRefType string) string {
	switch crossRefType {
	case "journal-article":
		return "journalArticle"
	case "proceedings-article":
		return "conferencePaper"
	case "book-chapter":
		return "bookSection"
	case "dissertation":
		return "thesis"
	case "report":
		return "report"
	case "posted-content":
		return "preprint"
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
