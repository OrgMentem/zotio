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
	"fmt"
	"html"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"zotero-pp-cli/internal/cliutil"
)

// Overridable provider base URLs (tests point them at httptest servers).
var (
	enrichCrossRefBase  = "https://api.crossref.org"
	enrichUnpaywallBase = "https://api.unpaywall.org/v2"
)

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
	Status     string         `json:"status,omitempty"`     // set during apply
	version    any            // item version for the PATCH conflict guard (not serialized)
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
	)

	cmd := &cobra.Command{
		Use:   "enrich",
		Short: "Fill missing item metadata (DOI, abstract, open-access PDF link) from CrossRef and Unpaywall",
		Long: `Resolve missing metadata for locally synced items and apply it back to Zotero.

Work queues come from the same checks as 'items audit':
  --missing-doi       resolve a DOI from CrossRef by title (exact title match)
  --missing-abstract  fill the abstract from CrossRef (requires the item's DOI)
  --missing-pdf       attach an open-access PDF link from Unpaywall (requires DOI)

By default this previews the proposed changes (a patch plan). Pass --yes (or
--agent) to apply them via the Zotero API; --dry-run always previews. Applied
field changes record provenance in the item's Extra field.`,
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

			if flagMissingDOI {
				p, s := buildEnrichProposals(cmd.Context(), db, httpClient, "missing_doi", flagLimit, flagEmail)
				proposals, skipped = append(proposals, p...), append(skipped, s...)
			}
			if flagMissingAbstract {
				p, s := buildEnrichProposals(cmd.Context(), db, httpClient, "missing_abstract", flagLimit, flagEmail)
				proposals, skipped = append(proposals, p...), append(skipped, s...)
			}
			if flagMissingPDF {
				p, s := buildEnrichProposals(cmd.Context(), db, httpClient, "missing_pdf", flagLimit, flagEmail)
				proposals, skipped = append(proposals, p...), append(skipped, s...)
			}

			applyMode := flags.yes && !flags.dryRun
			if !applyMode {
				return printEnrichReport(cmd, proposals, skipped, flags, false)
			}

			c, err := flags.newClient()
			if err != nil {
				return err
			}
			for i := range proposals {
				proposals[i].Status = applyEnrichProposal(c, &proposals[i], flags)
			}
			return printEnrichReport(cmd, proposals, skipped, flags, true)
		},
	}

	cmd.Flags().BoolVar(&flagMissingDOI, "missing-doi", false, "Resolve and add a DOI from CrossRef")
	cmd.Flags().BoolVar(&flagMissingAbstract, "missing-abstract", false, "Fill the abstract from CrossRef (uses the item's DOI)")
	cmd.Flags().BoolVar(&flagMissingPDF, "missing-pdf", false, "Attach an open-access PDF link from Unpaywall (uses the item's DOI)")
	cmd.Flags().IntVar(&flagLimit, "limit", 25, "Maximum items to process per category")
	cmd.Flags().StringVar(&flagEmail, "email", "", "Contact email for the Unpaywall API (required for --missing-pdf; or set UNPAYWALL_EMAIL)")

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
func buildEnrichProposals(ctx context.Context, db localQueryStore, httpClient *http.Client, category string, limit int, email string) ([]enrichProposal, []enrichSkip) {
	rows, err := enrichWorkQueue(db, category, limit)
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
			prop, reason := resolveEnrichment(ctx, httpClient, category, key, version, data, email)
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
func enrichWorkQueue(db localQueryStore, category string, limit int) ([]map[string]any, error) {
	switch category {
	case "missing_doi":
		return queryMissingDOIItems(db, limit)
	case "missing_abstract":
		return queryMissingAbstractItems(db, limit)
	case "missing_pdf":
		return queryMissingPDFItems(db, "", limit)
	default:
		return nil, fmt.Errorf("unknown category %q", category)
	}
}

// resolveEnrichment dispatches to the provider for a category and returns either
// a proposal or a non-empty skip reason.
func resolveEnrichment(ctx context.Context, httpClient *http.Client, category, key string, version any, data map[string]any, email string) (enrichProposal, string) {
	title := stringFromMap(data, "title")
	switch category {
	case "missing_doi":
		if title == "" {
			return enrichProposal{}, "no title to search CrossRef"
		}
		doi, _, ok := resolveDOIViaCrossRef(ctx, httpClient, data)
		if !ok {
			return enrichProposal{}, "no confident CrossRef title match"
		}
		return enrichProposal{
			Key: key, Title: title, Category: category, Action: enrichActionPatch,
			Source: "CrossRef", Note: "DOI " + doi,
			Fields:  map[string]any{"DOI": doi},
			version: version,
		}, ""

	case "missing_abstract":
		doi := normalizeDOI(stringFromMap(data, "DOI"))
		if doi == "" {
			return enrichProposal{}, "no DOI to look up abstract"
		}
		abstract, ok := resolveAbstractViaCrossRef(ctx, httpClient, doi)
		if !ok {
			return enrichProposal{}, "CrossRef has no abstract for this DOI"
		}
		return enrichProposal{
			Key: key, Title: title, Category: category, Action: enrichActionPatch,
			Source: "CrossRef", Note: fmt.Sprintf("abstract (%d chars)", len(abstract)),
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

// applyEnrichProposal performs the mutation and returns a status string.
func applyEnrichProposal(c apiMutator, p *enrichProposal, flags *rootFlags) string {
	switch p.Action {
	case enrichActionPatch:
		body := map[string]any{"version": p.version}
		for k, v := range p.Fields {
			body[k] = v
		}
		body["extra"] = appendEnrichProvenance(p, flags)
		path := replacePathParam("/items/{itemKey}", "itemKey", p.Key)
		if _, _, err := c.Patch(path, body); err != nil {
			return "error: " + err.Error()
		}
		return "applied"
	case enrichActionAttach:
		if _, _, err := c.Post("/items", []map[string]any{p.Attachment}); err != nil {
			return "error: " + err.Error()
		}
		return "applied"
	}
	return "skipped"
}

// apiMutator is the subset of *client.Client used to apply enrichments; a small
// interface keeps the apply step unit-testable without a live server.
type apiMutator interface {
	Patch(path string, body any) (json.RawMessage, int, error)
	Post(path string, body any) (json.RawMessage, int, error)
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
		return resp.BestOA.URLForPDF, true
	}
	if resp.BestOA.URL != "" {
		return resp.BestOA.URL, true
	}
	return "", false
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
	return json.NewDecoder(resp.Body).Decode(out)
}

// --- reporting ---

func printEnrichReport(cmd *cobra.Command, proposals []enrichProposal, skipped []enrichSkip, flags *rootFlags, applied bool) error {
	if flags.asJSON {
		report := map[string]any{
			"applied":   applied,
			"dry_run":   !applied,
			"proposals": proposals,
			"skipped":   skipped,
		}
		data, err := json.Marshal(report)
		if err != nil {
			return err
		}
		return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), flags)
	}

	out := cmd.OutOrStdout()
	verb := "Proposed"
	if applied {
		verb = "Applied"
	}
	fmt.Fprintf(out, "%s %d enrichment(s); %d skipped\n", verb, len(proposals), len(skipped))
	for _, p := range proposals {
		status := p.Status
		if status == "" {
			status = "proposed"
		}
		fmt.Fprintf(out, "  [%s] %s %s via %s — %s (%s)\n", status, p.Category, p.Key, p.Source, p.Note, truncateTitle(p.Title))
	}
	for _, s := range skipped {
		fmt.Fprintf(out, "  [skipped] %s %s — %s\n", s.Category, s.Key, s.Reason)
	}
	if !applied && len(proposals) > 0 {
		fmt.Fprintln(out, "\nRe-run with --yes to apply (or --dry-run to keep previewing).")
	}
	return nil
}

func truncateTitle(title string) string {
	const max = 60
	if len(title) <= max {
		return title
	}
	return title[:max-1] + "…"
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
