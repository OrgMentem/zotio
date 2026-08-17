// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"encoding/json"
	"testing"
)

func TestFilterAnnotationItems_ColorNameMatchesHex(t *testing.T) {
	// Store annotations as hex values, as Zotero does.
	items := []map[string]any{
		{"key": "A1", "data": map[string]any{"annotationColor": "#ffd400", "annotationType": "highlight"}}, // yellow
		{"key": "A2", "data": map[string]any{"annotationColor": "#ff6666", "annotationType": "highlight"}}, // red
		{"key": "A3", "data": map[string]any{"annotationColor": "#a28ae5", "annotationType": "note"}},      // purple
	}
	data, err := json.Marshal(items)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// --color yellow must match #ffd400
	out, err := filterAnnotationItems(json.RawMessage(data), "yellow", "")
	if err != nil {
		t.Fatalf("filter yellow: %v", err)
	}
	var got []json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("--color yellow: got %d results, want 1 (should match #ffd400)", len(got))
	}
	if c := jsonStringField(got[0], "annotationColor"); c != "#ffd400" {
		t.Fatalf("matched wrong color: %q", c)
	}

	// --color #ffd400 (raw hex) must still match
	out, err = filterAnnotationItems(json.RawMessage(data), "#ffd400", "")
	if err != nil {
		t.Fatalf("filter #ffd400: %v", err)
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal hex: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("--color #ffd400: got %d, want 1", len(got))
	}

	// --color red must match #ff6666 but not yellow/purple
	out, err = filterAnnotationItems(json.RawMessage(data), "red", "")
	if err != nil {
		t.Fatalf("filter red: %v", err)
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal red: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("--color red: got %d, want 1", len(got))
	}
	if c := jsonStringField(got[0], "annotationColor"); c != "#ff6666" {
		t.Fatalf("red matched wrong: %q", c)
	}

	// case-insensitive name
	out, err = filterAnnotationItems(json.RawMessage(data), "YELLOW", "")
	if err != nil {
		t.Fatalf("filter YELLOW: %v", err)
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal YELLOW: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("--color YELLOW (case-insensitive): got %d, want 1", len(got))
	}

	// case-insensitive hex
	out, err = filterAnnotationItems(json.RawMessage(data), "#FFD400", "")
	if err != nil {
		t.Fatalf("filter #FFD400: %v", err)
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal #FFD400: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("--color #FFD400 (case-insensitive hex): got %d, want 1", len(got))
	}

	// --color yellow must NOT match red or purple
	out, err = filterAnnotationItems(json.RawMessage(data), "yellow", "")
	if err != nil {
		t.Fatalf("filter yellow again: %v", err)
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, raw := range got {
		if c := jsonStringField(raw, "annotationColor"); c == "#ff6666" || c == "#a28ae5" {
			t.Fatalf("--color yellow matched non-yellow %q", c)
		}
	}
}

func TestFilterAnnotationItems_ColorWithWhitespace(t *testing.T) {
	items := []map[string]any{
		{"key": "A1", "data": map[string]any{"annotationColor": "#ffd400"}},
	}
	data, _ := json.Marshal(items)
	out, err := filterAnnotationItems(json.RawMessage(data), "  yellow  ", "")
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	var got []json.RawMessage
	_ = json.Unmarshal(out, &got)
	if len(got) != 1 {
		t.Fatalf("trimmed yellow: got %d, want 1", len(got))
	}
}

func TestFilterAnnotationItems_AllColors(t *testing.T) {
	cases := []struct{ name, hex string }{
		{"yellow", "#ffd400"},
		{"red", "#ff6666"},
		{"green", "#5fb236"},
		{"blue", "#2ea8e5"},
		{"purple", "#a28ae5"},
		{"orange", "#f19837"},
	}
	for _, tc := range cases {
		items := []map[string]any{
			{"key": "A1", "data": map[string]any{"annotationColor": tc.hex}},
			{"key": "A2", "data": map[string]any{"annotationColor": "#000000"}},
		}
		data, _ := json.Marshal(items)
		out, err := filterAnnotationItems(json.RawMessage(data), tc.name, "")
		if err != nil {
			t.Fatalf("%s filter: %v", tc.name, err)
		}
		var got []json.RawMessage
		_ = json.Unmarshal(out, &got)
		if len(got) != 1 {
			t.Fatalf("%s: got %d, want 1 (hex %s)", tc.name, len(got), tc.hex)
		}
		if c := jsonStringField(got[0], "annotationColor"); c != tc.hex {
			t.Fatalf("%s matched %q, want %q", tc.name, c, tc.hex)
		}
	}
}
