// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package main

import (
	"fmt"
	"os"

	"zotio/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		// Cobra's own error printing is silenced, so this is the only place a
		// failure reaches the terminal.
		fmt.Fprintf(os.Stderr, "Error: %s\n", err.Error())
		os.Exit(cli.ExitCode(err))
	}
}
