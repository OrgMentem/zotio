// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"zotio/internal/client"
)

// resolveListRead is resolveRead for a list command that documents `--limit 0`
// as "no limit". Zotero serves one page per request (25 rows by default, 100 at
// most), so a single read would print a partial list as the authoritative
// answer. On the live plane a request that asks for everything (limit 0 or
// absent) or for more than one page (limit above 100) therefore walks pages
// from the requested start offset; a window of 1..100 rows stays one request.
// The local mirror is unaffected: it already honors limit 0.
func resolveListRead(ctx context.Context, c *client.Client, flags *rootFlags, resourceType string, isList bool, path string, params map[string]string) (json.RawMessage, DataProvenance, error) {
	return resolveReadVia(ctx, flags, resourceType, isList, path, params, func() (json.RawMessage, error) {
		return fetchLiveListPages(ctx, c, path, params)
	})
}

// fetchLiveListPages reads path page by page with limit=zoteroPageMax and
// advancing start offsets until a short page, then returns the rows as one
// JSON array. params carries the caller's filters plus an optional --limit and
// --start.
//
// A page that is null or not an array is an error, never the end of the list.
// Where Zotero sends Total-Results it must agree with the rows received, so a
// well-formed but partial answer is refused instead of passing for the end.
func fetchLiveListPages(ctx context.Context, c *client.Client, path string, params map[string]string) (json.RawMessage, error) {
	limit, limitErr := strconv.Atoi(params["limit"])
	if params["limit"] == "" {
		limit, limitErr = 0, nil
	}
	start, startErr := strconv.Atoi(params["start"])
	if params["start"] == "" {
		start, startErr = 0, nil
	}
	// Only a plain JSON array can be walked: other export formats return an
	// opaque document or an object. A one-page window is one request.
	if limitErr != nil || startErr != nil || (params["format"] != "" && params["format"] != "json") ||
		(limit > 0 && limit <= zoteroPageMax) {
		return c.GetWithHeadersContext(ctx, path, params, nil)
	}

	var (
		rows    []json.RawMessage
		first   json.RawMessage
		prev    []byte
		totals  pageTotals
		offset  = start
		natural bool
	)
	for pages := 0; ; pages++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		want := zoteroPageMax
		if limit > 0 {
			if remaining := limit - len(rows); remaining < want {
				want = remaining
			}
		}
		pageParams := make(map[string]string, len(params)+2)
		for key, value := range params {
			pageParams[key] = value
		}
		pageParams["start"] = strconv.Itoa(offset)
		pageParams["limit"] = strconv.Itoa(want)

		data, headers, err := c.GetWithResponseHeadersContext(ctx, path, pageParams)
		if err != nil {
			return nil, err
		}
		// Same fixed, body-free error every other live read reports.
		if err := validateJSONReadBody(data, params); err != nil {
			return nil, err
		}
		if repeatedPage(prev, data) {
			return nil, apiErr(fmt.Errorf("pagination for %s ignored start %d", path, offset))
		}
		prev = data
		page, err := decodeListPage[json.RawMessage](data, fmt.Sprintf("%s page at start %d", path, offset))
		if err != nil {
			return nil, err
		}
		// The desktop API clamps an offset past the end and answers []: a valid
		// empty window, not a page that overruns Total-Results. Only a first
		// page can start past the end; a non-empty page there is still refused
		// by observe below.
		if pages == 0 && len(page) == 0 {
			if total, present, terr := parseTotalResults(headers.Get("Total-Results")); terr == nil && present && offset >= total {
				return json.RawMessage("[]"), nil
			}
		}
		if err := totals.observe(headers.Get("Total-Results"), offset, len(page)); err != nil {
			return nil, apiErr(fmt.Errorf("%s page at start %d: %w", path, offset, err))
		}
		if pages == 0 {
			first = data
		}
		rows = append(rows, page...)
		offset += len(page)
		if len(page) < want {
			natural = true
			break
		}
		if limit > 0 && len(rows) >= limit {
			break
		}
	}
	if natural {
		if err := totals.finish(offset); err != nil {
			return nil, apiErr(fmt.Errorf("%s: %w", path, err))
		}
	}
	if len(rows) == 0 {
		return json.RawMessage("[]"), nil
	}
	// One short page is the whole answer: return its bytes untouched.
	if len(rows) < zoteroPageMax && first != nil {
		return first, nil
	}
	return json.Marshal(rows)
}
