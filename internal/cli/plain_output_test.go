// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// --plain must emit tab-separated text, never silently fall back to JSON.

package cli

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func plainOutput(t *testing.T, payload string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := printOutputWithFlags(&buf, json.RawMessage(payload), &rootFlags{plain: true}); err != nil {
		t.Fatalf("printOutputWithFlags(--plain): %v", err)
	}
	if strings.HasPrefix(strings.TrimSpace(buf.String()), "{") {
		t.Fatalf("--plain emitted JSON:\n%s", buf.String())
	}
	return buf.String()
}

// The read commands wrap results in a provenance envelope. Rendering the wrapper
// instead of the records is what made --plain return JSON.
func TestPlainOutputRendersProvenanceEnvelope(t *testing.T) {
	out := plainOutput(t, `{
		"meta": {"source": "live"},
		"results": [
			{"key":"AAA","data":{"itemType":"journalArticle","title":"First Paper"}},
			{"key":"BBB","data":{"itemType":"book","title":"Second Work"}}
		]
	}`)

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("want a header plus 2 records, got %d lines:\n%s", len(lines), out)
	}
	header := strings.Split(lines[0], "\t")
	if len(header) < 3 {
		t.Fatalf("header = %q, want key/itemType/title columns", lines[0])
	}
	for _, want := range []string{"key", "itemType", "title"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("header %q missing %q", lines[0], want)
		}
	}
	for _, want := range []string{"AAA", "First Paper"} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("first record %q missing %q", lines[1], want)
		}
	}
	if got := len(strings.Split(lines[1], "\t")); got != len(header) {
		t.Errorf("record has %d cells, header has %d: rows must stay rectangular", got, len(header))
	}
	if strings.Contains(out, "source") {
		t.Errorf("envelope metadata leaked into the records:\n%s", out)
	}
}

// Table cells truncate at 60 characters for display. --plain feeds cut/awk, so
// truncating would hand the caller a quietly wrong value.
func TestPlainOutputDoesNotTruncateValues(t *testing.T) {
	long := strings.Repeat("z", 200)
	out := plainOutput(t, `[{"title":"`+long+`"}]`)
	if !strings.Contains(out, long) {
		t.Fatalf("--plain truncated a %d-character value:\n%s", len(long), out)
	}
}

// A tab or newline inside a value would shift columns or split the record.
func TestPlainOutputNeutralizesSeparatorsInValues(t *testing.T) {
	out := plainOutput(t, `[{"a":"one\ttwo","b":"three\nfour"}]`)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("value newline split the record into %d lines:\n%s", len(lines), out)
	}
	header := strings.Split(lines[0], "\t")
	if got := len(strings.Split(lines[1], "\t")); got != len(header) {
		t.Fatalf("value tab added columns: %d cells for %d headers\n%s", got, len(header), out)
	}
	if !strings.Contains(lines[1], "one two") || !strings.Contains(lines[1], "three four") {
		t.Fatalf("separators were not collapsed to spaces:\n%s", out)
	}
}

// items get returns a single object; --plain must still render it as text.
func TestPlainOutputRendersSingleObject(t *testing.T) {
	out := plainOutput(t, `{"key":"AAA","data":{"itemType":"book","title":"Solo"}}`)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want header plus one record, got:\n%s", out)
	}
	if !strings.Contains(lines[1], "AAA") || !strings.Contains(lines[1], "Solo") {
		t.Fatalf("single-object record = %q", lines[1])
	}
}

// A bare array of scalars has no columns; one value per line is the useful shape.
func TestPlainOutputRendersScalarArray(t *testing.T) {
	out := plainOutput(t, `["AAA","BBB","CCC"]`)
	if got := strings.TrimRight(out, "\n"); got != "AAA\nBBB\nCCC" {
		t.Fatalf("scalar array = %q, want one value per line", got)
	}
}

// --csv stays more specific than --plain when both are set.
func TestCSVWinsOverPlain(t *testing.T) {
	var buf bytes.Buffer
	if err := printOutputWithFlags(&buf, json.RawMessage(`[{"a":"1","b":"2"}]`), &rootFlags{plain: true, csv: true}); err != nil {
		t.Fatalf("printOutputWithFlags: %v", err)
	}
	if !strings.Contains(buf.String(), "a,b") {
		t.Fatalf("want CSV header, got:\n%s", buf.String())
	}
}

// An object with several sibling arrays has no single record list. Rendering it
// as one row put an entire JSON array in each cell — worse than the format being
// unavailable, so it must fall back to JSON.
func TestPlainOutputFallsBackToJSONForMultipleArrays(t *testing.T) {
	var buf bytes.Buffer
	payload := `{"missing-pdf":[{"key":"A"}],"missing-abstract":[{"key":"B"}]}`
	if err := printOutputWithFlags(&buf, json.RawMessage(payload), &rootFlags{plain: true}); err != nil {
		t.Fatalf("printOutputWithFlags(--plain): %v", err)
	}
	var decoded map[string][]map[string]string
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("multi-array --plain output is neither records nor valid JSON:\n%s", buf.String())
	}
	if len(decoded["missing-pdf"]) != 1 || len(decoded["missing-abstract"]) != 1 {
		t.Fatalf("fallback JSON lost data: %#v", decoded)
	}
}

// A bare JSON string must not print its own quotes.
func TestPlainOutputUnquotesScalarPayload(t *testing.T) {
	var buf bytes.Buffer
	if err := printOutputWithFlags(&buf, json.RawMessage(`"file:///tmp/a.pdf"`), &rootFlags{plain: true}); err != nil {
		t.Fatalf("printOutputWithFlags(--plain): %v", err)
	}
	if got := strings.TrimRight(buf.String(), "\n"); got != "file:///tmp/a.pdf" {
		t.Fatalf("scalar payload = %q, want the unquoted value", got)
	}
}

// A mixed array must not silently drop its scalar entries.
func TestPlainOutputKeepsScalarsInMixedArray(t *testing.T) {
	var buf bytes.Buffer
	if err := printOutputWithFlags(&buf, json.RawMessage(`[{"key":"A"},"BARE",{"key":"C"}]`), &rootFlags{plain: true}); err != nil {
		t.Fatalf("printOutputWithFlags(--plain): %v", err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("mixed array rendered %d lines, want one per entry:\n%s", len(lines), buf.String())
	}
	if lines[1] != "BARE" {
		t.Fatalf("mixed array dropped or reordered its scalar entry:\n%s", buf.String())
	}
}

// Zotero response wrappers rendered as whole JSON objects inside one cell,
// pushing item rows past 2 KB across ~35 columns.
func TestPlainOutputDropsStructuralWrapperColumns(t *testing.T) {
	out := plainOutput(t, `{"results":[{"key":"AAA","library":{"id":1,"type":"user"},
		"links":{"self":{"href":"x"}},"meta":{"numChildren":2},
		"data":{"itemType":"journalArticle","title":"Paper","relations":{}}}]}`)
	header := strings.Split(strings.Split(strings.TrimRight(out, "\n"), "\n")[0], "\t")
	for _, banned := range []string{"library", "links", "meta", "relations"} {
		for _, col := range header {
			if col == banned {
				t.Errorf("column %q is a response wrapper, not bibliographic data: %v", banned, header)
			}
		}
	}
	for _, want := range []string{"key", "title", "itemType"} {
		found := false
		for _, col := range header {
			if col == want {
				found = true
			}
		}
		if !found {
			t.Errorf("header %v dropped bibliographic field %q", header, want)
		}
	}
}

// The wrappers leaked in through the "fields only later records carry" path,
// which appended every unseen field unfiltered. An explicit --select must still
// win there.
func TestPlainHeadersFiltersWrappersFromLaterRecords(t *testing.T) {
	items := []map[string]any{
		{"key": "AAA", "title": "First"},
		{"key": "BBB", "title": "Second", "links": map[string]any{"self": "x"}, "meta": map[string]any{"n": 1}},
	}

	got := plainHeaders(items, false)
	for _, banned := range []string{"links", "meta"} {
		if slices.Contains(got, banned) {
			t.Errorf("headers = %v, want %q dropped without an explicit --select", got, banned)
		}
	}
	if !slices.Contains(got, "key") || !slices.Contains(got, "title") {
		t.Errorf("headers = %v, want the bibliographic fields kept", got)
	}

	explicit := plainHeaders(items, true)
	if !slices.Contains(explicit, "links") || !slices.Contains(explicit, "meta") {
		t.Errorf("headers = %v, want explicitly selected wrappers kept", explicit)
	}
}
