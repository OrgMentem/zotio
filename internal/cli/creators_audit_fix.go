// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"zotio/internal/client"
	"zotio/internal/mutation"
)

type creatorAuditFixPlan struct {
	alias     string
	canonical string
	itemKeys  map[string]bool
	tier      creatorVariantTier
}

type creatorRenameUpdate struct {
	key string
	// renames is the alias->canonical display-name mapping to apply to this
	// item's creators array. Both `creators audit fix` (possibly several
	// aliases folded into one item write) and `creators rename` (always a
	// single alias) populate it identically so both share the write-plane
	// apply path below.
	renames map[string]string
	// version is the read plane's object version, recorded for the plan only.
	// The write precondition is resolved from the write plane at apply time
	// (see applyCreatorRenameUpdate) because the two planes do not share a
	// version space.
	version any
	// changes describes the rename(s) for the plan preview, computed from the
	// read plane at plan time. The write itself recomputes the creators array
	// from a fresh write-plane read rather than trusting this.
	changes []mutation.Change
}

func newCreatorsAuditFixCmd(flags *rootFlags) *cobra.Command {
	var flagScope string
	var flagMaps []string

	cmd := &cobra.Command{
		Use:   "fix",
		Short: "Apply safe creator variant renames from creators audit",
		Long: `Apply safe creator variant renames from creators audit through the shared
mutation preview/apply envelope.

By default this previews item writes without sending network mutations. Pass --yes
to apply. Tier-1 exact-normalized groups are planned automatically. Tier-2
initial/full-name groups are planned only for explicit repeatable mappings such
as --map "J. Smith=John Smith". Tier-3 ambiguous groups are never planned.

Creator renames PATCH each affected item with its full creators array and a
version precondition, preserving creatorType, creator order, and unrelated
creators. Applied creator renames are journaled for audit history, but journal
undo does not reverse them because ordered creator-array inverses are not
supported.`,
		Example: `  zotio creators audit fix
  zotio creators audit fix --map "J. Smith=John Smith"
  zotio creators audit fix --scope collection:ABCD1234 --yes`,
		Annotations: map[string]string{
			"zotio:endpoint":                   "creators.audit.fix",
			"zotio:method":                     "PATCH",
			"zotio:path":                       "/items/{itemKey}",
			"mcp:read-only":                    "false",
			"zotio:destructive":                "false",
			"zotio:supports-dry-run":           "true",
			"zotio:requires-allow-destructive": "false",
			"zotio:default-max-changes":        "500",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			explicitMaps, err := parseCreatorAuditFixMaps(flagMaps)
			if err != nil {
				return usageErr(err)
			}

			rawDB, err := openStoreForRead(cmd.Context(), "zotio")
			if err != nil {
				return fmt.Errorf("opening local database: %w", err)
			}
			if rawDB == nil {
				fmt.Fprintln(cmd.OutOrStdout(), "Run 'zotio sync' first.")
				return nil
			}
			defer rawDB.Close()
			db := localQueryStore{rawDB}

			spec, err := parseScopeSpec(flagScope)
			if err != nil {
				return usageErr(err)
			}
			scope, err := resolveScope(db, spec)
			if err != nil {
				return err
			}
			if scope.Precondition != "" {
				return preconditionErr(fmt.Errorf("%s required for scope %s", scope.Precondition, scope.Expr))
			}

			occurrences, err := queryCreatorAuditOccurrences(db, scope)
			if err != nil {
				return fmt.Errorf("querying creators: %w", err)
			}
			groups := buildCreatorVariantGroups(occurrences)
			plans, err := buildCreatorAuditFixPlans(groups, explicitMaps)
			if err != nil {
				return usageErr(err)
			}

			var renameApply func(creatorRenameUpdate) (string, any, error)
			ops, err := buildCreatorAuditFixOps(db, plans, func(update creatorRenameUpdate) (string, any, error) {
				if renameApply == nil {
					err := errors.New("write client not initialized")
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
				renameApply = func(update creatorRenameUpdate) (string, any, error) {
					return applyCreatorRenameUpdate(c, update)
				}
			}

			env, runErr := runMutation(cmd.Context(), flags, "creators.audit.fix", ops)
			renderErr := renderMutation(cmd, flags, env, func(env mutation.Envelope) string {
				action := "would fix"
				if env.Mode == "apply" {
					action = "fixed"
				}
				return fmt.Sprintf("%s %d creator item write(s)", action, env.Plan.Summary.Planned)
			})
			if renderErr != nil {
				return renderErr
			}
			return runErr
		},
	}
	cmd.Flags().StringVar(&flagScope, "scope", "library", "Item scope: library, collection:<key>, tag:<tag>, item:<key>, or query:<text>")
	cmd.Flags().StringArrayVar(&flagMaps, "map", nil, "Tier-2 alias mapping alias=canonical; repeatable")
	return cmd
}

func parseCreatorAuditFixMaps(values []string) (map[string]string, error) {
	mappings := make(map[string]string, len(values))
	for _, raw := range values {
		left, right, ok := strings.Cut(raw, "=")
		if !ok {
			return nil, fmt.Errorf("--map must be alias=canonical, got %q", raw)
		}
		alias := collapseCreatorWhitespace(left)
		canonical := collapseCreatorWhitespace(right)
		if alias == "" || canonical == "" {
			return nil, fmt.Errorf("--map must have non-empty alias and canonical names, got %q", raw)
		}
		if alias == canonical {
			return nil, fmt.Errorf("--map %q does not rename anything", raw)
		}
		if _, exists := mappings[alias]; exists {
			return nil, fmt.Errorf("duplicate --map alias %q", alias)
		}
		mappings[alias] = canonical
	}
	return mappings, nil
}

func buildCreatorAuditFixPlans(groups []creatorVariantGroup, explicitMaps map[string]string) ([]creatorAuditFixPlan, error) {
	plansByAlias := make(map[string]creatorAuditFixPlan)
	tier2Aliases := make(map[string]bool)

	for _, group := range groups {
		switch group.Tier {
		case creatorVariantTierExact:
			for _, member := range group.members {
				if member.Name == group.Canonical {
					continue
				}
				addCreatorAuditFixPlan(plansByAlias, creatorAuditFixPlan{
					alias:     member.Name,
					canonical: group.Canonical,
					itemKeys:  creatorVariantItemKeySet(member),
					tier:      group.Tier,
				})
			}
		case creatorVariantTierInitials:
			for _, member := range group.members {
				if member.Name == group.Canonical {
					continue
				}
				tier2Aliases[member.Name] = true
			}
		}
	}

	for alias, canonical := range explicitMaps {
		if !tier2Aliases[alias] {
			return nil, fmt.Errorf("--map alias %q was not found in any tier-2 creator_variant_initials group", alias)
		}
		if existing, exists := plansByAlias[alias]; exists {
			if existing.canonical != canonical {
				return nil, fmt.Errorf("--map alias %q conflicts with automatic tier-1 rename to %q", alias, existing.canonical)
			}
			continue
		}
		for _, group := range groups {
			if group.Tier != creatorVariantTierInitials {
				continue
			}
			for _, member := range group.members {
				if member.Name != alias {
					continue
				}
				addCreatorAuditFixPlan(plansByAlias, creatorAuditFixPlan{
					alias:     alias,
					canonical: canonical,
					itemKeys:  creatorVariantItemKeySet(member),
					tier:      group.Tier,
				})
			}
		}
	}

	aliases := make([]string, 0, len(plansByAlias))
	for alias := range plansByAlias {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	plans := make([]creatorAuditFixPlan, 0, len(aliases))
	for _, alias := range aliases {
		plans = append(plans, plansByAlias[alias])
	}
	return plans, nil
}

func addCreatorAuditFixPlan(plansByAlias map[string]creatorAuditFixPlan, plan creatorAuditFixPlan) {
	if existing, ok := plansByAlias[plan.alias]; ok {
		for key := range plan.itemKeys {
			existing.itemKeys[key] = true
		}
		plansByAlias[plan.alias] = existing
		return
	}
	plansByAlias[plan.alias] = plan
}

func creatorVariantItemKeySet(member *creatorNameVariant) map[string]bool {
	keys := make(map[string]bool, len(member.itemKeys))
	for key := range member.itemKeys {
		keys[key] = true
	}
	return keys
}

func buildCreatorAuditFixOps(db localQueryStore, plans []creatorAuditFixPlan, apply func(creatorRenameUpdate) (string, any, error)) ([]mutation.Op, error) {
	if len(plans) == 0 {
		return nil, nil
	}
	renamesByItem := make(map[string]map[string]string)
	for _, plan := range plans {
		for key := range plan.itemKeys {
			if renamesByItem[key] == nil {
				renamesByItem[key] = make(map[string]string)
			}
			renamesByItem[key][plan.alias] = plan.canonical
		}
	}

	itemKeys := make([]string, 0, len(renamesByItem))
	for key := range renamesByItem {
		itemKeys = append(itemKeys, key)
	}
	sort.Strings(itemKeys)

	ops := make([]mutation.Op, 0, len(itemKeys))
	for _, key := range itemKeys {
		item, err := loadCreatorAuditFixItem(db, key)
		if err != nil {
			return nil, fmt.Errorf("loading item %s: %w", key, err)
		}
		update, ok, err := buildCreatorRenameUpdate(key, item, renamesByItem[key])
		if err != nil {
			return nil, fmt.Errorf("planning creator audit fix for item %s: %w", key, err)
		}
		if !ok {
			continue
		}
		updateForApply := update
		ops = append(ops, mutation.Op{
			ID:              "creators.audit.fix:" + update.key,
			Key:             update.key,
			Kind:            "creator_rename",
			ExpectedVersion: mutationExpectedVersion(update.version),
			Changes:         update.changes,
			Destructive:     false,
			Apply: func() (string, any, error) {
				return apply(updateForApply)
			},
		})
	}
	return ops, nil
}

func loadCreatorAuditFixItem(db localQueryStore, key string) (map[string]any, error) {
	rows, err := db.QueryRaw("SELECT data FROM resources WHERE resource_type='items' AND id=?", key)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("not found in local store")
	}
	var item map[string]any
	if err := json.Unmarshal([]byte(sqlStringValue(rows[0]["data"])), &item); err != nil {
		return nil, err
	}
	return item, nil
}

func buildCreatorRenameUpdate(key string, item map[string]any, renames map[string]string) (creatorRenameUpdate, bool, error) {
	if itemKey, ok := item["key"].(string); ok && itemKey != "" {
		key = itemKey
	}
	// The read plane's version is recorded for the plan only. A row may
	// legitimately carry none: write-through strips the stale version from
	// rows it replays, and an item never pushed upstream has no write-plane
	// version at all. Requiring one here made every such item invisible to
	// creator renames; the write precondition is instead resolved from the
	// write plane at apply time (see applyCreatorRenameUpdate).
	version := item["version"]
	_, changes, changed, err := renamedItemCreators(item, renames)
	if err != nil || !changed {
		return creatorRenameUpdate{}, changed, err
	}
	return creatorRenameUpdate{key: key, version: version, renames: renames, changes: changes}, true, nil
}

func renamedItemCreators(item map[string]any, renames map[string]string) ([]any, []mutation.Change, bool, error) {
	dataObj, ok := item["data"].(map[string]any)
	if !ok {
		return nil, nil, false, fmt.Errorf("missing data object")
	}
	rawCreators, ok := dataObj["creators"].([]any)
	if !ok {
		return []any{}, nil, false, nil
	}

	renamed := make([]any, 0, len(rawCreators))
	changed := false
	changedAliases := make(map[string]string)
	for _, rawCreator := range rawCreators {
		creatorObj, ok := rawCreator.(map[string]any)
		if !ok {
			renamed = append(renamed, rawCreator)
			continue
		}
		copied := copyCreatorObject(creatorObj)
		display := creatorDisplayNameFromObject(copied)
		if canonical, ok := renames[display]; ok {
			rewriteCreatorDisplayName(copied, canonical)
			changed = true
			changedAliases[display] = canonical
		}
		renamed = append(renamed, copied)
	}
	if !changed {
		return renamed, nil, false, nil
	}
	aliases := make([]string, 0, len(changedAliases))
	for alias := range changedAliases {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	changes := make([]mutation.Change, 0, len(aliases))
	for _, alias := range aliases {
		changes = append(changes, mutation.Change{Field: "creators", Remove: alias, Add: changedAliases[alias]})
	}
	return renamed, changes, true, nil
}

func copyCreatorObject(creator map[string]any) map[string]any {
	copied := make(map[string]any, len(creator))
	for k, v := range creator {
		copied[k] = v
	}
	return copied
}

func creatorDisplayNameFromObject(creator map[string]any) string {
	firstName, _ := creator["firstName"].(string)
	lastName, _ := creator["lastName"].(string)
	name, _ := creator["name"].(string)
	display, _, _ := creatorDisplayParts(firstName, lastName, name)
	return display
}

func rewriteCreatorDisplayName(creator map[string]any, displayName string) {
	if name, ok := creator["name"].(string); ok && collapseCreatorWhitespace(name) != "" {
		creator["name"] = displayName
		delete(creator, "firstName")
		delete(creator, "lastName")
		return
	}
	firstName, lastName := parseCreatorSingleName(displayName)
	delete(creator, "name")
	if firstName != "" {
		creator["firstName"] = firstName
	} else {
		delete(creator, "firstName")
	}
	if lastName != "" {
		creator["lastName"] = lastName
	} else {
		delete(creator, "lastName")
	}
}

// applyCreatorRenameUpdate performs the rename(s) against the write plane.
//
// It deliberately re-reads the item from the write plane instead of PATCHing
// the creators array captured at plan time. Plan-time state comes from the
// read plane (the local desktop API), whose object versions live in a
// different version space and are empty for items never pushed upstream — so
// the plan-time version yields no If-Unmodified-Since-Version header and
// Zotero refuses the write outright ("Either If-Unmodified-Since-Version or
// object version property must be provided for key-based writes"). Re-reading
// also means the PATCH replaces the write plane's own creators array rather
// than overwriting it with a stale copy. Mirrors applyTagRenameUpdate in
// tags_rename.go.
func applyCreatorRenameUpdate(c *client.Client, update creatorRenameUpdate) (string, any, error) {
	path := replacePathParam("/items/{itemKey}", "itemKey", update.key)
	currentData, currentVersion, err := c.GetFromWriteBaseWithVersion(nil, path, nil)
	if err != nil {
		return "failed", err.Error(), err
	}
	var current map[string]any
	if err := json.Unmarshal(currentData, &current); err != nil {
		wrapped := fmt.Errorf("parsing item %s from the write plane: %w", update.key, err)
		return "failed", wrapped.Error(), wrapped
	}
	creators, _, changed, err := renamedItemCreators(current, update.renames)
	if err != nil {
		wrapped := fmt.Errorf("item %s: %w", update.key, err)
		return "failed", wrapped.Error(), wrapped
	}
	if !changed {
		aliases := make([]string, 0, len(update.renames))
		for alias := range update.renames {
			aliases = append(aliases, alias)
		}
		sort.Strings(aliases)
		return "no_op", map[string]any{
			"code":    "creator_absent",
			"message": fmt.Sprintf("item no longer carries creator name(s) %s on the write plane", strings.Join(aliases, ", ")),
			"from":    aliases,
		}, nil
	}

	// Defence in depth: nothing may PATCH without a precondition. Zotero
	// refuses such a write anyway ("Either If-Unmodified-Since-Version or
	// object version property must be provided for key-based writes"); failing
	// here reports the real cause instead of relaying that opaque message.
	if currentVersion <= 0 {
		err := fmt.Errorf("no write-plane version for item %s; refusing to write without an If-Unmodified-Since-Version precondition", update.key)
		return "failed", err.Error(), err
	}
	headers := map[string]string{"If-Unmodified-Since-Version": strconv.Itoa(currentVersion)}
	_, statusCode, err := c.PatchWithHeaders(path, map[string]any{
		"creators": creators,
	}, headers)
	if err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusPreconditionFailed || apiErr.StatusCode == http.StatusPreconditionRequired) {
			return "conflict", apiErr.Body, err
		}
		return "failed", err.Error(), err
	}
	if statusCode < 200 || statusCode >= 300 {
		return "failed", fmt.Sprintf("HTTP %d", statusCode), fmt.Errorf("patch returned HTTP %d", statusCode)
	}
	return "applied", nil, nil
}
