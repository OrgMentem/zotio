// Copyright 2026 OrgMentem. Licensed under Apache-2.0. See LICENSE.
// PATCH: Add hand-written library analytics command group missing from the generated CLI.

package cli

import "github.com/spf13/cobra"

func newLibraryCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "library",
		Short: "Library-wide analytics and reporting",
	}
	cmd.AddCommand(newLibraryStatsCmd(flags))
	cmd.AddCommand(newLibraryHealthCmd(flags))
	return cmd
}
