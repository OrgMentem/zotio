// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"zotio/internal/mutation"
)

func newCreatorsRenameCmd(flags *rootFlags) *cobra.Command {
	var flagFrom string
	var flagTo string
	var flagScope string

	cmd := &cobra.Command{
		Use:   "rename --from <oldName> --to <newName>",
		Short: "Rename a creator display name across matching items",
		Long: `Rename a creator's display name across every item in scope that carries it.

This is the fix half of creators audit: run creators audit first to find variant
groups, then apply the rename it suggests (or any alias/canonical pair of your
own choosing) with this command.

The rename PATCHes each affected item with its full creators array and a
version precondition, preserving creatorType, creator order, and unrelated
creators. It handles both creator shapes: firstName/lastName and a single name
field.`,
		Example: `  zotio creators rename --from "Adam J Rock" --to "Adam J. Rock"
  zotio creators rename --from "Adam J Rock" --to "Adam J. Rock" --yes
  zotio creators rename --from "Adam J Rock" --to "Adam J. Rock" --scope collection:ABCD1234`,
		Annotations: map[string]string{
			"zotio:endpoint":                   "creators.rename",
			"zotio:method":                     "PATCH",
			"zotio:path":                       "/items/{itemKey}",
			"mcp:read-only":                    "false",
			"zotio:destructive":                "false",
			"zotio:supports-dry-run":           "true",
			"zotio:requires-allow-destructive": "false",
			"zotio:default-max-changes":        "500",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if flagFrom == "" {
				return usageErr(fmt.Errorf("required flag %q not set", "from"))
			}
			if flagTo == "" {
				return usageErr(fmt.Errorf("required flag %q not set", "to"))
			}
			if flagFrom == flagTo {
				return usageErr(fmt.Errorf("--from and --to must differ"))
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

			itemKeys, err := creatorRenameCandidateItems(db, scope, flagFrom)
			if err != nil {
				return fmt.Errorf("finding items for creator %q: %w", flagFrom, err)
			}

			plans := []creatorAuditFixPlan{{alias: flagFrom, canonical: flagTo, itemKeys: itemKeys}}
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

			env, runErr := runMutation(cmd.Context(), flags, "creators.rename", ops)
			renderErr := renderMutation(cmd, flags, env, creatorRenameSingleLine(flagFrom, flagTo))
			if renderErr != nil {
				return renderErr
			}
			return runErr
		},
	}
	cmd.Flags().StringVar(&flagFrom, "from", "", "Old creator display name")
	cmd.Flags().StringVar(&flagTo, "to", "", "New creator display name")
	cmd.Flags().StringVar(&flagScope, "scope", "library", "Item scope: library, collection:<key>, tag:<tag>, item:<key>, or query:<text>")
	return cmd
}

// creatorRenameCandidateItems finds every item in scope with a creator whose
// display name exactly matches name, reusing the same occurrence query and
// name-parsing rules creators audit groups against, so `creators rename`
// finds precisely what `creators audit` reported.
func creatorRenameCandidateItems(db localQueryStore, scope scopeResult, name string) (map[string]bool, error) {
	occurrences, err := queryCreatorAuditOccurrences(db, scope)
	if err != nil {
		return nil, err
	}
	itemKeys := make(map[string]bool)
	for _, occ := range occurrences {
		if occ.DisplayName == name {
			itemKeys[occ.ItemKey] = true
		}
	}
	return itemKeys, nil
}

func creatorRenameSingleLine(oldName, newName string) func(mutation.Envelope) string {
	return func(env mutation.Envelope) string {
		action := "would rename"
		if env.Mode == "apply" {
			action = "renamed"
		}
		return fmt.Sprintf("%s creator %s -> %s in %d item(s)", action, oldName, newName, env.Plan.Summary.Planned)
	}
}
