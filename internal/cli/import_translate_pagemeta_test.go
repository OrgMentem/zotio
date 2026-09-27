// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Fixtures below are trimmed to the decisive tags from public corpus pages in
// the translator-replay investigation (R/corpus/pages), noted per fixture.

// fastlyChallengeHTML is trimmed from doi-0005 (Springer "Client Challenge").
const fastlyChallengeHTML = `<!DOCTYPE html><html lang="en"><head>
<link href="/_fs-ch-1T1wmsGaOgGaSxcX/assets/styles.css" rel="stylesheet" />
<meta name="viewport" content="width=device-width, initial-scale=1" />
<title>Client Challenge</title>
</head><body><div id="challenge">
<noscript><span>JavaScript is disabled in your browser.</span><p>Please enable JavaScript to proceed.</p></noscript>
<p>A required part of this site couldn't load. This may be due to a browser extension.</p>
</div><script src="/_fs-ch-1T1wmsGaOgGaSxcX/assets/client.js"></script></body></html>`

// cloudflareChallengeHTML is the canonical Cloudflare interstitial shape:
// the widget markers are injected at runtime, so served HTML may carry only
// the title plus challenge text.
const cloudflareChallengeHTML = `<!DOCTYPE html><html><head>
<title>Just a moment...</title>
</head><body><h1>Just a moment...</h1>
<p>Verifying you are human. This may take a few seconds.</p>
<p>Performance &amp; security by Cloudflare.</p>
</body></html>`

const cloudflareCaptchaHTML = `<!DOCTYPE html><html><head>
<title>Attention Required! | Cloudflare</title>
</head><body><form id="challenge-form" action="/cdn-cgi/challenge" method="post">
<p>Please complete the security check to access the site.</p>
<div class="cf-turnstile" data-sitekey="x"></div>
</form></body></html>`

// awsWAFChallengeHTML is trimmed from doi-0029 (IEEE); the challenge state
// blob is replaced with a placeholder, since only the gokuProps marker counts.
const awsWAFChallengeHTML = `<!DOCTYPE html><html lang="en"><head>
<meta charset="utf-8"><title></title>
<script type="text/javascript">window.awsWafCookieDomainList = [];
window.gokuProps = {"key":"synthetic-challenge-state"};</script>
</head><body></body></html>`

// anubisChallengeHTML is trimmed from doi-0094 (Columbia).
const anubisChallengeHTML = `<!doctype html><html lang="en"><head>
<title>Making sure you&#39;re not a bot!</title>
<meta name="robots" content="noindex,nofollow">
<script src="/.within.website/x/anubis/js/main.js"></script>
</head><body><div class="centered-div">
<h1>Making sure you&#39;re not a bot!</h1>
<p id="status">Loading...</p>
<p>You are seeing this because the administrator of this website has set up Anubis to protect the server.</p>
</div></body></html>`

// powChallengeHTML is trimmed from doi-0111 (intrechtdok "Security Check").
const powChallengeHTML = `<!DOCTYPE html><html lang="en"><head>
<meta charset="UTF-8"><title>Security Check</title>
<script>async function solvePOW(challenge){ return calculatingSolution(challenge); }</script>
</head><body><div class="container"><h1>Security Check</h1>
<p>Please wait a moment while we verify your connection...</p>
</div></body></html>`

// turnstileWidgetHTML is a normal article that embeds a Turnstile comment
// widget (the doi.org-404 shape): it must not read as a challenge.
const turnstileWidgetHTML = `<!doctype html><html><head>
<title>Some Research Finding — Example Blog</title>
<script src="https://challenges.cloudflare.com/turnstile/v0/api.js"></script>
<meta name="citation_title" content="Some Research Finding">
<meta name="citation_author" content="Doe, Jane">
</head><body><article><h1>Some Research Finding</h1>
<p>Results and discussion.</p>
<form><div class="cf-turnstile" data-sitekey="comments"></div></form>
</article></body></html>`

// challengePlatformNormalHTML is a normal article on a provider that injects
// the Cloudflare detection script into ordinary responses (Cambridge shape).
const challengePlatformNormalHTML = `<!doctype html><html><head>
<title>A survey of extraction methods | Example Press</title>
<script src="/cdn-cgi/challenge-platform/scripts/jsd/main.js"></script>
<meta name="citation_title" content="A survey of extraction methods">
<meta name="citation_doi" content="10.1017/S135132492100036X">
</head><body><article><h1>A survey of extraction methods</h1></article></body></html>`

// recaptchaNormalHTML is a normal article that loads reCAPTCHA (Frontiers
// shape): without "checking your browser" text it is not a challenge.
const recaptchaNormalHTML = `<!doctype html><html><head>
<title>Functional analysis | Frontiers</title>
<script src="https://www.google.com/recaptcha/api.js"></script>
<meta name="citation_title" content="Functional analysis">
</head><body><article><h1>Functional analysis</h1></article></body></html>`

// jsonldArticleHTML carries only JSON-LD structured metadata plus <title>.
const jsonldArticleHTML = `<!doctype html><html><head>
<title>Publisher landing page</title>
<script type="application/ld+json">
{"@context":"https://schema.org","@type":"ScholarlyArticle",
"headline":"Attention Is All You Need",
"author":[{"@type":"Person","familyName":"Vaswani","givenName":"Ashish"},"Noam Shazeer"],
"datePublished":"2017-06-12",
"isPartOf":{"@type":"Periodical","name":"Advances in Neural Information Processing Systems"},
"identifier":"10.5555/3524938.3524999",
"pagination":"5998-6008"}
</script>
</head><body><h1>Attention Is All You Need</h1></body></html>`

// pageDOIHTML has a DOI in page metadata but not in the URL.
const pageDOIHTML = `<!doctype html><html><head>
<title>Publisher landing page</title>
<meta name="citation_title" content="Embedded Title, Not From CrossRef">
<meta name="citation_doi" content="10.1234/pagedoi">
<meta name="citation_journal_title" content="Embedded Venue">
</head><body>...</body></html>`

func serveHTML(t *testing.T, body string) *httptest.Server {
	t.Helper()
	old := allowPrivateOutboundForTests.Load()
	allowPrivateOutboundForTests.Store(true)
	t.Cleanup(func() { allowPrivateOutboundForTests.Store(old) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func stubCrossRef(t *testing.T, hits *int, body string, status int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits++
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	withBase(t, &enrichCrossRefBase, srv.URL)
}

func TestBuildImportItemFromURL_PageDOIGetsCrossRef(t *testing.T) {
	hits := 0
	stubCrossRef(t, &hits, `{"message":{"type":"journal-article","title":["CrossRef Title"],"DOI":"10.1234/pagedoi","container-title":["CrossRef Venue"]}}`, 0)
	pagesrv := serveHTML(t, pageDOIHTML)

	item, source, err := buildImportItemFromURL(context.Background(), http.DefaultClient, pagesrv.URL+"/landing/no-doi-here")
	if err != nil {
		t.Fatalf("buildImportItemFromURL: %v", err)
	}
	if source != "CrossRef (page DOI 10.1234/pagedoi)" {
		t.Fatalf("source = %q, want page-DOI CrossRef", source)
	}
	if item["title"] != "CrossRef Title" {
		t.Errorf("title = %v, want the CrossRef record, not embedded metadata", item["title"])
	}
	if hits != 1 {
		t.Errorf("CrossRef hits = %d, want 1", hits)
	}
}

func TestExtractDOIFromPageMeta_Forms(t *testing.T) {
	cases := []struct {
		name  string
		metas map[string][]string
		ld    string
		url   string
		want  string
	}{
		{"citation_doi", map[string][]string{"citation_doi": {"10.1234/bare"}}, "", "", "10.1234/bare"},
		{"dc.identifier doi prefix", map[string][]string{"dc.identifier": {"doi:10.1017/S135132492100036X"}}, "", "", "10.1017/S135132492100036X"},
		{"citation_doi wins over a different jsonld DOI", map[string][]string{"citation_doi": {"10.1111/meta"}}, `{"@type":"ScholarlyArticle","identifier":"10.2222/ld"}`, "", "10.1111/meta"},
		{"prism.doi", map[string][]string{"prism.doi": {"10.1088/2058-9565/ad8fca"}}, "", "", "10.1088/2058-9565/ad8fca"},
		{"dc.identifier article number ignored", map[string][]string{"dc.identifier": {"2439"}}, "", "", ""},
		{"dc.identifier ARK ignored", map[string][]string{"dc.identifier": {"ark:/12148/cb17045264z"}}, "", "", ""},
		{"jsonld identifier string", nil, `{"@type":"ScholarlyArticle","identifier":"10.5555/3524938.3524999"}`, "", "10.5555/3524938.3524999"},
		{"jsonld identifier object value", nil, `{"@type":"ScholarlyArticle","identifier":{"@type":"PropertyValue","value":"10.1234/objdoi"}}`, "", "10.1234/objdoi"},
		{"jsonld @id doi.org", nil, `{"@type":"ScholarlyArticle","@id":"https://doi.org/10.1234/atid"}`, "", "10.1234/atid"},
		{"jsonld sameAs list", nil, `{"@type":"ScholarlyArticle","sameAs":["https://example.org/x","https://doi.org/10.1234/sameas"]}`, "", "10.1234/sameas"},
		{"jsonld @id non-doi ignored", nil, `{"@type":"ScholarlyArticle","@id":"https://example.org/article/1"}`, "", ""},
		{"jsonld citation names another work", nil, `{"@type":"ScholarlyArticle","citation":["10.9999/other-work"]}`, "", ""},
		{"jsonld full schema.org type URL", nil, `{"@type":"https://schema.org/ScholarlyArticle","identifier":"10.1234/typeurl"}`, "", "10.1234/typeurl"},
		{"jsonld schema: prefixed type in a list", nil, `{"@type":["schema:WebPage","schema:ScholarlyArticle"],"identifier":"10.1234/prefixed"}`, "", "10.1234/prefixed"},
		{"jsonld graph flattened", nil, `{"@graph":[{"@type":"WebPage","name":"x"},{"@type":"ScholarlyArticle","identifier":"10.1234/graphdoi"}]}`, "", "10.1234/graphdoi"},
		{"unparseable jsonld skipped", nil, `not json at all`, "", ""},
		// A nested related-work node sorts before the primary's own
		// identifier; only the primary entity's own DOI may be used.
		{"nested related work ignored", nil, `{"@type":"ScholarlyArticle","citation":[{"@type":"ScholarlyArticle","identifier":"10.9999/related"}],"identifier":"10.1234/primary"}`, "", "10.1234/primary"},
		{"nested related work only", nil, `{"@type":"ScholarlyArticle","isBasedOn":{"@type":"ScholarlyArticle","identifier":"10.9999/related"}}`, "", ""},
		// A related top-level article listed first loses to the node that
		// names the page URL.
		{"top-level related first, primary anchored by URL", nil, `[{"@type":"ScholarlyArticle","identifier":"10.9999/related","url":"https://x.example/other"},{"@type":"ScholarlyArticle","identifier":"10.1234/primary","mainEntityOfPage":"https://x.example/landing/"}]`, "https://x.example/landing", "10.1234/primary"},
		{"several unanchored DOIs are ambiguous", nil, `[{"@type":"ScholarlyArticle","identifier":"10.9999/a"},{"@type":"ScholarlyArticle","identifier":"10.9999/b"}]`, "https://x.example/landing", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var docs []any
			if tc.ld != "" {
				docs = parseJSONLDBlocks(`<script type="application/ld+json">` + tc.ld + `</script>`)
			}
			primary := selectPrimaryJSONLD(docs, tc.metas, tc.url)
			if got := extractDOIFromPageMeta(tc.metas, primary); got != tc.want {
				t.Errorf("extractDOIFromPageMeta = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildImportItemFromURL_NonDOIDcIdentifierStaysEmbedded(t *testing.T) {
	hits := 0
	stubCrossRef(t, &hits, `{}`, 0)
	pagesrv := serveHTML(t, `<!doctype html><html><head>
<title>Landing</title>
<meta name="citation_title" content="OJS Article Title">
<meta name="DC.Identifier" content="2439">
</head><body>...</body></html>`)

	item, source, err := buildImportItemFromURL(context.Background(), http.DefaultClient, pagesrv.URL+"/article/view/2439")
	if err != nil {
		t.Fatalf("buildImportItemFromURL: %v", err)
	}
	if source != "embedded metadata" {
		t.Fatalf("source = %q, want embedded metadata", source)
	}
	if item["title"] != "OJS Article Title" {
		t.Errorf("title = %v", item["title"])
	}
	if _, ok := item["DOI"]; ok {
		t.Errorf("DOI = %v, want no DOI for an article number", item["DOI"])
	}
	if hits != 0 {
		t.Errorf("CrossRef hits = %d, want none for a non-DOI identifier", hits)
	}
}

func TestBuildImportItemFromURL_PageDOICrossRefFailureFallsBack(t *testing.T) {
	hits := 0
	stubCrossRef(t, &hits, ``, http.StatusNotFound)
	pagesrv := serveHTML(t, pageDOIHTML)

	item, source, err := buildImportItemFromURL(context.Background(), http.DefaultClient, pagesrv.URL+"/landing/no-doi-here")
	if err != nil {
		t.Fatalf("buildImportItemFromURL: %v", err)
	}
	if source != "embedded metadata" {
		t.Fatalf("source = %q, want embedded fallback when CrossRef misses", source)
	}
	if item["title"] != "Embedded Title, Not From CrossRef" {
		t.Errorf("title = %v, want embedded title", item["title"])
	}
	if item["DOI"] != "10.1234/pagedoi" {
		t.Errorf("DOI = %v, want the page DOI kept on fallback", item["DOI"])
	}
}

func TestBuildImportItemFromURL_PageDOIBackfillsEmptyFieldsOnly(t *testing.T) {
	hits := 0
	// CrossRef record carries volume but no issue, pages, or abstract: the
	// embedded volume must not overwrite it, the embedded issue/pages fill
	// the gaps, and the record keeps its own venue (never the embedded one).
	stubCrossRef(t, &hits, `{"message":{"type":"journal-article","title":["CrossRef Title"],"DOI":"10.1234/pagedoi","container-title":["CrossRef Venue"],"volume":"9"}}`, 0)
	pagesrv := serveHTML(t, `<!doctype html><html><head>
<title>Publisher landing page</title>
<meta name="citation_title" content="Embedded Title">
<meta name="citation_doi" content="10.1234/pagedoi">
<meta name="citation_volume" content="WRONG-VOLUME">
<meta name="citation_issue" content="7">
<meta name="citation_firstpage" content="100">
<meta name="citation_lastpage" content="110">
<meta name="citation_journal_title" content="Embedded Venue">
<meta name="citation_abstract" content="Embedded abstract.">
</head><body>...</body></html>`)

	item, source, err := buildImportItemFromURL(context.Background(), http.DefaultClient, pagesrv.URL+"/landing/no-doi-here")
	if err != nil {
		t.Fatalf("buildImportItemFromURL: %v", err)
	}
	if source != "CrossRef (page DOI 10.1234/pagedoi)" {
		t.Fatalf("source = %q, want page-DOI CrossRef", source)
	}
	if item["title"] != "CrossRef Title" || item["publicationTitle"] != "CrossRef Venue" {
		t.Errorf("item = %v, want the CrossRef record, not embedded title/venue", item)
	}
	if item["volume"] != "9" {
		t.Errorf("volume = %v, want the CrossRef value, not the embedded one", item["volume"])
	}
	if item["issue"] != "7" || item["pages"] != "100-110" || item["abstractNote"] != "Embedded abstract." {
		t.Errorf("item = %v, want issue/pages/abstract backfilled from the page", item)
	}
}

func TestBuildImportItemFromURL_ChallengePagesRefused(t *testing.T) {
	cases := map[string]string{
		"fastly":             fastlyChallengeHTML,
		"cloudflare-moment":  cloudflareChallengeHTML,
		"cloudflare-captcha": cloudflareCaptchaHTML,
		"aws-waf":            awsWAFChallengeHTML,
		"anubis":             anubisChallengeHTML,
		"proof-of-work":      powChallengeHTML,
		// A challenge page carrying metadata is still a challenge: fail closed.
		"challenge-with-doi": `<html><head><title>Client Challenge</title>
<meta name="citation_doi" content="10.1234/real"><script src="/_fs-ch-9x/client.js"></script>
</head><body><p>Client Challenge: enable JavaScript to proceed.</p></body></html>`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			hits := 0
			stubCrossRef(t, &hits, `{"message":{"type":"journal-article","title":["Must Not Win"],"DOI":"10.1234/real"}}`, 0)
			pagesrv := serveHTML(t, body)
			item, source, err := buildImportItemFromURL(context.Background(), http.DefaultClient, pagesrv.URL+"/article")
			if err == nil || !strings.Contains(err.Error(), "bot challenge") {
				t.Fatalf("err = %v (item=%v source=%q), want a bot-challenge error", err, item, source)
			}
			if item != nil {
				t.Errorf("item = %v, want nil for a challenge page", item)
			}
			if hits != 0 {
				t.Errorf("CrossRef hits = %d, want none: challenge refuses before lookup", hits)
			}
		})
	}
}

func TestChallengeReason_HeaderEvidence(t *testing.T) {
	page := pageContent{title: "Article", body: "<html><body>text</body></html>", headers: http.Header{"X-Amzn-Waf-Action": {"captcha"}}}
	if r := challengeReason(page); !strings.Contains(r, "AWS WAF") {
		t.Errorf("reason = %q, want AWS WAF", r)
	}
	page = pageContent{title: "Article", body: "<html><body>text</body></html>", headers: http.Header{"Cf-Mitigated": {"challenge"}}}
	if r := challengeReason(page); !strings.Contains(r, "Cloudflare") {
		t.Errorf("reason = %q, want Cloudflare", r)
	}
}

func TestBuildImportItemFromURL_VendorScriptsWithoutChallengeTitleStayNormal(t *testing.T) {
	for name, body := range map[string]string{
		"turnstile comment widget":  turnstileWidgetHTML,
		"challenge-platform script": challengePlatformNormalHTML,
		"recaptcha on article":      recaptchaNormalHTML,
	} {
		t.Run(name, func(t *testing.T) {
			pagesrv := serveHTML(t, body)
			item, source, err := buildImportItemFromURL(context.Background(), http.DefaultClient, pagesrv.URL+"/article")
			if err != nil {
				t.Fatalf("buildImportItemFromURL: %v, want no challenge error", err)
			}
			if source != "embedded metadata" && source != "CrossRef (page DOI 10.1017/S135132492100036X)" {
				t.Fatalf("source = %q, want a real item", source)
			}
			if item == nil {
				t.Fatal("want an item, got nil")
			}
		})
	}
}

func TestItemFromEmbeddedMeta_JSONLDArticle(t *testing.T) {
	metas, title := parseHTMLMeta(jsonldArticleHTML)
	docs := parseJSONLDBlocks(jsonldArticleHTML)
	item := itemFromEmbeddedMeta(pageContent{metas: metas, title: title, body: jsonldArticleHTML}, selectPrimaryJSONLD(docs, metas, "https://x/landing"), "https://x/landing", "2026-06-16")
	if item == nil {
		t.Fatal("expected an item from JSON-LD")
	}
	if item["title"] != "Attention Is All You Need" {
		t.Errorf("title = %v", item["title"])
	}
	if item["itemType"] != "journalArticle" {
		t.Errorf("itemType = %v, want journalArticle", item["itemType"])
	}
	if item["DOI"] != "10.5555/3524938.3524999" {
		t.Errorf("DOI = %v", item["DOI"])
	}
	if item["date"] != "2017-06-12" {
		t.Errorf("date = %v", item["date"])
	}
	if item["publicationTitle"] != "Advances in Neural Information Processing Systems" {
		t.Errorf("publicationTitle = %v", item["publicationTitle"])
	}
	if item["pages"] != "5998-6008" {
		t.Errorf("pages = %v", item["pages"])
	}
	creators, ok := item["creators"].([]map[string]any)
	if !ok || len(creators) != 2 {
		t.Fatalf("creators = %v, want 2", item["creators"])
	}
	if creators[0]["lastName"] != "Vaswani" || creators[0]["firstName"] != "Ashish" {
		t.Errorf("creator[0] = %v, want the givenName/familyName object", creators[0])
	}
	if creators[1]["lastName"] != "Shazeer" || creators[1]["firstName"] != "Noam" {
		t.Errorf("creator[1] = %v, want the parsed string author", creators[1])
	}
}

func TestPagesFromMeta(t *testing.T) {
	cases := []struct {
		name  string
		metas map[string][]string
		ld    string
		want  string
	}{
		{"citation first-last", map[string][]string{"citation_firstpage": {"128"}, "citation_lastpage": {"135"}}, "", "128-135"},
		{"citation first only", map[string][]string{"citation_firstpage": {"e012345"}}, "", "e012345"},
		{"prism starting-ending", map[string][]string{"prism.startingpage": {"100"}, "prism.endingpage": {"110"}}, "", "100-110"},
		{"citation beats prism", map[string][]string{"citation_firstpage": {"1"}, "citation_lastpage": {"9"}, "prism.startingpage": {"100"}}, "", "1-9"},
		{"jsonld pageStart pageEnd", nil, `{"@type":"ScholarlyArticle","pageStart":"10","pageEnd":"20"}`, "10-20"},
		{"jsonld pagination", nil, `{"@type":"ScholarlyArticle","pagination":"33-41"}`, "33-41"},
		{"fb:pages is not pages", map[string][]string{"fb:pages": {"106360295673577"}}, "", ""},
		{"none", nil, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var article map[string]any
			if tc.ld != "" {
				article = selectPrimaryJSONLD(parseJSONLDBlocks(`<script type="application/ld+json">`+tc.ld+`</script>`), nil, "")
			}
			if got := pagesFromMeta(tc.metas, article); got != tc.want {
				t.Errorf("pagesFromMeta = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEmbeddedItemType_JSONLD(t *testing.T) {
	empty := map[string][]string{}
	cases := map[string]string{
		`{"@type":"ScholarlyArticle"}`: "journalArticle",
		`{"@type":"Article"}`:          "journalArticle",
		`{"@type":"Chapter"}`:          "bookSection",
		`{"@type":"Book"}`:             "book",
		`{"@type":"NewsArticle"}`:      "webpage",
		`{"@type":"WebPage"}`:          "webpage",
	}
	for ld, want := range cases {
		article := selectPrimaryJSONLD(parseJSONLDBlocks(`<script type="application/ld+json">`+ld+`</script>`), nil, "")
		if got := embeddedItemType(empty, jsonldArticleType(article)); got != want {
			t.Errorf("ld %s: itemType = %q, want %q", ld, got, want)
		}
	}
	// Citation signals keep priority over JSON-LD.
	article := selectPrimaryJSONLD(parseJSONLDBlocks(`<script type="application/ld+json">{"@type":"Book"}</script>`), nil, "")
	if got := embeddedItemType(map[string][]string{"citation_conference_title": {"Proc"}}, jsonldArticleType(article)); got != "conferencePaper" {
		t.Errorf("itemType = %q, want conferencePaper", got)
	}
}

func TestImportURLChallengeSurfacesError(t *testing.T) {
	pagesrv := serveHTML(t, fastlyChallengeHTML)
	flags := rootFlags{asJSON: true, dryRun: true, timeout: 5 * time.Second}
	cmd := newImportUrlCmd(&flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{pagesrv.URL + "/article"})
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "bot challenge") {
		t.Fatalf("err = %v, want the bot-challenge error", err)
	}
}

// Title fallbacks: pages whose only title signal is og:title, dc.title,
// twitter:title, or <title> still import an embedded item with that title.
func TestItemFromEmbeddedMeta_TitleFallbacks(t *testing.T) {
	cases := map[string]string{
		`<meta property="og:title" content="OG Title">`:       "OG Title",
		`<meta name="dc.title" content="DC Title">`:           "DC Title",
		`<meta name="twitter:title" content="Twitter Title">`: "Twitter Title",
		``: "Page Title Only",
	}
	for head, want := range cases {
		body := `<html><head><title>Page Title Only</title>` + head + `</head><body>x</body></html>`
		metas, title := parseHTMLMeta(body)
		item := itemFromEmbeddedMeta(pageContent{metas: metas, title: title, body: body}, nil, "https://x.example/p", "2026-06-16")
		if item == nil || item["title"] != want {
			t.Errorf("head %q: item = %v, want title %q", head, item, want)
		}
	}
}

// Backfill must not copy fields from a related JSON-LD article that is not
// the page-DOI's work, even when it is the only JSON-LD article on the page.
func TestBuildImportItemFromURL_BackfillIgnoresRelatedJSONLD(t *testing.T) {
	hits := 0
	stubCrossRef(t, &hits, `{"message":{"type":"journal-article","title":["CrossRef Title"],"DOI":"10.1234/pagedoi"}}`, 0)
	pagesrv := serveHTML(t, `<!doctype html><html><head>
<title>Landing</title>
<meta name="citation_title" content="Main Work">
<meta name="citation_doi" content="10.1234/pagedoi">
<script type="application/ld+json">{"@type":"ScholarlyArticle","headline":"Related Work","identifier":"10.9999/related","pagination":"999-1000","abstract":"Related abstract."}</script>
</head><body>...</body></html>`)

	item, source, err := buildImportItemFromURL(context.Background(), http.DefaultClient, pagesrv.URL+"/landing")
	if err != nil {
		t.Fatalf("buildImportItemFromURL: %v", err)
	}
	if source != "CrossRef (page DOI 10.1234/pagedoi)" {
		t.Fatalf("source = %q", source)
	}
	if _, ok := item["pages"]; ok {
		t.Errorf("pages = %v, want none: the JSON-LD node is another work", item["pages"])
	}
	if _, ok := item["abstractNote"]; ok {
		t.Errorf("abstractNote = %v, want none: the JSON-LD node is another work", item["abstractNote"])
	}
}

// A JSON-LD node that states no DOI does not prove it is the record's work,
// so its fields are not backfilled even when it is the selected entity.
func TestBuildImportItemFromURL_BackfillIgnoresJSONLDWithoutDOI(t *testing.T) {
	hits := 0
	stubCrossRef(t, &hits, `{"message":{"type":"journal-article","title":["CrossRef Title"],"DOI":"10.1234/pagedoi"}}`, 0)
	pagesrv := serveHTML(t, `<!doctype html><html><head>
<title>Landing</title>
<meta name="citation_title" content="Main Work">
<meta name="citation_doi" content="10.1234/pagedoi">
<script type="application/ld+json">{"@type":"ScholarlyArticle","headline":"Unanchored Work","pagination":"999-1000","abstract":"Unanchored abstract."}</script>
</head><body>...</body></html>`)

	item, _, err := buildImportItemFromURL(context.Background(), http.DefaultClient, pagesrv.URL+"/landing")
	if err != nil {
		t.Fatalf("buildImportItemFromURL: %v", err)
	}
	if _, ok := item["pages"]; ok {
		t.Errorf("pages = %v, want none from a JSON-LD node without the record DOI", item["pages"])
	}
	if _, ok := item["abstractNote"]; ok {
		t.Errorf("abstractNote = %v, want none from a JSON-LD node without the record DOI", item["abstractNote"])
	}
}

// Backfill from the primary JSON-LD node is allowed when that node states
// the same DOI as the record.
func TestBuildImportItemFromURL_BackfillFromSameWorkJSONLD(t *testing.T) {
	hits := 0
	stubCrossRef(t, &hits, `{"message":{"type":"journal-article","title":["CrossRef Title"],"DOI":"10.1234/ld"}}`, 0)
	pagesrv := serveHTML(t, `<!doctype html><html><head>
<title>Landing</title>
<script type="application/ld+json">{"@type":"ScholarlyArticle","headline":"Main Work","identifier":"10.1234/ld","pagination":"5-9"}</script>
</head><body>...</body></html>`)

	item, _, err := buildImportItemFromURL(context.Background(), http.DefaultClient, pagesrv.URL+"/landing")
	if err != nil {
		t.Fatalf("buildImportItemFromURL: %v", err)
	}
	if item["pages"] != "5-9" {
		t.Errorf("pages = %v, want 5-9 from the same-DOI JSON-LD node", item["pages"])
	}
}

// Backfill never writes a field the record's item type does not define.
func TestBuildImportItemFromURL_BackfillRespectsItemTypeFields(t *testing.T) {
	hits := 0
	stubCrossRef(t, &hits, `{"message":{"type":"posted-content","title":["A Preprint"],"DOI":"10.1234/pre"}}`, 0)
	pagesrv := serveHTML(t, `<!doctype html><html><head>
<title>Landing</title>
<meta name="citation_title" content="A Preprint">
<meta name="citation_doi" content="10.1234/pre">
<meta name="citation_volume" content="3">
<meta name="citation_issue" content="2">
<meta name="citation_firstpage" content="10">
</head><body>...</body></html>`)

	item, _, err := buildImportItemFromURL(context.Background(), http.DefaultClient, pagesrv.URL+"/landing")
	if err != nil {
		t.Fatalf("buildImportItemFromURL: %v", err)
	}
	if item["itemType"] != "preprint" {
		t.Fatalf("itemType = %v, want preprint", item["itemType"])
	}
	for _, key := range []string{"volume", "issue", "pages"} {
		if _, ok := item[key]; ok {
			t.Errorf("%s = %v, want none: preprint does not define it", key, item[key])
		}
	}
}

// A bot wall that answers non-2xx with the interstitial is refused, not
// turned into a bare webpage item.
func TestBuildImportItemFromURL_Non2xxChallengeRefused(t *testing.T) {
	old := allowPrivateOutboundForTests.Load()
	allowPrivateOutboundForTests.Store(true)
	t.Cleanup(func() { allowPrivateOutboundForTests.Store(old) })
	for _, status := range []int{http.StatusForbidden, http.StatusServiceUnavailable} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(cloudflareChallengeHTML))
		}))
		item, _, err := buildImportItemFromURL(context.Background(), http.DefaultClient, srv.URL+"/article")
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), "bot challenge") {
			t.Errorf("status %d: err = %v item = %v, want a bot-challenge error", status, err, item)
		}
	}
	// A plain non-2xx page without challenge markers still falls back.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<html><head><title>Not Found</title></head><body>404</body></html>`))
	}))
	defer srv.Close()
	item, source, err := buildImportItemFromURL(context.Background(), http.DefaultClient, srv.URL+"/missing")
	if err != nil || source != "fallback (no metadata)" || item["itemType"] != "webpage" {
		t.Errorf("404 page: item=%v source=%q err=%v, want the webpage fallback", item, source, err)
	}
}

// A vendor name in an article title is not challenge wording.
func TestChallengeReason_VendorNameTitleIsNotAChallenge(t *testing.T) {
	body := `<!doctype html><html><head><title>Cloudflare Challenge Methods</title>
<script src="https://challenges.cloudflare.com/turnstile/v0/api.js"></script>
<meta name="citation_title" content="Cloudflare Challenge Methods"></head>
<body><article>How bot mitigation works.</article><div class="cf-turnstile"></div></body></html>`
	metas, title := parseHTMLMeta(body)
	if r := challengeReason(pageContent{metas: metas, title: title, body: body, status: 200}); r != "" {
		t.Errorf("challengeReason = %q, want none for an article about Cloudflare", r)
	}
}

// Venues go into the field each embedded item type defines.
func TestItemFromEmbeddedMeta_ContainerFieldPerType(t *testing.T) {
	cases := []struct {
		name, head, wantType, wantField string
		absent                          []string
	}{
		{"jsonld chapter", `<script type="application/ld+json">{"@type":"Chapter","name":"Ch 1","isPartOf":{"@type":"Book","name":"The Book"},"pagination":"1-20"}</script>`, "bookSection", "bookTitle", []string{"publicationTitle"}},
		{"conference", `<meta name="citation_title" content="P"><meta name="citation_conference_title" content="The Proc">`, "conferencePaper", "proceedingsTitle", []string{"publicationTitle"}},
		{"webpage site name", `<meta property="og:title" content="A Post"><meta property="og:site_name" content="The Site">`, "webpage", "websiteTitle", []string{"publicationTitle"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `<html><head><title>x</title>` + tc.head + `</head><body></body></html>`
			metas, title := parseHTMLMeta(body)
			docs := parseJSONLDBlocks(body)
			item := itemFromEmbeddedMeta(pageContent{metas: metas, title: title, body: body}, selectPrimaryJSONLD(docs, metas, ""), "https://x.example/p", "2026-06-16")
			if item["itemType"] != tc.wantType {
				t.Fatalf("itemType = %v, want %s", item["itemType"], tc.wantType)
			}
			if item[tc.wantField] == nil {
				t.Errorf("%s missing: %v", tc.wantField, item)
			}
			for _, key := range tc.absent {
				if _, ok := item[key]; ok {
					t.Errorf("%s = %v, want absent for %s", key, item[key], tc.wantType)
				}
			}
		})
	}
}
