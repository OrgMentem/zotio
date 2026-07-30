// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cobratree

import (
	"encoding/json"
	"strings"
	"testing"

	"zotio/internal/mcp/bound"
)

// The transport carries bound.MaxBytes, so retaining more than that only ever
// fed a truncation. A mirrored export over a large library used to be
// materialized whole to produce at most 60 KB of result.
func TestBoundedCaptureRetainsOnlyTheTransportBudget(t *testing.T) {
	var capture boundedCapture
	chunk := strings.Repeat("x", 8192)
	writes := (bound.MaxBytes / len(chunk)) + 12

	for range writes {
		n, err := capture.Write([]byte(chunk))
		if err != nil || n != len(chunk) {
			t.Fatalf("Write = %d, %v; a short count would truncate the command's own writer", n, err)
		}
	}

	if got := len(capture.String()); got != bound.MaxBytes {
		t.Errorf("retained %d bytes, want the %d-byte cap", got, bound.MaxBytes)
	}
	if want := int64(writes * len(chunk)); capture.Total() != want {
		t.Errorf("Total() = %d, want %d; discarded bytes must still be counted", capture.Total(), want)
	}
}

// Counting the discards is what keeps the truncation notice honest: a result
// that reported only the retained size would tell an agent its 400 MB export
// was 60 KB.
func TestTextCaptureReportsTheRealSizeAfterTruncation(t *testing.T) {
	var capture boundedCapture
	chunk := strings.Repeat("y", 1<<20)
	for range 8 {
		_, _ = capture.Write([]byte(chunk))
	}

	rendered := bound.TextCapture(capture.String(), capture.Total())
	var envelope struct {
		Truncated     bool  `json:"truncated"`
		OriginalBytes int64 `json:"original_bytes"`
		MaxBytes      int   `json:"max_bytes"`
	}
	if err := json.Unmarshal([]byte(rendered), &envelope); err != nil {
		t.Fatalf("oversized result is not a self-describing envelope: %v", err)
	}
	if !envelope.Truncated {
		t.Error("envelope does not report truncation")
	}
	if want := int64(8 << 20); envelope.OriginalBytes != want {
		t.Errorf("original_bytes = %d, want %d", envelope.OriginalBytes, want)
	}
	if len(rendered) > bound.MaxBytes {
		t.Errorf("rendered result is %d bytes, over the %d budget", len(rendered), bound.MaxBytes)
	}
}

// Output that fits must arrive byte-exact: a host parsing a small JSON result
// cannot be handed an envelope instead.
func TestTextCapturePassesSmallOutputThroughUnchanged(t *testing.T) {
	var capture boundedCapture
	payload := `{"ok":true,"items":[]}`
	_, _ = capture.Write([]byte(payload))

	if got := bound.TextCapture(capture.String(), capture.Total()); got != payload {
		t.Errorf("TextCapture = %q, want the payload unchanged", got)
	}
}
