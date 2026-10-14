// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package main

import (
	"os"

	"zotio/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		// Cobra's own error printing is silenced, so this is the only place a
		// failure reaches the terminal. ReportError prints the human line, the
		// single JSON error object when one is due, and honours --quiet.
		os.Exit(cli.ReportError(os.Stdout, os.Stderr, err))
	}
}
