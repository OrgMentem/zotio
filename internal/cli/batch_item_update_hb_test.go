// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A 2xx batch whose failed entry is not a {code, message} object passes the
// envelope check, but the object was still rejected in some way. Decoding the
// whole failed map at once used to drop every failure and report the rejected
// object applied; it must now report that object's outcome as unknown while
// the well-formed sibling outcome is kept.
func TestBatchItemUpdaterMalformedFailedEntryIsNotApplied(t *testing.T) {
	for name, failed := range map[string]any{
		"string entry":      "rejected",
		"string code":       map[string]any{"code": "412", "message": "stale"},
		"null entry":        nil,
		"missing code":      map[string]any{"message": "rejected"},
		"non-string reason": map[string]any{"code": 400, "message": map[string]any{"why": "bad"}},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"successful": map[string]any{"1": map[string]any{"key": "K2", "version": 2}},
					"unchanged":  map[string]any{},
					"failed":     map[string]any{"0": failed},
				})
			}))
			t.Cleanup(server.Close)
			t.Setenv("ZOTERO_BASE_URL", server.URL+"/users/0")
			t.Setenv("ZOTERO_CONFIG", t.TempDir()+"/missing.toml")
			c, err := (&rootFlags{}).newClient()
			if err != nil {
				t.Fatal(err)
			}
			updater := newBatchItemUpdater(c, "/items", "items.tags.add", []map[string]any{
				{"key": "K1", "version": 1, "tags": []map[string]any{{"tag": "sweep"}}},
				{"key": "K2", "version": 1, "tags": []map[string]any{{"tag": "sweep"}}},
			})

			status, reason, _ := updater.outcome(0)
			if status == "applied" {
				t.Fatalf("K1 status = applied, want an unknown outcome for a malformed failure entry")
			}
			if !strings.Contains(fmt.Sprint(reason), "outcome is unknown") {
				t.Errorf("K1 reason = %v, want the unknown-outcome detail", reason)
			}
			if status, _, _ := updater.outcome(1); status != "applied" {
				t.Errorf("K2 status = %q, want applied: its success was attributed", status)
			}
			if updater.Err() == nil || !strings.Contains(updater.Err().Error(), "malformed failure") {
				t.Errorf("Err() = %v, want the malformed-failure detail", updater.Err())
			}
		})
	}
}
