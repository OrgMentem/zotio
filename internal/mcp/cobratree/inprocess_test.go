// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean c4ke): cover the in-process Cobra mirror handler that replaced
// the companion-binary shell-out.

package cobratree

import (
	"context"
	"fmt"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"
)

func newEchoRoot() *cobra.Command {
	root := &cobra.Command{Use: "zotio", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(&cobra.Command{
		Use: "echo",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintf(cmd.OutOrStdout(), "echoed:%s", strings.Join(args, ","))
			return nil
		},
	})
	root.AddCommand(&cobra.Command{
		Use: "fail",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprint(cmd.OutOrStdout(), "partial output")
			return fmt.Errorf("boom")
		},
	})
	return root
}

func toolResultText(t *testing.T, res *mcplib.CallToolResult) string {
	t.Helper()
	if res == nil || len(res.Content) == 0 {
		return ""
	}
	tc, ok := mcplib.AsTextContent(res.Content[0])
	if !ok {
		t.Fatalf("content[0] is not text: %T", res.Content[0])
	}
	return tc.Text
}

func TestInProcessHandler_Success(t *testing.T) {
	h := inProcessHandler(newEchoRoot, []string{"echo"})
	req := mcplib.CallToolRequest{Params: mcplib.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"args": "hello world"},
	}}
	res, err := h(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned protocol error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %q", toolResultText(t, res))
	}
	if got := toolResultText(t, res); got != "echoed:hello,world" {
		t.Errorf("result = %q, want echoed:hello,world", got)
	}
}

func TestInProcessHandler_Error(t *testing.T) {
	h := inProcessHandler(newEchoRoot, []string{"fail"})
	req := mcplib.CallToolRequest{Params: mcplib.CallToolParams{Name: "fail"}}
	res, err := h(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected IsError result for a failing command")
	}
	if got := toolResultText(t, res); !strings.Contains(got, "boom") {
		t.Errorf("error result %q does not contain 'boom'", got)
	}
}

func TestRegisterAll_NilFactoryIsNoOp(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("RegisterAll(nil) panicked: %v", r)
		}
	}()
	s := server.NewMCPServer("test", "0.0.0")
	RegisterAll(s, nil)
	// A factory that returns nil must also be a no-op.
	RegisterAll(s, func() *cobra.Command { return nil })
}

func TestRegisterAll_RegistersMirrorTools(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("RegisterAll panicked: %v", r)
		}
	}()
	s := server.NewMCPServer("test", "0.0.0")
	RegisterAll(s, newEchoRoot)
}
