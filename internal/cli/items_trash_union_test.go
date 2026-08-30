// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// items trash must show what zotio just trashed, not only what the read plane knows.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spf13/cobra"

	"zotio/internal/store"
)

// runTrashCmd executes the command and returns its stdout.
func runTrashCmd(t *testing.T, cmd *cobra.Command) string {
	t.Helper()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("items trash: %v (out %s)", err, out.String())
	}
	return out.String()
}

// Walk-test W-4: writes route to api.zotero.org, but `items trash` reads the
// Zotero desktop local API, which does not learn about a trash until Zotero syncs
// it down — it returned an empty trash for an item the web plane already reported
// as deleted. So right after `items delete`, this command could not show what was
// just trashed, and `--data-source local` (normally the LESS current source) was
// the only one that was right. The union fixes that without losing items trashed
// in the Zotero UI.
func TestItemsTrashUnionsMirroredTrash(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dbPath := helpersTestDefaultDBPath(t, "zotio")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	// The mirror knows about a trashed item the read plane has not reported.
	if _, _, err := db.UpsertBatch("items-trash", []json.RawMessage{
		json.RawMessage(`{"key":"PENDING1","version":9,"data":{"key":"PENDING1","itemType":"journalArticle","title":"Trashed by zotio"}}`),
	}); err != nil {
		t.Fatalf("seed mirror trash: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	// The read plane reports one other trashed item (e.g. trashed in the UI).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"key":"UITRASH","data":{"key":"UITRASH","itemType":"book","title":"Trashed in Zotero"}}]`))
	}))
	defer srv.Close()

	cmd := newItemsTrashCmd(&rootFlags{
		configPath: testConfigFile(t, srv.URL+"/users/0"),
		asJSON:     true,
	})
	out := runTrashCmd(t, cmd)

	var env struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("decode envelope from %q: %v", out, err)
	}
	keys := map[string]bool{}
	for _, item := range env.Results {
		if key, _ := item["key"].(string); key != "" {
			keys[key] = true
		}
	}
	if !keys["UITRASH"] {
		t.Errorf("dropped the read plane's trashed item; keys = %v", keys)
	}
	if !keys["PENDING1"] {
		t.Errorf("did not surface the mirror's trashed item, so `items trash` still cannot show what `items delete` just trashed; keys = %v", keys)
	}
}

// The union is auto mode's self-healing answer for the write-through window.
// An explicit --data-source live means "the API only" (resolveRead), so the
// mirror must stay out of it: automation that reads the flag as authoritative
// was handed trashed items the plane never reported, e.g. a trash performed on
// another machine that Zotero has not synced.
func TestItemsTrashLiveDataSourceExcludesMirror(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dbPath := helpersTestDefaultDBPath(t, "zotio")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := db.UpsertBatch("items-trash", []json.RawMessage{
		json.RawMessage(`{"key":"PENDING1","version":9,"data":{"key":"PENDING1","itemType":"journalArticle","title":"Trashed by zotio"}}`),
	}); err != nil {
		t.Fatalf("seed mirror trash: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"key":"UITRASH","data":{"key":"UITRASH","itemType":"book","title":"Trashed in Zotero"}}]`))
	}))
	defer srv.Close()

	cmd := newItemsTrashCmd(&rootFlags{
		configPath: testConfigFile(t, srv.URL+"/users/0"),
		asJSON:     true,
		dataSource: "live",
	})
	out := runTrashCmd(t, cmd)

	var env struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("decode envelope from %q: %v", out, err)
	}
	keys := map[string]bool{}
	for _, item := range env.Results {
		if key, _ := item["key"].(string); key != "" {
			keys[key] = true
		}
	}
	if !keys["UITRASH"] {
		t.Errorf("dropped the read plane's trashed item; keys = %v", keys)
	}
	if keys["PENDING1"] {
		t.Errorf("--data-source live returned a mirror-only row, so the flag does not mean 'the API only'; keys = %v", keys)
	}
}

// The explicit-live branch runs sortAndPaginateTrash, so it owes callers the
// same ordering and page window the union branch provides: dateModified
// descending, missing dates last, key ascending as tie-breaker.
func TestItemsTrashLiveDataSourceSortsAndPaginates(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// Deliberately out of order, with one row missing dateModified.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"key":"OLD","data":{"key":"OLD","itemType":"book","dateModified":"2026-01-01T00:00:00Z"}},
			{"key":"NODATE","data":{"key":"NODATE","itemType":"book"}},
			{"key":"NEW","data":{"key":"NEW","itemType":"book","dateModified":"2026-03-01T00:00:00Z"}},
			{"key":"MID","data":{"key":"MID","itemType":"book","dateModified":"2026-02-01T00:00:00Z"}}
		]`))
	}))
	defer srv.Close()

	cmd := newItemsTrashCmd(&rootFlags{
		configPath: testConfigFile(t, srv.URL+"/users/0"),
		asJSON:     true,
		dataSource: "live",
	})
	cmd.SetArgs([]string{"--start", "1", "--limit", "2"})
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("items trash --data-source live: %v (out %s)", err, buf.String())
	}

	var env struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope from %q: %v", buf.String(), err)
	}
	var got []string
	for _, item := range env.Results {
		key, _ := item["key"].(string)
		got = append(got, key)
	}
	// Full order is NEW, MID, OLD, NODATE; start=1 limit=2 takes MID and OLD.
	want := []string{"MID", "OLD"}
	if len(got) != len(want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("keys = %v, want %v", got, want)
		}
	}
}

// An item both sources know about must appear once.
func TestItemsTrashUnionDeduplicates(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dbPath := helpersTestDefaultDBPath(t, "zotio")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := db.UpsertBatch("items-trash", []json.RawMessage{
		json.RawMessage(`{"key":"BOTH","version":9,"data":{"key":"BOTH","itemType":"book"}}`),
	}); err != nil {
		t.Fatalf("seed mirror trash: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"key":"BOTH","data":{"key":"BOTH","itemType":"book"}}]`))
	}))
	defer srv.Close()

	cmd := newItemsTrashCmd(&rootFlags{
		configPath: testConfigFile(t, srv.URL+"/users/0"),
		asJSON:     true,
	})
	out := runTrashCmd(t, cmd)
	var env struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if len(env.Results) != 1 {
		t.Fatalf("got %d results, want 1 after de-duplication: %s", len(env.Results), out)
	}
}
