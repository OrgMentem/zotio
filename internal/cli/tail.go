// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"zotio/internal/client"
	"zotio/internal/store"

	"github.com/spf13/cobra"
)

// tailDeletionsUnsupportedOnce keeps the "/deleted is not implemented" notice
// to a single line per process. The condition is a property of the API plane,
// not of any one poll, so repeating it every interval would be pure noise on
// the local API, where it is permanent.
var tailDeletionsUnsupportedOnce sync.Once

func newTailCmd(flags *rootFlags) *cobra.Command {
	var resource string
	var interval time.Duration
	var follow bool
	var workflowPath string
	// dbFlag is the caller's --db override for the store holding the
	// per-resource version cursor; the resolved path never lands back here.
	var dbFlag string

	cmd := &cobra.Command{
		Use:         "tail [resource]",
		Short:       "Stream live changes by polling the API at regular intervals",
		Annotations: map[string]string{"mcp:read-only": "true"},
		Long: `Tail streams live data changes by polling the API at configurable intervals.
Events are emitted as NDJSON to stdout for piping to other tools.
Gracefully shuts down on SIGTERM/SIGINT.

When --workflow <spec.json> is set, tail runs the workflow once after a poll
cycle that emits events. It previews unless this tail invocation carries --yes.
Destructive steps also require --allow-destructive on this tail invocation.
A failed applied run leaves its checkpoint: subsequent applied triggers refuse
until it is resumed or deleted with zotio workflow run <spec> --yes --resume.
The cursor advances only after the triggered invocation returns, so a crash
before or during the trigger replays the same batch on the next poll.

A workflow that reads trigger values (${trigger.json:PATH} or "stdin_trigger")
receives the cycle's events, with upserted and deleted keys in separate lists.
It is skipped, with a notice, on the first poll (no stored cursor), which lists
the current records rather than changes, and on a cycle with more than 500
events.

Deletions are reported only when the configured API serves /deleted. The Zotero
desktop local API does not, so against the default local base this feed emits
upserts only, and says so once on the first poll that checks; point base_url
(or ZOTERO_BASE_URL) at the Web API if you need delete events. If a poll cannot
read deletions for any other reason, the cursor is held and that window is
retried, so upserts may repeat.

Note: For APIs with WebSocket or SSE support, a future version will use
native streaming instead of polling.`,
		Example: `  # Tail item changes every 10 seconds
  zotio tail items --interval 10s

  # Tail a specific resource
  zotio tail collections --interval 5s

  # Pipe to jq for filtering
  zotio tail items --interval 30s | jq 'select(.event == "upsert")'`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("allow-destructive") && workflowPath == "" {
				return usageErr(fmt.Errorf("--allow-destructive requires --workflow"))
			}
			// --interval feeds time.NewTicker below, which panics on a
			// non-positive duration, so reject it as usage input rather
			// than crash with a Go runtime stack. Same shape as watch's
			// interval check (watch.go); tail keeps no minimum because
			// sub-10s polls are a documented tail use.
			// --follow=false never builds a ticker and never reads the
			// interval, so that mode is left alone.
			if follow && interval <= 0 {
				return usageErr(fmt.Errorf("--interval must be positive"))
			}
			if workflowPath != "" {
				if _, err := readWorkflowRunSpec(workflowPath); err != nil {
					return err
				}
			}

			if len(args) > 0 {
				resource = args[0]
			}
			// JSON help envelope: when called with no resource AND --json,
			// surface the list of known resources so agents can discover
			// what to pass without parsing a usage error message.
			// Envelope: {resources: [...], note}.
			if resource == "" && flags.asJSON {
				return printJSONFiltered(cmd.OutOrStdout(), map[string]any{
					"resources": tailKnownResources(),
					"note":      "tail requires a resource name; pass one of the listed names",
				}, flags)
			}
			if resource == "" {
				return usageErr(fmt.Errorf("resource name required (e.g., 'tail items')"))
			}

			c, err := flags.newClient()
			if err != nil {
				return err
			}
			c.NoCache = true

			// Resolve the real change-feed endpoint (items -> /items, etc.)
			// and reject non-change-feed resources.
			path, err := syncResourcePath(resource)
			if err != nil {
				return err
			}

			// Open the local store so each poll resumes from the per-resource
			// version cursor instead of re-fetching all. The path is a local,
			// resolved on every invocation: see resolveDBPath.
			dbPath, err := resolveDBPath(dbFlag, "zotio")
			if err != nil {
				return err
			}

			// Tail streams live and owns delivery per cycle; drop the spool so
			// root.go's post-run flush never fires (it would spool the whole
			// stream forever).
			sink := flags.deliverSink
			flags.deliverSpool.cleanup()
			flags.deliverSpool = nil

			sig := make(chan os.Signal, 1)
			signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
			defer signal.Stop(sig)

			fmt.Fprintln(cmd.ErrOrStderr(), tailStartBanner(resource, interval, follow))

			// Run a trigger after delivery but before the cursor advances. A
			// crash or workflow failure leaves this window retryable. A crash
			// after a successful trigger can replay it, so workflows must be
			// idempotent. Without --workflow there is no hook, so no batch is
			// collected.
			var afterDelivery func(batch tailChangeBatch) error
			if workflowPath != "" {
				afterDelivery = func(batch tailChangeBatch) error {
					if len(batch.Events) == 0 {
						return nil
					}
					inv := workflowRunInvocation{
						Yes:              flags.yes,
						AllowDestructive: flags.allowDestructive,
						DryRun:           flags.dryRun,
						Agent:            flags.agent,
						NoInput:          flags.noInput,
					}
					// A spec that cannot be read is reported by the run
					// itself. Only a spec that reads trigger values gets the
					// batch, so any other spec runs exactly as before.
					if spec, err := readWorkflowRunSpec(workflowPath); err == nil && workflowRunSpecReadsTrigger(spec) {
						trigger, skip := tailWorkflowTrigger(resource, batch)
						if skip != "" {
							fmt.Fprintf(cmd.ErrOrStderr(), "[tail] %s workflow skipped: %s\n", time.Now().UTC().Format(time.RFC3339), skip)
							return nil
						}
						inv.Trigger = trigger
					}
					return runTriggeredWorkflowE(cmd.Context(), cmd, "tail", workflowPath, inv)
				}
			}

			poll := func() error {
				return withInstallationWriterLock(cmd, flags, "tail poll", func() error {
					// Opening may initialize or migrate the store. It belongs
					// to the same transaction as cursor load and publication.
					db, err := store.OpenWithContext(cmd.Context(), dbPath)
					if err != nil {
						return fmt.Errorf("opening local database: %w", err)
					}
					defer db.Close()
					emit := func() error {
						_, err := emitChangesWithHook(cmd.Context(), c, db, resource, path, sink, cmd.OutOrStdout(), afterDelivery)
						return err
					}
					if sink.Scheme == "file" {
						lockPath, _, err := outputWriterLockPath(sink.Target)
						if err != nil {
							return err
						}
						return withPathWriterLock(cmd, lockPath, "tail delivery", emit)
					}
					return emit()
				})
			}
			pollErr := poll()
			if pollErr != nil {
				if !follow {
					return fmt.Errorf("initial tail poll: %w", pollErr)
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "[tail] poll error: %v; holding the cursor for retry\n", pollErr)
			}

			// Honor --follow=false as a single poll.
			if !follow {
				return nil
			}

			// Wait a full interval after each poll, including failed polls.
			// This backs off retries even when a request outlasts the interval.
			timer := time.NewTimer(interval)
			defer timer.Stop()

			for {
				select {
				case <-cmd.Context().Done():
					return cmd.Context().Err()
				case <-sig:
					fmt.Fprintln(cmd.ErrOrStderr(), "\nShutting down gracefully...")
					return nil
				case <-timer.C:
					if err := poll(); err != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "[tail] poll error: %v; holding the cursor for retry\n", err)
					}
					timer.Reset(interval)
				}
			}
		},
	}

	cmd.Flags().StringVar(&resource, "resource", "", "Resource type to tail")
	cmd.Flags().DurationVar(&interval, "interval", 10*time.Second, "Poll interval")
	cmd.Flags().BoolVar(&follow, "follow", true, "Keep running (set --follow=false for single poll)")
	// Cursor persistence location.
	cmd.Flags().StringVar(&dbFlag, "db", "", "Database path (default: ~/.local/share/zotio/data.db)")
	cmd.Flags().StringVar(&workflowPath, "workflow", "", "Run this workflow once after an event-bearing poll; previews unless --yes, and failed applied runs require zotio workflow run <spec> --yes --resume")
	cmd.Flags().BoolVar(&flags.allowDestructive, "allow-destructive", false, "Allow destructive steps in --workflow (requires --workflow)")

	return cmd
}

// tailStartBanner announces what this invocation will actually do.
//
// One-shot mode gets its own sentence. It returns after the initial poll, so
// it never builds a ticker and never reads --interval (see newTailCmd), and
// the follow banner therefore named an interval the run does not use and
// offered a Ctrl+C for a command that has already exited. That wording
// described a different command, and it is reachable in this release now
// that --follow=false --interval 0 is accepted instead of rejected.
func tailStartBanner(resource string, interval time.Duration, follow bool) string {
	if !follow {
		return fmt.Sprintf("Polling %s once (--follow=false); no interval is used", resource)
	}
	return fmt.Sprintf("Tailing %s every %s (Ctrl+C to stop)", resource, interval)
}

// tailKnownResources returns the change-feed resources tail can stream: the
// four resources with a /deleted bucket. Schema has no change feed and is
// omitted.
func tailKnownResources() []string {
	return []string{
		"collections",
		"items",
		"searches",
		"tags",
	}
}

// tailChangeBatch is one poll cycle's emitted events, in emission order, as
// the post-delivery hook sees them.
type tailChangeBatch struct {
	// Baseline marks the first poll (no stored cursor): its upserts list the
	// current records, not changes.
	Baseline bool
	Events   []workflowRunTriggerEvent
}

// tailWorkflowTrigger turns a batch into the trigger for a workflow that
// reads trigger values. A non-empty reason means the run is skipped: a
// baseline lists the whole current set rather than changes, and a batch
// above the bound is never cut down to a partial key list. A skip lets the
// cursor advance; the events were still delivered.
func tailWorkflowTrigger(resource string, batch tailChangeBatch) (*workflowRunTrigger, string) {
	if batch.Baseline {
		return nil, fmt.Sprintf("the first %s poll lists %d current record(s), not changes; workflows that read trigger values start with the next poll", resource, len(batch.Events))
	}
	trigger, err := newWorkflowRunTrigger("tail", resource, batch.Events)
	if err != nil {
		return nil, err.Error() + "; run the workflow over an explicit scope instead"
	}
	return trigger, ""
}

// tailPageSize is the page size tail requests for a change window. Zotero's
// Web API answers 25 objects when no limit is sent and caps a request at 100,
// so every poll pages explicitly instead of trusting one response to hold the
// whole window.
const tailPageSize = 100

// tailMaxWindowAttempts bounds how often a change window is re-read from its
// start because the library moved while it was being paged.
const tailMaxWindowAttempts = 3

// errTailWindowMoved marks a window whose pages were read at different
// library versions.
var errTailWindowMoved = errors.New("the library changed while the change window was being read")

// fetchTailWindow reads every page of the change window since the given
// library version (0 = the whole current set) and returns its objects with the
// library version the window was read at.
//
// The returned version becomes the poll's cursor, so it may only describe a
// window that was read completely and at one version. Last-Modified-Version is
// the version of the whole library, not of the last object on a page: saving
// the first page's version after one response silently skipped every object
// past that page (Web API default: 25) for good. A page that reports a
// different version than the first means the library changed mid-walk, so
// offsets may have shifted and an object may sit on no page at all; the walk
// restarts, and after tailMaxWindowAttempts the poll fails with its cursor
// untouched. Any page failure likewise discards what was read.
func fetchTailWindow(ctx context.Context, c *client.Client, resource, path string, since int) ([]json.RawMessage, int, error) {
	for attempt := 1; ; attempt++ {
		items, version, err := fetchTailWindowOnce(ctx, c, resource, path, since)
		if err == nil {
			return items, version, nil
		}
		if !errors.Is(err, errTailWindowMoved) && !errors.Is(err, errTotalResultsChanged) {
			return nil, 0, err
		}
		if attempt >= tailMaxWindowAttempts {
			return nil, 0, fmt.Errorf("tail %s: %w on each of %d attempts; holding the cursor so the next poll retries: %w", resource, errTailWindowMoved, attempt, err)
		}
	}
}

// fetchTailWindowOnce walks the window once. Termination is judged by the
// server's Total-Results when it sends one, and by a short page otherwise.
func fetchTailWindowOnce(ctx context.Context, c *client.Client, resource, path string, since int) ([]json.RawMessage, int, error) {
	var items []json.RawMessage
	var windowVersion int
	var totals pageTotals
	var previous json.RawMessage
	for page := 0; ; page++ {
		start := len(items)
		params := map[string]string{
			"limit": strconv.Itoa(tailPageSize),
			"start": strconv.Itoa(start),
		}
		if since > 0 {
			params["since"] = strconv.Itoa(since)
		}
		body, version, totalHeader, err := c.GetWithVersionAndHeaderContext(ctx, path, params, "Total-Results")
		if err != nil {
			return nil, 0, err
		}
		if page == 0 {
			windowVersion = version
		} else if version != windowVersion {
			return nil, 0, fmt.Errorf("%w: page at offset %d is at version %d, the window began at %d", errTailWindowMoved, start, version, windowVersion)
		}

		pageItems, _, _, isPage, extractErr := extractPageItemsWithError(body, "")
		if extractErr != nil {
			return nil, 0, fmt.Errorf("tail %s: decoding change page: %w", resource, extractErr)
		}
		if !isPage {
			return nil, 0, fmt.Errorf("tail %s: decoding change page: expected JSON array or object", resource)
		}
		if err := totals.observe(totalHeader, start, len(pageItems)); err != nil {
			if errors.Is(err, errTotalResultsChanged) {
				return nil, 0, err
			}
			return nil, 0, fmt.Errorf("tail %s: change page at offset %d: %w", resource, start, err)
		}
		// A server that ignores start answers the same page every time.
		// Distinct objects cannot repeat byte for byte, so a repeat is a
		// stuck walk, not more data.
		if len(pageItems) > 0 && bytes.Equal(body, previous) {
			return nil, 0, fmt.Errorf("tail %s: the API ignored start=%d and repeated the previous page", resource, start)
		}
		previous = body
		items = append(items, pageItems...)

		if totals.known {
			if err := totals.finish(len(items)); err == nil {
				return items, windowVersion, nil
			} else if len(pageItems) == 0 {
				return nil, 0, fmt.Errorf("tail %s: change window ended early: %w", resource, err)
			}
			continue
		}
		if len(pageItems) < tailPageSize {
			return items, windowVersion, nil
		}
	}
}

// emitChangesWithHook polls one resource for changes since the stored tail
// cursor, emits upsert/delete NDJSON events for the cycle, routes them to the
// deliver sink, and advances the cursor. It returns the number of emitted
// events. Tail is a deduplicated version-cursor change feed rather than a
// full re-fetch each poll. The cursor is namespaced "tail:<resource>" in
// sync_state so it never collides with sync's own checkpoint.
//
// A non-nil afterDelivery runs a triggered workflow after delivery and before
// saving the cursor. It receives the emitted events. A failure holds the
// cursor for retry. A crash after the hook completes can replay it, so
// triggers have at-least-once semantics.
func emitChangesWithHook(ctx context.Context, c *client.Client, db *store.Store, resource, path string, sink DeliverSink, w io.Writer, afterDelivery func(batch tailChangeBatch) error) (int, error) {
	cursorKey := "tail:" + resource
	cursor, err := db.GetLibraryVersion(cursorKey, c.BaseURL)
	if err != nil {
		return 0, fmt.Errorf("tail %s: reading cursor: %w", resource, err)
	}

	// params carries only the since filter: /deleted is a single bucketed
	// document, not a paginated list, so it takes no limit or start.
	params := map[string]string{}
	if cursor > 0 {
		params["since"] = strconv.Itoa(cursor)
	}

	// The whole change window is read before anything is emitted or the
	// cursor moves: the version saved below is the library version of the
	// WHOLE window, so it is only valid once every page of it was seen.
	items, newVer, err := fetchTailWindow(ctx, c, resource, path, cursor)
	if err != nil {
		return 0, err
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	now := time.Now().UTC().Format(time.RFC3339)
	emitted := 0
	batch := tailChangeBatch{Baseline: cursor <= 0}
	recordEvent := func(event, key string) {
		if afterDelivery != nil {
			batch.Events = append(batch.Events, workflowRunTriggerEvent{Event: event, Resource: resource, Key: key})
		}
	}

	for i, item := range items {
		var obj map[string]any
		if err := json.Unmarshal(item, &obj); err != nil {
			return 0, fmt.Errorf("tail %s: decoding change object %d: %w", resource, i, err)
		}
		if obj == nil {
			return 0, fmt.Errorf("tail %s: decoding change object %d: expected JSON object", resource, i)
		}
		// Identity uses the shared extractor so per-resource keys keep
		// working (tags identify by name, everything else by key). An object
		// without identity would otherwise emit a null-key event and advance
		// the cursor past a window that can never be recovered, so fail the
		// poll and hold the cursor instead.
		key := store.ExtractResourceID(resource, obj)
		if key == "" {
			return 0, fmt.Errorf("tail %s: decoding change object %d: missing resource identity", resource, i)
		}
		if resource == "tags" {
			// /deleted identifies tags by name, not the mirror's name/type ID.
			key = fmt.Sprintf("%v", store.LookupFieldValue(obj, "tag"))
		}
		event := map[string]any{
			"event":     "upsert",
			"resource":  resource,
			"key":       key,
			"version":   store.LookupFieldValue(obj, "version"),
			"timestamp": now,
			"data":      obj,
		}
		if err := enc.Encode(event); err != nil {
			return emitted, err
		}
		emitted++
		recordEvent("upsert", key)
	}

	// Deletions only make sense once a baseline cursor exists: the first
	// poll (cursor == 0) emits the full current set as upserts and skips
	// /deleted, which is the intended change-feed bootstrap.
	//
	// A failure here is classified rather than uniformly warned past, because
	// the two cases have opposite correct responses:
	//
	//   - The plane does not implement /deleted at all. The Zotero local API
	//     404s it, and local is the default base, so this is the steady state
	//     for most users. No deletion can be missed by advancing, because none
	//     is observable on this plane ever. Holding the cursor here would wedge
	//     the feed permanently and re-emit the whole window every poll.
	//   - Anything else (5xx, timeout, connection reset). Deletions may well
	//     exist in this window and were simply not retrieved. Advancing the
	//     cursor past them loses them permanently, since the next poll asks
	//     for changes strictly after newVer. Hold the cursor so the window is
	//     retried; the upserts already emitted redeliver, which is the
	//     at-least-once contract a change feed is allowed to have.
	deletionsIncomplete := false
	if cursor > 0 {
		delBody, _, derr := c.GetWithVersionContext(ctx, "/deleted", params)
		if derr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return 0, ctxErr
			}
			var apiErr *client.APIError
			if errors.As(derr, &apiErr) && (apiErr.StatusCode == http.StatusNotFound || apiErr.StatusCode == http.StatusNotImplemented) {
				tailDeletionsUnsupportedOnce.Do(func() {
					fmt.Fprintf(os.Stderr, "warning: tail %s: this Zotero API does not implement /deleted (HTTP %d); deletions will not be reported on this feed\n", resource, apiErr.StatusCode)
				})
			} else {
				deletionsIncomplete = true
				fmt.Fprintf(os.Stderr, "warning: tail %s: fetching deletions failed: %v; holding the cursor at %d so this window is retried (upserts may repeat)\n", resource, derr, cursor)
			}
		} else {
			var buckets map[string][]string
			if err := json.Unmarshal(delBody, &buckets); err != nil {
				return 0, fmt.Errorf("tail %s: decoding deletions: %w", resource, err)
			}
			for _, k := range buckets[resource] {
				event := map[string]any{
					"event":     "delete",
					"resource":  resource,
					"key":       k,
					"timestamp": now,
				}
				if err := enc.Encode(event); err != nil {
					return emitted, err
				}
				emitted++
				recordEvent("delete", k)
			}
		}
	}

	// Delivery is all-or-nothing per batch from the cursor's perspective.
	// The cursor below advances only after the stdout write, every
	// configured sink acknowledgment (a 2xx webhook response, a completed
	// file write+sync+close), the post-delivery trigger (when set), and the
	// cursor save itself all succeed. Any failure returns before the save,
	// so the next poll re-fetches the same window via since and redelivers
	// the whole batch: duplicates are possible (at-least-once; webhook
	// receivers must be idempotent). A failed file append rolls back to
	// its original size, preserving line boundaries for the retried batch.
	// There is deliberately no per-event checkpoint and no durable
	// pending-batch state: the held cursor IS the retry state. This
	// per-cycle delivery bypasses the shared deliver_spool path on purpose
	// (newTailCmd drops the spool so the post-run flush never fires), so a
	// batch reaches its sink here and never again through root.go;
	// postDeliverWebhook makes a single POST attempt with no application
	// retry, so no hidden retry can deliver after the cursor has moved.
	out := buf.Bytes()
	if len(out) > 0 {
		if _, err := w.Write(out); err != nil {
			return emitted, err
		}
		switch sink.Scheme {
		case "webhook":
			if err := deliverWebhook(ctx, sink.Target, out, true); err != nil {
				return emitted, fmt.Errorf("tail %s: delivering webhook: %w", resource, err)
			}
		case "file":
			dir := filepath.Dir(sink.Target)
			if dir != "" && dir != "." {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					return emitted, fmt.Errorf("tail %s: creating delivery directory: %w", resource, err)
				}
			}
			f, err := os.OpenFile(sink.Target, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err != nil {
				return emitted, fmt.Errorf("tail %s: opening delivery file: %w", resource, err)
			}
			if err := appendTailBatch(f, out); err != nil {
				_ = f.Close()
				return emitted, fmt.Errorf("tail %s: writing delivery file: %w", resource, err)
			}
			if err := f.Close(); err != nil {
				return emitted, fmt.Errorf("tail %s: closing delivery file: %w", resource, err)
			}
		}
	}

	if afterDelivery != nil {
		if err := afterDelivery(batch); err != nil {
			return emitted, fmt.Errorf("tail %s: triggered workflow: %w", resource, err)
		}
	}

	if newVer > cursor && !deletionsIncomplete {
		if err := db.SaveLibraryVersion(cursorKey, c.BaseURL, newVer); err != nil {
			return emitted, fmt.Errorf("tail %s: saving cursor: %w", resource, err)
		}
	}
	return emitted, nil
}

// tailAppendFile keeps the durable append operation testable with short writes
// and sync failures, without replacing process-wide filesystem functions.
type tailAppendFile interface {
	Stat() (os.FileInfo, error)
	Write([]byte) (int, error)
	Truncate(int64) error
	Sync() error
}

func appendTailBatch(f tailAppendFile, batch []byte) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	n, err := f.Write(batch)
	if err == nil && n != len(batch) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = f.Sync()
	}
	if err == nil {
		return nil
	}
	if truncateErr := f.Truncate(info.Size()); truncateErr != nil {
		return errors.Join(err, fmt.Errorf("rolling back delivery append: %w", truncateErr))
	}
	if syncErr := f.Sync(); syncErr != nil {
		return errors.Join(err, fmt.Errorf("syncing delivery rollback: %w", syncErr))
	}
	return err
}
