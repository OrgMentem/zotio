// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package client

import (
	"context"
	"encoding/json"
	"strings"
)

// GetWithVersionAndHeaderContext performs a GET and returns the body, the
// Zotero Last-Modified-Version (0 when absent or unparseable) and the trimmed
// value of one more response header (empty when absent).
//
// A paginated read needs both from the SAME response: Last-Modified-Version
// says which library snapshot the page belongs to, and Total-Results says how
// many objects that snapshot holds. Reading them through GetWithVersionContext
// and GetWithHeaderContext would cost two requests whose snapshots can differ.
// Like those helpers it bypasses the read cache and honours ctx; a nil ctx
// falls back to the client base context.
func (c *Client) GetWithVersionAndHeaderContext(ctx context.Context, path string, params map[string]string, header string) (json.RawMessage, int, string, error) {
	if ctx == nil {
		ctx = c.baseCtx()
	}
	respBody, _, hdr, err := c.doRequest(ctx, "GET", path, params, nil, nil)
	if err != nil {
		return nil, 0, "", err
	}
	if hdr == nil {
		return respBody, 0, "", nil
	}
	return respBody, parseLastModifiedVersion(hdr), strings.TrimSpace(hdr.Get(header)), nil
}
