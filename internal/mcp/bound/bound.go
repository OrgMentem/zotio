// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

package bound

import (
	"encoding/json"
	"unicode/utf8"
)

const (
	MaxBytes = 60000
	MaxItems = 50

	maxPreviewBytes = 4000
)

const (
	endpointListNote = "Typed MCP endpoint response was bounded for MCP output. Narrow the request with limit, offset, filters, search/sql, or a command-mirror tool with --agent/--compact/--select."
	jsonResultNote   = "MCP JSON result exceeded the tool result budget. Narrow the request with limit, filters, search/sql, or --select/--compact where available."
	textResultNote   = "MCP command output exceeded the tool result budget. Rerun with narrower flags, --agent, --compact, --select, or --limit where available."
)

// TextCapture renders command output that was captured under a retention cap.
// prefix is what was kept, total is how many bytes the command actually wrote.
//
// Splitting the two is what lets the caller stop buffering at MaxBytes instead
// of materializing gigabytes to report 60 KB: the preview only ever quotes the
// first few KB, and original_bytes stays truthful because the writer counted
// everything it discarded.
func TextCapture(prefix string, total int64) string {
	if total <= MaxBytes && total == int64(len(prefix)) {
		return prefix
	}
	return textCapturePreview(prefix, total)
}

// textCapturePreview renders a preview even when the retained output itself
// fits the transport cap. Callers use it when their enclosing transport
// framing needs part of that cap.
func textCapturePreview(prefix string, total int64) string {
	return previewEnvelopeSized([]byte(prefix), total, textResultNote)
}

func fitJSONItems(items []json.RawMessage, build func([]json.RawMessage) any) []byte {
	limit := len(items)
	if limit > MaxItems {
		limit = MaxItems
	}
	for n := limit; n > 0; n-- {
		out, err := json.Marshal(build(items[:n]))
		if err != nil {
			continue
		}
		if len(out) <= MaxBytes {
			return out
		}
	}
	out, _ := json.Marshal(build(items[:0]))
	return out
}

// previewEnvelopeSized reports originalBytes separately from what it was handed,
// so a caller that stopped buffering early still describes the real output size.
func previewEnvelopeSized(data []byte, originalBytes int64, note string) string {
	limit := len(data)
	if limit > maxPreviewBytes {
		limit = maxPreviewBytes
	}
	for limit >= 0 {
		out, err := json.Marshal(map[string]any{
			"truncated":      true,
			"resumable":      false,
			"original_bytes": originalBytes,
			"max_bytes":      MaxBytes,
			"preview":        previewString(data, limit),
			"note":           note,
		})
		if err == nil && len(out) <= MaxBytes {
			return string(out)
		}
		if limit == 0 {
			break
		}
		if limit < 512 {
			limit = 0
		} else {
			limit -= 512
		}
	}
	out, _ := json.Marshal(map[string]any{
		"truncated":      true,
		"resumable":      false,
		"original_bytes": len(data),
		"max_bytes":      MaxBytes,
		"note":           note,
	})
	return string(out)
}

func previewString(data []byte, limit int) string {
	if limit > len(data) {
		limit = len(data)
	}
	for limit > 0 {
		r, size := utf8.DecodeLastRune(data[:limit])
		if r != utf8.RuneError || size != 1 {
			break
		}
		limit--
	}
	return string(data[:limit])
}
