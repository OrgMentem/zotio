// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// TestZoteroItemHasChildren pins the prefetch eligibility gate consulted by
// `annotations export` and `annotations timeline` before either fetches
// /items/<key>/children. A regression here makes both commands skip every
// annotation without reporting an error, so each arm of the predicate and each
// numeric representation sqlIntValue accepts is stated as its own case.
// Payloads are decoded from JSON so the numbers arrive as float64, exactly as
// they do from the Zotero API.
func TestZoteroItemHasChildren(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    bool
	}{
		{
			name:    "meta numChildren positive",
			payload: `{"key":"K1","meta":{"numChildren":2}}`,
			want:    true,
		},
		{
			name:    "meta numChildren zero falls through to false",
			payload: `{"key":"K1","meta":{"numChildren":0}}`,
			want:    false,
		},
		{
			name:    "meta numChildren zero falls through to positive data numChildren",
			payload: `{"key":"K1","meta":{"numChildren":0},"data":{"numChildren":1}}`,
			want:    true,
		},
		{
			name:    "nested data numChildren positive without meta",
			payload: `{"key":"K1","data":{"numChildren":3}}`,
			want:    true,
		},
		{
			name:    "numChildren as numeric string",
			payload: `{"key":"K1","meta":{"numChildren":"4"}}`,
			want:    true,
		},
		{
			name:    "numChildren as non-numeric string",
			payload: `{"key":"K1","meta":{"numChildren":"many"}}`,
			want:    false,
		},
		{
			name:    "numChildren as boolean is not a count",
			payload: `{"key":"K1","data":{"numChildren":true}}`,
			want:    false,
		},
		{
			name:    "numChildren as fraction below one truncates to zero",
			payload: `{"key":"K1","meta":{"numChildren":0.5}}`,
			want:    false,
		},
		{
			name:    "numChildren null",
			payload: `{"key":"K1","meta":{"numChildren":null},"data":{"numChildren":null}}`,
			want:    false,
		},
		{
			name:    "links children present with a href value",
			payload: `{"key":"K1","links":{"children":{"href":"https://api.zotero.org/users/0/items/K1/children"}}}`,
			want:    true,
		},
		{
			name:    "links children present with a null value",
			payload: `{"key":"K1","links":{"children":null}}`,
			want:    true,
		},
		{
			name:    "links map present without a children key",
			payload: `{"key":"K1","links":{"self":{"href":"https://api.zotero.org/users/0/items/K1"}}}`,
			want:    false,
		},
		{
			name:    "links map present without a children key while counts are zero",
			payload: `{"key":"K1","meta":{"numChildren":0},"data":{"numChildren":0},"links":{"self":{"href":"x"}}}`,
			want:    false,
		},
		{
			name:    "links map absent and no counts",
			payload: `{"key":"K1","meta":{"numCollections":1},"data":{"itemType":"journalArticle"}}`,
			want:    false,
		},
		{
			name:    "empty item",
			payload: `{}`,
			want:    false,
		},
		{
			name:    "nil item",
			payload: `null`,
			want:    false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var item map[string]any
			if err := json.Unmarshal([]byte(tc.payload), &item); err != nil {
				t.Fatalf("decode payload %s: %v", tc.payload, err)
			}
			if got := zoteroItemHasChildren(item); got != tc.want {
				t.Fatalf("zoteroItemHasChildren(%s) = %t, want %t", tc.payload, got, tc.want)
			}
		})
	}
}

// TestAnnotationsTimelineRequestsChildrenOnlyForParentsWithChildren proves the
// predicate really gates the network call, not just its own return value. The
// fixture serves an annotation for BOTH parents, so a gate that leaks would
// show up twice: an extra request to the childless parent's children endpoint
// and a stray annotation in the results. A gate stuck at false yields no
// children request and no annotations at all.
func TestAnnotationsTimelineRequestsChildrenOnlyForParentsWithChildren(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	var mu sync.Mutex
	childRequests := map[string]int{}
	countChildRequest := func(key string) {
		mu.Lock()
		defer mu.Unlock()
		childRequests[key]++
	}
	childRequestCount := func(key string) int {
		mu.Lock()
		defer mu.Unlock()
		return childRequests[key]
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/users/0/collections/CDEF2345":
			_, _ = w.Write([]byte(`{"key":"CDEF2345","data":{"key":"CDEF2345","name":"Reading"}}`))
		case "/users/0/collections/CDEF2345/items/top":
			_, _ = w.Write([]byte(`[
				{"key":"HASKIDS","meta":{"numChildren":1},"data":{"key":"HASKIDS","itemType":"journalArticle","title":"Parent With Annotations"}},
				{"key":"NOKIDS","meta":{"numChildren":0},"links":{"self":{"href":"https://api.zotero.org/users/0/items/NOKIDS"}},"data":{"key":"NOKIDS","itemType":"journalArticle","title":"Parent Without Annotations","numChildren":0}}
			]`))
		case "/users/0/items/HASKIDS/children":
			countChildRequest("HASKIDS")
			_, _ = w.Write([]byte(`[{"key":"ANNKEPT","version":1,"data":{"key":"ANNKEPT","itemType":"annotation","parentItem":"HASKIDS","dateAdded":"2026-08-08T10:00:00Z","annotationColor":"#ffd400","annotationType":"highlight","annotationText":"A highlighted passage"}}]`))
		case "/users/0/items/NOKIDS/children":
			countChildRequest("NOKIDS")
			_, _ = w.Write([]byte(`[{"key":"ANNLEAKED","version":1,"data":{"key":"ANNLEAKED","itemType":"annotation","parentItem":"NOKIDS","dateAdded":"2026-08-09T10:00:00Z","annotationColor":"#a28ae5","annotationType":"highlight","annotationText":"Must not be fetched"}}]`))
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

	cmd := newAnnotationsTimelineCmd(&rootFlags{asJSON: true, dataSource: "live", noCache: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--collection", "CDEF2345"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("annotations timeline: %v; stderr: %s", err, errOut.String())
	}

	if got := childRequestCount("HASKIDS"); got != 1 {
		t.Fatalf("children requests for the parent with children = %d, want 1", got)
	}
	if got := childRequestCount("NOKIDS"); got != 0 {
		t.Fatalf("children requests for the childless parent = %d, want 0", got)
	}

	env := decodeResultsArrayEnvelope(t, out.Bytes())
	if len(env.Results) != 1 {
		t.Fatalf("results = %#v, want exactly the one annotation of the parent with children", env.Results)
	}
	if env.Results[0]["key"] != "ANNKEPT" || env.Results[0]["parent_item"] != "HASKIDS" {
		t.Fatalf("results[0] = %#v, want annotation ANNKEPT of parent HASKIDS", env.Results[0])
	}
}

// annotationsExportTestServer serves one item (PAPER1) whose PDF (PDF1) holds
// three annotations, shaped like the Zotero local API: annotations hang off
// the attachment, never off the item.
func annotationsExportTestServer(t *testing.T) {
	t.Helper()
	annotation := func(key, added string) string {
		return `{"key":"` + key + `","version":1,"data":{"key":"` + key + `","itemType":"annotation","parentItem":"PDF1","dateAdded":"` + added + `","annotationType":"highlight","annotationText":"text ` + key + `"}}`
	}
	anns := []string{
		annotation("ANN3", "2026-08-03T10:00:00Z"),
		annotation("ANN2", "2026-08-02T10:00:00Z"),
		annotation("ANN1", "2026-08-01T10:00:00Z"),
	}
	paper := `{"key":"PAPER1","version":1,"meta":{"numChildren":1},"data":{"key":"PAPER1","itemType":"journalArticle","title":"A Paper","date":"2024"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/users/0/items" && r.URL.Query().Get("itemType") == "annotation":
			_, _ = w.Write([]byte("[" + strings.Join(anns, ",") + "]"))
		case r.URL.Path == "/users/0/items/PDF1":
			_, _ = w.Write([]byte(`{"key":"PDF1","data":{"key":"PDF1","itemType":"attachment","parentItem":"PAPER1"}}`))
		case r.URL.Path == "/users/0/items/PAPER1":
			_, _ = w.Write([]byte(paper))
		case r.URL.Path == "/users/0/collections/CDEF2345":
			_, _ = w.Write([]byte(`{"key":"CDEF2345","data":{"key":"CDEF2345","name":"Reading"}}`))
		case r.URL.Path == "/users/0/collections/CDEF2345/items/top":
			_, _ = w.Write([]byte("[" + paper + "]"))
		case r.URL.Path == "/users/0/items/PAPER1/children":
			_, _ = w.Write([]byte(`[{"key":"PDF1","data":{"key":"PDF1","itemType":"attachment","parentItem":"PAPER1"}}]`))
		case r.URL.Path == "/users/0/items/PDF1/children":
			_, _ = w.Write([]byte("[" + strings.Join(anns, ",") + "]"))
		default:
			t.Errorf("unexpected request %s", r.URL.String())
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
}

func runAnnotationsExportTest(t *testing.T, flags *rootFlags, args ...string) (string, error) {
	t.Helper()
	cmd := newAnnotationsExportCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// --limit caps the annotations exported, not the parent items scanned: it used
// to be passed to the item listing, so the 50 newest items often held no
// annotations at all and the export came back empty.
func TestAnnotationsExportLimitCapsAnnotationsAndGroupsByTopLevelItem(t *testing.T) {
	annotationsExportTestServer(t)
	out, err := runAnnotationsExportTest(t, &rootFlags{asJSON: true, dataSource: "live", noCache: true}, "--limit", "2")
	if err != nil {
		t.Fatalf("annotations export: %v", err)
	}
	var env struct {
		Meta    DataProvenance         `json:"meta"`
		Results []annotationExportItem `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("decode envelope: %v; output=%q", err, out)
	}
	if env.Meta.Source != "live" || len(env.Results) != 1 {
		t.Fatalf("meta = %+v, results = %+v, want one live item group", env.Meta, env.Results)
	}
	group := env.Results[0]
	if group.Key != "PAPER1" || group.Title != "A Paper" || len(group.Annotations) != 2 {
		t.Fatalf("group = %+v, want PAPER1 with the 2 newest annotations", group)
	}
	if group.Annotations[0].Key != "ANN3" || group.Annotations[1].Key != "ANN2" {
		t.Fatalf("annotations = %+v, want ANN3 then ANN2", group.Annotations)
	}
}

// A collection scope reaches annotations through the item's attachments.
func TestAnnotationsExportScopeWalksAttachments(t *testing.T) {
	annotationsExportTestServer(t)
	out, err := runAnnotationsExportTest(t, &rootFlags{asJSON: true, dataSource: "live", noCache: true}, "--collection", "CDEF2345", "--limit", "0")
	if err != nil {
		t.Fatalf("annotations export: %v", err)
	}
	var env struct {
		Results []annotationExportItem `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("decode envelope: %v; output=%q", err, out)
	}
	if len(env.Results) != 1 || env.Results[0].Key != "PAPER1" || len(env.Results[0].Annotations) != 3 {
		t.Fatalf("results = %+v, want PAPER1 with its 3 annotations", env.Results)
	}
}

// --dry-run reads the source but never touches the output: the existing file
// keeps its bytes and no .lock sibling appears. It used to truncate the file.
func TestAnnotationsExportDryRunLeavesOutputUntouched(t *testing.T) {
	annotationsExportTestServer(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "notes.md")
	if err := os.WriteFile(target, []byte("PRECIOUS"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runAnnotationsExportTest(t, &rootFlags{asJSON: true, dryRun: true, dataSource: "live", noCache: true}, "-o", target)
	if err != nil {
		t.Fatalf("annotations export --dry-run: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "PRECIOUS" {
		t.Fatalf("output after --dry-run = %q (err %v), want it unchanged", got, err)
	}
	if _, err := os.Stat(target + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("--dry-run left a lock sibling: %v", err)
	}
	var result annotationsExportFileResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("decode result: %v; output=%q", err, out)
	}
	if !result.DryRun || !result.Replaced || result.Annotations != 3 {
		t.Fatalf("result = %+v, want dry_run, replaced, 3 annotations", result)
	}
}

// Under --json a markdown export is wrapped as {"format","content"}.
func TestAnnotationsExportMarkdownUnderJSONIsWrapped(t *testing.T) {
	annotationsExportTestServer(t)
	out, err := runAnnotationsExportTest(t, &rootFlags{asJSON: true, dataSource: "live", noCache: true}, "--format", "markdown")
	if err != nil {
		t.Fatalf("annotations export: %v", err)
	}
	var payload struct {
		Format  string `json:"format"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("decode: %v; output=%q", err, out)
	}
	if payload.Format != "markdown" || !strings.Contains(payload.Content, "# A Paper (2024)") {
		t.Fatalf("payload = %+v, want markdown content for A Paper", payload)
	}
}

// Zotero returns 25 records per request unless a limit is given, and at most
// 100 with one. An attachment with more annotations than a page, or an item
// with many attachments, must be read to the end: the export used to stop at
// the first page.
func TestAnnotationsExportPagesThroughAttachmentsAndAnnotations(t *testing.T) {
	const total = 130
	annotations := make([]string, 0, total)
	for i := range total {
		key := fmt.Sprintf("ANN%03d", i)
		annotations = append(annotations, `{"key":"`+key+`","version":1,"data":{"key":"`+key+`","itemType":"annotation","parentItem":"PDF000","dateAdded":"2026-08-01T10:00:00Z","annotationType":"highlight","annotationText":"text `+key+`"}}`)
	}
	attachments := make([]string, 0, 30)
	for i := range 30 {
		key := fmt.Sprintf("PDF%03d", i)
		attachments = append(attachments, `{"key":"`+key+`","data":{"key":"`+key+`","itemType":"attachment","parentItem":"PAPER1"}}`)
	}
	page := func(w http.ResponseWriter, r *http.Request, rows []string) {
		limit, start := 25, 0
		if v := r.URL.Query().Get("limit"); v != "" {
			limit, _ = strconv.Atoi(v)
			limit = min(limit, 100)
		}
		start, _ = strconv.Atoi(r.URL.Query().Get("start"))
		end := min(start+limit, len(rows))
		if start > len(rows) {
			start = len(rows)
		}
		_, _ = w.Write([]byte("[" + strings.Join(rows[start:end], ",") + "]"))
	}
	paper := `{"key":"PAPER1","version":1,"meta":{"numChildren":30},"data":{"key":"PAPER1","itemType":"journalArticle","title":"A Paper"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/users/0/collections/CDEF2345":
			_, _ = w.Write([]byte(`{"key":"CDEF2345","data":{"key":"CDEF2345","name":"Reading"}}`))
		case r.URL.Path == "/users/0/collections/CDEF2345/items/top":
			_, _ = w.Write([]byte("[" + paper + "]"))
		case r.URL.Path == "/users/0/items/PAPER1/children":
			page(w, r, attachments)
		case r.URL.Path == "/users/0/items/PDF000/children":
			page(w, r, annotations)
		case strings.HasPrefix(r.URL.Path, "/users/0/items/PDF") && strings.HasSuffix(r.URL.Path, "/children"):
			_, _ = w.Write([]byte("[]"))
		default:
			t.Errorf("unexpected request %s", r.URL.String())
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

	out, err := runAnnotationsExportTest(t, &rootFlags{asJSON: true, dataSource: "live", noCache: true}, "--collection", "CDEF2345", "--limit", "0")
	if err != nil {
		t.Fatalf("annotations export: %v", err)
	}
	var env struct {
		Results []annotationExportItem `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if len(env.Results) != 1 || len(env.Results[0].Annotations) != total {
		got := 0
		if len(env.Results) > 0 {
			got = len(env.Results[0].Annotations)
		}
		t.Fatalf("exported %d annotations, want all %d across pages", got, total)
	}
}
