// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"zotio/internal/client"
	"zotio/internal/mutation"
)

func TestSingleCreateAmbiguousTransportIsJournaled(t *testing.T) {
	setupCreateRouteJournal(t)
	fastRetryBackoff(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Zotero-Write-Token") == "" {
			t.Error("missing write token")
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close()
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	flags := &rootFlags{yes: true, maxChanges: -1}
	c, err := flags.newClient()
	if err != nil {
		t.Fatal(err)
	}
	var routeErr error
	env, err := runMutation(context.Background(), flags, "items.new", []mutation.Op{{
		ID: "create", Kind: "item_create", Changes: []mutation.Change{{Field: "title", Add: "Example"}},
		Apply: func() (string, any, error) {
			var res itemCreateResult
			res, routeErr = routeCreateItemVia(context.Background(), flags, "web", c, map[string]any{"title": "Example", "itemType": "book"}, "", false)
			return singleItemCreateApplyResult(res, routeErr, "")
		},
	}})
	if err == nil || !client.IsAmbiguousWriteError(routeErr) {
		t.Fatalf("expected ambiguous transport error: route=%v mutation=%v", routeErr, err)
	}
	assertCreateRouteUnknown(t, env)
}
