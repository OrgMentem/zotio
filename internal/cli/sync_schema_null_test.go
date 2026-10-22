// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"zotio/internal/store"
)

// A per-item-type schema payload that is JSON null instead of the endpoint's
// array used to be stored as-is and then stamped with the current schema
// version, so the version check vouched for it forever and a deep schema
// reader decoded it as "this type has no fields".
func TestSyncDependentSchemaRejectsNullPayloadAndRepairs(t *testing.T) {
	syncTestWithHumanFriendly(t, false)

	var mu sync.Mutex
	nulls := map[string]bool{} // "<endpoint>?<itemType>" answered with null
	hits := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Zotero-Schema-Version", "100")
		switch r.URL.Path {
		case "/itemTypes":
			fmt.Fprint(w, `[{"itemType":"book"},{"itemType":"journalArticle"}]`)
		case "/itemTypeFields", "/itemTypeCreatorTypes":
			itemType := r.URL.Query().Get("itemType")
			mu.Lock()
			hits[r.URL.Path]++
			isNull := nulls[r.URL.Path+"?"+itemType]
			mu.Unlock()
			switch {
			case isNull:
				fmt.Fprint(w, `null`)
			case r.URL.Path == "/itemTypeFields":
				fmt.Fprint(w, `[{"field":"title"},{"field":"date"}]`)
			default:
				fmt.Fprint(w, `[{"creatorType":"author"}]`)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	db := syncTestOpenStore(t)
	defer db.Close()
	c := syncTestClient(server.URL + "/users/0")
	if res := syncResource(context.Background(), c, db, "schema", 0, false, 0, false); res.Err != nil {
		t.Fatalf("schema prerequisite sync: %v", res.Err)
	}
	setNull := func(key string, on bool) {
		mu.Lock()
		nulls[key] = on
		mu.Unlock()
	}
	hitCount := func(path string) int {
		mu.Lock()
		defer mu.Unlock()
		return hits[path]
	}

	for _, tc := range []struct {
		resource string
		endpoint string
		get      func(*store.Store, string) (json.RawMessage, error)
	}{
		{"schema-item-type-fields", "/itemTypeFields", (*store.Store).SchemaItemTypeFields},
		{"schema-item-type-creator-types", "/itemTypeCreatorTypes", (*store.Store).SchemaItemTypeCreatorTypes},
	} {
		t.Run(tc.resource, func(t *testing.T) {
			// One item type answers null: the sync fails and nothing is stamped.
			setNull(tc.endpoint+"?book", true)
			res := syncResource(context.Background(), c, db, tc.resource, 0, false, 0, false)
			if res.Err == nil {
				t.Fatal("sync accepted a null per-type payload, want an error")
			}
			if version, err := db.ZoteroSchemaVersion(tc.resource); err != nil || version != "" {
				t.Fatalf("schema checkpoint after a null payload = %q (err %v), want none", version, err)
			}
			if row, err := tc.get(db, "book"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("cached row for the null type = %s (err %v), want none stored", row, err)
			}

			// The same schema version, healthy now: an ordinary sync repairs it.
			setNull(tc.endpoint+"?book", false)
			if res := syncResource(context.Background(), c, db, tc.resource, 0, false, 0, false); res.Err != nil || res.Count != 2 {
				t.Fatalf("recovery sync = %+v, want 2 rows and no error", res)
			}
			if version, _ := db.ZoteroSchemaVersion(tc.resource); version != "100" {
				t.Fatalf("schema checkpoint after recovery = %q, want 100", version)
			}

			// A row poisoned by an earlier release (null, with the version
			// stamped current) is incomplete, not valid: the next ordinary
			// sync refetches instead of skipping.
			if _, err := db.UpsertKeyedContext(context.Background(), tc.resource, []string{"book"}, []json.RawMessage{json.RawMessage(`null`)}); err != nil {
				t.Fatalf("poison cached row: %v", err)
			}
			before := hitCount(tc.endpoint)
			if res := syncResource(context.Background(), c, db, tc.resource, 0, false, 0, false); res.Err != nil || res.Count != 2 {
				t.Fatalf("repair sync = %+v, want 2 rows and no error", res)
			}
			if hitCount(tc.endpoint) == before {
				t.Fatal("sync skipped a poisoned cache because the schema version matched")
			}
			row, err := tc.get(db, "book")
			if err != nil {
				t.Fatalf("cached row after repair: %v", err)
			}
			if string(row) == "null" {
				t.Fatal("cached row is still null after the repair sync")
			}
		})
	}

	// A deep-schema reader must refuse a null row, not read it as an empty list.
	if _, err := db.UpsertKeyedContext(context.Background(), "schema-item-type-fields", []string{"book"}, []json.RawMessage{json.RawMessage(`null`)}); err != nil {
		t.Fatalf("poison cached row: %v", err)
	}
	if _, _, available, err := cachedDeepSchema(db, []string{"book", "journalArticle"}, "100"); !available || !errors.Is(err, errDeepSchemaCachePrecondition) {
		t.Errorf("cachedDeepSchema over a null row: available=%v err=%v, want a deep-schema cache precondition refusal", available, err)
	}
	if _, err := decodeSchemaList(json.RawMessage(`null`), "/itemTypeFields", "field"); err == nil {
		t.Error("decodeSchemaList accepted null, want an error")
	}
}
