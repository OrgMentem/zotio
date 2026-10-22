// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Cover the in-process Cobra mirror handler.

package cobratree

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

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

// Mirror-side witness for the hidden-subcommand bypass: safeInProcessHandler
// must refuse positionals that resolve to a hidden subcommand, while ordinary
// positionals still run. Mirrors TestOrchCommandRunRejectsHiddenSubcommandViaArgs
// on the facade; both go through positionalBindingError per ADR-0001.
func TestSafeInProcessHandlerRejectsHiddenSubcommandViaArgs(t *testing.T) {
	var drifted bool
	newRoot := func() *cobra.Command {
		root := &cobra.Command{Use: "zotio", SilenceUsage: true, SilenceErrors: true}
		capabilities := &cobra.Command{
			Use:         "capabilities",
			Short:       "Emit the capability registry",
			Args:        cobra.ArbitraryArgs,
			Annotations: map[string]string{ReadOnlyAnnotation: "true"},
			RunE: func(c *cobra.Command, args []string) error {
				fmt.Fprintf(c.OutOrStdout(), "capabilities:%s", strings.Join(args, ","))
				return nil
			},
		}
		drift := &cobra.Command{
			Use:         "drift",
			Short:       "Probe capability drift",
			Annotations: map[string]string{ReadOnlyAnnotation: "true", HiddenAnnotation: "true"},
			RunE: func(*cobra.Command, []string) error {
				drifted = true
				return nil
			},
		}
		capabilities.AddCommand(drift)
		root.AddCommand(capabilities)
		return root
	}
	allowed := safeFlagNames((&cobra.Command{Use: "capabilities"}))
	h := safeInProcessHandler(newRoot, []string{"capabilities"}, allowed)
	req := mcplib.CallToolRequest{Params: mcplib.CallToolParams{
		Name:      "capabilities",
		Arguments: map[string]any{"args": "drift"},
	}}
	res, err := h(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("mirror accepted args that resolve to a hidden subcommand, got %q", toolResultText(t, res))
	}
	if drifted {
		t.Fatal("refused call still executed the hidden subcommand")
	}
	if got := toolResultText(t, res); !strings.Contains(got, `"capabilities drift"`) || !strings.Contains(got, `"capabilities"`) {
		t.Fatalf("error = %q, want it to name the selected and validated commands", got)
	}

	ordinary := mcplib.CallToolRequest{Params: mcplib.CallToolParams{
		Name:      "capabilities",
		Arguments: map[string]any{"args": "some-key"},
	}}
	ordinaryRes, err := h(context.Background(), ordinary)
	if err != nil {
		t.Fatalf("handler returned protocol error: %v", err)
	}
	if ordinaryRes.IsError {
		t.Fatalf("mirror refused an ordinary positional: %q", toolResultText(t, ordinaryRes))
	}
	if got := toolResultText(t, ordinaryRes); !strings.Contains(got, "capabilities:some-key") {
		t.Fatalf("run result = %q, want the ordinary positional to reach the command", got)
	}
}

// failureRoot builds a root whose "run" command writes partial output, then
// fails with detail (or panics with it when panics is set).
func failureRoot(partial, detail string, panics bool) func() *cobra.Command {
	return func() *cobra.Command {
		root := &cobra.Command{Use: "zotio", SilenceUsage: true, SilenceErrors: true}
		root.AddCommand(&cobra.Command{
			Use: "run",
			RunE: func(cmd *cobra.Command, _ []string) error {
				fmt.Fprint(cmd.OutOrStdout(), partial)
				if panics {
					panic(detail)
				}
				return fmt.Errorf("%s", detail)
			},
		})
		return root
	}
}

// A failing command's diagnostic rides outside the library-data block with its
// own 4096-byte cap, and the data block gives up that much of the transport
// budget. Whatever the sizes, the result must stay inside bound.MaxBytes, stay
// valid UTF-8 (a rune split by the cut must back up, not turn into U+FFFD),
// keep the head of the diagnostic, and keep the diagnostic out of the block.
// The recovered-panic path prefixes "panic: ", so sizes are stated for the
// whole diagnostic, prefix included.
func TestInProcessHandlerBoundsFailureDiagnostics(t *testing.T) {
	const head = "diag-head:"
	// diagnostic returns the full text after the data block, minus the
	// leading newline: prefix + head + padding up to total bytes.
	pad := func(prefix string, total int) string {
		return prefix + head + strings.Repeat("a", total-len(prefix)-len(head))
	}
	cases := []struct {
		name string
		// build returns the whole diagnostic for the given prefix.
		build func(prefix string) string
		// wantLen is the exact length of the diagnostic after the data block
		// (newline included) when it is truncated; 0 means it must survive
		// verbatim.
		wantLen int
	}{
		// The suffix is "\n" + diagnostic, so 4095 diagnostic bytes is exactly the cap.
		{name: "below cap", build: func(p string) string { return pad(p, 4094) }},
		{name: "at cap", build: func(p string) string { return pad(p, 4095) }},
		{name: "just above cap", build: func(p string) string { return pad(p, 4096) }, wantLen: 4096},
		{name: "far above cap", build: func(p string) string { return pad(p, 3*4096) }, wantLen: 4096},
		{
			// A 3-byte rune whose first byte sits at suffix offset 4092: the
			// 4093-byte cut lands inside it and must back up to 4092.
			name:    "multibyte rune straddles the cut",
			build:   func(p string) string { return pad(p, 4091) + strings.Repeat("\u20ac", 3) },
			wantLen: 4092 + len("..."),
		},
	}
	partials := map[string]string{
		"short partial output": "partial output",
		// Framed, this fills the transport cap exactly: it passes on its own
		// but leaves no room for the diagnostic unless the budget shrinks.
		"cap-filling partial output": strings.Repeat("x", bound.MaxBytes-len(bound.UntrustedBlock(""))),
	}
	for _, panics := range []bool{false, true} {
		prefix := ""
		if panics {
			prefix = "panic: "
		}
		for _, tc := range cases {
			for partialName, partial := range partials {
				t.Run(fmt.Sprintf("panic=%t/%s/%s", panics, tc.name, partialName), func(t *testing.T) {
					diagnostic := tc.build(prefix)
					res := runMirroredInProcess(context.Background(),
						failureRoot(partial, strings.TrimPrefix(diagnostic, prefix), panics), []string{"run"}, nil)
					if res == nil || !res.IsError {
						t.Fatalf("result = %+v, want error tool result", res)
					}
					got := toolResultText(t, res)
					if len(got) > bound.MaxBytes {
						t.Errorf("result is %d bytes, over the %d-byte transport budget", len(got), bound.MaxBytes)
					}
					if !utf8.ValidString(got) || strings.ContainsRune(got, utf8.RuneError) {
						t.Errorf("result is not clean UTF-8 (cut rune not backed up)")
					}
					closer := strings.LastIndex(got, "<<<END-ZOTERO-DATA ")
					if !strings.Contains(got, "<<<ZOTERO-DATA ") || closer < 0 {
						t.Fatalf("library output is not framed as data")
					}
					tail := got[closer+strings.Index(got[closer:], ">>>")+len(">>>"):]
					if !strings.HasPrefix(tail, "\n"+prefix+head) {
						t.Errorf("diagnostic outside the data block lost its head: %q", tail[:min(len(tail), 40)])
					}
					if strings.Contains(got[:closer], head) {
						t.Errorf("diagnostic leaked into the library-data block")
					}
					if tc.wantLen == 0 {
						if tail != "\n"+diagnostic {
							t.Errorf("a diagnostic within the cap was altered (%d bytes after the block)", len(tail))
						}
						return
					}
					if !strings.HasSuffix(tail, "...") {
						t.Errorf("an oversized diagnostic carries no truncation marker")
					}
					if len(tail) != tc.wantLen {
						t.Errorf("diagnostic is %d bytes after the block, want %d", len(tail), tc.wantLen)
					}
				})
			}
		}
	}
}
