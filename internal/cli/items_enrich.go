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
	"strconv"
	"strings"
	"time"
	"unicode"

	"zotio/internal/client"
	"zotio/internal/cliutil"
	"zotio/internal/mutation"
	"zotio/internal/store"

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
	// enrichActionTag writes tags, which a field PATCH cannot do safely: the
	// tag array is replace-only, so the write has to re-read the item's
	// current tags and merge. It therefore reuses the tag-write applier
	// instead of the field-patch path.
	enrichActionTag enrichAction = "tag"
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
	Tags        []string       `json:"tags,omitempty"` // subject enrichment: namespaced tags to add
	// Statuses now live in mutation.Result items, not proposal JSON.
	version any               // item version for the PATCH conflict guard (not serialized)
	extra   string            // item's current Extra so provenance appends instead of replacing (not serialized)
	revert  []mutation.Change // inverse Apply captured from the live item, for journal undo (not serialized)
}

// enrichSkip records a candidate for which no confident proposal was produced.
type enrichSkip struct {
	Key      string `json:"key"`
	Title    string `json:"title"`
	Category string `json:"category"`
	Reason   string `json:"reason"`
}

type enrichLocalReadError struct {
	err error
}

func (e *enrichLocalReadError) Error() string {
	return fmt.Sprintf("loading item from local store: %v", e.err)
}

func (e *enrichLocalReadError) Unwrap() error {
	return e.err
}

func newItemsEnrichCmd(flags *rootFlags) *cobra.Command {
	var (
		flagMissingDOI        bool
		flagMissingAbstract   bool
		flagMissingPDF        bool
		flagRepairPDF         bool
		flagMissingCitation   bool
		flagMissingSubjects   bool
		flagMissingMesh       bool
		flagLimit             int
		flagEmail             string
		flagNoOpenAlex        bool
		flagNoSemanticScholar bool
		flagValidate          bool
		flagCollection        string
		flagScope             string
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
		// --agent no longer implies --yes; help names --yes as the apply switch. --scope selects the enrichment cohort.
		Long: `Resolve missing metadata for locally synced items and apply it back to Zotero.

Work queues come from the same checks as 'items audit':
  --missing-doi       resolve a DOI by title from CrossRef, then OpenAlex/Semantic Scholar (exact title match)
  --missing-abstract  fill the abstract from CrossRef, then OpenAlex/Semantic Scholar (requires the item's DOI)
  --missing-pdf       attach an open-access PDF from Unpaywall (requires DOI)
  --repair-pdf        re-attach an open-access PDF for items whose EXISTING attachment
                      file is missing or broken on disk (requires DOI and an exact key
                      set from --keys/--keys-from, since "broken" is a live fact this
                      command cannot observe locally). Feed it the broken_attachment_file
                      findings from 'zotio library health --verify-files --json'.
  --missing-citation  fill the core citation fields from CrossRef (requires DOI):
                      creators, title, date and the venue field of the item's
                      own type, plus volume/issue/pages when CrossRef carries
                      them and the item's copy is blank. A blank volume is only
                      a defect if the venue actually has one, so those fields
                      are reported by --validate, not by 'library health'.
  --missing-subjects  propose OpenAlex subject terms as automatic "concept/<term>"
                      tags for untagged items (requires DOI). Only concepts
                      OpenAlex scores at or above 0.50 are proposed, the
                      strongest six at most, so an untagged library becomes
                      facetable without burying the operator's own tags.
  --missing-mesh      propose PubMed MeSH descriptors as automatic "mesh/<descriptor>"
                      tags for items with a PMID (the PMID field or an Extra
                      "PMID:" line) or a DOI that PubMed knows. The PMID is read
                      from the item; a DOI is resolved through PubMed ESearch.
                      Every MeshHeadingList descriptor is proposed in PubMed's
                      order; qualifiers and the major-topic marker are not.
                      Items that already carry a mesh/ tag are left alone, and
                      a term the item already has is never proposed twice. A
                      record PubMed has not indexed yet is skipped, so a later
                      run picks it up. PubMed calls send tool=zotio and the
                      --email contact, paced to 3 requests per second.

PDF attachment modes:
  linked-url   create a linked_url attachment (default; no download)
  linked-file  download the PDF to --pdf-dir and create a linked_file child item; linked files do not sync to other devices

Downloaded PDFs accept application/pdf, application/octet-stream, or an absent
Content-Type, then must pass a %PDF- magic check. Stored (imported-file)
retro-attachment is handled by 'zotio attachments add <item-key> <file>', which
uploads through the Zotero Web API file-upload protocol.

By default this previews the proposed changes (a patch plan) and never downloads
PDF bytes. Pass --scope to select the cohort through the shared grammar
(collection:KEY, tag:NAME, item:KEY, query:TEXT, saved-search:KEY); --collection
is the older spelling of --scope collection:KEY and still works. Pass
--keys/--keys-from to scope to exact item keys. Pass --yes to apply via the
Zotero API; --dry-run always previews.
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
			// --repair-pdf shares every part of the missing-pdf attach path
			// except its work queue, so the attachment flags gate on both.
			wantsPDF := flagMissingPDF || flagRepairPDF
			if cmd.Flags().Changed("attach-mode") && !wantsPDF {
				return usageErr(fmt.Errorf("--attach-mode is only valid with --missing-pdf or --repair-pdf"))
			}
			if cmd.Flags().Changed("pdf-dir") && !wantsPDF {
				return usageErr(fmt.Errorf("--pdf-dir is only valid with --missing-pdf or --repair-pdf"))
			}
			if wantsPDF && attachMode == "linked-file" && strings.TrimSpace(flagPDFDir) == "" {
				return usageErr(fmt.Errorf("--pdf-dir is required with --attach-mode linked-file"))
			}
			if !flagValidate && !flagMissingDOI && !flagMissingAbstract && !wantsPDF && !flagMissingCitation && !flagMissingSubjects && !flagMissingMesh {
				return usageErr(fmt.Errorf("specify at least one of --missing-doi, --missing-abstract, --missing-pdf, --repair-pdf, --missing-citation, --missing-subjects, --missing-mesh, or --validate"))
			}
			if err := validateLimitFlag("--limit", flagLimit); err != nil {
				return err
			}
			// Whether an attachment file is broken is only knowable from the
			// live desktop API, which this command does not call. Without a
			// bounded cohort the queue would be "every item that HAS a PDF",
			// and applying that would attach a second copy to a healthy
			// library. Fail closed instead of guessing the cohort.
			//
			// --collection counts: it is the older spelling of
			// --scope collection:KEY (reconcileScopeFlags lowers it below),
			// and it names just as bounded a cohort. Checking only the raw
			// --scope string here would reject the documented spelling.
			if flagRepairPDF && strings.TrimSpace(keys) == "" && strings.TrimSpace(keysFrom) == "" &&
				strings.TrimSpace(flagScope) == "" && strings.TrimSpace(flagCollection) == "" {
				return usageErr(fmt.Errorf("--repair-pdf requires --keys, --keys-from, --scope, or --collection: a broken attachment file is a live fact this command cannot detect, so it repairs only the items you name (pipe `zotio library health --verify-files --json` findings in)"))
			}

			pdfDir := ""
			if wantsPDF && attachMode == "linked-file" {
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
				return errUnsyncedStoreFor(cmd, flags)
			}
			defer rawDB.Close()
			db := localQueryStore{rawDB}

			if strings.TrimSpace(keys) != "" && strings.TrimSpace(keysFrom) != "" {
				return usageErr(fmt.Errorf("--keys cannot be combined with --keys-from"))
			}
			// --scope and --keys/--keys-from both hand the work queue an exact key
			// set, and the grammar has no multi-key arm to reconcile them through,
			// so there is no expression that could prove they agree.
			if strings.TrimSpace(flagScope) != "" && (strings.TrimSpace(keys) != "" || strings.TrimSpace(keysFrom) != "") {
				return usageErr(fmt.Errorf("--scope cannot be combined with --keys/--keys-from: both select exact item keys and the scope grammar has no multi-key arm to reconcile them; pass one of them"))
			}
			// --collection lowers to an exact scope key set below, so combining it
			// with an explicit key set has the same ambiguity as --scope.
			if strings.TrimSpace(flagCollection) != "" && (strings.TrimSpace(keys) != "" || strings.TrimSpace(keysFrom) != "") {
				return usageErr(fmt.Errorf("--collection cannot be combined with --keys/--keys-from: both select exact item keys; pass one of them"))
			}
			collectionScope := scopeSugarFor("collection", "collection", flagCollection)
			effectiveScope, err := reconcileScopeFlags(flagScope, collectionScope)
			if err != nil {
				return err
			}
			if strings.TrimSpace(flagCollection) != "" {
				if err := validateKeyArg("collection", strings.TrimSpace(flagCollection)); err != nil {
					return err
				}
			}
			// reconcileScopeFlags intentionally leaves bespoke flags on their
			// existing query paths when --scope is absent. Enrich also needs the
			// collection as an exact key allow-set for repair_pdf, whose queue has
			// no safe library-wide form, so lower this command's sugar here.
			if effectiveScope == "" && collectionScope.set {
				effectiveScope = collectionScope.expr
			}
			sel, err := resolveScopeSelection(cmd, flags, "items enrich", db, effectiveScope)
			if err != nil {
				return err
			}
			keyFilter, err := enrichKeyFilter(keys, keysFrom, cmd.InOrStdin())
			if err != nil {
				return err
			}
			// A resolved cohort enters the same allow-set --keys-from feeds, so the
			// two spellings share one selection path instead of two.
			if sel.restricted() {
				keyFilter = sel.Keys
			}

			httpClient := &http.Client{Timeout: enrichTimeout(flags.timeout)}
			useOpenAlex := !flagNoOpenAlex
			useSemanticScholar := !flagNoSemanticScholar
			if flagValidate {
				report, err := validateEnrichItems(cmd.Context(), db, httpClient, flagLimit, flagCollection, keyFilter)
				if err != nil {
					return err
				}
				if err := renderEnrichValidationReport(cmd, flags, report); err != nil {
					return err
				}
				// The report is printed, but a run in which a provider could
				// not answer is a partial result. Exit 13 lets a script tell
				// it from a clean pass, as the other degraded reports do.
				if len(report.Incomplete) > 0 {
					return markRendered(degradedErr(fmt.Errorf("validation incomplete: %d provider check(s) got no answer; those items are not counted as validated. Rerun when the providers recover", len(report.Incomplete))))
				}
				return nil
			}

			var proposals []enrichProposal
			var skipped []enrichSkip
			var proposalErrs []error

			// Thread collection scope through every selected enrichment category.
			if flagMissingDOI {
				p, s, buildErr := buildEnrichProposals(cmd.Context(), db, httpClient, "missing_doi", flagLimit, flagCollection, keyFilter, flagEmail, useOpenAlex, useSemanticScholar, "linked-url", "")
				proposals, skipped = append(proposals, p...), append(skipped, s...)
				if buildErr != nil {
					proposalErrs = append(proposalErrs, buildErr)
				}
			}
			if flagMissingAbstract {
				p, s, buildErr := buildEnrichProposals(cmd.Context(), db, httpClient, "missing_abstract", flagLimit, flagCollection, keyFilter, flagEmail, useOpenAlex, useSemanticScholar, "linked-url", "")
				proposals, skipped = append(proposals, p...), append(skipped, s...)
				if buildErr != nil {
					proposalErrs = append(proposalErrs, buildErr)
				}
			}
			if flagMissingPDF {
				p, s, buildErr := buildEnrichProposals(cmd.Context(), db, httpClient, "missing_pdf", flagLimit, flagCollection, keyFilter, flagEmail, useOpenAlex, useSemanticScholar, attachMode, pdfDir)
				proposals, skipped = append(proposals, p...), append(skipped, s...)
				if buildErr != nil {
					proposalErrs = append(proposalErrs, buildErr)
				}
			}
			if flagRepairPDF {
				p, s, buildErr := buildEnrichProposals(cmd.Context(), db, httpClient, "repair_pdf", flagLimit, flagCollection, keyFilter, flagEmail, useOpenAlex, useSemanticScholar, attachMode, pdfDir)
				proposals, skipped = append(proposals, p...), append(skipped, s...)
				if buildErr != nil {
					proposalErrs = append(proposalErrs, buildErr)
				}
			}
			if flagMissingCitation {
				p, s, buildErr := buildEnrichProposals(cmd.Context(), db, httpClient, "missing_citation", flagLimit, flagCollection, keyFilter, flagEmail, useOpenAlex, useSemanticScholar, "linked-url", "")
				proposals, skipped = append(proposals, p...), append(skipped, s...)
				if buildErr != nil {
					proposalErrs = append(proposalErrs, buildErr)
				}
			}
			if flagMissingSubjects {
				p, s, buildErr := buildEnrichProposals(cmd.Context(), db, httpClient, "missing_subjects", flagLimit, flagCollection, keyFilter, flagEmail, useOpenAlex, useSemanticScholar, "linked-url", "")
				proposals, skipped = append(proposals, p...), append(skipped, s...)
				if buildErr != nil {
					proposalErrs = append(proposalErrs, buildErr)
				}
			}
			if flagMissingMesh {
				p, s, buildErr := buildEnrichProposals(cmd.Context(), db, httpClient, "missing_mesh", flagLimit, flagCollection, keyFilter, flagEmail, useOpenAlex, useSemanticScholar, "linked-url", "")
				proposals, skipped = append(proposals, p...), append(skipped, s...)
				if buildErr != nil {
					proposalErrs = append(proposalErrs, buildErr)
				}
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
			if wantsPDF && attachMode != "linked-url" {
				flags.continueOnError = true
				defer func() { flags.continueOnError = restoreContinue }()
			}
			env, runErr := runMutation(cmd.Context(), flags, "items.enrich", ops)
			if len(skipped) > 0 {
				// Merge, never replace: runMutation has already put the
				// applied run's run_id here, and journal undo needs it.
				// Only benign skips live here (no match, nothing to fill);
				// a failed provider lookup is an error, reported below.
				journal, _ := env.Journal.(map[string]any)
				if journal == nil {
					journal = map[string]any{}
				}
				journal["skipped"] = skipped
				env.Journal = journal
			}
			if len(proposalErrs) > 0 {
				runErr = enrichLookupFailure(&env, runErr, proposalErrs)
			}
			return renderMutationResult(cmd, flags, env, runErr, nil)
		},
	}
	cmd.Flags().BoolVar(&flagMissingDOI, "missing-doi", false, "Resolve and add a DOI from CrossRef, OpenAlex, or Semantic Scholar")
	cmd.Flags().BoolVar(&flagMissingAbstract, "missing-abstract", false, "Fill the abstract from CrossRef, OpenAlex, or Semantic Scholar (uses the item's DOI)")
	cmd.Flags().BoolVar(&flagMissingPDF, "missing-pdf", false, "Attach an open-access PDF from Unpaywall as a link or download (uses the item's DOI)")
	cmd.Flags().BoolVar(&flagRepairPDF, "repair-pdf", false, "Re-attach an open-access PDF for named items whose existing attachment file is broken or missing on disk (uses the item's DOI; requires --keys/--keys-from/--scope/--collection)")
	cmd.Flags().BoolVar(&flagMissingCitation, "missing-citation", false, "Fill the core citation fields from CrossRef: creators, title, date, venue, plus volume/issue/pages when the provider has them and the item does not (uses the item's DOI)")
	// No backticks in this usage string: cobra reads a backquoted word as the
	// flag's value placeholder, so `concept/<term>` rendered as if this bool
	// flag took an argument.
	cmd.Flags().BoolVar(&flagMissingSubjects, "missing-subjects", false, "Propose OpenAlex subject terms as automatic concept/<term> tags for untagged items (uses the item's DOI)")
	cmd.Flags().BoolVar(&flagMissingMesh, "missing-mesh", false, "Propose PubMed MeSH descriptors as automatic mesh/<descriptor> tags for items with a PMID or DOI that carry no mesh/ tag yet")
	cmd.Flags().StringVar(&flagAttachMode, "attach-mode", "linked-url", "PDF attachment handling: linked-url or linked-file; stored retro-attachment is handled by `zotio attachments add`")
	cmd.Flags().StringVar(&flagPDFDir, "pdf-dir", "", "Directory for linked-file PDF downloads; responses must be PDF/octet-stream/unspecified Content-Type plus %PDF- magic")
	cmd.Flags().IntVar(&flagLimit, "limit", 25, "Maximum items to process per category (0 = no limit)")
	cmd.Flags().StringVar(&flagScope, "scope", scopeFlagDefaultUnset, scopeFlagUsageDefaultLibrary)
	// --collection predates --scope and stays supported; it lowers to
	// --scope collection:KEY (see reconcileScopeFlags).
	cmd.Flags().StringVar(&flagCollection, "collection", "", "Scope the work queue to items in a collection key")
	cmd.Flags().StringVar(&keys, "keys", "", "Comma-separated exact item keys to enrich")
	cmd.Flags().StringVar(&keysFrom, "keys-from", "", "Read exact item keys from a file or '-' for stdin, then enrich only matching queued items")
	cmd.Flags().StringVar(&flagEmail, "email", "", "Contact email for Unpaywall (required for --missing-pdf), the OpenAlex polite pool and NCBI PubMed for --missing-mesh (both optional); or set UNPAYWALL_EMAIL")
	cmd.Flags().BoolVar(&flagNoOpenAlex, "no-openalex", false, "Disable the OpenAlex fallback for --missing-doi/--missing-abstract")
	cmd.Flags().BoolVar(&flagNoSemanticScholar, "no-semantic-scholar", false, "Disable the Semantic Scholar fallback for --missing-doi/--missing-abstract")
	cmd.Flags().BoolVar(&flagValidate, "validate", false, "Read-only DOI discrepancy report against CrossRef and OpenCitations")

	return cmd
}

// enrichLookupFailure folds failed provider lookups (and unreadable local
// rows) into the envelope: ok:false with an error entry, and the same
// non-zero exit the process returns. Benign skips never reach here; they stay
// in journal.skipped with ok:true. An engine error already on the envelope (a
// gate refusal) keeps its own entry and exit code. A run that applied part of
// its plan before the failures exits 13 (partial). Otherwise the exit code
// follows what the providers said: auth failures (401/403) exit 4, other
// client errors (400, ...) exit 2, transport failures, 429 and 5xx exit 5,
// and an unreadable local row exits 1.
func enrichLookupFailure(env *mutation.Envelope, runErr error, lookupErrs []error) error {
	joined := errors.Join(lookupErrs...)
	if env.Error != nil {
		return errors.Join(runErr, joined)
	}
	env.OK = false
	env.Error = &mutation.Error{Code: "enrich_lookup_failed", Message: joined.Error()}
	if env.Result != nil && env.Result.Summary.Applied > 0 {
		return degradedErr(errors.Join(runErr, joined))
	}
	var typed error
	switch enrichLookupExitCode(joined) {
	case 4:
		typed = authErr(joined)
	case 2:
		typed = usageErr(joined)
	case 5:
		typed = apiErr(joined)
	default:
		typed = joined
	}
	return errors.Join(runErr, typed)
}

var enrichProviderStatusRE = regexp.MustCompile(`\bHTTP (\d{3})\b`)

// enrichLookupExitCode ranks the failed lookups in err and returns the exit
// code of the most specific class: auth (4) over client error (2) over
// upstream failure (5) over everything else (1). A provider status is read
// from the lookup's own message; a lookup with no status is a transport,
// decode or cancellation failure, which counts as upstream.
func enrichLookupExitCode(err error) int {
	rank := map[int]int{4: 4, 2: 3, 5: 2, 1: 1}
	best := 1
	consider := func(code int) {
		if rank[code] > rank[best] {
			best = code
		}
	}
	var walk func(error)
	walk = func(e error) {
		if e == nil {
			return
		}
		if multi, ok := e.(interface{ Unwrap() []error }); ok {
			for _, inner := range multi.Unwrap() {
				walk(inner)
			}
			return
		}
		var providerErr *enrichProviderError
		if !errors.As(e, &providerErr) {
			return
		}
		match := enrichProviderStatusRE.FindStringSubmatch(providerErr.Error())
		if match == nil {
			consider(5)
			return
		}
		status, _ := strconv.Atoi(match[1])
		switch {
		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			consider(4)
		case status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500:
			consider(5)
		case status >= 400:
			consider(2)
		default:
			consider(5)
		}
	}
	walk(err)
	return best
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
func buildEnrichProposals(ctx context.Context, db localQueryStore, httpClient *http.Client, category string, limit int, collection string, keyFilter map[string]bool, email string, useOpenAlex bool, useSemanticScholar bool, attachMode string, pdfDir string) ([]enrichProposal, []enrichSkip, error) {
	rows, err := enrichWorkQueueForKeys(db, category, limit, collection, keyFilter)
	if err != nil {
		return nil, nil, fmt.Errorf("querying %s work queue: %w", category, err)
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
			if errors.Is(gerr, store.ErrNotFound) {
				return outcome{skip: enrichSkip{Key: key, Category: category, Reason: "item not found in local store"}, skipped: true}, nil
			}
			if gerr != nil {
				return outcome{}, &enrichLocalReadError{err: gerr}
			}
			version, data := enrichItemFields(raw)
			title := stringFromMap(data, "title")
			prop, reason, resolveErr := resolveEnrichment(ctx, httpClient, category, key, version, data, email, useOpenAlex, useSemanticScholar, attachMode, pdfDir)
			if resolveErr != nil {
				return outcome{}, &enrichProviderError{err: resolveErr}
			}
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
	// A failed provider lookup or an unreadable local row is not a "no match":
	// it becomes a command error (and an error entry on the envelope), so the
	// run cannot exit clean after an outage. Only a cancelled lookup, which
	// carries neither error type, stays a skip.
	var buildErrs []error
	for _, fanoutErr := range fanoutErrs {
		var localReadErr *enrichLocalReadError
		if errors.As(fanoutErr.Err, &localReadErr) {
			buildErrs = append(buildErrs, fmt.Errorf("loading enrichment item %s: %w", fanoutErr.Source, fanoutErr.Err))
			continue
		}
		var providerErr *enrichProviderError
		if errors.As(fanoutErr.Err, &providerErr) {
			buildErrs = append(buildErrs, fmt.Errorf("enriching item %s (%s): %w", fanoutErr.Source, category, fanoutErr.Err))
			continue
		}
		skipped = append(skipped, enrichSkip{
			Key:      fanoutErr.Source,
			Category: category,
			Reason:   fmt.Sprintf("resolving enrichment: %v", fanoutErr.Err),
		})
	}
	return proposals, skipped, errors.Join(buildErrs...)
}

const enrichKeyChunkSize = 900

// enrichWorkQueueForKeys keeps exact remediation scopes in SQL rather than
// materializing the full work queue. SQLite caps bound variables, so large
// key selections run as separately ordered chunks before applying the global
// result limit.
func enrichWorkQueueForKeys(db localQueryStore, category string, limit int, collection string, keyFilter map[string]bool) ([]map[string]any, error) {
	if keyFilter == nil {
		return enrichWorkQueue(db, category, limit, collection)
	}

	keys := make([]string, 0, len(keyFilter))
	for key, allowed := range keyFilter {
		if allowed {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return nil, nil
	}

	// The citation queue is filtered in Go, not in SQL, so it has no bound-
	// variable ceiling and must not be chunked: each chunk would re-run the
	// whole query and append the same rows again.
	if category == "missing_citation" {
		rows, err := queryCitationEnrichCandidates(db, 0, collection)
		if err != nil {
			return nil, err
		}
		return applyEnrichQueueLimit(filterEnrichRowsByKeys(rows, keyFilter), limit), nil
	}
	sort.Strings(keys)

	rows := make([]map[string]any, 0, len(keys))
	for start := 0; start < len(keys); start += enrichKeyChunkSize {
		end := min(start+enrichKeyChunkSize, len(keys))
		chunk, err := queryEnrichWorkQueueKeyChunk(db, category, collection, keys[start:end])
		if err != nil {
			return nil, err
		}
		rows = append(rows, chunk...)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		left, right := rows[i]["date_added"], rows[j]["date_added"]
		if left == nil {
			return false
		}
		if right == nil {
			return true
		}
		return sqlStringValue(left) > sqlStringValue(right)
	})
	return applyEnrichQueueLimit(rows, limit), nil
}

// applyEnrichQueueLimit caps a work queue assembled in Go. The SQL paths cap
// themselves with LIMIT; the chunked and Go-filtered paths cannot, because the
// cap applies to the merged result rather than to any one query.
func applyEnrichQueueLimit(rows []map[string]any, limit int) []map[string]any {
	if limit > 0 && len(rows) > limit {
		return rows[:limit]
	}
	return rows
}

// queryEnrichWorkQueueKeyChunk must select exactly what the unkeyed queue in
// enrichWorkQueue selects, only narrowed to the given keys: a cohort passed
// through --scope/--keys is a filter on the queue, never a second definition of
// "missing". Both arms therefore carry libraryTopLevelItemsPredicate, the same
// guard queryMissingDOIItems/queryMissingAbstractItems apply. Without it a
// child attachment or a standalone note named in the key set joined the
// missing-abstract queue, and enrichment proposed an abstract for a PDF.
func queryEnrichWorkQueueKeyChunk(db localQueryStore, category, collection string, keys []string) ([]map[string]any, error) {
	switch category {
	case "missing_doi":
		query := `
SELECT
	id AS key,
	json_extract(data, '$.data.title') AS title,
	json_extract(data, '$.data.itemType') AS item_type,
	json_extract(data, '$.data.DOI') AS doi,
	json_extract(data, '$.data.dateAdded') AS date_added
FROM resources
WHERE resource_type = 'items'
	AND ` + libraryTopLevelItemsPredicate + `
	AND item_type IN ('journalArticle', 'conferencePaper', 'preprint')
	AND (json_extract(data, '$.data.DOI') IS NULL OR TRIM(json_extract(data, '$.data.DOI')) = '')`
		args := enrichCollectionFilterArgs(&query, "data", collection)
		query += `
	AND id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",") + `)
ORDER BY date_added DESC`
		for _, key := range keys {
			args = append(args, key)
		}
		return db.QueryRaw(query, args...)
	case "missing_abstract":
		query := `
SELECT
	id AS key,
	json_extract(data, '$.data.title') AS title,
	json_extract(data, '$.data.itemType') AS item_type,
	json_extract(data, '$.data.DOI') AS doi,
	json_extract(data, '$.data.dateAdded') AS date_added
FROM resources
WHERE resource_type = 'items'
	AND ` + libraryTopLevelItemsPredicate + `
	AND (json_extract(data, '$.data.abstractNote') IS NULL OR TRIM(json_extract(data, '$.data.abstractNote')) = '')`
		args := enrichCollectionFilterArgs(&query, "data", collection)
		query += `
	AND id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",") + `)
ORDER BY date_added DESC`
		for _, key := range keys {
			args = append(args, key)
		}
		return db.QueryRaw(query, args...)
	case "missing_pdf":
		return queryMissingPDFItemsForKeys(db, "", 0, collection, keys)
	case "repair_pdf":
		return queryRepairPDFItemsForKeys(db, 0, collection, keys)
	case "missing_subjects":
		return queryMissingTagsItemsForKeys(db, 0, collection, keys)
	case "missing_mesh":
		return queryMissingMeshItems(db, 0, collection, keys)
	default:
		return nil, fmt.Errorf("unknown category %q", category)
	}
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
	case "repair_pdf":
		// Unreachable in practice: the command refuses --repair-pdf without a
		// key set or scope, so this arm never sees an unkeyed queue. Name it
		// anyway rather than returning "unknown category" for a real one.
		return nil, fmt.Errorf("repair_pdf has no library-wide work queue: name the broken items with --keys/--keys-from/--scope")
	case "missing_subjects":
		return queryMissingTagsItemsScoped(db, limit, collection)
	case "missing_mesh":
		return queryMissingMeshItems(db, limit, collection, nil)
	case "missing_citation":
		return queryCitationEnrichCandidates(db, limit, collection)
	default:
		return nil, fmt.Errorf("unknown category %q", category)
	}
}

// enrichValidationIncomplete names one provider check that could not be
// answered for one item: a transport failure, a 5xx or 429, an undecodable
// body. It is not a finding about the metadata, and the item is not counted as
// validated while one is outstanding.
type enrichValidationIncomplete struct {
	Key      string `json:"key"`
	DOI      string `json:"doi"`
	Provider string `json:"provider"`
	Error    string `json:"error"`
}

type enrichValidationReport struct {
	// Validated counts DOI items whose every provider check was answered.
	Validated      int       `json:"validated"`
	Findings       []Finding `json:"findings"`
	UnverifiedDOIs []string  `json:"unverified_dois"`
	// Incomplete lists the checks no provider answered, so an outage cannot
	// read as a clean pass.
	Incomplete []enrichValidationIncomplete `json:"incomplete"`
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
		Incomplete:     []enrichValidationIncomplete{},
	}
	// The key filter runs in Go, so a SQL LIMIT has to come off whenever a
	// selection is present: it would spend the budget on rows the filter then
	// drops and validate fewer items than the selection actually holds.
	queueLimit := limit
	if keyFilter != nil {
		queueLimit = 0
	}
	rows, err := queryEnrichValidationItems(db, queueLimit, collection)
	if err != nil {
		return report, fmt.Errorf("querying validation work queue: %w", err)
	}
	rows = applyEnrichQueueLimit(filterEnrichRowsByKeys(rows, keyFilter), limit)

	type outcome struct {
		validated     bool
		findings      []Finding
		unverifiedDOI string
		incomplete    []enrichValidationIncomplete
	}
	results, fanoutErrs := cliutil.FanoutRun(ctx, rows,
		func(row map[string]any) string { return sqlStringValue(row["key"]) },
		func(ctx context.Context, row map[string]any) (outcome, error) {
			key := sqlStringValue(row["key"])
			raw, err := db.Get("items", key)
			// Absence stays a skip, as it was before the fan-out: this pass
			// validates what it can read, and a row the mirror no longer holds
			// (pruned by a concurrent sync) is not a validation failure. A real
			// read failure is still reported rather than silently dropped.
			if errors.Is(err, store.ErrNotFound) {
				return outcome{}, nil
			}
			if err != nil {
				return outcome{}, fmt.Errorf("loading validation item: %w", err)
			}
			_, data := enrichItemFields(raw)
			doi := normalizeDOI(stringFromMap(data, "DOI"))
			if doi == "" {
				return outcome{}, nil
			}

			result := outcome{}
			work, crossRefFound, crossRefErr := lookupCrossRefWork(ctx, httpClient, doi)
			if crossRefErr != nil {
				result.incomplete = append(result.incomplete, enrichValidationIncomplete{Key: key, DOI: doi, Provider: "crossref", Error: crossRefErr.Error()})
			}
			// CrossRef answering "no such record" (404/410) leaves nothing to
			// compare, but it is an answer, not an outage.
			if crossRefFound {
				storedTitle := strings.TrimSpace(stringFromMap(data, "title"))
				providerTitle := ""
				if len(work.Title) > 0 {
					providerTitle = strings.TrimSpace(work.Title[0])
				}
				if providerTitle != "" && normalizeTitleForMatch(storedTitle) != normalizeTitleForMatch(providerTitle) {
					result.findings = append(result.findings, enrichValidationFinding(key, storedTitle, "title", storedTitle, providerTitle, "crossref"))
				}
				storedYear := firstFourDigitYear(stringFromMap(data, "date"))
				providerYear := crossRefYear(work.Published)
				if providerYear != "" && storedYear != providerYear {
					result.findings = append(result.findings, enrichValidationFinding(key, stringFromMap(data, "title"), "year", storedYear, providerYear, "crossref"))
				}
				// Render gaps: CrossRef carries the value and the item's copy is
				// blank. This is the measuring surface for the fields
				// citationIncompletePredicate deliberately refuses to assert
				// locally, so the fixer writes nothing no check has verified.
				for _, field := range citationRenderFields[stringFromMap(data, "itemType")] {
					if strings.TrimSpace(stringFromMap(data, field)) != "" {
						continue
					}
					if value := citationRenderFromCrossRef(field, work); value != "" {
						result.findings = append(result.findings, enrichValidationFinding(key, stringFromMap(data, "title"), field, "", value, "crossref"))
					}
				}
			}
			registered, openCitationsErr := resolveDOIRegisteredViaOpenCitations(ctx, httpClient, doi)
			if openCitationsErr != nil {
				result.incomplete = append(result.incomplete, enrichValidationIncomplete{Key: key, DOI: doi, Provider: "opencitations", Error: openCitationsErr.Error()})
			} else if !registered {
				result.unverifiedDOI = key + ":" + doi
				result.findings = append(result.findings, enrichValidationFinding(key, stringFromMap(data, "title"), "doi_registration", doi, "not_registered", "opencitations"))
			}
			// A lookup cut short by cancellation is the run being stopped, not
			// a provider outage: surface it as the worker's error.
			if err := ctx.Err(); err != nil {
				return outcome{}, err
			}
			result.validated = len(result.incomplete) == 0
			return result, nil
		})

	// FanoutRun returns successful outcomes in row order. Merge them here so
	// report ordering stays byte-identical and no worker mutates shared slices.
	for _, result := range results {
		// Findings from a provider that did answer stay in the report even
		// when the other one did not; only the validated count waits for both.
		report.Incomplete = append(report.Incomplete, result.Value.incomplete...)
		report.Findings = append(report.Findings, result.Value.findings...)
		if result.Value.unverifiedDOI != "" {
			report.UnverifiedDOIs = append(report.UnverifiedDOIs, result.Value.unverifiedDOI)
		}
		if result.Value.validated {
			report.Validated++
		}
	}
	return report, FanoutReportErrors(fanoutErrs)
}

func enrichValidationFinding(key, title, field, stored, provider, source string) Finding {
	finding := Finding{
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
	// A blank local field is a gap the fixer closes; a field that DISAGREES
	// with the provider is a judgement call, and auto-overwriting a title or a
	// year the operator may have corrected by hand would be a data loss. Only
	// the gap gets an autofix route.
	if stored == "" && provider != "" {
		finding.Autofixable = true
		finding.RecommendedAction = &RecommendedAction{Command: "zotio items enrich --missing-citation --keys-from -"}
	}
	return finding
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
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Validated %d DOI item(s); %d finding(s); %d DOI(s) not registered in OpenCitations.\n", report.Validated, len(report.Findings), len(report.UnverifiedDOIs))
	if len(report.Incomplete) > 0 {
		fmt.Fprintf(out, "Incomplete: %d provider check(s) got no answer and are not counted as validated:\n", len(report.Incomplete))
		for _, gap := range report.Incomplete {
			fmt.Fprintf(out, "  %s %s (%s): %s\n", sanitizeForTerminal(gap.Key), sanitizeForTerminal(gap.DOI), gap.Provider, sanitizeForTerminal(gap.Error))
		}
	}
	return nil
}

// resolveEnrichment dispatches to the provider for a category and returns a
// proposal, a non-empty skip reason, or an error. The skip reason means the
// providers answered and have no usable metadata. The error means at least one
// provider could not answer and none supplied a result, so the item's metadata
// is unknown, not absent.
func resolveEnrichment(ctx context.Context, httpClient *http.Client, category, key string, version any, data map[string]any, email string, useOpenAlex bool, useSemanticScholar bool, attachMode string, pdfDir string) (enrichProposal, string, error) {
	title := stringFromMap(data, "title")
	// lookupErrs collects failed provider lookups. A later provider that finds
	// a result still wins, but without one the failures decide the outcome.
	var lookupErrs []error
	note := func(err error) {
		if err != nil {
			lookupErrs = append(lookupErrs, err)
		}
	}
	// miss reports "no result": a provider error when any lookup failed,
	// otherwise the genuine no-match reason.
	miss := func(reason string) (enrichProposal, string, error) {
		if err := errors.Join(lookupErrs...); err != nil {
			return enrichProposal{}, "", err
		}
		return enrichProposal{}, reason, nil
	}
	switch category {
	case "missing_doi":
		if title == "" {
			return enrichProposal{}, "no title to search for a DOI", nil
		}
		doi, _, ok, err := resolveDOIViaCrossRef(ctx, httpClient, data)
		note(err)
		source := "CrossRef"
		if !ok && useOpenAlex {
			d, ok2, err := resolveDOIViaOpenAlex(ctx, httpClient, data, email)
			note(err)
			if ok2 {
				doi, ok, source = d, true, "OpenAlex"
			}
		}
		if !ok && useSemanticScholar {
			d, ok2, err := resolveDOIViaSemanticScholar(ctx, httpClient, data)
			note(err)
			if ok2 {
				doi, ok, source = d, true, "Semantic Scholar"
			}
		}
		if !ok {
			return miss("no confident CrossRef/OpenAlex/Semantic Scholar title match")
		}
		return enrichProposal{
			Key: key, Title: title, Category: category, Action: enrichActionPatch,
			Source: source, Note: "DOI " + doi,
			Fields:  map[string]any{"DOI": doi},
			version: version,
			extra:   stringFromMap(data, "extra"),
		}, "", nil

	case "missing_abstract":
		doi := normalizeDOI(stringFromMap(data, "DOI"))
		if doi == "" {
			return enrichProposal{}, "no DOI to look up abstract", nil
		}
		abstract, ok, err := resolveAbstractViaCrossRef(ctx, httpClient, doi)
		note(err)
		source := "CrossRef"
		if !ok && useOpenAlex {
			a, ok2, err := resolveAbstractViaOpenAlex(ctx, httpClient, doi, email)
			note(err)
			if ok2 {
				abstract, ok, source = a, true, "OpenAlex"
			}
		}
		if !ok && useSemanticScholar {
			a, ok2, err := resolveAbstractViaSemanticScholar(ctx, httpClient, doi)
			note(err)
			if ok2 {
				abstract, ok, source = a, true, "Semantic Scholar"
			}
		}
		if !ok {
			return miss("no abstract on CrossRef, OpenAlex, or Semantic Scholar for this DOI")
		}
		return enrichProposal{
			Key: key, Title: title, Category: category, Action: enrichActionPatch,
			Source: source, Note: fmt.Sprintf("abstract (%d chars)", len(abstract)),
			Fields:  map[string]any{"abstractNote": abstract},
			version: version,
			extra:   stringFromMap(data, "extra"),
		}, "", nil

	case "missing_citation":
		// A DOI is the only safe identity anchor here. The --missing-doi
		// resolver matches on an exact title, but a citation-incomplete item
		// may lack creators AND date AND venue, so a title match has far less
		// corroboration and a wrong match writes a wrong reference — the very
		// failure this check exists to catch.
		doi := normalizeDOI(stringFromMap(data, "DOI"))
		if doi == "" {
			return enrichProposal{}, "no DOI to look up citation fields; run --missing-doi first", nil
		}
		work, found, err := lookupCrossRefWork(ctx, httpClient, doi)
		if err != nil {
			return enrichProposal{}, "", err
		}
		if !found {
			return enrichProposal{}, "no CrossRef record for this DOI", nil
		}
		fields := citationEnrichFields(data, work)
		if len(fields) == 0 {
			return enrichProposal{}, "CrossRef supplied none of the citation fields this item lacks", nil
		}
		return enrichProposal{
			Key: key, Title: title, Category: category, Action: enrichActionPatch,
			Source: "CrossRef", Note: "citation fields " + strings.Join(sortedFieldNames(fields), ", "),
			Fields:  fields,
			version: version,
			extra:   stringFromMap(data, "extra"),
		}, "", nil

	case "missing_subjects":
		// OpenAlex only. CrossRef carries no subject vocabulary, and the
		// concepts OpenAlex assigns are discipline-agnostic, which is what an
		// untagged library needs; the biomedical MeSH route is the separate
		// missing_mesh category below.
		doi := normalizeDOI(stringFromMap(data, "DOI"))
		if doi == "" {
			return enrichProposal{}, "no DOI to look up subject terms; run --missing-doi first", nil
		}
		if !useOpenAlex {
			return enrichProposal{}, "subject terms come from OpenAlex, which --no-openalex disabled", nil
		}
		tags, ok, err := resolveSubjectsViaOpenAlex(ctx, httpClient, doi, email)
		if err != nil {
			return enrichProposal{}, "", err
		}
		if !ok {
			return enrichProposal{}, fmt.Sprintf("OpenAlex has no concepts scoring at least %.2f for this DOI", minOpenAlexConceptScore), nil
		}
		return enrichProposal{
			Key: key, Title: title, Category: category, Action: enrichActionTag,
			Source: "OpenAlex", Note: "subject tags " + strings.Join(tags, ", "),
			Tags:    tags,
			version: version,
			extra:   stringFromMap(data, "extra"),
		}, "", nil

	case "missing_mesh":
		tags, source, reason, err := resolveMeshViaPubMed(ctx, httpClient, data, email)
		if err != nil {
			return enrichProposal{}, "", err
		}
		if reason != "" {
			return enrichProposal{}, reason, nil
		}
		return enrichProposal{
			Key: key, Title: title, Category: category, Action: enrichActionTag,
			Source: source, Note: "MeSH tags " + strings.Join(tags, ", "),
			Tags:    tags,
			version: version,
			extra:   stringFromMap(data, "extra"),
		}, "", nil

	// repair_pdf resolves and attaches exactly as missing_pdf does; only the
	// work queue differs, because the item already HAS a PDF child whose file
	// is broken. The new child is an additional attachment: Zotero has no
	// re-point operation for a broken one, and deleting it would discard the
	// annotations stored against it.
	case "missing_pdf", "repair_pdf":
		doi := normalizeDOI(stringFromMap(data, "DOI"))
		if doi == "" {
			return enrichProposal{}, "no DOI to look up open-access PDF", nil
		}
		if email == "" {
			email = enrichUnpaywallEmail()
		}
		if email == "" {
			return enrichProposal{}, "Unpaywall requires a contact email (--email or UNPAYWALL_EMAIL)", nil
		}
		var pdfURL string
		var ok bool
		var err error
		if attachMode == "linked-file" {
			pdfURL, ok, err = resolveDownloadPDFViaUnpaywall(ctx, httpClient, doi, email)
		} else {
			pdfURL, ok, err = resolvePDFViaUnpaywall(ctx, httpClient, doi, email)
		}
		if err != nil {
			return enrichProposal{}, "", err
		}
		if !ok {
			return enrichProposal{}, "no open-access PDF found on Unpaywall", nil
		}
		p := enrichProposal{
			Key: key, Title: title, Category: category, Action: enrichActionAttach,
			Source: "Unpaywall", Note: pdfURL, AttachMode: attachMode, DownloadURL: pdfURL,
		}
		switch attachMode {
		case "linked-file":
			p.PDFPath = filepath.Join(pdfDir, key+".pdf")
			p.Note = fmt.Sprintf("linked-file -> %s (download %q)", p.PDFPath, pdfURL)
			return p, "", nil
		default:
			p.AttachMode = "linked-url"
			p.Attachment = map[string]any{
				"itemType":    "attachment",
				"linkMode":    "linked_url",
				"title":       "Open-access PDF (Unpaywall)",
				"url":         pdfURL,
				"parentItem":  key,
				"contentType": "application/pdf",
			}
			return p, "", nil
		}
	}
	return enrichProposal{}, "unknown category", nil
}

// citationEnrichFields returns the citation-core fields the STORED item lacks
// and CrossRef can supply. Every test reads the stored item, never what the
// provider returned: a Zotero PATCH replaces a whole field, and `creators` is
// an array, so proposing creators for an item that already has them would
// destroy the operator's own names. An item joins this queue for any single
// missing field, so filling only the blank ones is the whole point.
//
// `extra` is never a key here. applyEnrichProposalWithContext overwrites
// body["extra"] with the provenance line, so an `extra` field would be
// silently dropped.
func citationEnrichFields(data map[string]any, work crossRefWork) map[string]any {
	fields := map[string]any{}
	if creators, _ := data["creators"].([]any); len(creators) == 0 {
		if resolved := crossRefCreators(work.Author); len(resolved) > 0 {
			fields["creators"] = resolved
		}
	}
	if strings.TrimSpace(stringFromMap(data, "title")) == "" {
		if title := firstCrossRefString(work.Title, ""); title != "" {
			fields["title"] = title
		}
	}
	if strings.TrimSpace(stringFromMap(data, "date")) == "" {
		if year := crossRefYear(work.Published); year != "" {
			fields["date"] = year
		}
	}
	// The venue field name is per item type, so it comes from the same table
	// that generates the check's SQL. A type absent from that table has no
	// venue requirement and contributes nothing.
	if field, ok := citationVenueFields[stringFromMap(data, "itemType")]; ok {
		if strings.TrimSpace(stringFromMap(data, field)) == "" {
			if venue := citationVenueFromCrossRef(field, work); venue != "" {
				fields[field] = venue
			}
		}
	}
	// Render fields: filled only when the item's own type defines them, the
	// item's copy is blank, AND CrossRef actually carries a value. That last
	// condition is the whole reason these are not in the check predicate - an
	// article-number journal has no volume, so "blank locally" alone proves
	// nothing and a local-only assertion would gate CI on a field the venue
	// never had.
	for _, field := range citationRenderFields[stringFromMap(data, "itemType")] {
		if strings.TrimSpace(stringFromMap(data, field)) != "" {
			continue
		}
		if value := citationRenderFromCrossRef(field, work); value != "" {
			fields[field] = value
		}
	}
	return fields
}

// citationVenueFromCrossRef maps a CrossRef work onto the one venue field the
// item's own type defines. A preprint's `repository` has no CrossRef
// counterpart: posted-content carries an institution or a group title, and
// neither reliably names the repository. That arm therefore resolves to
// nothing and the item is reported as a skip rather than filled with a guess.
func citationVenueFromCrossRef(field string, work crossRefWork) string {
	switch field {
	case "publicationTitle", "proceedingsTitle":
		return firstCrossRefString(work.ContainerTitle, "")
	case "publisher":
		return strings.TrimSpace(work.Publisher)
	default:
		return ""
	}
}

// citationRenderFromCrossRef maps a CrossRef work onto one Zotero render field.
// CrossRef names the page range `page`; Zotero names it `pages`.
func citationRenderFromCrossRef(field string, work crossRefWork) string {
	switch field {
	case "volume":
		return strings.TrimSpace(work.Volume)
	case "issue":
		return strings.TrimSpace(work.Issue)
	case "pages":
		return strings.TrimSpace(work.Page)
	default:
		return ""
	}
}

// sortedFieldNames keeps a proposal's note stable across runs; ranging a Go
// map directly would reorder it on every call.
func sortedFieldNames(fields map[string]any) []string {
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func applyEnrichProposalWithContext(ctx context.Context, downloader enrichPDFDownloader, c apiMutator, p *enrichProposal, flags *rootFlags) (string, any, error) {
	switch p.Action {
	case enrichActionPatch:
		if c == nil {
			err := errors.New("missing API client")
			return "failed", err.Error(), err
		}
		// The proposal came from the local mirror. Resolve both Extra and the
		// version from the write plane so this wholesale field replacement
		// preserves live content and guards the PATCH in the write plane's
		// independent version space.
		return applyEnrichFieldPatch(ctx, c, p, flags)
	case enrichActionAttach:
		switch p.AttachMode {
		case "", "linked-url":
			if c == nil {
				err := errors.New("missing API client")
				return "failed", err.Error(), err
			}
			// The work queue is a mirror query, so it cannot see a child this
			// or an earlier run created since the last sync: re-running the
			// command (the natural retry after a partial failure) would
			// otherwise hang another copy of the same open-access link on the
			// item. Reconcile against the live write plane before posting, and
			// send a token derived from parent and URL so a response lost
			// after Zotero committed cannot commit a second copy either.
			writer, ok := c.(linkedURLWriter)
			if !ok {
				err := errors.New("attaching an open-access PDF requires the Zotero write client to reconcile existing children")
				return "failed", err.Error(), err
			}
			existing, reconcileErr := findLinkedURLAttachment(ctx, writer, p.Key, p.DownloadURL)
			if reconcileErr != nil {
				return enrichErrorStatus(reconcileErr)
			}
			if existing != "" {
				return "no_op", map[string]any{"existing_attachment": existing, "url": p.DownloadURL}, nil
			}
			baseToken := writeToken("zotio.items.enrich.linked-url", p.Key, p.DownloadURL)
			var resp json.RawMessage
			for generation := 0; ; generation++ {
				var err error
				resp, _, err = writer.PostWithHeaders("/items", []map[string]any{p.Attachment}, map[string]string{"Zotero-Write-Token": writeTokenGeneration(baseToken, generation)})
				if err == nil {
					break
				}
				var apiErr *client.APIError
				if client.IsAmbiguousWriteError(err) || (errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusPreconditionFailed) {
					// The token may already have committed the child. Only the
					// live children can say.
					existing, rerr := findLinkedURLAttachment(ctx, writer, p.Key, p.DownloadURL)
					if rerr == nil && existing != "" {
						return "applied", map[string]any{"existing_attachment": existing, "url": p.DownloadURL}, nil
					}
					// A used token with no live attachment left means the
					// earlier one was deleted: a deliberate re-add takes the
					// next token.
					if rerr == nil && apiErr != nil && generation+1 < writeTokenMaxGenerations {
						continue
					}
					err = fmt.Errorf("creating linked-url attachment: %w; the write token for %s under %s was already submitted but no matching attachment was found; review the item in Zotero before re-running", err, p.DownloadURL, p.Key)
				}
				return enrichErrorStatus(err)
			}
			// A Zotero batch write answers HTTP 200 even when it rejects the
			// element, so the only record of the outcome is the body. Require
			// the created key, exactly as ensureLinkedAttachment does for the
			// linked-file sibling: a `failed` entry or a response naming no
			// key means no attachment exists, and "applied" would journal a
			// write that never happened.
			if failErr := batchWriteFailuresError("creating linked-url attachment", decodeBatchWriteResponse(resp).Failed); failErr != nil {
				return enrichErrorStatus(failErr)
			}
			if _, ok := createdItemKey(resp); !ok {
				return enrichErrorStatus(errors.New("could not read created linked-url attachment key from /items response"))
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
	case enrichActionTag:
		writeClient, ok := c.(*client.Client)
		if !ok || writeClient == nil {
			err := errors.New("subject tags require the Zotero write client")
			return "failed", err.Error(), err
		}
		return applyEnrichSubjectTags(ctx, writeClient, p, flags)
	}
	return "no_op", "unknown enrichment action", nil
}

// applyEnrichSubjectTags merges subject tags into an item and records
// provenance in the same PATCH.
//
// It cannot reuse applyItemTagAdd: that helper reads the current tags with a
// plain GET, which under hybrid routing hits the DESKTOP plane while the PATCH
// is routed to the Web API. `items tags add` is safe because it holds a client
// built by newWriteClient; enrich builds its client with newClient, so the
// read must be pinned to the write plane here (write_precondition.go:16-33).
//
// It also writes Extra. Every other enrich category records where a change
// came from, and the help promises it; the automatic tag type and the
// `concept/` prefix mark a tag as managed but name neither the provider nor
// the date. Tags and Extra go in ONE patch so a partial apply cannot leave a
// tag with no provenance.
func applyEnrichSubjectTags(ctx context.Context, c *client.Client, p *enrichProposal, flags *rootFlags) (string, any, error) {
	path := replacePathParam("/items/{itemKey}", "itemKey", p.Key)
	currentData, version, err := c.GetFromWriteBaseWithVersionContext(ctx, path, nil)
	if err != nil {
		return "failed", err.Error(), err
	}
	currentTags, err := itemDataTags(currentData)
	if err != nil {
		return "failed", err.Error(), err
	}
	nextTags := copyItemTags(currentTags)
	added := make([]string, 0, len(p.Tags))
	for _, tag := range p.Tags {
		if itemHasTag(currentTags, tag) {
			continue
		}
		nextTags = append(nextTags, map[string]any{"tag": tag, "type": 1})
		added = append(added, tag)
	}
	if len(added) == 0 {
		// The mirror said the item was untagged; the live item already has
		// every proposed term. Nothing to write, and no provenance to record
		// for a write that did not happen.
		return "no_op", map[string]any{"tags": "already present"}, nil
	}
	// Extra is a wholesale replace, so the provenance line must be appended
	// to the LIVE value. `p.extra` came from the mirror, which can be older
	// than the item; using it would delete anything written to Extra since
	// the last sync, such as a Better BibTeX "Citation Key:" line.
	live := *p
	_, liveData := enrichItemFields(currentData)
	live.extra = stringFromMap(liveData, "extra")
	// Name only the terms this write added, so a term that landed on the live
	// item since the last sync is not credited to the provider.
	live.Tags = added
	extra, line := enrichProvenanceExtra(&live)
	body := map[string]any{"tags": nextTags, "extra": extra}
	// Journal undo needs the inverse of exactly this write: the terms it added,
	// not every proposed term, and the provenance line only if this write put
	// it there.
	revert := make([]mutation.Change, 0, len(added)+1)
	for _, tag := range added {
		revert = append(revert, mutation.Change{Field: "tags", Remove: tag, TagType: 1})
	}
	p.revert = withExtraLineRevert(revert, line)
	// The tags and Extra above were derived from currentData, so the write must
	// be guarded by the version that read returned. Resolving a fresh version
	// here would let a tag or Extra edit made since the read pass the
	// precondition and be overwritten by this stale payload.
	snapshot := &enrichWriteSnapshotMutator{
		apiMutator: c,
		reader:     c,
		path:       path,
		data:       currentData,
		version:    version,
		pending:    true,
	}
	return patchWithWritePlaneVersion(ctx, snapshot, path, body)
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
// interface keeps the apply step unit-testable without a live server. It
// includes the write-plane precondition surface because a key-based Zotero
// write must carry an If-Unmodified-Since-Version read from the plane the
// write is routed to (see write_precondition.go).
// linkedURLWriter is what the linked-url attach needs beyond apiMutator: a
// write-plane children read to reconcile against, and a POST that carries a
// Zotero-Write-Token.
type linkedURLWriter interface {
	writePlaneReader
	PostWithHeaders(path string, body any, headers map[string]string) (json.RawMessage, int, error)
}

type apiMutator interface {
	Patch(path string, body any) (json.RawMessage, int, error)
	PatchWithHeaders(path string, body any, headers map[string]string) (json.RawMessage, int, error)
	Post(path string, body any) (json.RawMessage, int, error)
}

// enrichWriteSnapshotMutator lets patchWithWritePlaneVersion consume a
// write-plane item read that the enrich path already needed for live Extra.
// Only the first matching read uses the snapshot. A retry after an ambiguous
// PATCH delegates to the real reader so reconciliation observes current state.
type enrichWriteSnapshotMutator struct {
	apiMutator
	reader  writePlaneReader
	path    string
	data    json.RawMessage
	version int
	pending bool
}

func (m *enrichWriteSnapshotMutator) GetFromWriteBaseWithVersionContext(ctx context.Context, path string, params map[string]string) (json.RawMessage, int, error) {
	if m.pending && path == m.path {
		m.pending = false
		return m.data, m.version, nil
	}
	return m.reader.GetFromWriteBaseWithVersionContext(ctx, path, params)
}

func applyEnrichFieldPatch(ctx context.Context, c apiMutator, p *enrichProposal, flags *rootFlags) (string, any, error) {
	path := replacePathParam("/items/{itemKey}", "itemKey", p.Key)
	reader, ok := c.(writePlaneReader)
	if !ok {
		err := fmt.Errorf("client for %s cannot read the write plane; refusing to write without an If-Unmodified-Since-Version precondition", path)
		return "failed", err.Error(), err
	}
	currentData, version, err := reader.GetFromWriteBaseWithVersionContext(ctx, path, nil)
	if err != nil {
		return classifyWriteError(err)
	}
	var current map[string]any
	if err := json.Unmarshal(currentData, &current); err != nil {
		err = fmt.Errorf("decoding write-plane item %s: %w", path, err)
		return "failed", err.Error(), err
	}
	liveData := current
	if inner, ok := current["data"].(map[string]any); ok {
		liveData = inner
	}

	live := *p
	live.extra = stringFromMap(liveData, "extra")
	body := make(map[string]any, len(p.Fields)+1)
	for k, v := range p.Fields {
		body[k] = v
	}
	extra, line := enrichProvenanceExtra(&live)
	body["extra"] = extra
	// Journal undo restores each field to what the live item held a moment
	// ago, not to what the possibly stale mirror said it held.
	p.revert = enrichFieldRevert(p.Fields, liveData, line)

	snapshot := &enrichWriteSnapshotMutator{
		apiMutator: c,
		reader:     reader,
		path:       path,
		data:       currentData,
		version:    version,
		pending:    true,
	}
	return patchWithWritePlaneVersion(ctx, snapshot, path, body)
}

func enrichNeedsAPIMutator(proposals []enrichProposal) bool {
	for _, proposal := range proposals {
		switch proposal.Action {
		case enrichActionPatch, enrichActionAttach, enrichActionTag:
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
			// Apply fills proposal.revert for the two actions that write
			// Extra; every other action leaves it nil and is not reversible.
			Revert: func() []mutation.Change { return proposal.revert },
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
		// Extra is built from a fresh write-plane read during Apply. The mirror
		// value in p.extra can be stale, so omit Extra from the planned replay.
		// The next sync then accepts the write plane's authoritative value.
		return changes
	case enrichActionAttach:
		switch p.AttachMode {
		case "linked-file":
			return []mutation.Change{{Field: "attachment", Add: fmt.Sprintf("linked-file -> %s (download %q)", p.PDFPath, p.DownloadURL)}}
		default:
			// linked-url POSTs a NEW CHILD attachment item; the parent's own
			// fields (including its real "url" field) are never touched by
			// Zotero. Record the child body under a synthetic, non-schema
			// field name so the mirror can never mistake it for a real
			// parent-item field and replay it onto the parent's data.
			return []mutation.Change{{Field: "attachment", Add: p.Attachment}}
		}
	case enrichActionTag:
		// TagType 1 marks the tag automatic, which is how a managed subject
		// term stays distinguishable from a tag the operator typed. The
		// mirror replays these as tag additions, never as a tags array.
		changes := make([]mutation.Change, 0, len(p.Tags))
		for _, tag := range p.Tags {
			changes = append(changes, mutation.Change{Field: "tags", Add: tag, TagType: 1})
		}
		// Extra is built from a fresh write-plane read during Apply. Do not
		// replay the mirror-derived p.extra; the next sync supplies the
		// authoritative value.
		return changes
	default:
		return nil
	}
}

func enrichErrorStatus(err error) (string, any, error) {
	// Single source of truth for 412/428 -> "conflict" lives in
	// write_precondition.go; keep this name for existing call sites.
	return classifyWriteError(err)
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

// enrichProvenanceLine is the audit-trail line appended to Extra on apply. It
// names the fields actually written: a reader auditing a bibliography needs to
// know which values came from a provider rather than from them, and the
// citation category writes a different subset for every item.
func enrichProvenanceLine(p *enrichProposal) string {
	field := "DOI"
	switch p.Category {
	case "missing_abstract":
		field = "abstract"
	case "missing_citation":
		field = strings.Join(sortedFieldNames(p.Fields), "+")
	case "missing_subjects":
		// Name the terms, not just "tags": the point of the line is that a
		// reader can tell which tags came from a provider rather than them,
		// and the automatic tag type alone does not say which provider or
		// when.
		field = "subject tags " + strings.Join(p.Tags, "+")
	case "missing_mesh":
		// The terms can run to a dozen or more, so the line carries the count;
		// the PMID in the source ties it to the PubMed record that lists them.
		field = fmt.Sprintf("MeSH tags (%d)", len(p.Tags))
	}
	return fmt.Sprintf("zotio: %s added via %s on %s", field, p.Source, time.Now().UTC().Format("2006-01-02"))
}

// appendEnrichProvenance returns the new Extra value: the item's existing Extra
// with the provenance line appended (so re-runs leave an audit trail). The
// existing content is always preserved — Extra carries user data such as Better
// BibTeX "Citation Key:" lines that a wholesale PATCH replace would destroy.
func appendEnrichProvenance(p *enrichProposal, _ *rootFlags) string {
	extra, _ := enrichProvenanceExtra(p)
	return extra
}

// enrichProvenanceExtra is appendEnrichProvenance plus the line this call
// added. added is empty when an identical line was already present (a
// same-day re-run), even one an editor gave a CRLF terminator: the earlier
// write owns that line, so undoing this one must not delete it.
func enrichProvenanceExtra(p *enrichProposal) (extra, added string) {
	line := enrichProvenanceLine(p)
	existing := strings.TrimRight(p.extra, "\n")
	if existing == "" {
		return line, line
	}
	for _, l := range strings.Split(existing, "\n") {
		if strings.TrimSuffix(l, "\r") == line {
			return existing, "" // identical provenance already recorded (same-day re-run)
		}
	}
	return existing + "\n" + line, line
}

// extraLineField names the synthetic change that carries one whole line of an
// item's Extra. It is the only way undo can say "remove the provenance line
// this run added": Extra is a free-text field the enrich write replaced in
// full, so an Add/Remove pair on "extra" itself would clobber the user's
// other lines on replay.
const extraLineField = "extra_line"

// withExtraLineRevert appends the removal of the line a write added to Extra.
// An empty line means the write added none, so there is nothing to remove.
func withExtraLineRevert(revert []mutation.Change, line string) []mutation.Change {
	if line == "" {
		return revert
	}
	return append(revert, mutation.Change{Field: extraLineField, Remove: line})
}

// enrichFieldRevert builds the inverse of a field PATCH: each written field
// goes back to the value the live item held before the write (Add), and only
// while it still holds the value the run wrote (Remove). It returns nil, so
// the op stays refused as before, when a written value is not a string: undo
// has no way to restore anything else.
func enrichFieldRevert(fields, live map[string]any, line string) []mutation.Change {
	revert := make([]mutation.Change, 0, len(fields)+1)
	for _, name := range sortedFieldNames(fields) {
		written, ok := fields[name].(string)
		if !ok {
			return nil
		}
		revert = append(revert, mutation.Change{Field: name, Add: stringFromMap(live, name), Remove: written})
	}
	return withExtraLineRevert(revert, line)
}

// removeExtraLine deletes the first line of extra equal to line, together with
// that line's own separator, and reports whether it found one. It is the exact
// inverse of enrichProvenanceExtra on a trimmed Extra: no blank line is left
// behind and every other byte, including lines the user added after the
// provenance line, is kept.
//
// Lines are split on LF, and a trailing CR is not part of the line's content:
// an editor that rewrote Extra with CRLF separators still matches. A line
// takes the separator after it with it, so the other lines keep their own CRLF
// terminators; the last line has none, so it takes the CRLF before it instead.
func removeExtraLine(extra, line string) (string, bool) {
	if line == "" {
		return extra, false
	}
	lines := strings.Split(extra, "\n")
	for i, l := range lines {
		if strings.TrimSuffix(l, "\r") != line {
			continue
		}
		if i == len(lines)-1 && i > 0 {
			lines[i-1] = strings.TrimSuffix(lines[i-1], "\r")
		}
		return strings.Join(append(lines[:i:i], lines[i+1:]...), "\n"), true
	}
	return extra, false
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
	clientCopy, releaseTransport, err := d.guardedClient()
	if err != nil {
		return err
	}
	defer releaseTransport()
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

// guardedClient returns a client whose transport dials only vetted
// destinations, plus a release func the caller must invoke when the download
// finishes. Release is load-bearing: the guarded transport is cloned per call
// (its DialContext closes over this downloader's resolver, so a shared cache
// would bind the wrong guard), and an unreleased clone strands its own
// connection pool of idle sockets in long-running zotio-mcp/watch processes.
func (d enrichPDFDownloader) guardedClient() (*http.Client, func(), error) {
	base := d.client
	if base == nil {
		base = http.DefaultClient
	}
	clientCopy := *base
	if d.dialGuard == nil {
		// Shares the caller's transport; closing its idle connections here
		// would evict pooled sockets this call never created.
		return &clientCopy, func() {}, nil
	}
	var transport *http.Transport
	switch t := base.Transport.(type) {
	case nil:
		transport = http.DefaultTransport.(*http.Transport).Clone()
	case *http.Transport:
		transport = t.Clone()
	default:
		return nil, nil, fmt.Errorf("guarding PDF download: unsupported HTTP transport %T", base.Transport)
	}
	// A proxy would resolve the destination outside this process and bypass the
	// destination-address guard, so PDF fetches always connect directly.
	transport.Proxy = nil
	transport.DialContext = d.dialContext
	clientCopy.Transport = transport
	return &clientCopy, transport.CloseIdleConnections, nil
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

// writePDFResponse stages the body in a same-directory temporary file and
// publishes it at destPath only after the header, size cap, fsync, and close
// all succeed. A process killed mid-download therefore leaves, at worst, an
// unreferenced temporary file: destPath is either absent or a complete PDF, so
// a retry never meets a truncated file that the no-clobber guard would refuse.
func writePDFResponse(body io.Reader, destPath string, maxBytes int64) error {
	if maxBytes < 5 {
		return fmt.Errorf("PDF size cap must be at least 5 bytes")
	}
	tmpPath, err := stageAtomicOutput(filepath.Dir(destPath), 0o600, func(w io.Writer) error {
		if err := copyPDFBody(w, body, destPath, maxBytes); err != nil {
			return err
		}
		if s, ok := w.(interface{ Sync() error }); ok {
			if err := s.Sync(); err != nil {
				return fmt.Errorf("syncing %s: %w", destPath, err)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return publishPDFNoClobber(tmpPath, destPath)
}

// copyPDFBody applies the %PDF- magic gate and the size cap while copying.
func copyPDFBody(w io.Writer, body io.Reader, destPath string, maxBytes int64) error {
	header := make([]byte, 5)
	n, err := io.ReadFull(body, header)
	if err != nil {
		return fmt.Errorf("downloaded file is not a PDF: missing %%PDF- header")
	}
	if string(header) != "%PDF-" {
		return fmt.Errorf("downloaded file is not a PDF: missing %%PDF- header")
	}
	written, err := w.Write(header[:n])
	if err != nil {
		return fmt.Errorf("writing %s: %w", destPath, err)
	}
	remaining := maxBytes - int64(written)
	limited := &io.LimitedReader{R: body, N: remaining + 1}
	copied, err := io.Copy(w, limited)
	if err != nil {
		return fmt.Errorf("writing %s: %w", destPath, err)
	}
	if int64(written)+copied > maxBytes {
		return fmt.Errorf("downloaded PDF exceeds size cap of %d bytes", maxBytes)
	}
	return nil
}

// publishPDFNoClobber moves the staged file to destPath without replacing
// anything already there. A hard link fails with EEXIST instead of
// overwriting, which keeps the no-clobber guarantee atomic even if another
// writer creates destPath after the early existence check. Filesystems
// without hard links first claim destPath with an O_EXCL placeholder, so no
// other writer can create it, then rename the staged file over that
// placeholder. The staged file is removed on every path.
func publishPDFNoClobber(tmpPath, destPath string) error {
	defer func() { _ = os.Remove(tmpPath) }()
	err := os.Link(tmpPath, destPath)
	if err == nil {
		return nil
	}
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("refusing to clobber existing file %s", destPath)
	}
	placeholder, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("refusing to clobber existing file %s", destPath)
		}
		return fmt.Errorf("publishing %s: %w", destPath, err)
	}
	_ = placeholder.Close()
	if renameErr := os.Rename(tmpPath, destPath); renameErr != nil {
		_ = os.Remove(destPath)
		return fmt.Errorf("publishing %s: %w", destPath, renameErr)
	}
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

// enrichProviderError marks a lookup that could not be answered: a transport
// failure, a non-404 HTTP status (429, 5xx), an undecodable body, or a
// cancelled context. It is distinct from a provider's authoritative "no such
// record", so a failed lookup is reported as an error instead of being
// recorded as proof that the item has no metadata.
type enrichProviderError struct {
	err error
}

func (e *enrichProviderError) Error() string {
	return fmt.Sprintf("provider lookup failed: %v", e.err)
}

func (e *enrichProviderError) Unwrap() error {
	return e.err
}

// getProviderJSON performs getJSON and separates the three outcomes a resolver
// must not conflate: the record exists and decoded (found, nil), the provider
// answered that no such record exists, HTTP 404 or 410 (not found, nil), and
// the provider could not answer (not found, error naming the provider).
func getProviderJSON(ctx context.Context, httpClient *http.Client, provider, rawURL string, out any) (bool, error) {
	err := getJSON(ctx, httpClient, rawURL, out)
	switch {
	case err == nil:
		return true, nil
	case isRegistryRecordAbsent(err):
		return false, nil
	default:
		return false, fmt.Errorf("%s: %w", provider, err)
	}
}

// lookupCrossRefWork fetches a CrossRef work by DOI with the same three-way
// outcome as getProviderJSON.
func lookupCrossRefWork(ctx context.Context, httpClient *http.Client, doi string) (crossRefWork, bool, error) {
	work, err := fetchCrossRefWork(ctx, httpClient, doi, nil)
	switch {
	case err == nil:
		return work, true, nil
	case isRegistryRecordAbsent(err):
		return crossRefWork{}, false, nil
	default:
		return crossRefWork{}, false, err
	}
}

// resolveDOIViaCrossRef searches CrossRef by bibliographic query and returns the
// DOI of the candidate whose title matches the item's title exactly (after
// normalization), guarding against attaching a wrong DOI. A provider failure is
// returned as an error, never as a missing match.
func resolveDOIViaCrossRef(ctx context.Context, httpClient *http.Client, data map[string]any) (string, crossRefWork, bool, error) {
	title := stringFromMap(data, "title")
	if title == "" {
		return "", crossRefWork{}, false, nil
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
	found, err := getProviderJSON(ctx, httpClient, "CrossRef", u, &resp)
	if err != nil || !found {
		return "", crossRefWork{}, false, err
	}
	want := normalizeTitleForMatch(title)
	// Empty-equals-empty must never count as an exact match: punctuation-only
	// or (pre-fix) non-Latin titles all collapsed to "" and would otherwise
	// accept the first provider hit as a false exact match.
	if want == "" {
		return "", crossRefWork{}, false, nil
	}
	for _, w := range resp.Message.Items {
		if len(w.Title) == 0 || w.DOI == "" {
			continue
		}
		if cand := normalizeTitleForMatch(w.Title[0]); cand != "" && cand == want {
			return normalizeDOI(w.DOI), w, true, nil
		}
	}
	return "", crossRefWork{}, false, nil
}

// resolveAbstractViaCrossRef fetches a work by DOI and returns its abstract with
// JATS XML markup stripped.
func resolveAbstractViaCrossRef(ctx context.Context, httpClient *http.Client, doi string) (string, bool, error) {
	work, found, err := lookupCrossRefWork(ctx, httpClient, doi)
	if err != nil || !found {
		return "", false, err
	}
	abstract := stripJATS(work.Abstract)
	if abstract == "" {
		return "", false, nil
	}
	return abstract, true, nil
}

type unpaywallResponse struct {
	BestOA struct {
		URLForPDF string `json:"url_for_pdf"`
		URL       string `json:"url"`
	} `json:"best_oa_location"`
}

// resolvePDFViaUnpaywall returns the best open-access PDF URL for a DOI.
func resolvePDFViaUnpaywall(ctx context.Context, httpClient *http.Client, doi, email string) (string, bool, error) {
	u := enrichUnpaywallBase + "/" + url.PathEscape(doi) + "?" + url.Values{"email": {email}}.Encode()
	var resp unpaywallResponse
	found, err := getProviderJSON(ctx, httpClient, "Unpaywall", u, &resp)
	if err != nil || !found {
		return "", false, err
	}
	if resp.BestOA.URLForPDF != "" {
		// Unpaywall data becomes a
		// Zotero linked-url attachment, so only HTTPS public URLs are accepted.
		if err := validateExternalHTTPURL(resp.BestOA.URLForPDF, true); err == nil {
			return resp.BestOA.URLForPDF, true, nil
		}
	}
	if resp.BestOA.URL != "" {
		if err := validateExternalHTTPURL(resp.BestOA.URL, true); err == nil {
			return resp.BestOA.URL, true, nil
		}
	}
	return "", false, nil
}

func resolveDownloadPDFViaUnpaywall(ctx context.Context, httpClient *http.Client, doi, email string) (string, bool, error) {
	u := enrichUnpaywallBase + "/" + url.PathEscape(doi) + "?" + url.Values{"email": {email}}.Encode()
	var resp unpaywallResponse
	found, err := getProviderJSON(ctx, httpClient, "Unpaywall", u, &resp)
	if err != nil || !found {
		return "", false, err
	}
	for _, candidate := range []string{resp.BestOA.URLForPDF, resp.BestOA.URL} {
		parsed, err := url.Parse(strings.TrimSpace(candidate))
		if err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Hostname() != "" {
			return candidate, true, nil
		}
	}
	return "", false, nil
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
func resolveDOIViaSemanticScholar(ctx context.Context, httpClient *http.Client, data map[string]any) (string, bool, error) {
	title := stringFromMap(data, "title")
	if title == "" {
		return "", false, nil
	}
	u := enrichSemanticScholarBase + "/paper/search?fields=title,externalIds&limit=5&query=" + url.QueryEscape(title)
	var resp semanticScholarSearchResponse
	found, err := getProviderJSON(ctx, httpClient, "Semantic Scholar", u, &resp)
	if err != nil || !found {
		return "", false, err
	}
	want := normalizeTitleForMatch(title)
	if want == "" {
		return "", false, nil
	}
	for _, paper := range resp.Data {
		if paper.Title == "" || paper.ExternalIDs.DOI == "" {
			continue
		}
		if cand := normalizeTitleForMatch(paper.Title); cand != "" && cand == want {
			return normalizeDOI(paper.ExternalIDs.DOI), true, nil
		}
	}
	return "", false, nil
}

// Semantic Scholar DOI lookup fallback for abstracts.
func resolveAbstractViaSemanticScholar(ctx context.Context, httpClient *http.Client, doi string) (string, bool, error) {
	var resp semanticScholarPaper
	found, err := getProviderJSON(ctx, httpClient, "Semantic Scholar", enrichSemanticScholarBase+"/paper/DOI:"+url.PathEscape(doi)+"?fields=abstract", &resp)
	if err != nil || !found {
		return "", false, err
	}
	abstract := strings.TrimSpace(resp.Abstract)
	if abstract == "" {
		return "", false, nil
	}
	return abstract, true, nil
}

// CrossRef DOI fetch for the creator-audit pass, which counts a failed lookup
// itself. Enrichment and validation use lookupCrossRefWork, which keeps a
// provider failure apart from a missing record.
func fetchCrossRefWorkByDOI(ctx context.Context, httpClient *http.Client, doi string) (crossRefWork, bool) {
	work, err := fetchCrossRefWork(ctx, httpClient, doi, nil)
	if err != nil {
		return crossRefWork{}, false
	}
	return work, true
}

// OpenCitations registration probe for DOI validation mode. An answer is
// (registered, nil): an empty result is OpenCitations saying it holds no record
// of the DOI. Anything that is not an answer (transport failure, non-2xx, an
// undecodable or null body) is an error, never a "not registered" verdict, so
// the caller can report the check as incomplete.
func resolveDOIRegisteredViaOpenCitations(ctx context.Context, httpClient *http.Client, doi string) (bool, error) {
	var resp []map[string]any
	if err := getJSON(ctx, httpClient, enrichOpenCitationsBase+"/metadata/"+url.PathEscape(doi), &resp); err != nil {
		return false, fmt.Errorf("OpenCitations: %w", err)
	}
	if resp == nil {
		return false, errors.New("OpenCitations: response body was null, not a list")
	}
	return len(resp) > 0, nil
}

// --- OpenAlex (fallback for DOI + abstract; OA PDFs stay on Unpaywall, whose
// data OpenAlex merely re-serves). ---

type openAlexWork struct {
	ID                    string               `json:"id"`
	DOI                   string               `json:"doi"`
	Title                 string               `json:"title"`
	AbstractInvertedIndex map[string][]int     `json:"abstract_inverted_index"`
	Authorships           []openAlexAuthorship `json:"authorships"`
	Concepts              []openAlexConcept    `json:"concepts"`
}

// openAlexConcept is one subject term OpenAlex assigns to a work, with the
// confidence it assigns to that term.
type openAlexConcept struct {
	DisplayName string  `json:"display_name"`
	Score       float64 `json:"score"`
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
func resolveDOIViaOpenAlex(ctx context.Context, httpClient *http.Client, data map[string]any, email string) (string, bool, error) {
	title := stringFromMap(data, "title")
	if title == "" {
		return "", false, nil
	}
	var resp openAlexSearchResponse
	found, err := getProviderJSON(ctx, httpClient, "OpenAlex", openAlexWorksURL("title.search:"+openAlexFilterLiteral(title), email), &resp)
	if err != nil || !found {
		return "", false, err
	}
	want := normalizeTitleForMatch(title)
	if want == "" {
		return "", false, nil
	}
	for _, w := range resp.Results {
		if w.DOI == "" || w.Title == "" {
			continue
		}
		if cand := normalizeTitleForMatch(w.Title); cand != "" && cand == want {
			return normalizeDOI(w.DOI), true, nil
		}
	}
	return "", false, nil
}

// resolveAbstractViaOpenAlex fetches a work by DOI and reconstructs its abstract
// from OpenAlex's inverted index.
func resolveAbstractViaOpenAlex(ctx context.Context, httpClient *http.Client, doi, email string) (string, bool, error) {
	var resp openAlexSearchResponse
	found, err := getProviderJSON(ctx, httpClient, "OpenAlex", openAlexWorksURL("doi:"+openAlexFilterLiteral(strings.ToLower(doi)), email), &resp)
	if err != nil || !found {
		return "", false, err
	}
	if len(resp.Results) == 0 {
		return "", false, nil
	}
	abstract := reconstructAbstract(resp.Results[0].AbstractInvertedIndex)
	if abstract == "" {
		return "", false, nil
	}
	return abstract, true, nil
}

const (
	// OpenAlex scores every concept it assigns, including weak ones. A low
	// threshold turns a facetable subject list into noise, and these tags are
	// written into the operator's library, so the bar is deliberately high.
	minOpenAlexConceptScore = 0.5
	// Zotero shows tags inline on the item; a long tail buries the operator's
	// own tags. Keep the strongest handful.
	maxEnrichSubjectTags = 6
	// Tags are namespaced so a managed subject term stays distinguishable
	// from a tag the operator typed, and so removing them again is a single
	// selectable prefix.
	enrichSubjectTagPrefix = "concept/"
)

// resolveSubjectsViaOpenAlex returns namespaced subject tags for a DOI, keeping
// only concepts OpenAlex is confident about, strongest first.
func resolveSubjectsViaOpenAlex(ctx context.Context, httpClient *http.Client, doi, email string) ([]string, bool, error) {
	var resp openAlexSearchResponse
	found, err := getProviderJSON(ctx, httpClient, "OpenAlex", openAlexWorksURL("doi:"+openAlexFilterLiteral(strings.ToLower(doi)), email), &resp)
	if err != nil || !found {
		return nil, false, err
	}
	if len(resp.Results) == 0 {
		return nil, false, nil
	}
	concepts := append([]openAlexConcept(nil), resp.Results[0].Concepts...)
	// OpenAlex documents no ordering guarantee, and the cap keeps only the
	// leading entries, so sort before truncating or the cap would drop
	// stronger terms in favour of whatever arrived first.
	sort.SliceStable(concepts, func(i, j int) bool { return concepts[i].Score > concepts[j].Score })
	tags := make([]string, 0, maxEnrichSubjectTags)
	seen := make(map[string]bool, maxEnrichSubjectTags)
	for _, concept := range concepts {
		if concept.Score < minOpenAlexConceptScore {
			continue
		}
		name := strings.TrimSpace(concept.DisplayName)
		if name == "" {
			continue
		}
		tag := enrichSubjectTagPrefix + name
		if seen[tag] {
			continue
		}
		seen[tag] = true
		tags = append(tags, tag)
		if len(tags) == maxEnrichSubjectTags {
			break
		}
	}
	if len(tags) == 0 {
		return nil, false, nil
	}
	return tags, true, nil
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

// httpStatusError is getJSON's error for a non-2xx answer, kept typed so a
// caller can tell a missing record (404) from a provider failure.
type httpStatusError struct{ Code int }

func (e *httpStatusError) Error() string { return fmt.Sprintf("HTTP %d", e.Code) }

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
		return &httpStatusError{Code: resp.StatusCode}
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
		// Keep any Unicode letter or number so CJK/Cyrillic/Arabic titles
		// normalize to meaningful content instead of the empty string.
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
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
