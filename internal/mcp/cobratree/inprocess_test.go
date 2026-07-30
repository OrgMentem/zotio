// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Cover the in-process Cobra mirror handler.

package cobratree

import (
	"context"
	"fmt"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"

	"zotio/internal/mcp/bound"
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
	// Opaque (non-JSON) mirrored output is library content as far as the
	// transport can tell -- an export blob or a rendered table -- so it arrives
	// inside the data-not-directive block. JSON results keep their bare shape;
	// TestInProcessHandler_JSONOutputIsNotWrappedInProse covers that side.
	got := toolResultText(t, res)
	if !strings.Contains(got, "echoed:hello,world") {
		t.Errorf("result = %q, want it to carry echoed:hello,world", got)
	}
	if !strings.Contains(got, "<<<ZOTERO-DATA ") {
		t.Errorf("result = %q, want opaque output framed as library data", got)
	}
}

// The counterpart guarantee, and the reason framing is not applied blanket:
// hosts parse JSON tool results, and this same path carries zotio's own
// structured output (workflow reports, every --agent command), which is not
// library-authored at all. Prefixing prose to those breaks their consumers and
// mislabels the source, so structured output must arrive byte-exact.
func TestInProcessHandler_JSONOutputIsNotWrappedInProse(t *testing.T) {
	newJSONRoot := func() *cobra.Command {
		root := &cobra.Command{Use: "root"}
		root.AddCommand(&cobra.Command{
			Use: "report",
			RunE: func(cmd *cobra.Command, _ []string) error {
				_, err := cmd.OutOrStdout().Write([]byte(`{"ok":true,"mode":"preview"}`))
				return err
			},
		})
		return root
	}
	h := inProcessHandler(newJSONRoot, []string{"report"})
	res, err := h(context.Background(), mcplib.CallToolRequest{Params: mcplib.CallToolParams{Name: "report"}})
	if err != nil {
		t.Fatalf("handler returned protocol error: %v", err)
	}
	if got := toolResultText(t, res); got != `{"ok":true,"mode":"preview"}` {
		t.Errorf("result = %q, want the JSON report byte-exact", got)
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
	} else if !strings.Contains(got, "<<<ZOTERO-DATA ") {
		t.Errorf("error result %q does not frame partial output as library data", got)
	}
}

func TestRunMirroredInProcessRecoversPanicAndRestoresState(t *testing.T) {
	previousStateGuard := StateGuard
	t.Cleanup(func() {
		StateGuard = previousStateGuard
	})
	restored := false
	StateGuard = func() func() {
		return func() {
			restored = true
		}
	}
	rootFactory := func() *cobra.Command {
		return &cobra.Command{
			Use:           "zotio",
			SilenceUsage:  true,
			SilenceErrors: true,
			RunE: func(cmd *cobra.Command, _ []string) error {
				fmt.Fprint(cmd.OutOrStdout(), "partial output")
				panic("mirror panic")
			},
		}
	}

	res := runMirroredInProcess(context.Background(), rootFactory, nil, nil)
	if res == nil || !res.IsError {
		t.Fatalf("result = %+v, want error tool result", res)
	}
	if got := toolResultText(t, res); !strings.Contains(got, "mirror panic") || !strings.Contains(got, "partial output") {
		t.Fatalf("panic result = %q, want panic value and command output", got)
	} else if !strings.Contains(got, "<<<ZOTERO-DATA ") {
		t.Fatalf("panic result = %q, want partial output framed as library data", got)
	}
	if !restored {
		t.Fatal("state guard restore was not called")
	}
}

func TestInProcessHandlerBoundsFramedOpaqueOutput(t *testing.T) {
	for _, size := range []int{bound.MaxBytes - 1, bound.MaxBytes, bound.MaxBytes + 1} {
		t.Run(fmt.Sprintf("%d_bytes", size), func(t *testing.T) {
			rootFactory := func() *cobra.Command {
				root := &cobra.Command{Use: "zotio", SilenceUsage: true, SilenceErrors: true}
				root.AddCommand(&cobra.Command{
					Use: "export",
					RunE: func(cmd *cobra.Command, _ []string) error {
						_, err := fmt.Fprint(cmd.OutOrStdout(), strings.Repeat("x", size))
						return err
					},
				})
				return root
			}

			res := runMirroredInProcess(context.Background(), rootFactory, []string{"export"}, nil)
			if res == nil || res.IsError {
				t.Fatalf("result = %+v, want successful tool result", res)
			}
			got := toolResultText(t, res)
			if len(got) > bound.MaxBytes {
				t.Errorf("result is %d bytes, over the %d-byte transport budget", len(got), bound.MaxBytes)
			}
			if !strings.Contains(got, "<<<ZOTERO-DATA ") {
				t.Errorf("result does not frame opaque output as library data: %q", got)
			}
			if !strings.Contains(got, `"truncated":true`) {
				t.Errorf("result does not retain an oversized-output preview: %q", got)
			}
		})
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
