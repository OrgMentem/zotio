// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"zotio/internal/store"
)

func TestTagsInventoryAggregationAndCollectionScope(t *testing.T) {
	seedLocalQueryPlannerDB(t)
	db, err := store.OpenWithContext(t.Context(), helpersTestDefaultDBPath(t, "zotio"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.UpsertBatch("items", []json.RawMessage{
		json.RawMessage(`{"key":"I1","version":1,"data":{"key":"I1","itemType":"book","collections":["COL1"],"tags":[{"tag":" Atlas ","type":0},{"tag":"Mixed","type":0},{"tag":"Only"},{"tag":"Alpha"},{"tag":" Variant "},{"tag":"Variant"},{"tag":"ParentChild"},{"tag":"   "}]}}`),
		json.RawMessage(`{"key":"I2","version":1,"data":{"key":"I2","itemType":"book","collections":["COL1"],"tags":[{"tag":"Atlas"},{"tag":"Mixed","type":1},{"tag":"Only"},{"tag":"Alpha"}]}}`),
		json.RawMessage(`{"key":"I3","version":1,"data":{"key":"I3","itemType":"book","collections":["COL2"],"tags":[{"tag":"Atlas"},{"tag":"alpha"}]}}`),
		json.RawMessage(`{"key":"I4","version":1,"data":{"key":"I4","itemType":"book","collections":[],"tags":[{"tag":"Mixed","type":0},{"tag":"Ambiguous"}]}}`),
		json.RawMessage(`{"key":"I5","version":1,"data":{"key":"I5","itemType":"book","collections":["COL1","COL2"],"tags":[{"tag":"Linked"},{"tag":"Ambiguous"}]}}`),
		json.RawMessage(`{"key":"CHILD","version":1,"data":{"key":"CHILD","itemType":"attachment","parentItem":"I1","collections":[],"tags":[{"tag":"Ghost"},{"tag":"Atlas"},{"tag":"ParentChild"}]}}`),
	}); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, _, err := db.UpsertBatch("collections", []json.RawMessage{
		json.RawMessage(`{"key":"FAKE","version":1,"data":{"key":"FAKE","itemType":"book","collections":["COL1"],"tags":[{"tag":"Atlas"}]}}`),
	}); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, collection string
		want             []tagInventoryItem
	}{
		{"all collections", "", []tagInventoryItem{
			{Tag: "Atlas", CollectionCount: 3, LibraryCount: 3, CollectionOnly: true},
			{Tag: "Alpha", CollectionCount: 2, LibraryCount: 2, CollectionOnly: true},
			{Tag: "Ambiguous", CollectionCount: 2, LibraryCount: 2},
			{Tag: "Linked", CollectionCount: 2, LibraryCount: 1, CollectionOnly: true},
			{Tag: "Mixed", CollectionCount: 2, LibraryCount: 3},
			{Tag: "ml", CollectionCount: 2, LibraryCount: 2, CollectionOnly: true},
			{Tag: "nlp", CollectionCount: 2, LibraryCount: 1, CollectionOnly: true},
			{Tag: "Only", CollectionCount: 2, LibraryCount: 2, CollectionOnly: true},
			{Tag: "Variant", CollectionCount: 2, LibraryCount: 1, CollectionOnly: true},
			{Tag: "alpha", CollectionCount: 1, LibraryCount: 1, CollectionOnly: true},
			{Tag: "ParentChild", CollectionCount: 1, LibraryCount: 1, CollectionOnly: true},
		}},
		{"COL1", "COL1", []tagInventoryItem{
			{Tag: "Alpha", CollectionCount: 2, LibraryCount: 2, CollectionOnly: true},
			{Tag: "Atlas", CollectionCount: 2, LibraryCount: 3},
			{Tag: "Mixed", CollectionCount: 2, LibraryCount: 3},
			{Tag: "Only", CollectionCount: 2, LibraryCount: 2, CollectionOnly: true},
			{Tag: "Variant", CollectionCount: 2, LibraryCount: 1, CollectionOnly: true},
			{Tag: "Ambiguous", CollectionCount: 1, LibraryCount: 2},
			{Tag: "Linked", CollectionCount: 1, LibraryCount: 1, CollectionOnly: true},
			{Tag: "ml", CollectionCount: 1, LibraryCount: 2},
			{Tag: "nlp", CollectionCount: 1, LibraryCount: 1, CollectionOnly: true},
			{Tag: "ParentChild", CollectionCount: 1, LibraryCount: 1, CollectionOnly: true},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newTagsInventoryCmd(&rootFlags{asJSON: true})
			if tc.collection != "" {
				cmd.SetArgs([]string{"--collection", tc.collection})
			}
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&bytes.Buffer{})
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			var got []tagInventoryItem
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatalf("decode inventory %q: %v", out.String(), err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("inventory = %+v, want %+v", got, tc.want)
			}
		})
	}
}
