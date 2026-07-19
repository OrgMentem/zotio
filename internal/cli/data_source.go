// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"zotio/internal/client"
	"zotio/internal/store"
)

// isNetworkError returns true for errors caused by network connectivity issues
// (DNS, connection refused, timeout). HTTP 4xx/5xx errors are NOT network errors.
func isNetworkError(err error) bool {
	if err == nil {
		return false
	}
	var urlErr *url.Error
	if As(err, &urlErr) {
		// url.Error wraps the underlying network error
		err = urlErr.Err
	}
	var netErr *net.OpError
	if As(err, &netErr) {
		return true
	}
	var dnsErr *net.DNSError
	if As(err, &dnsErr) {
		return true
	}
	// Check for common network error strings
	msg := err.Error()
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "network is unreachable") ||
		strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "TLS handshake timeout")
}

// openStoreForRead opens the local SQLite store for reading.
// Returns nil, nil if the database file does not exist (no sync has been run).
func openStoreForRead(ctx context.Context, cliName string) (*store.Store, error) {
	dbPath := defaultDBPath(cliName)
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}
	return store.OpenWithContext(ctx, dbPath)
}

// localProvenance builds a DataProvenance for local data reads.
func localProvenance(db *store.Store, resourceType, reason string) DataProvenance {
	prov := DataProvenance{
		Source:       "local",
		Reason:       reason,
		ResourceType: resourceType,
	}
	_, lastSynced, _, err := db.GetSyncState(resourceType)
	if err == nil && !lastSynced.IsZero() {
		prov.SyncedAt = &lastSynced
	}
	return prov
}

func attachFreshness(prov DataProvenance, flags *rootFlags) DataProvenance {
	if flags != nil {
		prov.Freshness = flags.freshnessMeta
	}
	return prov
}

// isLocalListRead detects generated list commands that still pass isList=false.
// base resource paths ("/collections", "/tags",
// "/searches") contain no concrete item key, so local mode must list rows
// instead of treating the resource name as an ID.
func isLocalListRead(resourceType string, isList bool, path string) bool {
	if isList {
		return true
	}
	return strings.Trim(path, "/") == resourceType
}

// resolveRead dispatches a GET request to either the live API or local store
// based on the --data-source flag. Returns the response data and provenance metadata.
//
// Parameters:
//   - c: the HTTP client for live API calls
//   - flags: root flags containing dataSource setting
//   - resourceType: the store resource type name (e.g., "links", "domains")
//   - isList: true for list endpoints, false for get-by-ID endpoints
//   - path: the API path (e.g., "/links" or "/links/abc123")
//   - params: query parameters for the API call
//   - headers: per-endpoint required headers (e.g. cal-api-version, Stripe-Version)
//     baked in by the command template at codegen time. Pass nil when the endpoint
//     declares no per-endpoint header overrides. Without this parameter, store-backed
//     reads on per-endpoint-versioned APIs silently get the wrong response shape
//     (cal-com retro #334 F1).
func resolveRead(ctx context.Context, c *client.Client, flags *rootFlags, resourceType string, isList bool, path string, params map[string]string, headers map[string]string) (json.RawMessage, DataProvenance, error) {
	switch flags.dataSource {
	case "local":
		data, prov, err := resolveLocal(ctx, resourceType, isList, path, params, "user_requested")
		return data, attachFreshness(prov, flags), err

	case "live":
		data, err := c.GetWithHeaders(path, params, headers)
		if err != nil {
			return nil, DataProvenance{}, err
		}
		return data, attachFreshness(DataProvenance{Source: "live"}, flags), nil

	default: // "auto"
		data, err := c.GetWithHeaders(path, params, headers)
		if err == nil {
			writeThroughCache(ctx, resourceType, data)
			return data, attachFreshness(DataProvenance{Source: "live"}, flags), nil
		}
		if !isNetworkError(err) {
			// HTTP 4xx/5xx errors propagate — not a fallback case
			return nil, DataProvenance{}, err
		}
		// Network error — try local fallback
		fallbackData, fallbackProv, fallbackErr := resolveLocal(ctx, resourceType, isList, path, params, "api_unreachable")
		if fallbackErr != nil {
			return nil, DataProvenance{}, fmt.Errorf("API unreachable and no local data. Run 'zotio sync' to enable offline access.\n\nOriginal error: %w", err)
		}
		return fallbackData, attachFreshness(fallbackProv, flags), nil
	}
}

// writeThroughCache upserts live API results into the local SQLite store so
// FTS search covers everything the user has looked up — not just explicit syncs.
// Best-effort: the live result already succeeded, so a cache write failure is
// non-fatal and only emits a stderr warning (it never fails the read path).
func writeThroughCache(ctx context.Context, resourceType string, data json.RawMessage) {
	// schema/type lists (itemTypes, itemFields, …) are read-only reference
	// data, not library content — skip the cache. Tags flow through: the
	// store's ResourceIDFieldOverrides keys them by tag name.
	if resourceType == "schema" {
		return
	}
	db, err := store.OpenWithContext(ctx, defaultDBPath("zotio"))
	if err != nil {
		return
	}
	defer db.Close()

	// Collect items to upsert from various response shapes
	var items []json.RawMessage

	// Try direct array first
	if json.Unmarshal(data, &items) != nil || len(items) == 0 {
		items = nil
		// Try object — check for common envelope patterns (results, data, items)
		var envelope map[string]json.RawMessage
		if json.Unmarshal(data, &envelope) == nil {
			for _, key := range []string{"results", "data", "items"} {
				if raw, ok := envelope[key]; ok {
					var arr []json.RawMessage
					if json.Unmarshal(raw, &arr) == nil && len(arr) > 0 {
						items = arr
						break
					}
				}
			}
			// Single object with an id field (e.g., detail response)
			if items == nil {
				if _, ok := envelope["id"]; ok {
					if _, _, err := db.UpsertBatch(resourceType, []json.RawMessage{data}); err != nil {
						fmt.Fprintf(os.Stderr, "warning: write-through cache failed for %q: %v\n", resourceType, err)
					}
					return
				}
			}
		}
	}

	if len(items) > 0 {
		if _, _, err := db.UpsertBatch(resourceType, items); err != nil {
			fmt.Fprintf(os.Stderr, "warning: write-through cache failed for %q: %v\n", resourceType, err)
		}
	}
}

// resolveLocal reads data from the local SQLite store.
// Item-list endpoints are routed through resolveLocalItemList so supported
// Zotero filters/scopes are applied locally. Other endpoints fall back to a
// generic resource dump and warn when request params cannot be reproduced.
func resolveLocal(ctx context.Context, resourceType string, isList bool, path string, params map[string]string, reason string) (json.RawMessage, DataProvenance, error) {
	db, err := openStoreForRead(ctx, "zotio")
	if err != nil {
		return nil, DataProvenance{}, fmt.Errorf("opening local database: %w\nRun 'zotio sync' first.", err)
	}
	if db == nil {
		return nil, DataProvenance{}, fmt.Errorf("no local data. Run 'zotio sync' first")
	}
	defer db.Close()

	prov := localProvenance(db, resourceType, reason)

	// Zotero-aware local query planner. For item-list reads
	// (/items, /items/top, /collections/{key}/items[/top]) apply the same
	// scopes the live endpoint would (itemType, tag, collection, top-level,
	// quick-search, sort, direction, limit, start) instead of dumping all
	// synced rows. Keyed on the path so it also covers the collection-items
	// command, which labels its resource "collections".
	if data, handled, qErr := resolveLocalItemList(db, path, params); handled {
		if qErr != nil {
			return nil, DataProvenance{}, qErr
		}
		itemProv := localProvenance(db, "items", reason)
		itemProv.Scoped = true
		return data, itemProv, nil
	}

	if data, handled, qErr := resolveLocalTrashList(db, resourceType, isList, path, params); handled {
		if qErr != nil {
			return nil, DataProvenance{}, qErr
		}
		trashProv := localProvenance(db, "items-trash", reason)
		trashProv.Scoped = true
		return data, trashProv, nil
	}
	// /collections/top is a scoped endpoint, not a collection keyed "top".
	// Local parity for top-level collection filtering is not implemented.
	if resourceType == "collections" && strings.Trim(path, "/") == "collections/top" {
		return nil, DataProvenance{}, fmt.Errorf("unsupported local scope %q: top-level collections are not implemented", path)
	}

	// Warn only when this generic read carries filters local data can't
	// reproduce. limit/start are applied below, so they never warrant a warning.
	// The warning stays scoped to genuinely unreproducible filters; item-list
	// filters returned above are already applied.
	if hasUnreproducibleParams(params) {
		fmt.Fprintf(os.Stderr, "warning: local data may be unfiltered — this endpoint's filters are not applied to cached data\n")
	}

	// list-shaped base paths must dump all local rows
	// even when the generated command passed isList=false.
	if isLocalListRead(resourceType, isList, path) {
		raw, err := db.List(resourceType, 0) // 0 = no limit, return all synced data
		if err != nil {
			return nil, DataProvenance{}, fmt.Errorf("querying local store: %w", err)
		}
		// Filter out empty/invalid records (empty arrays, null, whitespace-only)
		// that can end up in the store from pagination boundary artifacts.
		var items []json.RawMessage
		for _, r := range raw {
			trimmed := strings.TrimSpace(string(r))
			if trimmed == "" || trimmed == "null" || trimmed == "[]" || trimmed == "{}" {
				continue
			}
			items = append(items, r)
		}
		if len(items) == 0 {
			return nil, DataProvenance{}, fmt.Errorf("no local data for %q. Run 'zotio sync' first", resourceType)
		}
		// apply start
		// offset then limit so paginated local list reads mirror the live API
		// (limit is also re-applied by the caller's truncateJSONArray).
		items = paginateLocalRows(items, params)
		if len(items) == 0 {
			return json.RawMessage("[]"), prov, nil
		}
		// Marshal []json.RawMessage into a single JSON array
		data, err := json.Marshal(items)
		if err != nil {
			return nil, DataProvenance{}, fmt.Errorf("marshaling local data: %w", err)
		}
		return data, prov, nil
	}

	// Get by ID — extract and unescape the final path segment as the ID.
	parts := strings.Split(strings.TrimRight(path, "/"), "/")
	encodedID := parts[len(parts)-1]
	id, err := url.PathUnescape(encodedID)
	if err != nil {
		return nil, DataProvenance{}, fmt.Errorf("unescaping local resource ID %q: %w", encodedID, err)
	}

	item, err := db.Get(resourceType, id)
	if err != nil {
		return nil, DataProvenance{}, fmt.Errorf("querying local store: %w", err)
	}
	if item == nil {
		return nil, DataProvenance{}, fmt.Errorf("resource %q with ID %q not found in local store. Run 'zotio sync' first", resourceType, id)
	}
	return item, prov, nil
}

// reproducibleLocalParams are request params a generic local list read can
// honor (pagination) or that don't filter the row set (output format), so they
// must not trigger the "local data may be unfiltered" warning..
var reproducibleLocalParams = map[string]bool{"limit": true, "start": true, "format": true}

// hasUnreproducibleParams reports whether params contains any filter a generic
// local read cannot reproduce (anything outside reproducibleLocalParams).
func hasUnreproducibleParams(params map[string]string) bool {
	for k, v := range params {
		if v == "" {
			continue
		}
		if !reproducibleLocalParams[k] {
			return true
		}
	}
	return false
}

// paginateLocalRows applies the Zotero start offset then limit to a generic
// local list result so paginated reads mirror the live API. An out-of-range
// start yields an empty slice (a live page past the end)..
func paginateLocalRows(rows []json.RawMessage, params map[string]string) []json.RawMessage {
	if start, _ := strconv.Atoi(params["start"]); start > 0 {
		if start >= len(rows) {
			return nil
		}
		rows = rows[start:]
	}
	if limit, _ := strconv.Atoi(params["limit"]); limit > 0 && limit < len(rows) {
		rows = rows[:limit]
	}
	return rows
}

// resolveLocalTrashList reproduces the live /items/trash list's default order
// and SQL pagination. A completed empty sync is valid local data; the absence
// of both rows and a sync timestamp remains a loud hydration error.
func resolveLocalTrashList(db *store.Store, resourceType string, isList bool, path string, params map[string]string) (json.RawMessage, bool, error) {
	if resourceType != "items-trash" || !isLocalListRead(resourceType, isList, path) {
		return nil, false, nil
	}

	limit, _ := strconv.Atoi(params["limit"])
	start, _ := strconv.Atoi(params["start"])
	rows, err := db.QueryTrash(store.TrashQuery{Limit: limit, Start: start})
	if err != nil {
		return nil, true, fmt.Errorf("querying local trash: %w", err)
	}
	if len(rows) == 0 {
		totalRows, countErr := db.Count("items-trash")
		if countErr != nil {
			return nil, true, fmt.Errorf("counting local trash: %w", countErr)
		}
		if totalRows == 0 {
			_, lastSynced, _, stateErr := db.GetSyncState("items-trash")
			if stateErr != nil {
				return nil, true, fmt.Errorf("querying local trash sync state: %w", stateErr)
			}
			if lastSynced.IsZero() {
				return nil, true, fmt.Errorf("no local data for %q. Run 'zotio sync' first", resourceType)
			}
		}
		return json.RawMessage("[]"), true, nil
	}

	data, err := json.Marshal(rows)
	if err != nil {
		return nil, true, fmt.Errorf("marshaling local trash: %w", err)
	}
	return data, true, nil
}

// resolveLocalItemList runs the Zotero-aware item query planner when the path
// is an item-list endpoint, returning (data, true, err). It returns
// (nil, false, nil) for non-item-list paths so the caller falls back to its
// generic get/list handling. An empty match yields a JSON empty array, which
// mirrors a live list that matched nothing.
func resolveLocalItemList(db *store.Store, path string, params map[string]string) (json.RawMessage, bool, error) {
	collectionKey, parentKey, topOnly, isList := parseItemListPath(path)
	if !isList {
		return nil, false, nil
	}
	q := store.ItemQuery{
		ItemType:   params["itemType"],
		Tag:        params["tag"],
		Collection: collectionKey,
		Parent:     parentKey,
		TopOnly:    topOnly,
		Query:      params["q"],
		Sort:       params["sort"],
		Direction:  params["direction"],
	}
	if v := params["limit"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, true, fmt.Errorf("invalid limit %q: must be an integer", v)
		}
		q.Limit = n
	}
	if v := params["start"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, true, fmt.Errorf("invalid start %q: must be an integer", v)
		}
		q.Start = n
	}
	items, err := db.QueryItems(q)
	if err != nil {
		return nil, true, fmt.Errorf("local item query: %w", err)
	}
	if len(items) == 0 {
		return json.RawMessage("[]"), true, nil
	}
	data, err := json.Marshal(items)
	if err != nil {
		return nil, true, fmt.Errorf("marshaling local items: %w", err)
	}
	return data, true, nil
}

// parseItemListPath classifies a Zotero API path as an item-list endpoint and
// extracts the collection key, parent key, and top-level flag. Recognizes
// /items, /items/top, /collections/{key}/items[/top], and
// /items/{key}/children (scoped to a parent's child items)..
func parseItemListPath(path string) (collectionKey, parentKey string, topOnly, isList bool) {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case len(segs) == 1 && segs[0] == "items":
		return "", "", false, true
	case len(segs) == 2 && segs[0] == "items" && segs[1] == "top":
		return "", "", true, true
	case len(segs) == 3 && segs[0] == "items" && segs[2] == "children":
		return "", segs[1], false, true
	case len(segs) >= 3 && segs[0] == "collections" && segs[2] == "items":
		return segs[1], "", len(segs) >= 4 && segs[3] == "top", true
	}
	return "", "", false, false
}

// Ensure time import is used (compilation guard).
var _ = time.Now
