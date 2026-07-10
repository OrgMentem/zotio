// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	importDiscoverDirectionBackward = "backward"
	importDiscoverDirectionForward  = "forward"
	importDiscoverDirectionBoth     = "both"
)

type importDiscoverSummary struct {
	ItemsScanned            int                      `json:"items_scanned"`
	ReferencesSeen          int                      `json:"references_seen"`
	UniqueCitedDOIs         int                      `json:"unique_cited_dois"`
	Candidates              int                      `json:"candidates"`
	Entries                 int                      `json:"entries"`
	SkippedAlreadyInLibrary int                      `json:"skipped_already_in_library"`
	SkippedTitleDuplicate   int                      `json:"skipped_title_duplicate"`
	Sources                 []referenceSourceSummary `json:"sources"`
}

type importDiscoverReport struct {
	Scope   string                `json:"scope"`
	Out     string                `json:"out"`
	Summary importDiscoverSummary `json:"summary"`
}

type referenceSourceItem struct {
	Key   string
	Title string
	DOI   string
}

type referenceSourceSummary struct {
	Key       string `json:"key"`
	DOI       string `json:"doi"`
	Direction string `json:"direction,omitempty"`
	Refs      int    `json:"refs"`
	Truncated bool   `json:"truncated"`
	Error     string `json:"error,omitempty"`
}

type referenceFetchOptions struct {
	IncludeCrossRef      bool
	CountUniquePerSource bool
	Cache                *providerJSONCache
}

type referenceFetchResult struct {
	Provider  string
	DOIs      []string
	Titles    map[string]string
	Truncated bool
}

type citedDOICandidate struct {
	DOI         string
	Count       int
	Title       string
	Providers   []string
	CitedByKeys []string
	Direction   string
}

type citedDOIAggregate struct {
	ReferencesSeen  int
	UniqueCitedDOIs int
	Candidates      []citedDOICandidate
	Sources         []referenceSourceSummary
}

func newImportDiscoverCmd(flags *rootFlags) *cobra.Command {
	var flagScope string
	var flagOut string
	var flagLimit int
	var flagMinCount int
	var flagDirection string

	cmd := &cobra.Command{
		Use:         "discover",
		Short:       "Discover missing references and write a reviewable import manifest",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(flagScope) == "" {
				return usageErr(fmt.Errorf("--scope is required"))
			}
			if strings.TrimSpace(flagOut) == "" {
				return usageErr(fmt.Errorf("--out is required"))
			}
			if flagLimit < 1 {
				return usageErr(fmt.Errorf("--limit must be >= 1"))
			}
			if flagMinCount < 1 {
				return usageErr(fmt.Errorf("--min-count must be >= 1"))
			}

			manifest, report, err := buildImportDiscoverManifestWithDirection(cmd.Context(), flags, flagScope, flagOut, flagLimit, flagMinCount, flagDirection)
			if err != nil {
				return err
			}
			f, err := os.Create(flagOut)
			if err != nil {
				return fmt.Errorf("creating manifest: %w", err)
			}
			defer f.Close()
			if err := writeImportManifest(f, manifest); err != nil {
				return fmt.Errorf("writing manifest: %w", err)
			}
			if flags.asJSON {
				return printCommandJSON(cmd.OutOrStdout(), report, flags)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Wrote %d manifest entries to %s\n", report.Summary.Entries, flagOut)
			fmt.Fprintf(cmd.OutOrStdout(), "Summary: items scanned=%d; references seen=%d; unique cited DOIs=%d; candidates=%d; already in library=%d; title duplicates=%d\n",
				report.Summary.ItemsScanned,
				report.Summary.ReferencesSeen,
				report.Summary.UniqueCitedDOIs,
				report.Summary.Candidates,
				report.Summary.SkippedAlreadyInLibrary,
				report.Summary.SkippedTitleDuplicate,
			)
			return nil
		},
	}
	cmd.Flags().StringVar(&flagScope, "scope", "", "Scope expression to discover from (required; e.g. collection:<key>, tag:<tag>, item:<key>, query:<text>, library)")
	cmd.Flags().StringVar(&flagOut, "out", "", "Path to write the reviewable import manifest")
	cmd.Flags().IntVar(&flagLimit, "limit", 25, "Maximum manifest entries to emit")
	cmd.Flags().IntVar(&flagMinCount, "min-count", 2, "Minimum number of source items citing a DOI")
	cmd.Flags().StringVar(&flagDirection, "direction", importDiscoverDirectionBackward, "Citation chase direction: backward, forward, or both")
	_ = cmd.MarkFlagRequired("scope")
	_ = cmd.MarkFlagRequired("out")
	return cmd
}

func importDiscoverDirections(direction string) ([]string, error) {
	switch strings.ToLower(strings.TrimSpace(direction)) {
	case "", importDiscoverDirectionBackward:
		return []string{importDiscoverDirectionBackward}, nil
	case importDiscoverDirectionForward:
		return []string{importDiscoverDirectionForward}, nil
	case importDiscoverDirectionBoth:
		return []string{importDiscoverDirectionBackward, importDiscoverDirectionForward}, nil
	default:
		return nil, fmt.Errorf("--direction must be backward, forward, or both")
	}
}

func importDiscoverCandidateDirection(values map[string]bool) string {
	if values[importDiscoverDirectionBackward] && values[importDiscoverDirectionForward] {
		return importDiscoverDirectionBoth
	}
	if values[importDiscoverDirectionForward] {
		return importDiscoverDirectionForward
	}
	return importDiscoverDirectionBackward
}

func buildImportDiscoverManifestWithDirection(ctx context.Context, flags *rootFlags, scopeExpr string, outPath string, limit int, minCount int, direction string) (importManifest, importDiscoverReport, error) {
	directions, err := importDiscoverDirections(direction)
	if err != nil {
		return importManifest{}, importDiscoverReport{}, usageErr(err)
	}
	spec, err := parseScopeSpec(scopeExpr)
	if err != nil {
		return importManifest{}, importDiscoverReport{}, usageErr(err)
	}
	rawDB, err := openStoreForRead(ctx, "zotio")
	if err != nil {
		return importManifest{}, importDiscoverReport{}, fmt.Errorf("opening local store: %w", err)
	}
	if rawDB == nil {
		return importManifest{}, importDiscoverReport{}, preconditionErr(fmt.Errorf("run 'zotio sync' first to enable citation discovery"))
	}
	defer rawDB.Close()

	db := localQueryStore{rawDB}
	resolved, err := resolveScope(db, spec)
	if err != nil {
		return importManifest{}, importDiscoverReport{}, fmt.Errorf("resolving scope: %w", err)
	}
	if resolved.Precondition != "" {
		return importManifest{}, importDiscoverReport{}, preconditionErr(fmt.Errorf("scope %q requires %s", resolved.Expr, resolved.Precondition))
	}

	sourceItems, err := queryImportDiscoverItems(db, resolved)
	if err != nil {
		return importManifest{}, importDiscoverReport{}, fmt.Errorf("querying scope DOI items: %w", err)
	}
	libraryDOIs, err := buildLibraryDOIIndex(rawDB)
	if err != nil {
		return importManifest{}, importDiscoverReport{}, fmt.Errorf("indexing library DOIs: %w", err)
	}
	libraryTitles, err := queryLibraryTitleSet(db)
	if err != nil {
		return importManifest{}, importDiscoverReport{}, fmt.Errorf("indexing library titles: %w", err)
	}

	httpClient := &http.Client{Timeout: enrichTimeout(flags.timeout)}
	providerCache := newProviderJSONCache(flags.noCache)
	agg, err := buildReferenceAggregateForDirections(ctx, httpClient, sourceItems, directions, referenceFetchOptions{IncludeCrossRef: true, CountUniquePerSource: true, Cache: providerCache})
	if err != nil {
		return importManifest{}, importDiscoverReport{}, err
	}

	manifest := importManifest{SchemaVersion: importManifestSchemaVersion, Entries: make([]importManifestEntry, 0, limit)}
	report := importDiscoverReport{
		Scope: resolved.Expr,
		Out:   outPath,
		Summary: importDiscoverSummary{
			ItemsScanned:    len(sourceItems),
			ReferencesSeen:  agg.ReferencesSeen,
			UniqueCitedDOIs: agg.UniqueCitedDOIs,
			Sources:         agg.Sources,
		},
	}

	for _, candidate := range agg.Candidates {
		if candidate.Count < minCount {
			continue
		}
		report.Summary.Candidates++
		if libraryDOIs.byDOI[candidate.DOI].key != "" {
			report.Summary.SkippedAlreadyInLibrary++
			continue
		}
		if len(manifest.Entries) >= limit {
			break
		}

		discovery := &importDiscovery{
			Direction:   candidate.Direction,
			Provider:    strings.Join(candidate.Providers, "+"),
			CitedByKeys: append([]string(nil), candidate.CitedByKeys...),
			Count:       candidate.Count,
		}
		entry := importManifestEntry{
			Path:           "",
			Classification: "new",
			Action:         "create",
			IdentifierType: "doi",
			Identifier:     candidate.DOI,
			Title:          candidate.Title,
			Status:         "unresolved",
			Discovery:      discovery,
		}

		item, err := fetchCrossRefItemWithCache(ctx, httpClient, candidate.DOI, providerCache)
		if err != nil {
			entry.Note = err.Error()
			manifest.Entries = append(manifest.Entries, entry)
			continue
		}
		entry.Item = item
		entry.Status = "resolved"
		if title, _ := stringValue(item["title"]); strings.TrimSpace(title) != "" {
			entry.Title = strings.TrimSpace(title)
		}
		if libraryTitles[normalizeDuplicateTitle(entry.Title)] {
			entry.Action = "skip"
			entry.Item = nil
			entry.Note = "title already exists in library"
			report.Summary.SkippedTitleDuplicate++
		}
		manifest.Entries = append(manifest.Entries, entry)
	}
	report.Summary.Entries = len(manifest.Entries)
	return manifest, report, nil
}

func queryImportDiscoverItems(db localQueryStore, scope scopeResult) ([]referenceSourceItem, error) {
	query := `
SELECT
	id AS key,
	json_extract(data, '$.data.title') AS title,
	json_extract(data, '$.data.DOI') AS doi
FROM resources
WHERE resource_type = 'items'
	AND COALESCE(parent_key, '') = ''
	AND item_type NOT IN ('attachment', 'note', 'annotation')
	AND NULLIF(TRIM(COALESCE(json_extract(data, '$.data.DOI'), '')), '') IS NOT NULL`
	args := make([]any, 0, len(scope.Keys))
	if !scope.All {
		if len(scope.Keys) == 0 {
			return []referenceSourceItem{}, nil
		}
		query += "\n\tAND id IN (" + sqlPlaceholders(len(scope.Keys)) + ")"
		for _, key := range scope.Keys {
			args = append(args, key)
		}
	}
	query += "\nORDER BY json_extract(data, '$.data.dateModified') DESC, id ASC"
	rows, err := db.QueryRaw(query, args...)
	if err != nil {
		return nil, err
	}
	items := make([]referenceSourceItem, 0, len(rows))
	for _, row := range rows {
		doi := normalizedGapDOI(sqlStringValue(row["doi"]))
		if doi == "" {
			continue
		}
		items = append(items, referenceSourceItem{Key: sqlStringValue(row["key"]), Title: sqlStringValue(row["title"]), DOI: doi})
	}
	return items, nil
}

func queryLibraryTitleSet(db localQueryStore) (map[string]bool, error) {
	rows, err := db.QueryRaw(`
SELECT json_extract(data, '$.data.title') AS title
FROM resources
WHERE resource_type = 'items'
	AND COALESCE(TRIM(json_extract(data, '$.data.title')), '') != ''
	AND COALESCE(json_extract(data, '$.data.itemType'), '') NOT IN ('attachment', 'annotation', 'note')`)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(rows))
	for _, row := range rows {
		if title := normalizeDuplicateTitle(sqlStringValue(row["title"])); title != "" {
			out[title] = true
		}
	}
	return out, nil
}

func sqlPlaceholders(n int) string {
	if n <= 0 {
		return ""
	}
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "?"
	}
	return strings.Join(parts, ",")
}

func buildReferenceAggregate(ctx context.Context, httpClient *http.Client, items []referenceSourceItem, opts referenceFetchOptions) (citedDOIAggregate, error) {
	return buildReferenceAggregateForDirections(ctx, httpClient, items, []string{importDiscoverDirectionBackward}, opts)
}

func buildReferenceAggregateForDirections(ctx context.Context, httpClient *http.Client, items []referenceSourceItem, directions []string, opts referenceFetchOptions) (citedDOIAggregate, error) {
	if len(directions) == 0 {
		directions = []string{importDiscoverDirectionBackward}
	}
	counts := map[string]int{}
	titles := map[string]string{}
	providers := map[string]map[string]bool{}
	citedBy := map[string][]string{}
	candidateDirections := map[string]map[string]bool{}
	out := citedDOIAggregate{Sources: make([]referenceSourceSummary, 0, len(items)*len(directions))}

	firstFetch := true
	for _, direction := range directions {
		for _, item := range items {
			if !firstFetch {
				if err := sleepWithContext(ctx, 200*time.Millisecond); err != nil {
					return citedDOIAggregate{}, err
				}
			}
			firstFetch = false

			var refs referenceFetchResult
			var err error
			switch direction {
			case importDiscoverDirectionBackward:
				refs, err = fetchOutgoingReferences(ctx, httpClient, item.DOI, opts)
			case importDiscoverDirectionForward:
				refs, err = fetchIncomingReferences(ctx, httpClient, item.DOI, opts)
			default:
				return citedDOIAggregate{}, usageErr(fmt.Errorf("--direction must be backward, forward, or both"))
			}
			if err != nil {
				// Degrade per source: a single oversized or failing provider
				// response must not abort the whole chase. The failure stays
				// visible in the source summary; the aggregate only fails when
				// no source could be fetched at all.
				out.Sources = append(out.Sources, referenceSourceSummary{Key: item.Key, DOI: item.DOI, Direction: sourceDirectionLabel(directions, direction), Error: err.Error()})
				continue
			}

			sourceDirection := sourceDirectionLabel(directions, direction)
			out.Sources = append(out.Sources, referenceSourceSummary{Key: item.Key, DOI: item.DOI, Direction: sourceDirection, Refs: len(refs.DOIs), Truncated: refs.Truncated})
			out.ReferencesSeen += len(refs.DOIs)
			seenForSource := map[string]bool{}
			for _, ref := range refs.DOIs {
				doi := normalizedGapDOI(ref)
				if doi == "" {
					continue
				}
				if opts.CountUniquePerSource {
					if seenForSource[doi] {
						continue
					}
					seenForSource[doi] = true
				}
				counts[doi]++
				if !containsString(citedBy[doi], item.Key) {
					citedBy[doi] = append(citedBy[doi], item.Key)
				}
				if providers[doi] == nil {
					providers[doi] = map[string]bool{}
				}
				if refs.Provider != "" {
					providers[doi][refs.Provider] = true
				}
				if candidateDirections[doi] == nil {
					candidateDirections[doi] = map[string]bool{}
				}
				candidateDirections[doi][direction] = true
				if title := strings.TrimSpace(refs.Titles[doi]); title != "" && titles[doi] == "" {
					titles[doi] = title
				}
			}
		}
	}

	candidates := make([]citedDOICandidate, 0, len(counts))
	for doi, count := range counts {
		candidates = append(candidates, citedDOICandidate{DOI: doi, Count: count, Title: titles[doi], Providers: sortedStringSet(providers[doi]), CitedByKeys: citedBy[doi], Direction: importDiscoverCandidateDirection(candidateDirections[doi])})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Count != candidates[j].Count {
			return candidates[i].Count > candidates[j].Count
		}
		return candidates[i].DOI < candidates[j].DOI
	})
	out.UniqueCitedDOIs = len(counts)
	out.Candidates = candidates
	if len(items) > 0 && len(out.Candidates) == 0 && allSourcesFailed(out.Sources) {
		return citedDOIAggregate{}, apiErr(fmt.Errorf("fetching citations failed for every source item; first error: %s", out.Sources[0].Error))
	}
	return out, nil
}

func sourceDirectionLabel(directions []string, direction string) string {
	if len(directions) > 1 || direction != importDiscoverDirectionBackward {
		return direction
	}
	return ""
}

func allSourcesFailed(sources []referenceSourceSummary) bool {
	for _, source := range sources {
		if source.Error == "" {
			return false
		}
	}
	return len(sources) > 0
}

func fetchOutgoingReferences(ctx context.Context, httpClient *http.Client, doi string, opts referenceFetchOptions) (referenceFetchResult, error) {
	refs, err := fetchCOCIReferenceDOIs(ctx, httpClient, doi, opts.Cache)
	if err == nil && len(refs) > 0 {
		return referenceFetchResult{Provider: providerCOCI, DOIs: refs, Titles: map[string]string{}}, nil
	}

	s2Refs, s2Titles, truncated, s2Err := fetchSemanticScholarReferenceDOIs(ctx, httpClient, doi, opts.Cache)
	if s2Err == nil && len(s2Refs) > 0 {
		return referenceFetchResult{Provider: providerSemanticScholar, DOIs: s2Refs, Titles: s2Titles, Truncated: truncated}, nil
	}
	if !opts.IncludeCrossRef {
		if s2Err != nil {
			return referenceFetchResult{}, s2Err
		}
		return referenceFetchResult{Provider: providerSemanticScholar, DOIs: s2Refs, Titles: s2Titles, Truncated: truncated}, nil
	}

	crRefs, crErr := fetchCrossRefReferenceDOIs(ctx, httpClient, doi, opts.Cache)
	if crErr == nil && len(crRefs) > 0 {
		return referenceFetchResult{Provider: providerCrossRef, DOIs: crRefs, Titles: map[string]string{}}, nil
	}
	if s2Err != nil {
		return referenceFetchResult{}, s2Err
	}
	if crErr != nil && err != nil {
		return referenceFetchResult{}, crErr
	}
	return referenceFetchResult{Provider: providerSemanticScholar, DOIs: s2Refs, Titles: s2Titles, Truncated: truncated}, nil
}

func fetchIncomingReferences(ctx context.Context, httpClient *http.Client, doi string, opts referenceFetchOptions) (referenceFetchResult, error) {
	refs, err := fetchCOCICitationDOIs(ctx, httpClient, doi, opts.Cache)
	if err == nil && len(refs) > 0 {
		return referenceFetchResult{Provider: providerCOCI, DOIs: refs, Titles: map[string]string{}}, nil
	}

	oaRefs, truncated, oaErr := fetchOpenAlexCitationDOIs(ctx, httpClient, doi, opts.Cache)
	if oaErr == nil {
		return referenceFetchResult{Provider: providerOpenAlex, DOIs: oaRefs, Titles: map[string]string{}, Truncated: truncated}, nil
	}
	return referenceFetchResult{}, oaErr
}

type cociCitation struct {
	Citing string `json:"citing"`
}

func fetchCOCICitationDOIs(ctx context.Context, httpClient *http.Client, doi string, providerCache *providerJSONCache) ([]string, error) {
	var refs []cociCitation
	if err := getCappedProviderJSON(ctx, httpClient, providerCOCI, collectionGapsOpenCitationsBase+"/citations/"+url.PathEscape(doi), providerCache, &refs); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		if ref.Citing != "" {
			out = append(out, ref.Citing)
		}
	}
	return out, nil
}

const (
	openAlexForwardPageSize = 200
	openAlexForwardCap      = 1000
)

type openAlexWorksPageResponse struct {
	Meta struct {
		NextCursor string `json:"next_cursor"`
	} `json:"meta"`
	Results []openAlexWork `json:"results"`
}

func fetchOpenAlexCitationDOIs(ctx context.Context, httpClient *http.Client, doi string, providerCache *providerJSONCache) ([]string, bool, error) {
	workID, err := fetchOpenAlexWorkIDForDOI(ctx, httpClient, doi, providerCache)
	if err != nil {
		return nil, false, err
	}
	if workID == "" {
		return nil, false, nil
	}
	return fetchOpenAlexCitingWorkDOIs(ctx, httpClient, workID, providerCache)
}

func fetchOpenAlexWorkIDForDOI(ctx context.Context, httpClient *http.Client, doi string, providerCache *providerJSONCache) (string, error) {
	var work openAlexWork
	rawURL := enrichOpenAlexBase + "/works/https://doi.org/" + url.PathEscape(normalizedGapDOI(doi))
	if err := getCappedProviderJSON(ctx, httpClient, providerOpenAlex, rawURL, providerCache, &work); err != nil {
		return "", err
	}
	return strings.TrimSpace(work.ID), nil
}

func fetchOpenAlexCitingWorkDOIs(ctx context.Context, httpClient *http.Client, workID string, providerCache *providerJSONCache) ([]string, bool, error) {
	dois := make([]string, 0, openAlexForwardPageSize)
	cursor := "*"
	worksSeen := 0
	truncated := false
	for {
		var page openAlexWorksPageResponse
		v := url.Values{
			"cursor":   {cursor},
			"filter":   {"cites:" + workID},
			"per-page": {fmt.Sprintf("%d", openAlexForwardPageSize)},
			// Trim pages to the two fields we consume; full work objects
			// (abstracts, authorships) overflow the provider response cap.
			"select": {"id,doi"},
		}
		rawURL := enrichOpenAlexBase + "/works?" + v.Encode()
		if err := getCappedProviderJSON(ctx, httpClient, providerOpenAlex, rawURL, providerCache, &page); err != nil {
			return nil, false, err
		}
		for _, work := range page.Results {
			if worksSeen >= openAlexForwardCap {
				truncated = true
				break
			}
			worksSeen++
			if doi := normalizedGapDOI(work.DOI); doi != "" {
				dois = append(dois, doi)
			}
		}
		nextCursor := strings.TrimSpace(page.Meta.NextCursor)
		if worksSeen >= openAlexForwardCap && nextCursor != "" {
			truncated = true
		}
		if truncated || nextCursor == "" || nextCursor == cursor {
			break
		}
		cursor = nextCursor
	}
	return dois, truncated, nil
}

func sortedStringSet(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}
