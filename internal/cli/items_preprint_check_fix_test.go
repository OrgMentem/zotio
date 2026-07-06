// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zotio/internal/mutation"
)

type preprintFixGetter struct {
	preprintItems json.RawMessage
	queryItems    json.RawMessage
}

func (g preprintFixGetter) Get(path string, params map[string]string) (json.RawMessage, error) {
	if path != "/items" {
		return nil, fmt.Errorf("unexpected path %q", path)
	}
	if params["itemType"] == "preprint" {
		if g.preprintItems != nil {
			return g.preprintItems, nil
		}
		return json.RawMessage(`[]`), nil
	}
	if params["q"] == "arxiv" {
		if g.queryItems != nil {
			return g.queryItems, nil
		}
		return json.RawMessage(`[]`), nil
	}
	return nil, fmt.Errorf("unexpected params %#v", params)
}

func withDefaultTransport(t *testing.T, rt http.RoundTripper) {
	t.Helper()
	saved := http.DefaultTransport
	http.DefaultTransport = rt
	t.Cleanup(func() { http.DefaultTransport = saved })
}

func TestPreprintCheckFixProposalDOIWritePolicy(t *testing.T) {
	match := crossrefMatch{DOI: "10.1234/published", Venue: "Journal of Done", Year: 2026}
	cases := []struct {
		name           string
		data           map[string]any
		wantDOIWrite   bool
		wantSkipReason string
	}{
		{
			name:         "empty DOI field gets published DOI",
			data:         map[string]any{"key": "K1", "title": "Paper", "DOI": "", "extra": "ArXiv: 2401.00001"},
			wantDOIWrite: true,
		},
		{
			name:         "arxiv self DOI gets replaced",
			data:         map[string]any{"key": "K2", "title": "Paper", "DOI": "https://doi.org/10.48550/arXiv.2401.00001", "extra": "ArXiv: 2401.00001"},
			wantDOIWrite: true,
		},
		{
			name:           "different DOI is a conflict",
			data:           map[string]any{"key": "K3", "title": "Paper", "DOI": "10.9999/already-published", "extra": "ArXiv: 2401.00001"},
			wantSkipReason: "doi_conflict",
		},
		{
			name:         "schema without DOI field records provenance only",
			data:         map[string]any{"key": "K4", "title": "Paper", "extra": "ArXiv: 2401.00001"},
			wantDOIWrite: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			item := map[string]any{"key": tc.data["key"], "version": float64(7), "data": tc.data}
			proposal, skip, ok := preprintCheckFixProposalForItem(item, match)
			if tc.wantSkipReason != "" {
				if ok {
					t.Fatalf("proposal unexpectedly succeeded: %+v", proposal)
				}
				if skip.Reason != tc.wantSkipReason {
					t.Fatalf("skip reason = %q, want %q", skip.Reason, tc.wantSkipReason)
				}
				return
			}
			if !ok {
				t.Fatalf("proposal skipped unexpectedly: %+v", skip)
			}
			_, wroteDOI := proposal.Fields["DOI"]
			if wroteDOI != tc.wantDOIWrite {
				t.Fatalf("DOI write presence = %v, want %v; fields=%+v", wroteDOI, tc.wantDOIWrite, proposal.Fields)
			}
			if tc.wantDOIWrite && proposal.Fields["DOI"] != match.DOI {
				t.Fatalf("DOI write = %v, want %s", proposal.Fields["DOI"], match.DOI)
			}
			if extra, ok := proposal.Fields["extra"].(string); !ok || !strings.Contains(extra, "published as doi:10.1234/published (Journal of Done, 2026)") {
				t.Fatalf("extra provenance = %q, want published DOI/venue/year", extra)
			}
		})
	}
}

func TestPreprintCheckFixProposalAppendsExtraVerbatim(t *testing.T) {
	existing := "Citation Key: smith2024\nPinned note: do not rewrite"
	item := map[string]any{
		"key":     "K1",
		"version": float64(3),
		"data": map[string]any{
			"key":   "K1",
			"title": "Paper",
			"DOI":   "",
			"extra": existing,
		},
	}
	proposal, skip, ok := preprintCheckFixProposalForItem(item, crossrefMatch{DOI: "10.5555/published"})
	if !ok {
		t.Fatalf("proposal skipped unexpectedly: %+v", skip)
	}
	extra, _ := proposal.Fields["extra"].(string)
	if !strings.HasPrefix(extra, existing+"\n") {
		t.Fatalf("extra = %q, want existing Extra preserved verbatim before appended provenance", extra)
	}
	if !strings.Contains(extra, "zotio preprint-check: published as doi:10.5555/published") {
		t.Fatalf("extra = %q, want appended zotio provenance", extra)
	}
}

func TestPreprintCheckFixPlannedOpCarriesVersionIntoPatch(t *testing.T) {
	proposal := preprintCheckFixProposal{
		Key:     "K1",
		Title:   "Paper",
		DOI:     "10.1234/published",
		Fields:  map[string]any{"DOI": "10.1234/published", "extra": "published provenance"},
		version: float64(17),
	}
	mutator := &fakeMutator{}
	ops := preprintCheckFixPlannedOps([]preprintCheckFixProposal{proposal}, func() apiMutator { return mutator })
	if len(ops) != 1 {
		t.Fatalf("planned ops = %d, want 1", len(ops))
	}
	if ops[0].ExpectedVersion != 17 {
		t.Fatalf("expected version = %d, want 17", ops[0].ExpectedVersion)
	}

	status, reason, err := ops[0].Apply()
	if err != nil || reason != nil || status != "applied" {
		t.Fatalf("apply = status %q reason %v err %v, want applied", status, reason, err)
	}
	if mutator.patchPath != "/items/K1" {
		t.Fatalf("patch path = %q, want /items/K1", mutator.patchPath)
	}
	if mutator.patchBody["version"] != float64(17) {
		t.Fatalf("patch version = %v, want 17", mutator.patchBody["version"])
	}
	if mutator.patchBody["DOI"] != "10.1234/published" {
		t.Fatalf("patch DOI = %v, want published DOI", mutator.patchBody["DOI"])
	}
}

func TestBuildPreprintCheckFixOpsNoArxivIDSkip(t *testing.T) {
	getter := preprintFixGetter{preprintItems: json.RawMessage(`[{
		"key":"K0","version":1,
		"data":{"key":"K0","title":"Preprint Without Identifier","itemType":"preprint","extra":"submitted manuscript"}
	}]`)}
	cmd := newItemsPreprintCheckFixCmd(&rootFlags{})
	cmd.SetContext(context.Background())
	ops, skipped, err := buildPreprintCheckFixOps(cmd, &rootFlags{timeout: time.Second}, getter, func() apiMutator { return nil }, 1)
	if err != nil {
		t.Fatalf("buildPreprintCheckFixOps: %v", err)
	}
	if len(ops) != 0 {
		t.Fatalf("ops = %d, want 0 when no arXiv identifier can be extracted", len(ops))
	}
	if len(skipped) != 1 || skipped[0].Key != "K0" || skipped[0].Reason != "no_arxiv_id" {
		t.Fatalf("skipped = %+v, want K0 no_arxiv_id", skipped)
	}
}

func TestBuildPreprintCheckFixOpsStillPreprintSkip(t *testing.T) {
	withDefaultTransport(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "export.arxiv.org" {
			return testHTTPResponse(http.StatusOK, arxivAtomFeedXML(`<id>http://arxiv.org/abs/2401.00001</id>`)), nil
		}
		return testHTTPResponse(http.StatusNotFound, `{}`), nil
	}))

	getter := preprintFixGetter{preprintItems: json.RawMessage(`[{
		"key":"K1","version":4,
		"data":{"key":"K1","title":"Unpublished","DOI":"","extra":"ArXiv: 2401.00001"}
	}]`)}
	cmd := newItemsPreprintCheckFixCmd(&rootFlags{})
	cmd.SetContext(context.Background())
	ops, skipped, err := buildPreprintCheckFixOps(cmd, &rootFlags{timeout: time.Second}, getter, func() apiMutator { return nil }, 1)
	if err != nil {
		t.Fatalf("buildPreprintCheckFixOps: %v", err)
	}
	if len(ops) != 0 {
		t.Fatalf("ops = %d, want 0 for still-unpublished preprint", len(ops))
	}
	if len(skipped) != 1 || skipped[0].Key != "K1" || skipped[0].Reason != "still_preprint" {
		t.Fatalf("skipped = %+v, want K1 still_preprint", skipped)
	}
}

func TestItemsPreprintCheckFixDefaultsToPreviewWithoutPatch(t *testing.T) {
	var patchCount int
	zoteroServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			patchCount++
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/items") {
			_, _ = w.Write([]byte(`[{
				"key":"K1","version":9,
				"data":{"key":"K1","title":"Published Now","DOI":"","extra":"ArXiv: 2401.00001"}
			}]`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer zoteroServer.Close()

	savedTransport := http.DefaultTransport
	withDefaultTransport(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Host {
		case "export.arxiv.org":
			return testHTTPResponse(http.StatusOK, arxivAtomFeedXML(`<id>http://arxiv.org/abs/2401.00001</id><arxiv:doi>10.2000/published</arxiv:doi>`)), nil
		case "api.crossref.org":
			return testHTTPResponse(http.StatusOK, crossrefWorkJSON("10.2000/published", "Journal", 2025)), nil
		default:
			return savedTransport.RoundTrip(req)
		}
	}))

	t.Setenv("ZOTERO_BASE_URL", zoteroServer.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	flags := &rootFlags{asJSON: true, timeout: time.Second}
	cmd := newItemsPreprintCheckFixCmd(flags)
	cmd.SetArgs([]string{"--limit", "1"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("preprint-check fix preview: %v", err)
	}
	if patchCount != 0 {
		t.Fatalf("PATCH count = %d, want 0 in default preview mode", patchCount)
	}

	var env mutation.Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode preview envelope %q: %v", out.String(), err)
	}
	if !env.OK || env.Mode != "preview" || env.PreviewReason != "default" || env.Result != nil {
		t.Fatalf("envelope = %+v, want default preview without result", env)
	}
	if env.Plan.Summary.Planned != 1 || len(env.Plan.Operations) != 1 {
		t.Fatalf("plan = %+v, want one planned DOI fix", env.Plan)
	}
	if env.Plan.Operations[0].ExpectedVersion != 9 {
		t.Fatalf("expected version = %d, want 9", env.Plan.Operations[0].ExpectedVersion)
	}
}
