// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH: Add hand-written local tag drift audit missing from the generated CLI.

package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

type tagAuditPlan struct {
	Canonical      string   `json:"canonical"`
	Aliases        []string `json:"aliases"`
	TotalItems     int      `json:"total_items"`
	RenameCommands []string `json:"rename_commands"`
}

type countedTag struct {
	name  string
	count int
}

func newTagsAuditCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Audit tags for case and spacing drift",
		Example: `  zotero-pp-cli tags audit
  zotero-pp-cli tags audit --json`,
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			rawDB, err := openStoreForRead(cmd.Context(), "zotero-pp-cli")
			if err != nil {
				return fmt.Errorf("opening database: %w", err)
			}
			if rawDB == nil {
				fmt.Fprintln(cmd.OutOrStdout(), "Run 'zotero-pp-cli sync' first.")
				return nil
			}
			defer rawDB.Close()
			db := localQueryStore{rawDB}

			tagRows, err := db.QueryRaw(`
SELECT DISTINCT json_extract(tags.value, '$.tag') AS tag_name
FROM resources, json_each(json_extract(data, '$.data.tags')) AS tags
WHERE resource_type = 'items' AND tag_name IS NOT NULL AND tag_name != ''`)
			if err != nil {
				return fmt.Errorf("querying tags: %w", err)
			}
			countRows, err := db.QueryRaw(`
SELECT json_extract(tags.value, '$.tag') AS tag_name, COUNT(*) AS item_count
FROM resources, json_each(json_extract(data, '$.data.tags')) AS tags
WHERE resource_type = 'items' AND tag_name IS NOT NULL AND tag_name != ''
GROUP BY tag_name ORDER BY item_count DESC`)
			if err != nil {
				return fmt.Errorf("querying tag counts: %w", err)
			}

			plans := buildTagAuditPlans(tagRows, countRows)
			if flags.asJSON {
				data, err := json.Marshal(plans)
				if err != nil {
					return err
				}
				jsonFlags := *flags
				jsonFlags.compact = false
				return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), &jsonFlags)
			}
			return printTagAuditReport(cmd, len(tagRows), plans, flags.dryRun)
		},
	}
	return cmd
}

func buildTagAuditPlans(tagRows, countRows []map[string]any) []tagAuditPlan {
	counts := make(map[string]int, len(countRows))
	for _, row := range countRows {
		name := sqlStringValue(row["tag_name"])
		if name == "" {
			continue
		}
		counts[name] = sqlIntValue(row["item_count"])
	}

	groups := make(map[string][]countedTag)
	seen := make(map[string]bool, len(tagRows))
	for _, row := range tagRows {
		name := sqlStringValue(row["tag_name"])
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		normalized := normalizeTagAuditName(name)
		if normalized == "" {
			continue
		}
		groups[normalized] = append(groups[normalized], countedTag{name: name, count: counts[name]})
	}

	plans := make([]tagAuditPlan, 0)
	for _, tags := range groups {
		if len(tags) <= 1 {
			continue
		}
		sort.Slice(tags, func(i, j int) bool {
			if tags[i].count != tags[j].count {
				return tags[i].count > tags[j].count
			}
			return tags[i].name < tags[j].name
		})
		canonical := tags[0].name
		aliases := make([]string, 0, len(tags)-1)
		commands := make([]string, 0, len(tags)-1)
		total := 0
		for _, tag := range tags {
			total += tag.count
			if tag.name == canonical {
				continue
			}
			aliases = append(aliases, tag.name)
			commands = append(commands, fmt.Sprintf(
				`zotero-pp-cli tags rename --from "%s" --to "%s"`,
				quoteTagAuditCommandArg(tag.name),
				quoteTagAuditCommandArg(canonical),
			))
		}
		plans = append(plans, tagAuditPlan{
			Canonical:      canonical,
			Aliases:        aliases,
			TotalItems:     total,
			RenameCommands: commands,
		})
	}

	sort.Slice(plans, func(i, j int) bool {
		if plans[i].TotalItems != plans[j].TotalItems {
			return plans[i].TotalItems > plans[j].TotalItems
		}
		return plans[i].Canonical < plans[j].Canonical
	})
	return plans
}

func normalizeTagAuditName(tag string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(tag)), " "))
}

func quoteTagAuditCommandArg(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`, "`", "\\`")
	return replacer.Replace(value)
}

func printTagAuditReport(cmd *cobra.Command, totalTags int, plans []tagAuditPlan, dryRun bool) error {
	summaryTitle := "## Summary"
	if dryRun {
		summaryTitle += " (dry run)"
	}
	fmt.Fprintln(cmd.OutOrStdout(), summaryTitle)
	fmt.Fprintf(cmd.OutOrStdout(), "Total tags: %d\n", totalTags)
	fmt.Fprintf(cmd.OutOrStdout(), "Duplicate groups: %d\n\n", len(plans))
	fmt.Fprintln(cmd.OutOrStdout(), "## Merge plan")
	if len(plans) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No duplicate tag groups found.")
		return nil
	}
	for _, plan := range plans {
		for _, command := range plan.RenameCommands {
			fmt.Fprintln(cmd.OutOrStdout(), command)
		}
	}
	return nil
}
