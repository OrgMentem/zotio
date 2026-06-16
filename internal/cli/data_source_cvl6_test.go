// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean cvl6): fixture-backed parity tests proving --data-source local
// reproduces the live endpoint's scoped key sets and ordering.

package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"zotero-pp-cli/internal/store"
)

// cvl6 seed items in arbitrary insertion order; the local planner must sort and
// filter them independently to match the live (pre-sorted) fixtures.
var cvl6Items = []json.RawMessage{
	json.RawMessage(`{"key":"A","version":1,"data":{"key":"A","itemType":"journalArticle","title":"Beta","date":"2020","collections":["COL1"],"tags":[{"tag":"ml"}],"creators":[{"lastName":"Zhang"}]}}`),
	json.RawMessage(`{"key":"B","version":1,"data":{"key":"B","itemType":"journalArticle","title":"Alpha","date":"2022","collections":["COL1","COL2"],"tags":[{"tag":"nlp"}]}}`),
	json.RawMessage(`{"key":"C","version":1,"data":{"key":"C","itemType":"book","title":"Gamma","date":"2019","collections":["COL2"],"tags":[{"tag":"ml"}]}}`),
}

func cvl6SeedLocalDB(t *testing.T) {
	t.Helper()
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	t.Setenv("ZOTERO_BASE_URL", "http://127.0.0.1:1/api/users/0") // unused in local mode
	dbPath := defaultDBPath("zotero-pp-cli")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := db.UpsertBatch("items", cvl6Items); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func runItemsListKeys(t *testing.T, flags *rootFlags, args []string) []string {
	t.Helper()
	cmd := newItemsListCmd(flags)
	cmd.SetArgs(args)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("items list %v: %v", args, err)
	}
	var env struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	keys := make([]string, 0, len(env.Results))
	for _, r := range env.Results {
		keys = append(keys, r["key"].(string))
	}
	return keys
}

func equalKeys(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestItemsListLocalParity_ItemTypeSort asserts the live and local key order
// match for an itemType-filtered, title-sorted query.
func TestItemsListLocalParity_ItemTypeSort(t *testing.T) {
	// Live fixture returns the API's already-filtered+sorted truth: [B, A].
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[` + string(cvl6Items[1]) + `,` + string(cvl6Items[0]) + `]`))
	}))
	defer srv.Close()

	args := []string{"--item-type", "journalArticle", "--sort", "title", "--direction", "asc"}

	t.Setenv("ZOTERO_BASE_URL", srv.URL)
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	liveFlags := &rootFlags{asJSON: true, dataSource: "live", noCache: true, timeout: time.Second}
	liveKeys := runItemsListKeys(t, liveFlags, args)
	if want := []string{"B", "A"}; !equalKeys(liveKeys, want) {
		t.Fatalf("live keys = %v, want %v", liveKeys, want)
	}

	cvl6SeedLocalDB(t)
	localFlags := &rootFlags{asJSON: true, dataSource: "local", noCache: true, timeout: time.Second}
	localKeys := runItemsListKeys(t, localFlags, args)

	if !equalKeys(localKeys, liveKeys) {
		t.Fatalf("local keys = %v, want parity with live %v", localKeys, liveKeys)
	}
}

// TestItemsListLocalParity_Tag asserts tag-scoped local results match the live
// truth (tag=ml, title asc -> [A, C]).
func TestItemsListLocalParity_Tag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[` + string(cvl6Items[0]) + `,` + string(cvl6Items[2]) + `]`))
	}))
	defer srv.Close()

	args := []string{"--tag", "ml", "--sort", "title", "--direction", "asc"}

	t.Setenv("ZOTERO_BASE_URL", srv.URL)
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	liveFlags := &rootFlags{asJSON: true, dataSource: "live", noCache: true, timeout: time.Second}
	liveKeys := runItemsListKeys(t, liveFlags, args)
	if want := []string{"A", "C"}; !equalKeys(liveKeys, want) {
		t.Fatalf("live keys = %v, want %v", liveKeys, want)
	}

	cvl6SeedLocalDB(t)
	localFlags := &rootFlags{asJSON: true, dataSource: "local", noCache: true, timeout: time.Second}
	localKeys := runItemsListKeys(t, localFlags, args)
	if !equalKeys(localKeys, liveKeys) {
		t.Fatalf("local tag keys = %v, want parity with live %v", localKeys, liveKeys)
	}
}

// TestItemsListLocalNoMatchEmptyArray confirms an unmatched local scope returns
// an empty array, not an error (matching a live empty list).
func TestItemsListLocalNoMatchEmptyArray(t *testing.T) {
	cvl6SeedLocalDB(t)
	flags := &rootFlags{asJSON: true, dataSource: "local", noCache: true, timeout: time.Second}
	keys := runItemsListKeys(t, flags, []string{"--tag", "doesnotexist"})
	if len(keys) != 0 {
		t.Fatalf("expected no keys, got %v", keys)
	}
}
