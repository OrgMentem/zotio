// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"zotio/internal/store"
)

// zaIsolate gives the test an isolated HOME and store path, and points the Web
// API at a server that fails the test on any request: a refused argument list
// must never reach Zotero.
func zaIsolate(t *testing.T) {
	t.Helper()
	isolateDemoEnv(t, "0")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected request", http.StatusTeapot)
	}))
	t.Cleanup(server.Close)
	t.Setenv("ZOTERO_BASE_URL", server.URL+"/users/0")
	t.Setenv("ZOTERO_API_KEY", "za-test-key")
	t.Setenv("ZOTERO_PROFILE", "")
	t.Setenv("ZOTERO_GROUP", "")
}

// zaExecuteRoot runs the real command tree the way the CLI and the MCP mirror
// do. Not parallel-safe: RootCmd binds package-level flag globals.
func zaExecuteRoot(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := RootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs(args)
	err := ExecuteRootInProcess(context.Background(), root)
	return out.String(), err
}

// A missing required positional used to print help and exit 0, so a script
// whose key variable came out empty recorded `items delete` or `searches
// materialize --yes` as a completed step. It must be a usage error (exit 2)
// that names what is missing, while an explicit --help still exits 0.
func TestZaMissingPositionalIsUsageError(t *testing.T) {
	zaIsolate(t)
	for _, tc := range []struct {
		args  []string
		names []string
	}{
		{args: []string{"collections", "delete"}, names: []string{"<collectionKey>"}},
		{args: []string{"collections", "export"}, names: []string{"<collectionKey>"}},
		{args: []string{"collections", "gaps"}, names: []string{"<collectionKey>"}},
		{args: []string{"collections", "get"}, names: []string{"<collectionKey>"}},
		{args: []string{"collections", "items"}, names: []string{"<collectionKey>"}},
		{args: []string{"collections", "move", "--to", "PARENT01"}, names: []string{"<collectionKey>"}},
		{args: []string{"collections", "stats"}, names: []string{"<collectionKey>"}},
		{args: []string{"collections", "subcollections"}, names: []string{"<collectionKey>"}},
		{args: []string{"collections", "tags"}, names: []string{"<collectionKey>"}},
		{args: []string{"collections", "update"}, names: []string{"<collectionKey>"}},
		{args: []string{"import", "doi"}, names: []string{"<doi>"}},
		{args: []string{"import", "file"}, names: []string{"<path>"}},
		{args: []string{"import", "pmid"}, names: []string{"<pmid>"}},
		{args: []string{"import", "arxiv"}, names: []string{"<id>"}},
		{args: []string{"import", "isbn"}, names: []string{"<isbn>"}},
		{args: []string{"import", "url"}, names: []string{"<url>"}},
		{args: []string{"items", "annotations"}, names: []string{"<itemKey>"}},
		{args: []string{"items", "bibcheck"}, names: []string{"<manuscript...>"}},
		{args: []string{"items", "children"}, names: []string{"<itemKey>"}},
		{args: []string{"items", "cite"}, names: []string{"<itemKey>"}},
		{args: []string{"items", "collections-of"}, names: []string{"<itemKey>"}},
		{args: []string{"items", "delete", "--yes"}, names: []string{"<itemKey>"}},
		{args: []string{"items", "file"}, names: []string{"<itemKey>"}},
		{args: []string{"items", "fulltext"}, names: []string{"<itemKey>"}},
		{args: []string{"items", "get"}, names: []string{"<itemKey>"}},
		{args: []string{"items", "note-template"}, names: []string{"<itemKey>"}},
		{args: []string{"items", "open"}, names: []string{"<key>"}},
		{args: []string{"items", "restore"}, names: []string{"<itemKey>"}},
		// summarize also takes a cohort, so the refusal names every accepted input.
		{args: []string{"items", "summarize"}, names: []string{"<itemKey>", "--collection", "--scope"}},
		{args: []string{"items", "update", "--title", "T"}, names: []string{"<itemKey>"}},
		{args: []string{"search"}, names: []string{"<query>"}},
		{args: []string{"annotations", "search"}, names: []string{"<query>"}},
		{args: []string{"searches", "get"}, names: []string{"<searchKey>"}},
		{args: []string{"searches", "materialize", "--to", "COLL0001", "--yes"}, names: []string{"<searchKey>"}},
		{args: []string{"searches", "run"}, names: []string{"<searchKey>"}},
		{args: []string{"tags", "get"}, names: []string{"<tagName>"}},
		{args: []string{"vault", "resolve", "--keep-vault"}, names: []string{"<citekey-or-item-key>"}},
		// Commands that already declared a Cobra arity bound share the same
		// mechanism, so they refuse with the same code and the same naming.
		{args: []string{"items", "tags", "list"}, names: []string{"<itemKey>"}},
		{args: []string{"items", "get", "K1", "K2"}, names: []string{"<itemKey>"}},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			out, err := zaExecuteRoot(t, tc.args...)
			if err == nil {
				t.Fatalf("succeeded (stdout=%q), want a usage error naming %v", out, tc.names)
			}
			if code := ExitCode(err); code != 2 {
				t.Errorf("exit %d (%v), want 2 (usage)", code, err)
			}
			for _, name := range tc.names {
				if !strings.Contains(err.Error(), name) {
					t.Errorf("error = %q, want it to name %s", err.Error(), name)
				}
			}
			if out != "" {
				t.Errorf("stdout = %q, want nothing: a refused argument list is not a help request", out)
			}

			if _, err := zaExecuteRoot(t, slices.Concat(tc.args, []string{"--help"})...); err != nil {
				t.Errorf("--help = %v (exit %d), want help and exit 0", err, ExitCode(err))
			}
		})
	}
}

// Zero positionals stay legal when the command takes its input another way:
// `items summarize` reads a cohort from --collection or --scope.
func TestZaSummarizeCohortNeedsNoPositional(t *testing.T) {
	zaIsolate(t)
	db, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	items := []json.RawMessage{
		json.RawMessage(`{"key":"ZAITEM01","version":1,"data":{"key":"ZAITEM01","itemType":"journalArticle","title":"Alpha","collections":["ZACOLL01"]}}`),
		json.RawMessage(`{"key":"ZAITEM02","version":1,"data":{"key":"ZAITEM02","itemType":"journalArticle","title":"Beta"}}`),
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed items: %v", err)
	}
	if err := db.SaveSyncState("items", "", len(items)); err != nil {
		t.Fatalf("save sync state: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	for _, input := range [][]string{
		{"--collection", "ZACOLL01"},
		{"--scope", "collection:ZACOLL01"},
	} {
		t.Run(strings.Join(input, " "), func(t *testing.T) {
			args := slices.Concat([]string{"items", "summarize", "--json", "--no-fulltext"}, input)
			out, err := zaExecuteRoot(t, args...)
			if err != nil {
				t.Fatalf("items summarize %v = %v (exit %d), want the cohort bundle", input, err, ExitCode(err))
			}
			var bundle summarizeCollectionBundle
			if err := json.Unmarshal([]byte(out), &bundle); err != nil {
				t.Fatalf("decode bundle %q: %v", out, err)
			}
			if bundle.ItemCount != 1 || len(bundle.Items) != 1 || bundle.Items[0].Key != "ZAITEM01" {
				t.Fatalf("bundle = %+v, want only ZAITEM01 from ZACOLL01", bundle)
			}
		})
	}
}
