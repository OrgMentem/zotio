// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"zotio/internal/store"

	"github.com/spf13/cobra"
)

// isNilOrEmpty checks whether a JSON object has nil or empty values for
// common identifier fields (title, name, identifier, id).
// Also checks nested "document" objects for search result wrappers.
func isNilOrEmpty(raw json.RawMessage) bool {
	var obj map[string]interface{}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return true
	}
	// Check top-level fields.:
	// keep Zotero resource envelopes from local FTS results; they identify rows
	// by top-level "key" and usually keep title/name under nested "data".
	if hasSearchIdentity(obj, []string{"title", "name", "identifier", "id", "key", "slug"}) {
		return false
	}
	if data, ok := obj["data"].(map[string]interface{}); ok {
		if hasSearchIdentity(data, []string{"title", "name", "identifier", "id", "key", "slug", "itemType"}) {
			return false
		}
	}
	// Check nested "document" for search result wrappers like {score, document: {name, ...}}
	if doc, ok := obj["document"]; ok {
		if docMap, ok := doc.(map[string]interface{}); ok {
			if hasSearchIdentity(docMap, []string{"title", "name", "identifier", "id", "key", "slug"}) {
				return false
			}
		}
	}
	// If the object has a "score" field, it's likely a search result — keep it
	if _, ok := obj["score"]; ok {
		return false
	}
	return true
}

func hasSearchIdentity(obj map[string]interface{}, keys []string) bool {
	for _, key := range keys {
		if v, ok := obj[key]; ok {
			if v == nil {
				continue
			}
			if s, ok := v.(string); ok {
				if strings.TrimSpace(s) != "" {
					return true
				}
				continue
			}
			return true
		}
	}
	return false
}

// extractSearchResults unwraps API search responses by checking common envelope paths.
func extractSearchResults(data json.RawMessage) ([]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("decoding search response: expected a JSON array or object")
	}

	switch trimmed[0] {
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, fmt.Errorf("decoding search response array: %w", err)
		}
		return items, nil
	case '{':
		var wrapped map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &wrapped); err != nil {
			return nil, fmt.Errorf("decoding search response object: %w", err)
		}
		for _, key := range []string{"data", "results", "items", "records", "entries"} {
			if inner, ok := wrapped[key]; ok && len(bytes.TrimSpace(inner)) > 0 && bytes.TrimSpace(inner)[0] == '[' {
				var items []json.RawMessage
				if err := json.Unmarshal(inner, &items); err != nil {
					return nil, fmt.Errorf("decoding search response %q array: %w", key, err)
				}
				return items, nil
			}
		}
		return []json.RawMessage{data}, nil
	default:
		return nil, fmt.Errorf("decoding search response: expected a JSON array or object")
	}
}

func newSearchCmd(flags *rootFlags) *cobra.Command {
	var resourceType string
	var limit int
	var dbPath string

	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Full-text search across synced data or live API",
		Long: `Search data using FTS5 full-text search on locally synced data,
or hit the API's search endpoint when available.

In auto mode (default): uses the API search endpoint if the API has one,
otherwise searches local data. Falls back to local on network failure.
In live mode: uses the API search endpoint only.
In local mode: searches locally synced data only.`,
		Example: `  # Search (uses API endpoint if available, local FTS otherwise)
  zotio search "error timeout"

  # Force local search only
  zotio search "payment failed" --data-source local

  # Search a specific resource type locally
  zotio search "critical" --type transactions --data-source local

  # JSON output for piping
  zotio search "critical" --json --limit 20`,
		Annotations: map[string]string{"mcp:hidden": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			query := args[0]
			// Zotero item full-text search is GET /items
			// with qmode=everything; /searches returns saved-search definitions.
			if flags.dataSource != "local" {
				c, err := flags.newClient()
				if err != nil {
					return err
				}
				params := map[string]string{"q": query, "qmode": "everything"}
				if limit > 0 {
					params["limit"] = fmt.Sprintf("%d", limit)
				}
				data, getErr := c.Get("/items", params)
				if getErr == nil {
					// Live search succeeded.
					results, err := extractSearchResults(data)
					if err != nil {
						return fmt.Errorf("decoding live search response: %w", err)
					}
					prov := DataProvenance{Source: "live"}
					return outputSearchResults(cmd, flags, results, limit, prov)
				}
				// Check if it's a network error for auto-mode fallback
				if flags.dataSource == "live" || !isNetworkError(getErr) {
					return classifyAPIError(getErr, flags)
				}
				// auto mode + network error: fall through to local FTS
				fmt.Fprintf(cmd.ErrOrStderr(), "API unreachable, falling back to local search.\n")
			}

			// Local FTS search
			if dbPath == "" {
				dbPath = defaultDBPath("zotio")
			}

			db, err := store.OpenWithContext(cmd.Context(), dbPath)
			if err != nil {
				return fmt.Errorf("opening local database: %w\nRun 'zotio sync' first to populate the local database.", err)
			}
			defer db.Close()

			var results []json.RawMessage
			// default local search runs cross-resource FTS
			// (previously a no-op); --type scopes it to one resource type.
			if resourceType == "" {
				results, err = db.Search(query, limit)
			} else {
				results, err = db.SearchByType(query, resourceType, limit)
			}
			if err != nil {
				return fmt.Errorf("search failed: %w", err)
			}

			reason := "user_requested"
			if flags.dataSource == "auto" {
				reason = "api_unreachable"
			}
			prov := localProvenance(db, "search", reason)

			return outputSearchResults(cmd, flags, results, limit, prov)
		},
	}

	cmd.Flags().StringVar(&resourceType, "type", "", "Filter by resource type")
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum results to return")
	cmd.Flags().StringVar(&dbPath, "db", "", "Database path (default: ~/.local/share/zotio/data.db)")

	return cmd
}

// outputSearchResults filters, counts, and outputs search results with provenance.
func outputSearchResults(cmd *cobra.Command, flags *rootFlags, results []json.RawMessage, limit int, prov DataProvenance) error {
	// keep the defensive JSON-shape filter only
	// for live API search wrappers. Local FTS already returns concrete rows, so
	// it avoids the per-result unmarshal hot path.
	if prov.Source == "live" {
		filtered := make([]json.RawMessage, 0, len(results))
		for _, r := range results {
			if !isNilOrEmpty(r) {
				filtered = append(filtered, r)
			}
		}
		results = filtered
	}

	// Enforce limit across aggregated results.
	if limit > 0 && len(results) > limit {
		results = results[:limit]
	}

	jsonMode := flags.asJSON || !isTerminal(cmd.OutOrStdout())

	// JSON mode always emits a valid envelope, including on no matches —
	// agents pipe stdout through json.loads / jq and need parseable output
	// regardless of result count. The filtered slice is built via make
	// above, so it's non-nil even when empty; json.Marshal renders that
	// as `[]` rather than `null`.
	if jsonMode {
		data, err := json.Marshal(results)
		if err != nil {
			return err
		}
		wrapped, err := wrapWithProvenance(data, prov)
		if err != nil {
			return err
		}
		return printOutput(cmd.OutOrStdout(), wrapped, true)
	}

	if len(results) == 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "No results (source: %s)\n", prov.Source)
		return nil
	}

	printProvenance(cmd, len(results), prov)
	// Human at a terminal: same auto table/card rendering as the list
	// commands. Fall back to raw JSON only if the rows don't decode.
	var items []map[string]any
	if raw, err := json.Marshal(results); err == nil {
		if json.Unmarshal(raw, &items) == nil && len(items) > 0 {
			return printAutoTable(cmd.OutOrStdout(), items)
		}
	}
	for _, r := range results {
		fmt.Fprintln(cmd.OutOrStdout(), string(r))
	}
	return nil
}
