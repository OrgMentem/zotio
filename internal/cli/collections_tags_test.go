// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestCollectionsTagsAutoWriteThroughTargetsTags runs a live auto-mode
// `collections tags` against a test server and then opens the SQLite mirror:
// the tag row must be stored under the tags resource, and no tag payload may
// be present under the collections resource.
func TestCollectionsTagsAutoWriteThroughTargetsTags(t *testing.T) {
	collChildCacheIsolateMirror(t)
	const tagPayload = `{"tag":"AutoTag","meta":{"type":0,"numItems":1}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/collections/CLCTAB23":
			// The existence check every collection-scoped command runs first.
			_, _ = w.Write([]byte(`{"key":"CLCTAB23","version":1,"data":{"key":"CLCTAB23","name":"Mine","parentCollection":false}}`))
		case "/collections/CLCTAB23/tags":
			_, _ = w.Write([]byte(`[` + tagPayload + `]`))
		default:
			t.Errorf("path = %q, want the collection or its tags", r.URL.Path)
		}
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL)

	flags := &rootFlags{asJSON: true, dataSource: "auto", noCache: true, timeout: 5 * time.Second}
	cmd := newCollectionsTagsCmd(flags)
	cmd.SetArgs([]string{"CLCTAB23"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("collections tags: %v", err)
	}
	env := collChildCacheDecodeEnvelope(t, out.String())
	if env.Meta.Source != "live" {
		t.Fatalf("provenance source = %q, want live", env.Meta.Source)
	}
	if !strings.Contains(string(env.Results), "AutoTag") {
		t.Fatalf("results = %s, want the live AutoTag row", env.Results)
	}

	mirror := collChildCacheOpenMirror(t, helpersTestDefaultDBPath(t, "zotio"))
	if n, err := mirror.Count("collections"); err != nil || n != 1 {
		t.Fatalf("collections rows = %d, err = %v; want only the verified collection, no tag payload", n, err)
	}
	rows, err := mirror.List("tags", 0)
	if err != nil {
		t.Fatalf("list tags: %v", err)
	}
	if len(rows) != 1 || !strings.Contains(string(rows[0]), "AutoTag") {
		t.Fatalf("tags rows = %q, want exactly the live AutoTag row", rows)
	}

	// A local collections list against this mirror holds the verified
	// collection and never exposes the cached tag row as a collection.
	localFlags := &rootFlags{asJSON: true, dataSource: "local", noCache: true, timeout: time.Second}
	localData, _, err := resolveRead(context.Background(), nil, localFlags, "collections", false, "/collections", nil, nil)
	if err != nil {
		t.Fatalf("local collections list: %v", err)
	}
	if !strings.Contains(string(localData), "CLCTAB23") || strings.Contains(string(localData), "AutoTag") {
		t.Fatalf("local collections = %s, want only the verified collection", localData)
	}
}
