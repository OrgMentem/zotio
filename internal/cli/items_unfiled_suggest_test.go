// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"zotio/internal/store"
)

func seedUnfiledSuggestions(t *testing.T) {
	t.Helper()
	isolateItemsSimilarStore(t)
	db, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var items []json.RawMessage
	addItem := func(key, collection, tag, creator, venue, itemType string) {
		t.Helper()
		collections := []string{}
		if collection != "" {
			collections = append(collections, collection)
		}
		data, err := json.Marshal(map[string]any{
			"key": key, "version": 1,
			"data": map[string]any{
				"key": key, "itemType": itemType, "title": key, "dateAdded": "2026-01-01",
				"collections": collections, "tags": []map[string]string{{"tag": tag}},
				"creators": []map[string]string{{"name": creator}}, "publicationTitle": venue,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, data)
	}
	for i := 1; i <= 3; i++ {
		addItem(fmt.Sprintf("A%d", i), "COLA", "botany", "Ada Leaf", "Plant Review", "journalArticle")
		addItem(fmt.Sprintf("B%d", i), "COLB", "astronomy", "Bea Star", "Space Review", "journalArticle")
	}
	addItem("C1", "COLC", "botany", "Ada Leaf", "Plant Review", "journalArticle")
	addItem("C2", "COLC", "geology", "Geo Rock", "Earth Review", "journalArticle")
	addItem("C3", "COLC", "history", "His Archive", "Past Review", "journalArticle")
	addItem("D1", "DELETED", "botany", "Ada Leaf", "Plant Review", "journalArticle")
	addItem("P1", "PENDING", "botany", "Ada Leaf", "Plant Review", "journalArticle")
	addItem("UA", "", "botany", "Ada Leaf", "Plant Review", "journalArticle")
	addItem("UB", "", "astronomy", "Bea Star", "Space Review", "book")
	addItem("NONE", "", "chemistry", "Cal Stone", "Chemistry Review", "journalArticle")
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatal(err)
	}
	collections := []json.RawMessage{
		json.RawMessage(`{"key":"COLA","data":{"key":"COLA","name":"Botany"}}`),
		json.RawMessage(`{"key":"COLB","data":{"key":"COLB","name":"Astronomy"}}`),
		json.RawMessage(`{"key":"COLC","data":{"key":"COLC","name":"Mixed"}}`),
		json.RawMessage(`{"key":"PENDING","data":{"key":"PENDING","name":"Pending deletion"}}`),
		json.RawMessage(`{"key":"DELETED","data":{"key":"DELETED","name":"Old Botany","deleted":true}}`),
	}
	if _, _, err := db.UpsertBatch("collections", collections); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(`INSERT INTO pending_writes (resource_type, id, changes, deleted) VALUES ('collections', 'PENDING', '[]', 1)`); err != nil {
		t.Fatal(err)
	}
}

func runUnfiledSuggestCommand(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()
	cmd := newItemsUnfiledCmd(&rootFlags{asJSON: true})
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs(args)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	return out.Bytes(), err
}

func TestItemsUnfiledSuggestRanksAndExplainsCollections(t *testing.T) {
	seedUnfiledSuggestions(t)
	first, err := runUnfiledSuggestCommand(t, "--suggest")
	if err != nil {
		t.Fatal(err)
	}
	second, err := runUnfiledSuggestCommand(t, "--suggest")
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("suggestions changed between runs: %v\n%s\n%s", err, first, second)
	}
	var envelope struct {
		Results []struct {
			Key         string              `json:"key"`
			Suggestions []unfiledSuggestion `json:"suggestions"`
			Action      string              `json:"recommended_action"`
		} `json:"results"`
	}
	if err := json.Unmarshal(first, &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Results) != 3 {
		t.Fatalf("results: %+v", envelope.Results)
	}
	want := map[string]string{"UA": "COLA", "UB": "COLB"}
	for _, row := range envelope.Results {
		if row.Key == "NONE" {
			if row.Suggestions == nil || len(row.Suggestions) != 0 || row.Action != "" {
				t.Fatalf("unmatched row: %+v", row)
			}
			continue
		}
		if len(row.Suggestions) == 0 || row.Suggestions[0].Collection != want[row.Key] ||
			row.Action != fmt.Sprintf("zotio items move %s --to %s", row.Key, want[row.Key]) {
			t.Fatalf("wrong suggestion for %s: %+v", row.Key, row)
		}
		if row.Suggestions[0].Score != 1 || row.Suggestions[0].Name == "" {
			t.Fatalf("score/name for %s: %+v", row.Key, row.Suggestions[0])
		}
		reasons := strings.Join(row.Suggestions[0].Reasons, " | ")
		for _, signal := range []string{"shared tag", "shared creator", "same venue", "matched item"} {
			if !strings.Contains(reasons, signal) {
				t.Errorf("%s reasons %q lack %q", row.Key, reasons, signal)
			}
		}
		if row.Key == "UA" && (len(row.Suggestions) != 2 ||
			row.Suggestions[1].Collection != "COLC" || row.Suggestions[1].Score != 0.33) {
			t.Fatalf("top-three voting did not discount the one-hit collection: %+v", row)
		}
		for _, suggestion := range row.Suggestions {
			if suggestion.Collection == "DELETED" || suggestion.Collection == "PENDING" {
				t.Fatalf("deleted collection suggested: %+v", row)
			}
		}
	}
	// jq '[.results[] | select(.suggestions[0].collection == "COLA")]'
	// emits a key-bearing array accepted by items move --keys-from -.
	var selected []any
	for _, row := range envelope.Results {
		if len(row.Suggestions) > 0 && row.Suggestions[0].Collection == "COLA" {
			selected = append(selected, row)
		}
	}
	pipe, err := json.Marshal(selected)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := resolveKeys(nil, "-", bytes.NewReader(pipe))
	if err != nil || !reflect.DeepEqual(keys, []string{"UA"}) {
		t.Fatalf("move --keys-from pipe: %v, %v", keys, err)
	}
}

func TestItemsUnfiledSuggestIsOptInAndRespectsFilters(t *testing.T) {
	seedUnfiledSuggestions(t)
	plain, err := runUnfiledSuggestCommand(t)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(plain, []byte(`"suggestions"`)) || bytes.Contains(plain, []byte(`"recommended_action"`)) {
		t.Fatalf("plain output gained suggestion fields: %s", plain)
	}
	filtered, err := runUnfiledSuggestCommand(t, "--suggest", "--type", "book", "--limit", "1", "--suggest-limit", "1")
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Results []struct {
			Key         string              `json:"key"`
			Suggestions []unfiledSuggestion `json:"suggestions"`
		} `json:"results"`
	}
	if err := json.Unmarshal(filtered, &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Results) != 1 || envelope.Results[0].Key != "UB" ||
		len(envelope.Results[0].Suggestions) != 1 || envelope.Results[0].Suggestions[0].Collection != "COLB" {
		t.Fatalf("filtered results: %+v", envelope.Results)
	}
	_, err = runUnfiledSuggestCommand(t, "--suggest-limit", "2")
	if err == nil || ExitCode(err) != 2 || !strings.Contains(err.Error(), "--suggest-limit requires --suggest") {
		t.Fatalf("missing usage exit 2: %v", err)
	}
}

func TestItemsUnfiledSuggestNeedsLocalStore(t *testing.T) {
	isolateItemsSimilarStore(t)
	_, err := runUnfiledSuggestCommand(t, "--suggest")
	if err == nil || ExitCode(err) != 9 || !strings.Contains(err.Error(), "run 'zotio sync' first to enable collection suggestions") {
		t.Fatalf("missing local store exit 9: %v", err)
	}
}
