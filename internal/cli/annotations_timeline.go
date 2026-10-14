// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func newAnnotationsTimelineCmd(flags *rootFlags) *cobra.Command {
	var flagLimit int
	var flagSince string
	var flagItem string
	var flagCollection string
	var flagScope string
	// bypass the mirror fallback and read live only.
	var refresh bool

	cmd := &cobra.Command{
		Use:   "timeline",
		Short: "List annotations sorted by creation date",
		Long: `List annotations newest first. --item names the top-level item that owns the
annotations, never the attachment that holds them. --collection and --item are
shorthand for --scope collection:KEY and --scope item:KEY.

With --data-source auto (the default) the command reads live and serves the
local mirror when Zotero is unreachable.`,
		Example: `  zotio annotations timeline --limit 50
  zotio annotations timeline --since 2024-01-01
  zotio annotations timeline --item ABCD2345 --json
  zotio annotations timeline --scope "collection:ABCD2345 AND tag:to-read"`,
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateLimitFlag("--limit", flagLimit); err != nil {
				return err
			}
			if refresh && flags.dataSource == "local" {
				return usageErr(fmt.Errorf("--refresh cannot be used with --data-source local"))
			}
			scopeExpr, err := annotationScopeExpr(flagScope,
				scopeSugarFor("collection", "collection", flagCollection),
				scopeSugarFor("item", "item", flagItem))
			if err != nil {
				return err
			}
			if err := validateAnnotationScope(scopeExpr); err != nil {
				return err
			}
			since, hasSince, err := parseAnnotationSince(flagSince)
			if err != nil {
				return err
			}
			readFlags := flags
			if refresh {
				override := *flags
				override.dataSource = "live"
				readFlags = &override
			}
			c, err := flags.newClient()
			if err != nil {
				return err
			}
			reader := newAnnotationReader(cmd, flags, readFlags, c)
			result, err := reader.collect(cmd.Context(), annotationReadQuery{
				Scope:       scopeExpr,
				Limit:       flagLimit,
				Since:       since,
				HasSince:    hasSince,
				NewestFirst: true,
			})
			if err != nil {
				return err
			}
			summaries := make([]annotationSummary, 0, len(result.Rows))
			for _, row := range result.Rows {
				summaries = append(summaries, row.Summary)
			}
			noteEmptyLocalAnnotations(cmd, result.Prov, len(summaries))
			return printCommandJSONEnvelope(cmd.OutOrStdout(), summaries, flags, result.Prov)
		},
	}
	cmd.Flags().IntVar(&flagLimit, "limit", 50, "Maximum number of annotations to return (0 = no limit)")
	cmd.Flags().StringVar(&flagSince, "since", "", "ISO date string; include annotations after this date")
	cmd.Flags().StringVar(&flagItem, "item", "", "Only annotations of this top-level item key (same as --scope item:KEY)")
	cmd.Flags().StringVar(&flagCollection, "collection", "", "Only annotations of items in this collection key (same as --scope collection:KEY)")
	cmd.Flags().StringVar(&flagScope, "scope", scopeFlagDefaultUnset, scopeFlagUsageDefaultLibrary)
	cmd.Flags().BoolVar(&refresh, "refresh", false, "Read live only, with no local mirror fallback (same as --data-source live)")

	return cmd
}

func parseAnnotationSince(value string) (time.Time, bool, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false, nil
	}
	parsed, err := parseZoteroTime(value)
	if err != nil {
		return time.Time{}, false, usageErr(fmt.Errorf("invalid --since value %q: expected ISO date or RFC3339 timestamp", value))
	}
	return parsed, true, nil
}

func parseZoteroTime(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, fmt.Errorf("empty time")
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed, nil
	}
	if parsed, err := time.Parse("2006-01-02", value); err == nil {
		return parsed, nil
	}
	return time.Time{}, fmt.Errorf("invalid time %q", value)
}

// sortAnnotationsByInstantDesc orders annotations newest-first by parsed
// instant. Comparing the raw RFC3339 strings would order by text, which only
// coincides with chronological order while every timestamp shares one offset
// and width; a payload carrying mixed offsets would silently interleave.
// Unparseable timestamps cannot be placed on the timeline at all, so they sort
// last by key rather than being dropped or comparing as the zero instant.
func sortAnnotationsByInstantDesc(annotations []annotationSummary) []annotationSummary {
	return sortByAnnotationInstantDesc(annotations, func(a annotationSummary) annotationSummary { return a })
}

// sortByAnnotationInstantDesc is sortAnnotationsByInstantDesc for any row type
// that carries an annotationSummary.
func sortByAnnotationInstantDesc[T any](rows []T, summary func(T) annotationSummary) []T {
	type keyed struct {
		row   T
		key   string
		added time.Time
		valid bool
	}
	keys := make([]keyed, len(rows))
	for i, row := range rows {
		s := summary(row)
		added, err := parseZoteroTime(s.DateAdded)
		keys[i] = keyed{row: row, key: s.Key, added: added, valid: err == nil}
	}
	sort.SliceStable(keys, func(i, j int) bool {
		if keys[i].valid != keys[j].valid {
			return keys[i].valid
		}
		if keys[i].valid && !keys[i].added.Equal(keys[j].added) {
			return keys[i].added.After(keys[j].added)
		}
		return keys[i].key < keys[j].key
	})
	sorted := make([]T, len(keys))
	for i, k := range keys {
		sorted[i] = k.row
	}
	return sorted
}
