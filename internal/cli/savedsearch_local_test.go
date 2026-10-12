// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"zotio/internal/client"
	"zotio/internal/store"
)

// ssLocalNoon is local noon on a calendar day, in the RFC 3339 UTC form the
// API stores. Local noon keeps the day stable under DATE(x, 'localtime').
func ssLocalNoon(year int, month time.Month, day int) string {
	return time.Date(year, month, day, 12, 0, 0, 0, time.Local).UTC().Format(time.RFC3339)
}

func ssItem(key, itemType string, fields map[string]any) json.RawMessage {
	data := map[string]any{"key": key, "itemType": itemType, "dateAdded": ssLocalNoon(2022, time.June, 1)}
	for name, value := range fields {
		data[name] = value
	}
	raw, err := json.Marshal(map[string]any{"key": key, "version": 1, "data": data})
	if err != nil {
		panic(err)
	}
	return raw
}

func ssCreator(kind, first, last string) map[string]any {
	return map[string]any{"creatorType": kind, "firstName": first, "lastName": last}
}

func ssTags(names ...string) []map[string]any {
	tags := make([]map[string]any, len(names))
	for i, name := range names {
		tags[i] = map[string]any{"tag": name, "type": 0}
	}
	return tags
}

// seedSavedSearchStore mirrors a small library. Expected sets in the tests
// below are derived by hand from Zotero's search.js semantics:
//
//	A1 article  "Séance on Deep Learning"  2020-03-15  Jane Smith (author), Bob Lee (editor)
//	            tags ai, To Read; collection C1; Nature; DOI; added 2021-05-10
//	A2 book     "Gardening Basics"         "March 2019" Acme Press (single field)
//	            tag garden; collection C2; Penguin; added 2019-03-01
//	A3 article  "Learning Theory"          2021  Jane Smith (author); tag ai
//	A4 paper    "Neural Nets"              2018-07; proceedingsTitle ICML
//	A5 article  "Subtopic"                 collection C1a (child of C1)
//	B1 attachment "Full Text PDF", child of A1, tag child-only
//	N1 note, child of A3
//	T1 (trash)  "Deep trash"; TC1 attachment under T1 "Deep trash child"
func seedSavedSearchStore(t *testing.T, searches map[string]string) *store.Store {
	t.Helper()
	db, err := store.OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "zotio.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	items := []json.RawMessage{
		ssItem("A1", "journalArticle", map[string]any{
			"title": "Séance on Deep Learning", "date": "2020-03-15", "publicationTitle": "Nature", "DOI": "10.1/abc",
			"creators":    []map[string]any{ssCreator("author", "Jane", "Smith"), ssCreator("editor", "Bob", "Lee")},
			"tags":        ssTags("ai", "To Read"),
			"collections": []string{"C1"},
			"dateAdded":   ssLocalNoon(2021, time.May, 10),
		}),
		ssItem("A2", "book", map[string]any{
			"title": "Gardening Basics", "date": "March 2019", "publisher": "Penguin",
			"creators":    []map[string]any{{"creatorType": "author", "name": "Acme Press"}},
			"tags":        ssTags("garden"),
			"collections": []string{"C2"},
			"dateAdded":   ssLocalNoon(2019, time.March, 1),
		}),
		ssItem("A3", "journalArticle", map[string]any{
			"title": "Learning Theory", "date": "2021",
			"creators": []map[string]any{ssCreator("author", "Jane", "Smith")},
			"tags":     ssTags("ai"),
		}),
		ssItem("A4", "conferencePaper", map[string]any{"title": "Neural Nets", "date": "2018-07", "proceedingsTitle": "ICML"}),
		ssItem("A5", "journalArticle", map[string]any{"title": "Subtopic", "collections": []string{"C1a"}}),
		ssItem("B1", "attachment", map[string]any{"title": "Full Text PDF", "parentItem": "A1", "tags": ssTags("child-only")}),
		ssItem("N1", "note", map[string]any{"parentItem": "A3"}),
		ssItem("TC1", "attachment", map[string]any{"title": "Deep trash child", "parentItem": "T1"}),
		ssItem("LIVE1", "journalArticle", map[string]any{"title": "Only reachable live"}),
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed items: %v", err)
	}
	if _, _, err := db.UpsertBatch("items-trash", []json.RawMessage{
		ssItem("T1", "journalArticle", map[string]any{"title": "Deep trash"}),
	}); err != nil {
		t.Fatalf("seed trash: %v", err)
	}
	collections := []json.RawMessage{
		json.RawMessage(`{"key":"C1","version":1,"data":{"key":"C1","name":"One","parentCollection":false}}`),
		json.RawMessage(`{"key":"C1a","version":1,"data":{"key":"C1a","name":"One A","parentCollection":"C1"}}`),
		json.RawMessage(`{"key":"C2","version":1,"data":{"key":"C2","name":"Two","parentCollection":false}}`),
	}
	if _, _, err := db.UpsertBatch("collections", collections); err != nil {
		t.Fatalf("seed collections: %v", err)
	}
	for key, conditions := range searches {
		def := `{"key":"` + key + `","version":3,"data":{"key":"` + key + `","name":"search ` + key + `","conditions":` + conditions + `}}`
		if _, _, err := db.UpsertBatch("searches", []json.RawMessage{json.RawMessage(def)}); err != nil {
			t.Fatalf("seed search %s: %v", key, err)
		}
	}
	if err := db.SaveSyncState("items", "", len(items)); err != nil {
		t.Fatalf("seed items sync state: %v", err)
	}
	return db
}

func ssConds(conds ...[3]string) string {
	out := make([]map[string]string, len(conds))
	for i, c := range conds {
		out[i] = map[string]string{"condition": c[0], "operator": c[1], "value": c[2]}
	}
	raw, err := json.Marshal(out)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func ssKeys(keys ...string) []string {
	sort.Strings(keys)
	return keys
}

// Each case is one saved search whose expected key set is written from
// Zotero's semantics, not from the evaluator's output.
func TestSavedSearchLocalEvaluatesSupportedOperators(t *testing.T) {
	cases := []struct {
		name       string
		conditions string
		want       []string
	}{
		// Text fields: case/accent-insensitive, base-field mapping.
		{"title contains folds accents", ssConds([3]string{"title", "contains", "seance"}), ssKeys("A1")},
		{"title contains is case-insensitive", ssConds([3]string{"title", "contains", "LEARNING"}), ssKeys("A1", "A3")},
		{"title is", ssConds([3]string{"title", "is", "gardening basics"}), ssKeys("A2")},
		{"title beginsWith", ssConds([3]string{"title", "beginsWith", "neural"}), ssKeys("A4")},
		{"title doesNotContain matches items without a title", ssConds([3]string{"title", "doesNotContain", "learning"}), ssKeys("A2", "A4", "A5", "B1", "LIVE1", "N1")},
		{"title isNot", ssConds([3]string{"title", "isNot", "Subtopic"}), ssKeys("A1", "A2", "A3", "A4", "B1", "LIVE1", "N1")},
		{"publicationTitle covers type-specific base fields", ssConds([3]string{"publicationTitle", "is", "icml"}), ssKeys("A4")},
		{"publisher contains", ssConds([3]string{"publisher", "contains", "pengu"}), ssKeys("A2")},
		{"field isNotEmpty", ssConds([3]string{"DOI", "isNotEmpty", ""}), ssKeys("A1")},
		{"field isEmpty", ssConds([3]string{"DOI", "isEmpty", ""}), ssKeys("A2", "A3", "A4", "A5", "B1", "LIVE1", "N1")},
		// Creators.
		{"creator contains full name", ssConds([3]string{"creator", "contains", "jane smith"}), ssKeys("A1", "A3")},
		{"creator is single-field name", ssConds([3]string{"creator", "is", "acme press"}), ssKeys("A2")},
		{"creator isNot", ssConds([3]string{"creator", "isNot", "jane smith"}), ssKeys("A2", "A4", "A5", "B1", "LIVE1", "N1")},
		{"author ignores editors", ssConds([3]string{"author", "is", "bob lee"}), ssKeys()},
		{"editor matches editors", ssConds([3]string{"editor", "is", "bob lee"}), ssKeys("A1")},
		// Tags, including child items, which carry their own tags.
		{"tag is is case-insensitive", ssConds([3]string{"tag", "is", "AI"}), ssKeys("A1", "A3")},
		{"tag contains", ssConds([3]string{"tag", "contains", "read"}), ssKeys("A1")},
		{"tag matches a child item", ssConds([3]string{"tag", "is", "child-only"}), ssKeys("B1")},
		{"tag isNot", ssConds([3]string{"tag", "isNot", "ai"}), ssKeys("A2", "A4", "A5", "B1", "LIVE1", "N1")},
		// Collections.
		{"collection is", ssConds([3]string{"collection", "is", "C1"}), ssKeys("A1")},
		{"collection is recursive", ssConds([3]string{"collection", "is", "C1"}, [3]string{"recursive", "true", ""}), ssKeys("A1", "A5")},
		{"collection isNot", ssConds([3]string{"collection", "isNot", "C2"}), ssKeys("A1", "A3", "A4", "A5", "B1", "LIVE1", "N1")},
		// Item type.
		{"itemType is", ssConds([3]string{"itemType", "is", "book"}), ssKeys("A2")},
		{"itemType isNot", ssConds([3]string{"itemType", "isNot", "journalArticle"}), ssKeys("A2", "A4", "B1", "N1")},
		// Dates: a YYYY value spans the year, isBefore/isAfter are strict.
		{"date is a year", ssConds([3]string{"date", "is", "2020"}), ssKeys("A1")},
		{"date is a month", ssConds([3]string{"date", "is", "2020-03"}), ssKeys("A1")},
		{"date is a wrong month", ssConds([3]string{"date", "is", "2020-04"}), ssKeys()},
		{"date is a day", ssConds([3]string{"date", "is", "2020-03-15"}), ssKeys("A1")},
		{"date is parses freeform month year", ssConds([3]string{"date", "is", "2019-03"}), ssKeys("A2")},
		{"date isBefore a year", ssConds([3]string{"date", "isBefore", "2020"}), ssKeys("A2", "A4")},
		{"date isAfter a year", ssConds([3]string{"date", "isAfter", "2020"}), ssKeys("A3")},
		{"date isAfter a month is after the month", ssConds([3]string{"date", "isAfter", "2019-03"}), ssKeys("A1", "A3")},
		{"date isNot", ssConds([3]string{"date", "isNot", "2020"}), ssKeys("A2", "A3", "A4", "A5", "B1", "LIVE1", "N1")},
		{"dateAdded is a day", ssConds([3]string{"dateAdded", "is", "2021-05-10"}), ssKeys("A1")},
		{"dateAdded isBefore a year", ssConds([3]string{"dateAdded", "isBefore", "2020"}), ssKeys("A2")},
		{"dateAdded isAfter a day", ssConds([3]string{"dateAdded", "isAfter", "2021-05-10"}), ssKeys("A3", "A4", "A5", "B1", "LIVE1", "N1")},
		{"dateAdded isBefore a date-time", ssConds([3]string{"dateAdded", "isBefore", "2020-01-01 00:00:00"}), ssKeys("A2")},
		// joinMode.
		{"default join is all", ssConds([3]string{"title", "contains", "learning"}, [3]string{"tag", "is", "to read"}), ssKeys("A1")},
		{"joinMode all", ssConds([3]string{"joinMode", "all", ""}, [3]string{"title", "contains", "learning"}, [3]string{"tag", "is", "garden"}), ssKeys()},
		{"joinMode any", ssConds([3]string{"joinMode", "any", ""}, [3]string{"title", "contains", "learning"}, [3]string{"tag", "is", "garden"}), ssKeys("A1", "A2", "A3")},
		// Child-item flags.
		{"noChildren drops child matches", ssConds([3]string{"tag", "is", "child-only"}, [3]string{"noChildren", "true", ""}), ssKeys()},
		{"noChildren false keeps them", ssConds([3]string{"tag", "is", "child-only"}, [3]string{"noChildren", "false", ""}), ssKeys("B1")},
		{"includeParents adds the parent", ssConds([3]string{"tag", "is", "child-only"}, [3]string{"includeParents", "true", ""}), ssKeys("A1", "B1")},
		{"includeChildren adds child notes and attachments", ssConds([3]string{"title", "is", "learning theory"}, [3]string{"includeChildren", "true", ""}), ssKeys("A3", "N1")},
		{"includeParentsAndChildren", ssConds([3]string{"title", "contains", "learning"}, [3]string{"includeParentsAndChildren", "true", ""}), ssKeys("A1", "A3", "B1", "N1")},
		{"include flags apply whatever their position", ssConds([3]string{"includeParents", "true", ""}, [3]string{"tag", "is", "child-only"}), ssKeys("A1", "B1")},
		// Trash: neither a trashed item nor its children match, even a negation.
		{"trashed items and their children never match", ssConds([3]string{"title", "contains", "trash"}), ssKeys()},
		{"trashed items are absent from a negation too", ssConds([3]string{"title", "doesNotContain", "zzz"}), ssKeys("A1", "A2", "A3", "A4", "A5", "B1", "LIVE1", "N1")},
	}

	searches := make(map[string]string, len(cases))
	for i, tc := range cases {
		searches["S"+string(rune('a'+i/26))+string(rune('a'+i%26))] = tc.conditions
	}
	db := seedSavedSearchStore(t, searches)

	for i, tc := range cases {
		key := "S" + string(rune('a'+i/26)) + string(rune('a'+i%26))
		t.Run(tc.name, func(t *testing.T) {
			got, err := evaluateSavedSearchLocal(context.Background(), db, key)
			if err != nil {
				t.Fatalf("evaluate: %v", err)
			}
			want := tc.want
			if want == nil {
				want = []string{}
			}
			gotKeys := got.Keys
			if gotKeys == nil {
				gotKeys = []string{}
			}
			if !reflect.DeepEqual(gotKeys, want) {
				t.Fatalf("keys = %v, want %v (conditions %s)", gotKeys, want, tc.conditions)
			}
		})
	}
}

func TestSavedSearchLocalTopLevelKeysDropChildren(t *testing.T) {
	db := seedSavedSearchStore(t, map[string]string{
		"SS1": ssConds([3]string{"title", "contains", "learning"}, [3]string{"includeParentsAndChildren", "true", ""}),
	})
	got, err := evaluateSavedSearchLocal(context.Background(), db, "SS1")
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if want := ssKeys("A1", "A3", "B1", "N1"); !reflect.DeepEqual(got.Keys, want) {
		t.Fatalf("keys = %v, want %v", got.Keys, want)
	}
	if want := ssKeys("A1", "A3"); !reflect.DeepEqual(got.TopLevelKeys, want) {
		t.Fatalf("top-level keys = %v, want %v", got.TopLevelKeys, want)
	}
}

// A definition with ANY unsupported part is refused whole, with every
// offending part named; the supported conditions next to it never produce a
// partial cohort.
func TestSavedSearchLocalRefusesUnsupportedDefinitions(t *testing.T) {
	cases := []struct {
		name       string
		conditions string
		want       []string
	}{
		{"fulltextContent", ssConds([3]string{"title", "contains", "learning"}, [3]string{"fulltextContent", "contains", "kernel"}), []string{"fulltextContent"}},
		{"note", ssConds([3]string{"note", "contains", "x"}), []string{"note: condition"}},
		{"anyField", ssConds([3]string{"anyField", "contains", "x"}), []string{"anyField"}},
		{"year", ssConds([3]string{"year", "is", "2020"}), []string{"year"}},
		{"nested group", ssConds([3]string{"groupStart", "true", ""}, [3]string{"tag", "is", "ai"}, [3]string{"groupEnd", "true", ""}), []string{"groupStart", "groupEnd"}},
		{"nested saved search", ssConds([3]string{"savedSearch", "is", "OTHER"}), []string{"savedSearch"}},
		{"unfiled flag", ssConds([3]string{"tag", "is", "ai"}, [3]string{"unfiled", "true", ""}), []string{"unfiled"}},
		{"unknown condition", ssConds([3]string{"flibbertigibbet", "is", "x"}), []string{"flibbertigibbet"}},
		{"unsupported operator", ssConds([3]string{"title", "isBefore", "x"}), []string{`title: operator "isBefore"`}},
		{"beginsWith on a number field", ssConds([3]string{"pages", "beginsWith", "1"}), []string{`pages: operator "beginsWith"`}},
		{"isInTheLast", ssConds([3]string{"dateAdded", "isInTheLast", "3 days"}), []string{`dateAdded: operator "isInTheLast"`}},
		{"LIKE wildcard", ssConds([3]string{"title", "contains", "100%"}), []string{"LIKE wildcard"}},
		{"relative date value", ssConds([3]string{"dateAdded", "isBefore", "yesterday"}), []string{`dateAdded: value "yesterday"`}},
		{"bad join mode", ssConds([3]string{"joinMode", "some", ""}, [3]string{"tag", "is", "ai"}), []string{"joinMode"}},
		{"bad flag operator", ssConds([3]string{"noChildren", "maybe", ""}, [3]string{"tag", "is", "ai"}), []string{"noChildren"}},
		{"no match conditions", ssConds([3]string{"noChildren", "true", ""}), []string{"no match conditions"}},
		{"empty conditions", `[]`, []string{"no match conditions"}},
		// Mirrored data the evaluator cannot be sure about.
		{"collection missing from the mirror", ssConds([3]string{"collection", "is", "GONE"}), []string{`collection: key "GONE"`}},
	}
	searches := make(map[string]string, len(cases))
	for i, tc := range cases {
		searches["U"+string(rune('a'+i))] = tc.conditions
	}
	db := seedSavedSearchStore(t, searches)

	for i, tc := range cases {
		key := "U" + string(rune('a'+i))
		t.Run(tc.name, func(t *testing.T) {
			got, err := evaluateSavedSearchLocal(context.Background(), db, key)
			var unsupported *savedSearchUnsupportedError
			if !errors.As(err, &unsupported) {
				t.Fatalf("err = %v (keys %v), want *savedSearchUnsupportedError", err, got.Keys)
			}
			if len(got.Keys) != 0 {
				t.Fatalf("an unsupported definition returned a partial cohort: %v", got.Keys)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not name %q", err.Error(), want)
				}
			}
		})
	}
}

func TestSavedSearchLocalRefusesUnparseableMirroredItemDate(t *testing.T) {
	db := seedSavedSearchStore(t, map[string]string{
		"SD": ssConds([3]string{"date", "isAfter", "2000"}),
	})
	if _, _, err := db.UpsertBatch("items", []json.RawMessage{
		ssItem("ODD", "journalArticle", map[string]any{"title": "Odd", "date": "sometime in spring"}),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := evaluateSavedSearchLocal(context.Background(), db, "SD")
	var unsupported *savedSearchUnsupportedError
	if !errors.As(err, &unsupported) || !strings.Contains(err.Error(), "ODD") || !strings.Contains(err.Error(), "sometime in spring") {
		t.Fatalf("err = %v, want an unsupported error naming ODD and its date", err)
	}
}

func TestSavedSearchLocalAbsentDefinitionIsNotMirrored(t *testing.T) {
	db := seedSavedSearchStore(t, nil)
	_, err := evaluateSavedSearchLocal(context.Background(), db, "NOPE")
	if !errors.Is(err, errSavedSearchNotMirrored) {
		t.Fatalf("err = %v, want errSavedSearchNotMirrored", err)
	}
}

// A mirror holding only the definition (a write-through cache) never ran an
// items sync, so evaluation must refuse rather than report an empty cohort.
func TestSavedSearchLocalRefusesWhenItemsNeverSynced(t *testing.T) {
	db, err := store.OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "zotio.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	def := `{"key":"SS1","version":1,"data":{"key":"SS1","name":"d","conditions":` + ssConds([3]string{"title", "contains", "x"}) + `}}`
	if _, _, err := db.UpsertBatch("searches", []json.RawMessage{json.RawMessage(def)}); err != nil {
		t.Fatalf("seed search: %v", err)
	}
	got, err := evaluateSavedSearchLocal(context.Background(), db, "SS1")
	if !errors.Is(err, errSavedSearchItemsNotSynced) {
		t.Fatalf("err = %v (keys %v), want errSavedSearchItemsNotSynced", err, got.Keys)
	}

	t.Setenv("HOME", t.TempDir())
	stubSavedSearchLiveUnavailable(t)
	r, err := resolveScopeLive(context.Background(), &rootFlags{quiet: true, timeout: time.Second}, localQueryStore{Store: db}, scopeSpec{Type: "saved-search", Value: "SS1"})
	if err != nil {
		t.Fatalf("resolveScopeLive: %v", err)
	}
	if r.Precondition != preconditionLiveLocalAPI || len(r.Keys) != 0 || !strings.Contains(r.PreconditionDetail, "items sync") {
		t.Fatalf("result = %+v, want the unmet precondition naming the missing items sync", r)
	}
}

// includeParentsAndChildren is independent of includeParents: a later
// includeParents=false must not cancel the combined flag (Zotero search.js
// ORs them).
func TestSavedSearchLocalCombinedFlagSurvivesLaterIncludeParentsFalse(t *testing.T) {
	db := seedSavedSearchStore(t, map[string]string{
		"SS1": ssConds([3]string{"title", "contains", "full text"}, [3]string{"includeParentsAndChildren", "true", ""}, [3]string{"includeParents", "false", ""}),
		"SS2": ssConds([3]string{"title", "contains", "séance"}, [3]string{"includeParentsAndChildren", "true", ""}, [3]string{"includeChildren", "false", ""}),
	})
	got, err := evaluateSavedSearchLocal(context.Background(), db, "SS1")
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if want := ssKeys("A1", "B1"); !reflect.DeepEqual(got.Keys, want) {
		t.Fatalf("parent expansion keys = %v, want %v", got.Keys, want)
	}
	got, err = evaluateSavedSearchLocal(context.Background(), db, "SS2")
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if want := ssKeys("A1", "B1"); !reflect.DeepEqual(got.Keys, want) {
		t.Fatalf("child expansion keys = %v, want %v", got.Keys, want)
	}
}

// stubSavedSearchLiveUnavailable makes the live path report the desktop as
// unreachable, the state the local evaluator exists for.
func stubSavedSearchLiveUnavailable(t *testing.T) {
	t.Helper()
	old := savedSearchLiveClient
	savedSearchLiveClient = func(context.Context, *rootFlags) (*client.Client, string, error) {
		return nil, "Zotero desktop local API is not reachable", nil
	}
	t.Cleanup(func() { savedSearchLiveClient = old })
}

func TestResolveScopeLiveFallsBackToLocalEvaluationWhenDesktopUnavailable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubSavedSearchLiveUnavailable(t)
	db := seedSavedSearchStore(t, map[string]string{
		"SS1": ssConds([3]string{"title", "contains", "learning"}, [3]string{"includeChildren", "true", ""}),
		"SSX": ssConds([3]string{"title", "contains", "learning"}, [3]string{"fulltextContent", "contains", "kernel"}),
	})
	flags := &rootFlags{quiet: true, timeout: time.Second}

	t.Run("supported search resolves top-level keys with provenance", func(t *testing.T) {
		r, err := resolveScopeLive(context.Background(), flags, localQueryStore{Store: db}, scopeSpec{Type: "saved-search", Value: "SS1"})
		if err != nil {
			t.Fatalf("resolveScopeLive: %v", err)
		}
		if r.Precondition != "" || r.PreconditionDetail != "" {
			t.Fatalf("precondition = %q (%s), want cleared by local evaluation", r.Precondition, r.PreconditionDetail)
		}
		if want := []string{"A1", "A3"}; !reflect.DeepEqual(r.Keys, want) {
			t.Fatalf("keys = %v, want %v (children dropped like the live arm)", r.Keys, want)
		}
		for _, want := range []string{"SS1", "local mirror", "not reachable"} {
			if !strings.Contains(r.LocalEvaluation, want) {
				t.Fatalf("LocalEvaluation = %q, want it to mention %q", r.LocalEvaluation, want)
			}
		}
	})

	t.Run("unsupported condition keeps the precondition and names it", func(t *testing.T) {
		r, err := resolveScopeLive(context.Background(), flags, localQueryStore{Store: db}, scopeSpec{Type: "saved-search", Value: "SSX"})
		if err != nil {
			t.Fatalf("resolveScopeLive: %v", err)
		}
		if r.Precondition != preconditionLiveLocalAPI || len(r.Keys) != 0 || r.LocalEvaluation != "" {
			t.Fatalf("result = %+v, want the unmet precondition and no partial cohort", r)
		}
		for _, want := range []string{"fulltextContent", "not reachable"} {
			if !strings.Contains(r.PreconditionDetail, want) {
				t.Fatalf("detail = %q, want it to mention %q", r.PreconditionDetail, want)
			}
		}
		if code := ExitCode(scopePreconditionErr(context.Background(), nil, flags, "library health", r)); code != 9 {
			t.Fatalf("refusal exit = %d, want 9", code)
		}
	})

	t.Run("absent from the mirror keeps the precondition", func(t *testing.T) {
		r, err := resolveScopeLive(context.Background(), flags, localQueryStore{Store: db}, scopeSpec{Type: "saved-search", Value: "NOPE"})
		if err != nil {
			t.Fatalf("resolveScopeLive: %v", err)
		}
		if r.Precondition != preconditionLiveLocalAPI || len(r.Keys) != 0 || !strings.Contains(r.PreconditionDetail, "not in the mirror") {
			t.Fatalf("result = %+v, want the unmet precondition naming the missing mirror row", r)
		}
	})

	t.Run("data-source live never falls back", func(t *testing.T) {
		liveOnly := &rootFlags{quiet: true, timeout: time.Second, dataSource: "live"}
		r, err := resolveScopeLive(context.Background(), liveOnly, localQueryStore{Store: db}, scopeSpec{Type: "saved-search", Value: "SS1"})
		if err != nil {
			t.Fatalf("resolveScopeLive: %v", err)
		}
		if r.Precondition != preconditionLiveLocalAPI || len(r.Keys) != 0 || r.LocalEvaluation != "" {
			t.Fatalf("result = %+v, want the plain live precondition", r)
		}
	})
}

// Live stays authoritative: with the desktop answering, the mirror is never
// consulted, even when it holds a different answer.
func TestResolveScopeLivePrefersLiveOverLocalEvaluation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	db := seedSavedSearchStore(t, map[string]string{
		"SS1": ssConds([3]string{"title", "contains", "learning"}),
	})
	stubSavedSearchLocalAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/users/0/searches/SS1/items" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`[{"key":"LIVE1","data":{}}]`))
	})
	r, err := resolveScopeLive(context.Background(), &rootFlags{quiet: true, timeout: 5 * time.Second}, localQueryStore{Store: db}, scopeSpec{Type: "saved-search", Value: "SS1"})
	if err != nil {
		t.Fatalf("resolveScopeLive: %v", err)
	}
	if want := []string{"LIVE1"}; !reflect.DeepEqual(r.Keys, want) || r.LocalEvaluation != "" {
		t.Fatalf("result = %+v, want the live cohort %v without local provenance", r, want)
	}
}

// A desktop that answers 403 (local API access off) cannot execute the
// search, so the mirror takes over.
func TestResolveScopeLiveFallsBackToLocalEvaluationOnForbidden(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	db := seedSavedSearchStore(t, map[string]string{
		"SS1": ssConds([3]string{"tag", "is", "garden"}),
	})
	stubSavedSearchLocalAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/searches/SS1/items") {
			http.Error(w, "Local API is not enabled", http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	})
	r, err := resolveScopeLive(context.Background(), &rootFlags{quiet: true, timeout: 5 * time.Second}, localQueryStore{Store: db}, scopeSpec{Type: "saved-search", Value: "SS1"})
	if err != nil {
		t.Fatalf("resolveScopeLive: %v", err)
	}
	if want := []string{"A2"}; r.Precondition != "" || !reflect.DeepEqual(r.Keys, want) || !strings.Contains(r.LocalEvaluation, "HTTP 403") {
		t.Fatalf("result = %+v, want local cohort %v naming the 403", r, want)
	}
}

// The real definition reaches the fallback without a caller-supplied store.
func TestResolveScopeLiveLocalFallbackOpensTheDefaultMirror(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("ZOTIO_DEMO", "0")
	stubSavedSearchLiveUnavailable(t)

	dbPath, err := defaultDBPath("zotio")
	if err != nil {
		t.Fatalf("default db path: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("mkdir store dir: %v", err)
	}
	seeded, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open default store: %v", err)
	}
	if _, _, err := seeded.UpsertBatch("items", []json.RawMessage{
		ssItem("ONLY1", "book", map[string]any{"title": "Offline"}),
	}); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	def := `{"key":"SD","version":1,"data":{"key":"SD","name":"d","conditions":` + ssConds([3]string{"title", "is", "offline"}) + `}}`
	if _, _, err := seeded.UpsertBatch("searches", []json.RawMessage{json.RawMessage(def)}); err != nil {
		t.Fatalf("seed search: %v", err)
	}
	if err := seeded.SaveSyncState("items", "", 1); err != nil {
		t.Fatalf("seed sync state: %v", err)
	}
	if err := seeded.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	r, err := resolveScopeLive(context.Background(), &rootFlags{quiet: true, timeout: time.Second}, localQueryStore{}, scopeSpec{Type: "saved-search", Value: "SD"})
	if err != nil {
		t.Fatalf("resolveScopeLive: %v", err)
	}
	if want := []string{"ONLY1"}; r.Precondition != "" || !reflect.DeepEqual(r.Keys, want) {
		t.Fatalf("result = %+v, want %v from the default mirror", r, want)
	}
}

// An intersection keeps the provenance of every saved-search arm that was
// evaluated locally, joined in operand order; a plain arm adds none. When any
// arm is refused, the combined result carries the precondition and no
// provenance for a cohort that never formed.
func TestResolveScopeIntersectionCarriesLocalEvaluationFromEachSavedSearchArm(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubSavedSearchLiveUnavailable(t)
	db := seedSavedSearchStore(t, map[string]string{
		"SS1": ssConds([3]string{"title", "contains", "learning"}),
		"SS2": ssConds([3]string{"tag", "is", "to read"}),
		"SSX": ssConds([3]string{"fulltextContent", "contains", "kernel"}),
	})
	flags := &rootFlags{quiet: true, timeout: time.Second}
	resolve := func(expr string) scopeResult {
		t.Helper()
		spec, err := parseScopeSpec(expr)
		if err != nil {
			t.Fatalf("parseScopeSpec(%q): %v", expr, err)
		}
		r, err := resolveScopeLive(context.Background(), flags, localQueryStore{Store: db}, spec)
		if err != nil {
			t.Fatalf("resolveScopeLive(%q): %v", expr, err)
		}
		return r
	}

	both := resolve("saved-search:SS1 AND saved-search:SS2")
	if both.Precondition != "" || !reflect.DeepEqual(both.Keys, []string{"A1"}) {
		t.Fatalf("result = %+v, want keys [A1] with the precondition cleared", both)
	}
	first, second := strings.Index(both.LocalEvaluation, "saved search SS1"), strings.Index(both.LocalEvaluation, "saved search SS2")
	if first < 0 || second < first || !strings.Contains(both.LocalEvaluation, "; ") {
		t.Fatalf("LocalEvaluation = %q, want SS1's then SS2's provenance joined with %q", both.LocalEvaluation, "; ")
	}

	mixed := resolve("saved-search:SS2 AND tag:ai")
	if !reflect.DeepEqual(mixed.Keys, []string{"A1"}) || strings.Count(mixed.LocalEvaluation, "saved search") != 1 {
		t.Fatalf("result = %+v, want keys [A1] and exactly one saved-search provenance", mixed)
	}

	refused := resolve("saved-search:SS1 AND saved-search:SSX")
	if refused.Precondition != preconditionLiveLocalAPI || len(refused.Keys) != 0 || refused.LocalEvaluation != "" {
		t.Fatalf("result = %+v, want the unmet precondition with no keys and no provenance", refused)
	}
	if !strings.Contains(refused.PreconditionDetail, "fulltextContent") {
		t.Fatalf("detail = %q, want it to name fulltextContent", refused.PreconditionDetail)
	}
}
