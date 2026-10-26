// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// validateCommandUsage runs pure command-specific flag checks before capability
// preflight. Keep environment checks in the command's execution path.
func validateCommandUsage(cmd *cobra.Command, args []string) error {
	switch commandRegistryPath(cmd) {
	case "library health":
		return validateLibraryHealthUsage(cmd)
	case "searches materialize":
		return validateSearchesMaterializeUsage(cmd, args)
	case "creators rename":
		return validateCreatorsRenameUsage(cmd)
	default:
		return nil
	}
}

func validateLibraryHealthUsage(cmd *cobra.Command) error {
	failOn, err := cmd.Flags().GetString("fail-on")
	if err != nil {
		return err
	}
	if failOn != "" {
		_, err = parseFailOnSeverity("fail-on", failOn)
	}
	return err
}

func validateSearchesMaterializeUsage(cmd *cobra.Command, args []string) error {
	toCollection, err := cmd.Flags().GetString("to")
	if err != nil {
		return err
	}
	if toCollection == "" {
		return usageErr(fmt.Errorf("required flag %q not set", "to"))
	}
	if err := validateKeyArg("collection", toCollection); err != nil {
		return err
	}
	return validateKeyArg("search", args[0])
}

func validateCreatorsRenameUsage(cmd *cobra.Command) error {
	from, err := cmd.Flags().GetString("from")
	if err != nil {
		return err
	}
	to, err := cmd.Flags().GetString("to")
	if err != nil {
		return err
	}
	if from == "" {
		return usageErr(fmt.Errorf("required flag %q not set", "from"))
	}
	if to == "" {
		return usageErr(fmt.Errorf("required flag %q not set", "to"))
	}
	if from == to {
		return usageErr(fmt.Errorf("--from and --to must differ"))
	}
	return nil
}

// leafNoPositionalArgs describes unexpected values as arguments, not commands.
// Only suggest a known primary flag or an unambiguous required string flag.
func leafNoPositionalArgs(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	message := fmt.Sprintf("%s takes no positional arguments (got %q)", cmd.CommandPath(), args[0])
	primary := cmd.Flags().Lookup("item-type")
	if primary == nil {
		var required *pflag.Flag
		count := 0
		cmd.LocalNonPersistentFlags().VisitAll(func(flag *pflag.Flag) {
			if flag.Value.Type() == "string" && len(flag.Annotations[cobra.BashCompOneRequiredFlag]) != 0 {
				required = flag
				count++
			}
		})
		if count == 1 {
			primary = required
		}
	}
	if primary != nil {
		message += fmt.Sprintf("; did you mean --%s %q?", primary.Name, args[0])
	}
	return usageErr(fmt.Errorf("%s", message))
}
