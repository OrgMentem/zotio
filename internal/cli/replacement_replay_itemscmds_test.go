package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"zotio/internal/mutation"
)

func TestItemsCmdsReplacementPreviousListsWriteThrough(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	t.Setenv("ZOTERO_API_KEY", "test-key")
	const key = "ABCD2345"
	const raw = `{"key":"ABCD2345","version":7,"data":{"key":"ABCD2345","version":7,"itemType":"journalArticle","title":"Paper","tags":[{"tag":"old"}],"collections":["EFGH6789"]}}`
	dbPath := helpersTestDefaultDBPath(t, "zotio")
	itemsUpdateSeedMirrorRow(t, dbPath, raw)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Last-Modified-Version", "7")
			_, _ = io.WriteString(w, raw)
		case http.MethodPatch:
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected method %s", r.Method)
			http.Error(w, "unexpected", http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()
	t.Setenv("ZOTERO_BASE_URL", server.URL+"/users/0")
	previousHook := mirrorWriteThrough
	mirrorWriteThrough = applyMirrorWriteThrough
	defer func() { mirrorWriteThrough = previousHook }()
	cmd := newItemsUpdateCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{key, "--tags", `[{"tag":"new"}]`, "--collections", `["JKLM2345"]`})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("update: %v: %s", err, out.String())
	}
	var env mutation.Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Result == nil || env.Result.Summary.Applied != 1 {
		t.Fatalf("result: %+v", env.Result)
	}
	for _, change := range env.Plan.Operations[0].Changes {
		if change.Remove == nil {
			t.Fatalf("previous list missing: %+v", change)
		}
	}
	if tags := itemsUpdateMirroredTags(t, dbPath, key); len(tags) != 1 || tags[0]["tag"] != "new" {
		t.Fatalf("mirror tags: %v", tags)
	}
	if cols := itemsUpdateMirroredCollections(t, dbPath, key); len(cols) != 1 || cols[0] != "JKLM2345" {
		t.Fatalf("mirror collections: %v", cols)
	}
	if !itemsUpdatePendingMarkerPresent(t, dbPath, key) {
		t.Fatal("replacement has no pending-write marker")
	}
}

func TestItemsCmdsReplacementRejectsMalformedPreviousLists(t *testing.T) {
	for _, tc := range []struct {
		name   string
		apply  func(map[string]any, mutation.Change) bool
		add    any
		remove any
	}{
		{"tags scalar", applyTagsSetChangeToData, []any{}, "x"},
		{"tags invalid name", applyTagsSetChangeToData, []any{}, []any{map[string]any{"tag": 3}}},
		{"tags invalid type", applyTagsSetChangeToData, []any{}, []any{map[string]any{"tag": "old", "type": "invalid"}}},
		{"collections scalar", applyCollectionsSetChangeToData, []string{}, "x"},
		{"collections invalid key", applyCollectionsSetChangeToData, []string{}, []any{3}},
		{"collections empty key", applyCollectionsSetChangeToData, []string{}, []string{""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := map[string]any{}
			if tc.apply(data, mutation.Change{Add: tc.add, Remove: tc.remove}) {
				t.Fatal("malformed previous list replayed")
			}
			if len(data) != 0 {
				t.Fatalf("rejected change modified data: %v", data)
			}
		})
	}
}
