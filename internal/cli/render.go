// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"io"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/width"
)

// displayWidth returns the on-screen column width of s: ANSI SGR escape
// sequences count as zero and East Asian wide/fullwidth runes count as two.
// This is what alignment must be computed from — byte or rune counts drift
// as soon as a cell contains color codes or CJK text.
func displayWidth(s string) int {
	w := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b { // ESC: skip a CSI sequence through its final byte
			j := i + 1
			if j < len(s) && s[j] == '[' {
				j++
				for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
					j++
				}
				if j < len(s) {
					j++ // consume the final byte (e.g. 'm')
				}
			}
			i = j
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		w += runeWidth(r)
		i += size
	}
	return w
}

func runeWidth(r rune) int {
	switch width.LookupRune(r).Kind() {
	case width.EastAsianWide, width.EastAsianFullwidth:
		return 2
	}
	return 1
}

// padRight pads s with spaces to w display columns. Strings already at or
// beyond w are returned unchanged.
func padRight(s string, w int) string {
	gap := w - displayWidth(s)
	if gap <= 0 {
		return s
	}
	return s + strings.Repeat(" ", gap)
}

// truncate shortens s to at most max display columns, appending "..." when
// content was dropped. Rune-safe: never splits a UTF-8 sequence.
func truncate(s string, max int) string {
	if displayWidth(s) <= max {
		return s
	}
	budget := max - 3 // reserve room for "..."
	if budget < 1 {
		budget = max
	}
	w := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		rw := runeWidth(r)
		if w+rw > budget {
			if budget == max { // degenerate max <= 3: hard cut, no ellipsis
				return s[:i]
			}
			return s[:i] + "..."
		}
		w += rw
		i += size
	}
	return s
}

// columnStyle picks a subtle color for a table column by header name:
// identity keys and timestamps recede (dim), type-ish fields get an accent.
// Values stay untouched — styling only ever wraps, never rewrites.
func columnStyle(header string) func(string) string {
	h := strings.ReplaceAll(strings.ToLower(header), "_", "")
	switch {
	case h == "key" || h == "version":
		return dim
	case strings.Contains(h, "date") || h == "created" || h == "updated":
		return dim
	case h == "itemtype" || h == "type" || h == "kind":
		return cyan
	case h == "status":
		return statusStyle
	}
	return func(s string) string { return s }
}

// statusStyle colors a status cell by severity: red for retraction-grade
// findings, yellow for cautionary ones, green for healthy states. Unknown
// statuses pass through unstyled.
func statusStyle(v string) string {
	switch strings.ToLower(v) {
	case "retracted", "error", "failed", "missing":
		return red(v)
	case "correction", "concern", "expression of concern", "warning", "stale":
		return yellow(v)
	case "ok", "clear", "active", "healthy":
		return green(v)
	}
	return v
}

// renderColumns writes an aligned table: bold uppercase headers, per-column
// styles, alignment computed from display width so ANSI codes and wide runes
// never skew padding. The last column is not padded (no trailing spaces).
func renderColumns(w io.Writer, headers []string, rows [][]string) error {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = displayWidth(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if cw := displayWidth(cell); cw > widths[i] {
				widths[i] = cw
			}
		}
	}

	var b strings.Builder
	for i, h := range headers {
		cell := bold(strings.ToUpper(strings.ReplaceAll(h, "_", " ")))
		if i < len(headers)-1 {
			cell = padRight(cell, widths[i]) + "  "
		}
		b.WriteString(cell)
	}
	b.WriteByte('\n')

	styles := make([]func(string) string, len(headers))
	for i, h := range headers {
		styles[i] = columnStyle(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			cell = styles[i](cell)
			if i < len(row)-1 {
				cell = padRight(cell, widths[i]) + "  "
			}
			b.WriteString(cell)
		}
		b.WriteByte('\n')
	}
	_, err := io.WriteString(w, b.String())
	return err
}
