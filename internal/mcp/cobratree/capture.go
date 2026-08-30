// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cobratree

import (
	"strings"
	"sync"

	"zotio/internal/mcp/bound"
)

// boundedCapture collects a mirrored command's output up to the transport's
// budget and counts the rest.
//
// The MCP result can carry bound.MaxBytes, so retaining more than that only
// ever fed a truncation. A mirrored `items list` or `annotations export` over a
// large library was materialized in full -- twice, once in the buffer and once
// in String -- to produce at most 60 KB of tool result.
//
// Truncating here is safe in a way it is not for workflow step output: this
// text is terminal for the call. Nothing downstream parses it as a contract,
// and an oversized result becomes a self-describing preview envelope that names
// the real size, which is why the discarded bytes are still counted.
type boundedCapture struct {
	// mu guards buf and total. runMirroredInProcess points BOTH stdout and
	// stderr of every mirrored command at one instance, and a command is free to
	// write from its own goroutines (sync's worker pool already writes warnings
	// to stderr off the main goroutine). strings.Builder is not safe for
	// concurrent use, so without this the writes tear. The writes are bounded by
	// bound.MaxBytes, so the lock costs nothing worth measuring.
	mu    sync.Mutex
	buf   strings.Builder
	total int64
}

func (c *boundedCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := bound.MaxBytes - c.buf.Len(); room > 0 {
		if len(p) <= room {
			c.buf.Write(p)
		} else {
			c.buf.Write(p[:room])
		}
	}
	c.total += int64(len(p))
	return len(p), nil
}

// String returns the retained prefix, never more than bound.MaxBytes.
func (c *boundedCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// Total returns everything the command wrote, including bytes dropped past the
// cap, so the truncation notice does not understate the output.
func (c *boundedCapture) Total() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}
