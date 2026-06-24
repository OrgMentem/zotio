// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH: Add hand-written Zotero desktop URI opener missing from the generated CLI.

package cli

import (
	"fmt"
	"net/url"
	"os/exec"

	"zotero-pp-cli/internal/cliutil"

	"github.com/spf13/cobra"
)

func newItemsOpenCmd(flags *rootFlags) *cobra.Command {
	var flagLaunch bool

	cmd := &cobra.Command{
		Use:   "open <itemKey>",
		Short: "Print or launch a Zotero desktop item URI",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			// PATCH(glean zotero-pp-cli-3969059246039413): encode the item key as one URI path segment before handing it to Zotero/open.
			uri := "zotero://select/library/items/" + url.PathEscape(args[0])
			if !flagLaunch {
				fmt.Fprintln(cmd.OutOrStdout(), uri)
				return nil
			}
			if cliutil.IsVerifyEnv() {
				fmt.Fprintf(cmd.OutOrStdout(), "would open: %s\n", uri)
				return nil
			}
			if flags.asJSON {
				if err := exec.Command("open", uri).Run(); err != nil {
					return fmt.Errorf("opening Zotero item: %w", err)
				}
				return printJSONFiltered(cmd.OutOrStdout(), map[string]any{"uri": uri, "launched": true}, flags)
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "Opening %s in Zotero...\n", uri)
			if err := exec.Command("open", uri).Run(); err != nil {
				return fmt.Errorf("opening Zotero item: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&flagLaunch, "launch", false, "Launch the URI with the macOS open command")

	return cmd
}
