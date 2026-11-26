// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// decodeListPage decodes one page of a paged Zotero list read. Every walker
// that ends on a short page must decode through it: json.Unmarshal reads a
// JSON null body into a nil slice with no error, which a length check cannot
// tell from a real empty page, so a null page would pass for the end of the
// list and silently truncate the walk. Only an actual array may end a walk;
// null and any other shape are a malformed answer (an API error, exit 5).
//
// what names the read in the error, e.g. "children of ABCD1234 at start 100".
func decodeListPage[T any](data []byte, what string) ([]T, error) {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil, apiErr(fmt.Errorf("%s returned JSON null instead of an array; refusing to treat it as the end of the list", what))
	}
	var page []T
	if err := json.Unmarshal(data, &page); err != nil {
		return nil, apiErr(fmt.Errorf("decoding %s: %w", what, err))
	}
	return page, nil
}
