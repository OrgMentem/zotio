// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"zotio/internal/connector"
	"zotio/internal/mutation"
)

// SaveItems can commit and still answer 500. When the follow-up lookup that
// would settle it fails, nothing proves the item is absent, so the create must
// be reported as a possibly committed conflict that keeps its session evidence,
// never as a plain failure a caller would retry into a duplicate.
func TestConnectorCreateUnconfirmableSaveErrorIsCommittedConflict(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/connector/saveItems":
			http.Error(w, `{"error":"save failed"}`, http.StatusInternalServerError)
		case "/users/0/items/top":
			http.Error(w, `{"message":"Forbidden"}`, http.StatusForbidden)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	oldConnectorForCreate := connectorForCreate
	connectorForCreate = func(*rootFlags) (*connector.Client, error) {
		return connector.New(srv.URL+"/connector", time.Second), nil
	}
	t.Cleanup(func() { connectorForCreate = oldConnectorForCreate })

	flags := &rootFlags{
		asJSON:     true,
		yes:        true,
		maxChanges: -1,
		configPath: testConfigFile(t, srv.URL+"/users/0"),
		via:        "connector",
		timeout:    time.Second,
	}
	ops := []mutation.Op{{
		ID:      "import.test",
		Key:     "test-key",
		Kind:    "item_create",
		Changes: []mutation.Change{{Field: "source", Add: "test"}},
		Apply: func() (string, any, error) {
			res, err := routeCreateItemVia(context.Background(), flags, "connector", nil, map[string]any{
				"title":    "Maybe Landed",
				"itemType": "journalArticle",
			}, "", false)
			return singleItemCreateApplyResult(res, err, "test-key")
		},
	}}
	env, err := mutation.Run(mutationOptions(flags), "import.test", ops)
	if err == nil {
		t.Fatal("mutation succeeded, want the unresolved create reported")
	}
	if env.Result == nil || len(env.Result.Items) != 1 {
		t.Fatalf("result = %+v, want one item", env.Result)
	}
	item := env.Result.Items[0]
	if item.Status != "conflict" || env.Result.Summary.Applied != 0 {
		t.Fatalf("status = %q summary = %+v, want an unapplied conflict", item.Status, env.Result.Summary)
	}
	reason, ok := item.Reason.(map[string]any)
	if !ok || reason["committed"] != true || reason["session"] == "" || reason["connector_key"] == "" {
		t.Fatalf("reason = %#v, want possibly-committed connector evidence", item.Reason)
	}
}
