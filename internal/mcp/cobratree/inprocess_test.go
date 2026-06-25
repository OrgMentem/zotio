// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.

package cobratree

import (
	"testing"

	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"
)

func newEchoRoot() *cobra.Command {
	root := &cobra.Command{Use: "zotero-pp-cli", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(&cobra.Command{
		Use:   "echo [args]",
		Short: "echo args",
		Run:   func(cmd *cobra.Command, args []string) {},
	})
	return root
}

func TestRegisterAll_NilRootIsNoOp(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("RegisterAll(nil) panicked: %v", r)
		}
	}()
	s := server.NewMCPServer("test", "0.0.0")
	RegisterAll(s, nil)
	RegisterAll(s, (*cobra.Command)(nil))
	RegisterAll(s, func() *cobra.Command { return nil })
}

func TestRegisterAll_AcceptsRootAndLegacyFactory(t *testing.T) {
	s := server.NewMCPServer("test", "0.0.0")
	cliPath := func() (string, error) { return "/bin/echo", nil }
	RegisterAll(s, newEchoRoot(), cliPath)
	RegisterAll(s, newEchoRoot, cliPath)
}
