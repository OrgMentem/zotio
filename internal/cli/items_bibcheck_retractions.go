// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Gate manuscripts on retracted or concern-flagged cited works.

package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// bibcheckRetractedKind and bibcheckConcernKind name the findings that
// --check-retractions adds. retracted_item is the kind library health already
// uses for a CrossRef retraction, so one name means one thing across commands.
// Health folds an expression of concern into retracted_item at critical;
// bibcheck keeps the two apart because a concern is a warning about a paper
// the authors may still defend, while a retraction withdraws it.
const (
	bibcheckRetractedKind = "retracted_item"
	bibcheckConcernKind   = "expression_of_concern"
)

// bibcheckItemResolver maps one manuscript citation occurrence to the library
// item it cites. ok is false for an occurrence that resolves to no item or to
// several, which the retraction check leaves alone: only a citation bibcheck
// can pin to exactly one item is looked up.
type bibcheckItemResolver func(bibcheckOccurrence) (itemKey string, ok bool)

// bibcheckCiteKeyResolver resolves an occurrence by its citekey with the rule
// bibcheck reports: a key held by exactly one item resolves.
func bibcheckCiteKeyResolver(items []citekeyItem) bibcheckItemResolver {
	byCiteKey := bibcheckItemsByCiteKey(items)
	return func(o bibcheckOccurrence) (string, bool) {
		matches := byCiteKey[o.CiteKey]
		if len(matches) != 1 {
			return "", false
		}
		return matches[0].Key, true
	}
}

// bibcheckCitedItem is one library item the manuscript cites, with every
// manuscript location that cites it.
type bibcheckCitedItem struct {
	ItemKey   string
	CiteKeys  []string
	Locations []bibcheckLocation
}

// collectBibcheckCitedItems groups resolved occurrences by item, in
// first-citation order. Two citekeys (or a citekey and a URI) that name the
// same item share one entry, so the item costs one CrossRef request and its
// finding lists every place the manuscript cites it.
func collectBibcheckCitedItems(occurrences []bibcheckOccurrence, resolve bibcheckItemResolver) []*bibcheckCitedItem {
	byKey := make(map[string]*bibcheckCitedItem)
	var order []*bibcheckCitedItem
	seenCiteKey := make(map[string]map[string]bool)
	seenLocation := make(map[string]map[bibcheckLocation]bool)
	for _, occurrence := range occurrences {
		itemKey, ok := resolve(occurrence)
		if !ok || itemKey == "" {
			continue
		}
		cited := byKey[itemKey]
		if cited == nil {
			cited = &bibcheckCitedItem{ItemKey: itemKey}
			byKey[itemKey] = cited
			order = append(order, cited)
			seenCiteKey[itemKey] = make(map[string]bool)
			seenLocation[itemKey] = make(map[bibcheckLocation]bool)
		}
		if occurrence.CiteKey != "" && !seenCiteKey[itemKey][occurrence.CiteKey] {
			seenCiteKey[itemKey][occurrence.CiteKey] = true
			cited.CiteKeys = append(cited.CiteKeys, occurrence.CiteKey)
		}
		if occurrence.File != "" {
			loc := bibcheckLocation{File: occurrence.File, Line: occurrence.Line}
			if !seenLocation[itemKey][loc] {
				seenLocation[itemKey][loc] = true
				cited.Locations = append(cited.Locations, loc)
			}
		}
	}
	return order
}

// bibcheckRetractionSummary says what the retraction check covered, so a
// report with no retraction findings can be told apart from one that never
// looked. Checked counts lookups CrossRef answered; Unchecked counts cited
// items with no answer, whether for want of a DOI or because the lookup failed.
type bibcheckRetractionSummary struct {
	CitedItems   int `json:"cited_items"`
	Checked      int `json:"checked"`
	Flagged      int `json:"flagged"`
	Unregistered int `json:"unregistered,omitempty"`
	Unchecked    int `json:"unchecked,omitempty"`
	Errors       int `json:"errors,omitempty"`
}

type bibcheckRetractionResult struct {
	Findings []Finding
	Summary  bibcheckRetractionSummary
	Skipped  []healthSkip
}

// runBibcheckRetractionCheck looks up exactly the DOI-bearing items among
// cited, through the same CrossRef client and per-DOI lookup that library
// health and items retract-check use. Nothing is cached: a one-shot run has no
// earlier answer to fall back on, so a failed lookup is a skip, never a pass.
//
// A CrossRef lookup that fails is reported in Skipped and does not discard the
// lookups that succeeded; the caller treats a skip as "cannot certify".
func runBibcheckRetractionCheck(ctx context.Context, flags *rootFlags, db localQueryStore, cited []*bibcheckCitedItem, src FindingSource) (bibcheckRetractionResult, error) {
	result := bibcheckRetractionResult{Summary: bibcheckRetractionSummary{CitedItems: len(cited)}}
	if len(cited) == 0 {
		return result, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	citedByKey := make(map[string]*bibcheckCitedItem, len(cited))
	keys := make(map[string]bool, len(cited))
	for _, item := range cited {
		citedByKey[item.ItemKey] = item
		keys[item.ItemKey] = true
	}

	// Decide whether there is anything to ask before touching the network: a
	// manuscript whose cited items carry no DOI must not need CrossRef.
	rows, err := queryRetractionCheckItems(db, 0, "")
	if err != nil {
		return result, fmt.Errorf("querying DOI-bearing items: %w", err)
	}
	withDOI := 0
	for _, row := range rows {
		if keys[sqlStringValue(row["key"])] && normalizeDOI(sqlStringValue(row["doi"])) != "" {
			withDOI++
		}
	}
	if withDOI == 0 {
		result.Summary.Unchecked = len(cited)
		return result, nil
	}

	timeout := enrichTimeout(0)
	if flags != nil {
		timeout = enrichTimeout(flags.timeout)
	}
	httpClient := &http.Client{Timeout: timeout}
	if err := probeCrossrefRetractionAPI(ctx, httpClient); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return result, ctxErr
		}
		result.Summary.Unchecked = len(cited)
		result.Skipped = append(result.Skipped, healthSkip{
			Kind:         bibcheckRetractedKind,
			Precondition: "external_crossref",
			Detail:       fmt.Sprintf("CrossRef retraction checking is not reachable (%s); the %d cited DOI(s) were not checked, so this report cannot certify that no cited work is retracted.", err, withDOI),
			Remediation: []healthRemediation{
				{Action: "retry_check_retractions", Text: "Re-run items bibcheck with --check-retractions after network access is restored."},
			},
		})
		return result, nil
	}

	report, err := runRetractionCheckCached(ctx, db, httpClient, 0, "", nil, keys)
	if err != nil {
		return result, err
	}
	// report.Summary.Checked counts every DOI asked about, failed lookups
	// included. Checked here means answered: a failed lookup is not checked.
	result.Summary.Checked = report.Summary.Checked - report.Summary.Errors
	result.Summary.Unregistered = report.Summary.Unregistered
	result.Summary.Errors = report.Summary.Errors
	result.Summary.Unchecked = len(cited) - result.Summary.Checked
	if report.Summary.Errors > 0 {
		details := make([]string, 0, len(report.Errors))
		for _, e := range report.Errors {
			details = append(details, fmt.Sprintf("%s (%s): %s", e.ItemKey, e.DOI, e.Error))
		}
		result.Skipped = append(result.Skipped, healthSkip{
			Kind:         bibcheckRetractedKind,
			Precondition: "external_crossref",
			Detail:       fmt.Sprintf("CrossRef retraction checking encountered %d lookup error(s); those cited works were not checked and cannot be certified: %s", report.Summary.Errors, strings.Join(details, "; ")),
			Remediation: []healthRemediation{
				{Action: "retry_check_retractions", Text: "Re-run items bibcheck with --check-retractions to retry the failed lookups."},
			},
		})
	}

	for _, row := range report.Findings {
		finding, ok := bibcheckRetractionFinding(row, citedByKey[row.ItemKey], src)
		if ok {
			result.Findings = append(result.Findings, finding)
		}
	}
	result.Summary.Flagged = len(result.Findings)
	return result, nil
}

// bibcheckRetractionFinding turns one CrossRef notice into a finding carrying
// the manuscript locations that cite the item. A retraction is critical and an
// expression of concern high; a correction is informational and never a finding,
// as in library health.
func bibcheckRetractionFinding(row retractionCheckFinding, cited *bibcheckCitedItem, src FindingSource) (Finding, bool) {
	var kind, severity, label string
	switch row.Status {
	case "retracted":
		kind, severity, label = bibcheckRetractedKind, sevCritical, "Retracted work cited"
	case "concern":
		kind, severity, label = bibcheckConcernKind, sevHigh, "Cited work has an expression of concern"
	default:
		return Finding{}, false
	}

	var citeKeys []string
	var locations []bibcheckLocation
	if cited != nil {
		citeKeys, locations = cited.CiteKeys, cited.Locations
	}
	citeKey := ""
	if len(citeKeys) > 0 {
		citeKey = citeKeys[0]
	}
	evidence := bibcheckEvidence(citeKey, locations, nil, "")
	if len(citeKeys) > 0 {
		evidence["citekeys"] = citeKeys
	}
	evidence["doi"] = row.DOI
	evidence["status"] = row.Status
	evidence["update_type"] = row.UpdateType
	evidence["notice_doi"] = row.NoticeDOI
	evidence["date"] = row.UpdateDate
	evidence["source"] = row.Source
	evidence["label"] = row.Label

	title := label
	if row.Title != "" {
		title = fmt.Sprintf("%s: %s", label, row.Title)
	}
	return Finding{
		Kind:              kind,
		Severity:          severity,
		ItemKey:           row.ItemKey,
		Title:             title,
		Evidence:          evidence,
		Source:            src,
		RecommendedAction: &RecommendedAction{Text: "Replace or remove the citation in the manuscript, or cite the notice explicitly", Command: "zotio items retract-check --json"},
	}, true
}

// printBibcheckRetractionStatus renders the retraction check's coverage and
// every skip, so a lookup that did not run is never read as an all-clear.
func printBibcheckRetractionStatus(out io.Writer, report bibcheckReport) {
	if report.Retractions == nil {
		return
	}
	s := report.Retractions
	fmt.Fprintf(out, "\nRetraction check: %d cited item(s), %d checked, %d flagged", s.CitedItems, s.Checked, s.Flagged)
	if s.Unchecked > 0 {
		fmt.Fprintf(out, ", %d not checked", s.Unchecked)
	}
	if s.Errors > 0 {
		fmt.Fprintf(out, ", %d lookup error(s)", s.Errors)
	}
	fmt.Fprintln(out)
	for _, skip := range report.Skipped {
		fmt.Fprintf(out, "  SKIPPED %s (%s) — %s\n", skip.Kind, skip.Precondition, sanitizeForTerminal(skip.Detail))
		for _, r := range skip.Remediation {
			switch {
			case r.Command != "":
				fmt.Fprintf(out, "    Fix: %s\n", r.Command)
			case r.Text != "":
				fmt.Fprintf(out, "    Fix: %s\n", r.Text)
			}
		}
	}
}

// printBibcheckRetractionFinding renders one retraction or concern finding as
// a line in the findings list. Titles and DOIs come from the library and the
// locations from the manuscript, so every cell is sanitized.
func printBibcheckRetractionFinding(out io.Writer, finding Finding) {
	citeKey, _ := finding.Evidence["citekey"].(string)
	doi, _ := finding.Evidence["doi"].(string)
	status, _ := finding.Evidence["status"].(string)
	target := sanitizeForTerminal(citeKey)
	if target == "" {
		target = sanitizeForTerminal(finding.ItemKey)
	}
	fmt.Fprintf(out, "  %s %s %s (%s, doi %s) — %s\n", finding.Kind, finding.Severity, target, sanitizeForTerminal(status), sanitizeForTerminal(doi), sanitizeForTerminal(formatBibcheckLocations(finding.Evidence["locations"])))
}
