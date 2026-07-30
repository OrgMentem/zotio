// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cobratree

import (
	"strings"

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
	buf   strings.Builder
	total int64
}

func (c *boundedCapture) Write(p []byte) (int, error) {
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
func (c *boundedCapture) String() string { return c.buf.String() }

// Total returns everything the command wrote, including bytes dropped past the
// cap, so the truncation notice does not understate the output.
func (c *boundedCapture) Total() int64 { return c.total }
