// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean 37jv): metadata-enriched imports. URL import used to create a bare
// webpage item whose title was the URL itself. This adds a translator-style
// pipeline: a DOI embedded in the URL resolves full metadata from CrossRef, and
// otherwise the page's embedded "citation_*"/Open Graph/Dublin Core meta tags
// (the same signals Zotero's Embedded Metadata translator reads) are mapped into
// a typed item. A bare webpage item remains the last-resort fallback. Both
// import url and import doi gain a --dry-run preview of the request body.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var (
	doiInURLRE    = regexp.MustCompile(`10\.\d{4,9}/[^\s"'<>?#]+`)
	metaTagRE     = regexp.MustCompile(`(?is)<meta\b[^>]*>`)
	titleTagRE    = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	attrContentRE = regexp.MustCompile(`(?is)\bcontent\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))`)
	attrNameRE    = regexp.MustCompile(`(?is)\bname\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))`)
	attrPropRE    = regexp.MustCompile(`(?is)\bproperty\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))`)
)

// PATCH(glean zotero-pp-cli-43513a119010f6e1,zotero-pp-cli-fc0741de747e391d):
// shared cap for ad-hoc external provider responses that are buffered locally.
func readCappedExternalBody(r io.Reader, maxBytes int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("external response exceeded %d bytes", maxBytes)
	}
	return body, nil
}

// buildImportItemFromURL resolves the richest item it can for a URL: CrossRef
// metadata when a DOI is embedded in the URL, otherwise embedded page metadata,
// otherwise a bare webpage item. The returned string describes the source used.
func buildImportItemFromURL(ctx context.Context, httpClient *http.Client, rawURL string) (map[string]any, string) {
	accessDate := time.Now().Format("2006-01-02")

	if doi := extractDOIFromURL(rawURL); doi != "" {
		if item, ok := crossRefItemFromDOI(ctx, httpClient, doi); ok {
			item["url"] = rawURL
			item["accessDate"] = accessDate
			return item, "CrossRef (DOI " + doi + ")"
		}
	}

	if metas, pageTitle, ok := fetchPageMeta(ctx, httpClient, rawURL); ok {
		if item := itemFromEmbeddedMeta(metas, pageTitle, rawURL, accessDate); item != nil {
			return item, "embedded metadata"
		}
	}

	return map[string]any{
		"itemType":   "webpage",
		"title":      rawURL,
		"url":        rawURL,
		"accessDate": accessDate,
	}, "fallback (no metadata)"
}

// extractDOIFromURL pulls a DOI out of a URL (e.g. publisher article or PDF
// links), trimming a trailing .pdf and stray punctuation.
func extractDOIFromURL(rawURL string) string {
	m := doiInURLRE.FindString(rawURL)
	if m == "" {
		return ""
	}
	m = strings.TrimSuffix(m, ".pdf")
	m = strings.TrimSuffix(m, ".full")
	m = strings.TrimRight(m, ".,);]")
	return normalizeDOI(m)
}

// crossRefItemFromDOI fetches a work by DOI and maps it to a Zotero item,
// including a stripped abstract when present. Reuses the import_doi mapper.
func crossRefItemFromDOI(ctx context.Context, httpClient *http.Client, doi string) (map[string]any, bool) {
	var resp crossRefWorkResponse
	if err := getJSON(ctx, httpClient, enrichCrossRefBase+"/works/"+url.PathEscape(doi), &resp); err != nil {
		return nil, false
	}
	return crossRefItemFromWork(resp.Message, doi), true
}

// fetchPageMeta downloads an HTML page (size-capped) and returns its meta tags
// keyed by name/property plus the <title>. Returns ok=false for non-HTML
// responses (e.g. a raw PDF) or transport errors.
func fetchPageMeta(ctx context.Context, httpClient *http.Client, rawURL string) (map[string][]string, string, bool) {
	// PATCH(glean zotero-pp-cli-357222230859d0f3): URL imports may fetch
	// arbitrary user input, so metadata scraping is limited to public HTTP(S)
	// endpoints and never probes loopback/private/link-local services.
	if err := validateExternalHTTPURL(rawURL, false); err != nil {
		return nil, "", false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", false
	}
	req.Header.Set("User-Agent", "zotio/1.0.0")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := externalFetchHTTPClient(httpClient, false).Do(req)
	if err != nil {
		return nil, "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", false
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" && !strings.Contains(strings.ToLower(ct), "html") {
		return nil, "", false
	}
	body, err := readCappedExternalBody(resp.Body, 1<<20)
	if err != nil {
		return nil, "", false
	}
	metas, title := parseHTMLMeta(string(body))
	if len(metas) == 0 && title == "" {
		return nil, "", false
	}
	return metas, title, true
}

// parseHTMLMeta extracts <meta> name/property -> content values (repeated keys
// preserved, e.g. multiple citation_author tags) and the <title> text.
func parseHTMLMeta(body string) (map[string][]string, string) {
	metas := map[string][]string{}
	for _, tag := range metaTagRE.FindAllString(body, -1) {
		content := firstSubmatch(attrContentRE.FindStringSubmatch(tag))
		if content == "" {
			continue
		}
		key := firstSubmatch(attrNameRE.FindStringSubmatch(tag))
		if key == "" {
			key = firstSubmatch(attrPropRE.FindStringSubmatch(tag))
		}
		if key == "" {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		metas[key] = append(metas[key], strings.TrimSpace(html.UnescapeString(content)))
	}
	title := ""
	if m := titleTagRE.FindStringSubmatch(body); m != nil {
		title = strings.TrimSpace(html.UnescapeString(m[1]))
	}
	return metas, title
}

// itemFromEmbeddedMeta maps embedded metadata to a typed Zotero item. Returns
// nil when no usable title is present so the caller can fall back.
func itemFromEmbeddedMeta(metas map[string][]string, pageTitle, rawURL, accessDate string) map[string]any {
	title := firstMeta(metas, "citation_title", "og:title", "dc.title", "twitter:title")
	if title == "" {
		title = pageTitle
	}
	if title == "" {
		return nil
	}

	item := map[string]any{
		"title":      title,
		"url":        rawURL,
		"accessDate": accessDate,
	}

	doi := normalizeDOI(firstMeta(metas, "citation_doi", "dc.identifier", "prism.doi"))
	if doi != "" {
		item["DOI"] = doi
	}
	if creators := creatorsFromMeta(metas); len(creators) > 0 {
		item["creators"] = creators
	}
	if date := firstMeta(metas, "citation_publication_date", "citation_date", "citation_online_date", "dc.date", "article:published_time", "prism.publicationdate"); date != "" {
		item["date"] = date
	}
	if pub := firstMeta(metas, "citation_journal_title", "citation_conference_title", "prism.publicationname", "dc.source", "og:site_name"); pub != "" {
		item["publicationTitle"] = pub
	}
	if abstract := firstMeta(metas, "citation_abstract", "dc.description", "og:description", "description"); abstract != "" {
		item["abstractNote"] = abstract
	}
	if vol := firstMeta(metas, "citation_volume", "prism.volume"); vol != "" {
		item["volume"] = vol
	}
	if iss := firstMeta(metas, "citation_issue", "prism.number"); iss != "" {
		item["issue"] = iss
	}

	item["itemType"] = embeddedItemType(metas)
	return item
}

// embeddedItemType infers a Zotero item type from which embedded signals exist.
func embeddedItemType(metas map[string][]string) string {
	switch {
	case has(metas, "citation_journal_title") || has(metas, "citation_doi") || has(metas, "prism.doi"):
		return "journalArticle"
	case has(metas, "citation_conference_title"):
		return "conferencePaper"
	case has(metas, "citation_arxiv_id") || has(metas, "citation_technical_report_institution"):
		return "preprint"
	case has(metas, "citation_isbn"), has(metas, "citation_inbook_title"):
		return "bookSection"
	default:
		return "webpage"
	}
}

// creatorsFromMeta builds author creators from citation_author / dc.creator
// tags (repeated tags become multiple authors).
func creatorsFromMeta(metas map[string][]string) []map[string]any {
	names := metas["citation_author"]
	if len(names) == 0 {
		names = metas["citation_authors"]
	}
	if len(names) == 0 {
		names = metas["dc.creator"]
	}
	// citation_authors is sometimes a single semicolon-separated string.
	if len(names) == 1 && strings.Contains(names[0], ";") {
		names = splitAndTrim(names[0], ";")
	}
	creators := make([]map[string]any, 0, len(names))
	for _, n := range names {
		if c := parseCreatorName(n); c != nil {
			creators = append(creators, c)
		}
	}
	return creators
}

// parseCreatorName parses "Last, First" or "First Last" into a Zotero creator.
func parseCreatorName(name string) map[string]any {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	if comma := strings.Index(name, ","); comma >= 0 {
		last := strings.TrimSpace(name[:comma])
		first := strings.TrimSpace(name[comma+1:])
		return map[string]any{"creatorType": "author", "lastName": last, "firstName": first}
	}
	if sp := strings.LastIndex(name, " "); sp >= 0 {
		return map[string]any{"creatorType": "author", "firstName": strings.TrimSpace(name[:sp]), "lastName": strings.TrimSpace(name[sp+1:])}
	}
	return map[string]any{"creatorType": "author", "name": name}
}

// printImportDryRun renders the proposed item body for --dry-run.
func printImportDryRun(cmd *cobra.Command, item map[string]any, source string, flags *rootFlags) error {
	envelope := map[string]any{
		"dry_run": true,
		"source":  source,
		"item":    item,
	}
	if flags.asJSON {
		data, err := json.Marshal(envelope)
		if err != nil {
			return err
		}
		return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), flags)
	}
	pretty, err := json.MarshalIndent(item, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Would import via %s:\n%s\n", source, pretty)
	return nil
}

// --- small helpers ---

func firstSubmatch(m []string) string {
	if len(m) < 2 {
		return ""
	}
	for _, g := range m[1:] {
		if g != "" {
			return g
		}
	}
	return ""
}

func firstMeta(metas map[string][]string, keys ...string) string {
	for _, k := range keys {
		if vals := metas[k]; len(vals) > 0 && strings.TrimSpace(vals[0]) != "" {
			return strings.TrimSpace(vals[0])
		}
	}
	return ""
}

func has(metas map[string][]string, key string) bool {
	return len(metas[key]) > 0
}

func splitAndTrim(s, sep string) []string {
	parts := strings.Split(s, sep)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
