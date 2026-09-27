// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Metadata-enriched imports. URL import used to create a bare
// webpage item whose title was the URL itself. This adds a translator-style
// pipeline: a DOI embedded in the URL resolves full metadata from CrossRef, and
// otherwise the page's embedded "citation_*"/Open Graph/Dublin Core meta tags
// (the same signals Zotero's Embedded Metadata translator reads) are mapped into
// a typed item. A bare webpage item remains the last-resort fallback.

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
)

var (
	doiInURLRE     = regexp.MustCompile(`10\.\d{4,9}/[^\s"'<>?#]+`)
	doiShapeRE     = regexp.MustCompile(`^10\.\d{4,9}/\S+$`)
	doiOrgURLRE    = regexp.MustCompile(`(?i)^(?:https?://(?:dx\.)?doi\.org/|doi:\s*|info:doi/|urn:doi:)\S+`)
	metaTagRE      = regexp.MustCompile(`(?is)<meta\b[^>]*>`)
	titleTagRE     = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	jsonLDScriptRE = regexp.MustCompile(`(?is)<script\b[^>]*\btype\s*=\s*(?:"application/ld\+json"|'application/ld\+json'|application/ld\+json)[^>]*>(.*?)</script>`)
	scriptBlockRE  = regexp.MustCompile(`(?is)<script\b.*?</script>|<style\b.*?</style>|<!--.*?-->`)
	htmlTagRE      = regexp.MustCompile(`<[^>]*>`)
	attrContentRE  = regexp.MustCompile(`(?is)\bcontent\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))`)
	attrNameRE     = regexp.MustCompile(`(?is)\bname\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))`)
	attrPropRE     = regexp.MustCompile(`(?is)\bproperty\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))`)
)

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

// pageContent is a fetched HTML page kept for metadata extraction: the parsed
// meta tags and <title> plus the raw body for JSON-LD and challenge checks.
type pageContent struct {
	metas   map[string][]string
	title   string
	body    string
	status  int
	headers http.Header
}

// buildImportItemFromURL resolves the richest item it can for a URL: CrossRef
// metadata when a DOI is embedded in the URL, then CrossRef metadata for a DOI
// found in the page metadata, otherwise embedded page metadata, otherwise a
// bare webpage item. The returned string describes the source used. A
// bot-challenge page is a hard error: its interstitial title must never
// become an item.
func buildImportItemFromURL(ctx context.Context, httpClient *http.Client, rawURL string) (map[string]any, string, error) {
	accessDate := time.Now().Format("2006-01-02")

	if doi := extractDOIFromURL(rawURL); doi != "" {
		if item, err := crossRefItemFromDOI(ctx, httpClient, doi); err == nil {
			item["url"] = rawURL
			item["accessDate"] = accessDate
			return item, "CrossRef (DOI " + doi + ")", nil
		}
	}

	page, ok := fetchPageMeta(ctx, httpClient, rawURL)
	if !ok {
		return fallbackWebpageItem(rawURL, accessDate), "fallback (no metadata)", nil
	}
	if reason := challengeReason(page); reason != "" {
		return nil, "", fmt.Errorf("import url: page at %s looks like a bot challenge (%s); not importing its placeholder title", rawURL, reason)
	}

	ldDocs := parseJSONLDBlocks(page.body)
	primary := selectPrimaryJSONLD(ldDocs, page.metas, rawURL)
	embedded := itemFromEmbeddedMeta(page, primary, rawURL, accessDate)
	if doi := extractDOIFromPageMeta(page.metas, primary); doi != "" {
		if item, err := crossRefItemFromDOI(ctx, httpClient, doi); err == nil {
			item["url"] = rawURL
			item["accessDate"] = accessDate
			// Crossref owns the record, but it can leave volume, issue, pages
			// or the abstract empty where the page names them: backfill only
			// empty fields, only from sources tied to this same work, and
			// only onto fields the record's item type defines.
			backfillFromPage(item, page.metas, primary, doi)
			return item, "CrossRef (page DOI " + doi + ")", nil
		}
	}

	if embedded != nil {
		return embedded, "embedded metadata", nil
	}

	return fallbackWebpageItem(rawURL, accessDate), "fallback (no metadata)", nil
}

func fallbackWebpageItem(rawURL, accessDate string) map[string]any {
	return map[string]any{
		"itemType":   "webpage",
		"title":      rawURL,
		"url":        rawURL,
		"accessDate": accessDate,
	}
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
func crossRefItemFromDOI(ctx context.Context, httpClient *http.Client, doi string) (map[string]any, error) {
	var resp crossRefWorkResponse
	if err := getJSON(ctx, httpClient, enrichCrossRefBase+"/works/"+url.PathEscape(doi), &resp); err != nil {
		return nil, fmt.Errorf("fetching CrossRef metadata: %w", err)
	}
	if strings.TrimSpace(firstCrossRefString(resp.Message.Title, "")) == "" {
		return nil, fmt.Errorf("invalid CrossRef response for DOI %q: missing message title", doi)
	}
	return crossRefItemFromWork(resp.Message, doi), nil
}

// fetchPageMeta downloads an HTML page (size-capped) and returns its meta tags
// keyed by name/property, the <title>, and the raw body for JSON-LD and
// challenge checks. Returns ok=false for non-HTML responses (e.g. a raw PDF),
// empty pages, or transport errors.
func fetchPageMeta(ctx context.Context, httpClient *http.Client, rawURL string) (pageContent, bool) {
	// URL imports may fetch
	// arbitrary user input, so metadata scraping is limited to public HTTP(S)
	// endpoints and never probes loopback/private/link-local services.
	if err := validateExternalHTTPURL(rawURL, false); err != nil {
		return pageContent{}, false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return pageContent{}, false
	}
	req.Header.Set("User-Agent", "zotio/1.0.0")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := externalFetchHTTPClient(httpClient, false).Do(req)
	if err != nil {
		return pageContent{}, false
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" && !strings.Contains(strings.ToLower(ct), "html") {
		return pageContent{}, false
	}
	body, err := readCappedExternalBody(resp.Body, 1<<20)
	if err != nil {
		return pageContent{}, false
	}
	metas, title := parseHTMLMeta(string(body))
	page := pageContent{metas: metas, title: title, body: string(body), status: resp.StatusCode, headers: resp.Header}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Bot walls often answer 403/429/503 with the interstitial as the
		// body. Hand a challenge back so the caller refuses it; any other
		// non-2xx page carries no metadata worth trusting.
		if challengeReason(page) != "" {
			return page, true
		}
		return pageContent{}, false
	}
	// A challenge page with no metas and no title (AWS WAF serves an empty
	// title) still returns ok: the caller must refuse it, not fall back.
	if len(metas) == 0 && title == "" && !hasJSONLDBlock(page.body) && challengeReason(page) == "" {
		return pageContent{}, false
	}
	return page, true
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

// itemFromEmbeddedMeta maps embedded metadata to a typed Zotero item. article
// is the page's primary JSON-LD entity (selectPrimaryJSONLD) or nil. Returns
// nil when no usable title is present so the caller can fall back.
func itemFromEmbeddedMeta(page pageContent, article map[string]any, rawURL, accessDate string) map[string]any {
	metas := page.metas
	title := firstMeta(metas, "citation_title")
	if title == "" && article != nil {
		title = jsonldTitle(article)
	}
	if title == "" {
		title = firstMeta(metas, "og:title", "dc.title", "twitter:title")
	}
	if title == "" {
		title = page.title
	}
	if title == "" {
		return nil
	}

	itemType := embeddedItemType(metas, jsonldArticleType(article))
	item := map[string]any{
		"itemType":   itemType,
		"title":      title,
		"url":        rawURL,
		"accessDate": accessDate,
	}

	if doi := extractDOIFromPageMeta(metas, article); doi != "" {
		item["DOI"] = doi
	}
	if creators := creatorsFromMeta(metas, article); len(creators) > 0 {
		item["creators"] = creators
	}
	if date := embeddedDate(metas, article); date != "" {
		item["date"] = date
	}
	if pub := embeddedVenue(metas, article); pub != "" {
		if field := embeddedContainerField(itemType); field != "" {
			item[field] = pub
		}
	}
	if abstract := firstMeta(metas, "citation_abstract", "dc.description", "og:description", "description"); abstract != "" {
		item["abstractNote"] = abstract
	}
	wantVolume, wantIssue, wantPages := itemTypeCitationFields(itemType)
	if vol := firstMeta(metas, "citation_volume", "prism.volume"); vol != "" && wantVolume {
		item["volume"] = vol
	}
	if iss := firstMeta(metas, "citation_issue", "prism.number"); iss != "" && wantIssue {
		item["issue"] = iss
	}
	if pages := pagesFromMeta(metas, article); pages != "" && wantPages {
		item["pages"] = pages
	}
	return item
}

// embeddedContainerField names the Zotero field that holds the venue for an
// embedded item type, or "" when the type has no container field (book).
func embeddedContainerField(itemType string) string {
	switch itemType {
	case "journalArticle", "magazineArticle", "newspaperArticle":
		return "publicationTitle"
	case "conferencePaper":
		return "proceedingsTitle"
	case "bookSection":
		return "bookTitle"
	case "preprint":
		return "repository"
	case "webpage":
		return "websiteTitle"
	default:
		return ""
	}
}

// backfillFromPage fills CrossRef-empty volume, issue, pages and abstract on
// a page-DOI record, never overwriting a provider value and never writing a
// field the record's item type does not define. Sources must describe the
// same work: the page's citation/PRISM metas (skipped when they name another
// DOI) and the primary JSON-LD node only when its own DOI is the record's.
func backfillFromPage(item map[string]any, metas map[string][]string, primary map[string]any, doi string) {
	itemType, _ := item["itemType"].(string)
	wantVolume, wantIssue, wantPages := itemTypeCitationFields(itemType)
	metasSameWork := true
	if metaDOI := metaPageDOI(metas); metaDOI != "" && !strings.EqualFold(metaDOI, doi) {
		metasSameWork = false
	}
	ldSameWork := false
	if primary != nil {
		for _, d := range jsonldNodeDOIs(primary) {
			if strings.EqualFold(d, doi) {
				ldSameWork = true
				break
			}
		}
	}
	fill := func(key, value string) {
		if strings.TrimSpace(value) == "" {
			return
		}
		if cur, _ := item[key].(string); strings.TrimSpace(cur) != "" {
			return
		}
		item[key] = strings.TrimSpace(value)
	}
	if metasSameWork {
		if wantVolume {
			fill("volume", firstMeta(metas, "citation_volume", "prism.volume"))
		}
		if wantIssue {
			fill("issue", firstMeta(metas, "citation_issue", "prism.number"))
		}
		if wantPages {
			fill("pages", pagesFromMeta(metas, nil))
		}
		fill("abstractNote", firstMeta(metas, "citation_abstract", "dc.description", "og:description", "description"))
	}
	if ldSameWork {
		if wantPages {
			fill("pages", jsonldPages(primary))
		}
		fill("abstractNote", jsonldString(primary, "abstract"))
	}
}

// embeddedItemType infers a Zotero item type from which embedded signals exist.
func embeddedItemType(metas map[string][]string, jsonldType string) string {
	switch {
	case has(metas, "citation_journal_title") || has(metas, "citation_doi") || has(metas, "prism.doi"):
		return "journalArticle"
	case has(metas, "citation_conference_title"):
		return "conferencePaper"
	case has(metas, "citation_arxiv_id") || has(metas, "citation_technical_report_institution"):
		return "preprint"
	case has(metas, "citation_isbn"), has(metas, "citation_inbook_title"):
		return "bookSection"
	case jsonldType == "scholarlyarticle" || jsonldType == "article":
		return "journalArticle"
	case jsonldType == "chapter":
		return "bookSection"
	case jsonldType == "book":
		return "book"
	default:
		return "webpage"
	}
}

// embeddedDate prefers citation dates, then the JSON-LD datePublished, then
// the looser page-level dates.
func embeddedDate(metas map[string][]string, article map[string]any) string {
	if date := firstMeta(metas, "citation_publication_date", "citation_date", "citation_online_date"); date != "" {
		return date
	}
	if article != nil {
		if date := jsonldString(article, "datePublished"); date != "" {
			return date
		}
	}
	return firstMeta(metas, "dc.date", "article:published_time", "prism.publicationdate")
}

// embeddedVenue prefers citation venues, then the JSON-LD container name,
// then the looser page-level venue.
func embeddedVenue(metas map[string][]string, article map[string]any) string {
	if pub := firstMeta(metas, "citation_journal_title", "citation_conference_title"); pub != "" {
		return pub
	}
	if article != nil {
		if venue := jsonldVenue(article["isPartOf"]); venue != "" {
			return venue
		}
	}
	return firstMeta(metas, "prism.publicationname", "dc.source", "og:site_name")
}

// pagesFromMeta reads page ranges from citation, PRISM, then JSON-LD signals.
// fb:pages is deliberately excluded: it names a Facebook page, not pages.
func pagesFromMeta(metas map[string][]string, article map[string]any) string {
	if first := firstMeta(metas, "citation_firstpage"); first != "" {
		if last := firstMeta(metas, "citation_lastpage"); last != "" {
			return first + "-" + last
		}
		return first
	}
	if start := firstMeta(metas, "prism.startingpage"); start != "" {
		if end := firstMeta(metas, "prism.endingpage"); end != "" {
			return start + "-" + end
		}
		return start
	}
	if article != nil {
		return jsonldPages(article)
	}
	return ""
}

// creatorsFromMeta builds author creators from citation_author tags, then
// JSON-LD authors, then dc.creator tags (repeated tags become multiple
// authors).
func creatorsFromMeta(metas map[string][]string, article map[string]any) []map[string]any {
	names := metas["citation_author"]
	if len(names) == 0 {
		names = metas["citation_authors"]
	}
	if creators := creatorsFromNames(names); len(creators) > 0 {
		return creators
	}
	if article != nil {
		if creators := jsonldAuthors(article["author"]); len(creators) > 0 {
			return creators
		}
	}
	return creatorsFromNames(metas["dc.creator"])
}

func creatorsFromNames(names []string) []map[string]any {
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

// --- bot-challenge detection ---

// challengeReason reports why a fetched page looks like a bot-challenge or
// interstitial page, or "" when it looks like a real page. Every body rule
// pairs vendor structure with challenge title or text, so an article about a
// vendor, a page embedding a comment widget, or a provider that injects
// detection scripts into ordinary responses stays a normal page.
func challengeReason(page pageContent) string {
	// Header evidence first: the vendor names the challenge itself.
	if action := strings.TrimSpace(page.headers.Get("X-Amzn-Waf-Action")); strings.EqualFold(action, "challenge") || strings.EqualFold(action, "captcha") {
		return "AWS WAF " + action + " action"
	}
	if strings.EqualFold(strings.TrimSpace(page.headers.Get("Cf-Mitigated")), "challenge") {
		return "Cloudflare challenge (cf-mitigated)"
	}

	lowerHTML := strings.ToLower(page.body)
	text := visibleText(page.body)
	lowerCombined := strings.ToLower(page.title + "\n" + text)
	lowerTitle := strings.ToLower(strings.TrimSpace(page.title))
	hasChallengeText := strings.Contains(lowerCombined, "verifying you are human") ||
		strings.Contains(lowerCombined, "verify you are human") ||
		strings.Contains(lowerCombined, "checking your browser") ||
		strings.Contains(lowerCombined, "review the security of your connection") ||
		strings.Contains(lowerCombined, "complete the security check") ||
		strings.Contains(lowerCombined, "are you a robot")
	// Challenge wording only: a vendor name alone ("Cloudflare Challenge
	// Methods") is an article title, not an interstitial.
	challengeTitle := hasChallengeText ||
		lowerTitle == "security check" ||
		strings.Contains(lowerTitle, "verify you are human") ||
		strings.Contains(lowerTitle, "confirm you are human") ||
		strings.Contains(lowerTitle, "one more step")

	// Cloudflare interstitial titles are decisive on their own: the widget
	// markers are injected at runtime, so served HTML may carry only the title.
	if strings.HasPrefix(lowerTitle, "just a moment") {
		return `Cloudflare challenge title "Just a moment"`
	}
	if strings.Contains(lowerTitle, "attention required") {
		return `Cloudflare challenge title "Attention Required"`
	}

	// Cloudflare Turnstile or challenge-platform scripts plus a challenge
	// title. The platform script alone is not a marker: providers inject it
	// into ordinary responses.
	hasTurnstile := strings.Contains(lowerHTML, "challenges.cloudflare.com/turnstile") ||
		strings.Contains(lowerHTML, "cf-turnstile") ||
		strings.Contains(lowerHTML, "cf-chl-") ||
		strings.Contains(lowerHTML, "challenge-form") ||
		strings.Contains(lowerHTML, "challenge-running") ||
		strings.Contains(lowerHTML, "captcha-box")
	if hasTurnstile && (challengeTitle || hasChallengeText) {
		return "Cloudflare challenge widget with challenge title"
	}
	if strings.Contains(lowerHTML, "/cdn-cgi/challenge-platform/") && (challengeTitle || hasChallengeText) {
		return "Cloudflare challenge platform with challenge title"
	}

	// Fastly client challenge: bootstrap script plus challenge text.
	if strings.Contains(lowerHTML, "_fs-ch-") && strings.Contains(lowerCombined, "challenge") {
		return "Fastly client challenge"
	}
	// AWS WAF: state blob, explicit vendor challenge text, or a 202
	// challenge with challenge title/text.
	if strings.Contains(lowerHTML, "gokuprops") {
		return "AWS WAF challenge"
	}
	if lowerText := strings.ToLower(text); strings.Contains(lowerText, "aws waf") &&
		(strings.Contains(lowerText, "challenge") || strings.Contains(lowerText, "captcha") || strings.Contains(lowerText, "bot")) {
		return "AWS WAF challenge text"
	}
	if page.status == http.StatusAccepted && (challengeTitle || hasChallengeText) {
		return "AWS WAF challenge (HTTP 202)"
	}
	// Anubis: vendor assets plus bot/challenge text. Bare mentions of the
	// vendor in article text do not count: the asset paths do.
	hasAnubisAssets := strings.Contains(lowerHTML, "anubis/static") ||
		strings.Contains(lowerHTML, "anubis_version") ||
		strings.Contains(lowerHTML, "anubis_challenge") ||
		strings.Contains(lowerHTML, "window.anubis") ||
		strings.Contains(lowerHTML, "/.within.website/")
	if hasAnubisAssets &&
		(strings.Contains(lowerCombined, "bot") || strings.Contains(lowerCombined, "challenge") ||
			strings.Contains(lowerCombined, "proof-of-work") || strings.Contains(lowerCombined, "proof of work") ||
			strings.Contains(lowerCombined, "making sure")) {
		return "Anubis challenge"
	}
	// PerimeterX / DataDome: vendor code plus verification text.
	hasPerimeterX := strings.Contains(lowerHTML, "px-captcha") ||
		strings.Contains(lowerHTML, "perimeterx.net") ||
		strings.Contains(lowerHTML, "_pxappid")
	if hasPerimeterX &&
		(strings.Contains(lowerCombined, "captcha") || strings.Contains(lowerCombined, "challenge") ||
			strings.Contains(lowerCombined, "verify") || strings.Contains(lowerCombined, "robot") ||
			strings.Contains(lowerCombined, "are you human")) {
		return "PerimeterX challenge"
	}
	if strings.Contains(lowerHTML, "datadome.co") &&
		(strings.Contains(lowerCombined, "captcha") || strings.Contains(lowerCombined, "challenge") ||
			strings.Contains(lowerCombined, "verify") || strings.Contains(lowerCombined, "robot")) {
		return "DataDome challenge"
	}
	// PMC browser check and generic proof-of-work interstitials. The solver
	// token must be code (a function or status string), not prose: an article
	// about proof-of-work stays normal.
	if strings.Contains(lowerHTML, "recaptcha") && strings.Contains(lowerCombined, "checking your browser") {
		return "browser check (reCAPTCHA)"
	}
	hasSolver := strings.Contains(lowerHTML, "proof-of-work") ||
		strings.Contains(lowerHTML, "solvepow") || strings.Contains(lowerHTML, "calculating solution")
	if hasSolver && (strings.Contains(lowerCombined, "security check") ||
		strings.Contains(lowerCombined, "verify your connection") || strings.Contains(lowerCombined, "please wait a moment")) {
		return "proof-of-work challenge"
	}
	return ""
}

// visibleText strips scripts, styles, comments and tags for challenge-text
// matching, bounded so a large page stays cheap.
func visibleText(body string) string {
	text := scriptBlockRE.ReplaceAllString(body, " ")
	text = htmlTagRE.ReplaceAllString(text, " ")
	text = html.UnescapeString(text)
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > 40000 {
		text = text[:40000]
	}
	return text
}

// --- page DOI extraction ---

// isDOIShape reports whether s is a bare DOI after normalization.
func isDOIShape(s string) bool {
	return doiShapeRE.MatchString(normalizeDOI(s))
}

// metaPageDOI returns the page's DOI from its meta tags: citation_doi, then a
// DOI-shaped dc.identifier, then prism.doi. A dc.identifier that is not
// DOI-shaped (an article number, an ARK) is ignored.
func metaPageDOI(metas map[string][]string) string {
	for _, key := range []string{"citation_doi", "dc.identifier", "prism.doi"} {
		for _, v := range metas[key] {
			if doi := normalizeDOI(v); isDOIShape(doi) {
				return doi
			}
		}
	}
	return ""
}

// extractDOIFromPageMeta finds a page's DOI: the meta-tag DOI first
// (citation_doi wins over any JSON-LD DOI), then the primary JSON-LD
// entity's own identifier/doi/@id/sameAs DOI. Nested nodes (citations,
// related works, isPartOf) are never consulted: they name other works.
func extractDOIFromPageMeta(metas map[string][]string, primary map[string]any) string {
	if doi := metaPageDOI(metas); doi != "" {
		return doi
	}
	if dois := jsonldNodeDOIs(primary); len(dois) > 0 {
		return dois[0]
	}
	return ""
}

// jsonldNodeDOIs returns the DOIs a JSON-LD node states for itself: its
// doi/identifier values (strings, PropertyValue objects, or lists of them)
// and doi.org forms of its @id/sameAs. It does not descend into other keys.
func jsonldNodeDOIs(node map[string]any) []string {
	if node == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	add := func(doi string) {
		if doi != "" && !seen[strings.ToLower(doi)] {
			seen[strings.ToLower(doi)] = true
			out = append(out, doi)
		}
	}
	var identifier func(v any)
	identifier = func(v any) {
		switch t := v.(type) {
		case string:
			if doi := normalizeDOI(t); isDOIShape(doi) {
				add(doi)
			} else if m := doiInURLRE.FindString(t); m != "" {
				if doi := normalizeDOI(m); isDOIShape(doi) {
					add(doi)
				}
			}
		case map[string]any:
			// A PropertyValue names the DOI in "value" (or links it in @id).
			if val, ok := t["value"].(string); ok {
				identifier(val)
			}
			doiOrgLink(t["@id"], add)
		case []any:
			for _, e := range t {
				identifier(e)
			}
		}
	}
	for _, key := range []string{"doi", "identifier"} {
		identifier(node[key])
	}
	for _, key := range []string{"@id", "sameAs"} {
		doiOrgLink(node[key], add)
	}
	return out
}

// doiOrgLink keeps only doi.org (or doi:) forms of an @id/sameAs value.
func doiOrgLink(v any, add func(doi string)) {
	switch t := v.(type) {
	case string:
		if doiOrgURLRE.MatchString(strings.TrimSpace(t)) {
			if doi := normalizeDOI(t); isDOIShape(doi) {
				add(doi)
			}
		}
	case []any:
		for _, e := range t {
			doiOrgLink(e, add)
		}
	}
}

// --- JSON-LD article extraction ---

// hasJSONLDBlock reports whether the page carries any JSON-LD script block.
func hasJSONLDBlock(body string) bool {
	return jsonLDScriptRE.MatchString(body)
}

// parseJSONLDBlocks parses every ld+json script block, skipping blocks that
// do not parse. @graph members are flattened to top-level documents.
func parseJSONLDBlocks(body string) []any {
	var docs []any
	for _, m := range jsonLDScriptRE.FindAllStringSubmatch(body, -1) {
		if len(m) < 2 {
			continue
		}
		text := strings.TrimSpace(html.UnescapeString(m[1]))
		if text == "" {
			continue
		}
		var parsed any
		if err := json.Unmarshal([]byte(text), &parsed); err != nil {
			continue
		}
		docs = append(docs, flattenJSONLDDoc(parsed)...)
	}
	return docs
}

func flattenJSONLDDoc(v any) []any {
	m, ok := v.(map[string]any)
	if !ok {
		if list, ok := v.([]any); ok {
			var out []any
			for _, e := range list {
				out = append(out, flattenJSONLDDoc(e)...)
			}
			return out
		}
		return nil
	}
	if graph, ok := m["@graph"]; ok {
		if list, ok := graph.([]any); ok {
			var out []any
			for _, e := range list {
				out = append(out, flattenJSONLDDoc(e)...)
			}
			return out
		}
	}
	return []any{m}
}

// jsonldArticleTypeNames are the ld+json types mapped into the embedded path.
var jsonldArticleTypeNames = map[string]bool{
	"scholarlyarticle": true,
	"article":          true,
	"book":             true,
	"chapter":          true,
}

// selectPrimaryJSONLD picks the page's own ScholarlyArticle/Article/Book/
// Chapter entity from the top-level (and @graph) documents, or nil when no
// candidate can be trusted to describe this page. In order:
//  1. a candidate whose own DOI equals the meta-tag DOI;
//  2. with a meta-tag DOI, candidates stating a different DOI are dropped
//     (they describe other works);
//  3. a single remaining candidate;
//  4. the candidate whose url/@id/mainEntityOfPage is the page URL;
//  5. several unanchored candidates naming more than one DOI are ambiguous
//     (a listing or related-works page): nil;
//  6. otherwise the first ScholarlyArticle, then the first candidate.
func selectPrimaryJSONLD(docs []any, metas map[string][]string, pageURL string) map[string]any {
	var cands []map[string]any
	for _, doc := range docs {
		if m, ok := doc.(map[string]any); ok && jsonldArticleTypeNames[jsonldArticleType(m)] {
			cands = append(cands, m)
		}
	}
	if metaDOI := metaPageDOI(metas); metaDOI != "" {
		kept := cands[:0:0]
		for _, c := range cands {
			dois := jsonldNodeDOIs(c)
			for _, d := range dois {
				if strings.EqualFold(d, metaDOI) {
					return c
				}
			}
			if len(dois) == 0 {
				kept = append(kept, c)
			}
		}
		cands = kept
	}
	switch len(cands) {
	case 0:
		return nil
	case 1:
		return cands[0]
	}
	if want := normalizePageURL(pageURL); want != "" {
		for _, c := range cands {
			if jsonldNodeNamesURL(c, want) {
				return c
			}
		}
	}
	distinct := map[string]bool{}
	for _, c := range cands {
		for _, d := range jsonldNodeDOIs(c) {
			distinct[strings.ToLower(d)] = true
		}
	}
	if len(distinct) > 1 {
		return nil
	}
	for _, c := range cands {
		if jsonldArticleType(c) == "scholarlyarticle" {
			return c
		}
	}
	return cands[0]
}

// jsonldNodeNamesURL reports whether a node's url, @id or mainEntityOfPage
// (string or {"@id": …}) is the normalized page URL.
func jsonldNodeNamesURL(node map[string]any, want string) bool {
	for _, key := range []string{"url", "@id", "mainEntityOfPage"} {
		switch v := node[key].(type) {
		case string:
			if normalizePageURL(v) == want {
				return true
			}
		case map[string]any:
			if s, ok := v["@id"].(string); ok && normalizePageURL(s) == want {
				return true
			}
		}
	}
	return false
}

// normalizePageURL compares URLs by host and path, ignoring scheme, case,
// a leading www. and a trailing slash.
func normalizePageURL(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	s = strings.TrimPrefix(s, "www.")
	return strings.TrimRight(s, "/")
}

func jsonldArticleType(m map[string]any) string {
	switch t := m["@type"].(type) {
	case string:
		return strings.ToLower(strings.TrimSpace(t))
	case []any:
		for _, e := range t {
			if s, ok := e.(string); ok && jsonldArticleTypeNames[strings.ToLower(strings.TrimSpace(s))] {
				return strings.ToLower(strings.TrimSpace(s))
			}
		}
	}
	return ""
}

func jsonldTitle(article map[string]any) string {
	return jsonldString(article, "headline", "name")
}

func jsonldString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// jsonldAuthors maps a JSON-LD author value (string, person object, or list)
// to Zotero creators.
func jsonldAuthors(v any) []map[string]any {
	var creators []map[string]any
	add := func(c map[string]any) {
		if c != nil {
			creators = append(creators, c)
		}
	}
	switch t := v.(type) {
	case string:
		add(parseCreatorName(t))
	case map[string]any:
		add(jsonldPerson(t))
	case []any:
		for _, e := range t {
			switch a := e.(type) {
			case string:
				add(parseCreatorName(a))
			case map[string]any:
				add(jsonldPerson(a))
			}
		}
	}
	return creators
}

func jsonldPerson(m map[string]any) map[string]any {
	if name := jsonldString(m, "name"); name != "" {
		return parseCreatorName(name)
	}
	given := jsonldString(m, "givenName")
	family := jsonldString(m, "familyName")
	if given == "" && family == "" {
		return nil
	}
	c := map[string]any{"creatorType": "author"}
	if family != "" {
		c["lastName"] = family
		if given != "" {
			c["firstName"] = given
		}
		return c
	}
	c["name"] = given
	return c
}

// jsonldVenue reads the container name from isPartOf (object, name, or
// list). A bare URL is not a venue name and is skipped.
func jsonldVenue(v any) string {
	switch t := v.(type) {
	case string:
		if s := strings.TrimSpace(t); s != "" && !strings.Contains(s, "://") {
			return s
		}
	case map[string]any:
		return jsonldString(t, "name")
	case []any:
		for _, e := range t {
			if venue := jsonldVenue(e); venue != "" {
				return venue
			}
		}
	}
	return ""
}

// jsonldPages reads pagination or a pageStart/pageEnd pair.
func jsonldPages(article map[string]any) string {
	if p := jsonldString(article, "pagination"); p != "" {
		return p
	}
	start := jsonldString(article, "pageStart")
	if start == "" {
		return ""
	}
	if end := jsonldString(article, "pageEnd"); end != "" {
		return start + "-" + end
	}
	return start
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
