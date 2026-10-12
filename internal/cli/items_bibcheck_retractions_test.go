// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// bibcheckCrossrefStub is a CrossRef double that records every request, so a
// test can assert not only what bibcheck reported but what it asked for.
type bibcheckCrossrefStub struct {
	mu         sync.Mutex
	probes     int
	works      []string
	notices    map[string]string // DOI -> JSON array body for updated-by
	fail       map[string]bool   // DOI -> answer HTTP 500
	probeFails bool
}

func newBibcheckCrossrefStub(t *testing.T) *bibcheckCrossrefStub {
	t.Helper()
	stub := &bibcheckCrossrefStub{notices: map[string]string{}, fail: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.mu.Lock()
		defer stub.mu.Unlock()
		if r.URL.Path == "/works" && r.URL.Query().Get("rows") == "0" {
			stub.probes++
			if stub.probeFails {
				http.Error(w, "CrossRef is down", http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte(`{"message":{}}`))
			return
		}
		escaped := strings.TrimPrefix(r.URL.EscapedPath(), "/works/")
		doi, err := url.PathUnescape(escaped)
		if err != nil || escaped == r.URL.EscapedPath() {
			http.Error(w, "unexpected CrossRef request", http.StatusNotFound)
			t.Errorf("unexpected CrossRef request %q", r.URL.String())
			return
		}
		stub.works = append(stub.works, doi)
		if stub.fail[doi] {
			http.Error(w, "upstream exploded", http.StatusInternalServerError)
			return
		}
		updatedBy, ok := stub.notices[doi]
		if !ok {
			updatedBy = `[]`
		}
		_, _ = fmt.Fprintf(w, `{"message":{"updated-by":%s}}`, updatedBy)
	}))
	t.Cleanup(srv.Close)
	withBase(t, &crossrefRetractionBaseURL, srv.URL)
	return stub
}

func (s *bibcheckCrossrefStub) requested() (probes int, works []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.probes, append([]string(nil), s.works...)
}

const bibcheckRetractionNotice = `[{"DOI":"10.777/notice","type":"retraction","label":"Retracted","source":"publisher","updated":{"date-parts":[[2025,1,15]]}}]`

const bibcheckCorrectionNotice = `[{"DOI":"10.777/correction","type":"correction","label":"Correction","source":"publisher","updated":{"date-parts":[[2022]]}}]`

const bibcheckConcernAndCorrectionNotice = `[{"DOI":"10.777/correction","type":"correction","label":"Correction","source":"publisher","updated":{"date-parts":[[2022]]}},{"DOI":"10.777/concern","type":"expression_of_concern","label":"Expression of concern","source":"crossmark","updated":{"date-parts":[[2024,3]]}}]`

// bibcheckRetractionItem builds a complete journal article so the only
// findings a test sees are the ones it is about.
func bibcheckRetractionItem(key, citeKey, doi string) json.RawMessage {
	doiField := ""
	if doi != "" {
		doiField = fmt.Sprintf(`"DOI":%q,`, doi)
	}
	return json.RawMessage(fmt.Sprintf(`{"key":%q,"version":1,"data":{"key":%q,"itemType":"journalArticle","title":"Work %s","creators":[{"lastName":"Doe"}],"date":"2024","publicationTitle":"Journal A",%s"extra":"Citation Key: %s"}}`, key, key, key, doiField, citeKey))
}

func bibcheckFindingsOfKind(report bibcheckReport, kind string) []Finding {
	var out []Finding
	for _, f := range report.Findings {
		if f.Kind == kind {
			out = append(out, f)
		}
	}
	return out
}

// A cited DOI with a CrossRef retraction is a critical retracted_item at the
// line of the \cite, --fail-on high exits 11, and only the cited items with a
// DOI are looked up: the uncited retracted item in the library is never asked
// about.
func TestBibcheckCheckRetractionsFlagsCitedRetractionAtCiteLine(t *testing.T) {
	home := bibcheckIsolatedHome(t)
	seedBibcheckItems(t, []json.RawMessage{
		bibcheckRetractionItem("CITED1", "alpha", "10.777/retracted"),
		bibcheckRetractionItem("NODOI1", "gamma", ""),
		bibcheckRetractionItem("UNCITED", "beta", "10.777/uncited"),
	})
	stub := newBibcheckCrossrefStub(t)
	stub.notices["10.777/retracted"] = bibcheckRetractionNotice
	stub.notices["10.777/uncited"] = bibcheckRetractionNotice
	manuscript := filepath.Join(home, "paper.tex")
	writeTestFile(t, manuscript, "Intro.\nAs shown by \\cite{alpha}.\n\nAgain \\parencite{alpha,gamma}.\n")

	report, _, err := runBibcheckJSON(t, manuscript, "--check-retractions", "--fail-on", "high")
	if err == nil || ExitCode(err) != 11 {
		t.Fatalf("err = %v (exit %d), want gate exit 11", err, ExitCode(err))
	}
	retracted := bibcheckFindingsOfKind(report, "retracted_item")
	if len(retracted) != 1 {
		t.Fatalf("retracted_item findings = %+v, want exactly one", report.Findings)
	}
	f := retracted[0]
	if f.Severity != sevCritical || f.ItemKey != "CITED1" || f.Source.Kind != "local" {
		t.Fatalf("finding = %+v, want critical CITED1 from the local source", f)
	}
	if got := evidenceString(t, f, "file_line"); got != manuscript+":2" {
		t.Fatalf("file_line = %q, want the first \\cite line %s:2", got, manuscript)
	}
	want := []string{manuscript + ":2", manuscript + ":4"}
	if got := evidenceStrings(t, f, "file_lines"); !reflect.DeepEqual(got, want) {
		t.Fatalf("file_lines = %v, want every citing line %v", got, want)
	}
	if evidenceString(t, f, "doi") != "10.777/retracted" || evidenceString(t, f, "status") != "retracted" || evidenceString(t, f, "notice_doi") != "10.777/notice" || evidenceString(t, f, "date") != "2025-01-15" {
		t.Fatalf("evidence = %+v, want the CrossRef notice", f.Evidence)
	}
	if report.Retractions == nil || *report.Retractions != (bibcheckRetractionSummary{CitedItems: 2, Checked: 1, Flagged: 1, Unchecked: 1}) {
		t.Fatalf("retractions summary = %+v, want 2 cited, 1 checked, 1 flagged, 1 unchecked", report.Retractions)
	}
	if len(report.Skipped) != 0 {
		t.Fatalf("skipped = %+v, want none", report.Skipped)
	}
	probes, works := stub.requested()
	if probes != 1 || !reflect.DeepEqual(works, []string{"10.777/retracted"}) {
		t.Fatalf("CrossRef saw probes=%d works=%v, want one probe and only the cited DOI", probes, works)
	}
}

// An expression of concern is high, a correction is not a finding, and a
// retraction notice reached through an item cited by a Pandoc key works the
// same as through TeX.
func TestBibcheckCheckRetractionsConcernIsHighAndCorrectionIsSilent(t *testing.T) {
	home := bibcheckIsolatedHome(t)
	seedBibcheckItems(t, []json.RawMessage{
		bibcheckRetractionItem("CONCERN", "alpha", "10.777/concern-work"),
		bibcheckRetractionItem("FIXED", "beta", "10.777/fixed-work"),
	})
	stub := newBibcheckCrossrefStub(t)
	stub.notices["10.777/concern-work"] = bibcheckConcernAndCorrectionNotice
	stub.notices["10.777/fixed-work"] = bibcheckCorrectionNotice
	manuscript := filepath.Join(home, "paper.md")
	writeTestFile(t, manuscript, "Intro.\n\nSee @alpha and @beta.\n")

	report, _, err := runBibcheckJSON(t, manuscript, "--check-retractions")
	if err != nil {
		t.Fatalf("no --fail-on must not gate: %v", err)
	}
	if len(bibcheckFindingsOfKind(report, "retracted_item")) != 0 {
		t.Fatalf("findings = %+v, want no retracted_item for a concern or a correction", report.Findings)
	}
	concern := bibcheckFindingsOfKind(report, "expression_of_concern")
	if len(concern) != 1 || concern[0].Severity != sevHigh || concern[0].ItemKey != "CONCERN" {
		t.Fatalf("expression_of_concern findings = %+v, want one high finding for CONCERN", concern)
	}
	if got := evidenceString(t, concern[0], "file_line"); got != manuscript+":3" {
		t.Fatalf("file_line = %q, want %s:3", got, manuscript)
	}
	if report.Retractions.Flagged != 1 || report.Retractions.Checked != 2 {
		t.Fatalf("retractions summary = %+v, want 2 checked, 1 flagged", report.Retractions)
	}

	_, _, err = runBibcheckJSON(t, manuscript, "--check-retractions", "--fail-on", "high")
	if err == nil || ExitCode(err) != 11 {
		t.Fatalf("err = %v, want --fail-on high to exit 11 on a concern", err)
	}
}

// The check is off by default: no flag, no CrossRef request, no retraction
// section in the report.
func TestBibcheckWithoutCheckRetractionsMakesNoCrossrefRequest(t *testing.T) {
	home := bibcheckIsolatedHome(t)
	seedBibcheckItems(t, []json.RawMessage{bibcheckRetractionItem("CITED1", "alpha", "10.777/retracted")})
	stub := newBibcheckCrossrefStub(t)
	stub.notices["10.777/retracted"] = bibcheckRetractionNotice
	manuscript := filepath.Join(home, "paper.tex")
	writeTestFile(t, manuscript, "\\cite{alpha}\n")

	report, raw, err := runBibcheckJSON(t, manuscript, "--fail-on", "high")
	if err != nil {
		t.Fatalf("items bibcheck without the flag: %v", err)
	}
	probes, works := stub.requested()
	if probes != 0 || len(works) != 0 {
		t.Fatalf("CrossRef saw probes=%d works=%v, want no request without --check-retractions", probes, works)
	}
	if report.Retractions != nil || len(report.Skipped) != 0 || bytes.Contains(raw, []byte(`"retractions"`)) || bytes.Contains(raw, []byte(`"skipped"`)) {
		t.Fatalf("report carries a retraction section without the flag: %s", raw)
	}
}

// A lookup CrossRef answers with HTTP 500 is reported as skipped, never as an
// all-clear. Under --fail-on it exits 9: the gate cannot certify the
// manuscript. Without a gate the run still exits 0 but the skip is in the
// report. A retraction found for another cited item is kept, and its gate (11)
// wins over the skip.
func TestBibcheckCheckRetractionsReportsLookupFailure(t *testing.T) {
	home := bibcheckIsolatedHome(t)
	seedBibcheckItems(t, []json.RawMessage{
		bibcheckRetractionItem("BROKEN", "alpha", "10.777/broken"),
		bibcheckRetractionItem("RETRACT", "beta", "10.777/retracted"),
	})
	stub := newBibcheckCrossrefStub(t)
	stub.fail["10.777/broken"] = true
	stub.notices["10.777/retracted"] = bibcheckRetractionNotice

	onlyBroken := filepath.Join(home, "broken.tex")
	writeTestFile(t, onlyBroken, "\\cite{alpha}\n")
	both := filepath.Join(home, "both.tex")
	writeTestFile(t, both, "\\cite{alpha}\n\\cite{beta}\n")

	t.Run("gated exits 9", func(t *testing.T) {
		report, _, err := runBibcheckJSON(t, onlyBroken, "--check-retractions", "--fail-on", "high")
		if err == nil || ExitCode(err) != 9 {
			t.Fatalf("err = %v (exit %d), want precondition exit 9", err, ExitCode(err))
		}
		if len(report.Skipped) != 1 {
			t.Fatalf("skipped = %+v, want one loud skip", report.Skipped)
		}
		skip := report.Skipped[0]
		if skip.Kind != "retracted_item" || skip.Precondition != "external_crossref" || !strings.Contains(skip.Detail, "HTTP 500") || !strings.Contains(skip.Detail, "BROKEN") || len(skip.Remediation) == 0 {
			t.Fatalf("skip = %+v, want an external_crossref skip naming BROKEN and HTTP 500", skip)
		}
		if report.Retractions == nil || report.Retractions.Errors != 1 || report.Retractions.Flagged != 0 {
			t.Fatalf("retractions summary = %+v, want one lookup error and nothing flagged", report.Retractions)
		}
	})

	t.Run("ungated still reports the skip", func(t *testing.T) {
		report, _, err := runBibcheckJSON(t, onlyBroken, "--check-retractions")
		if err != nil {
			t.Fatalf("no --fail-on must not gate: %v", err)
		}
		if len(report.Skipped) != 1 {
			t.Fatalf("skipped = %+v, want the lookup failure reported even without a gate", report.Skipped)
		}
	})

	t.Run("a found retraction keeps its gate", func(t *testing.T) {
		report, _, err := runBibcheckJSON(t, both, "--check-retractions", "--fail-on", "high")
		if err == nil || ExitCode(err) != 11 {
			t.Fatalf("err = %v, want exit 11 for the retraction that did resolve", err)
		}
		retracted := bibcheckFindingsOfKind(report, "retracted_item")
		if len(retracted) != 1 || retracted[0].ItemKey != "RETRACT" || len(report.Skipped) != 1 {
			t.Fatalf("findings = %+v skipped = %+v, want RETRACT flagged and the BROKEN lookup skipped", report.Findings, report.Skipped)
		}
	})
}

// A CrossRef that cannot be reached at all is one skip, not one error per DOI,
// and no per-DOI request goes out.
func TestBibcheckCheckRetractionsReportsUnreachableProvider(t *testing.T) {
	home := bibcheckIsolatedHome(t)
	seedBibcheckItems(t, []json.RawMessage{bibcheckRetractionItem("CITED1", "alpha", "10.777/retracted")})
	stub := newBibcheckCrossrefStub(t)
	stub.probeFails = true
	manuscript := filepath.Join(home, "paper.tex")
	writeTestFile(t, manuscript, "\\cite{alpha}\n")

	report, _, err := runBibcheckJSON(t, manuscript, "--check-retractions", "--fail-on", "high")
	if err == nil || ExitCode(err) != 9 {
		t.Fatalf("err = %v (exit %d), want precondition exit 9", err, ExitCode(err))
	}
	if len(report.Skipped) != 1 || !strings.Contains(report.Skipped[0].Detail, "not reachable") {
		t.Fatalf("skipped = %+v, want one not-reachable skip", report.Skipped)
	}
	if report.Retractions == nil || report.Retractions.Checked != 0 || report.Retractions.Unchecked != 1 {
		t.Fatalf("retractions summary = %+v, want the cited item counted as unchecked", report.Retractions)
	}
	if _, works := stub.requested(); len(works) != 0 {
		t.Fatalf("CrossRef saw work lookups %v after a failed probe", works)
	}
}

// A manuscript that cites only items with no DOI has nothing to ask CrossRef,
// so the network is not touched, and the report says nothing was checked.
func TestBibcheckCheckRetractionsWithoutCitedDOIMakesNoRequest(t *testing.T) {
	home := bibcheckIsolatedHome(t)
	seedBibcheckItems(t, []json.RawMessage{
		bibcheckRetractionItem("NODOI1", "alpha", ""),
		bibcheckRetractionItem("UNCITED", "beta", "10.777/uncited"),
	})
	stub := newBibcheckCrossrefStub(t)
	stub.notices["10.777/uncited"] = bibcheckRetractionNotice
	manuscript := filepath.Join(home, "paper.tex")
	writeTestFile(t, manuscript, "\\cite{alpha}\n\\cite{ghost}\n")

	report, _, err := runBibcheckJSON(t, manuscript, "--check-retractions", "--fail-on", "none")
	if err != nil {
		t.Fatalf("items bibcheck: %v", err)
	}
	if probes, works := stub.requested(); probes != 0 || len(works) != 0 {
		t.Fatalf("CrossRef saw probes=%d works=%v, want no request", probes, works)
	}
	if report.Retractions == nil || *report.Retractions != (bibcheckRetractionSummary{CitedItems: 1, Unchecked: 1}) || len(report.Skipped) != 0 {
		t.Fatalf("retractions = %+v skipped = %+v, want 1 cited, 1 unchecked, no skip", report.Retractions, report.Skipped)
	}
}

// Human output names the retracted work at its manuscript line and states
// the coverage; a skipped lookup is printed, not omitted.
func TestBibcheckCheckRetractionsHumanOutput(t *testing.T) {
	home := bibcheckIsolatedHome(t)
	seedBibcheckItems(t, []json.RawMessage{
		bibcheckRetractionItem("RETRACT", "alpha", "10.777/retracted"),
		bibcheckRetractionItem("BROKEN", "beta", "10.777/broken"),
	})
	stub := newBibcheckCrossrefStub(t)
	stub.notices["10.777/retracted"] = bibcheckRetractionNotice
	stub.fail["10.777/broken"] = true
	manuscript := filepath.Join(home, "paper.tex")
	writeTestFile(t, manuscript, "Intro.\n\\cite{alpha}\n\\cite{beta}\n")

	cmd := newItemsCmd(&rootFlags{})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{"bibcheck", manuscript, "--check-retractions"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("items bibcheck: %v", err)
	}
	text := out.String()
	for _, want := range []string{
		"Retraction check: 2 cited item(s), 1 checked, 1 flagged, 1 not checked, 1 lookup error(s)",
		"SKIPPED retracted_item (external_crossref)",
		"retracted_item critical alpha (retracted, doi 10.777/retracted) — " + manuscript + ":2",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("human output is missing %q:\n%s", want, text)
		}
	}
}
