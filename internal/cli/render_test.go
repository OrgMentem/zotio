// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDisplayWidth(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{"plain ascii", "hello", 5},
		{"empty", "", 0},
		{"ansi bold stripped", "\033[1mKEY\033[0m", 3},
		{"ansi dim with text around", "a\033[2mbb\033[0mc", 4},
		{"cjk wide runes", "深層学習", 8},
		{"mixed ascii cjk", "ai論文", 6},
		{"ansi wrapping cjk", "\033[36m論\033[0m", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := displayWidth(tt.in); got != tt.want {
				t.Errorf("displayWidth(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestTruncateRuneSafe(t *testing.T) {
	// Byte-slicing "日本語の論文タイトル" mid-rune produced mojibake before;
	// truncate must always return valid UTF-8 and honor display width.
	s := "日本語の論文タイトルとても長い"
	got := truncate(s, 12)
	if !utf8.ValidString(got) {
		t.Fatalf("truncate returned invalid UTF-8: %q", got)
	}
	if w := displayWidth(got); w > 12 {
		t.Errorf("truncate width = %d, want <= 12 (%q)", w, got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("expected ellipsis suffix, got %q", got)
	}
	// Short strings pass through untouched.
	if got := truncate("short", 60); got != "short" {
		t.Errorf("truncate(short) = %q", got)
	}
}

func TestRenderColumnsAlignment(t *testing.T) {
	// Columns must align on display width even when cells mix CJK and ASCII.
	// (Color is off here: tests run without a TTY, so styling is a no-op and
	// the alignment assertion sees exactly what a pipe would.)
	var b strings.Builder
	headers := []string{"key", "title", "itemType"}
	rows := [][]string{
		{"ABCD1234", "ImageNet classification", "journalArticle"},
		{"EFGH5678", "深層学習による物体検出", "journalArticle"},
		{"IJKL9012", "x", "book"},
	}
	if err := renderColumns(&b, headers, rows); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("got %d lines, want 4:\n%s", len(lines), b.String())
	}
	// The last column must start at the same display offset on every line.
	wantOffset := -1
	for i, line := range lines {
		idx := strings.LastIndex(line, "  ")
		if idx < 0 {
			t.Fatalf("line %d has no column gap: %q", i, line)
		}
		offset := displayWidth(line[:idx+2])
		if wantOffset == -1 {
			wantOffset = offset
		} else if offset != wantOffset {
			t.Errorf("line %d: last column starts at %d, want %d\n%s", i, offset, wantOffset, b.String())
		}
	}
	// No trailing whitespace on any line.
	for i, line := range lines {
		if strings.TrimRight(line, " ") != line {
			t.Errorf("line %d has trailing spaces: %q", i, line)
		}
	}
}

func TestFormatObjectSummaryZoteroShapes(t *testing.T) {
	tests := []struct {
		name string
		in   map[string]any
		want string
	}{
		{"tag", map[string]any{"tag": "/unread", "type": float64(1)}, "/unread"},
		{"author creator", map[string]any{"creatorType": "author", "firstName": "Alex", "lastName": "Krizhevsky"}, "Alex Krizhevsky"},
		{"editor creator annotated", map[string]any{"creatorType": "editor", "firstName": "Jane", "lastName": "Doe"}, "Jane Doe (editor)"},
		{"single-field name creator", map[string]any{"creatorType": "author", "name": "DeepMind"}, "DeepMind"},
		{"named object", map[string]any{"name": "Machine Learning"}, "Machine Learning"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatObjectSummary(tt.in); got != tt.want {
				t.Errorf("formatObjectSummary(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
	// Unknown shapes fall back to JSON, never empty.
	if got := formatObjectSummary(map[string]any{"weird": true}); got == "" {
		t.Error("unknown shape returned empty string")
	}
}

func TestPrintAutoCardsLabelAlignment(t *testing.T) {
	var b strings.Builder
	items := []map[string]any{{
		"key":          "ABCD1234",
		"title":        "A Paper",
		"dateModified": "2026-07-11T00:00:00Z",
		"dateAdded":    "2026-07-10T00:00:00Z",
		"version":      float64(12042),
		"itemType":     "journalArticle",
		"citationKey":  "doe2026paper",
		"library":      "My Library",
		"tags":         []any{map[string]any{"tag": "/unread", "type": float64(1)}},
	}}
	if err := printAutoCards(&b, items); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	// Every "label: value" line must put the value at the same column.
	valueCol := -1
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimPrefix(line, "  ")
		if trimmed == line || !strings.Contains(trimmed, ":") || strings.HasPrefix(line, "    ") {
			continue
		}
		rest := strings.TrimLeft(trimmed[strings.Index(trimmed, ":")+1:], " ")
		if rest == "" {
			continue // multi-line label like "tags:"
		}
		col := displayWidth(line) - displayWidth(rest)
		if valueCol == -1 {
			valueCol = col
		} else if col != valueCol {
			t.Errorf("value column %d != %d on line %q\nfull output:\n%s", col, valueCol, line, out)
		}
	}
	// Tags must render as tag names, not raw JSON, and be indented.
	if strings.Contains(out, `{"tag"`) {
		t.Errorf("tags rendered as raw JSON:\n%s", out)
	}
	if !strings.Contains(out, "    /unread") {
		t.Errorf("expected indented tag '/unread':\n%s", out)
	}
}
