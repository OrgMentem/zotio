// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"zotio/internal/store"

	"github.com/spf13/cobra"
)

func newWorkflowCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "workflow",
		Short: "Compound workflows that combine multiple API operations",
	}

	cmd.AddCommand(newWorkflowArchiveCmd(flags))
	cmd.AddCommand(newWorkflowRunCmd(flags))
	cmd.AddCommand(newWorkflowStatusCmd(flags))

	return cmd
}

func newWorkflowArchiveCmd(flags *rootFlags) *cobra.Command {
	var dbPath string
	var full bool

	cmd := &cobra.Command{
		Use:   "archive",
		Short: "Sync all resources to local store for offline access and search",
		Long: `Archive fetches all syncable resources from the API and stores them in a
local SQLite database. Supports incremental sync (only new data since last run)
and full resync. After archiving, use 'search' for instant full-text search.`,
		Example: `  # Archive all resources
  zotio workflow archive

  # Full re-archive (ignore previous sync state)
  zotio workflow archive --full`,
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
			s, err := store.OpenWithContext(cmd.Context(), dbPath)
			if err != nil {
				return fmt.Errorf("opening store: %w", err)
			}
			defer s.Close()

			// top-level alias endpoints fold
			// into canonical items/collections storage; archiving them separately
			// creates redundant fetches and stale-looking status rows.
			resources := []string{"collections", "items", "items-trash", "schema", "schema-creator-fields", "schema-item-fields", "searches", "tags"}
			totalSynced := 0
			var failures []string
			var accessWarnings []string

			for _, resource := range resources {
				start := 0
				checkpointCount := 0
				if !full {
					existing, _, existingCount, stateErr := s.GetSyncState(resource)
					if stateErr != nil {
						detail := fmt.Sprintf("%s: reading sync state: %v", resource, stateErr)
						failures = append(failures, detail)
						fmt.Fprintf(cmd.ErrOrStderr(), "  error: %s\n", detail)
						continue
					}
					if existing != "" {
						if offset, err := strconv.Atoi(existing); err == nil && offset >= 0 {
							start = offset
							checkpointCount = existingCount
						}
					}
				}

				fmt.Fprintf(cmd.ErrOrStderr(), "Syncing %s...\n", resource)

				const pageSize = 100
				params := map[string]string{
					"limit": strconv.Itoa(pageSize),
					"start": strconv.Itoa(start),
				}
				count := 0
				resourceIncomplete := false
				var previousPage json.RawMessage
				for {
					data, fetchErr := c.Get("/"+resource, params)
					if fetchErr != nil {
						resourceIncomplete = true
						if warning, ok := isSyncAccessWarning(fetchErr); ok {
							detail := fmt.Sprintf("%s: access denied (%s)", resource, warning.Reason)
							accessWarnings = append(accessWarnings, detail)
							fmt.Fprintf(cmd.ErrOrStderr(), "  warning: %s\n", detail)
						} else {
							detail := fmt.Sprintf("%s: fetching: %v", resource, fetchErr)
							failures = append(failures, detail)
							fmt.Fprintf(cmd.ErrOrStderr(), "  error: %s\n", detail)
						}
						break
					}

					var items []json.RawMessage
					if err := json.Unmarshal(data, &items); err != nil {
						// Some schema endpoints return one object. A malformed
						// response is an incomplete archive, not a singleton.
						var singleton map[string]json.RawMessage
						if err := json.Unmarshal(data, &singleton); err != nil {
							resourceIncomplete = true
							detail := fmt.Sprintf("%s: parsing response: %v", resource, err)
							failures = append(failures, detail)
							fmt.Fprintf(cmd.ErrOrStderr(), "  error: %s\n", detail)
							break
						}
						if err := s.Upsert(resource, resource+"-singleton", data); err != nil {
							resourceIncomplete = true
							detail := fmt.Sprintf("%s: storing singleton: %v", resource, err)
							failures = append(failures, detail)
							fmt.Fprintf(cmd.ErrOrStderr(), "  error: %s\n", detail)
							break
						}
						count++
						checkpointCount++
						if err := s.SaveSyncState(resource, "", checkpointCount); err != nil {
							resourceIncomplete = true
							detail := fmt.Sprintf("%s: saving sync state: %v", resource, err)
							failures = append(failures, detail)
							fmt.Fprintf(cmd.ErrOrStderr(), "  error: %s\n", detail)
						}
						break
					}
					if len(items) == 0 {
						if err := s.SaveSyncState(resource, "", checkpointCount); err != nil {
							resourceIncomplete = true
							detail := fmt.Sprintf("%s: saving sync state: %v", resource, err)
							failures = append(failures, detail)
							fmt.Fprintf(cmd.ErrOrStderr(), "  error: %s\n", detail)
						}
						break
					}
					if previousPage != nil && bytes.Equal(data, previousPage) {
						resourceIncomplete = true
						detail := fmt.Sprintf("%s: pagination did not advance", resource)
						failures = append(failures, detail)
						fmt.Fprintf(cmd.ErrOrStderr(), "  error: %s\n", detail)
						break
					}
					previousPage = data

					stored, unresolved, err := s.UpsertBatch(resource, items)
					if err != nil {
						resourceIncomplete = true
						detail := fmt.Sprintf("%s: storing page: %v", resource, err)
						failures = append(failures, detail)
						fmt.Fprintf(cmd.ErrOrStderr(), "  error: %s\n", detail)
						break
					}
					count += stored
					checkpointCount += stored
					if unresolved > 0 {
						resourceIncomplete = true
						detail := fmt.Sprintf("%s: %d row(s) had no stable primary key", resource, unresolved)
						failures = append(failures, detail)
						fmt.Fprintf(cmd.ErrOrStderr(), "  error: %s\n", detail)
						break
					}

					start += len(items)
					nextCursor := ""
					if len(items) == pageSize {
						nextCursor = strconv.Itoa(start)
					}
					if err := s.SaveSyncState(resource, nextCursor, checkpointCount); err != nil {
						resourceIncomplete = true
						detail := fmt.Sprintf("%s: saving sync state: %v", resource, err)
						failures = append(failures, detail)
						fmt.Fprintf(cmd.ErrOrStderr(), "  error: %s\n", detail)
						break
					}
					if nextCursor == "" {
						break
					}
					params["start"] = nextCursor
				}

				totalSynced += count
				if resourceIncomplete {
					fmt.Fprintf(cmd.ErrOrStderr(), "  %s: incomplete after %d items\n", resource, count)
				} else {
					fmt.Fprintf(cmd.ErrOrStderr(), "  %s: %d items\n", resource, count)
				}
			}

			status := "complete"
			if len(failures) > 0 {
				status = "incomplete"
			} else if len(accessWarnings) > 0 {
				status = "complete_with_access_warnings"
			}
			if flags.asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				if err := enc.Encode(map[string]any{
					"status":           status,
					"resources_synced": len(resources),
					"total_items":      totalSynced,
					"store_path":       dbPath,
					"access_warnings":  accessWarnings,
					"failures":         failures,
					"timestamp":        time.Now().UTC().Format(time.RFC3339),
				}); err != nil {
					return err
				}
			} else if len(failures) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "Archive incomplete: %d items stored across %d resources to %s\n", totalSynced, len(resources), dbPath)
			} else if len(accessWarnings) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "Archive completed with %d access warnings: %d items across %d resources to %s\n", len(accessWarnings), totalSynced, len(resources), dbPath)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "Archived %d items across %d resources to %s\n", totalSynced, len(resources), dbPath)
			}
			if len(failures) > 0 {
				return degradedErr(fmt.Errorf("archive incomplete: %s", strings.Join(failures, "; ")))
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&dbPath, "db", "", "Database path (default: ~/.local/share/zotio/data.db)")
	cmd.Flags().BoolVar(&full, "full", false, "Full re-archive (ignore previous sync state)")

	return cmd
}

func newWorkflowStatusCmd(flags *rootFlags) *cobra.Command {
	var dbPath string

	cmd := &cobra.Command{
		Use:         "status",
		Short:       "Show local archive status and sync state for all resources",
		Annotations: map[string]string{"mcp:read-only": "true"},
		Example: `  # Show archive status
  zotio workflow status

  # Show status as JSON
  zotio workflow status --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if dbPath == "" {
				var err error
				dbPath, err = defaultDBPath("zotio")
				if err != nil {
					return err
				}
			}
			s, err := store.OpenWithContext(cmd.Context(), dbPath)
			if err != nil {
				return fmt.Errorf("opening store: %w", err)
			}
			defer s.Close()

			status, err := s.Status()
			if err != nil {
				return err
			}

			if flags.asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(status)
			}

			if len(status) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No archived data. Run 'workflow archive' to sync.")
				return nil
			}

			fmt.Fprintln(cmd.OutOrStdout(), "Archive Status:")
			total := 0
			for resource, count := range status {
				fmt.Fprintf(cmd.OutOrStdout(), "  %-30s %d items\n", resource, count)
				total += count
			}
			fmt.Fprintf(cmd.OutOrStdout(), "\n  Total: %d items\n", total)
			fmt.Fprintf(cmd.OutOrStdout(), "  Store: %s\n", dbPath)
			return nil
		},
	}

	cmd.Flags().StringVar(&dbPath, "db", "", "Database path")

	return cmd
}

// defaultDBPath is defined in helpers.go
