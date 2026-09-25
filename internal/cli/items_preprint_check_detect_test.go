// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// preprintDetectStubGetter is a fake zoteroGetter serving one canned page per
// query shape. Both candidate sources paginate through fetchZoteroItems, which
// stops at the first short page, so one response per source is enough.
type preprintDetectStubGetter struct {
	t     *testing.T
	pages map[string][]map[string]any
}

func (s *preprintDetectStubGetter) Get(path string, params map[string]string) (json.RawMessage, error) {
	// The fake must mirror the real query shapes: fetchPreprintCheckCandidates
	// pages /items twice, once with itemType=preprint and once with q=arxiv.
	// Ignoring path and serving the arXiv page for any non-preprint query
	// let a dropped q=arxiv or a wrong endpoint pass.
	if path != "/items" {
		return nil, fmt.Errorf("stub Get path = %q, want /items", path)
	}
	if params["start"] != "0" && params["start"] != "" {
		return json.RawMessage(`[]`), nil
	}
	var items []map[string]any
	switch {
	case params["itemType"] == "preprint" && params["q"] == "":
		items = s.pages["preprint"]
	case params["q"] == "arxiv" && params["itemType"] == "":
		items = s.pages["arxiv"]
	default:
		return nil, fmt.Errorf("stub Get params = %#v, want itemType=preprint or q=arxiv", params)
	}
	raw, err := json.Marshal(items)
	if err != nil {
		s.t.Fatalf("marshal stub items: %v", err)
	}
	return raw, nil
}

// fetchArxivPreprintCandidates had no command-level witness: the arXiv-ID
// filter and cross-source dedup could regress (dropping every candidate or
// double-reporting) while CI stayed green.
func TestFetchArxivPreprintCandidatesFiltersAndDedups(t *testing.T) {
	withArxiv := map[string]any{"key": "PUB1", "title": "Published paper", "url": "https://arxiv.org/abs/2301.00001"}
	withoutArxiv := map[string]any{"key": "DRAFT1", "title": "Draft without identifier", "itemType": "preprint"}
	// ARXIVONLY exists only in the q=arxiv source: dropping that query must
	// lose it, so the test proves the second candidate source is queried.
	arxivOnly := map[string]any{"key": "ARXIVONLY", "title": "Found only by q=arxiv", "url": "https://arxiv.org/abs/2301.00003"}
	stub := &preprintDetectStubGetter{t: t, pages: map[string][]map[string]any{
		"preprint": {withArxiv, withoutArxiv},
		"arxiv":    {withArxiv, arxivOnly},
	}}

	candidates, err := fetchArxivPreprintCandidates(stub, 20)
	if err != nil {
		t.Fatalf("fetchArxivPreprintCandidates: %v", err)
	}
	got := map[string]bool{}
	for _, c := range candidates {
		got[zoteroString(c, "key")] = true
	}
	for _, want := range []string{"PUB1", "ARXIVONLY"} {
		if !got[want] {
			t.Errorf("candidates missing %q (got keys %v); q=arxiv source may not have been queried", want, got)
		}
	}
	if got["DRAFT1"] {
		t.Errorf("candidates contain DRAFT1 (got keys %v); the arXiv-ID filter did not drop the identifier-less draft", got)
	}
	if len(candidates) != 2 {
		keys := make([]string, 0, len(candidates))
		for _, c := range candidates {
			keys = append(keys, zoteroString(c, "key"))
		}
		t.Fatalf("candidates = %v, want [PUB1 ARXIVONLY] (arxiv-only, deduped across sources)", keys)
	}
}

// preprintCheckFindings maps published results to agent-consumable Findings.
// The mapping (Kind, evidence, autofix flag, remediation command) was
// unexercised: a dropped or malformed published finding would pass CI.
func TestPreprintCheckFindingsMapsPublishedResults(t *testing.T) {
	results := []preprintCheckResult{
		{Key: "PUB1", Title: "Published paper", ArxivID: "2301.00001", Status: "published", DOI: "10.1145/1234567.890123", Venue: "Journal of Tests", Year: 2024},
		{Key: "DRAFT1", Title: "Still a preprint", ArxivID: "2301.00002", Status: "preprint"},
	}

	findings := preprintCheckFindings(results)
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want 1 (the preprint-only result maps to nothing)", len(findings))
	}
	finding := findings[0]
	if finding.Kind != "preprint_published" {
		t.Errorf("Kind = %q, want preprint_published", finding.Kind)
	}
	if finding.ItemKey != "PUB1" || finding.Title != "Published paper" {
		t.Errorf("finding targets %q/%q, want PUB1/Published paper", finding.ItemKey, finding.Title)
	}
	for key, want := range map[string]any{
		"arxiv_id": "2301.00001",
		"doi":      "10.1145/1234567.890123",
		"status":   "published",
		"venue":    "Journal of Tests",
		"year":     2024,
	} {
		if finding.Evidence[key] != want {
			t.Errorf("evidence[%q] = %#v, want %#v (full evidence: %#v)", key, finding.Evidence[key], want, finding.Evidence)
		}
	}
	if !finding.Autofixable {
		t.Error("Autofixable = false, want true so the fix subcommand is offered")
	}
	if finding.RecommendedAction == nil || finding.RecommendedAction.Command != "zotio items preprint-check fix" {
		t.Errorf("RecommendedAction = %#v, want the preprint-check fix command", finding.RecommendedAction)
	}

	if len(preprintCheckFindings([]preprintCheckResult{{Key: "D", Status: "preprint"}})) != 0 {
		t.Fatal("preprint-only results produced findings, want none")
	}
}

// checkPreprintCandidate wires one Zotero candidate through both metadata
// providers. The stub transport answers both hosts, so this exercises the
// published path end to end without network and without the command's
// inter-item sleep.
func TestCheckPreprintCandidateResolvesPublished(t *testing.T) {
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Hostname() {
		case "export.arxiv.org":
			// The arXiv query must carry this candidate's ID: sending
			// 2301.00002 for PUB1 used to return the same DOI fixture and
			// still mark it published.
			if got := req.URL.Query().Get("id_list"); got != "2301.00001" {
				return nil, fmt.Errorf("arXiv id_list = %q, want 2301.00001 for PUB1", got)
			}
			return testHTTPResponse(http.StatusOK, arxivAtomFeedXML(`<arxiv:doi>10.1145/1234567.890123</arxiv:doi>`)), nil
		case "api.crossref.org":
			// lookupCrossrefDOI escapes the DOI into the path, so the slash
			// arrives as %2F; pin the escaped form to catch a wrong DOI too.
			if got := req.URL.EscapedPath(); got != "/works/10.1145%2F1234567.890123" {
				return nil, fmt.Errorf("CrossRef path = %q, want /works/10.1145%%2F1234567.890123", got)
			}
			return testHTTPResponse(http.StatusOK, crossrefWorkJSON("10.1145/1234567.890123", "Journal of Tests", 2024)), nil
		default:
			t.Fatalf("unexpected host %q", req.URL.Hostname())
			return nil, nil
		}
	})
	httpClient := &http.Client{Transport: transport}

	published, err := checkPreprintCandidate(context.Background(),
		map[string]any{"key": "PUB1", "title": "Published paper", "url": "https://arxiv.org/abs/2301.00001"},
		httpClient)
	if err != nil {
		t.Fatalf("checkPreprintCandidate: %v", err)
	}
	if published.Status != "published" || published.DOI != "10.1145/1234567.890123" || published.Venue != "Journal of Tests" || published.Year != 2024 {
		t.Fatalf("published result = %+v, want the CrossRef match recorded", published)
	}
	if findings := preprintCheckFindings([]preprintCheckResult{published}); len(findings) != 1 || findings[0].ItemKey != "PUB1" {
		t.Fatalf("findings for resolved candidate = %#v, want one finding for PUB1", findings)
	}

	control, err := checkPreprintCandidate(context.Background(),
		map[string]any{"key": "DRAFT1", "title": "Draft", "url": "https://arxiv.org/abs/2301.00002"},
		&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Hostname() == "export.arxiv.org" {
				if got := req.URL.Query().Get("id_list"); got != "2301.00002" {
					return nil, fmt.Errorf("arXiv id_list = %q, want 2301.00002 for DRAFT1", got)
				}
				return testHTTPResponse(http.StatusOK, arxivAtomFeedXML(``)), nil
			}
			t.Fatalf("unexpected host %q (no external DOI means no CrossRef call)", req.URL.Hostname())
			return nil, nil
		})})
	if err != nil {
		t.Fatalf("checkPreprintCandidate (control): %v", err)
	}
	if control.Status != "preprint" {
		t.Fatalf("control status = %q, want preprint", control.Status)
	}
}
