// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// tags list's --plain rows must be explicable when a tag exists as both
// automatic and manual.

package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// N4-1: tags list returns one row per (tag, type) pair, so a tag existing as
// both automatic and manual — Depression is type 1 on 10 items and type 0 on 5 —
// produced two rows with identical "tag" values and no visible difference,
// because --plain drops "meta" as a structural wrapper.
func TestFlattenTagMetaForDisplaySurfacesTypeAndCount(t *testing.T) {
	in := json.RawMessage(`[
		{"tag":"Depression","meta":{"type":1,"numItems":10},"links":{}},
		{"tag":"Depression","meta":{"type":0,"numItems":5},"links":{}}
	]`)
	out := flattenTagMetaForDisplay(in)

	var tags []map[string]any
	if err := json.Unmarshal(out, &tags); err != nil {
		t.Fatalf("decode flattened output: %v", err)
	}
	if len(tags) != 2 {
		t.Fatalf("got %d tags, want 2", len(tags))
	}
	if got := tags[0]["type"]; got != float64(1) {
		t.Errorf("tags[0][type] = %v, want 1", got)
	}
	if got := tags[0]["num_items"]; got != float64(10) {
		t.Errorf("tags[0][num_items] = %v, want 10", got)
	}
	if got := tags[1]["type"]; got != float64(0) {
		t.Errorf("tags[1][type] = %v, want 0", got)
	}
	// The raw meta object must survive too: JSON consumers already had this,
	// and flattening must add columns, not remove information.
	if _, ok := tags[0]["meta"]; !ok {
		t.Error("flattening dropped the original meta object")
	}
}

// A payload without meta (or not a list) passes through unchanged rather than
// erroring.
func TestFlattenTagMetaForDisplayPassesThroughUnaffectedInput(t *testing.T) {
	for _, in := range []json.RawMessage{
		json.RawMessage(`[{"tag":"NoMeta"}]`),
		json.RawMessage(`{"not":"a list"}`),
		json.RawMessage(`not json`),
	} {
		if got := flattenTagMetaForDisplay(in); string(got) != string(in) {
			t.Errorf("flattenTagMetaForDisplay(%s) = %s, want unchanged", in, got)
		}
	}
}

// End to end: --plain must show a column distinguishing the two Depression
// rows, not two identical lines.
func TestTagsListPlainDistinguishesDuplicateTagNames(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"tag":"Depression","meta":{"type":1,"numItems":10}},
			{"tag":"Depression","meta":{"type":0,"numItems":5}}
		]`))
	}))
	defer srv.Close()

	cmd := newTagsListCmd(&rootFlags{plain: true, configPath: testConfigFile(t, srv.URL+"/users/0")})
	out := runTrashCmd(t, cmd)

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("want a header plus 2 rows, got %d lines:\n%s", len(lines), out)
	}
	if !strings.Contains(lines[0], "type") {
		t.Fatalf("header %q has no type column to disambiguate the duplicate tag names", lines[0])
	}
	if lines[1] == lines[2] {
		t.Fatalf("the two Depression rows are identical and inexplicable:\n%s\n%s", lines[1], lines[2])
	}
}
