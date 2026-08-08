// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestItemsCollectionsOfJSONUsesReadEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/users/0/items/ITEM1":
			_, _ = w.Write([]byte(`{"data":{"key":"ITEM1","collections":["COLL1"]}}`))
		case "/users/0/collections/COLL1":
			_, _ = w.Write([]byte(`{"data":{"key":"COLL1","name":"Reading"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	flags := &rootFlags{asJSON: true}
	cmd := newItemsCollectionsOfCmd(flags)
	cmd.SetArgs([]string{"ITEM1"})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("items collections-of: %v", err)
	}
	var envelope struct {
		Meta    map[string]any      `json:"meta"`
		Results []itemCollectionRow `json:"results"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatalf("decode envelope: %v (output=%s)", err, out.String())
	}
	if len(envelope.Results) != 1 || envelope.Results[0].Key != "COLL1" || envelope.Results[0].Name != "Reading" {
		t.Fatalf("results = %+v, want COLL1/Reading", envelope.Results)
	}
	if envelope.Meta["source"] == nil {
		t.Fatalf("meta = %+v, want provenance source", envelope.Meta)
	}
}
