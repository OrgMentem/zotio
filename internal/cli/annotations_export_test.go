// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
		case "/users/0/collections/COLLA/items":
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
	cmd.SetArgs([]string{"--collection", "COLLA"})

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
