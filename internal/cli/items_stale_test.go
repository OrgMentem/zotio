// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"
	"zotio/internal/store"
)

func TestItemsStaleFiltersAndLimit(t *testing.T) {
	today := time.Now().UTC()
	date := func(days int) string {
		return today.AddDate(0, 0, -days).Format("2006-01-02") + "T12:00:00Z"
	}
	item := func(key, typ string, days int) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(`{"key":%q,"version":1,"data":{"key":%q,"itemType":%q,"title":%q,"dateAdded":%q}}`, key, key, typ, key, date(days)))
	}
	child := func(key, typ, parent, contentType string) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(`{"key":%q,"version":1,"data":{"key":%q,"itemType":%q,"parentItem":%q,"contentType":%q,"dateAdded":%q}}`, key, key, typ, parent, contentType, date(50)))
	}
	db := seedStoreWithItems(t, []json.RawMessage{
		item("OLD", "book", 40), item("NONPDF", "book", 39),
		item("PDF", "book", 38), item("ANNOT", "book", 37),
		item("BOTH", "book", 36), item("INSIDE", "book", 31),
		item("BOUNDARY", "book", 30), item("YOUNG", "book", 29),
		child("NOTE", "note", "OLD", ""),
		child("OTHER", "attachment", "NONPDF", "text/plain"),
		child("PDFCHILD", "attachment", "PDF", "application/pdf"),
		child("ANNOTCHILD", "annotation", "ANNOT", ""),
		child("BOTH_PDF", "attachment", "BOTH", "application/pdf"),
		child("BOTH_ANNOT", "annotation", "BOTH", ""),
	})
	for _, tc := range []struct {
		name          string
		noPDF, noAnno bool
		limit         int
		want          []string
	}{
		{"default excludes both", false, false, 0, []string{"OLD", "NONPDF", "INSIDE", "BOUNDARY"}},
		{"only PDF excluded", true, false, 0, []string{"OLD", "NONPDF", "ANNOT", "INSIDE", "BOUNDARY"}},
		{"only annotation excluded", false, true, 0, []string{"OLD", "NONPDF", "PDF", "INSIDE", "BOUNDARY"}},
		{"both excluded", true, true, 0, []string{"OLD", "NONPDF", "INSIDE", "BOUNDARY"}},
		{"limit after age ordering", true, true, 2, []string{"OLD", "NONPDF"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := queryStaleItems(db, 30, tc.noPDF, tc.noAnno, tc.limit)
			if err != nil {
				t.Fatal(err)
			}
			keys := make([]string, 0, len(rows))
			for _, row := range rows {
				keys = append(keys, sqlStringValue(row["key"]))
			}
			if !reflect.DeepEqual(keys, tc.want) {
				t.Errorf("stale keys = %v, want %v", keys, tc.want)
			}
		})
	}
}

func TestItemsStaleCommandEnvelopeAndInvalidDays(t *testing.T) {
	cmd := newItemsStaleCmd(&rootFlags{asJSON: true})
	cmd.SetArgs([]string{"--days", "-1"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil || err.Error() != "--days must be non-negative" {
		t.Fatalf("negative days error = %v, want --days must be non-negative", err)
	}

	seedLocalQueryPlannerDB(t)
	db, err := store.OpenWithContext(t.Context(), helpersTestDefaultDBPath(t, "zotio"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.UpsertBatch("items", []json.RawMessage{
		json.RawMessage(`{"key":"STALE_COMMAND","version":1,"data":{"key":"STALE_COMMAND","itemType":"book","dateAdded":"2020-01-01"}}`),
	}); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	cmd = newItemsStaleCmd(&rootFlags{asJSON: true})
	cmd.SetArgs([]string{"--days", "30"})
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
	if len(envelope.Results) != 1 || envelope.Results[0].Key != "STALE_COMMAND" {
		t.Errorf("stale command results = %v, want [STALE_COMMAND]", envelope.Results)
	}
}
