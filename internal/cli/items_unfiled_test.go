// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"zotio/internal/store"
)

func TestItemsUnfiledQueryFiltersOrderingAndLimit(t *testing.T) {
	items := []json.RawMessage{
		json.RawMessage(`{"key":"NULL","version":1,"data":{"key":"NULL","itemType":"book","dateAdded":"2026-01-01"}}`),
		json.RawMessage(`{"key":"EMPTY","version":1,"data":{"key":"EMPTY","itemType":"journalArticle","collections":[],"dateAdded":"2026-01-04"}}`),
		json.RawMessage(`{"key":"NEWBOOK","version":1,"data":{"key":"NEWBOOK","itemType":"book","collections":[],"dateAdded":"2026-01-03"}}`),
		json.RawMessage(`{"key":"FILED","version":1,"data":{"key":"FILED","itemType":"book","collections":["COL1"],"dateAdded":"2026-01-08"}}`),
		json.RawMessage(`{"key":"NOTE","version":1,"data":{"key":"NOTE","itemType":"note","parentItem":"NULL","dateAdded":"2026-01-07"}}`),
		json.RawMessage(`{"key":"ATT","version":1,"data":{"key":"ATT","itemType":"attachment","parentItem":"NULL","dateAdded":"2026-01-06"}}`),
		json.RawMessage(`{"key":"ANNOT","version":1,"data":{"key":"ANNOT","itemType":"annotation","parentItem":"ATT","dateAdded":"2026-01-05"}}`),
	}
	db := seedStoreWithItems(t, items)
	if _, _, err := db.UpsertBatch("items-trash", []json.RawMessage{
		json.RawMessage(`{"key":"TRASHED","version":1,"data":{"key":"TRASHED","itemType":"book","dateAdded":"2026-01-09"}}`),
	}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, itemType string
		limit          int
		want           []string
	}{
		{"all unfiled in descending date order", "", 0, []string{"EMPTY", "NEWBOOK", "NULL"}},
		{"type filter", "book", 0, []string{"NEWBOOK", "NULL"}},
		{"limit follows filtering and order", "", 2, []string{"EMPTY", "NEWBOOK"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := queryUnfiledItems(db, tc.itemType, tc.limit)
			if err != nil {
				t.Fatal(err)
			}
			keys := make([]string, 0, len(rows))
			for _, row := range rows {
				keys = append(keys, sqlStringValue(row["key"]))
			}
			if !reflect.DeepEqual(keys, tc.want) {
				t.Errorf("unfiled keys = %v, want %v", keys, tc.want)
			}
		})
	}
}

func TestItemsUnfiledCommandUsesLocalStore(t *testing.T) {
	seedLocalQueryPlannerDB(t)
	db, err := store.OpenWithContext(t.Context(), helpersTestDefaultDBPath(t, "zotio"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.UpsertBatch("items", []json.RawMessage{
		json.RawMessage(`{"key":"COMMAND","version":1,"data":{"key":"COMMAND","itemType":"book","title":"Unfiled","collections":[],"dateAdded":"2026-01-01"}}`),
	}); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	cmd := newItemsUnfiledCmd(&rootFlags{asJSON: true})
	cmd.SetArgs([]string{"--type", "book"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Results []struct {
			Key string `json:"key"`
		} `json:"results"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatalf("decode command output %q: %v", out.String(), err)
	}
	if len(envelope.Results) != 1 || envelope.Results[0].Key != "COMMAND" {
		t.Errorf("unfiled command results = %v, want [COMMAND]", envelope.Results)
	}
}
