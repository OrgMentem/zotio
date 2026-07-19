// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"zotio/internal/client"
	"zotio/internal/cliutil"
	"zotio/internal/store"

	"github.com/spf13/cobra"
)

// syncResult holds the outcome of syncing a single resource.
type syncResult struct {
	Resource string
	Count    int
	Err      error
	Warn     error
	Duration time.Duration
}

type syncEventWriterContextKey struct{}

func emitSyncEvent(ctx context.Context, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	writer, _ := ctx.Value(syncEventWriterContextKey{}).(io.Writer)
	if writer == nil {
		writer = io.Discard
	}
	_, _ = writer.Write(append(b, '\n'))
}

// syncEventWriter serializes the JSONL sync events emitted by the concurrent
// worker pool onto a single underlying writer (cmd stdout), which is not itself
// safe for concurrent Write.
type syncEventWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncEventWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func processDequeuedSyncResource(ctx context.Context, resource string, results chan<- syncResult, syncOne func(string) syncResult) bool {
	if ctx.Err() != nil {
		return false
	}
	results <- syncOne(resource)
	return true
}

func runSyncWorker(ctx context.Context, work <-chan string, results chan<- syncResult, syncOne func(string) syncResult) {
	for {
		select {
		case <-ctx.Done():
			return
		case resource, ok := <-work:
			if !ok {
				return
			}
			// Stop workers between resources when the sync context is canceled.
			if !processDequeuedSyncResource(ctx, resource, results, syncOne) {
				return
			}
		}
	}
}

func newSyncCmd(flags *rootFlags) *cobra.Command {
	var resources []string
	var full bool
	var sinceVersion int
	var concurrency int
	var dbPath string
	var maxPages int
	var latestOnly bool
	var strict bool
	var fulltext bool

	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Sync API data to local SQLite for offline search and analysis",
		// Sync populates the store; it must never be gated by a synced-store preflight.
		Annotations: map[string]string{"zotio:preflight": "skip"},
		Long: `Sync data from the API into a local SQLite database. Supports resumable
incremental sync (only fetches new data since last sync) and full resync.
Once synced, use the 'search' command for instant full-text search.

Exit codes & warnings:
  Resources the API denies access to (HTTP 403, or HTTP 400 with an
  access-policy body) are reported as warnings rather than failing the
  run. In --json mode each is emitted as a {"event":"sync_warning",...}
  line carrying status, reason, and message fields, and a final
  {"event":"sync_summary",...} aggregates the run.

  Exit 0 when at least one resource synced and no resource flagged in
  the spec as critical (x-critical: true) failed. Pass --strict to exit
  non-zero on any per-resource failure. Exit is always
  non-zero when every selected resource failed, regardless of --strict.`,
		Example: `  # Sync all resources
  zotio sync

  # Sync specific resources only
  zotio sync --resources channels,messages

  # Full resync (ignore previous checkpoint)
  zotio sync --full

  # Incremental sync: only objects modified since a Zotero library version
  zotio sync --since 4521

  # Parallel sync with 8 workers
  zotio sync --concurrency 8

  # Latest-only: refresh head of each resource, no historical backfill
  zotio sync --latest-only`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := flags.newClient()
			if err != nil {
				return err
			}
			c.NoCache = true

			if dbPath == "" {
				dbPath, err = defaultDBPath("zotio")
				if err != nil {
					return err
				}
			}

			db, err := store.OpenWithContext(cmd.Context(), dbPath)
			if err != nil {
				return fmt.Errorf("opening local database: %w", err)
			}
			defer db.Close()

			// If no specific resources, sync top-level resources
			if len(resources) == 0 {
				resources = defaultSyncResources()
			}
			// Keep live and trashed item state coupled before any cursor mutation
			// or worker scheduling so an items sync can provide local parity.
			resources = normalizeSyncResources(resources)

			// --full: clear all sync cursors before starting
			if full {
				for _, resource := range resources {
					if err := db.SaveSyncState(resource, "", 0); err != nil {
						return fmt.Errorf("clearing sync cursor for %s (--full): %w", resource, err)
					}
				}
			}

			// --latest-only narrows to the first page of each resource
			// ignoring the historical resume cursor. We cap maxPages at 1
			// here rather than re-interpreting it downstream so the rest
			// of the sync loop stays oblivious. Mutually-useful with
			// --since: if the user set --since, that threshold still wins
			// and we don't short-circuit historical context they asked for.
			if latestOnly {
				if sinceVersion == 0 {
					maxPages = 1
					// Clear the cursor so we start from the head each time;
					// the goal of --latest-only is "refresh the top" not
					// "resume from wherever I left off".
					for _, resource := range resources {
						existing, _, _, gerr := db.GetSyncState(resource)
						if gerr != nil {
							return fmt.Errorf("reading sync cursor for %s (--latest-only): %w", resource, gerr)
						}
						if existing != "" {
							if err := db.SaveSyncState(resource, "", 0); err != nil {
								return fmt.Errorf("clearing sync cursor for %s (--latest-only): %w", resource, err)
							}
						}
					}
				} else if humanFriendly {
					fmt.Fprintln(os.Stderr, "warning: --latest-only ignored because --since is set; --since takes precedence")
				}
			}

			// Worker pool: produce resources, N workers consume
			if concurrency < 1 {
				concurrency = 4
			}

			ctx := context.WithValue(cmd.Context(), syncEventWriterContextKey{}, &syncEventWriter{w: cmd.OutOrStdout()})
			started := time.Now()
			work := make(chan string, len(resources))
			results := make(chan syncResult, len(resources))

			var wg sync.WaitGroup
			for i := 0; i < concurrency; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					runSyncWorker(ctx, work, results, func(resource string) syncResult {
						return syncResource(ctx, c, db, resource, sinceVersion, full, maxPages, concurrency == 1)
					})
				}()
			}

			// Enqueue all resources, stopping promptly if the command is canceled.
		enqueue:
			for _, resource := range resources {
				select {
				case <-ctx.Done():
					break enqueue
				case work <- resource:
				}
			}
			close(work)

			// Collect results in a separate goroutine
			go func() {
				wg.Wait()
				close(results)
			}()

			var totalSynced int
			var errCount int
			var criticalErrCount int
			var warnCount int
			// keep structured per-resource
			// failures in memory too, because MCP captures cmd output but legacy
			// sync warnings/errors were written to process stdout/stderr.
			var failedResources []string
			var criticalFailedResources []string
			var warnedResources []string
			var successCount int
			for res := range results {
				if res.Err != nil {
					detail := fmt.Sprintf("%s: %v", res.Resource, res.Err)
					failedResources = append(failedResources, detail)
					if humanFriendly {
						fmt.Fprintf(os.Stderr, "  %s: error: %v\n", res.Resource, res.Err)
					}
					errCount++
					if criticalResources[res.Resource] {
						criticalErrCount++
						criticalFailedResources = append(criticalFailedResources, detail)
					}
				} else if res.Warn != nil {
					detail := fmt.Sprintf("%s: %v", res.Resource, res.Warn)
					warnedResources = append(warnedResources, detail)
					if humanFriendly {
						fmt.Fprintf(os.Stderr, "  %s: warning: %v\n", res.Resource, res.Warn)
					}
					warnCount++
				} else {
					if humanFriendly {
						fmt.Fprintf(os.Stderr, "  %s: %d synced (done)\n", res.Resource, res.Count)
					}
					totalSynced += res.Count
					successCount++
				}
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}

			// The full-text pass runs after the core resource sync. It keeps a
			// separate checkpoint, so an incomplete fulltext pass must retain
			// that checkpoint for retry without invalidating the core data.
			var fulltextErr error
			if fulltext {
				fulltextErr = syncFulltext(cmd.Context(), c, db, full)
			}

			elapsed := time.Since(started)
			totalResources := successCount + warnCount + errCount
			if humanFriendly {
				if fulltextErr != nil {
					fmt.Fprintf(os.Stderr, "Sync incomplete: fulltext: %v\n", fulltextErr)
				} else if warnCount > 0 {
					fmt.Fprintf(os.Stderr, "Sync complete: %d records across %d resources (%d warned, %.1fs)\n",
						totalSynced, totalResources, warnCount, elapsed.Seconds())
				} else {
					fmt.Fprintf(os.Stderr, "Sync complete: %d records across %d resources (%.1fs)\n",
						totalSynced, totalResources, elapsed.Seconds())
				}
			} else {
				emitSyncEvent(ctx, struct {
					Event        string `json:"event"`
					TotalRecords int    `json:"total_records"`
					Resources    int    `json:"resources"`
					Success      int    `json:"success"`
					Warned       int    `json:"warned"`
					Errored      int    `json:"errored"`
					FulltextOK   bool   `json:"fulltext_ok"`
					DurationMS   int64  `json:"duration_ms"`
				}{
					Event:        "sync_summary",
					TotalRecords: totalSynced,
					Resources:    totalResources,
					Success:      successCount,
					Warned:       warnCount,
					Errored:      errCount,
					FulltextOK:   fulltextErr == nil,
					DurationMS:   elapsed.Milliseconds(),
				})
			}

			// Exit-code policy:
			//   1. --strict + any error  -> non-zero
			//   2. any critical failure  -> non-zero regardless of --strict
			//   3. nothing synced        -> non-zero (preserves "all-warned" / "all-errored" exit)
			//   4. otherwise             -> exit 0 (any data synced + no critical failed)
			if fulltextErr != nil {
				return degradedErr(fmt.Errorf("fulltext sync incomplete: %w", fulltextErr))
			}
			if strict && errCount > 0 {
				return fmt.Errorf("%d resource(s) failed to sync: %s", errCount, strings.Join(failedResources, "; "))
			}
			if criticalErrCount > 0 {
				return fmt.Errorf("%d critical resource(s) failed to sync: %s", criticalErrCount, strings.Join(criticalFailedResources, "; "))
			}
			if successCount == 0 {
				if warnCount > 0 && errCount == 0 {
					return fmt.Errorf("%d resource(s) skipped due to insufficient access: %s", warnCount, strings.Join(warnedResources, "; "))
				}
				if errCount > 0 {
					return fmt.Errorf("%d resource(s) failed to sync: %s", errCount, strings.Join(failedResources, "; "))
				}
			}
			return nil
		},
	}

	cmd.Flags().StringSliceVar(&resources, "resources", nil, "Comma-separated resource types to sync (selecting items also syncs items-trash)")
	cmd.Flags().BoolVar(&full, "full", false, "Full resync (ignore previous checkpoint)")
	cmd.Flags().IntVar(&sinceVersion, "since", 0, "Only sync objects modified since this Zotero library version (0 = use stored checkpoint). Get versions from a prior sync or 'items list --since'.")
	cmd.Flags().IntVar(&concurrency, "concurrency", 4, "Number of parallel sync workers")
	cmd.Flags().StringVar(&dbPath, "db", "", "Database path (default: ~/.local/share/zotio/data.db)")
	cmd.Flags().IntVar(&maxPages, "max-pages", 100, "Maximum pages to fetch per resource (0 = unlimited; cap-hit emits a sync_warning event)")
	cmd.Flags().BoolVar(&latestOnly, "latest-only", false, "Refresh head of each resource only; clears resume cursor and caps pages at 1. Mutually exclusive with --since (--since wins).")
	cmd.Flags().BoolVar(&strict, "strict", false, "Exit non-zero on any per-resource failure (default: only critical failures or all-resource failure exit non-zero).")
	cmd.Flags().BoolVar(&fulltext, "fulltext", false, "Also sync PDF full-text content (slower; one request per attachment)")

	return cmd
}

// syncFulltext fetches changed PDF full-text content and stores it as
// fulltext-typed rows so 'items fulltext' and 'search' can read it offline.
// It advances its checkpoint only after every changed attachment has been
// fetched and durably persisted, so a failed pass is retried on the next run.
func syncFulltext(ctx context.Context, c *client.Client, db *store.Store, full bool) error {
	cursor := 0
	if !full {
		v, gerr := db.GetLibraryVersion("fulltext")
		if gerr != nil {
			return fmt.Errorf("reading fulltext checkpoint: %w", gerr)
		}
		cursor = v
	}
	// The /fulltext endpoint returns 400 without `since`, so always send it,
	// including 0 on a full sync.
	params := map[string]string{"since": strconv.Itoa(cursor)}
	body, newVer, err := c.GetWithVersion("/fulltext", params)
	if err != nil {
		return fmt.Errorf("fetching fulltext index: %w", err)
	}
	var changed map[string]int
	if err := json.Unmarshal(body, &changed); err != nil {
		return fmt.Errorf("parsing fulltext index: %w", err)
	}
	if len(changed) > 0 {
		keys := make([]string, 0, len(changed))
		for k := range changed {
			keys = append(keys, k)
		}
		// the API has no batch fulltext endpoint,
		// so the per-item fetches still fan out, but persist them in a single
		// keyed transaction instead of one writeMu-serialized Upsert per item
		// (which caused lock contention and many tiny transactions).
		results, errs := cliutil.FanoutRun(ctx, keys,
			func(k string) string { return k },
			func(fctx context.Context, k string) (json.RawMessage, error) {
				// url-encode path param to prevent segment injection.
				ft, _, ferr := c.GetWithVersionContext(fctx, "/items/"+url.PathEscape(k)+"/fulltext", nil)
				if ferr != nil {
					return nil, ferr
				}
				return ft, nil
			})
		if len(results) > 0 {
			ids := make([]string, 0, len(results))
			payloads := make([]json.RawMessage, 0, len(results))
			for _, r := range results {
				ids = append(ids, r.Source)
				payloads = append(payloads, r.Value)
			}
			if uerr := db.UpsertKeyed("fulltext", ids, payloads); uerr != nil {
				return fmt.Errorf("persisting fulltext: %w", uerr)
			}
		}
		if len(errs) > 0 {
			return fmt.Errorf("%d of %d fulltext fetches failed", len(errs), len(keys))
		}
	}
	if newVer > cursor {
		if err := db.SaveLibraryVersion("fulltext", newVer); err != nil {
			return fmt.Errorf("persisting fulltext checkpoint: %w", err)
		}
	}
	return nil
}

// schema sync resources use Zotero's global schema API,
// not the library-scoped /users|groups/<id> base used by ordinary resources.
type syncHTTPClient interface {
	GetWithVersion(string, map[string]string) (json.RawMessage, int, error)
	GetWithVersionContext(context.Context, string, map[string]string) (json.RawMessage, int, error)
	RateLimit() float64
}

func syncClientForResource(c syncHTTPClient, resource string) syncHTTPClient {
	if !isSchemaSyncResource(resource) {
		return c
	}
	if concrete, ok := c.(*client.Client); ok && concrete != nil {
		return concrete.CloneForRead(stripLibraryPrefix(concrete.BaseURL))
	}
	return c
}

func isSchemaSyncResource(resource string) bool {
	switch resource {
	case "schema", "schema-item-fields", "schema-creator-fields":
		return true
	default:
		return false
	}
}

// syncResource handles the full paginated sync of a single resource.
// It resumes from the last cursor unless sinceVersion or full mode overrides it.
func syncResource(ctx context.Context, c syncHTTPClient, db *store.Store, resource string, sinceVersion int, full bool, maxPages int, inlineProgress bool) syncResult {
	started := time.Now()

	if !humanFriendly {
		emitSyncEvent(ctx, struct {
			Event    string `json:"event"`
			Resource string `json:"resource"`
		}{
			Event:    "sync_start",
			Resource: resource,
		})
	}

	path, err := syncResourcePath(resource)
	if err != nil {
		return syncResult{Resource: resource, Err: err, Duration: time.Since(started)}
	}
	// schema resources are global, so their page GETs
	// must use a client whose base URL has the library segment stripped.
	requestClient := syncClientForResource(c, resource)

	// Resume cursor from sync_state unless a full sync explicitly starts over.
	existingCursor := ""
	if !full {
		var stateErr error
		existingCursor, _, _, stateErr = db.GetSyncState(resource)
		if stateErr != nil {
			return syncResult{Resource: resource, Err: fmt.Errorf("reading sync cursor for %s: %w", resource, stateErr), Duration: time.Since(started)}
		}
	}

	// Determine the since value: an explicit --since version wins; otherwise use
	// the stored Last-Modified-Version checkpoint for incremental sync (skipped
	// on --full). Zotero's `since` is an integer library version, not a timestamp.
	sinceParam := determineSinceParam()
	effectiveSince := sinceVersion
	if effectiveSince == 0 && !full {
		v, versionErr := db.GetLibraryVersion(resource)
		if versionErr != nil {
			return syncResult{Resource: resource, Err: fmt.Errorf("reading library-version checkpoint for %s: %w", resource, versionErr), Duration: time.Since(started)}
		}
		if v > 0 {
			effectiveSince = v
		}
	}
	libraryVersion := 0

	cursor := existingCursor
	pageSize := determinePaginationDefaults()

	var progressCount int64
	pagesFetched := 0
	lastNextCursor := ""
	// extractFailureTotal accumulates per-item primary-key extraction
	// misses across pages within this resource sync. Resource-level
	// concurrency is 1 (one goroutine per resource via the work channel)
	// so this counter cannot race. We emit one primary_key_unresolved
	// sync_anomaly per resource per run when there's at least one miss
	// (rate-limited via the anomalyEmitted flag) and a roll-up
	// all_items_failed_id_extraction event when 100% of a single page
	// failed extraction.
	var extractFailureTotal int
	var consumedTotal int
	var totalCount int
	anomalyEmitted := false
	completedNaturally := false

	for {
		params := map[string]string{}

		// Set page size
		params[pageSize.limitParam] = strconv.Itoa(pageSize.limit)

		// Set cursor for resume
		if cursor != "" {
			params[pageSize.cursorParam] = cursor
		} else if pageSize.cursorParam == "start" {
			params[pageSize.cursorParam] = "0"
		}

		// Set since filter (integer Zotero library version)
		if effectiveSince > 0 {
			params[sinceParam] = strconv.Itoa(effectiveSince)
		}

		data, respVersion, err := requestClient.GetWithVersionContext(ctx, path, params)
		// Capture the library version from the first response that reports one;
		// using the earliest avoids missing objects changed mid-sync.
		if libraryVersion == 0 && respVersion > 0 {
			libraryVersion = respVersion
		}
		if err != nil {
			if w, ok := isSyncAccessWarning(err); ok {
				if !humanFriendly {
					emitSyncEvent(ctx, struct {
						Event    string `json:"event"`
						Resource string `json:"resource"`
						Status   int    `json:"status"`
						Reason   string `json:"reason"`
						Message  string `json:"message"`
					}{
						Event:    "sync_warning",
						Resource: resource,
						Status:   w.Status,
						Reason:   w.Reason,
						Message:  w.Message,
					})
				}
				return syncResult{Resource: resource, Count: totalCount, Warn: fmt.Errorf("skipped %s: %s", resource, w.Reason), Duration: time.Since(started)}
			}
			if !humanFriendly {
				emitSyncEvent(ctx, struct {
					Event    string `json:"event"`
					Resource string `json:"resource"`
					Error    string `json:"error"`
				}{
					Event:    "sync_error",
					Resource: resource,
					Error:    err.Error(),
				})
			}
			return syncResult{Resource: resource, Count: totalCount, Err: fmt.Errorf("fetching %s: %w", resource, err), Duration: time.Since(started)}
		}

		// Decode pages before deciding whether a response is a singleton record.
		items, nextCursor, hasMore, isPage, extractErr := extractPageItemsWithError(data, pageSize.cursorParam)
		if extractErr != nil {
			err := fmt.Errorf("decoding page for %s: %w", resource, extractErr)
			if !humanFriendly {
				emitSyncEvent(ctx, struct {
					Event    string `json:"event"`
					Resource string `json:"resource"`
					Error    string `json:"error"`
				}{
					Event:    "sync_error",
					Resource: resource,
					Error:    err.Error(),
				})
			}
			return syncResult{Resource: resource, Count: totalCount, Err: err, Duration: time.Since(started)}
		}
		if nextCursor == "" && pageSize.cursorParam == "start" && len(items) == pageSize.limit {
			// Zotero array endpoints paginate via start/limit and put Link headers
			// outside the JSON body, so derive the next offset when a full page arrives.
			currentStart, _ := strconv.Atoi(params[pageSize.cursorParam])
			nextCursor = strconv.Itoa(currentStart + len(items))
			hasMore = true
		}

		if isPage && len(items) == 0 {
			if !hasMore || nextCursor == "" {
				completedNaturally = true
				break
			}
			pagesFetched++
			if maxPages > 0 && pagesFetched >= maxPages {
				break
			}
			if nextCursor == lastNextCursor {
				break
			}
			lastNextCursor = nextCursor
			cursor = nextCursor
			continue
		}

		if len(items) == 0 {
			// A validated object response is a singleton resource record.
			if err := upsertSingleObject(db, resource, data); err != nil {
				if !humanFriendly {
					emitSyncEvent(ctx, struct {
						Event    string `json:"event"`
						Resource string `json:"resource"`
						Error    string `json:"error"`
					}{
						Event:    "sync_error",
						Resource: resource,
						Error:    err.Error(),
					})
				}
				return syncResult{Resource: resource, Err: err, Duration: time.Since(started)}
			}
			totalCount++
			break
		}

		// Batch upsert all items from this page. UpsertBatch returns
		// (stored, extractFailures, err): stored counts rows actually
		// landed; extractFailures counts items that survived JSON
		// unmarshal but had no extractable primary key (templated
		// IDField AND generic fallback both missed). Tracking these
		// separately lets us emit precise sync_anomaly events: a
		// roll-up "all_items_failed_id_extraction" when an entire
		// page yields zero stored, a per-resource
		// "primary_key_unresolved" the first time any single item
		// fails, and the F4b "stored_count_zero_after_extraction"
		// probe when extraction succeeded but rows still didn't land.
		stored, extractFailures, err := upsertResourceBatch(db, resource, items)
		if err != nil {
			if !humanFriendly {
				emitSyncEvent(ctx, struct {
					Event    string `json:"event"`
					Resource string `json:"resource"`
					Error    string `json:"error"`
				}{
					Event:    "sync_error",
					Resource: resource,
					Error:    err.Error(),
				})
			}
			return syncResult{Resource: resource, Count: totalCount, Err: fmt.Errorf("upserting batch for %s: %w", resource, err), Duration: time.Since(started)}
		}

		consumedTotal += len(items)
		extractFailureTotal += extractFailures

		// When a non-empty page yielded zero stored rows, the API
		// returned items in a shape we couldn't extract IDs from —
		// likely scalar IDs (Firebase /topstories.json, GitHub user-
		// repo lists) where the spec author should declare a hydration
		// pattern, or an unrecognized primary-key field name.
		if len(items) > 0 && stored == 0 {
			if humanFriendly {
				fmt.Fprintf(os.Stderr, "warning: %s returned %d items but stored 0 — the local store will be empty for this resource. Likely cause: scalar item shape rather than objects with extractable IDs.\n", resource, len(items))
			} else {
				emitSyncEvent(ctx, struct {
					Event    string `json:"event"`
					Resource string `json:"resource"`
					Consumed int    `json:"consumed"`
					Stored   int    `json:"stored"`
					Reason   string `json:"reason"`
				}{
					Event:    "sync_anomaly",
					Resource: resource,
					Consumed: len(items),
					Stored:   0,
					Reason:   "all_items_failed_id_extraction",
				})
			}
			anomalyEmitted = true
		} else if extractFailures > 0 && !anomalyEmitted {
			// Per-item primary-key resolution failure but at least one
			// item landed — emit one structured warning per resource per
			// sync run so users see the first occurrence of silent drops
			// instead of waiting for total failure.
			if humanFriendly {
				fmt.Fprintf(os.Stderr, "\nwarning: %s had %d item(s) on this page with no extractable primary key — those rows were dropped silently. Annotate the spec with x-resource-id to fix.\n", resource, extractFailures)
			} else {
				emitSyncEvent(ctx, struct {
					Event    string `json:"event"`
					Resource string `json:"resource"`
					Consumed int    `json:"consumed"`
					Stored   int    `json:"stored"`
					Count    int    `json:"count"`
					Reason   string `json:"reason"`
				}{
					Event:    "sync_anomaly",
					Resource: resource,
					Consumed: len(items),
					Stored:   stored,
					Count:    extractFailures,
					Reason:   "primary_key_unresolved",
				})
			}
			anomalyEmitted = true
		}

		totalCount += stored
		atomic.AddInt64(&progressCount, int64(stored))

		// Progress reporting (include rate limit info when active)
		currentRate := c.RateLimit()
		if humanFriendly {
			// \r in-place progress only works for a
			// single writer. With concurrency>1 the workers' interleaved \r
			// updates garble the terminal, so suppress per-page progress then and
			// rely on the per-resource "N synced (done)" summary; single-worker
			// runs keep the live in-place counter.
			if inlineProgress {
				if currentRate > 0 {
					fmt.Fprintf(os.Stderr, "\r  %s: %d synced [%.1f req/s]", resource, atomic.LoadInt64(&progressCount), currentRate)
				} else {
					fmt.Fprintf(os.Stderr, "\r  %s: %d synced", resource, atomic.LoadInt64(&progressCount))
				}
			}
		} else {
			if currentRate > 0 {
				emitSyncEvent(ctx, struct {
					Event    string  `json:"event"`
					Resource string  `json:"resource"`
					Fetched  int64   `json:"fetched"`
					RateRPS  float64 `json:"rate_rps"`
				}{
					Event:    "sync_progress",
					Resource: resource,
					Fetched:  atomic.LoadInt64(&progressCount),
					RateRPS:  currentRate,
				})
			} else {
				emitSyncEvent(ctx, struct {
					Event    string `json:"event"`
					Resource string `json:"resource"`
					Fetched  int64  `json:"fetched"`
				}{
					Event:    "sync_progress",
					Resource: resource,
					Fetched:  atomic.LoadInt64(&progressCount),
				})
			}
		}

		// Save cursor after each page for resumability
		if err := db.SaveSyncState(resource, nextCursor, totalCount); err != nil {
			// Non-fatal: log and continue
			fmt.Fprintf(os.Stderr, "\nwarning: failed to save sync state for %s: %v\n", resource, err)
		}

		pagesFetched++

		terminalPage := !hasMore || len(items) < pageSize.limit || nextCursor == ""
		if terminalPage {
			completedNaturally = true
			break
		}

		// Enforce page ceiling to prevent runaway syncs on large-catalog APIs
		if maxPages > 0 && pagesFetched >= maxPages {
			if humanFriendly {
				fmt.Fprintf(os.Stderr, "\n  %s: reached --max-pages limit (%d pages, %d items)\n", resource, maxPages, totalCount)
			} else {
				emitSyncEvent(ctx, struct {
					Event    string `json:"event"`
					Resource string `json:"resource"`
					Reason   string `json:"reason"`
					Message  string `json:"message"`
				}{
					Event:    "sync_warning",
					Resource: resource,
					Reason:   "max_pages_cap_hit",
					Message:  fmt.Sprintf("reached --max-pages cap of %d; data may be truncated. Re-run with --max-pages 0 (unlimited) or higher to verify.", maxPages),
				})
			}
			break
		}

		// Sticky-cursor detector: if the API echoes the same next cursor across
		// consecutive pages without advancing, abort to prevent burning the
		// --max-pages budget on a non-terminating loop. Checked AFTER the cap
		// guard so cap-hit takes precedence; terminal pages have already exited
		// above.
		if nextCursor != "" && nextCursor == lastNextCursor {
			if humanFriendly {
				fmt.Fprintf(os.Stderr, "\n  %s: API returned the same next cursor across two pages; aborting to prevent budget waste.\n", resource)
			} else {
				emitSyncEvent(ctx, struct {
					Event    string `json:"event"`
					Resource string `json:"resource"`
					Reason   string `json:"reason"`
					Message  string `json:"message"`
				}{
					Event:    "sync_warning",
					Resource: resource,
					Reason:   "stuck_pagination",
					Message:  fmt.Sprintf("API returned the same next cursor across two pages for resource %s; aborting to prevent budget waste.", resource),
				})
			}
			break
		}
		lastNextCursor = nextCursor

		cursor = nextCursor
	}

	// Final sync state only advances checkpoints after natural pagination
	// completion.: defensive exits
	// (--max-pages or stuck cursor) leave the resume cursor and since-version
	// checkpoint intact so a later sync cannot skip unfetched pages.
	if completedNaturally {
		if serr := db.SaveSyncState(resource, "", totalCount); serr != nil {
			return syncResult{Resource: resource, Count: totalCount, Err: fmt.Errorf("persisting sync checkpoint: %w", serr), Duration: time.Since(started)}
		}
		if libraryVersion > 0 {
			if serr := db.SaveLibraryVersion(resource, libraryVersion); serr != nil {
				return syncResult{Resource: resource, Count: totalCount, Err: fmt.Errorf("persisting library-version checkpoint: %w", serr), Duration: time.Since(started)}
			}
		}
	}

	// F4b symptom probe: if items were consumed and successfully
	// extracted (extractFailures < consumed) but nothing landed in
	// the store, something downstream of extraction silently dropped
	// rows — FTS5 trigger error, transaction rollback, character
	// encoding. Emit a sync_anomaly so the symptom is visible the
	// next time it recurs; the underlying root cause is held out for
	// controlled repro.
	if consumedTotal > 0 && totalCount == 0 && extractFailureTotal < consumedTotal {
		if humanFriendly {
			fmt.Fprintf(os.Stderr, "\nwarning: %s consumed %d items, extracted %d primary keys, but stored 0 rows — extraction succeeded yet nothing landed. Investigate FTS triggers / transaction rollback / encoding.\n", resource, consumedTotal, consumedTotal-extractFailureTotal)
		} else {
			emitSyncEvent(ctx, struct {
				Event           string `json:"event"`
				Resource        string `json:"resource"`
				Consumed        int    `json:"consumed"`
				Stored          int    `json:"stored"`
				ExtractFailures int    `json:"extract_failures"`
				Reason          string `json:"reason"`
			}{
				Event:           "sync_anomaly",
				Resource:        resource,
				Consumed:        consumedTotal,
				Stored:          0,
				ExtractFailures: extractFailureTotal,
				Reason:          "stored_count_zero_after_extraction",
			})
		}
	}

	if !humanFriendly {
		emitSyncEvent(ctx, struct {
			Event      string `json:"event"`
			Resource   string `json:"resource"`
			Total      int    `json:"total"`
			DurationMS int64  `json:"duration_ms"`
		}{
			Event:      "sync_complete",
			Resource:   resource,
			Total:      totalCount,
			DurationMS: time.Since(started).Milliseconds(),
		})
	}

	return syncResult{Resource: resource, Count: totalCount, Duration: time.Since(started)}
}

// paginationDefaults holds the resolved pagination parameter names and page size.
type paginationDefaults struct {
	cursorParam string
	limitParam  string
	limit       int
}

// determinePaginationDefaults returns the pagination parameter names to use.
// Values are detected from the API spec by the profiler at generation time.
func determinePaginationDefaults() paginationDefaults {
	return paginationDefaults{
		cursorParam: "start",
		limitParam:  "limit",
		limit:       100,
	}
}

// determineSinceParam returns the query parameter name for incremental sync filtering.
func determineSinceParam() string {
	return "since"
}

// extractPageItems preserves the legacy helper API for non-sync consumers.
// Sync callers use extractPageItemsWithError so malformed responses cannot be
// mistaken for singleton records.
func extractPageItems(data json.RawMessage, cursorParam string) ([]json.RawMessage, string, bool) {
	items, cursor, hasMore, _, _ := extractPageItemsWithError(data, cursorParam)
	return items, cursor, hasMore
}

// extractPageItemsWithError extracts a collection page and distinguishes it
// from a validated singleton object. isPage is true for direct arrays and
// recognized pagination envelopes, including empty envelopes.
func extractPageItemsWithError(data json.RawMessage, cursorParam string) ([]json.RawMessage, string, bool, bool, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, "", false, false, fmt.Errorf("empty response body")
	}

	if data[0] == '[' {
		var items []json.RawMessage
		if err := json.Unmarshal(data, &items); err != nil {
			return nil, "", false, false, fmt.Errorf("decoding response array: %w", err)
		}
		return items, "", false, true, nil
	}
	if data[0] != '{' {
		return nil, "", false, false, fmt.Errorf("expected JSON array or object")
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, "", false, false, fmt.Errorf("decoding response object: %w", err)
	}

	itemKeys := []string{"data", "results", "items", "records", "nodes", "entries"}
	for _, key := range itemKeys {
		raw, ok := envelope[key]
		if !ok {
			continue
		}
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 || raw[0] != '[' {
			continue
		}
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, "", false, false, fmt.Errorf("decoding pagination envelope %q: %w", key, err)
		}
		nextCursor, hasMore := extractPaginationFromEnvelope(envelope, cursorParam)
		return items, nextCursor, hasMore, true, nil
	}

	// Fall back to one non-empty resource-named array, e.g. {"markets":[...]}.
	var arrayItems []json.RawMessage
	arrayCount := 0
	for _, raw := range envelope {
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 || raw[0] != '[' {
			continue
		}
		var candidate []json.RawMessage
		if err := json.Unmarshal(raw, &candidate); err == nil && len(candidate) > 0 {
			arrayItems = candidate
			arrayCount++
		}
	}
	if arrayCount == 1 {
		nextCursor, hasMore := extractPaginationFromEnvelope(envelope, cursorParam)
		return arrayItems, nextCursor, hasMore, true, nil
	}

	// A syntactically valid object with no collection envelope is a singleton.
	return nil, "", false, false, nil
}

// extractPaginationFromEnvelope extracts cursor and has_more from a response envelope.
func extractPaginationFromEnvelope(envelope map[string]json.RawMessage, cursorParam string) (string, bool) {
	var hasMore bool

	nextCursor := nextCursorFromLinks(envelope, cursorParam)

	// Try common cursor field names
	cursorKeys := []string{
		"next_cursor", "nextCursor", "cursor", "next_page_token",
		"nextPageToken", "page_token", "after", "end_cursor", "endCursor",
	}
	if nextCursor == "" {
		nextCursor = findCursorInMap(envelope, cursorKeys)
	}

	// If no top-level cursor was found, look one level deeper into well-known
	// pagination wrapper objects. Slack returns {"messages":[...],
	// "response_metadata":{"next_cursor":"..."}}; MongoDB Atlas uses
	// "pagination"; many APIs use "meta" or "paging". Purely additive — only
	// runs when the top-level scan returned empty — and uses the same
	// cursorKeys set so wrapper contents go through the same name match.
	if nextCursor == "" {
		paginationWrapperKeys := []string{"response_metadata", "pagination", "meta", "paging"}
		for _, wrapperKey := range paginationWrapperKeys {
			rawWrapper, ok := envelope[wrapperKey]
			if !ok {
				continue
			}
			var inner map[string]json.RawMessage
			if json.Unmarshal(rawWrapper, &inner) != nil {
				continue
			}
			if c := findCursorInMap(inner, cursorKeys); c != "" {
				nextCursor = c
				break
			}
		}
	}

	// Try common has_more field names
	hasMoreKeys := []string{"has_more", "hasMore", "has_next", "hasNext", "next_page"}
	for _, key := range hasMoreKeys {
		if raw, ok := envelope[key]; ok {
			if err := json.Unmarshal(raw, &hasMore); err == nil {
				break
			}
		}
	}

	// If we found a cursor, assume there are more pages even without explicit has_more
	if nextCursor != "" && !hasMore {
		hasMore = true
	}

	return nextCursor, hasMore
}

// nextCursorFromLinks extracts JSON:API-style pagination cursors from
// {"links":{"next":"https://example.com/items?page[cursor]=..."}}.
func nextCursorFromLinks(envelope map[string]json.RawMessage, cursorParam string) string {
	rawLinks, ok := envelope["links"]
	if !ok {
		return ""
	}
	var links map[string]json.RawMessage
	if json.Unmarshal(rawLinks, &links) != nil {
		return ""
	}
	rawNext, ok := links["next"]
	if !ok {
		return ""
	}
	var nextURL string
	if json.Unmarshal(rawNext, &nextURL) != nil || nextURL == "" {
		return ""
	}

	cursorKeys := []string{cursorParam}
	if cursorParam != "page[cursor]" {
		cursorKeys = append(cursorKeys, "page[cursor]")
	}
	if cursorParam != "cursor" {
		cursorKeys = append(cursorKeys, "cursor")
	}
	if cursorParam != "after" {
		cursorKeys = append(cursorKeys, "after")
	}

	parsed, err := url.Parse(nextURL)
	if err != nil {
		return ""
	}
	values := parsed.Query()
	for _, key := range cursorKeys {
		if key == "" {
			continue
		}
		if cursor := values.Get(key); cursor != "" {
			return cursor
		}
	}
	return ""
}

// findCursorInMap returns the first non-empty string-typed value in m
// whose key matches one of cursorKeys. Used by extractPaginationFromEnvelope
// to scan both the top-level envelope and well-known wrapper objects with
// the same name-match rules — extracted so the two scans can't drift.
func findCursorInMap(m map[string]json.RawMessage, cursorKeys []string) string {
	for _, key := range cursorKeys {
		raw, ok := m[key]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil && s != "" {
			return s
		}
	}
	return ""
}

type discriminatorDispatch struct {
	Field  string
	Values map[string]string
}

var discriminatorDispatchers = map[string]discriminatorDispatch{}

func upsertResourceBatch(db *store.Store, resource string, items []json.RawMessage) (int, int, error) {
	storeResource := canonicalStoreResource(resource)
	if _, ok := discriminatorDispatchers[resource]; !ok {
		// store.UpsertBatch has its own generated ID map;
		// key resources with sync-local overrides here so tags and global schema
		// rows do not drop as primary_key_unresolved.
		if _, hasOverride := resourceIDFieldOverrides[storeResource]; hasOverride {
			return upsertResourceBatchWithExtractedIDs(db, storeResource, items)
		}
		return db.UpsertBatch(storeResource, items)
	}

	grouped := map[string][]json.RawMessage{}
	order := []string{}
	for _, item := range items {
		target := storeResource
		var obj map[string]any
		if err := json.Unmarshal(item, &obj); err == nil {
			target = canonicalStoreResource(resolveDiscriminatedResource(resource, obj))
		}
		if _, ok := grouped[target]; !ok {
			order = append(order, target)
		}
		grouped[target] = append(grouped[target], item)
	}

	var stored, extractFailures int
	for _, target := range order {
		var targetStored, targetExtractFailures int
		var err error
		if _, hasOverride := resourceIDFieldOverrides[target]; hasOverride {
			targetStored, targetExtractFailures, err = upsertResourceBatchWithExtractedIDs(db, target, grouped[target])
		} else {
			targetStored, targetExtractFailures, err = db.UpsertBatch(target, grouped[target])
		}
		if err != nil {
			return stored, extractFailures + targetExtractFailures, err
		}
		stored += targetStored
		extractFailures += targetExtractFailures
	}
	return stored, extractFailures, nil
}

// sync-owned ID overrides are applied before keyed batch
// writes so generated store metadata drift cannot drop name-keyed Zotero rows.
func upsertResourceBatchWithExtractedIDs(db *store.Store, resource string, items []json.RawMessage) (int, int, error) {
	ids := make([]string, 0, len(items))
	payloads := make([]json.RawMessage, 0, len(items))
	var extractFailures int
	for _, item := range items {
		var obj map[string]any
		if err := json.Unmarshal(item, &obj); err != nil {
			continue
		}
		id := extractID(resource, obj)
		if id == "" {
			extractFailures++
			continue
		}
		ids = append(ids, id)
		payloads = append(payloads, item)
	}
	if err := db.UpsertKeyed(resource, ids, payloads); err != nil {
		return 0, extractFailures, err
	}
	return len(ids), extractFailures, nil
}

func canonicalStoreResource(resource string) string {
	// top-level list aliases
	// contain the same records as their parent resource; store them under the
	// canonical type so explicit alias syncs do not flip resource_type metadata.
	switch resource {
	case "items-top":
		return "items"
	case "collections-top":
		return "collections"
	default:
		return resource
	}
}

func resolveDiscriminatedResource(resource string, obj map[string]any) string {
	dispatcher, ok := discriminatorDispatchers[resource]
	if !ok || dispatcher.Field == "" {
		return resource
	}
	value := store.LookupFieldValue(obj, dispatcher.Field)
	if value == nil {
		return resource
	}
	if target, ok := dispatcher.Values[fmt.Sprintf("%v", value)]; ok && target != "" {
		return target
	}
	return resource
}

// upsertSingleObject stores a non-array API response as a single record.
func upsertSingleObject(db *store.Store, resource string, data json.RawMessage) error {
	// Decode with UseNumber so large integer IDs (e.g. 55043301) keep their
	// literal form instead of being coerced to float64 ("5.5043301e+07").
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return fmt.Errorf("decoding single response for %s: %w", resource, err)
	}
	if obj == nil {
		return fmt.Errorf("decoding single response for %s: expected JSON object", resource)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("decoding single response for %s: expected a single JSON object", resource)
		}
		return fmt.Errorf("decoding single response for %s: %w", resource, err)
	}

	resource = resolveDiscriminatedResource(resource, obj)

	id := extractID(resource, obj)
	if id == "" {
		id = resource
	}

	switch resource {
	default:
		return db.Upsert(canonicalStoreResource(resource), id, data)
	}
}

// normalizeSyncResources preserves the caller's resource order while removing
// duplicate work. An items sync always includes the trash feed so the local
// store can discover item deletions; a trash-only sync remains independent.
func normalizeSyncResources(resources []string) []string {
	normalized := make([]string, 0, len(resources)+1)
	seen := make(map[string]struct{}, len(resources)+1)
	hasItems := false

	for _, resource := range resources {
		if _, exists := seen[resource]; exists {
			continue
		}
		seen[resource] = struct{}{}
		normalized = append(normalized, resource)
		hasItems = hasItems || resource == "items"
	}

	if hasItems {
		if _, hasTrash := seen["items-trash"]; !hasTrash {
			normalized = append(normalized, "items-trash")
		}
	}

	return normalized
}

func defaultSyncResources() []string {
	// default sync avoids
	// overlapping top-level aliases because /items and /collections already
	return []string{
		"collections",
		"items",
		"items-trash",
		"schema",
		"schema-creator-fields",
		"schema-item-fields",
		"searches",
		"tags",
	}
}

// syncResourcePath maps resource names to their actual API endpoint paths.
// For REST APIs this is typically "/<resource>". For non-REST APIs (e.g., Steam)
// this preserves the actual endpoint path like "/ISteamApps/GetAppList/v2".
func syncResourcePath(resource string) (string, error) {
	paths := map[string]string{
		"collections":           "/collections",
		"collections-top":       "/collections/top",
		"items":                 "/items",
		"items-top":             "/items/top",
		"items-trash":           "/items/trash",
		"schema":                "/itemTypes",
		"schema-creator-fields": "/creatorFields",
		"schema-item-fields":    "/itemFields",
		"searches":              "/searches",
		"tags":                  "/tags",
	}
	if p, ok := paths[resource]; ok {
		return p, nil
	}
	return "", fmt.Errorf("unknown sync resource %q", resource)
}

// resourceIDFieldOverrides is the store's shared map: both sync and the
// store's UpsertBatch must key rows identically, so there is one definition.
var resourceIDFieldOverrides = store.ResourceIDFieldOverrides

// genericIDFieldFallbacks is the runtime safety net for resources that did
// NOT receive a templated IDField. API-specific names belong in spec
// annotations (x-resource-id), not this list.
var genericIDFieldFallbacks = []string{"id", "ID", "name", "uuid", "slug", "key", "code", "uid"}

// criticalResources is the template-time projection of per-resource Critical
// (set by the profiler from the spec's path-item x-critical extension). It
// is consulted at error-aggregation time so a non-critical failure can be
// downgraded to a sync_warning + exit 0 unless --strict was passed.
//
// Includes both flat resources and dependent (parent-child) resources so a
// failed child sync flagged x-critical: true exits non-zero just like a
// flat-resource critical failure.
var criticalResources = map[string]bool{}

// extractID resolves an item's primary-key field. It consults the
// per-resource templated override first; on miss, it falls through to the
// generic fallback list. resource may be empty for callers that don't have
// a resource context (only the generic list applies in that case).
//
// Field lookups go through store.LookupFieldValue so snake_case overrides
// match camelCase JSON renderings. UpsertBatch resolves fields the same
// way — divergence between the two paths produces silent drops on
// heterogeneous payloads.
func extractID(resource string, obj map[string]any) string {
	if override, ok := resourceIDFieldOverrides[resource]; ok && override != "" {
		if v := store.LookupFieldValue(obj, override); v != nil {
			s := fmt.Sprintf("%v", v)
			if s != "" && s != "<nil>" {
				return s
			}
		}
	}
	for _, key := range genericIDFieldFallbacks {
		if v := store.LookupFieldValue(obj, key); v != nil {
			s := fmt.Sprintf("%v", v)
			if s != "" && s != "<nil>" {
				return s
			}
		}
	}
	return ""
}
