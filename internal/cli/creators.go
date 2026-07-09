// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import "github.com/spf13/cobra"

func newCreatorsCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "creators",
		Short: "Audit creator names across your Zotero library",
	}

	cmd.AddCommand(newCreatorsAuditCmd(flags))
	return cmd
}
