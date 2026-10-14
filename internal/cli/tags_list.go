// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"zotio/internal/client"
)

func newTagsListCmd(flags *rootFlags) *cobra.Command {
	var flagLimit int
	var flagStart int
	var flagQ string
	var flagQMode string

	cmd := &cobra.Command{
		Use:         "list",
		Short:       "List all tags in the library",
		Example:     "  zotio tags list --query machine --limit 20",
		Annotations: map[string]string{"zotio:endpoint": "tags.list", "zotio:method": "GET", "zotio:path": "/tags", "mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := flags.newClient()
			if err != nil {
				return err
			}

			path := "/tags"
			if err := validateLimitFlag("--limit", flagLimit); err != nil {
				return err
			}
			if flagStart < 0 {
				return usageErr(fmt.Errorf("invalid --start %d: must be 0 or greater", flagStart))
			}
			params, err := tagListParams(flagLimit, flagStart, flagQ, flagQMode)
			if err != nil {
				return usageErr(err)
			}
			data, prov, err := resolveRead(cmd.Context(), c, flags, "tags", false, path, params, nil)
			if err != nil {
				return classifyAPIError(err, flags)
			}
			// The live Zotero API ignores ?q=, so filter client-side over every
			// tag and then apply --start/--limit to the filtered list.
			if flagQ != "" && prov.Source == "live" {
				need := 0
				if flagLimit > 0 {
					need = flagStart + flagLimit
				}
				data, err = fetchLiveTagsMatching(c, flagQ, flagQMode, need)
				if err != nil {
					return classifyAPIError(err, flags)
				}
				data = windowJSONArray(data, flagStart, flagLimit)
			}
			// Honor --limit when the API accepts but ignores ?limit=N.
			data = truncateJSONArray(data, flagLimit)
			// Print provenance to stderr for human-facing output
			printProvenance(cmd, countResultItems(data), prov)
			// For JSON output, wrap with provenance envelope before passing through flags.
			// --select wins over --compact when both are set; --compact only runs when
			// no explicit fields were requested.
			if wantsJSONEnvelope(cmd.OutOrStdout(), flags) {
				filtered, err := applyFieldFlags(data, flags)
				if err != nil {
					return err
				}
				wrapped, wrapErr := wrapWithProvenance(filtered, prov)
				if wrapErr != nil {
					return wrapErr
				}
				return printOutput(cmd.OutOrStdout(), wrapped, true)
			}
			// For all other output modes (table, csv, plain, quiet), use the standard pipeline
			//
			// Zotero's raw /tags response nests type and numItems under "meta",
			// which --plain/table treat as a structural wrapper and drop (see
			// plainStructuralFields), so a manual and an automatic instance of the
			// same tag name rendered as identical, inexplicable duplicate rows
			// (tags list counted 793 rows for the 786 distinct names tags audit
			// reports — 7 tags exist as both). Promote them to columns for the
			// human paths only; JSON callers already have meta.type.
			data = flattenTagMetaForDisplay(data)
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
	cmd.Flags().IntVar(&flagLimit, "limit", 0, "Maximum number of tags to return (0 = no limit)")
	cmd.Flags().IntVar(&flagStart, "start", 0, "Pagination offset")
	cmd.Flags().StringVar(&flagQ, "query", "", "Filter tags by name substring")
	cmd.Flags().StringVar(&flagQMode, "query-mode", "contains", "Tag query mode: contains or startsWith")

	return cmd
}

// filterLiveTagsByQuery applies the --query substring filter client-side,
// because the Zotero API ignores q on /tags. Matching is case-insensitive;
// startsWith mode matches name prefixes.
func filterLiveTagsByQuery(data json.RawMessage, query, queryMode string) (json.RawMessage, error) {
	var tags []json.RawMessage
	if err := json.Unmarshal(data, &tags); err != nil {
		return data, nil
	}
	needle := strings.ToLower(query)
	kept := make([]json.RawMessage, 0, len(tags))
	for _, raw := range tags {
		var tag struct {
			Tag string `json:"tag"`
		}
		if err := json.Unmarshal(raw, &tag); err != nil {
			continue
		}
		name := strings.ToLower(tag.Tag)
		if (strings.TrimSpace(queryMode) == "startsWith" && strings.HasPrefix(name, needle)) ||
			(strings.TrimSpace(queryMode) != "startsWith" && strings.Contains(name, needle)) {
			kept = append(kept, raw)
		}
	}
	return json.Marshal(kept)
}

// fetchLiveTagsMatching pages through /tags, filtering each page client-side
// as it arrives. With need > 0 it stops as soon as that many matches have
// been collected; need == 0 reads every page.
func fetchLiveTagsMatching(c *client.Client, query, queryMode string, need int) (json.RawMessage, error) {
	const pageSize = 100
	var matched []json.RawMessage
	for start := 0; ; start += pageSize {
		// Send q/qmode too: the Web API honors them, the local API ignores
		// them, and the client-side filter below is correct for both.
		mode := strings.TrimSpace(queryMode)
		if mode == "" {
			mode = "contains"
		}
		page, err := c.Get("/tags", map[string]string{
			"limit": strconv.Itoa(pageSize), "start": strconv.Itoa(start),
			"q": query, "qmode": mode,
		})
		if err != nil {
			return nil, err
		}
		var rows []json.RawMessage
		if err := json.Unmarshal(page, &rows); err != nil {
			return nil, fmt.Errorf("parsing tags page: %w", err)
		}
		kept, err := filterLiveTagsByQuery(page, query, queryMode)
		if err != nil {
			return nil, err
		}
		var keptRows []json.RawMessage
		if err := json.Unmarshal(kept, &keptRows); err != nil {
			return nil, err
		}
		matched = append(matched, keptRows...)
		if need > 0 && len(matched) >= need {
			break
		}
		if len(rows) < pageSize {
			break
		}
	}
	if matched == nil {
		matched = []json.RawMessage{}
	}
	return json.Marshal(matched)
}

// windowJSONArray applies start then limit (0 = no limit) to a JSON array.
func windowJSONArray(data json.RawMessage, start, limit int) json.RawMessage {
	if start > 0 {
		var rows []json.RawMessage
		if err := json.Unmarshal(data, &rows); err != nil {
			return data
		}
		if start > len(rows) {
			start = len(rows)
		}
		out, err := json.Marshal(rows[start:])
		if err != nil {
			return data
		}
		data = out
	}
	return truncateJSONArray(data, limit)
}

func tagListParams(limit, start int, query, queryMode string) (map[string]string, error) {
	queryMode = strings.TrimSpace(queryMode)
	if queryMode == "" {
		queryMode = "contains"
	}
	if queryMode != "contains" && queryMode != "startsWith" {
		return nil, fmt.Errorf("invalid --query-mode %q: must be contains or startsWith", queryMode)
	}
	params := map[string]string{}
	if limit != 0 {
		params["limit"] = strconv.Itoa(limit)
	}
	if start != 0 {
		params["start"] = strconv.Itoa(start)
	}
	if query != "" {
		params["q"] = query
		params["qmode"] = queryMode
	}
	return params, nil
}

// flattenTagMetaForDisplay promotes meta.type and meta.numItems to top-level
// fields so the shared plain/table renderer, which drops "meta" as a
// structural wrapper, can still show them as columns. Non-object entries and
// entries with no meta pass through unchanged.
func flattenTagMetaForDisplay(data json.RawMessage) json.RawMessage {
	var tags []map[string]any
	if err := json.Unmarshal(data, &tags); err != nil {
		return data
	}
	changed := false
	for _, tag := range tags {
		meta, ok := tag["meta"].(map[string]any)
		if !ok {
			continue
		}
		if tagType, ok := meta["type"]; ok {
			tag["type"] = tagType
			changed = true
		}
		if numItems, ok := meta["numItems"]; ok {
			tag["num_items"] = numItems
			changed = true
		}
	}
	if !changed {
		return data
	}
	flattened, err := json.Marshal(tags)
	if err != nil {
		return data
	}
	return flattened
}
