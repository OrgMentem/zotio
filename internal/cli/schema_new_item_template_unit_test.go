// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// decodeOrderedSchemaValues and marshalLocalItemTemplate are reachable in CI
// only through the live-desktop command test, which skips without Zotero. A
// regression in order preservation, dedup, or the malformed-input arm would
// ship green and corrupt the template agents feed to items create. These
// fixture tests pin the helpers directly.
func TestDecodeOrderedSchemaValuesPreservesOrderAndDedups(t *testing.T) {
	data := json.RawMessage(`[{"field":"title"},{"field":"date"},{"field":"title"},{"field":"DOI"}]`)
	got, err := decodeOrderedSchemaValues(data, "/itemTypeFields", "field")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := []string{"title", "date", "DOI"}
	if len(got) != len(want) {
		t.Fatalf("values = %v, want %v (order matters)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("values = %v, want %v (order matters)", got, want)
		}
	}
}

func TestDecodeOrderedSchemaValuesSkipsUnusableRows(t *testing.T) {
	data := json.RawMessage(`[{"other":"x"},{"field":""},{"field":5},{"field":"date"}]`)
	got, err := decodeOrderedSchemaValues(data, "/creatorFields", "field")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0] != "date" {
		t.Fatalf("values = %v, want [date] (missing/empty/non-string rows skipped)", got)
	}
}

func TestDecodeOrderedSchemaValuesRejectsMalformedInput(t *testing.T) {
	if _, err := decodeOrderedSchemaValues(json.RawMessage(`[[`), "/itemTypeFields", "field"); err == nil {
		t.Fatal("malformed input decoded without an error")
	} else if !strings.Contains(err.Error(), "/itemTypeFields") {
		t.Fatalf("error = %q, want it to name the endpoint", err.Error())
	}
}

func TestMarshalLocalItemTemplateKeepsKeyOrder(t *testing.T) {
	raw, err := marshalLocalItemTemplate(
		"journalArticle",
		[]string{"title", "date", "tags", "collections", "relations", "creators", "title"},
		[]string{"author", "editor"},
		[]string{"firstName", "lastName"},
	)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	text := string(raw)
	// Zotero template order: identity, creators, fields, then the fixed tail.
	ordered := []string{`"itemType"`, `"creators"`, `"title"`, `"date"`, `"tags"`, `"collections"`, `"relations"`}
	last := -1
	for _, key := range ordered {
		idx := strings.Index(text, key)
		if idx < 0 {
			t.Fatalf("template %s misses key %s", text, key)
		}
		if idx < last {
			t.Fatalf("template %s has keys out of order at %s", text, key)
		}
		last = idx
	}
	if strings.Count(text, `"title"`) != 1 {
		t.Fatalf("template %s duplicates the title field", text)
	}

	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("template does not round-trip through encoding/json: %v", err)
	}
	creators, ok := decoded["creators"].([]any)
	if !ok || len(creators) != 1 {
		t.Fatalf("creators = %#v, want one blank creator", decoded["creators"])
	}
	creator, ok := creators[0].(map[string]any)
	if !ok {
		t.Fatalf("creator = %#v, want an object", creators[0])
	}
	if creator["creatorType"] != "author" {
		t.Errorf("creatorType = %#v, want the primary type author", creator["creatorType"])
	}
	for _, key := range []string{"firstName", "lastName"} {
		if _, ok := creator[key]; !ok {
			t.Errorf("creator misses %q: %#v", key, creator)
		}
	}
}

func TestCreatorTemplateFieldsBranches(t *testing.T) {
	if got := creatorTemplateFields([]string{"firstName", "lastName", "name"}); len(got) != 2 || got[0] != "firstName" || got[1] != "lastName" {
		t.Errorf("person fields = %v, want [firstName lastName]", got)
	}
	if got := creatorTemplateFields([]string{"name"}); len(got) != 1 || got[0] != "name" {
		t.Errorf("institution fields = %v, want [name]", got)
	}
	// A firstName without a lastName is not a person template: pass through.
	partial := []string{"firstName"}
	if got := creatorTemplateFields(partial); len(got) != 1 || got[0] != "firstName" {
		t.Errorf("partial fields = %v, want passthrough", got)
	}
}
