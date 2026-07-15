// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"zotio/internal/client"
	"zotio/internal/store"

	"github.com/spf13/cobra"
)

func newTailCmd(flags *rootFlags) *cobra.Command {
	var resource string
	var interval time.Duration
	var follow bool
	var workflowPath string
	// dbPath stores the per-resource version cursor.
	var dbPath string

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

Note: For APIs with WebSocket or SSE support, a future version will use
native streaming instead of polling.`,
		Example: `  # Tail all changes every 10 seconds
  zotio tail --interval 10s

  # Tail a specific resource
  zotio tail messages --interval 5s

  # Pipe to jq for filtering
  zotio tail events --interval 30s | jq 'select(.type == "error")'`,
		RunE: func(cmd *cobra.Command, args []string) error {
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
			// version cursor instead of re-fetching all.
			if dbPath == "" {
				dbPath = defaultDBPath("zotio")
			}
			db, err := store.OpenWithContext(cmd.Context(), dbPath)
			if err != nil {
				return fmt.Errorf("opening local database: %w", err)
			}
			defer db.Close()

			// Tail streams live and owns delivery per cycle; nil the deliver
			// buffer so root.go's post-run flush never fires (it would buffer
			// the whole stream forever).
			sink := flags.deliverSink
			flags.deliverBuf = nil

			sig := make(chan os.Signal, 1)
			signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)

			ticker := time.NewTicker(interval)
			defer ticker.Stop()

			fmt.Fprintf(os.Stderr, "Tailing %s every %s (Ctrl+C to stop)\n", resource, interval)

			// Initial poll
			if events, err := emitChanges(cmd.Context(), c, db, resource, path, sink, os.Stdout); err != nil {
				fmt.Fprintf(os.Stderr, "warning: initial poll failed: %v\n", err)
			} else if events >= 1 && workflowPath != "" {
				runTriggeredWorkflow(cmd, "tail", workflowPath, flags.yes)
			}

			// Honor --follow=false as a single poll.
			if !follow {
				return nil
			}

			for {
				select {
				case <-cmd.Context().Done():
					return cmd.Context().Err()
				case <-sig:
					fmt.Fprintln(os.Stderr, "\nShutting down gracefully...")
					return nil
				case <-ticker.C:
					if events, err := emitChanges(cmd.Context(), c, db, resource, path, sink, os.Stdout); err != nil {
						fmt.Fprintf(os.Stderr, "warning: poll failed: %v\n", err)
					} else if events >= 1 && workflowPath != "" {
						runTriggeredWorkflow(cmd, "tail", workflowPath, flags.yes)
					}
				}
			}
		},
	}

	cmd.Flags().StringVar(&resource, "resource", "", "Resource type to tail")
	cmd.Flags().DurationVar(&interval, "interval", 10*time.Second, "Poll interval")
	cmd.Flags().BoolVar(&follow, "follow", true, "Keep running (set --follow=false for single poll)")
	// Cursor persistence location.
	cmd.Flags().StringVar(&dbPath, "db", "", "Database path (default: ~/.local/share/zotio/data.db)")
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
	cursor, _ := db.GetLibraryVersion(cursorKey)

	params := map[string]string{}
	if cursor > 0 {
		params["since"] = strconv.Itoa(cursor)
	}

	body, newVer, err := c.GetWithVersion(path, params)
	if err != nil {
		return 0, err
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	now := time.Now().UTC().Format(time.RFC3339)
	emitted := 0

	items, _, _ := extractPageItems(body, "")
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
	if cursor > 0 {
		delBody, _, derr := c.GetWithVersion("/deleted", params)
		if derr != nil {
			fmt.Fprintf(os.Stderr, "warning: tail %s: fetching deletions failed: %v\n", resource, derr)
		} else {
			var buckets map[string][]string
			if err := json.Unmarshal(delBody, &buckets); err == nil {
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
	}

	out := buf.Bytes()
	if len(out) > 0 {
		if _, err := w.Write(out); err != nil {
			return emitted, err
		}
		switch sink.Scheme {
		case "webhook":
			if err := deliverWebhook(ctx, sink.Target, out, true); err != nil {
				fmt.Fprintf(os.Stderr, "warning: tail %s: webhook delivery failed: %v\n", resource, err)
			}
		case "file":
			dir := filepath.Dir(sink.Target)
			if dir != "" && dir != "." {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					fmt.Fprintf(os.Stderr, "warning: tail %s: file delivery failed: %v\n", resource, err)
					break
				}
			}
			f, ferr := os.OpenFile(sink.Target, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if ferr != nil {
				fmt.Fprintf(os.Stderr, "warning: tail %s: file delivery failed: %v\n", resource, ferr)
			} else {
				if _, werr := f.Write(out); werr != nil {
					fmt.Fprintf(os.Stderr, "warning: tail %s: file delivery failed: %v\n", resource, werr)
				}
				_ = f.Close()
			}
		}
	}

	if newVer > cursor {
		_ = db.SaveLibraryVersion(cursorKey, newVer)
	}
	return emitted, nil
}
