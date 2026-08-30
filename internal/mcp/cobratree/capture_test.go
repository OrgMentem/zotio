// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cobratree

import (
	"encoding/json"
	"strings"
	"sync"
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

// TestBoundedCaptureConcurrentWrites pins the writer contract. One instance
// receives BOTH stdout and stderr of every mirrored command
// (runMirroredInProcess), and a command may write from its own goroutines:
// sync's worker pool already emits stderr warnings off the main goroutine.
// strings.Builder is not safe for concurrent use, so without the mutex this
// tears the buffer and races the counter. Run under -race to see the failure.
func TestBoundedCaptureConcurrentWrites(t *testing.T) {
	const writers = 8
	const perWriter = 50
	line := []byte("warning: cursor save failed\n")

	var capture boundedCapture
	var wg sync.WaitGroup
	// A start gate, so the writers actually overlap instead of finishing in
	// spawn order; without it the race is easy to miss.
	start := make(chan struct{})
	errs := make(chan error, writers)
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range perWriter {
				if _, err := capture.Write(line); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("Write: %v", err)
	}

	want := int64(writers * perWriter * len(line))
	if got := capture.Total(); got != want {
		t.Errorf("Total() = %d, want %d: the counter lost writes", got, want)
	}
	// Every retained byte belongs to some copy of the line; a torn buffer would
	// leave a partial or interleaved fragment.
	retained := capture.String()
	if len(retained) > bound.MaxBytes {
		t.Fatalf("retained %d bytes, want at most %d", len(retained), bound.MaxBytes)
	}
	if rest := strings.ReplaceAll(retained, string(line), ""); rest != "" {
		t.Errorf("retained output contains a torn fragment: %q", rest)
	}
}
