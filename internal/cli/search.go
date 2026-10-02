// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
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
	var dbFlag string
	var fulltextOnly bool
	var flagScope string

	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Full-text search across synced data or live API",
		Long: `Search data using FTS5 full-text search on locally synced data,
or hit the API's search endpoint when available.

In auto mode (default): uses the API search endpoint if the API has one,
otherwise searches local data. Falls back to local on network failure.
In live mode: uses the API search endpoint only.
In local mode: searches locally synced data only.
Use --fulltext to search synced PDF text and resolve hits to parent items.
With --fulltext, --scope limits hits to one item cohort before ranking and
--limit apply, so a hit outside the cohort never takes a place in the limit.`,
		Example: `  # Search (uses API endpoint if available, local FTS otherwise)
  zotio search "error timeout"

  # Force local search only
  zotio search "payment failed" --data-source local

  # Search a specific resource type locally
  zotio search "critical" --type transactions --data-source local

  # Resolve synced PDF full-text matches to their parent items
  zotio search "calibration feedback" --fulltext --data-source local

  # Search PDF text only inside one collection
  zotio search "calibration feedback" --fulltext --scope collection:ABCD1234 --limit 5

  # JSON output for piping
  zotio search "critical" --json --limit 20`,
		// mcp:read-only is what the capability registry reads to type this
		// command as a read. Without it the registry falls back to
		// operation="other" and the conservative `--group all` gate refuses to
		// fan a plain search across libraries, which is the one read a
		// multi-library user reaches for first. mcp:hidden stays: the MCP
		// surface exposes search through the native framework tool, not the
		// command facade or a mirrored tool.
		Annotations: map[string]string{"mcp:hidden": "true", "mcp:read-only": "true"},
		Args:        cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			query := strings.Join(args, " ")
			if fulltextOnly && resourceType != "" {
				return fmt.Errorf("--fulltext cannot be combined with --type")
			}
			if fulltextOnly && flags.dataSource == "live" {
				return fmt.Errorf("--fulltext requires synced local data; use --data-source local or auto")
			}
			// Cohort membership is an item property. Only --fulltext resolves
			// every hit to a parent item; cross-resource search also returns
			// collections, tags and searches, which have no item membership.
			scopeExpr := strings.TrimSpace(flagScope)
			var scope scopeSpec
			if scopeExpr != "" {
				if !fulltextOnly {
					return usageErr(fmt.Errorf("--scope requires --fulltext: only full-text search resolves every hit to an item"))
				}
				var scopeErr error
				scope, scopeErr = parseScopeSpec(scopeExpr)
				if scopeErr != nil {
					return usageErr(scopeErr)
				}
			}
			// Zotero item full-text search is GET /items
			// with qmode=everything; /searches returns saved-search definitions.
			if !fulltextOnly && flags.dataSource != "local" {
				c, err := flags.newClient()
				if err != nil {
					return err
				}
				params := map[string]string{"q": query, "qmode": "everything"}
				if limit > 0 {
					params["limit"] = fmt.Sprintf("%d", limit)
				}
				// The live attempt goes through resolveRead so this read reports
				// the same provenance envelope (source plus --require-fresh
				// metadata) as every other list read.
				//
				// It is pinned to "live" rather than passing auto through:
				// resolveRead's own auto fallback is the item query planner,
				// which searches items only, while this command's fallback below
				// is cross-resource FTS. Letting auto fall back inside resolveRead
				// would silently narrow `search` to items on the exact invocation
				// where the user cannot see the API is gone.
				liveFlags := *flags
				liveFlags.dataSource = "live"
				data, prov, getErr := resolveRead(cmd.Context(), c, &liveFlags, "items", true, "/items", params, nil)
				if getErr == nil {
					results, err := extractSearchResults(data)
					if err != nil {
						return fmt.Errorf("decoding live search response: %w", err)
					}
					return outputSearchResults(cmd, flags, results, limit, prov)
				}
				// Check if it's a network error for auto-mode fallback
				if flags.dataSource == "live" || !isNetworkError(getErr) {
					return classifyAPIError(getErr, flags)
				}
				// auto mode + network error: fall through to local FTS
				fmt.Fprintf(cmd.ErrOrStderr(), "API unreachable, falling back to local search.\n")
			}

			// Local FTS search. The mirror path is resolved into a local on
			// every invocation and never written back into dbFlag, so a
			// `--group all` re-entry reads its own library: see resolveDBPath.
			dbPath, err := resolveDBPath(dbFlag, "zotio")
			if err != nil {
				return err
			}

			reason := "user_requested"
			if flags.dataSource == "auto" && !fulltextOnly {
				reason = "api_unreachable"
			}
			// The provenance resource names the mirror that answered, so
			// meta.synced_at reports the freshness of the data actually
			// searched. Cross-resource FTS has no single sync checkpoint and
			// keeps the index's own name.
			provenanceResource := "search"
			switch {
			case fulltextOnly:
				provenanceResource = "fulltext"
			case resourceType != "":
				provenanceResource = resourceType
			}

			// Stat before opening. store.OpenWithContext CREATES and migrates
			// the file, so a library that was never synced would gain a mirror
			// as a side effect of being read — and `--group all` fans this
			// command across every accessible group, so one search would leave
			// an empty data-group-<id>.db behind for each of them. A missing
			// mirror returns an empty local answer, but a saved-search scope
			// must still meet its live prerequisite before that early return.
			if _, statErr := os.Stat(dbPath); statErr != nil && os.IsNotExist(statErr) {
				if scope.Type == "saved-search" {
					result, scopeErr := resolveScopeLive(cmd.Context(), flags, localQueryStore{}, scope)
					if scopeErr != nil {
						return scopeErr
					}
					if result.Precondition != "" {
						return scopePreconditionErr(cmd.Context(), cmd.OutOrStdout(), flags, "search", result)
					}
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "No local mirror for this library. Run 'zotio sync' to populate it.\n")
				return outputSearchResults(cmd, flags, make([]json.RawMessage, 0), limit,
					DataProvenance{Source: "local", Reason: reason, ResourceType: provenanceResource, Scoped: true})
			}
			db, err := store.OpenReadOnlyContext(cmd.Context(), dbPath)
			if err != nil {
				return fmt.Errorf("opening local database: %w\nRun 'zotio sync' first to populate the local database.", err)
			}
			defer db.Close()

			// The cohort resolves against this library's own mirror, so a
			// `--group all` run scopes each library by its own keys.
			fulltextSearch := store.FulltextSearch{Query: query, Limit: limit}
			if scopeExpr != "" {
				sel, scopeErr := resolveScopeSelection(cmd, flags, commandRegistryPath(cmd), localQueryStore{Store: db}, scopeExpr)
				if scopeErr != nil {
					return scopeErr
				}
				if sel.restricted() {
					fulltextSearch.ParentKeys = make([]string, 0, len(sel.Keys))
					for key := range sel.Keys {
						fulltextSearch.ParentKeys = append(fulltextSearch.ParentKeys, key)
					}
				}
			}

			results := make([]json.RawMessage, 0)
			switch {
			case fulltextOnly:
				matches, searchErr := db.SearchFulltextContext(cmd.Context(), fulltextSearch)
				if searchErr != nil {
					err = searchErr
					break
				}
				for _, match := range matches {
					raw, marshalErr := json.Marshal(match)
					if marshalErr != nil {
						return fmt.Errorf("encoding full-text search result: %w", marshalErr)
					}
					results = append(results, raw)
				}
			case resourceType == "":
				// Default local search runs cross-resource FTS. Context-bound so
				// a Ctrl+C aborts the SQLite read like the fulltext and MCP paths.
				results, err = db.SearchContext(cmd.Context(), query, limit)
			default:
				results, err = db.SearchByTypeContext(cmd.Context(), query, resourceType, limit)
			}
			if err != nil {
				return fmt.Errorf("search failed: %w", err)
			}

			prov := attachFreshness(localProvenance(db, provenanceResource, reason), flags)
			// FTS applies the request's query, type filter, and limit, so this
			// is a planner-served read rather than the generic dump the Scoped
			// flag exists to distinguish (ADR-0002).
			prov.Scoped = true

			return outputSearchResults(cmd, flags, results, limit, prov)
		},
	}

	cmd.Flags().StringVar(&resourceType, "type", "", "Filter by resource type")
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum results to return")
	cmd.Flags().StringVar(&dbFlag, "db", "", "Database path (default: ~/.local/share/zotio/data.db)")
	cmd.Flags().BoolVar(&fulltextOnly, "fulltext", false, "Search synced PDF full text and return parent item context")
	cmd.Flags().StringVar(&flagScope, "scope", scopeFlagDefaultUnset, scopeFlagUsageDefaultLibrary+"; requires --fulltext")

	return cmd
}

// ResolveFulltextSearchScope resolves the CLI's item-cohort grammar for native
// MCP search. Nil keys mean unrestricted; an empty non-nil slice means no
// members. Saved searches use the same live prerequisite and freshness checks
// as CLI scope consumers. The caller must supply its library's open mirror.
func ResolveFulltextSearchScope(ctx context.Context, db *store.Store, expr string) ([]string, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, nil
	}
	spec, err := parseScopeSpec(expr)
	if err != nil {
		return nil, usageErr(err)
	}
	result, err := resolveScopeLive(ctx, nil, localQueryStore{Store: db}, spec)
	if err != nil {
		return nil, err
	}
	if result.Precondition != "" {
		return nil, scopePreconditionErr(ctx, nil, nil, "search", result)
	}
	if result.All {
		return nil, nil
	}
	return result.Keys, nil
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

	jsonMode := wantsJSONEnvelope(cmd.OutOrStdout(), flags)

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
	data, err := json.Marshal(results)
	if err != nil {
		return err
	}
	// --plain/--csv route through the shared formatter; a human at a terminal
	// gets the same auto table/card rendering as the list commands.
	if !wantsHumanTable(cmd.OutOrStdout(), flags) {
		return printOutputWithFlags(cmd.OutOrStdout(), data, flags)
	}
	var items []map[string]any
	if json.Unmarshal(data, &items) == nil && len(items) > 0 {
		return printAutoTable(cmd.OutOrStdout(), items)
	}
	for _, r := range results {
		fmt.Fprintln(cmd.OutOrStdout(), string(r))
	}
	return nil
}
