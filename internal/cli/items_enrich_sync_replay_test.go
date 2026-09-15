package cli

// Covers the full lifecycle the other enrich tests stop short of: apply ->
// mirror write-through -> pending marker -> the NEXT sync's
// reconcilePendingWrites over the authoritative server row. Recording a
// mirror-derived Extra in the mutation plan was only observable at that last
// step, where it erased the live value locally and left the marker set, so
// the erasure repeated on every subsequent sync until the marker expired.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestItemsEnrichAppliedExtraSurvivesNextSync(t *testing.T) {
	const localExtra = "stale mirror note"
	const liveExtra = "Citation Key: smith2020\nreader's own note"

	crsrv := crossRefSearchServer(t, "Attention Is All You Need", "10.1/attention")
	withBase(t, &enrichCrossRefBase, crsrv.URL)
	db := seedEnrichStore(t, localExtra)

	var patchedExtra string
	zsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/items/K1":
			w.Header().Set("Last-Modified-Version", "42")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"key": "K1", "version": 42,
				"data": map[string]any{"key": "K1", "extra": liveExtra},
			})
		case r.Method == http.MethodPatch && r.URL.Path == "/items/K1":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			patchedExtra = fmt.Sprint(body["extra"])
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(zsrv.Close)
	t.Setenv("ZOTERO_BASE_URL", zsrv.URL)

	oldMirror := mirrorWriteThrough
	mirrorWriteThrough = applyMirrorWriteThrough
	t.Cleanup(func() { mirrorWriteThrough = oldMirror })

	cmd := newItemsEnrichCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1})
	cmd.SetArgs([]string{"--missing-doi", "--no-openalex", "--no-semantic-scholar"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("enrich apply: %v", err)
	}
	t.Logf("PATCH extra = %q", patchedExtra)

	// The next sync serves the authoritative row: Zotero now holds the live
	// Extra plus the provenance line the PATCH appended.
	serverExtra := patchedExtra
	authoritative, _ := json.Marshal(map[string]any{
		"key": "K1", "version": 43,
		"data": map[string]any{
			"key": "K1", "version": 43,
			"extra": serverExtra,
			"DOI":   "10.1/attention",
		},
	})

	merged, stillPending := reconcilePendingWrites(db.Store, "items", []json.RawMessage{authoritative})
	if len(merged) != 1 {
		t.Fatalf("merged rows = %d, want 1", len(merged))
	}

	var got struct {
		Data struct {
			Extra string `json:"extra"`
			DOI   string `json:"DOI"`
		} `json:"data"`
	}
	if err := json.Unmarshal(merged[0], &got); err != nil {
		t.Fatalf("decode merged row: %v", err)
	}

	t.Logf("after sync: extra = %q", got.Data.Extra)
	t.Logf("after sync: still pending = %d", stillPending)

	if got.Data.Extra != serverExtra {
		t.Fatalf("SYNC CLOBBERED EXTRA\n got  %q\n want %q", got.Data.Extra, serverExtra)
	}
	if got.Data.DOI != "10.1/attention" {
		t.Fatalf("applied DOI lost through reconciliation: %q", got.Data.DOI)
	}
	if stillPending != 0 {
		t.Fatalf("pending marker did not clear (stillPending=%d): it would re-apply on every sync", stillPending)
	}
}
