// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"zotio/internal/client"
	"zotio/internal/store"
)

// isNetworkError returns true for errors caused by network connectivity issues
// (DNS, connection refused, timeout). HTTP 4xx/5xx errors are NOT network errors.
//
// Timeouts (context deadlines, http.Client timeouts) count as network errors:
// a slow API should serve the mirror, not fail the read. User cancellation
// never counts — an interrupted read must stay interrupted instead of
// printing local rows the user did not ask for.
func isNetworkError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ENETUNREACH) ||
		errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ETIMEDOUT) {
		return true
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
	// Timeout-indicating errors that carry no net.OpError (http.Client
	// timeouts, TLS handshake timeouts, context deadlines surviving the
	// url.Error unwrap above).
	var timeoutErr interface{ Timeout() bool }
	if As(err, &timeoutErr) && timeoutErr.Timeout() {
		return true
	}
	// APIError contains only an HTTP status and response body; it cannot wrap
	// a transport error. Never inspect its server-controlled text for markers.
	return false
}

// openStoreForRead opens the local SQLite store for reading.
// Returns nil, nil if the database file does not exist (no sync has been run).
func openStoreForRead(ctx context.Context, cliName string) (*store.Store, error) {
	dbPath, err := defaultDBPath(cliName)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}
	return store.OpenReadOnlyContext(ctx, dbPath)
}

// openStoreForWrite opens the local SQLite store for a path that intentionally
// updates the local mirror or its sidecar evidence.
func openStoreForWrite(ctx context.Context, cliName string) (*store.Store, error) {
	dbPath, err := defaultDBPath(cliName)
	if err != nil {
		return nil, err
	}
	return store.OpenWithContext(ctx, dbPath)
}

// openExistingStoreForWrite opens an already-synced local store for an
// intentional write without creating a mirror for libraries that have not been
// synced yet.
func openExistingStoreForWrite(ctx context.Context, cliName string) (*store.Store, error) {
	dbPath, err := defaultDBPath(cliName)
	if err != nil {
		return nil, err
	}
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
		data, err := c.GetWithHeadersContext(ctx, path, params, headers)
		if err != nil {
			return nil, DataProvenance{}, err
		}
		return data, attachFreshness(DataProvenance{Source: "live"}, flags), nil

	default: // "auto"
		data, err := c.GetWithHeadersContext(ctx, path, params, headers)
		if err == nil {
			writeThroughCacheForPath(ctx, resourceType, path, data)
			return data, attachFreshness(DataProvenance{Source: "live"}, flags), nil
		}
		if !isNetworkError(err) {
			// HTTP 4xx/5xx errors propagate — not a fallback case
			return nil, DataProvenance{}, err
		}
		// Network error — try local fallback. The live attempt may have
		// consumed the caller's deadline, so the mirror read runs detached
		// from it: a timeout that still failed the fallback would make the
		// documented offline resilience unreachable. Cancellation never
		// reaches this branch (it is not a network error), so detaching only
		// drops an already-expired timeout, never a user interrupt.
		fallbackData, fallbackProv, fallbackErr := resolveLocal(context.WithoutCancel(ctx), resourceType, isList, path, params, "api_unreachable")
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
	writeThroughCacheForPath(ctx, resourceType, "", data)
}

// writeThroughCacheForPath is writeThroughCache with the endpoint path, so the
// stored resource is derived from the endpoint instead of the caller's label.
// Collection child endpoints (/collections/{key}/items, /collections/{key}/tags)
// return item and tag rows while their commands label the read "collections";
// upserting those rows under "collections" poisoned the mirror until the next
// full collections sync, so the path decides the stored resource here.
func writeThroughCacheForPath(ctx context.Context, resourceType, path string, data json.RawMessage) {
	// schema/type lists (itemTypes, itemFields, …) are read-only reference
	// data, not library content — skip the cache. Tags flow through: the
	// store's ResourceIDFieldOverrides keys them by tag name.
	if resourceType == "schema" {
		return
	}
	target := cacheTargetResource(resourceType, path)
	dbPath, err := defaultDBPath("zotio")
	if err != nil {
		return
	}
	db, err := store.OpenWithContext(ctx, dbPath)
	if err != nil {
		return
	}
	defer db.Close()

	// Collect candidate rows to upsert from various response shapes
	var candidates []json.RawMessage

	// Try direct array first
	if json.Unmarshal(data, &candidates) != nil || len(candidates) == 0 {
		candidates = nil
		// Try object — check for common envelope patterns (results, data, items)
		var envelope map[string]json.RawMessage
		if json.Unmarshal(data, &envelope) == nil {
			for _, key := range []string{"results", "data", "items"} {
				if raw, ok := envelope[key]; ok {
					var arr []json.RawMessage
					if json.Unmarshal(raw, &arr) == nil && len(arr) > 0 {
						candidates = arr
						break
					}
				}
			}
			// Single-object Zotero detail responses are keyed by "key";
			// tag rows by "tag"; some non-Zotero resources still use "id".
			// Anything else (rendered formats, error text) is not rows.
			if candidates == nil {
				if _, ok := envelope["id"]; !ok {
					if _, ok := envelope["key"]; !ok {
						if _, ok := envelope["tag"]; !ok {
							return
						}
					}
				}
				candidates = []json.RawMessage{data}
			}
		}
	}

	// Only rows shaped like the target resource are stored: a scoped child
	// read must never land rows under a resource they do not belong to, and
	// rendered formats (bibtex, ris, …) carry no row identity at all.
	rows := filterWriteThroughRows(target, candidates)
	if len(rows) == 0 {
		return
	}
	if _, _, err := db.UpsertBatch(target, rows); err != nil {
		fmt.Fprintf(os.Stderr, "warning: write-through cache failed for %q: %v\n", target, err)
	}
}

// cacheTargetResource maps a live read endpoint to the mirror resource its
// response rows belong to. Item-list and tag-list child endpoints return rows
// of another resource than the collection label their commands carry, so they
// resolve to that resource here; every other endpoint keeps the caller's
// label. An empty path (the label-only wrapper) never remaps.
func cacheTargetResource(resourceType, path string) string {
	if _, isList, err := parseTagListPath(path); err == nil && isList {
		return "tags"
	}
	if _, _, _, itemList := parseItemListPath(path); itemList {
		return "items"
	}
	if segs := strings.Split(strings.Trim(path, "/"), "/"); len(segs) == 2 && segs[0] == "tags" {
		return "tags"
	}
	return resourceType
}

// writeThroughIDMarkers is the row-identity field each mirrored Zotero
// resource is keyed by. Rows missing it are not cacheable under that resource.
var writeThroughIDMarkers = map[string]string{
	"items":       "key",
	"items-trash": "key",
	"collections": "key",
	"tags":        "tag",
}

// filterWriteThroughRows drops response rows that do not carry the target
// resource's identity field. Resources without a known marker keep every
// candidate, preserving the historical behavior for non-Zotero shapes.
func filterWriteThroughRows(target string, candidates []json.RawMessage) []json.RawMessage {
	marker, known := writeThroughIDMarkers[target]
	if !known {
		return candidates
	}
	rows := make([]json.RawMessage, 0, len(candidates))
	for _, raw := range candidates {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			continue
		}
		rawID, ok := obj[marker]
		if !ok {
			continue
		}
		var id string
		if err := json.Unmarshal(rawID, &id); err != nil || strings.TrimSpace(id) == "" {
			continue
		}
		rows = append(rows, raw)
	}
	return rows
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

	// Zotero-aware local item query planner. For item-list reads
	// (/items, /items/top, /collections/{key}/items[/top]) it applies the
	// scopes it can reproduce locally (itemType, tag, collection, top-level,
	// quick-search, sort, direction, limit, start) instead of dumping all
	// synced rows. Keyed on the path so it also covers the collection-items
	// command, which labels its resource "collections".
	if data, handled, qErr := resolveLocalItemList(ctx, db, path, params); handled {
		if qErr != nil {
			return nil, DataProvenance{}, qErr
		}
		itemProv := localProvenance(db, "items", reason)
		itemProv.Scoped = true
		return data, itemProv, nil
	}

	if data, handled, qErr := resolveLocalTagList(ctx, db, path, params); handled {
		if qErr != nil {
			return nil, DataProvenance{}, qErr
		}
		tagProv := localProvenance(db, "tags", reason)
		tagProv.Scoped = true
		return data, tagProv, nil
	}

	if data, handled, qErr := resolveLocalTagGet(ctx, db, path); handled {
		if qErr != nil {
			return nil, DataProvenance{}, qErr
		}
		tagProv := localProvenance(db, "tags", reason)
		tagProv.Scoped = true
		return data, tagProv, nil
	}

	if data, handled, qErr := resolveLocalCollectionChildren(ctx, db, path, params); handled {
		if qErr != nil {
			return nil, DataProvenance{}, qErr
		}
		childProv := localProvenance(db, "collections", reason)
		childProv.Scoped = true
		return data, childProv, nil
	}
	// An item-list path can intentionally fall through when its scope cannot
	// be reproduced. Preserve its list classification for the generic dump:
	// collection-items commands otherwise look like keyed collection reads.
	if _, _, _, itemList := parseItemListPath(path); itemList {
		resourceType = "items"
		isList = true
		prov = localProvenance(db, resourceType, reason)
	}

	if data, handled, qErr := resolveLocalTrashList(ctx, db, resourceType, isList, path, params); handled {
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
		if _, _, err := parseLocalPagination(params); err != nil {
			return nil, DataProvenance{}, err
		}
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
			_, lastSynced, _, err := db.GetSyncState(resourceType)
			if err != nil {
				return nil, DataProvenance{}, fmt.Errorf("querying local %q sync state: %w", resourceType, err)
			}
			if lastSynced.IsZero() {
				return nil, DataProvenance{}, fmt.Errorf("no local data for %q. Run 'zotio sync' first", resourceType)
			}
			return json.RawMessage("[]"), prov, nil
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
	if errors.Is(err, store.ErrNotFound) {
		return nil, DataProvenance{}, fmt.Errorf("resource %q with ID %q not found in local store. Run 'zotio sync' first", resourceType, id)
	}
	if err != nil {
		return nil, DataProvenance{}, fmt.Errorf("querying local store: %w", err)
	}
	return item, prov, nil
}

// reproducibleLocalParams are request params a generic local list read can
// honor. Only pagination is reproducible by the generic dump; every other
// non-empty request param must trigger its unfiltered-data warning.
var reproducibleLocalParams = map[string]bool{"limit": true, "start": true}

// hasUnreproducibleParams reports whether params contains any request parameter
// a generic local read cannot reproduce (anything outside reproducibleLocalParams).
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

// parseLocalPagination validates the shared Zotero list pagination domain.
// Zero preserves the CLI's unbounded/no-offset semantics.
func parseLocalPagination(params map[string]string) (limit, start int, err error) {
	for _, param := range []struct {
		name string
		dest *int
	}{
		{name: "limit", dest: &limit},
		{name: "start", dest: &start},
	} {
		v := params[param.name]
		if v == "" {
			continue
		}
		n, parseErr := strconv.Atoi(v)
		if parseErr != nil {
			return 0, 0, fmt.Errorf("invalid %s %q: must be an integer", param.name, v)
		}
		if n < 0 {
			return 0, 0, fmt.Errorf("invalid %s %q: must be non-negative", param.name, v)
		}
		*param.dest = n
	}
	return limit, start, nil
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

// resolveLocalTagList reproduces /tags and /collections/{key}/tags through the
// dedicated tag read model. Unknown filters fail instead of falling through to
// a collection read whose last path segment would be misread as an item key.
func resolveLocalTagList(ctx context.Context, db *store.Store, path string, params map[string]string) (json.RawMessage, bool, error) {
	collectionKey, isList, err := parseTagListPath(path)
	if err != nil {
		return nil, true, err
	}
	if !isList {
		return nil, false, nil
	}
	if collectionKey != "" {
		if err := requireLocalResourceHydrated(db, "items"); err != nil {
			return nil, true, fmt.Errorf("collection tag scope: %w", err)
		}
	}
	supported := map[string]bool{"q": true, "qmode": true, "limit": true, "start": true}
	for key, value := range params {
		if value != "" && !supported[key] {
			return nil, true, fmt.Errorf("unsupported local tag parameter %q", key)
		}
	}

	limit, start, err := parseLocalPagination(params)
	if err != nil {
		return nil, true, err
	}
	rows, err := db.QueryTagsContext(ctx, store.TagQuery{
		Query:      params["q"],
		QueryMode:  params["qmode"],
		Collection: collectionKey,
		Limit:      limit,
		Start:      start,
	})
	if err != nil {
		return nil, true, fmt.Errorf("local tag query: %w", err)
	}
	if len(rows) == 0 {
		if err := requireLocalResourceHydrated(db, "tags"); err != nil {
			return nil, true, err
		}
		return json.RawMessage("[]"), true, nil
	}
	data, err := json.Marshal(rows)
	if err != nil {
		return nil, true, fmt.Errorf("marshaling local tags: %w", err)
	}
	return data, true, nil
}

func parseTagListPath(path string) (collectionKey string, isList bool, err error) {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case len(segs) == 1 && segs[0] == "tags":
		return "", true, nil
	case len(segs) == 3 && segs[0] == "collections" && segs[2] == "tags":
		key, unescapeErr := url.PathUnescape(segs[1])
		if unescapeErr != nil {
			return "", true, fmt.Errorf("unescaping local collection key %q: %w", segs[1], unescapeErr)
		}
		if key == "" {
			return "", true, fmt.Errorf("local collection key cannot be empty")
		}
		return key, true, nil
	default:
		return "", false, nil
	}
}

// resolveLocalTagGet reproduces /tags/{name}, whose live response is a list
// because one name can exist as both a manual and an automatic tag.
func resolveLocalTagGet(ctx context.Context, db *store.Store, path string) (json.RawMessage, bool, error) {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	if len(segs) != 2 || segs[0] != "tags" {
		return nil, false, nil
	}
	name, err := url.PathUnescape(segs[1])
	if err != nil {
		return nil, true, fmt.Errorf("unescaping local tag name %q: %w", segs[1], err)
	}
	if name == "" {
		return nil, true, fmt.Errorf("local tag name cannot be empty")
	}
	rows, err := db.QueryTagsContext(ctx, store.TagQuery{Name: name})
	if err != nil {
		return nil, true, fmt.Errorf("local tag query: %w", err)
	}
	if len(rows) == 0 {
		if err := requireLocalResourceHydrated(db, "tags"); err != nil {
			return nil, true, err
		}
		return json.RawMessage("[]"), true, nil
	}
	data, err := json.Marshal(rows)
	if err != nil {
		return nil, true, fmt.Errorf("marshaling local tags: %w", err)
	}
	return data, true, nil
}

// resolveLocalCollectionChildren reproduces /collections/{key}/collections, the
// subcollection list a recursive collection walk depends on. Without it the
// generic fallback reads the trailing "collections" segment as a collection key
// and reports the resource missing, so an offline `collections export` could
// never descend past the root collection.
//
// Child rows are ordered by name then key, matching mcp_graph.go's collection
// tree so both surfaces present one subcollection order.
func resolveLocalCollectionChildren(ctx context.Context, db *store.Store, path string, params map[string]string) (json.RawMessage, bool, error) {
	parentKey, isList, err := parseCollectionChildrenPath(path)
	if err != nil {
		return nil, true, err
	}
	if !isList {
		return nil, false, nil
	}
	for key, value := range params {
		if value != "" && !reproducibleLocalParams[key] {
			return nil, true, fmt.Errorf("unsupported local subcollection parameter %q", key)
		}
	}
	if _, _, err := parseLocalPagination(params); err != nil {
		return nil, true, err
	}
	qs := localQueryStore{db}
	rows, err := qs.QueryRawContext(ctx, `
SELECT data, json_extract(data,'$.data.name') AS name
FROM resources
WHERE resource_type='collections' AND json_extract(data,'$.data.parentCollection')=?
ORDER BY name, id`, parentKey)
	if err != nil {
		return nil, true, fmt.Errorf("local subcollection query: %w", err)
	}
	children := make([]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		if raw := sqlStringValue(row["data"]); raw != "" {
			children = append(children, json.RawMessage(raw))
		}
	}
	if len(children) == 0 {
		// A collection with no children is a valid answer, but an unsynced
		// mirror is not: both look like zero rows, so the sync checkpoint
		// decides which one this is.
		if err := requireLocalResourceHydrated(db, "collections"); err != nil {
			return nil, true, err
		}
		return json.RawMessage("[]"), true, nil
	}
	children = paginateLocalRows(children, params)
	if len(children) == 0 {
		return json.RawMessage("[]"), true, nil
	}
	data, err := json.Marshal(children)
	if err != nil {
		return nil, true, fmt.Errorf("marshaling local subcollections: %w", err)
	}
	return data, true, nil
}

func parseCollectionChildrenPath(path string) (parentKey string, isList bool, err error) {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	if len(segs) != 3 || segs[0] != "collections" || segs[2] != "collections" {
		return "", false, nil
	}
	key, unescapeErr := url.PathUnescape(segs[1])
	if unescapeErr != nil {
		return "", true, fmt.Errorf("unescaping local collection key %q: %w", segs[1], unescapeErr)
	}
	if key == "" {
		return "", true, fmt.Errorf("local collection key cannot be empty")
	}
	return key, true, nil
}

func requireLocalResourceHydrated(db *store.Store, resourceType string) error {
	totalRows, err := db.Count(resourceType)
	if err != nil {
		return fmt.Errorf("counting local %s: %w", resourceType, err)
	}
	if totalRows > 0 {
		return nil
	}
	_, lastSynced, _, err := db.GetSyncState(resourceType)
	if err != nil {
		return fmt.Errorf("querying local %s sync state: %w", resourceType, err)
	}
	if lastSynced.IsZero() {
		return fmt.Errorf("no local data for %q. Run 'zotio sync --resources %s' first", resourceType, resourceType)
	}
	return nil
}

// resolveLocalTrashList reproduces the live /items/trash list's default order
// and SQL pagination. A completed empty sync is valid local data; the absence
// of both rows and a sync timestamp remains a loud hydration error.
func resolveLocalTrashList(ctx context.Context, db *store.Store, resourceType string, isList bool, path string, params map[string]string) (json.RawMessage, bool, error) {
	if resourceType != "items-trash" || !isLocalListRead(resourceType, isList, path) {
		return nil, false, nil
	}

	limit, start, err := parseLocalPagination(params)
	if err != nil {
		return nil, true, err
	}
	rows, err := db.QueryTrashContext(ctx, store.TrashQuery{Limit: limit, Start: start})
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
// is an item-list endpoint and its requested scope is locally reproducible.
// It returns (nil, false, nil) for non-item-list paths or unreproducible
// scopes so the caller falls back to its honest generic get/list handling.
// An empty match yields a JSON empty array, which mirrors a live list that
// matched nothing.
func resolveLocalItemList(ctx context.Context, db *store.Store, path string, params map[string]string) (json.RawMessage, bool, error) {
	collectionKey, parentKey, topOnly, isList := parseItemListPath(path)
	if !isList {
		return nil, false, nil
	}

	// The local item planner cannot reproduce incremental version scopes or
	// rendered bibliography output. Fall through so the generic path warns
	// rather than presenting an unscoped JSON dump as a scoped result.
	if params["since"] != "" || params["format"] != "" {
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
	limit, start, err := parseLocalPagination(params)
	if err != nil {
		return nil, true, err
	}
	q.Limit = limit
	q.Start = start
	items, err := db.QueryItemsContext(ctx, q)
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
