// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	importMonitorPageSize = 100
	importMonitorMaxPages = 20 // Scan at most 2,000 works per author, even if the provider cursor keeps advancing.
)

var (
	monitorORCID          = regexp.MustCompile(`^\d{4}-\d{4}-\d{4}-[\dX]{4}$`)
	monitorOpenAlexAuthor = regexp.MustCompile(`^A\d+$`)
)

type importMonitorReport struct {
	Out                          string `json:"out"`
	WorksSeen                    int    `json:"works_seen"`
	Emitted                      int    `json:"emitted"`
	SkippedAlreadyInLibraryDOI   int    `json:"skipped_already_in_library_doi"`
	SkippedAlreadyInLibraryTitle int    `json:"skipped_already_in_library_title"`
	SkippedWithoutIdentifier     int    `json:"skipped_without_identifier"`
	Truncated                    bool   `json:"truncated"`
	TruncationReason             string `json:"truncation_reason,omitempty"`
}

type monitorAuthor struct {
	input  string
	filter string
}

type monitorWork struct {
	ID              string `json:"id"`
	DOI             string `json:"doi"`
	Title           string `json:"title"`
	PublicationDate string `json:"publication_date"`
}

type monitorWorksPage struct {
	Meta struct {
		NextCursor string `json:"next_cursor"`
	} `json:"meta"`
	Results []monitorWork `json:"results"`
}

type monitorCandidate struct {
	work    monitorWork
	authors []string
}

func newImportMonitorCmd(flags *rootFlags) *cobra.Command {
	var authorInputs []string
	var query, since, until, out string
	var limit int
	cmd := &cobra.Command{
		Use:   "monitor",
		Short: "Find new author or topic works and write a reviewable import manifest",
		Long: "Search OpenAlex for works published since a date, then exclude works already in the synced library " +
			"(by DOI, then by normalized title). The result is a reviewable import manifest; nothing is written to Zotero.\n\n" +
			"There is no saved state. Run this command on a schedule with an OS scheduler or a workflow spec. " +
			"Set --since a few weeks before the last run to overlap search windows. " +
			"Library de-duplication removes works you already imported. " +
			"OpenAlex publication-date filters cannot reliably find newly indexed or backdated works, so this is not a complete feed of newly indexed records. " +
			"Review the manifest with import resolve, then write it with import apply. " +
			"Works without a DOI are counted but omitted because import resolve needs a DOI to fetch their metadata.",
		Example: "  zotio import monitor --author 0000-0002-1825-0097 --since 2026-01-01 --out new-works.json\n" +
			"  zotio import monitor --query \"bayesian inference\" --author A5023888391 --since 2026-09-01 --out feed.json\n" +
			"  zotio import resolve new-works.json > reviewed.json && zotio import apply reviewed.json",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if len(authorInputs) == 0 && strings.TrimSpace(query) == "" {
				return usageErr(fmt.Errorf("at least one of --author or --query is required"))
			}
			if strings.TrimSpace(out) == "" {
				return usageErr(fmt.Errorf("--out is required"))
			}
			if limit < 1 {
				return usageErr(fmt.Errorf("--limit must be >= 1"))
			}
			if err := monitorDate(since); err != nil {
				return usageErr(fmt.Errorf("--since is required in YYYY-MM-DD format: %w", err))
			}
			if until != "" {
				if err := monitorDate(until); err != nil {
					return usageErr(fmt.Errorf("--until must be YYYY-MM-DD: %w", err))
				}
				if until < since {
					return usageErr(fmt.Errorf("--until must not precede --since"))
				}
			}
			authors := make([]monitorAuthor, 0, len(authorInputs))
			for _, input := range authorInputs {
				author, err := parseMonitorAuthor(input)
				if err != nil {
					return usageErr(err)
				}
				authors = append(authors, author)
			}
			lockPath, target, err := outputWriterLockPath(out)
			if err != nil {
				return fmt.Errorf("resolving manifest output: %w", err)
			}
			return withPathWriterLock(cmd, lockPath, "import monitor", func() error {
				manifest, report, err := buildImportMonitorManifest(cmd.Context(), flags, authors, strings.TrimSpace(query), since, until, limit, out)
				if err != nil {
					return err
				}
				if err := withAtomicOutputFile(target, 0o600, func(w io.Writer) error {
					return writeImportManifest(w, manifest)
				}); err != nil {
					return fmt.Errorf("writing manifest: %w", err)
				}
				if flags.asJSON {
					return printCommandJSON(cmd.OutOrStdout(), report, flags)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Wrote %d manifest entries to %s\n", report.Emitted, out)
				fmt.Fprintf(cmd.OutOrStdout(), "Works seen=%d; already in library (DOI)=%d; already in library (title)=%d; without DOI=%d; truncated=%t\n", report.WorksSeen, report.SkippedAlreadyInLibraryDOI, report.SkippedAlreadyInLibraryTitle, report.SkippedWithoutIdentifier, report.Truncated)
				if report.Truncated {
					fmt.Fprintf(cmd.OutOrStdout(), "Truncation: %s\n", report.TruncationReason)
				}
				return nil
			})
		},
	}
	cmd.Flags().StringArrayVar(&authorInputs, "author", nil, "Author ORCID or OpenAlex author ID (or their https URL); repeat to match any author")
	cmd.Flags().StringVar(&query, "query", "", "Free-text topic search; with --author, match both")
	cmd.Flags().StringVar(&since, "since", "", "Required publication date lower bound (YYYY-MM-DD, inclusive)")
	cmd.Flags().StringVar(&until, "until", "", "Publication date upper bound (YYYY-MM-DD, inclusive)")
	cmd.Flags().IntVar(&limit, "limit", 25, "Maximum manifest entries to write")
	cmd.Flags().StringVar(&out, "out", "", "Required reviewable import manifest output path")
	return cmd
}

func monitorDate(value string) error {
	date, err := time.Parse("2006-01-02", value)
	if err != nil || date.Format("2006-01-02") != value {
		return fmt.Errorf("invalid date %q", value)
	}
	return nil
}

func parseMonitorAuthor(input string) (monitorAuthor, error) {
	value := strings.TrimSpace(input)
	if strings.HasPrefix(value, "https://orcid.org/") {
		value = strings.TrimPrefix(value, "https://orcid.org/")
		if monitorORCID.MatchString(value) {
			return monitorAuthor{input: "https://orcid.org/" + value, filter: "author.orcid:https://orcid.org/" + value}, nil
		}
	} else if strings.HasPrefix(value, "https://openalex.org/") {
		value = strings.TrimPrefix(value, "https://openalex.org/")
		if monitorOpenAlexAuthor.MatchString(value) {
			return monitorAuthor{input: "https://openalex.org/" + value, filter: "authorships.author.id:https://openalex.org/" + value}, nil
		}
	} else if monitorORCID.MatchString(value) {
		return monitorAuthor{input: "https://orcid.org/" + value, filter: "author.orcid:https://orcid.org/" + value}, nil
	} else if monitorOpenAlexAuthor.MatchString(value) {
		return monitorAuthor{input: "https://openalex.org/" + value, filter: "authorships.author.id:https://openalex.org/" + value}, nil
	}
	return monitorAuthor{}, fmt.Errorf("invalid --author %q: use an ORCID (0000-0002-1825-0097 or https://orcid.org/...) or an OpenAlex author ID (A5023888391 or https://openalex.org/...)", input)
}

func buildImportMonitorManifest(ctx context.Context, flags *rootFlags, authors []monitorAuthor, query, since, until string, limit int, out string) (importManifest, importMonitorReport, error) {
	manifest := importManifest{SchemaVersion: importManifestSchemaVersion, Entries: []importManifestEntry{}}
	report := importMonitorReport{Out: out}
	rawDB, err := openStoreForRead(ctx, "zotio")
	if err != nil {
		return manifest, report, fmt.Errorf("opening local store: %w", err)
	}
	if rawDB == nil {
		return manifest, report, preconditionErr(fmt.Errorf("run 'zotio sync' first to enable literature monitoring"))
	}
	defer rawDB.Close()
	libraryDOIs, err := buildLibraryDOIIndex(ctx, rawDB)
	if err != nil {
		return manifest, report, fmt.Errorf("indexing library DOIs: %w", err)
	}
	libraryTitles, err := queryLibraryTitleSet(localQueryStore{rawDB})
	if err != nil {
		return manifest, report, fmt.Errorf("indexing library titles: %w", err)
	}

	client := &http.Client{Timeout: enrichTimeout(flags.timeout)}
	if len(authors) == 0 {
		authors = []monitorAuthor{{}}
	}
	candidates := map[string]*monitorCandidate{}
	pageCapReached := false
	for _, author := range authors {
		cursor := "*"
		for pageNum := range importMonitorMaxPages {
			filters := []string{"from_publication_date:" + since}
			if until != "" {
				filters = append(filters, "to_publication_date:"+until)
			}
			if author.filter != "" {
				filters = append(filters, author.filter)
			}
			v := url.Values{
				"filter":   {strings.Join(filters, ",")},
				"sort":     {"publication_date:desc"},
				"cursor":   {cursor},
				"per_page": {fmt.Sprintf("%d", importMonitorPageSize)},
				"select":   {"id,doi,title,publication_date"},
			}
			if query != "" {
				v.Set("search", query)
			}
			if email := enrichUnpaywallEmail(); email != "" {
				v.Set("mailto", email)
			}
			var response monitorWorksPage
			// A scheduled feed must see newly indexed works on every run. The
			// seven-day provider cache is suitable for metadata lookups, not
			// for a changing /works listing, so bypass both cache reads and writes.
			if err := getCappedProviderJSON(ctx, client, providerOpenAlex, enrichOpenAlexBase+"/works?"+v.Encode(), nil, &response); err != nil {
				return manifest, report, apiErr(fmt.Errorf("fetching OpenAlex works: %w", err))
			}
			if response.Results == nil {
				return manifest, report, apiErr(fmt.Errorf("fetching OpenAlex works: response has no results array"))
			}
			for _, work := range response.Results {
				if work.ID == "" || monitorDate(work.PublicationDate) != nil {
					return manifest, report, apiErr(fmt.Errorf("fetching OpenAlex works: invalid work ID or publication date"))
				}
				if work.PublicationDate < since || (until != "" && work.PublicationDate > until) {
					continue
				}
				candidate := candidates[work.ID]
				if candidate == nil {
					candidate = &monitorCandidate{work: work}
					candidates[work.ID] = candidate
				}
				if author.input != "" && !containsString(candidate.authors, author.input) {
					candidate.authors = append(candidate.authors, author.input)
				}
			}
			next := strings.TrimSpace(response.Meta.NextCursor)
			if next == "" {
				break
			}
			if len(response.Results) == 0 || next == cursor {
				pageCapReached = true
				break
			}
			if pageNum == importMonitorMaxPages-1 {
				pageCapReached = true
				break
			}
			cursor = next
		}
	}
	works := make([]*monitorCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		works = append(works, candidate)
	}
	sort.Slice(works, func(i, j int) bool {
		if works[i].work.PublicationDate != works[j].work.PublicationDate {
			return works[i].work.PublicationDate > works[j].work.PublicationDate
		}
		return works[i].work.ID < works[j].work.ID
	})
	report.WorksSeen = len(works)
	seenDOIs := map[string]bool{}
	seenTitles := map[string]bool{}
	for _, candidate := range works {
		work := candidate.work
		doi := normalizedGapDOI(work.DOI)
		title := normalizeExactTitle(work.Title)
		if doi != "" && libraryDOIs.byDOI[doi].key != "" {
			report.SkippedAlreadyInLibraryDOI++
			continue
		}
		if title != "" && libraryTitles[title] {
			report.SkippedAlreadyInLibraryTitle++
			continue
		}
		// An OpenAlex work ID is not an identifier import resolve can fetch.
		if doi == "" {
			report.SkippedWithoutIdentifier++
			continue
		}
		if seenDOIs[doi] || (title != "" && seenTitles[title]) {
			continue
		}
		seenDOIs[doi] = true
		if title != "" {
			seenTitles[title] = true
		}
		if len(manifest.Entries) >= limit {
			report.Truncated = true
			report.TruncationReason = "--limit reached; more new works are available"
			continue
		}
		manifest.Entries = append(manifest.Entries, importManifestEntry{
			Path: "", Classification: "new", Action: "create", IdentifierType: "doi", Identifier: doi,
			Title: work.Title, Status: "unresolved",
			Discovery: &importDiscovery{
				Provider: providerOpenAlex, Mode: "monitor", Authors: candidate.authors,
				Query: query, Since: since, Until: until, OpenAlexWorkID: work.ID, PublicationDate: work.PublicationDate,
			},
		})
	}
	report.Emitted = len(manifest.Entries)
	if pageCapReached {
		if report.Truncated {
			report.TruncationReason += "; OpenAlex page cap or stalled cursor reached; more works may be available"
		} else {
			report.Truncated = true
			report.TruncationReason = "OpenAlex page cap or stalled cursor reached; more works may be available"
		}
	}
	return manifest, report, nil
}
