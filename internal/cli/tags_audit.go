// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"zotio/internal/mutation"
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
		Example: `  zotio tags audit
  zotio tags audit --json`,
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			totalTags, plans, ok, err := readTagAuditPlans(cmd)
			if err != nil {
				return err
			}
			if !ok {
				return nil
			}
			if flags.asJSON {
				data, err := json.Marshal(plans)
				if err != nil {
					return err
				}
				jsonFlags := *flags
				jsonFlags.compact = false
				return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), &jsonFlags)
			}
			return printTagAuditReport(cmd, totalTags, plans, flags.dryRun)
		},
	}
	cmd.AddCommand(newTagsAuditFixCmd(flags))
	return cmd
}

func newTagsAuditFixCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "fix",
		Short: "Apply the tag rename plan produced by tags audit",
		Annotations: map[string]string{
			"mcp:read-only":                    "false",
			"zotio:destructive":                "false",
			"zotio:supports-dry-run":           "true",
			"zotio:requires-allow-destructive": "false",
			"zotio:default-max-changes":        "500",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			_, plans, ok, err := readTagAuditPlans(cmd)
			if err != nil {
				return err
			}
			if !ok {
				return nil
			}
			var renameApply func(tagRenameUpdate) (string, any, error)
			rawDB, err := openStoreForRead(cmd.Context(), "zotio")
			if err != nil {
				return fmt.Errorf("opening database: %w", err)
			}
			if rawDB == nil {
				fmt.Fprintln(cmd.OutOrStdout(), "Run 'zotio sync' first.")
				return nil
			}
			defer rawDB.Close()
			ops, err := buildTagAuditFixOps(localQueryStore{rawDB}, plans, func(update tagRenameUpdate) (string, any, error) {
				if renameApply == nil {
					err := fmt.Errorf("write client not initialized")
					return "failed", err.Error(), err
				}
				return renameApply(update)
			})
			if err != nil {
				return err
			}

			if resolveMutationMode(flags).Apply && len(ops) > 0 {
				c, err := flags.newWriteClient()
				if err != nil {
					return err
				}
				renameApply = func(update tagRenameUpdate) (string, any, error) {
					return applyTagRenameUpdate(c, update)
				}
			}

			env, runErr := runMutation(cmd.Context(), flags, "tags.audit.fix", ops)
			renderErr := renderMutation(cmd, flags, env, func(env mutation.Envelope) string {
				action := "would fix"
				if env.Mode == "apply" {
					action = "fixed"
				}
				return fmt.Sprintf("%s %d tag item write(s)", action, env.Plan.Summary.Planned)
			})
			if renderErr != nil {
				return renderErr
			}
			return runErr
		},
	}
	return cmd
}

// tagAuditDistinctQuery and tagAuditCountQuery enumerate library tags and their
// item counts. They are shared by `tags audit` and the `library health`
// tag-drift check so the two never drift.
const tagAuditDistinctQuery = `
SELECT DISTINCT json_extract(tags.value, '$.tag') AS tag_name
FROM resources, json_each(json_extract(data, '$.data.tags')) AS tags
WHERE resource_type = 'items' AND tag_name IS NOT NULL AND tag_name != ''`

const tagAuditCountQuery = `
SELECT json_extract(tags.value, '$.tag') AS tag_name, COUNT(*) AS item_count
FROM resources, json_each(json_extract(data, '$.data.tags')) AS tags
WHERE resource_type = 'items' AND tag_name IS NOT NULL AND tag_name != ''
GROUP BY tag_name ORDER BY item_count DESC`

func readTagAuditPlans(cmd *cobra.Command) (int, []tagAuditPlan, bool, error) {
	rawDB, err := openStoreForRead(cmd.Context(), "zotio")
	if err != nil {
		return 0, nil, false, fmt.Errorf("opening database: %w", err)
	}
	if rawDB == nil {
		fmt.Fprintln(cmd.OutOrStdout(), "Run 'zotio sync' first.")
		return 0, nil, false, nil
	}
	defer rawDB.Close()
	db := localQueryStore{rawDB}

	tagRows, err := db.QueryRaw(tagAuditDistinctQuery)
	if err != nil {
		return 0, nil, false, fmt.Errorf("querying tags: %w", err)
	}
	countRows, err := db.QueryRaw(tagAuditCountQuery)
	if err != nil {
		return 0, nil, false, fmt.Errorf("querying tag counts: %w", err)
	}

	return len(tagRows), buildTagAuditPlans(tagRows, countRows), true, nil
}

const tagAuditAliasItemsQuery = `
SELECT data
FROM resources r
WHERE r.resource_type = 'items'
	AND EXISTS (
		SELECT 1
		FROM json_each(json_extract(r.data, '$.data.tags')) AS tags
		WHERE json_extract(tags.value, '$.tag') = ?
	)
ORDER BY r.id ASC`

func buildTagAuditFixOps(db localQueryStore, plans []tagAuditPlan, apply func(tagRenameUpdate) (string, any, error)) ([]mutation.Op, error) {
	ops := make([]mutation.Op, 0)
	for _, plan := range plans {
		canonical := plan.Canonical
		for _, alias := range plan.Aliases {
			updates, err := tagAuditFixUpdates(db, alias, canonical)
			if err != nil {
				return nil, fmt.Errorf("planning tag audit fix for %q: %w", alias, err)
			}
			for _, update := range updates {
				update := update
				alias := alias
				op := mutation.Op{
					ID:              "tags.audit.fix:" + alias + ":" + update.key,
					Key:             update.key,
					Kind:            "tag_rename",
					ExpectedVersion: mutationExpectedVersion(update.version),
					Changes:         []mutation.Change{{Field: "tag", Remove: alias, Add: canonical}},
					Destructive:     false,
					Apply: func() (string, any, error) {
						return apply(update)
					},
				}
				ops = append(ops, op)
			}
		}
	}
	return ops, nil
}

func tagAuditFixUpdates(db localQueryStore, alias, canonical string) ([]tagRenameUpdate, error) {
	rows, err := db.QueryRaw(tagAuditAliasItemsQuery, alias)
	if err != nil {
		return nil, err
	}
	items := make([]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		raw := sqlStringValue(row["data"])
		if raw == "" {
			continue
		}
		items = append(items, json.RawMessage(raw))
	}
	data, err := json.Marshal(items)
	if err != nil {
		return nil, err
	}
	return buildTagRenameUpdates(data, alias, canonical)
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
			// Single-quote generated shell arguments and render line breaks inert.
			commands = append(commands, fmt.Sprintf(
				`zotio tags rename --from %s --to %s`,
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
	value = strings.ReplaceAll(value, "\r", `\r`)
	value = strings.ReplaceAll(value, "\n", `\n`)
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func printTagAuditReport(cmd *cobra.Command, totalTags int, plans []tagAuditPlan, dryRun bool) error {
	summaryTitle := "Summary"
	if dryRun {
		summaryTitle += " (dry run)"
	}
	fmt.Fprintln(cmd.OutOrStdout(), bold(summaryTitle))
	fmt.Fprintf(cmd.OutOrStdout(), "%s  %d\n", dim("total tags:"), totalTags)
	fmt.Fprintf(cmd.OutOrStdout(), "%s  %d\n\n", dim("duplicate groups:"), len(plans))
	fmt.Fprintln(cmd.OutOrStdout(), bold("Merge plan"))
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
