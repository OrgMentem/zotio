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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"zotio/internal/client"
	"zotio/internal/config"
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

func TestPreprintCheckFixPlannedOpUsesWritePlaneVersionHeader(t *testing.T) {
	proposal := preprintCheckFixProposal{
		Key:     "K1",
		Title:   "Paper",
		DOI:     "10.1234/published",
		Fields:  map[string]any{"DOI": "10.1234/published", "extra": "published provenance"},
		match:   crossrefMatch{DOI: "10.1234/published"},
		version: float64(17),
	}
	// The proposal's local-plane version (17) is kept only for plan display
	// (ExpectedVersion). The write precondition must come from the write plane,
	// sent as If-Unmodified-Since-Version, not as a body "version" key.
	mutator := &fakeMutator{
		writeVersion: 42,
		writeData:    json.RawMessage(`{"key":"K1","version":42,"data":{"key":"K1","DOI":"","extra":"Citation Key: web2024"}}`),
	}
	ops := preprintCheckFixPlannedOps(context.Background(), []preprintCheckFixProposal{proposal}, func() apiMutator { return mutator })
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
	if _, hasVersion := mutator.patchBody["version"]; hasVersion {
		t.Fatalf("patch body must not carry a version key; got %v", mutator.patchBody)
	}
	if mutator.patchBody["DOI"] != "10.1234/published" {
		t.Fatalf("patch DOI = %v, want published DOI", mutator.patchBody["DOI"])
	}
	if got := mutator.patchHeaders["If-Unmodified-Since-Version"]; got != "42" {
		t.Fatalf("If-Unmodified-Since-Version = %q, want %q (write-plane version)", got, "42")
	}
	// The payload and its precondition must come from ONE write-plane read: a
	// second read would pair a newer version with the older payload.
	if len(mutator.writeVerPaths) != 1 || mutator.writeVerPaths[0] != "/items/K1" {
		t.Fatalf("write-plane reads = %v, want exactly one for /items/K1", mutator.writeVerPaths)
	}
}

func TestPreprintCheckFixPlannedOpFailsClosedOnMissingWritePlaneVersion(t *testing.T) {
	proposal := preprintCheckFixProposal{
		Key:    "K1",
		Title:  "Paper",
		DOI:    "10.1234/published",
		Fields: map[string]any{"DOI": "10.1234/published"},
		match:  crossrefMatch{DOI: "10.1234/published"},
		// Local version present but write-plane version is absent (0) — must fail closed.
		version: float64(17),
	}
	mutator := &fakeMutator{
		writeVersion: 0,
		writeData:    json.RawMessage(`{"key":"K1","data":{"key":"K1","DOI":"","extra":""}}`),
	}
	ops := preprintCheckFixPlannedOps(context.Background(), []preprintCheckFixProposal{proposal}, func() apiMutator { return mutator })
	if len(ops) != 1 {
		t.Fatalf("planned ops = %d, want 1", len(ops))
	}
	status, reason, err := ops[0].Apply()
	if err == nil {
		t.Fatalf("apply err = nil, want error when write-plane version is 0")
	}
	if status != "failed" {
		t.Fatalf("apply status = %q, want failed", status)
	}
	if reason == nil || !strings.Contains(fmt.Sprint(reason), "write-plane version") {
		t.Fatalf("apply reason = %v, want message mentioning write-plane version", reason)
	}
	if mutator.patchPath != "" {
		t.Fatalf("patch path = %q, want no PATCH dispatched when version is 0", mutator.patchPath)
	}
}

// Planning reads the item through the read plane, which lags the Web API. The
// write must be derived from the write plane's current item: Extra is replaced
// wholesale, so a planned value built from a stale read deletes whatever was
// written since, and a DOI added on the Web since must still block the fix.
func TestPreprintCheckFixApplyRederivesFromWritePlane(t *testing.T) {
	const staleExtra = "ArXiv: 2401.00001"
	match := crossrefMatch{DOI: "10.1234/published", Venue: "Journal of Done", Year: 2026}
	cases := []struct {
		name       string
		liveItem   string
		wantStatus string
		wantPatch  bool
		wantDOI    string
		wantExtra  []string
	}{
		{
			name:       "web only extra line survives",
			liveItem:   `{"key":"K1","version":42,"data":{"key":"K1","DOI":"","extra":"ArXiv: 2401.00001\nCitation Key: web2024\nPinned: keep me"}}`,
			wantStatus: "applied",
			wantPatch:  true,
			wantDOI:    "10.1234/published",
			wantExtra:  []string{"ArXiv: 2401.00001\nCitation Key: web2024\nPinned: keep me\nzotio preprint-check: published as doi:10.1234/published"},
		},
		{
			name:       "arxiv self doi on the web is replaced",
			liveItem:   `{"key":"K1","version":42,"data":{"key":"K1","DOI":"10.48550/arXiv.2401.00001","extra":"ArXiv: 2401.00001"}}`,
			wantStatus: "applied",
			wantPatch:  true,
			wantDOI:    "10.1234/published",
			wantExtra:  []string{"ArXiv: 2401.00001\nzotio preprint-check: published as doi:10.1234/published"},
		},
		{
			name:       "published doi added on the web blocks the fix",
			liveItem:   `{"key":"K1","version":42,"data":{"key":"K1","DOI":"10.9999/other-journal","extra":"ArXiv: 2401.00001"}}`,
			wantStatus: "skipped",
			wantPatch:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The read plane still shows a blank DOI and the old Extra.
			readPlaneItem := map[string]any{
				"key": "K1", "version": float64(7),
				"data": map[string]any{"key": "K1", "title": "Paper", "DOI": "", "extra": staleExtra},
			}
			proposal, skip, ok := preprintCheckFixProposalForItem(readPlaneItem, match)
			if !ok {
				t.Fatalf("read-plane proposal skipped: %+v", skip)
			}
			mutator := &fakeMutator{writeVersion: 42, writeData: json.RawMessage(tc.liveItem)}
			ops := preprintCheckFixPlannedOps(context.Background(), []preprintCheckFixProposal{proposal}, func() apiMutator { return mutator })

			status, reason, err := ops[0].Apply()
			if status != tc.wantStatus || err != nil {
				t.Fatalf("apply = status %q reason %v err %v, want %q", status, reason, err, tc.wantStatus)
			}
			if !tc.wantPatch {
				if mutator.patchPath != "" {
					t.Fatalf("PATCH dispatched to %q, want none when the live DOI conflicts", mutator.patchPath)
				}
				if detail, _ := reason.(map[string]any); detail["reason"] != "doi_conflict" {
					t.Fatalf("reason = %v, want doi_conflict", reason)
				}
				if ops[0].Effective() != nil {
					t.Fatalf("Effective = %v, want nothing replayed for a skipped op", ops[0].Effective())
				}
				return
			}
			if mutator.patchBody["DOI"] != tc.wantDOI {
				t.Fatalf("patch DOI = %v, want %q", mutator.patchBody["DOI"], tc.wantDOI)
			}
			extra, _ := mutator.patchBody["extra"].(string)
			for _, want := range tc.wantExtra {
				if !strings.HasPrefix(extra, want) {
					t.Fatalf("patch extra = %q, want live Extra preserved then provenance: prefix %q", extra, want)
				}
			}
			// What the mirror replays is what was written, not the plan's
			// read-plane Extra.
			landed := ops[0].Effective()
			if len(landed) != 2 || landed[1].Field != "extra" || landed[1].Add != extra {
				t.Fatalf("Effective = %+v, want the DOI and the live-derived extra that was patched", landed)
			}
		})
	}
}

// A write-plane read that carries no data object cannot say what Extra holds.
// Patching anyway would replace Extra with the provenance line alone.
func TestPreprintCheckFixApplyRefusesUnusableWritePlaneItem(t *testing.T) {
	for name, body := range map[string]string{
		"null":           `null`,
		"empty object":   `{}`,
		"data not a map": `{"key":"K1","data":"oops"}`,
		"not json":       `<html>`,
	} {
		t.Run(name, func(t *testing.T) {
			proposal := preprintCheckFixProposal{Key: "K1", match: crossrefMatch{DOI: "10.1234/published"}}
			mutator := &fakeMutator{writeVersion: 42, writeData: json.RawMessage(body)}
			status, reason, err := applyPreprintCheckFixProposal(context.Background(), mutator, &proposal)
			if status != "failed" || err == nil {
				t.Fatalf("apply = status %q reason %v err %v, want failed with an error", status, reason, err)
			}
			if mutator.patchPath != "" {
				t.Fatalf("PATCH dispatched to %q, want none without live item data", mutator.patchPath)
			}
		})
	}
}

// A Web edit that lands after the apply-time read must lose to the version
// guard. Resolving a second version before the PATCH would carry the newer
// version with the older payload and pass.
func TestPreprintCheckFixApplyConflictsOnWebEditAfterRead(t *testing.T) {
	var (
		mu      sync.Mutex
		version = 10
		extra   = "Citation Key: web2024"
		gets    int
		patches int
	)
	writePlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			gets++
			w.Header().Set("Last-Modified-Version", strconv.Itoa(version))
			fmt.Fprintf(w, `{"key":"K1","version":%d,"data":{"key":"K1","DOI":"","extra":%q}}`, version, extra)
			// Another client edits Extra right after this read.
			version++
			extra += "\nAdded elsewhere"
		case http.MethodPatch:
			patches++
			if r.Header.Get("If-Unmodified-Since-Version") != strconv.Itoa(version) {
				http.Error(w, "precondition failed", http.StatusPreconditionFailed)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(writePlane.Close)
	c := client.New(&config.Config{BaseURL: writePlane.URL}, time.Second, 0)
	c.NoCache = true
	c.WriteBaseURL = writePlane.URL

	proposal := preprintCheckFixProposal{Key: "K1", match: crossrefMatch{DOI: "10.1234/published"}}
	status, detail, err := applyPreprintCheckFixProposal(context.Background(), c, &proposal)
	if status != "conflict" || err == nil {
		t.Fatalf("apply = status %q detail %v err %v, want conflict", status, detail, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if gets != 1 || patches != 1 {
		t.Fatalf("write plane saw %d GET and %d PATCH, want 1 and 1 (the guard is the version the payload was read at)", gets, patches)
	}
	if proposal.landed != nil {
		t.Fatalf("landed = %+v, want nothing recorded for a conflicted write", proposal.landed)
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

func TestBuildPreprintCheckFixOpsLookupFailureKeepsOtherResults(t *testing.T) {
	withDefaultTransport(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "export.arxiv.org" {
			if req.URL.Query().Get("id_list") == "2401.00001" {
				return testHTTPResponse(http.StatusInternalServerError, strings.Repeat("x", 1000)), nil
			}
			return testHTTPResponse(http.StatusOK, arxivAtomFeedXML(`<id>http://arxiv.org/abs/2401.00002</id>`)), nil
		}
		return testHTTPResponse(http.StatusNotFound, `{}`), nil
	}))

	getter := preprintFixGetter{preprintItems: json.RawMessage(`[
		{"key":"KBAD","version":1,"data":{"key":"KBAD","title":"Fails","extra":"ArXiv: 2401.00001"}},
		{"key":"KOK","version":1,"data":{"key":"KOK","title":"Fine","extra":"ArXiv: 2401.00002"}}
	]`)}
	cmd := newItemsPreprintCheckFixCmd(&rootFlags{})
	cmd.SetContext(context.Background())
	_, skipped, err := buildPreprintCheckFixOps(cmd, &rootFlags{timeout: time.Second}, getter, func() apiMutator { return nil }, 5)
	if err != nil {
		t.Fatalf("buildPreprintCheckFixOps: %v", err)
	}
	if len(skipped) != 2 {
		t.Fatalf("skipped = %+v, want 2 entries", skipped)
	}
	bySkip := map[string]preprintCheckFixSkip{}
	for _, s := range skipped {
		bySkip[s.Key] = s
	}
	if bySkip["KBAD"].Reason != "lookup_failed" || !strings.Contains(bySkip["KBAD"].Error, "HTTP 500") {
		t.Fatalf("KBAD skip = %+v, want lookup_failed with HTTP 500", bySkip["KBAD"])
	}
	if len(bySkip["KBAD"].Error) > 400 {
		t.Fatalf("error not truncated: %d bytes", len(bySkip["KBAD"].Error))
	}
	if bySkip["KOK"].Reason != "still_preprint" {
		t.Fatalf("KOK skip = %+v, want still_preprint (processing continued)", bySkip["KOK"])
	}
}

func TestPreprintCheckLookupFailureReportsErrorsAndKeepsResults(t *testing.T) {
	withDefaultTransport(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "export.arxiv.org" {
			if req.URL.Query().Get("id_list") == "2401.00001" {
				return testHTTPResponse(http.StatusBadGateway, "boom"), nil
			}
			return testHTTPResponse(http.StatusOK, arxivAtomFeedXML(`<arxiv:doi>10.1145/1.2</arxiv:doi>`)), nil
		}
		return testHTTPResponse(http.StatusOK, crossrefWorkJSON("10.1145/1.2", "Venue", 2024)), nil
	}))
	client := &http.Client{Timeout: time.Second}
	ctx := context.Background()
	if _, err := checkPreprintCandidate(ctx, map[string]any{"key": "KBAD", "extra": "ArXiv: 2401.00001"}, client); err == nil {
		t.Fatal("want error for failing lookup")
	}
	res, err := checkPreprintCandidate(ctx, map[string]any{"key": "KOK", "extra": "ArXiv: 2401.00002"}, client)
	if err != nil || res.Status != "published" {
		t.Fatalf("res = %+v err = %v, want published", res, err)
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
	flags := &rootFlags{asJSON: true, timeout: time.Second, maxChanges: -1}
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
