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
A failed applied run leaves its checkpoint: subsequent applied triggers refuse
until it is resumed or deleted with zotio workflow run <spec> --yes --resume.

Deletions are reported only when the configured API serves /deleted. The Zotero
desktop local API does not, so against the default local base this feed emits
upserts only, and says so once on the first poll that checks; point base_url
(or ZOTERO_BASE_URL) at the Web API if you need delete events. If a poll cannot
read deletions for any other reason, the cursor is held and that window is
retried, so upserts may repeat.

Note: For APIs with WebSocket or SSE support, a future version will use
native streaming instead of polling.`,
		Example: `  # Tail all changes every 10 seconds
  zotio tail --interval 10s

  # Tail a specific resource
  zotio tail messages --interval 5s

  # Pipe to jq for filtering
  zotio tail events --interval 30s | jq 'select(.type == "error")'`,
		RunE: func(cmd *cobra.Command, args []string) error {
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
				return fmt.Errorf("resource name required (e.g., 'tail items')")
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
			db, err := store.OpenWithContext(cmd.Context(), dbPath)
			if err != nil {
				return fmt.Errorf("opening local database: %w", err)
			}
			defer db.Close()

			// Tail streams live and owns delivery per cycle; drop the spool so
			// root.go's post-run flush never fires (it would spool the whole
			// stream forever).
			sink := flags.deliverSink
			flags.deliverSpool.cleanup()
			flags.deliverSpool = nil

			sig := make(chan os.Signal, 1)
			signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
			defer signal.Stop(sig)

			fmt.Fprintf(os.Stderr, "Tailing %s every %s (Ctrl+C to stop)\n", resource, interval)

			// Initial poll
			if events, err := emitChanges(cmd.Context(), c, db, resource, path, sink, cmd.OutOrStdout()); err != nil {
				return fmt.Errorf("initial tail poll: %w", err)
			} else if events >= 1 && workflowPath != "" {
				runTriggeredWorkflow(cmd.Context(), cmd, "tail", workflowPath, workflowRunInvocation{
					Yes:     flags.yes,
					DryRun:  flags.dryRun,
					Agent:   flags.agent,
					NoInput: flags.noInput,
				})
			}

			// Honor --follow=false as a single poll.
			if !follow {
				return nil
			}

			// The ticker exists only for the follow loop, so it is built
			// after the one-shot exit: no path can hand an unvalidated
			// interval to time.NewTicker.
			ticker := time.NewTicker(interval)
			defer ticker.Stop()

			for {
				select {
				case <-cmd.Context().Done():
					return cmd.Context().Err()
				case <-sig:
					fmt.Fprintln(os.Stderr, "\nShutting down gracefully...")
					return nil
				case <-ticker.C:
					if events, err := emitChanges(cmd.Context(), c, db, resource, path, sink, cmd.OutOrStdout()); err != nil {
						return fmt.Errorf("tail poll: %w", err)
					} else if events >= 1 && workflowPath != "" {
						runTriggeredWorkflow(cmd.Context(), cmd, "tail", workflowPath, workflowRunInvocation{
							Yes:     flags.yes,
							DryRun:  flags.dryRun,
							Agent:   flags.agent,
							NoInput: flags.noInput,
						})
					}
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

	return cmd
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

// emitChanges polls one resource for changes since the stored tail cursor,
// emits upsert/delete NDJSON events for the cycle, routes them to the deliver
// sink, and advances the cursor. It returns the number of emitted events.
// Tail is a deduplicated version-cursor change feed rather than a full
// re-fetch each poll. The cursor is namespaced "tail:<resource>" in
// sync_state so it never collides with sync's own checkpoint.
func emitChanges(ctx context.Context, c *client.Client, db *store.Store, resource, path string, sink DeliverSink, w io.Writer) (int, error) {
	cursorKey := "tail:" + resource
	cursor, _ := db.GetLibraryVersion(cursorKey, c.BaseURL)

	params := map[string]string{}
	if cursor > 0 {
		params["since"] = strconv.Itoa(cursor)
	}

	body, newVer, err := c.GetWithVersionContext(ctx, path, params)
	if err != nil {
		return 0, err
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	now := time.Now().UTC().Format(time.RFC3339)
	emitted := 0

	items, _, _, isPage, extractErr := extractPageItemsWithError(body, "")
	if extractErr != nil {
		return 0, fmt.Errorf("tail %s: decoding change page: %w", resource, extractErr)
	}
	if !isPage {
		return 0, fmt.Errorf("tail %s: decoding change page: expected JSON array or object", resource)
	}
	for _, item := range items {
		var obj map[string]any
		if err := json.Unmarshal(item, &obj); err != nil {
			continue
		}
		event := map[string]any{
			"event":     "upsert",
			"resource":  resource,
			"key":       fmt.Sprintf("%v", store.LookupFieldValue(obj, "key")),
			"version":   store.LookupFieldValue(obj, "version"),
			"timestamp": now,
			"data":      obj,
		}
		if err := enc.Encode(event); err != nil {
			return emitted, err
		}
		emitted++
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
			}
		}
	}

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
			if _, err := f.Write(out); err != nil {
				_ = f.Close()
				return emitted, fmt.Errorf("tail %s: writing delivery file: %w", resource, err)
			}
			if err := f.Close(); err != nil {
				return emitted, fmt.Errorf("tail %s: closing delivery file: %w", resource, err)
			}
		}
	}

	if newVer > cursor && !deletionsIncomplete {
		if err := db.SaveLibraryVersion(cursorKey, c.BaseURL, newVer); err != nil {
			return emitted, fmt.Errorf("tail %s: saving cursor: %w", resource, err)
		}
	}
	return emitted, nil
}
