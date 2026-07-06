// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// PATCH: Add hand-written Zotero desktop URI opener missing from the generated CLI.

package cli

import (
	"fmt"
	"net/url"

	"zotio/internal/cliutil"

	"github.com/spf13/cobra"
)

// zoteroDeepLink builds a cross-platform Zotero desktop deep link for an item,
// collection, or PDF attachment in the active library — personal by default, or a
// group library when the global --group <id> (or ZOTERO_GROUP) is set. It returns
// the URI, the normalized target type, and a human-readable library scope.
//
// PATCH(glean 8r0o): generalize the macOS-only, personal-item-only opener into a
// cross-platform deep-link layer (item/collection/attachment x personal/group).
// The library segment is the shared zoteroLibrarySegment() (one convention with
// the vault backlinks). The key is percent-encoded as one path segment (see glean
// 3969059246039413 / 1b05b22e) so a key containing "/" cannot re-target a
// different desktop object.
func zoteroDeepLink(targetType, key string) (uri, normType, libScope string, err error) {
	libScope = "personal"
	if activeGroupID != "" {
		libScope = "group:" + activeGroupID
	}
	seg := zoteroLibrarySegment() // "library" or "groups/<id>"
	keySeg := url.PathEscape(key)
	switch targetType {
	case "item", "":
		uri, normType = "zotero://select/"+seg+"/items/"+keySeg, "item"
	case "collection":
		uri, normType = "zotero://select/"+seg+"/collections/"+keySeg, "collection"
	case "attachment", "pdf":
		uri, normType = "zotero://open-pdf/"+seg+"/items/"+keySeg, "attachment"
	default:
		return "", "", "", fmt.Errorf("invalid --type %q: must be item, collection, or attachment", targetType)
	}
	return uri, normType, libScope, nil
}

func newItemsOpenCmd(flags *rootFlags) *cobra.Command {
	var flagLaunch bool
	var flagType string

	cmd := &cobra.Command{
		Use:   "open <key>",
		Short: "Print or launch a Zotero desktop deep link (item, collection, or PDF)",
		Long: `Build a zotero:// desktop deep link for an item (default), a collection
(--type collection), or a PDF attachment (--type attachment), then print it or
launch it (--launch) via the OS handler (open on macOS, xdg-open on Linux,
rundll32 on Windows).

The link targets the personal library by default, or a group library when the
global --group <id> flag (or ZOTERO_GROUP) is set. With --agent/--json the
command emits {uri, target_type, library_scope, launched}.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			uri, targetType, libScope, err := zoteroDeepLink(flagType, args[0])
			if err != nil {
				return err
			}
			emit := func(launched bool) error {
				return printJSONFiltered(cmd.OutOrStdout(), map[string]any{
					"uri":           uri,
					"target_type":   targetType,
					"library_scope": libScope,
					"launched":      launched,
				}, flags)
			}
			if !flagLaunch {
				if flags.asJSON {
					return emit(false)
				}
				fmt.Fprintln(cmd.OutOrStdout(), uri)
				return nil
			}
			if cliutil.IsVerifyEnv() {
				if flags.asJSON {
					return emit(false)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "would open: %s\n", uri)
				return nil
			}
			if !flags.asJSON {
				fmt.Fprintf(cmd.ErrOrStderr(), "Opening %s in Zotero...\n", uri)
			}
			if err := launchURI(uri); err != nil {
				return fmt.Errorf("opening Zotero %s: %w", targetType, err)
			}
			if flags.asJSON {
				return emit(true)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&flagLaunch, "launch", false, "Launch the deep link via the OS handler (open/xdg-open/rundll32)")
	cmd.Flags().StringVar(&flagType, "type", "item", "Target type: item, collection, or attachment (PDF)")

	return cmd
}
