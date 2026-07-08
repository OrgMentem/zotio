// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package main

import (
	"fmt"
	"os"

	"zotio/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(cli.ExitCode(err))
	}
}
