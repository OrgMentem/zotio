// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const maxRetractionProviderResponseBytes = 4 << 20

// crossrefRetractionBaseURL is overridable for retraction lookups.
var crossrefRetractionBaseURL = "https://api.crossref.org"

type retractionCheckFinding struct {
	ItemKey    string `json:"item_key"`
	Title      string `json:"title"`
	DOI        string `json:"doi"`
	Status     string `json:"status"`
	UpdateType string `json:"update_type"`
	Label      string `json:"label,omitempty"`
	NoticeDOI  string `json:"notice_doi,omitempty"`
	UpdateDate string `json:"update_date,omitempty"`
	Source     string `json:"source,omitempty"`
}

type retractionCheckError struct {
	ItemKey string `json:"item_key"`
	DOI     string `json:"doi"`
	Error   string `json:"error"`
}

type retractionCheckSummary struct {
	Checked      int `json:"checked"`
	Flagged      int `json:"flagged"`
	Unregistered int `json:"unregistered"`
	Errors       int `json:"errors,omitempty"`
}

type retractionCheckReport struct {
	Findings []retractionCheckFinding `json:"findings"`
	Summary  retractionCheckSummary   `json:"summary"`
	Errors   []retractionCheckError   `json:"errors,omitempty"`
}

type crossrefRetractionResponse struct {
	Message struct {
		UpdatedBy []crossrefUpdateNotice `json:"updated-by"`
	} `json:"message"`
}

type crossrefUpdateNotice struct {
	DOI     string `json:"DOI"`
	Type    string `json:"type"`
	Label   string `json:"label"`
	Source  string `json:"source"`
	Updated struct {
		DateParts [][]int `json:"date-parts"`
	} `json:"updated"`
}

// newItemsRetractCheckCmd registers read-only DOI retraction checking under items.
func newItemsRetractCheckCmd(flags *rootFlags) *cobra.Command {
	var flagLimit int
	var flagCollection string

	cmd := &cobra.Command{
		Use:   "retract-check",
		Short: "Check DOI-bearing local items for CrossRef retraction notices",
		Long: `Check DOI-bearing items from the locally synced store against CrossRef's
updated-by metadata. The command reports retractions, expressions of concern,
and corrections without writing to Zotero. Run 'zotio sync' first so the local
store is current.`,
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			rawDB, err := openStoreForRead(cmd.Context(), "zotio")
			if err != nil {
				return fmt.Errorf("opening database: %w", err)
			}
			if rawDB == nil {
				fmt.Fprintln(cmd.OutOrStdout(), "Run 'zotio sync' first.")
				return nil
			}
			defer rawDB.Close()

			report, err := runRetractionCheck(cmd.Context(), localQueryStore{rawDB}, &http.Client{Timeout: enrichTimeout(flags.timeout)}, flagLimit, flagCollection)
			if err != nil {
				return err
			}
			return renderRetractionCheckReport(cmd, flags, report)
		},
	}
	cmd.Flags().IntVar(&flagLimit, "limit", 0, "Maximum DOI-bearing items to check (0 = all)")
	cmd.Flags().StringVar(&flagCollection, "collection", "", "Scope checks to items in a collection key")

	return cmd
}

// queryRetractionCheckItems reuses the enrichment validation selector for DOI-bearing local items.
func queryRetractionCheckItems(db localQueryStore, limit int, collection string) ([]map[string]any, error) {
	return queryEnrichValidationItems(db, limit, collection)
}

// runRetractionCheck scans DOI-bearing rows while preserving the report-only exit contract.
func runRetractionCheck(ctx context.Context, db localQueryStore, httpClient *http.Client, limit int, collection string) (retractionCheckReport, error) {
	report := retractionCheckReport{Findings: []retractionCheckFinding{}}
	rows, err := queryRetractionCheckItems(db, limit, collection)
	if err != nil {
		return report, fmt.Errorf("querying DOI-bearing items: %w", err)
	}

	calls := 0
	for _, row := range rows {
		key := sqlStringValue(row["key"])
		title := sqlStringValue(row["title"])
		doi := normalizeDOI(sqlStringValue(row["doi"]))
		if doi == "" {
			continue
		}
		if calls > 0 {
			if err := sleepWithContext(ctx, 200*time.Millisecond); err != nil {
				return report, err
			}
		}
		calls++
		report.Summary.Checked++

		notices, registered, err := lookupCrossrefRetractionNotices(ctx, httpClient, doi)
		if err != nil {
			report.Errors = append(report.Errors, retractionCheckError{ItemKey: key, DOI: doi, Error: err.Error()})
			report.Summary.Errors = len(report.Errors)
			continue
		}
		if !registered {
			report.Summary.Unregistered++
			continue
		}
		for _, notice := range notices {
			report.Findings = append(report.Findings, retractionCheckFinding{
				ItemKey:    key,
				Title:      title,
				DOI:        doi,
				Status:     classifyCrossrefUpdateStatus(notice.Type),
				UpdateType: strings.TrimSpace(notice.Type),
				Label:      strings.TrimSpace(notice.Label),
				NoticeDOI:  normalizeDOI(notice.DOI),
				UpdateDate: crossrefDatePartsString(notice.Updated.DateParts),
				Source:     strings.TrimSpace(notice.Source),
			})
		}
	}
	report.Summary.Flagged = len(report.Findings)
	return report, nil
}

// lookupCrossrefRetractionNotices makes CrossRef DOI lookups context-aware, capped, and testable by base URL.
func lookupCrossrefRetractionNotices(ctx context.Context, httpClient *http.Client, doi string) ([]crossrefUpdateNotice, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, crossrefRetractionWorksURL(doi), nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Accept", crossrefContentType)
	req.Header.Set("User-Agent", crossrefUserAgent)

	resp, err := sameOriginExternalFetchHTTPClient(httpClient, false).Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("querying CrossRef for DOI %s: %w", doi, err)
	}
	defer resp.Body.Close()
	body, err := readCappedExternalBody(resp.Body, maxRetractionProviderResponseBytes)
	if err != nil {
		return nil, false, fmt.Errorf("reading CrossRef response for DOI %s: %w", doi, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, false, fmt.Errorf("CrossRef lookup for DOI %s returned HTTP %d: %s", doi, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var decoded crossrefRetractionResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, false, fmt.Errorf("parsing CrossRef response for DOI %s: %w", doi, err)
	}
	return decoded.Message.UpdatedBy, true, nil
}

// probeCrossrefRetractionAPI probes CrossRef separately so health skips loudly on network preconditions.
func probeCrossrefRetractionAPI(ctx context.Context, httpClient *http.Client) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(crossrefRetractionBaseURL, "/")+"/works?rows=0", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", crossrefContentType)
	req.Header.Set("User-Agent", crossrefUserAgent)
	resp, err := sameOriginExternalFetchHTTPClient(httpClient, false).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := readCappedExternalBody(resp.Body, maxRetractionProviderResponseBytes)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// crossrefRetractionWorksURL builds CrossRef work URLs from the overridable base.
func crossrefRetractionWorksURL(doi string) string {
	return strings.TrimRight(crossrefRetractionBaseURL, "/") + "/works/" + url.PathEscape(doi)
}

// classifyCrossrefUpdateStatus collapses CrossRef update types into user-facing statuses.
func classifyCrossrefUpdateStatus(updateType string) string {
	lower := strings.ToLower(strings.TrimSpace(updateType))
	switch {
	case lower == "retraction":
		return "retracted"
	case lower == "expression_of_concern" || strings.Contains(lower, "concern"):
		return "concern"
	default:
		return "correction"
	}
}

// crossrefDatePartsString renders CrossRef date-parts without inventing missing precision.
func crossrefDatePartsString(parts [][]int) string {
	if len(parts) == 0 || len(parts[0]) == 0 {
		return ""
	}
	date := parts[0]
	switch len(date) {
	case 1:
		return fmt.Sprintf("%04d", date[0])
	case 2:
		return fmt.Sprintf("%04d-%02d", date[0], date[1])
	default:
		return fmt.Sprintf("%04d-%02d-%02d", date[0], date[1], date[2])
	}
}

// sleepWithContext keeps the mandated CrossRef pacing cancellable.
func sleepWithContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// renderRetractionCheckReport renders table output for humans and JSON for machine modes.
func renderRetractionCheckReport(cmd *cobra.Command, flags *rootFlags, report retractionCheckReport) error {
	if wantsHumanTable(cmd.OutOrStdout(), flags) {
		if len(report.Findings) > 0 {
			rows := make([][]string, 0, len(report.Findings))
			for _, f := range report.Findings {
				rows = append(rows, []string{f.ItemKey, f.Status, f.UpdateType, f.NoticeDOI, f.UpdateDate, f.Source, f.Title})
			}
			if err := flags.printTable(cmd, []string{"KEY", "STATUS", "TYPE", "NOTICE DOI", "DATE", "SOURCE", "TITLE"}, rows); err != nil {
				return err
			}
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Checked %d DOI-bearing items; %d update notices flagged; %d DOI(s) not registered with CrossRef.\n", report.Summary.Checked, report.Summary.Flagged, report.Summary.Unregistered)
		if report.Summary.Errors > 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "%d CrossRef lookup error(s) were recorded; rerun with --json for details.\n", report.Summary.Errors)
		}
		return nil
	}
	data, err := json.Marshal(report)
	if err != nil {
		return err
	}
	return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), flags)
}
