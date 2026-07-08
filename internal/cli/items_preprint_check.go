// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// PATCH: Add hand-written arXiv preprint publication check workflow missing from the generated CLI.

package cli

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type preprintCheckResult struct {
	Key     string `json:"key"`
	Title   string `json:"title"`
	ArxivID string `json:"arxiv_id"`
	Status  string `json:"status"`
	DOI     string `json:"doi,omitempty"`
	Venue   string `json:"venue,omitempty"`
	Year    int    `json:"year,omitempty"`
}

type crossrefMatch struct {
	DOI   string
	Venue string
	Year  int
}

const (
	arxivAtomQueryURL   = "https://export.arxiv.org/api/query"
	arxivSelfDOIPrefix  = "10.48550/arxiv."
	crossrefWorksURL    = "https://api.crossref.org/works/"
	crossrefUserAgent   = "zotio/1.0 (+https://github.com/OrgMentem/zotio)"
	crossrefContentType = "application/json"
)

var (
	arxivURLPattern   = regexp.MustCompile(`(?i)arxiv\.org/(?:abs|pdf)/([a-z-]+/[0-9]{7}|[0-9]{4}\.[0-9]{4,5})(?:v[0-9]+)?`)
	arxivExtraPattern = regexp.MustCompile(`(?i)arxiv\s*:\s*([a-z-]+/[0-9]{7}|[0-9]{4}\.[0-9]{4,5})(?:v[0-9]+)?`)
	arxivVersionTail  = regexp.MustCompile(`(?i)v[0-9]+$`)
)

func newItemsPreprintCheckCmd(flags *rootFlags) *cobra.Command {
	var flagLimit int

	cmd := &cobra.Command{
		Use:         "preprint-check",
		Short:       "Check arXiv preprints for published CrossRef records",
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := flags.newClient()
			if err != nil {
				return err
			}
			candidates, err := fetchArxivPreprintCandidates(c, flagLimit)
			if err != nil {
				return classifyAPIError(err, flags)
			}

			httpClient := &http.Client{Timeout: flags.timeout}
			results := make([]preprintCheckResult, 0, len(candidates))
			for i, item := range candidates {
				if i > 0 {
					time.Sleep(200 * time.Millisecond)
				}
				arxivID := extractArxivID(item)
				result := preprintCheckResult{
					Key:     zoteroString(item, "key"),
					Title:   zoteroString(item, "title"),
					ArxivID: arxivID,
					Status:  "preprint",
				}
				match, found, err := lookupCrossrefArxiv(cmd.Context(), httpClient, arxivID)
				if err != nil {
					return err
				}
				if found {
					result.Status = "published"
					result.DOI = match.DOI
					result.Venue = match.Venue
					result.Year = match.Year
				}
				results = append(results, result)
			}
			return printCommandJSON(cmd.OutOrStdout(), results, flags)
		},
	}
	cmd.Flags().IntVar(&flagLimit, "limit", 20, "Maximum number of preprints to check")
	// PATCH(marketing-heroes): expose preview-first remediation for published arXiv preprints.
	cmd.AddCommand(newItemsPreprintCheckFixCmd(flags))

	return cmd
}

// PATCH(marketing-heroes): preserve the detect path's arXiv-ID filtering after factoring shared candidate fetches.
func fetchArxivPreprintCandidates(c zoteroGetter, limit int) ([]map[string]any, error) {
	return fetchPreprintCheckCandidates(c, limit, true)
}

// PATCH(marketing-heroes): let the fix command see raw candidates so no_arxiv_id skips are visible without changing detect output.
func fetchPreprintCheckFixCandidates(c zoteroGetter, limit int) ([]map[string]any, error) {
	return fetchPreprintCheckCandidates(c, limit, false)
}

// PATCH(marketing-heroes): share candidate paging/deduping while allowing fix to report no_arxiv_id skips.
func fetchPreprintCheckCandidates(c zoteroGetter, limit int, requireArxivID bool) ([]map[string]any, error) {
	fetchLimit := limit
	if fetchLimit <= 0 || fetchLimit < 100 {
		fetchLimit = 100
	}
	sources := []map[string]string{
		{"itemType": "preprint"},
		{"q": "arxiv"},
	}
	seen := map[string]bool{}
	candidates := make([]map[string]any, 0)
	for _, params := range sources {
		items, err := fetchZoteroItems(c, "/items", params, fetchLimit)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			key := zoteroString(item, "key")
			if key == "" || seen[key] {
				continue
			}
			if requireArxivID && extractArxivID(item) == "" {
				continue
			}
			seen[key] = true
			candidates = append(candidates, item)
			if limit > 0 && len(candidates) >= limit {
				return candidates, nil
			}
		}
	}
	return candidates, nil
}

func extractArxivID(item map[string]any) string {
	for _, value := range []string{
		zoteroString(item, "url"),
		zoteroString(item, "extra"),
		zoteroString(item, "archiveID"),
		zoteroString(item, "repository"),
	} {
		if id := extractArxivIDFromString(value); id != "" {
			return id
		}
	}
	return ""
}

func extractArxivIDFromString(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	for _, pattern := range []*regexp.Regexp{arxivURLPattern, arxivExtraPattern} {
		matches := pattern.FindStringSubmatch(value)
		if len(matches) > 1 {
			return normalizeArxivID(matches[1])
		}
	}
	return ""
}

func normalizeArxivID(id string) string {
	id = strings.TrimSpace(id)
	id = strings.TrimSuffix(id, ".pdf")
	return arxivVersionTail.ReplaceAllString(id, "")
}

func lookupCrossrefArxiv(ctx context.Context, httpClient *http.Client, arxivID string) (crossrefMatch, bool, error) {
	arxivID = normalizeArxivID(arxivID)
	if arxivID == "" {
		return crossrefMatch{}, false, nil
	}
	doi, found, err := lookupArxivExternalDOI(ctx, httpClient, arxivID)
	if err != nil || !found {
		return crossrefMatch{}, false, err
	}
	return lookupCrossrefDOI(ctx, httpClient, doi)
}

type arxivAtomFeed struct {
	Entries []arxivAtomEntry `xml:"entry"`
}

type arxivAtomEntry struct {
	DOI   string          `xml:"doi"`
	Links []arxivAtomLink `xml:"link"`
}

type arxivAtomLink struct {
	Title string `xml:"title,attr"`
	Href  string `xml:"href,attr"`
}

func lookupArxivExternalDOI(ctx context.Context, httpClient *http.Client, arxivID string) (string, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, arxivAtomQueryURL, nil)
	if err != nil {
		return "", false, err
	}
	q := req.URL.Query()
	q.Set("id_list", arxivID)
	req.URL.RawQuery = q.Encode()
	req.Header.Set("User-Agent", crossrefUserAgent)

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", false, fmt.Errorf("querying arXiv metadata for %s: %w", arxivID, err)
	}
	defer resp.Body.Close()
	// PATCH(glean zotero-pp-cli-fc0741de747e391d): cap external arXiv Atom
	// responses before buffering them for XML parsing.
	body, err := readCappedExternalBody(resp.Body, 4<<20)
	if err != nil {
		return "", false, fmt.Errorf("reading arXiv metadata for %s: %w", arxivID, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", false, fmt.Errorf("arXiv metadata lookup for %s returned HTTP %d: %s", arxivID, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var feed arxivAtomFeed
	if err := xml.Unmarshal(body, &feed); err != nil {
		return "", false, fmt.Errorf("parsing arXiv metadata for %s: %w", arxivID, err)
	}
	for _, entry := range feed.Entries {
		if doi := externalDOIFromArxivEntry(entry); doi != "" {
			return doi, true, nil
		}
	}
	return "", false, nil
}

func externalDOIFromArxivEntry(entry arxivAtomEntry) string {
	candidates := []string{entry.DOI}
	for _, link := range entry.Links {
		if strings.EqualFold(strings.TrimSpace(link.Title), "doi") {
			candidates = append(candidates, link.Href)
		}
	}
	for _, candidate := range candidates {
		doi := normalizeDOI(candidate)
		if doi == "" || isArxivSelfDOI(doi) {
			continue
		}
		return doi
	}
	return ""
}

func lookupCrossrefDOI(ctx context.Context, httpClient *http.Client, doi string) (crossrefMatch, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, crossrefWorksURL+url.PathEscape(doi), nil)
	if err != nil {
		return crossrefMatch{}, false, err
	}
	req.Header.Set("Accept", crossrefContentType)
	req.Header.Set("User-Agent", crossrefUserAgent)

	resp, err := httpClient.Do(req)
	if err != nil {
		return crossrefMatch{}, false, fmt.Errorf("querying CrossRef for DOI %s: %w", doi, err)
	}
	defer resp.Body.Close()
	// PATCH(glean zotero-pp-cli-fc0741de747e391d): cap external CrossRef
	// responses before buffering them for JSON parsing.
	body, err := readCappedExternalBody(resp.Body, 4<<20)
	if err != nil {
		return crossrefMatch{}, false, fmt.Errorf("reading CrossRef response for DOI %s: %w", doi, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return crossrefMatch{}, false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return crossrefMatch{}, false, fmt.Errorf("CrossRef lookup for DOI %s returned HTTP %d: %s", doi, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var decoded crossRefWorkResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return crossrefMatch{}, false, fmt.Errorf("parsing CrossRef response for DOI %s: %w", doi, err)
	}
	return crossrefMatchFromWork(decoded.Message, doi), true, nil
}

func crossrefMatchFromWork(work crossRefWork, fallbackDOI string) crossrefMatch {
	match := crossrefMatch{
		DOI: normalizeDOI(work.DOI),
	}
	if match.DOI == "" {
		match.DOI = fallbackDOI
	}
	if len(work.ContainerTitle) > 0 {
		match.Venue = work.ContainerTitle[0]
	}
	if len(work.Published.DateParts) > 0 && len(work.Published.DateParts[0]) > 0 {
		match.Year = work.Published.DateParts[0][0]
	}
	return match
}

func normalizeDOI(value string) string {
	doi := strings.TrimSpace(value)
	doi = strings.Trim(doi, "\"'<>")
	if parsed, err := url.Parse(doi); err == nil {
		host := strings.ToLower(parsed.Hostname())
		if host == "doi.org" || host == "dx.doi.org" {
			doi = strings.TrimPrefix(parsed.Path, "/")
		}
	}
	lower := strings.ToLower(doi)
	for _, prefix := range []string{"https://doi.org/", "http://doi.org/", "https://dx.doi.org/", "http://dx.doi.org/", "doi:"} {
		if strings.HasPrefix(lower, prefix) {
			doi = doi[len(prefix):]
			break
		}
	}
	doi = strings.TrimSpace(doi)
	doi = strings.Trim(doi, "\"'<>")
	return strings.TrimRight(doi, ".,;")
}

func isArxivSelfDOI(doi string) bool {
	return strings.HasPrefix(strings.ToLower(doi), arxivSelfDOIPrefix)
}
