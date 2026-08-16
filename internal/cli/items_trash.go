// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"time"

	"zotio/internal/client"

	"github.com/spf13/cobra"
)

func newItemsTrashCmd(flags *rootFlags) *cobra.Command {
	var flagLimit, flagStart int

	cmd := &cobra.Command{
		Use:         "trash",
		Short:       "List items in the trash",
		Example:     "  zotio items trash",
		Annotations: map[string]string{"zotio:endpoint": "items.trash", "zotio:method": "GET", "zotio:path": "/items/trash", "mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := flags.newClient()
			if err != nil {
				return err
			}

			path := "/items/trash"
			var data json.RawMessage
			var prov DataProvenance
			if flags.dataSource == "local" {
				params := map[string]string{}
				if flagLimit != 0 {
					params["limit"] = fmt.Sprintf("%v", flagLimit)
				}
				if flagStart != 0 {
					params["start"] = fmt.Sprintf("%v", flagStart)
				}
				var readErr error
				data, prov, readErr = resolveRead(cmd.Context(), c, flags, "items-trash", true, path, params, nil)
				if readErr != nil {
					return classifyAPIError(readErr, flags)
				}
				// Local reads already paginate via QueryTrash; this only honors a
				// limit the API accepted but ignored (idempotent when honored).
				data = truncateJSONArray(data, flagLimit)
			} else {
				// Non-local: Zotero Web API defaults `limit` to 25 and caps at 100
				// (basics#sorting_and_pagination). An omitted limit is NOT unbounded.
				// Page explicitly with limit=100 and incrementing start before the
				// union. GetWithHeadersContext discards Link headers, so we terminate
				// on a short page and guard with a bounded iteration count plus a
				// no-progress check.
				ctx := cmd.Context()
				var fetchErr error
				data, prov, fetchErr = fetchAllLiveTrash(ctx, c, flags, path)
				if fetchErr != nil {
					return fetchErr
				}
				data, prov = unionMirroredTrash(ctx, cmd, flags, data, prov, flagStart, flagLimit)
			}
			// Print provenance to stderr for human-facing output
			printProvenance(cmd, countResultItems(data), prov)
			// For JSON output, wrap with provenance envelope before passing through flags.
			// --select wins over --compact when both are set; --compact only runs when
			// no explicit fields were requested.
			if wantsJSONEnvelope(cmd.OutOrStdout(), flags) {
				filtered := data
				if flags.selectFields != "" {
					filtered = filterFields(filtered, flags.selectFields)
				} else if flags.compact {
					filtered = compactFields(filtered)
				}
				wrapped, wrapErr := wrapWithProvenance(filtered, prov)
				if wrapErr != nil {
					return wrapErr
				}
				return printOutput(cmd.OutOrStdout(), wrapped, true)
			}
			// For all other output modes (table, csv, plain, quiet), use the standard pipeline
			if wantsHumanTable(cmd.OutOrStdout(), flags) {
				var items []map[string]any
				if json.Unmarshal(data, &items) == nil && len(items) > 0 {
					if err := printAutoTable(cmd.OutOrStdout(), items); err != nil {
						return err
					}
					if len(items) >= 25 {
						fmt.Fprintf(os.Stderr, "\nShowing %d results. To narrow: add --limit, --json --select, or filter flags.\n", len(items))
					}
					return nil
				}
			}
			return printOutputWithFlags(cmd.OutOrStdout(), data, flags)
		},
	}
	cmd.Flags().IntVar(&flagLimit, "limit", 0, "Maximum number of items to return")
	cmd.Flags().IntVar(&flagStart, "start", 0, "Pagination offset (zero-based)")

	return cmd
}

// fetchAllLiveTrash pages /items/trash with limit=100 until a short page.
// It deduplicates keys across pages and guards against a non-terminating server
// with both a bounded iteration count and a no-progress (all-duplicate) check.
func fetchAllLiveTrash(ctx context.Context, c *client.Client, flags *rootFlags, path string) (json.RawMessage, DataProvenance, error) {
	const maxTrashPages = 500 // 50k rows; trash never approaches this
	seen := make(map[string]bool, zoteroPageMax)
	var all []json.RawMessage
	for page := range maxTrashPages {
		start := page * zoteroPageMax
		params := map[string]string{
			"limit": strconv.Itoa(zoteroPageMax),
			"start": strconv.Itoa(start),
		}
		pageData, err := c.GetWithHeadersContext(ctx, path, params, nil)
		if err != nil {
			if page == 0 && flags.dataSource == "auto" && isNetworkError(err) {
				fbData, fbProv, fbErr := resolveLocal(ctx, "items-trash", true, path, map[string]string{}, "api_unreachable")
				if fbErr != nil {
					return nil, DataProvenance{}, classifyAPIError(err, flags)
				}
				return fbData, attachFreshness(fbProv, flags), nil
			}
			return nil, DataProvenance{}, classifyAPIError(err, flags)
		}
		var pageItems []json.RawMessage
		if err := json.Unmarshal(pageData, &pageItems); err != nil {
			if page == 0 {
				return pageData, attachFreshness(DataProvenance{Source: "live"}, flags), nil
			}
			break
		}
		if len(pageItems) == 0 {
			if page == 0 {
				return pageData, attachFreshness(DataProvenance{Source: "live"}, flags), nil
			}
			break
		}
		added := 0
		for _, entry := range pageItems {
			key := jsonStringField(entry, "key")
			if key != "" && seen[key] {
				continue
			}
			if key != "" {
				seen[key] = true
			}
			all = append(all, entry)
			added++
		}
		if added == 0 {
			break
		}
		if len(pageItems) < zoteroPageMax {
			break
		}
	}
	if all == nil {
		all = []json.RawMessage{}
	}
	marshaled, err := json.Marshal(all)
	if err != nil {
		return nil, DataProvenance{}, err
	}
	prov := attachFreshness(DataProvenance{Source: "live"}, flags)
	writeThroughCache(ctx, "items-trash", marshaled)
	return marshaled, prov, nil
}

// unionMirroredTrash appends items the local mirror lists as trashed but the read
// plane has not reported yet, de-duplicated by key.
//
// Neither source alone is right: the read plane (the Zotero desktop local API)
// only learns about a trash once Zotero syncs it down from zotero.org, and the
// mirror only knows what the last sync or a write-through recorded. The union is
// the honest answer, and it self-heals as the read plane catches up.
//
// Best-effort: an unreadable mirror leaves the live result untouched. The merged
// view is sorted by dateModified descending (Zotero trash order) with key as a
// stable tie-breaker; rows with missing or unparseable dateModified sort last
// deterministically. Pagination (start/limit) is applied AFTER the merge and sort
// so a short live terminal page is not padded with out-of-page mirror rows.
func unionMirroredTrash(ctx context.Context, cmd *cobra.Command, flags *rootFlags, live json.RawMessage, prov DataProvenance, start, limit int) (json.RawMessage, DataProvenance) {
	var liveItems []json.RawMessage
	if err := json.Unmarshal(live, &liveItems); err != nil {
		// Not a list (an error envelope, say); nothing to union.
		return live, prov
	}
	seen := make(map[string]bool, len(liveItems))
	for _, entry := range liveItems {
		if key := jsonStringField(entry, "key"); key != "" {
			seen[key] = true
		}
	}

	mirrored, _, err := resolveLocal(ctx, "items-trash", true, "/items/trash", map[string]string{}, "trash_reconciliation")
	if err != nil {
		// No mirror — still sort and paginate the live set so ordering and
		// missing-date handling are consistent even without a union.
		liveItems = sortTrashItems(liveItems)
		liveItems = paginateTrashItems(liveItems, start, limit)
		if data, err := json.Marshal(liveItems); err == nil {
			return data, prov
		}
		return live, prov
	}
	var mirroredItems []json.RawMessage
	if err := json.Unmarshal(mirrored, &mirroredItems); err != nil {
		liveItems = sortTrashItems(liveItems)
		liveItems = paginateTrashItems(liveItems, start, limit)
		if data, err := json.Marshal(liveItems); err == nil {
			return data, prov
		}
		return live, prov
	}

	added := 0
	for _, entry := range mirroredItems {
		key := jsonStringField(entry, "key")
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		liveItems = append(liveItems, entry)
		added++
	}
	// Deterministic Zotero order: dateModified descending, missing/invalid last,
	// key ascending as stable tie-breaker.
	liveItems = sortTrashItems(liveItems)
	liveItems = paginateTrashItems(liveItems, start, limit)
	if added == 0 {
		// No new rows — but the live set has been reconciled (sorted and
		// paginated) so callers still see the correct ordered page.
		merged, err := json.Marshal(liveItems)
		if err != nil {
			return live, prov
		}
		return merged, prov
	}
	merged, err := json.Marshal(liveItems)
	if err != nil {
		return live, prov
	}
	fmt.Fprintf(cmd.ErrOrStderr(),
		"note: %d trashed item(s) came from the local mirror; the Zotero read API has not caught up with them yet\n", added)
	prov.Source = "live+local"
	return merged, prov
}

// trashDateModified extracts dateModified (nested under data or flat) and parses
// it. Unparseable or missing values sort last deterministically and never panic.
func trashDateModified(raw json.RawMessage) (time.Time, bool) {
	s := jsonStringField(raw, "dateModified")
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	// Zotero sometimes emits date-only or space-separated variants; try the
	// common ones without failing the sort.
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, true
	}
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

func sortTrashItems(items []json.RawMessage) []json.RawMessage {
	sort.SliceStable(items, func(i, j int) bool {
		ti, okI := trashDateModified(items[i])
		tj, okJ := trashDateModified(items[j])
		if okI && okJ {
			if !ti.Equal(tj) {
				return ti.After(tj)
			}
		} else if okI && !okJ {
			return true
		} else if !okI && okJ {
			return false
		}
		ki := jsonStringField(items[i], "key")
		kj := jsonStringField(items[j], "key")
		return ki < kj
	})
	return items
}

func paginateTrashItems(items []json.RawMessage, start, limit int) []json.RawMessage {
	if start < 0 {
		start = 0
	}
	if limit < 0 {
		limit = 0
	}
	if start >= len(items) {
		return []json.RawMessage{}
	}
	if start > 0 {
		items = items[start:]
	}
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	return items
}
