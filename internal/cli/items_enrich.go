// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean dk33): metadata enrichment/remediation pipeline. Turns the
// read-only `items audit` work queues (missing DOI / abstract / PDF) into
// provider-backed fixes: resolve metadata from CrossRef (DOI, abstract) and
// Unpaywall (open-access PDF), build a proposed patch plan, preview it by
// default, and apply via the existing PATCH/POST paths when --yes is set.
// Enrichment provenance is appended to each item's Extra field.

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"zotero-pp-cli/internal/client"
	"zotero-pp-cli/internal/cliutil"
)

// Overridable provider base URLs (tests point them at httptest servers).
var (
	enrichCrossRefBase  = "https://api.crossref.org"
	enrichUnpaywallBase = "https://api.unpaywall.org/v2"
	enrichOpenAlexBase  = "https://api.openalex.org"
)

const maxEnrichProviderResponseBytes = 4 << 20

var jatsTagRE = regexp.MustCompile(`<[^>]+>`)

type enrichAction string

const (
	enrichActionPatch  enrichAction = "patch"
	enrichActionAttach enrichAction = "attach"
)

// enrichProposal is one proposed remediation for one item.
type enrichProposal struct {
	Key        string         `json:"key"`
	Title      string         `json:"title"`
	Category   string         `json:"category"`
	Action     enrichAction   `json:"action"`
	Source     string         `json:"source"`
	Note       string         `json:"note"`
	Fields     map[string]any `json:"fields,omitempty"`     // patch: field -> new value
	Attachment map[string]any `json:"attachment,omitempty"` // attach: child item body
	// PATCH(glean write-safety): statuses now live in mutationResult items, not proposal JSON.
	version any // item version for the PATCH conflict guard (not serialized)
}

// enrichSkip records a candidate for which no confident proposal was produced.
type enrichSkip struct {
	Key      string `json:"key"`
	Title    string `json:"title"`
	Category string `json:"category"`
	Reason   string `json:"reason"`
}

func newItemsEnrichCmd(flags *rootFlags) *cobra.Command {
	var (
		flagMissingDOI      bool
		flagMissingAbstract bool
		flagMissingPDF      bool
		flagLimit           int
		flagEmail           string
		flagNoOpenAlex      bool
		// PATCH(glean bugfix): allow enrichment work queues to be scoped by collection key.
		flagCollection string
	)

	cmd := &cobra.Command{
		Use:   "enrich",
		Short: "Fill missing item metadata (DOI, abstract, open-access PDF link) from CrossRef, OpenAlex, and Unpaywall",
		// PATCH(glean write-safety): --agent no longer implies --yes; help names --yes as the apply switch. PATCH(glean bugfix): --collection scopes enrichment queues.
		Long: `Resolve missing metadata for locally synced items and apply it back to Zotero.

Work queues come from the same checks as 'items audit':
  --missing-doi       resolve a DOI by title from CrossRef, then OpenAlex (exact title match)
  --missing-abstract  fill the abstract from CrossRef, then OpenAlex (requires the item's DOI)
  --missing-pdf       attach an open-access PDF link from Unpaywall (requires DOI)

By default this previews the proposed changes (a patch plan). Pass --collection
to scope the work queue to items in a single collection. Pass --yes to apply
them via the Zotero API; --dry-run always previews. Applied field changes
record provenance in the item's Extra field.`,
		Annotations: map[string]string{"mcp:read-only": "false"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if !flagMissingDOI && !flagMissingAbstract && !flagMissingPDF {
				return fmt.Errorf("specify at least one of --missing-doi, --missing-abstract, --missing-pdf")
			}

			rawDB, err := openStoreForRead(cmd.Context(), "zotero-pp-cli")
			if err != nil {
				return fmt.Errorf("opening database: %w", err)
			}
			if rawDB == nil {
				fmt.Fprintln(cmd.OutOrStdout(), "Run 'zotero-pp-cli sync' first.")
				return nil
			}
			defer rawDB.Close()
			db := localQueryStore{rawDB}

			httpClient := &http.Client{Timeout: enrichTimeout(flags.timeout)}
			var proposals []enrichProposal
			var skipped []enrichSkip
			useOpenAlex := !flagNoOpenAlex

			// PATCH(glean bugfix): thread collection scope through every selected enrichment category.
			if flagMissingDOI {
				p, s := buildEnrichProposals(cmd.Context(), db, httpClient, "missing_doi", flagLimit, flagCollection, flagEmail, useOpenAlex)
				proposals, skipped = append(proposals, p...), append(skipped, s...)
			}
			if flagMissingAbstract {
				p, s := buildEnrichProposals(cmd.Context(), db, httpClient, "missing_abstract", flagLimit, flagCollection, flagEmail, useOpenAlex)
				proposals, skipped = append(proposals, p...), append(skipped, s...)
			}
			if flagMissingPDF {
				p, s := buildEnrichProposals(cmd.Context(), db, httpClient, "missing_pdf", flagLimit, flagCollection, flagEmail, useOpenAlex)
				proposals, skipped = append(proposals, p...), append(skipped, s...)
			}

			// PATCH(glean write-safety): preserve proposal building and route preview/apply through the shared mutation helper.
			mode := resolveMutationMode(flags)
			var mutator apiMutator
			if mode.Apply {
				c, err := flags.newClient()
				if err != nil {
					return err
				}
				mutator = c
			}
			ops := enrichPlannedOps(proposals, mutator, flags)
			env, runErr := runMutation(cmd.Context(), flags, "items.enrich", ops)
			if len(skipped) > 0 {
				env.Journal = map[string]any{"skipped": skipped}
			}
			renderErr := renderMutation(cmd, flags, env, nil)
			if renderErr != nil {
				return renderErr
			}
			return runErr
		},
	}

	cmd.Flags().BoolVar(&flagMissingDOI, "missing-doi", false, "Resolve and add a DOI from CrossRef")
	cmd.Flags().BoolVar(&flagMissingAbstract, "missing-abstract", false, "Fill the abstract from CrossRef, then OpenAlex (uses the item's DOI)")
	cmd.Flags().BoolVar(&flagMissingPDF, "missing-pdf", false, "Attach an open-access PDF link from Unpaywall (uses the item's DOI)")
	cmd.Flags().IntVar(&flagLimit, "limit", 25, "Maximum items to process per category")
	// PATCH(glean bugfix): expose collection scoping for the local work queue.
	cmd.Flags().StringVar(&flagCollection, "collection", "", "Scope the work queue to items in a collection key")
	cmd.Flags().StringVar(&flagEmail, "email", "", "Contact email for Unpaywall (required for --missing-pdf) and the OpenAlex polite pool (optional); or set UNPAYWALL_EMAIL")
	cmd.Flags().BoolVar(&flagNoOpenAlex, "no-openalex", false, "Disable the OpenAlex fallback for --missing-doi/--missing-abstract")

	return cmd
}

func enrichTimeout(t time.Duration) time.Duration {
	if t <= 0 {
		return 30 * time.Second
	}
	return t
}

// buildEnrichProposals resolves proposals for one category over the audit work
// queue. It loads each candidate's full payload from the local store so the
// provider has title/creators/DOI/version, then dispatches to the resolver.
// PATCH(glean bugfix): carry collection scope from the command into the work queue.
func buildEnrichProposals(ctx context.Context, db localQueryStore, httpClient *http.Client, category string, limit int, collection string, email string, useOpenAlex bool) ([]enrichProposal, []enrichSkip) {
	rows, err := enrichWorkQueue(db, category, limit, collection)
	if err != nil {
		return nil, []enrichSkip{{Category: category, Reason: fmt.Sprintf("querying work queue: %v", err)}}
	}

	// PATCH(glean perf-audit eedc): each candidate triggers an independent
	// CrossRef/Unpaywall lookup, so resolve them through a bounded fan-out
	// instead of sequentially. FanoutRun preserves source order, so proposal
	// ordering is unchanged; the apply step stays sequential to avoid hammering
	// the Zotero write API.
	type outcome struct {
		prop    enrichProposal
		skip    enrichSkip
		skipped bool
	}
	results, _ := cliutil.FanoutRun(ctx, rows,
		func(row map[string]any) string { return sqlStringValue(row["key"]) },
		func(ctx context.Context, row map[string]any) (outcome, error) {
			key := sqlStringValue(row["key"])
			raw, gerr := db.Get("items", key)
			if gerr != nil || raw == nil {
				return outcome{skip: enrichSkip{Key: key, Category: category, Reason: "item not found in local store"}, skipped: true}, nil
			}
			version, data := enrichItemFields(raw)
			title := stringFromMap(data, "title")
			prop, reason := resolveEnrichment(ctx, httpClient, category, key, version, data, email, useOpenAlex)
			if reason != "" {
				return outcome{skip: enrichSkip{Key: key, Title: title, Category: category, Reason: reason}, skipped: true}, nil
			}
			return outcome{prop: prop}, nil
		})

	var proposals []enrichProposal
	var skipped []enrichSkip
	for _, r := range results {
		if r.Value.skipped {
			skipped = append(skipped, r.Value.skip)
		} else {
			proposals = append(proposals, r.Value.prop)
		}
	}
	return proposals, skipped
}

// enrichWorkQueue returns the candidate rows for a category, reusing the audit
// queries so enrichment and reporting share one definition of "missing".
// PATCH(glean bugfix): pass collection scope to category-specific missing-item queries.
func enrichWorkQueue(db localQueryStore, category string, limit int, collection string) ([]map[string]any, error) {
	switch category {
	case "missing_doi":
		return queryMissingDOIItems(db, limit, collection)
	case "missing_abstract":
		return queryMissingAbstractItems(db, limit, collection)
	case "missing_pdf":
		return queryMissingPDFItems(db, "", limit, collection)
	default:
		return nil, fmt.Errorf("unknown category %q", category)
	}
}

// resolveEnrichment dispatches to the provider for a category and returns either
// a proposal or a non-empty skip reason.
func resolveEnrichment(ctx context.Context, httpClient *http.Client, category, key string, version any, data map[string]any, email string, useOpenAlex bool) (enrichProposal, string) {
	title := stringFromMap(data, "title")
	switch category {
	case "missing_doi":
		if title == "" {
			return enrichProposal{}, "no title to search for a DOI"
		}
		doi, _, ok := resolveDOIViaCrossRef(ctx, httpClient, data)
		source := "CrossRef"
		if !ok && useOpenAlex {
			if d, ok2 := resolveDOIViaOpenAlex(ctx, httpClient, data, email); ok2 {
				doi, ok, source = d, true, "OpenAlex"
			}
		}
		if !ok {
			return enrichProposal{}, "no confident CrossRef/OpenAlex title match"
		}
		return enrichProposal{
			Key: key, Title: title, Category: category, Action: enrichActionPatch,
			Source: source, Note: "DOI " + doi,
			Fields:  map[string]any{"DOI": doi},
			version: version,
		}, ""

	case "missing_abstract":
		doi := normalizeDOI(stringFromMap(data, "DOI"))
		if doi == "" {
			return enrichProposal{}, "no DOI to look up abstract"
		}
		abstract, ok := resolveAbstractViaCrossRef(ctx, httpClient, doi)
		source := "CrossRef"
		if !ok && useOpenAlex {
			if a, ok2 := resolveAbstractViaOpenAlex(ctx, httpClient, doi, email); ok2 {
				abstract, ok, source = a, true, "OpenAlex"
			}
		}
		if !ok {
			return enrichProposal{}, "no abstract on CrossRef or OpenAlex for this DOI"
		}
		return enrichProposal{
			Key: key, Title: title, Category: category, Action: enrichActionPatch,
			Source: source, Note: fmt.Sprintf("abstract (%d chars)", len(abstract)),
			Fields:  map[string]any{"abstractNote": abstract},
			version: version,
		}, ""

	case "missing_pdf":
		doi := normalizeDOI(stringFromMap(data, "DOI"))
		if doi == "" {
			return enrichProposal{}, "no DOI to look up open-access PDF"
		}
		if email == "" {
			email = enrichUnpaywallEmail()
		}
		if email == "" {
			return enrichProposal{}, "Unpaywall requires a contact email (--email or UNPAYWALL_EMAIL)"
		}
		pdfURL, ok := resolvePDFViaUnpaywall(ctx, httpClient, doi, email)
		if !ok {
			return enrichProposal{}, "no open-access PDF found on Unpaywall"
		}
		return enrichProposal{
			Key: key, Title: title, Category: category, Action: enrichActionAttach,
			Source: "Unpaywall", Note: pdfURL,
			Attachment: map[string]any{
				"itemType":   "attachment",
				"linkMode":   "linked_url",
				"title":      "Open-access PDF (Unpaywall)",
				"url":        pdfURL,
				"parentItem": key,
			},
		}, ""
	}
	return enrichProposal{}, "unknown category"
}

// PATCH(glean write-safety): return typed mutation statuses; details travel as result reasons.
func applyEnrichProposal(c apiMutator, p *enrichProposal, flags *rootFlags) (string, any, error) {
	if c == nil {
		err := errors.New("missing API client")
		return "failed", err.Error(), err
	}
	switch p.Action {
	case enrichActionPatch:
		body := map[string]any{"version": p.version}
		for k, v := range p.Fields {
			body[k] = v
		}
		body["extra"] = appendEnrichProvenance(p, flags)
		path := replacePathParam("/items/{itemKey}", "itemKey", p.Key)
		if _, _, err := c.Patch(path, body); err != nil {
			return enrichErrorStatus(err)
		}
		return "applied", nil, nil
	case enrichActionAttach:
		if _, _, err := c.Post("/items", []map[string]any{p.Attachment}); err != nil {
			return enrichErrorStatus(err)
		}
		return "applied", nil, nil
	}
	return "no_op", "unknown enrichment action", nil
}

// apiMutator is the subset of *client.Client used to apply enrichments; a small
// interface keeps the apply step unit-testable without a live server.
type apiMutator interface {
	Patch(path string, body any) (json.RawMessage, int, error)
	Post(path string, body any) (json.RawMessage, int, error)
}

// PATCH(glean write-safety): convert enrichment proposals into shared mutation operations.
func enrichPlannedOps(proposals []enrichProposal, c apiMutator, flags *rootFlags) []plannedOp {
	ops := make([]plannedOp, 0, len(proposals))
	for i := range proposals {
		proposal := proposals[i]
		ops = append(ops, plannedOp{
			ID:              proposal.Category + ":" + proposal.Key,
			Key:             proposal.Key,
			Kind:            proposal.Category,
			ExpectedVersion: mutationExpectedVersion(proposal.version),
			Changes:         enrichProposalChanges(proposal),
			apply: func() (string, any, error) {
				return applyEnrichProposal(c, &proposal, flags)
			},
		})
	}
	return ops
}

func enrichProposalChanges(p enrichProposal) []mutationChange {
	switch p.Action {
	case enrichActionPatch:
		keys := make([]string, 0, len(p.Fields))
		for key := range p.Fields {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		changes := make([]mutationChange, 0, len(keys))
		for _, key := range keys {
			changes = append(changes, mutationChange{Field: key, Add: p.Fields[key]})
		}
		return changes
	case enrichActionAttach:
		if url, ok := p.Attachment["url"]; ok {
			return []mutationChange{{Field: "url", Add: url}}
		}
		return []mutationChange{{Field: "attachment", Add: p.Attachment}}
	default:
		return nil
	}
}

func enrichErrorStatus(err error) (string, any, error) {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusPreconditionFailed || apiErr.StatusCode == http.StatusPreconditionRequired) {
		return "conflict", apiErr.Body, err
	}
	return "failed", err.Error(), err
}

func mutationExpectedVersion(version any) int {
	switch v := version.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	default:
		return 0
	}
}

// appendEnrichProvenance returns the new Extra value: the item's existing Extra
// with a provenance line appended (so re-runs leave an audit trail).
func appendEnrichProvenance(p *enrichProposal, flags *rootFlags) string {
	field := "DOI"
	if p.Category == "missing_abstract" {
		field = "abstract"
	}
	line := fmt.Sprintf("zotero-pp-cli: %s added via %s on %s", field, p.Source, time.Now().UTC().Format("2006-01-02"))
	existing := strings.TrimRight(currentExtra(p), "\n")
	if existing == "" {
		return line
	}
	return existing + "\n" + line
}

// currentExtra is set on the proposal's fields when known; enrichment patches
// never include the original Extra, so this is a placeholder hook that returns
// any Extra carried on the proposal (empty for field patches today).
func currentExtra(p *enrichProposal) string {
	if v, ok := p.Fields["extra"].(string); ok {
		return v
	}
	return ""
}

// --- providers ---

type crossRefSearchResponse struct {
	Message struct {
		Items []crossRefWork `json:"items"`
	} `json:"message"`
}

// resolveDOIViaCrossRef searches CrossRef by bibliographic query and returns the
// DOI of the candidate whose title matches the item's title exactly (after
// normalization), guarding against attaching a wrong DOI.
func resolveDOIViaCrossRef(ctx context.Context, httpClient *http.Client, data map[string]any) (string, crossRefWork, bool) {
	title := stringFromMap(data, "title")
	if title == "" {
		return "", crossRefWork{}, false
	}
	q := title
	if author := firstCreatorFamily(data); author != "" {
		q += " " + author
	}
	u := enrichCrossRefBase + "/works?" + url.Values{
		"query.bibliographic": {q},
		"rows":                {"5"},
	}.Encode()

	var resp crossRefSearchResponse
	if err := getJSON(ctx, httpClient, u, &resp); err != nil {
		return "", crossRefWork{}, false
	}
	want := normalizeTitleForMatch(title)
	for _, w := range resp.Message.Items {
		if len(w.Title) == 0 || w.DOI == "" {
			continue
		}
		if normalizeTitleForMatch(w.Title[0]) == want {
			return normalizeDOI(w.DOI), w, true
		}
	}
	return "", crossRefWork{}, false
}

// resolveAbstractViaCrossRef fetches a work by DOI and returns its abstract with
// JATS XML markup stripped.
func resolveAbstractViaCrossRef(ctx context.Context, httpClient *http.Client, doi string) (string, bool) {
	u := enrichCrossRefBase + "/works/" + url.PathEscape(doi)
	var resp crossRefWorkResponse
	if err := getJSON(ctx, httpClient, u, &resp); err != nil {
		return "", false
	}
	abstract := stripJATS(resp.Message.Abstract)
	if abstract == "" {
		return "", false
	}
	return abstract, true
}

type unpaywallResponse struct {
	BestOA struct {
		URLForPDF string `json:"url_for_pdf"`
		URL       string `json:"url"`
	} `json:"best_oa_location"`
}

// resolvePDFViaUnpaywall returns the best open-access PDF URL for a DOI.
func resolvePDFViaUnpaywall(ctx context.Context, httpClient *http.Client, doi, email string) (string, bool) {
	u := enrichUnpaywallBase + "/" + url.PathEscape(doi) + "?" + url.Values{"email": {email}}.Encode()
	var resp unpaywallResponse
	if err := getJSON(ctx, httpClient, u, &resp); err != nil {
		return "", false
	}
	if resp.BestOA.URLForPDF != "" {
		// PATCH(glean zotero-pp-cli-ecf77ae9074377de): Unpaywall data becomes a
		// Zotero linked-url attachment, so only HTTPS public URLs are accepted.
		if err := validateExternalHTTPURL(resp.BestOA.URLForPDF, true); err == nil {
			return resp.BestOA.URLForPDF, true
		}
	}
	if resp.BestOA.URL != "" {
		if err := validateExternalHTTPURL(resp.BestOA.URL, true); err == nil {
			return resp.BestOA.URL, true
		}
	}
	return "", false
}

// --- OpenAlex (fallback for DOI + abstract; OA PDFs stay on Unpaywall, whose
// data OpenAlex merely re-serves). PATCH(glean mmmd). ---

type openAlexWork struct {
	DOI                   string           `json:"doi"`
	Title                 string           `json:"title"`
	AbstractInvertedIndex map[string][]int `json:"abstract_inverted_index"`
}

type openAlexSearchResponse struct {
	Results []openAlexWork `json:"results"`
}

func openAlexWorksURL(filter, email string) string {
	v := url.Values{"filter": {filter}, "per_page": {"5"}}
	if email != "" {
		v.Set("mailto", email) // OpenAlex "polite pool"
	}
	return enrichOpenAlexBase + "/works?" + v.Encode()
}

func openAlexFilterLiteral(value string) string {
	// PATCH(glean zotero-pp-cli-0324fe29c56a35fe): OpenAlex decodes the query
	// string before parsing comma-separated filters; preserve commas as literal
	// value text instead of allowing a second filter predicate to be injected.
	return strings.ReplaceAll(value, ",", "%2C")
}

// resolveDOIViaOpenAlex searches OpenAlex by title and returns the DOI of the
// candidate whose title matches exactly (same guard as the CrossRef resolver).
func resolveDOIViaOpenAlex(ctx context.Context, httpClient *http.Client, data map[string]any, email string) (string, bool) {
	title := stringFromMap(data, "title")
	if title == "" {
		return "", false
	}
	var resp openAlexSearchResponse
	if err := getJSON(ctx, httpClient, openAlexWorksURL("title.search:"+openAlexFilterLiteral(title), email), &resp); err != nil {
		return "", false
	}
	want := normalizeTitleForMatch(title)
	for _, w := range resp.Results {
		if w.DOI == "" || w.Title == "" {
			continue
		}
		if normalizeTitleForMatch(w.Title) == want {
			return normalizeDOI(w.DOI), true
		}
	}
	return "", false
}

// resolveAbstractViaOpenAlex fetches a work by DOI and reconstructs its abstract
// from OpenAlex's inverted index.
func resolveAbstractViaOpenAlex(ctx context.Context, httpClient *http.Client, doi, email string) (string, bool) {
	var resp openAlexSearchResponse
	if err := getJSON(ctx, httpClient, openAlexWorksURL("doi:"+openAlexFilterLiteral(strings.ToLower(doi)), email), &resp); err != nil {
		return "", false
	}
	if len(resp.Results) == 0 {
		return "", false
	}
	abstract := reconstructAbstract(resp.Results[0].AbstractInvertedIndex)
	if abstract == "" {
		return "", false
	}
	return abstract, true
}

// reconstructAbstract turns OpenAlex's {word: [positions]} inverted index back
// into running text.
func reconstructAbstract(idx map[string][]int) string {
	maxPos := -1
	for _, positions := range idx {
		for _, p := range positions {
			if p > maxPos {
				maxPos = p
			}
		}
	}
	if maxPos < 0 {
		return ""
	}
	words := make([]string, maxPos+1)
	for word, positions := range idx {
		for _, p := range positions {
			if p >= 0 && p <= maxPos {
				words[p] = word
			}
		}
	}
	var b strings.Builder
	for _, w := range words {
		if w == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(w)
	}
	return strings.TrimSpace(b.String())
}

// getJSON performs a GET and decodes a JSON body, treating non-2xx as an error.
func getJSON(ctx context.Context, httpClient *http.Client, rawURL string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "zotero-pp-cli/1.0.0")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	// PATCH(glean zotero-pp-cli-856054bc5801ad5a): cap provider JSON bodies
	// before decoding so a hostile CrossRef/Unpaywall/OpenAlex-compatible server
	// cannot stream unbounded data into the enrichment process.
	limited := &io.LimitedReader{R: resp.Body, N: maxEnrichProviderResponseBytes + 1}
	if err := json.NewDecoder(limited).Decode(out); err != nil {
		return err
	}
	if limited.N <= 0 {
		return fmt.Errorf("provider response exceeded %d bytes", maxEnrichProviderResponseBytes)
	}
	return nil
}

// --- small helpers ---

func enrichItemFields(raw json.RawMessage) (version any, data map[string]any) {
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		return nil, map[string]any{}
	}
	version = obj["version"]
	if inner, ok := obj["data"].(map[string]any); ok {
		return version, inner
	}
	return version, obj
}

func stringFromMap(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func firstCreatorFamily(data map[string]any) string {
	creators, ok := data["creators"].([]any)
	if !ok {
		return ""
	}
	for _, c := range creators {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if last := stringFromMap(cm, "lastName"); last != "" {
			return last
		}
		if name := stringFromMap(cm, "name"); name != "" {
			return name
		}
	}
	return ""
}

func normalizeTitleForMatch(title string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(title) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		}
	}
	return b.String()
}

func stripJATS(s string) string {
	if s == "" {
		return ""
	}
	cleaned := jatsTagRE.ReplaceAllString(s, " ")
	cleaned = html.UnescapeString(cleaned)
	return strings.TrimSpace(strings.Join(strings.Fields(cleaned), " "))
}

func enrichUnpaywallEmail() string {
	return strings.TrimSpace(os.Getenv("UNPAYWALL_EMAIL"))
}
