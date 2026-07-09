// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

// expose an overridable COCI references base URL for tests and provider swaps.
var collectionGapsOpenCitationsBase = "https://opencitations.net/index/coci/api/v1"

type collectionGapItem struct {
	Key   string
	Title string
	DOI   string
}

type collectionGapRow struct {
	Rank   int    `json:"rank"`
	Count  int    `json:"count"`
	DOI    string `json:"doi"`
	Title  string `json:"title,omitempty"`
	Action string `json:"action"`
}

type collectionGapsSummary struct {
	ItemsScanned     int `json:"items_scanned"`
	ReferencesSeen   int `json:"references_seen"`
	UniqueCitedDOIs  int `json:"unique_cited_dois"`
	AlreadyInLibrary int `json:"already_in_library"`
	Gaps             int `json:"gaps"`
}

type collectionGapsReport struct {
	CollectionKey string                `json:"collection_key"`
	Summary       collectionGapsSummary `json:"summary"`
	Rows          []collectionGapRow    `json:"rows"`
}

func newCollectionsGapsCmd(flags *rootFlags) *cobra.Command {
	var flagLimit int
	var flagTop int

	cmd := &cobra.Command{
		Use:         "gaps <collectionKey>",
		Short:       "Find highly cited DOI references missing from a collection's library",
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			if flagLimit < 0 {
				return usageErr(fmt.Errorf("--limit must be >= 0"))
			}
			if flagTop < 1 {
				return usageErr(fmt.Errorf("--top must be >= 1"))
			}

			rawDB, err := openStoreForRead(cmd.Context(), "zotio")
			if err != nil {
				return fmt.Errorf("opening database: %w", err)
			}
			if rawDB == nil {
				return preconditionErr(fmt.Errorf("run 'zotio sync' first to enable collection gap analysis"))
			}
			defer rawDB.Close()

			report, err := buildCollectionGapsReportWithCache(cmd.Context(), localQueryStore{rawDB}, &http.Client{Timeout: enrichTimeout(flags.timeout)}, newProviderJSONCache(flags.noCache), args[0], flagLimit, flagTop)
			if err != nil {
				return err
			}
			if flags.asJSON {
				return printCommandJSON(cmd.OutOrStdout(), report, flags)
			}
			return printCollectionGapsReport(cmd, report)
		},
	}
	cmd.Flags().IntVar(&flagLimit, "limit", 50, "Maximum DOI-bearing collection items to scan (0 = all)")
	cmd.Flags().IntVar(&flagTop, "top", 20, "Number of citation gaps to show")
	return cmd
}

// aggregate references, exclude whole-library DOI holdings, and rank missing cited DOIs.
func buildCollectionGapsReport(ctx context.Context, db localQueryStore, httpClient *http.Client, collectionKey string, limit int, top int) (collectionGapsReport, error) {
	return buildCollectionGapsReportWithCache(ctx, db, httpClient, nil, collectionKey, limit, top)
}

// aggregate references, exclude whole-library DOI holdings, and rank missing cited DOIs.
func buildCollectionGapsReportWithCache(ctx context.Context, db localQueryStore, httpClient *http.Client, providerCache *providerJSONCache, collectionKey string, limit int, top int) (collectionGapsReport, error) {
	items, err := queryCollectionGapItems(db, collectionKey, limit)
	if err != nil {
		return collectionGapsReport{}, fmt.Errorf("querying collection DOI items: %w", err)
	}
	libraryDOIs, err := queryLibraryDOISet(db)
	if err != nil {
		return collectionGapsReport{}, fmt.Errorf("querying library DOIs: %w", err)
	}

	sources := make([]referenceSourceItem, 0, len(items))
	for _, item := range items {
		sources = append(sources, referenceSourceItem(item))
	}
	agg, err := buildReferenceAggregate(ctx, httpClient, sources, referenceFetchOptions{Cache: providerCache})
	if err != nil {
		return collectionGapsReport{}, err
	}

	alreadyInLibrary := 0
	candidates := make([]collectionGapRow, 0, len(agg.Candidates))
	for _, candidate := range agg.Candidates {
		if libraryDOIs[candidate.DOI] {
			alreadyInLibrary++
			continue
		}
		candidates = append(candidates, collectionGapRow{Count: candidate.Count, DOI: candidate.DOI, Title: candidate.Title, Action: "zotio import doi " + candidate.DOI})
	}
	if top > len(candidates) {
		top = len(candidates)
	}
	rows := candidates[:top]
	for i := range rows {
		rows[i].Rank = i + 1
		if rows[i].Title == "" {
			rows[i].Title = fetchGapTitleFromCrossRef(ctx, httpClient, rows[i].DOI, providerCache)
		}
	}

	return collectionGapsReport{
		CollectionKey: collectionKey,
		Summary: collectionGapsSummary{
			ItemsScanned:     len(items),
			ReferencesSeen:   agg.ReferencesSeen,
			UniqueCitedDOIs:  agg.UniqueCitedDOIs,
			AlreadyInLibrary: alreadyInLibrary,
			Gaps:             len(candidates),
		},
		Rows: rows,
	}, nil
}

func queryCollectionGapItems(db localQueryStore, collectionKey string, limit int) ([]collectionGapItem, error) {
	query := `
SELECT
	id AS key,
	json_extract(data, '$.data.title') AS title,
	json_extract(data, '$.data.DOI') AS doi
FROM resources
WHERE resource_type = 'items'
	AND COALESCE(parent_key, '') = ''
	AND item_type NOT IN ('attachment', 'note', 'annotation')
	AND NULLIF(TRIM(COALESCE(json_extract(data, '$.data.DOI'), '')), '') IS NOT NULL
	AND EXISTS (SELECT 1 FROM json_each(json_extract(data, '$.data.collections')) c WHERE c.value = ?)
ORDER BY json_extract(data, '$.data.dateModified') DESC, id ASC`
	args := []any{collectionKey}
	if limit > 0 {
		query += `
LIMIT ?`
		args = append(args, limit)
	}
	rows, err := db.QueryRaw(query, args...)
	if err != nil {
		return nil, err
	}
	items := make([]collectionGapItem, 0, len(rows))
	for _, row := range rows {
		doi := normalizedGapDOI(sqlStringValue(row["doi"]))
		if doi == "" {
			continue
		}
		items = append(items, collectionGapItem{Key: sqlStringValue(row["key"]), Title: sqlStringValue(row["title"]), DOI: doi})
	}
	return items, nil
}

func queryLibraryDOISet(db localQueryStore) (map[string]bool, error) {
	rows, err := db.QueryRaw(`
SELECT json_extract(data, '$.data.DOI') AS doi
FROM resources
WHERE resource_type = 'items'
	AND NULLIF(TRIM(COALESCE(json_extract(data, '$.data.DOI'), '')), '') IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(rows))
	for _, row := range rows {
		if doi := normalizedGapDOI(sqlStringValue(row["doi"])); doi != "" {
			out[doi] = true
		}
	}
	return out, nil
}

// prefer OpenCitations COCI references and fall back to Semantic Scholar when COCI is empty or unavailable.
func fetchOutgoingReferenceDOIs(ctx context.Context, httpClient *http.Client, doi string) ([]string, map[string]string, error) {
	refs, err := fetchOutgoingReferences(ctx, httpClient, doi, referenceFetchOptions{})
	if err != nil {
		return nil, nil, err
	}
	return refs.DOIs, refs.Titles, nil
}

type cociReference struct {
	Cited string `json:"cited"`
}

func fetchCOCIReferenceDOIs(ctx context.Context, httpClient *http.Client, doi string, providerCache *providerJSONCache) ([]string, error) {
	var refs []cociReference
	if err := getCappedProviderJSON(ctx, httpClient, providerCOCI, collectionGapsOpenCitationsBase+"/references/"+url.PathEscape(doi), providerCache, &refs); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		if ref.Cited != "" {
			out = append(out, ref.Cited)
		}
	}
	return out, nil
}

type semanticScholarReferencesResponse struct {
	Data []semanticScholarReference `json:"data"`
	Next int                        `json:"next,omitempty"`
}

type semanticScholarReference struct {
	CitedPaper semanticScholarReferencePaper `json:"citedPaper"`
}

type semanticScholarReferencePaper struct {
	ExternalIDs map[string]string `json:"externalIds"`
	Title       string            `json:"title"`
}

func fetchSemanticScholarReferenceDOIs(ctx context.Context, httpClient *http.Client, doi string, providerCache *providerJSONCache) ([]string, map[string]string, bool, error) {
	u := enrichSemanticScholarBase + "/paper/DOI:" + url.PathEscape(doi) + "/references?" + url.Values{
		"fields": {"externalIds,title"},
		"limit":  {"1000"},
	}.Encode()
	var resp semanticScholarReferencesResponse
	if err := getCappedProviderJSON(ctx, httpClient, providerSemanticScholar, u, providerCache, &resp); err != nil {
		return nil, nil, false, err
	}
	refs := make([]string, 0, len(resp.Data))
	titles := map[string]string{}
	for _, ref := range resp.Data {
		doi := normalizedGapDOI(ref.CitedPaper.ExternalIDs["DOI"])
		if doi == "" {
			continue
		}
		refs = append(refs, doi)
		if title := strings.TrimSpace(ref.CitedPaper.Title); title != "" {
			titles[doi] = title
		}
	}
	return refs, titles, resp.Next != 0 || len(resp.Data) >= 1000, nil
}

func fetchGapTitleFromCrossRef(ctx context.Context, httpClient *http.Client, doi string, providerCache *providerJSONCache) string {
	var resp crossRefWorkResponse
	if err := getCappedProviderJSON(ctx, httpClient, providerCrossRef, enrichCrossRefBase+"/works/"+url.PathEscape(doi), providerCache, &resp); err != nil {
		return ""
	}
	if len(resp.Message.Title) == 0 {
		return ""
	}
	return strings.TrimSpace(resp.Message.Title[0])
}

func printCollectionGapsReport(cmd *cobra.Command, report collectionGapsReport) error {
	tw := newTabWriter(cmd.OutOrStdout())
	fmt.Fprintln(tw, "RANK\tCOUNT\tDOI\tTITLE\tACTION")
	for _, row := range report.Rows {
		fmt.Fprintf(tw, "%d\t%d\t%s\t%s\t%s\n", row.Rank, row.Count, row.DOI, row.Title, row.Action)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "\nSummary: items scanned=%d; references seen=%d; unique cited DOIs=%d; already in library=%d; gaps=%d\n",
		report.Summary.ItemsScanned,
		report.Summary.ReferencesSeen,
		report.Summary.UniqueCitedDOIs,
		report.Summary.AlreadyInLibrary,
		report.Summary.Gaps,
	)
	return nil
}

func normalizedGapDOI(value string) string {
	return strings.ToLower(normalizeDOI(value))
}
