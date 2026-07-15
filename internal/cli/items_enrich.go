// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Metadata enrichment/remediation pipeline. Turns the
// read-only `items audit` work queues (missing DOI / abstract / PDF) into
// provider-backed fixes: resolve metadata from CrossRef (DOI, abstract) and
// Unpaywall (open-access PDF attachments), build a proposed patch plan, preview it by
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
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"zotio/internal/client"
	"zotio/internal/cliutil"
	"zotio/internal/mutation"

	"github.com/spf13/cobra"
)

// Overridable provider base URLs (tests point them at httptest servers).
var (
	enrichCrossRefBase  = "https://api.crossref.org"
	enrichUnpaywallBase = "https://api.unpaywall.org/v2"
	enrichOpenAlexBase  = "https://api.openalex.org"
	// Provider seams for Semantic Scholar DOI/abstract fallbacks and OpenCitations DOI validation.
	enrichSemanticScholarBase = "https://api.semanticscholar.org/graph/v1"
	enrichOpenCitationsBase   = "https://opencitations.net/index/api/v1"
)

const maxEnrichProviderResponseBytes = 4 << 20
const maxEnrichPDFBytes int64 = 100 << 20

var enrichPDFURLSafetyCheck = rejectLocalPDFURL
var enrichPDFDownloaderFactory = newEnrichPDFDownloader

var jatsTagRE = regexp.MustCompile(`<[^>]+>`)

type enrichAction string

const (
	enrichActionPatch  enrichAction = "patch"
	enrichActionAttach enrichAction = "attach"
)

// enrichProposal is one proposed remediation for one item.
type enrichProposal struct {
	Key         string         `json:"key"`
	Title       string         `json:"title"`
	Category    string         `json:"category"`
	Action      enrichAction   `json:"action"`
	Source      string         `json:"source"`
	Note        string         `json:"note"`
	Fields      map[string]any `json:"fields,omitempty"`      // fields: field -> new value
	Attachment  map[string]any `json:"attachment,omitempty"`  // linked_url attach: child item body
	AttachMode  string         `json:"attach_mode,omitempty"` // missing-pdf: linked-url or linked-file
	DownloadURL string         `json:"download_url,omitempty"`
	PDFPath     string         `json:"pdf_path,omitempty"`
	// Statuses now live in mutation.Result items, not proposal JSON.
	version any    // item version for the PATCH conflict guard (not serialized)
	extra   string // item's current Extra so provenance appends instead of replacing (not serialized)
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
		flagMissingDOI        bool
		flagMissingAbstract   bool
		flagMissingPDF        bool
		flagLimit             int
		flagEmail             string
		flagNoOpenAlex        bool
		flagNoSemanticScholar bool
		flagValidate          bool
		flagCollection        string
		flagAttachMode        string
		flagPDFDir            string
		// Exact remediation from `library health`
		// feeds item keys to enrichment so it previews/applies only the findings
		// it recommended, instead of broadening back out to the whole queue.
		keysFrom string
		keys     string
	)

	cmd := &cobra.Command{
		Use:   "enrich",
		Short: "Fill or validate item metadata (DOI, abstract, open-access PDF attachment) from CrossRef, OpenAlex, Semantic Scholar, Unpaywall, and OpenCitations",
		// --agent no longer implies --yes; help names --yes as the apply switch. --collection scopes enrichment queues.
		Long: `Resolve missing metadata for locally synced items and apply it back to Zotero.

Work queues come from the same checks as 'items audit':
  --missing-doi       resolve a DOI by title from CrossRef, then OpenAlex/Semantic Scholar (exact title match)
  --missing-abstract  fill the abstract from CrossRef, then OpenAlex/Semantic Scholar (requires the item's DOI)
  --missing-pdf       attach an open-access PDF from Unpaywall (requires DOI)

PDF attachment modes:
  linked-url   create a linked_url attachment (default; no download)
  linked-file  download the PDF to --pdf-dir and create a linked_file child item; linked files do not sync to other devices

Downloaded PDFs accept application/pdf, application/octet-stream, or an absent
Content-Type, then must pass a %PDF- magic check. Stored (imported-file)
retro-attachment is handled by 'zotio attachments add <item-key> <file>', which
uploads through the Zotero Web API file-upload protocol.

By default this previews the proposed changes (a patch plan) and never downloads
PDF bytes. Pass --collection to scope the work queue to items in a single
collection, or --keys/--keys-from to scope to exact item keys. Pass --yes to
apply via the Zotero API; --dry-run always previews.
Applied field changes record provenance in the item's Extra field.`,
		Annotations: map[string]string{"mcp:read-only": "false"},
		RunE: func(cmd *cobra.Command, args []string) error {
			attachMode := strings.TrimSpace(flagAttachMode)
			switch attachMode {
			case "linked-url", "linked-file":
			case "stored":
				return usageErr(fmt.Errorf("--attach-mode stored is not supported by items enrich; retro-attach a stored file with `zotio attachments add <item-key> <file>` (or use linked-url/linked-file here)"))
			default:
				return usageErr(fmt.Errorf("--attach-mode must be one of linked-url, linked-file"))
			}
			if cmd.Flags().Changed("attach-mode") && !flagMissingPDF {
				return usageErr(fmt.Errorf("--attach-mode is only valid with --missing-pdf"))
			}
			if cmd.Flags().Changed("pdf-dir") && !flagMissingPDF {
				return usageErr(fmt.Errorf("--pdf-dir is only valid with --missing-pdf"))
			}
			if flagMissingPDF && attachMode == "linked-file" && strings.TrimSpace(flagPDFDir) == "" {
				return usageErr(fmt.Errorf("--pdf-dir is required with --missing-pdf --attach-mode linked-file"))
			}
			if !flagValidate && !flagMissingDOI && !flagMissingAbstract && !flagMissingPDF {
				return fmt.Errorf("specify at least one of --missing-doi, --missing-abstract, --missing-pdf, or --validate")
			}

			pdfDir := ""
			if flagMissingPDF && attachMode == "linked-file" {
				abs, err := filepath.Abs(flagPDFDir)
				if err != nil {
					return usageErr(fmt.Errorf("resolving --pdf-dir: %w", err))
				}
				pdfDir = abs
			}

			rawDB, err := openStoreForRead(cmd.Context(), "zotio")
			if err != nil {
				return fmt.Errorf("opening database: %w", err)
			}
			if rawDB == nil {
				fmt.Fprintln(cmd.OutOrStdout(), "Run 'zotio sync' first.")
				return nil
			}
			defer rawDB.Close()
			db := localQueryStore{rawDB}

			if strings.TrimSpace(keys) != "" && strings.TrimSpace(keysFrom) != "" {
				return usageErr(fmt.Errorf("--keys cannot be combined with --keys-from"))
			}
			keyFilter, err := enrichKeyFilter(keys, keysFrom, cmd.InOrStdin())
			if err != nil {
				return err
			}

			httpClient := &http.Client{Timeout: enrichTimeout(flags.timeout)}
			useOpenAlex := !flagNoOpenAlex
			useSemanticScholar := !flagNoSemanticScholar
			if flagValidate {
				report, err := validateEnrichItems(cmd.Context(), db, httpClient, flagLimit, flagCollection, keyFilter)
				if err != nil {
					return err
				}
				return renderEnrichValidationReport(cmd, flags, report)
			}

			var proposals []enrichProposal
			var skipped []enrichSkip

			// Thread collection scope through every selected enrichment category.
			if flagMissingDOI {
				p, s := buildEnrichProposals(cmd.Context(), db, httpClient, "missing_doi", flagLimit, flagCollection, keyFilter, flagEmail, useOpenAlex, useSemanticScholar, "linked-url", "")
				proposals, skipped = append(proposals, p...), append(skipped, s...)
			}
			if flagMissingAbstract {
				p, s := buildEnrichProposals(cmd.Context(), db, httpClient, "missing_abstract", flagLimit, flagCollection, keyFilter, flagEmail, useOpenAlex, useSemanticScholar, "linked-url", "")
				proposals, skipped = append(proposals, p...), append(skipped, s...)
			}
			if flagMissingPDF {
				p, s := buildEnrichProposals(cmd.Context(), db, httpClient, "missing_pdf", flagLimit, flagCollection, keyFilter, flagEmail, useOpenAlex, useSemanticScholar, attachMode, pdfDir)
				proposals, skipped = append(proposals, p...), append(skipped, s...)
			}

			// Preserve proposal building and route preview/apply through the shared mutation helper.
			mode := resolveMutationMode(flags)
			var mutator apiMutator
			if mode.Apply && enrichNeedsAPIMutator(proposals) {
				c, err := flags.newClient()
				if err != nil {
					return err
				}
				mutator = c
			}
			ops := enrichPlannedOps(cmd.Context(), proposals, mutator, flags, httpClient)
			restoreContinue := flags.continueOnError
			if flagMissingPDF && attachMode != "linked-url" {
				flags.continueOnError = true
				defer func() { flags.continueOnError = restoreContinue }()
			}
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

	cmd.Flags().BoolVar(&flagMissingDOI, "missing-doi", false, "Resolve and add a DOI from CrossRef, OpenAlex, or Semantic Scholar")
	cmd.Flags().BoolVar(&flagMissingAbstract, "missing-abstract", false, "Fill the abstract from CrossRef, OpenAlex, or Semantic Scholar (uses the item's DOI)")
	cmd.Flags().BoolVar(&flagMissingPDF, "missing-pdf", false, "Attach an open-access PDF from Unpaywall as a link or download (uses the item's DOI)")
	cmd.Flags().StringVar(&flagAttachMode, "attach-mode", "linked-url", "PDF attachment handling: linked-url or linked-file; stored retro-attachment is handled by `zotio attachments add`")
	cmd.Flags().StringVar(&flagPDFDir, "pdf-dir", "", "Directory for linked-file PDF downloads; responses must be PDF/octet-stream/unspecified Content-Type plus %PDF- magic")
	cmd.Flags().IntVar(&flagLimit, "limit", 25, "Maximum items to process per category")
	// Expose collection scoping for the local work queue.
	cmd.Flags().StringVar(&flagCollection, "collection", "", "Scope the work queue to items in a collection key")
	cmd.Flags().StringVar(&keys, "keys", "", "Comma-separated exact item keys to enrich")
	cmd.Flags().StringVar(&keysFrom, "keys-from", "", "Read exact item keys from a file or '-' for stdin, then enrich only matching queued items")
	cmd.Flags().StringVar(&flagEmail, "email", "", "Contact email for Unpaywall (required for --missing-pdf) and the OpenAlex polite pool (optional); or set UNPAYWALL_EMAIL")
	cmd.Flags().BoolVar(&flagNoOpenAlex, "no-openalex", false, "Disable the OpenAlex fallback for --missing-doi/--missing-abstract")
	cmd.Flags().BoolVar(&flagNoSemanticScholar, "no-semantic-scholar", false, "Disable the Semantic Scholar fallback for --missing-doi/--missing-abstract")
	cmd.Flags().BoolVar(&flagValidate, "validate", false, "Read-only DOI discrepancy report against CrossRef and OpenCitations")

	return cmd
}

func enrichTimeout(t time.Duration) time.Duration {
	if t <= 0 {
		return 30 * time.Second
	}
	return t
}

// enrichKeyFilter parses direct or file-provided keys into an allow-set for
// exact remediation without widening back to the whole missing-* work queue.
func enrichKeyFilter(keys, keysFrom string, stdin io.Reader) (map[string]bool, error) {
	var (
		selected []string
		err      error
	)
	if strings.TrimSpace(keys) != "" {
		selected, err = parseMissingPDFKeys(keys)
	} else if keysFrom != "" {
		selected, err = resolveKeys(nil, keysFrom, stdin)
	}
	if err != nil {
		return nil, err
	}
	if len(selected) == 0 {
		return nil, nil
	}
	allow := make(map[string]bool, len(selected))
	for _, key := range selected {
		allow[key] = true
	}
	return allow, nil
}

func filterEnrichRowsByKeys(rows []map[string]any, allow map[string]bool) []map[string]any {
	if allow == nil {
		return rows
	}
	filtered := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if allow[sqlStringValue(row["key"])] {
			filtered = append(filtered, row)
		}
	}
	return filtered
}

// buildEnrichProposals resolves proposals for one category over the audit work
// queue. It loads each candidate's full payload from the local store so the
// provider has title/creators/DOI/version, then dispatches to the resolver.
// Carry collection and exact key scopes into the work queue, filtering before
// provider lookups so remediation stays bounded and cheap.
func buildEnrichProposals(ctx context.Context, db localQueryStore, httpClient *http.Client, category string, limit int, collection string, keyFilter map[string]bool, email string, useOpenAlex bool, useSemanticScholar bool, attachMode string, pdfDir string) ([]enrichProposal, []enrichSkip) {
	queryLimit := limit
	if keyFilter != nil {
		// Exact keys identify remediation targets. Fetch the complete queue
		// before filtering so the SQL limit cannot hide an older requested key,
		// then retain the requested limit below.
		queryLimit = 0
	}
	rows, err := enrichWorkQueue(db, category, queryLimit, collection)
	if err != nil {
		return nil, []enrichSkip{{Category: category, Reason: fmt.Sprintf("querying work queue: %v", err)}}
	}
	rows = filterEnrichRowsByKeys(rows, keyFilter)
	if keyFilter != nil && limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}

	// Each candidate triggers an independent
	// CrossRef/Unpaywall lookup, so resolve them through a bounded fan-out
	// instead of sequentially. FanoutRun preserves source order, so proposal
	// ordering is unchanged; the apply step stays sequential to avoid hammering
	// the Zotero write API.
	type outcome struct {
		prop    enrichProposal
		skip    enrichSkip
		skipped bool
	}
	results, fanoutErrs := cliutil.FanoutRun(ctx, rows,
		func(row map[string]any) string { return sqlStringValue(row["key"]) },
		func(ctx context.Context, row map[string]any) (outcome, error) {
			key := sqlStringValue(row["key"])
			raw, gerr := db.Get("items", key)
			if gerr != nil || raw == nil {
				return outcome{skip: enrichSkip{Key: key, Category: category, Reason: "item not found in local store"}, skipped: true}, nil
			}
			version, data := enrichItemFields(raw)
			title := stringFromMap(data, "title")
			prop, reason := resolveEnrichment(ctx, httpClient, category, key, version, data, email, useOpenAlex, useSemanticScholar, attachMode, pdfDir)
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
	for _, fanoutErr := range fanoutErrs {
		skipped = append(skipped, enrichSkip{
			Key:      fanoutErr.Source,
			Category: category,
			Reason:   fmt.Sprintf("resolving enrichment: %v", fanoutErr.Err),
		})
	}
	return proposals, skipped
}

// enrichWorkQueue returns the candidate rows for a category, reusing the audit
// queries so enrichment and reporting share one definition of "missing".
// Pass collection scope to category-specific missing-item queries.
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

type enrichValidationReport struct {
	Validated      int       `json:"validated"`
	Findings       []Finding `json:"findings"`
	UnverifiedDOIs []string  `json:"unverified_dois"`
}

// Validation mode uses the local enrichment selection path but narrows it to DOI-bearing items.
func queryEnrichValidationItems(db localQueryStore, limit int, collection string) ([]map[string]any, error) {
	query := `
SELECT
	id AS key,
	json_extract(data, '$.data.title') AS title,
	json_extract(data, '$.data.itemType') AS item_type,
	json_extract(data, '$.data.DOI') AS doi,
	json_extract(data, '$.data.dateAdded') AS date_added
FROM resources
WHERE resource_type = 'items'
	AND json_extract(data, '$.data.DOI') IS NOT NULL
	AND TRIM(json_extract(data, '$.data.DOI')) != ''`
	args := enrichCollectionFilterArgs(&query, "data", collection)
	query += `
ORDER BY date_added DESC`
	return queryItemsAuditRows(db, query, limit, args...)
}

// Read-only discrepancy pass for stored DOI metadata without building mutation operations.
func validateEnrichItems(ctx context.Context, db localQueryStore, httpClient *http.Client, limit int, collection string, keyFilter map[string]bool) (enrichValidationReport, error) {
	report := enrichValidationReport{
		Findings:       []Finding{},
		UnverifiedDOIs: []string{},
	}
	rows, err := queryEnrichValidationItems(db, limit, collection)
	if err != nil {
		return report, fmt.Errorf("querying validation work queue: %w", err)
	}
	rows = filterEnrichRowsByKeys(rows, keyFilter)
	for _, row := range rows {
		key := sqlStringValue(row["key"])
		raw, gerr := db.Get("items", key)
		if gerr != nil || raw == nil {
			continue
		}
		_, data := enrichItemFields(raw)
		doi := normalizeDOI(stringFromMap(data, "DOI"))
		if doi == "" {
			continue
		}
		report.Validated++
		if work, ok := fetchCrossRefWorkByDOI(ctx, httpClient, doi); ok {
			storedTitle := strings.TrimSpace(stringFromMap(data, "title"))
			providerTitle := ""
			if len(work.Title) > 0 {
				providerTitle = strings.TrimSpace(work.Title[0])
			}
			if providerTitle != "" && normalizeTitleForMatch(storedTitle) != normalizeTitleForMatch(providerTitle) {
				report.Findings = append(report.Findings, enrichValidationFinding(key, storedTitle, "title", storedTitle, providerTitle, "crossref"))
			}
			storedYear := firstFourDigitYear(stringFromMap(data, "date"))
			providerYear := crossRefYear(work.Published)
			if providerYear != "" && storedYear != providerYear {
				report.Findings = append(report.Findings, enrichValidationFinding(key, stringFromMap(data, "title"), "year", storedYear, providerYear, "crossref"))
			}
		}
		if registered, known := resolveDOIRegisteredViaOpenCitations(ctx, httpClient, doi); known && !registered {
			report.UnverifiedDOIs = append(report.UnverifiedDOIs, key+":"+doi)
			report.Findings = append(report.Findings, enrichValidationFinding(key, stringFromMap(data, "title"), "doi_registration", doi, "not_registered", "opencitations"))
		}
	}
	return report, nil
}

func enrichValidationFinding(key, title, field, stored, provider, source string) Finding {
	return Finding{
		Kind:     "validation_discrepancy",
		Severity: sevInfo,
		ItemKey:  key,
		Title:    strings.TrimSpace(title),
		Evidence: map[string]any{
			"field":    field,
			"stored":   stored,
			"provider": provider,
		},
		Source:            FindingSource{Kind: source},
		RecommendedAction: &RecommendedAction{Text: "Review the stored metadata in Zotero against the provider record"},
	}
}

// Validation mode emits machine-readable reports in JSON mode and a compact human summary otherwise.
func renderEnrichValidationReport(cmd *cobra.Command, flags *rootFlags, report enrichValidationReport) error {
	if flags.asJSON {
		data, err := json.Marshal(report)
		if err != nil {
			return err
		}
		return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), flags)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Validated %d DOI item(s); %d finding(s); %d DOI(s) not registered in OpenCitations.\n", report.Validated, len(report.Findings), len(report.UnverifiedDOIs))
	return nil
}

// resolveEnrichment dispatches to the provider for a category and returns either
// a proposal or a non-empty skip reason.
func resolveEnrichment(ctx context.Context, httpClient *http.Client, category, key string, version any, data map[string]any, email string, useOpenAlex bool, useSemanticScholar bool, attachMode string, pdfDir string) (enrichProposal, string) {
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
		if !ok && useSemanticScholar {
			if d, ok2 := resolveDOIViaSemanticScholar(ctx, httpClient, data); ok2 {
				doi, ok, source = d, true, "Semantic Scholar"
			}
		}
		if !ok {
			return enrichProposal{}, "no confident CrossRef/OpenAlex/Semantic Scholar title match"
		}
		return enrichProposal{
			Key: key, Title: title, Category: category, Action: enrichActionPatch,
			Source: source, Note: "DOI " + doi,
			Fields:  map[string]any{"DOI": doi},
			version: version,
			extra:   stringFromMap(data, "extra"),
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
		if !ok && useSemanticScholar {
			if a, ok2 := resolveAbstractViaSemanticScholar(ctx, httpClient, doi); ok2 {
				abstract, ok, source = a, true, "Semantic Scholar"
			}
		}
		if !ok {
			return enrichProposal{}, "no abstract on CrossRef, OpenAlex, or Semantic Scholar for this DOI"
		}
		return enrichProposal{
			Key: key, Title: title, Category: category, Action: enrichActionPatch,
			Source: source, Note: fmt.Sprintf("abstract (%d chars)", len(abstract)),
			Fields:  map[string]any{"abstractNote": abstract},
			version: version,
			extra:   stringFromMap(data, "extra"),
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
		var pdfURL string
		var ok bool
		if attachMode == "linked-file" {
			pdfURL, ok = resolveDownloadPDFViaUnpaywall(ctx, httpClient, doi, email)
		} else {
			pdfURL, ok = resolvePDFViaUnpaywall(ctx, httpClient, doi, email)
		}
		if !ok {
			return enrichProposal{}, "no open-access PDF found on Unpaywall"
		}
		p := enrichProposal{
			Key: key, Title: title, Category: category, Action: enrichActionAttach,
			Source: "Unpaywall", Note: pdfURL, AttachMode: attachMode, DownloadURL: pdfURL,
		}
		switch attachMode {
		case "linked-file":
			p.PDFPath = filepath.Join(pdfDir, key+".pdf")
			p.Note = fmt.Sprintf("linked-file -> %s (download %q)", p.PDFPath, pdfURL)
			return p, ""
		default:
			p.AttachMode = "linked-url"
			p.Attachment = map[string]any{
				"itemType":   "attachment",
				"linkMode":   "linked_url",
				"title":      "Open-access PDF (Unpaywall)",
				"url":        pdfURL,
				"parentItem": key,
			}
			return p, ""
		}
	}
	return enrichProposal{}, "unknown category"
}

// Return typed mutation statuses; details travel as result reasons.
func applyEnrichProposal(c apiMutator, p *enrichProposal, flags *rootFlags) (string, any, error) {
	return applyEnrichProposalWithContext(context.Background(), enrichPDFDownloaderFactory(http.DefaultClient), c, p, flags)
}

func applyEnrichProposalWithContext(ctx context.Context, downloader enrichPDFDownloader, c apiMutator, p *enrichProposal, flags *rootFlags) (string, any, error) {
	switch p.Action {
	case enrichActionPatch:
		if c == nil {
			err := errors.New("missing API client")
			return "failed", err.Error(), err
		}
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
		switch p.AttachMode {
		case "", "linked-url":
			if c == nil {
				err := errors.New("missing API client")
				return "failed", err.Error(), err
			}
			if _, _, err := c.Post("/items", []map[string]any{p.Attachment}); err != nil {
				return enrichErrorStatus(err)
			}
			return "applied", nil, nil
		case "linked-file":
			if c == nil {
				err := errors.New("missing API client")
				return "failed", err.Error(), err
			}
			if strings.TrimSpace(p.PDFPath) == "" {
				err := errors.New("missing PDF destination path")
				return "failed", err.Error(), err
			}
			if err := os.MkdirAll(filepath.Dir(p.PDFPath), 0o755); err != nil {
				return "failed", err.Error(), err
			}
			// Reuse a PDF left by a prior attempt (e.g. an ambiguous create
			// whose response was lost): ensureLinkedAttachment reconciles the
			// write token idempotently, while re-downloading would abort on the
			// no-clobber guard and block every retry.
			if _, statErr := os.Stat(p.PDFPath); statErr == nil {
				if err := enrichPDFHasMagic(p.PDFPath); err != nil {
					return "failed", err.Error(), err
				}
			} else if err := downloader.download(ctx, p.DownloadURL, p.PDFPath, maxEnrichPDFBytes); err != nil {
				return "failed", err.Error(), err
			}
			writeClient, ok := c.(*client.Client)
			if !ok {
				err := errors.New("linked-file attachment requires a Zotero write client")
				return "failed", err.Error(), err
			}
			req, err := newLinkedFileRequest(p.Key, p.PDFPath, filepath.Base(p.PDFPath))
			if err != nil {
				return "failed", err.Error(), err
			}
			if _, _, err := ensureLinkedAttachment(writeClient, req, flags); err != nil {
				if linkedAttachmentCreationConfirmedAbsent(err) {
					if cleanupErr := os.Remove(p.PDFPath); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
						err = fmt.Errorf("creating linked-file attachment: %w; could not remove downloaded file %s: %w", err, p.PDFPath, cleanupErr)
					} else {
						err = fmt.Errorf("creating linked-file attachment: %w; removed downloaded file %s after Zotero confirmed no attachment was created", err, p.PDFPath)
					}
				} else {
					err = fmt.Errorf("creating linked-file attachment: %w; kept downloaded file %s because attachment creation may have succeeded", err, p.PDFPath)
				}
				return "failed", err.Error(), err
			}
			return "applied", map[string]any{"path": p.PDFPath}, nil
		default:
			err := fmt.Errorf("unsupported PDF attach mode %q", p.AttachMode)
			return "failed", err.Error(), err
		}
	}
	return "no_op", "unknown enrichment action", nil
}

// enrichPDFHasMagic validates a retained PDF before it is reused on retry,
// reading only the 5-byte header so a large file is never buffered.
func enrichPDFHasMagic(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("reopening downloaded PDF: %w", err)
	}
	defer f.Close()
	magic := make([]byte, 5)
	if _, err := io.ReadFull(f, magic); err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	if string(magic) != "%PDF-" {
		return fmt.Errorf("existing file %s is not a PDF (missing %%PDF- header)", path)
	}
	return nil
}

// linkedAttachmentCreationConfirmedAbsent reports the API outcomes for which
// Zotero definitively rejected the create before creating a child. Every other
// outcome (including timeouts, 5xx, 409, 412, and 429) is ambiguous and must
// retain the local PDF because it may already be a linked attachment target.
func linkedAttachmentCreationConfirmedAbsent(err error) bool {
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.StatusCode {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden,
		http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotAcceptable,
		http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType,
		http.StatusUnprocessableEntity:
		return true
	default:
		return false
	}
}

// apiMutator is the subset of *client.Client used to apply enrichments; a small
// interface keeps the apply step unit-testable without a live server.
type apiMutator interface {
	Patch(path string, body any) (json.RawMessage, int, error)
	Post(path string, body any) (json.RawMessage, int, error)
}

func enrichNeedsAPIMutator(proposals []enrichProposal) bool {
	for _, proposal := range proposals {
		if proposal.Action == enrichActionPatch || proposal.Action == enrichActionAttach {
			return true
		}
	}
	return false
}

// Convert enrichment proposals into shared mutation operations.
func enrichPlannedOps(ctx context.Context, proposals []enrichProposal, c apiMutator, flags *rootFlags, httpClient *http.Client) []mutation.Op {
	ops := make([]mutation.Op, 0, len(proposals))
	downloader := enrichPDFDownloaderFactory(httpClient)
	for i := range proposals {
		proposal := proposals[i]
		ops = append(ops, mutation.Op{
			ID:              proposal.Category + ":" + proposal.Key,
			Key:             proposal.Key,
			Kind:            proposal.Category,
			ExpectedVersion: mutationExpectedVersion(proposal.version),
			Changes:         enrichProposalChanges(proposal),
			Apply: func() (string, any, error) {
				return applyEnrichProposalWithContext(ctx, downloader, c, &proposal, flags)
			},
		})
	}
	return ops
}

func enrichProposalChanges(p enrichProposal) []mutation.Change {
	switch p.Action {
	case enrichActionPatch:
		keys := make([]string, 0, len(p.Fields))
		for key := range p.Fields {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		changes := make([]mutation.Change, 0, len(keys))
		for _, key := range keys {
			changes = append(changes, mutation.Change{Field: key, Add: p.Fields[key]})
		}
		// The PATCH also rewrites Extra to append the provenance line; surface
		// that in the preview so consent covers every field the apply touches.
		changes = append(changes, mutation.Change{Field: "extra", Add: enrichProvenanceLine(&p)})
		return changes
	case enrichActionAttach:
		switch p.AttachMode {
		case "linked-file":
			return []mutation.Change{{Field: "attachment", Add: fmt.Sprintf("linked-file -> %s (download %q)", p.PDFPath, p.DownloadURL)}}
		default:
			if url, ok := p.Attachment["url"]; ok {
				return []mutation.Change{{Field: "url", Add: url}}
			}
			return []mutation.Change{{Field: "attachment", Add: p.Attachment}}
		}
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

// enrichProvenanceLine is the audit-trail line appended to Extra on apply.
func enrichProvenanceLine(p *enrichProposal) string {
	field := "DOI"
	if p.Category == "missing_abstract" {
		field = "abstract"
	}
	return fmt.Sprintf("zotio: %s added via %s on %s", field, p.Source, time.Now().UTC().Format("2006-01-02"))
}

// appendEnrichProvenance returns the new Extra value: the item's existing Extra
// with the provenance line appended (so re-runs leave an audit trail). The
// existing content is always preserved — Extra carries user data such as Better
// BibTeX "Citation Key:" lines that a wholesale PATCH replace would destroy.
func appendEnrichProvenance(p *enrichProposal, _ *rootFlags) string {
	line := enrichProvenanceLine(p)
	existing := strings.TrimRight(p.extra, "\n")
	if existing == "" {
		return line
	}
	for _, l := range strings.Split(existing, "\n") {
		if l == line {
			return existing // identical provenance already recorded (same-day re-run)
		}
	}
	return existing + "\n" + line
}

type enrichPDFResolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

type enrichPDFDownloader struct {
	client    *http.Client
	resolver  enrichPDFResolver
	dialGuard func(net.IP) error
}

func newEnrichPDFDownloader(client *http.Client) enrichPDFDownloader {
	return enrichPDFDownloader{
		client:    client,
		resolver:  net.DefaultResolver,
		dialGuard: rejectNonPublicPDFIP,
	}
}

func (d enrichPDFDownloader) download(ctx context.Context, rawURL, destPath string, maxBytes int64) error {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return fmt.Errorf("invalid PDF URL: %w", err)
	}
	if _, err := os.Stat(destPath); err == nil {
		return fmt.Errorf("refusing to clobber existing file %s", destPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("checking %s: %w", destPath, err)
	}
	if enrichPDFURLSafetyCheck != nil {
		if err := enrichPDFURLSafetyCheck(ctx, u); err != nil {
			return err
		}
	}
	clientCopy, err := d.guardedClient()
	if err != nil {
		return err
	}
	clientCopy.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 redirects")
		}
		if enrichPDFURLSafetyCheck != nil {
			return enrichPDFURLSafetyCheck(req.Context(), req.URL)
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	resp, err := clientCopy.Do(req)
	if err != nil {
		return fmt.Errorf("downloading PDF: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("downloading PDF: HTTP %d", resp.StatusCode)
	}
	if err := validateEnrichPDFContentType(resp.Header.Get("Content-Type")); err != nil {
		return err
	}
	return writePDFResponse(resp.Body, destPath, maxBytes)
}

func (d enrichPDFDownloader) guardedClient() (*http.Client, error) {
	base := d.client
	if base == nil {
		base = http.DefaultClient
	}
	clientCopy := *base
	if d.dialGuard == nil {
		return &clientCopy, nil
	}
	var transport *http.Transport
	switch t := base.Transport.(type) {
	case nil:
		transport = http.DefaultTransport.(*http.Transport).Clone()
	case *http.Transport:
		transport = t.Clone()
	default:
		return nil, fmt.Errorf("guarding PDF download: unsupported HTTP transport %T", base.Transport)
	}
	// A proxy would resolve the destination outside this process and bypass the
	// destination-address guard, so PDF fetches always connect directly.
	transport.Proxy = nil
	transport.DialContext = d.dialContext
	clientCopy.Transport = transport
	return &clientCopy, nil
}

func (d enrichPDFDownloader) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("parsing PDF destination %q: %w", address, err)
	}
	resolver := d.resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	addrs, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolving PDF destination %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("resolving PDF destination %s: no addresses", host)
	}
	for _, addr := range addrs {
		if err := d.dialGuard(addr.IP); err != nil {
			return nil, fmt.Errorf("refusing PDF destination %s (%s): %w", host, addr.IP, err)
		}
	}
	var lastErr error
	dialer := &net.Dialer{}
	for _, addr := range addrs {
		target := net.JoinHostPort(addr.IP.String(), port)
		conn, err := dialer.DialContext(ctx, network, target)
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("dialing PDF destination %s: %w", host, lastErr)
}

func rejectNonPublicPDFIP(ip net.IP) error {
	if ip == nil {
		return fmt.Errorf("invalid IP address")
	}
	if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() ||
		(ip.To4() == nil && len(ip) == net.IPv6len && ip[0]&0xfe == 0xfc) {
		return fmt.Errorf("non-public address")
	}
	return nil
}

// validateEnrichPDFContentType is the first file-type gate: PDF downloads may
// identify as PDF, generic binary, or omit Content-Type. Other media types are
// rejected before a destination file is created; writePDFResponse then enforces
// the %PDF- magic prefix as the second gate.
func validateEnrichPDFContentType(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	mediaType, _, err := mime.ParseMediaType(raw)
	if err != nil {
		return fmt.Errorf("downloaded PDF has invalid Content-Type %q: %w", raw, err)
	}
	switch strings.ToLower(mediaType) {
	case "application/pdf", "application/octet-stream":
		return nil
	default:
		return fmt.Errorf("refusing downloaded PDF with Content-Type %q", mediaType)
	}
}

func downloadEnrichPDF(ctx context.Context, httpClient *http.Client, rawURL, destPath string, maxBytes int64) error {
	return enrichPDFDownloaderFactory(httpClient).download(ctx, rawURL, destPath, maxBytes)
}

func writePDFResponse(body io.Reader, destPath string, maxBytes int64) error {
	if maxBytes < 5 {
		return fmt.Errorf("PDF size cap must be at least 5 bytes")
	}
	f, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("refusing to clobber existing file %s", destPath)
		}
		return fmt.Errorf("creating %s: %w", destPath, err)
	}
	keep := false
	defer func() {
		_ = f.Close()
		if !keep {
			_ = os.Remove(destPath)
		}
	}()

	header := make([]byte, 5)
	n, err := io.ReadFull(body, header)
	if err != nil {
		return fmt.Errorf("downloaded file is not a PDF: missing %%PDF- header")
	}
	if string(header) != "%PDF-" {
		return fmt.Errorf("downloaded file is not a PDF: missing %%PDF- header")
	}
	written, err := f.Write(header[:n])
	if err != nil {
		return fmt.Errorf("writing %s: %w", destPath, err)
	}
	remaining := maxBytes - int64(written)
	limited := &io.LimitedReader{R: body, N: remaining + 1}
	copied, err := io.Copy(f, limited)
	if err != nil {
		return fmt.Errorf("writing %s: %w", destPath, err)
	}
	if int64(written)+copied > maxBytes {
		return fmt.Errorf("downloaded PDF exceeds size cap of %d bytes", maxBytes)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", destPath, err)
	}
	keep = true
	return nil
}

func rejectLocalPDFURL(_ context.Context, u *url.URL) error {
	if u == nil {
		return fmt.Errorf("invalid PDF URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("PDF URL must use http or https")
	}
	host := strings.TrimSpace(u.Hostname())
	if host == "" {
		return fmt.Errorf("PDF URL host is empty")
	}
	if strings.EqualFold(host, "localhost") {
		return fmt.Errorf("refusing to download PDF from localhost")
	}
	if ip := net.ParseIP(host); ip != nil {
		if err := rejectNonPublicPDFIP(ip); err != nil {
			return fmt.Errorf("refusing to download PDF from non-public address %s", host)
		}
	}
	return nil
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
	work, ok := fetchCrossRefWorkByDOI(ctx, httpClient, doi)
	if !ok {
		return "", false
	}
	abstract := stripJATS(work.Abstract)
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
		// Unpaywall data becomes a
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

func resolveDownloadPDFViaUnpaywall(ctx context.Context, httpClient *http.Client, doi, email string) (string, bool) {
	u := enrichUnpaywallBase + "/" + url.PathEscape(doi) + "?" + url.Values{"email": {email}}.Encode()
	var resp unpaywallResponse
	if err := getJSON(ctx, httpClient, u, &resp); err != nil {
		return "", false
	}
	for _, candidate := range []string{resp.BestOA.URLForPDF, resp.BestOA.URL} {
		parsed, err := url.Parse(strings.TrimSpace(candidate))
		if err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Hostname() != "" {
			return candidate, true
		}
	}
	return "", false
}

// --- Semantic Scholar (fallback for DOI + abstract). ---

type semanticScholarPaper struct {
	Title       string `json:"title"`
	ExternalIDs struct {
		DOI string `json:"DOI"`
	} `json:"externalIds"`
	Abstract string `json:"abstract"`
}

type semanticScholarSearchResponse struct {
	Data []semanticScholarPaper `json:"data"`
}

// Semantic Scholar title-search DOI fallback with the same exact-title guard as CrossRef/OpenAlex.
func resolveDOIViaSemanticScholar(ctx context.Context, httpClient *http.Client, data map[string]any) (string, bool) {
	title := stringFromMap(data, "title")
	if title == "" {
		return "", false
	}
	u := enrichSemanticScholarBase + "/paper/search?fields=title,externalIds&limit=5&query=" + url.QueryEscape(title)
	var resp semanticScholarSearchResponse
	if err := getJSON(ctx, httpClient, u, &resp); err != nil {
		return "", false
	}
	want := normalizeTitleForMatch(title)
	for _, paper := range resp.Data {
		if paper.Title == "" || paper.ExternalIDs.DOI == "" {
			continue
		}
		if normalizeTitleForMatch(paper.Title) == want {
			return normalizeDOI(paper.ExternalIDs.DOI), true
		}
	}
	return "", false
}

// Semantic Scholar DOI lookup fallback for abstracts.
func resolveAbstractViaSemanticScholar(ctx context.Context, httpClient *http.Client, doi string) (string, bool) {
	var resp semanticScholarPaper
	if err := getJSON(ctx, httpClient, enrichSemanticScholarBase+"/paper/DOI:"+url.PathEscape(doi)+"?fields=abstract", &resp); err != nil {
		return "", false
	}
	abstract := strings.TrimSpace(resp.Abstract)
	if abstract == "" {
		return "", false
	}
	return abstract, true
}

// CrossRef DOI fetch shared by abstract enrichment and validation discrepancy checks.
func fetchCrossRefWorkByDOI(ctx context.Context, httpClient *http.Client, doi string) (crossRefWork, bool) {
	work, err := fetchCrossRefWork(ctx, httpClient, doi, nil)
	if err != nil {
		return crossRefWork{}, false
	}
	return work, true
}

// OpenCitations registration probe for DOI validation mode.
func resolveDOIRegisteredViaOpenCitations(ctx context.Context, httpClient *http.Client, doi string) (registered bool, known bool) {
	var resp []map[string]any
	if err := getJSON(ctx, httpClient, enrichOpenCitationsBase+"/metadata/"+url.PathEscape(doi), &resp); err != nil {
		// Transport/non-2xx failure: unverifiable, NOT a definitive "not registered".
		return false, false
	}
	return len(resp) > 0, true
}

// --- OpenAlex (fallback for DOI + abstract; OA PDFs stay on Unpaywall, whose
// data OpenAlex merely re-serves). ---

type openAlexWork struct {
	ID                    string               `json:"id"`
	DOI                   string               `json:"doi"`
	Title                 string               `json:"title"`
	AbstractInvertedIndex map[string][]int     `json:"abstract_inverted_index"`
	Authorships           []openAlexAuthorship `json:"authorships"`
}

type openAlexAuthorship struct {
	Author openAlexAuthor `json:"author"`
}

type openAlexAuthor struct {
	DisplayName string `json:"display_name"`
	ORCID       string `json:"orcid"`
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
	// OpenAlex decodes the query
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

const (
	maxOpenAlexAbstractPosition = 50000
	maxOpenAlexAbstractPairs    = maxOpenAlexAbstractPosition + 1
)

// reconstructAbstract turns OpenAlex's {word: [positions]} inverted index back
// into running text.
func reconstructAbstract(idx map[string][]int) string {
	maxPos := -1
	pairs := 0
	for _, positions := range idx {
		for _, p := range positions {
			pairs++
			if pairs > maxOpenAlexAbstractPairs || p > maxOpenAlexAbstractPosition {
				return ""
			}
			if p < 0 {
				continue
			}
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
	req.Header.Set("User-Agent", "zotio/1.0.0")
	resp, err := sameOriginExternalFetchHTTPClient(httpClient, false).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	// Cap provider JSON bodies
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
