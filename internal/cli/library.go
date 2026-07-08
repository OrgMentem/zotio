// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import "github.com/spf13/cobra"

func newLibraryCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "library",
		Short: "Library-wide analytics and reporting",
	}
	cmd.AddCommand(newLibraryStatsCmd(flags))
	cmd.AddCommand(newLibraryHealthCmd(flags))
	cmd.AddCommand(newLibraryWrappedCmd(flags))
	return cmd
}
