// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// MeSH enrichment for `items enrich --missing-mesh`: resolve a PubMed record
// for an item (its PMID, or a DOI looked up through ESearch), read the
// record's MeshHeadingList through EFetch, and propose the descriptors as
// automatic "mesh/<descriptor>" tags through the same tag-proposal path
// --missing-subjects uses.

package cli

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"zotio/internal/cliutil"
)

const (
	// Namespacing keeps a managed MeSH term distinguishable from a tag the
	// operator typed, and makes removing them again a single prefix.
	enrichMeshTagPrefix = "mesh/"

	// NCBI asks anonymous E-utilities clients to stay at or below three
	// requests per second. The limiter is shared by every lookup the command
	// runs, so the bounded fan-out cannot exceed it.
	enrichPubMedRatePerSec = 3
)

// enrichPubMedLimiter paces every ESearch and EFetch call. Tests replace it
// with nil, which disables pacing.
var enrichPubMedLimiter = cliutil.NewAdaptiveLimiter(enrichPubMedRatePerSec)

var (
	pubmedDigitsRE     = regexp.MustCompile(`^[0-9]+$`)
	pubmedExtraPMIDRE  = regexp.MustCompile(`(?im)^[ \t]*PMID:[ \t]*([0-9]+)[ \t]*$`)
	pubmedMissingDocRE = regexp.MustCompile(`(?i)cannot get document summary`)
)

// pubmedESearchResponse is the part of an ESearch JSON reply the DOI lookup
// reads. NCBI reports some failures inside an HTTP 200 body, so the error
// members are decoded as well.
type pubmedESearchResponse struct {
	Error         string `json:"error"`
	ESearchResult struct {
		IDList []string `json:"idlist"`
		Error  string   `json:"ERROR"`
	} `json:"esearchresult"`
}

// pubmedEFetchResult decodes an EFetch XML reply. The root element is
// PubmedArticleSet for a normal reply and eFetchResult for an NCBI error, so
// no XMLName is pinned.
type pubmedEFetchResult struct {
	Articles []pubmedArticle `xml:"PubmedArticle"`
	Errors   []string        `xml:"ERROR"`
}

type pubmedArticle struct {
	PMID       string              `xml:"MedlineCitation>PMID"`
	Headings   []pubmedMeshHeading `xml:"MedlineCitation>MeshHeadingList>MeshHeading"`
	ArticleIDs []pubmedArticleID   `xml:"PubmedData>ArticleIdList>ArticleId"`
}

// pubmedMeshHeading keeps the descriptor only. Qualifier subheadings and the
// MajorTopicYN marker are deliberately not read: see resolveMeshViaPubMed.
type pubmedMeshHeading struct {
	Descriptor string `xml:"DescriptorName"`
}

type pubmedArticleID struct {
	Type  string `xml:"IdType,attr"`
	Value string `xml:",chardata"`
}

// pubmedIDFromItem returns the item's PMID: Zotero's own PMID field first,
// then a "PMID:" line in Extra, the legacy home other tools still write (see
// items_find.go). Anything that is not all digits is ignored rather than
// sent to NCBI.
func pubmedIDFromItem(data map[string]any) string {
	if pmid := strings.TrimSpace(stringFromMap(data, "PMID")); pubmedDigitsRE.MatchString(pmid) {
		return pmid
	}
	if m := pubmedExtraPMIDRE.FindStringSubmatch(stringFromMap(data, "extra")); m != nil {
		return m[1]
	}
	return ""
}

// pubmedEtiquette returns the identification parameters NCBI asks every
// E-utilities client to send, so it can contact the operator before blocking
// a misbehaving tool.
func pubmedEtiquette(email string) url.Values {
	q := url.Values{}
	q.Set("tool", "zotio")
	if email = strings.TrimSpace(email); email != "" {
		q.Set("email", email)
	}
	return q
}

// pubmedPMIDForDOI resolves a DOI to a PMID with ESearch's [AID] (article
// identifier) field. The NCBI ID Converter was rejected: it only knows
// records that are in PubMed Central, which would miss most paywalled
// biomedical articles, the exact items MeSH triage matters for.
//
// It returns an empty PMID with a reason when PubMed has no record or more
// than one, because tagging the wrong paper is worse than tagging none.
func pubmedPMIDForDOI(ctx context.Context, httpClient *http.Client, doi, email string) (pmid, reason string, err error) {
	q := pubmedEtiquette(email)
	q.Set("db", "pubmed")
	q.Set("retmode", "json")
	q.Set("retmax", "2")
	q.Set("term", `"`+doi+`"[AID]`)
	enrichPubMedLimiter.WaitContext(ctx)
	var resp pubmedESearchResponse
	found, err := getProviderJSON(ctx, httpClient, "PubMed", importPubMedBase+"/esearch.fcgi?"+q.Encode(), &resp)
	if err != nil {
		return "", "", err
	}
	if !found {
		return "", "PubMed has no record for this DOI", nil
	}
	if msg := strings.TrimSpace(resp.Error + resp.ESearchResult.Error); msg != "" {
		return "", "", fmt.Errorf("PubMed: ESearch: %s", msg)
	}
	ids := make([]string, 0, len(resp.ESearchResult.IDList))
	for _, id := range resp.ESearchResult.IDList {
		if id = strings.TrimSpace(id); pubmedDigitsRE.MatchString(id) {
			ids = append(ids, id)
		}
	}
	switch len(ids) {
	case 0:
		return "", "PubMed has no record for this DOI", nil
	case 1:
		return ids[0], "", nil
	default:
		return "", "this DOI matches more than one PubMed record; add a PMID to disambiguate", nil
	}
}

// pubmedFetchArticle returns the EFetch record for a PMID. A missing record
// (HTTP 404, an empty set, or NCBI's "cannot get document summary") is a
// clean no-match; transport failures, 429/5xx, undecodable bodies, and any
// other NCBI error are provider errors.
func pubmedFetchArticle(ctx context.Context, httpClient *http.Client, pmid, email string) (pubmedArticle, bool, error) {
	q := pubmedEtiquette(email)
	q.Set("db", "pubmed")
	q.Set("retmode", "xml")
	q.Set("id", pmid)
	enrichPubMedLimiter.WaitContext(ctx)
	var result pubmedEFetchResult
	found, err := getProviderXML(ctx, httpClient, "PubMed", importPubMedBase+"/efetch.fcgi?"+q.Encode(), &result)
	if err != nil || !found {
		return pubmedArticle{}, false, err
	}
	if len(result.Articles) == 0 {
		for _, msg := range result.Errors {
			msg = strings.TrimSpace(msg)
			if msg != "" && !pubmedMissingDocRE.MatchString(msg) {
				return pubmedArticle{}, false, fmt.Errorf("PubMed: EFetch: %s", msg)
			}
		}
		return pubmedArticle{}, false, nil
	}
	for _, article := range result.Articles {
		if strings.TrimSpace(article.PMID) == pmid {
			return article, true, nil
		}
	}
	return result.Articles[0], true, nil
}

// pubmedArticleDOI returns the DOI PubMed lists for a record, normalized.
func pubmedArticleDOI(article pubmedArticle) string {
	for _, id := range article.ArticleIDs {
		if strings.EqualFold(strings.TrimSpace(id.Type), "doi") {
			return strings.ToLower(normalizeDOI(id.Value))
		}
	}
	return ""
}

// meshTagsFromArticle maps a record's MeshHeadingList to namespaced tags in
// PubMed's order, dropping blanks and repeats.
func meshTagsFromArticle(article pubmedArticle) []string {
	tags := make([]string, 0, len(article.Headings))
	seen := make(map[string]bool, len(article.Headings))
	for _, heading := range article.Headings {
		name := strings.Join(strings.Fields(heading.Descriptor), " ")
		if name == "" {
			continue
		}
		tag := enrichMeshTagPrefix + name
		if seen[tag] {
			continue
		}
		seen[tag] = true
		tags = append(tags, tag)
	}
	return tags
}

// storedItemTagNames returns the tag names on the item as the local mirror
// last saw them.
func storedItemTagNames(data map[string]any) map[string]bool {
	names := map[string]bool{}
	raw, _ := data["tags"].([]any)
	for _, entry := range raw {
		if obj, ok := entry.(map[string]any); ok {
			if name, _ := obj["tag"].(string); name != "" {
				names[name] = true
			}
		}
	}
	return names
}

// resolveMeshViaPubMed proposes "mesh/<descriptor>" tags for one item.
//
// Identity: the item's own PMID wins. Without one, its DOI is resolved to a
// PMID with ESearch, and the EFetch record must list the same DOI before any
// tag is proposed.
//
// Terms: every DescriptorName in the MeshHeadingList, in PubMed's order.
// Qualifiers (e.g. "drug therapy") are dropped because a descriptor/qualifier
// pair multiplies the tag count and splits one subject across many facets;
// the major-topic marker is dropped because it is an indexer judgment about
// emphasis, and a reviewer filtering on "mesh/Neoplasms" wants every record
// about it. Both stay available in PubMed for anyone who needs them.
//
// Tags the stored item already carries are never proposed again. The result
// is (tags, source, "", nil) on success, (nil, "", reason, nil) for a clean
// no-match, and (nil, "", "", err) when PubMed could not answer.
func resolveMeshViaPubMed(ctx context.Context, httpClient *http.Client, data map[string]any, email string) ([]string, string, string, error) {
	if email == "" {
		email = enrichUnpaywallEmail()
	}
	pmid := pubmedIDFromItem(data)
	doi := ""
	if pmid == "" {
		doi = normalizeDOI(stringFromMap(data, "DOI"))
		if doi == "" {
			return nil, "", "no PMID or DOI to look up MeSH terms", nil
		}
		var reason string
		var err error
		pmid, reason, err = pubmedPMIDForDOI(ctx, httpClient, doi, email)
		if err != nil || pmid == "" {
			return nil, "", reason, err
		}
	}
	article, found, err := pubmedFetchArticle(ctx, httpClient, pmid, email)
	if err != nil {
		return nil, "", "", err
	}
	if !found {
		return nil, "", fmt.Sprintf("PubMed has no record for PMID %s", pmid), nil
	}
	if doi != "" {
		if got := pubmedArticleDOI(article); got != strings.ToLower(doi) {
			return nil, "", fmt.Sprintf("PubMed record PMID %s does not list DOI %s", pmid, doi), nil
		}
	}
	all := meshTagsFromArticle(article)
	if len(all) == 0 {
		return nil, "", fmt.Sprintf("PubMed record PMID %s has no MeSH headings (it may not be indexed yet)", pmid), nil
	}
	have := storedItemTagNames(data)
	tags := make([]string, 0, len(all))
	for _, tag := range all {
		if !have[tag] {
			tags = append(tags, tag)
		}
	}
	if len(tags) == 0 {
		return nil, "", "item already carries every MeSH tag PubMed lists", nil
	}
	return tags, "PubMed (PMID " + pmid + ")", "", nil
}

// getProviderXML is getProviderJSON for XML replies: record found and
// decoded, authoritative "no such record" (HTTP 404/410), or a provider
// error. The body is capped before decoding.
func getProviderXML(ctx context.Context, httpClient *http.Client, provider, rawURL string, out any) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return false, fmt.Errorf("%s: %w", provider, err)
	}
	req.Header.Set("Accept", "application/xml, text/xml;q=0.9")
	req.Header.Set("User-Agent", "zotio/1.0.0")
	resp, err := sameOriginExternalFetchHTTPClient(httpClient, false).Do(req)
	if err != nil {
		return false, fmt.Errorf("%s: %w", provider, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		statusErr := fmt.Errorf("HTTP %d", resp.StatusCode)
		if isRegistryRecordAbsent(statusErr) {
			return false, nil
		}
		return false, fmt.Errorf("%s: %w", provider, statusErr)
	}
	body, err := readCappedExternalBody(resp.Body, maxEnrichProviderResponseBytes)
	if err != nil {
		return false, fmt.Errorf("%s: %w", provider, err)
	}
	if err := xml.Unmarshal(body, out); err != nil {
		return false, fmt.Errorf("%s: parsing XML: %w", provider, err)
	}
	return true, nil
}

// meshQueueSQL selects the --missing-mesh work queue: top-level items that
// carry an identifier PubMed can resolve (a PMID in the native field or in
// Extra, or a DOI) and no mesh/ tag yet. Excluding already-tagged items keeps
// the queue, which --limit truncates before any provider call, from being
// spent on items finished by an earlier run.
const meshQueueSQL = `
SELECT
	id AS key,
	json_extract(data, '$.data.title') AS title,
	json_extract(data, '$.data.itemType') AS item_type,
	json_extract(data, '$.data.DOI') AS doi,
	json_extract(data, '$.data.dateAdded') AS date_added
FROM resources
WHERE resource_type = 'items'
	AND ` + libraryTopLevelItemsPredicate + `
	AND (
		TRIM(COALESCE(json_extract(data, '$.data.PMID'), '')) <> ''
		OR TRIM(COALESCE(json_extract(data, '$.data.DOI'), '')) <> ''
		OR COALESCE(json_extract(data, '$.data.extra'), '') LIKE '%PMID:%'
	)
	AND NOT EXISTS (
		SELECT 1 FROM json_each(json_extract(data, '$.data.tags'))
		WHERE json_extract(value, '$.tag') LIKE 'mesh/%'
	)`

// queryMissingMeshItems returns the --missing-mesh queue. A nil keys slice is
// the library-wide (or collection-scoped) queue; otherwise it is narrowed to
// the named keys with the same predicate, so a --scope/--keys cohort filters
// the queue and never redefines it.
func queryMissingMeshItems(db localQueryStore, limit int, collection string, keys []string) ([]map[string]any, error) {
	query := meshQueueSQL
	args := enrichCollectionFilterArgs(&query, "data", collection)
	if keys != nil {
		if len(keys) == 0 {
			return nil, nil
		}
		query += `
	AND id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",") + `)`
		for _, key := range keys {
			args = append(args, key)
		}
	}
	query += `
ORDER BY date_added DESC`
	return queryItemsAuditRows(db, query, limit, args...)
}
