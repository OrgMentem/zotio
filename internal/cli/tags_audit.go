// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/spf13/cobra"

	"zotio/internal/mutation"
)

// tagAuditPrefer selects the canonical spelling within a duplicate tag group.
// "frequency" (the default, and the only policy in effect before --prefer
// existed) picks whichever spelling carries the most items; the case
// policies instead rewrite the group's normalized name into a single
// consistent convention. See buildTagAuditPlans for the automatic-tag
// carve-out that keeps a case policy from mangling MeSH-style imports.
type tagAuditPrefer string

const (
	tagAuditPreferFrequency tagAuditPrefer = "frequency"
	tagAuditPreferSentence  tagAuditPrefer = "sentence"
	tagAuditPreferTitle     tagAuditPrefer = "title"
	tagAuditPreferLower     tagAuditPrefer = "lower"
)

func parseTagAuditPrefer(value string) (tagAuditPrefer, error) {
	switch tagAuditPrefer(value) {
	case tagAuditPreferFrequency, tagAuditPreferSentence, tagAuditPreferTitle, tagAuditPreferLower:
		return tagAuditPrefer(value), nil
	default:
		return "", fmt.Errorf("invalid --prefer %q: want one of frequency, sentence, title, lower", value)
	}
}

type tagAuditPlan struct {
	Canonical      string   `json:"canonical"`
	Aliases        []string `json:"aliases"`
	TotalItems     int      `json:"total_items"`
	RenameCommands []string `json:"rename_commands"`
	// AutomaticSkipped is true when a non-frequency --prefer policy could not
	// be applied to this group because at least one of its variants carries
	// Zotero's automatic tag type (type: 1) on some item -- e.g. a
	// MeSH-derived term imported by a translator, where the library's Title
	// Case is already correct and a blanket case rewrite would mangle it.
	// The group falls back to the frequency policy instead, and this flag
	// surfaces that so the caller doesn't silently disagree with --prefer.
	AutomaticSkipped bool `json:"automatic_skipped,omitempty"`
}

type countedTag struct {
	name  string
	count int
}

func newTagsAuditCmd(flags *rootFlags) *cobra.Command {
	var prefer string
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Audit tags for case and spacing drift",
		Example: `  zotio tags audit
  zotio tags audit --json
  zotio tags audit --prefer title`,
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			policy, err := parseTagAuditPrefer(prefer)
			if err != nil {
				return err
			}
			totalTags, plans, ok, prov, err := readTagAuditPlans(cmd, policy)
			if err != nil {
				return err
			}
			if !ok {
				return nil
			}
			if flags.asJSON && wantsJSONEnvelope(cmd.OutOrStdout(), flags) {
				data, err := json.Marshal(plans)
				if err != nil {
					return err
				}
				if flags.selectFields != "" {
					data = filterFields(data, flags.selectFields)
				}
				wrapped, err := wrapWithProvenance(json.RawMessage(data), prov)
				if err != nil {
					return err
				}
				return printOutput(cmd.OutOrStdout(), wrapped, true)
			}
			return printTagAuditReport(cmd, totalTags, plans, policy, flags.dryRun)
		},
	}
	cmd.Flags().StringVar(&prefer, "prefer", string(tagAuditPreferFrequency),
		"Canonical spelling policy for duplicate groups: frequency (default, most-used spelling wins), sentence, title, or lower. Automatic (type 1) tags always keep the frequency canonical to avoid mangling MeSH-style imports.")
	cmd.AddCommand(newTagsAuditFixCmd(flags))
	return cmd
}

func newTagsAuditFixCmd(flags *rootFlags) *cobra.Command {
	var prefer string
	cmd := &cobra.Command{
		Use:   "fix",
		Short: "Apply the tag rename plan produced by tags audit",
		Example: `  zotio tags audit fix --yes
  zotio tags audit fix --prefer title --yes`,
		Annotations: map[string]string{
			"mcp:read-only":                    "false",
			"zotio:destructive":                "false",
			"zotio:supports-dry-run":           "true",
			"zotio:requires-allow-destructive": "false",
			"zotio:default-max-changes":        "500",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			policy, err := parseTagAuditPrefer(prefer)
			if err != nil {
				return err
			}
			_, plans, ok, _, err := readTagAuditPlans(cmd, policy)
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
	cmd.Flags().StringVar(&prefer, "prefer", string(tagAuditPreferFrequency),
		"Canonical spelling policy for duplicate groups: frequency, sentence, title, or lower. Must match the --prefer used for `tags audit` to apply the same targets shown there.")
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

// tagAuditAutomaticQuery names every distinct tag that carries Zotero's
// automatic type (type: 1) on at least one item. A non-frequency --prefer
// policy skips any duplicate group containing one of these names: automatic
// tags are typically translator/MeSH imports where the source casing (often
// Title Case) is the correct one, and a blanket case rewrite would corrupt
// it. See buildTagAuditPlans.
const tagAuditAutomaticQuery = `
SELECT DISTINCT json_extract(tags.value, '$.tag') AS tag_name
FROM resources, json_each(json_extract(data, '$.data.tags')) AS tags
WHERE resource_type = 'items' AND tag_name IS NOT NULL AND tag_name != ''
	AND CAST(json_extract(tags.value, '$.type') AS INTEGER) = 1`

func readTagAuditPlans(cmd *cobra.Command, prefer tagAuditPrefer) (int, []tagAuditPlan, bool, DataProvenance, error) {
	rawDB, err := openStoreForRead(cmd.Context(), "zotio")
	if err != nil {
		return 0, nil, false, DataProvenance{}, fmt.Errorf("opening database: %w", err)
	}
	if rawDB == nil {
		fmt.Fprintln(cmd.OutOrStdout(), "Run 'zotio sync' first.")
		return 0, nil, false, DataProvenance{}, nil
	}
	defer rawDB.Close()
	prov := localProvenance(rawDB, "tags", "local_only")
	db := localQueryStore{rawDB}

	tagRows, err := db.QueryRaw(tagAuditDistinctQuery)
	if err != nil {
		return 0, nil, false, DataProvenance{}, fmt.Errorf("querying tags: %w", err)
	}
	countRows, err := db.QueryRaw(tagAuditCountQuery)
	if err != nil {
		return 0, nil, false, DataProvenance{}, fmt.Errorf("querying tag counts: %w", err)
	}
	automaticTags := map[string]bool{}
	if prefer != tagAuditPreferFrequency {
		automaticRows, err := db.QueryRaw(tagAuditAutomaticQuery)
		if err != nil {
			return 0, nil, false, DataProvenance{}, fmt.Errorf("querying automatic tags: %w", err)
		}
		for _, row := range automaticRows {
			if name := sqlStringValue(row["tag_name"]); name != "" {
				automaticTags[name] = true
			}
		}
	}

	return len(tagRows), buildTagAuditPlans(tagRows, countRows, prefer, automaticTags), true, prov, nil
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
					Changes:         []mutation.Change{{Field: "tags", Remove: alias, Add: canonical, TagType: update.tagType}},
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

func buildTagAuditPlans(tagRows, countRows []map[string]any, prefer tagAuditPrefer, automaticTags map[string]bool) []tagAuditPlan {
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
	for normalized, tags := range groups {
		if len(tags) <= 1 {
			continue
		}
		sort.Slice(tags, func(i, j int) bool {
			if tags[i].count != tags[j].count {
				return tags[i].count > tags[j].count
			}
			return tags[i].name < tags[j].name
		})

		// Frequency is always the fallback: it is the only policy applied
		// when --prefer is left at its default, and it is also what a
		// non-frequency policy falls back to for a group carrying an
		// automatic (type 1) tag, so the plan never mangles a MeSH-style
		// import into some other case convention.
		effectivePrefer := prefer
		automaticSkipped := false
		if prefer != tagAuditPreferFrequency {
			for _, tag := range tags {
				if automaticTags[tag.name] {
					automaticSkipped = true
					effectivePrefer = tagAuditPreferFrequency
					break
				}
			}
		}

		canonical := tagAuditCanonicalName(effectivePrefer, normalized, tags)
		aliases := make([]string, 0, len(tags)-1)
		commands := make([]string, 0, len(tags))
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
			Canonical:        canonical,
			Aliases:          aliases,
			TotalItems:       total,
			RenameCommands:   commands,
			AutomaticSkipped: automaticSkipped,
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

// tagAuditCanonicalName picks the group's canonical spelling for the given
// (already-resolved) policy. Frequency keeps today's behavior exactly: the
// most-used spelling, tied broken alphabetically by sort.Slice above. The
// case policies instead rewrite the group's normalized name (lowercase,
// single-spaced) into one consistent convention, independent of which
// spelling happens to be most common -- that's the point of --prefer: one
// convention across the whole library, not a per-group popularity contest.
func tagAuditCanonicalName(prefer tagAuditPrefer, normalized string, tags []countedTag) string {
	switch prefer {
	case tagAuditPreferSentence:
		return tagAuditSentenceCase(normalized)
	case tagAuditPreferTitle:
		return tagAuditTitleCase(normalized)
	case tagAuditPreferLower:
		return normalized
	default:
		return tags[0].name
	}
}

func tagAuditSentenceCase(normalized string) string {
	r := []rune(normalized)
	if len(r) == 0 {
		return normalized
	}
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

func tagAuditTitleCase(normalized string) string {
	words := strings.Fields(normalized)
	for i, word := range words {
		r := []rune(word)
		if len(r) == 0 {
			continue
		}
		r[0] = unicode.ToUpper(r[0])
		for j := 1; j < len(r); j++ {
			if tagAuditTitleBoundary(r, j-1) {
				r[j] = unicode.ToUpper(r[j])
			}
		}
		words[i] = string(r)
	}
	return strings.Join(words, " ")
}

// tagAuditTitleBoundary identifies punctuation that conventionally starts a
// new word inside a title-case token. An apostrophe starts a word only for
// one-letter prefixes such as O'Brien; ordinary contractions such as don't
// remain a single word.
func tagAuditTitleBoundary(r []rune, separator int) bool {
	switch r[separator] {
	case '/', '-':
		return true
	case '\'', '’':
		return separator == 1 && unicode.IsLetter(r[0])
	default:
		return unicode.Is(unicode.Dash, r[separator])
	}
}

func normalizeTagAuditName(tag string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(tag)), " "))
}

func quoteTagAuditCommandArg(value string) string {
	value = strings.ReplaceAll(value, "\r", `\r`)
	value = strings.ReplaceAll(value, "\n", `\n`)
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func printTagAuditReport(cmd *cobra.Command, totalTags int, plans []tagAuditPlan, prefer tagAuditPrefer, dryRun bool) error {
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
	fixCommand := "zotio tags audit fix --yes"
	if prefer != tagAuditPreferFrequency {
		fixCommand += " --prefer " + string(prefer)
	}
	fmt.Fprintf(cmd.OutOrStdout(),
		"Run %s to apply every rename below in one batch (prepend --dry-run to preview first). The individual commands below remain a manual escape hatch for renaming groups one at a time.\n\n",
		bold(fixCommand))
	for _, plan := range plans {
		if plan.AutomaticSkipped {
			fmt.Fprintf(cmd.OutOrStdout(), "%s\n", dim(fmt.Sprintf(
				"# %s carries an automatic (type 1) tag; kept the frequency canonical instead of --prefer %s",
				plan.Canonical, prefer)))
		}
		for _, command := range plan.RenameCommands {
			fmt.Fprintln(cmd.OutOrStdout(), command)
		}
	}
	return nil
}
