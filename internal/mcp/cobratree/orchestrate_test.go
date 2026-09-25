// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cobratree

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/spf13/cobra"
)

func orchNewRoot() *cobra.Command {
	root := &cobra.Command{Use: "zotero", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().Bool("agent", false, "Run in agent mode")
	root.PersistentFlags().Bool("json", false, "Emit JSON")
	root.PersistentFlags().Bool("no-color", false, "Disable color")

	items := &cobra.Command{Use: "items", Short: "Work with items"}
	demo := &cobra.Command{
		Use:   "demo",
		Short: "Run the demo item command",
		RunE: func(c *cobra.Command, _ []string) error {
			title, _ := c.Flags().GetString("title")
			fmt.Fprintf(c.OutOrStdout(), "ran demo title=%s", title)
			return nil
		},
	}
	demo.Flags().String("title", "", "Demo title")

	view := &cobra.Command{
		Use:         "view",
		Short:       "View an item",
		Annotations: map[string]string{ReadOnlyAnnotation: "true"},
		RunE: func(c *cobra.Command, _ []string) error {
			fmt.Fprint(c.OutOrStdout(), "viewed item")
			return nil
		},
	}

	items.AddCommand(demo, view)
	root.AddCommand(items)
	return root
}

func orchResText(res *mcplib.CallToolResult) string {
	if res == nil {
		return ""
	}
	for _, c := range res.Content {
		if tc, ok := c.(mcplib.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}

func TestOrchCommandSearchListsMirrorableCommands(t *testing.T) {
	h := commandSearchHandler(orchNewRoot)
	req := mcplib.CallToolRequest{}
	res, err := h(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned protocol error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %q", orchResText(res))
	}

	var got []struct {
		Name    string `json:"name"`
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal([]byte(orchResText(res)), &got); err != nil {
		t.Fatalf("search result is not a JSON array: %v; text=%q", err, orchResText(res))
	}

	seenDemo := false
	for _, item := range got {
		if item.Name == "items demo" {
			seenDemo = true
		}
		if item.Name == "agent" || item.Name == "json" || item.Name == "no-color" || strings.Contains(item.Name, "agent") || strings.Contains(item.Name, "json") || strings.Contains(item.Name, "no-color") {
			t.Fatalf("search returned global flag %q as a command", item.Name)
		}
		if strings.TrimSpace(item.Summary) == "" {
			t.Fatalf("search item %q has empty summary", item.Name)
		}
	}
	if !seenDemo {
		t.Fatalf("search names = %#v, want items demo", got)
	}
}

func TestOrchCommandSearchDetailsExposesOnlyLocalSafeFlags(t *testing.T) {
	h := commandSearchHandler(orchNewRoot)
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"name": "items demo"}
	res, err := h(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned protocol error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %q", orchResText(res))
	}

	var got struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		TakesArgs   bool   `json:"takesArgs"`
		Flags       []struct {
			Name        string `json:"name"`
			Type        string `json:"type"`
			Required    bool   `json:"required"`
			Description string `json:"description"`
		} `json:"flags"`
	}
	if err := json.Unmarshal([]byte(orchResText(res)), &got); err != nil {
		t.Fatalf("detail result is not a JSON object: %v; text=%q", err, orchResText(res))
	}
	if got.Name != "items demo" {
		t.Fatalf("detail name = %q, want items demo", got.Name)
	}
	if strings.TrimSpace(got.Description) == "" {
		t.Fatal("detail description is empty")
	}
	if got.TakesArgs != commandTakesArgs((&cobra.Command{Use: "demo"})) {
		t.Fatalf("takesArgs = %v, want bool value matching commandTakesArgs for demo", got.TakesArgs)
	}

	seenTitle := false
	for _, flag := range got.Flags {
		switch flag.Name {
		case "title":
			seenTitle = true
			if flag.Type != "string" {
				t.Fatalf("title flag type = %q, want string", flag.Type)
			}
			if strings.TrimSpace(flag.Description) == "" {
				t.Fatal("title flag description is empty")
			}
		case "json", "no-color", "agent":
			t.Fatalf("detail exposed global flag %q", flag.Name)
		}
	}
	if !seenTitle {
		t.Fatalf("detail flags = %#v, want title", got.Flags)
	}
}

// Null positionals stay absent (matching workflow_submit), so an explicit
// JSON null does not fail a call that would succeed without the key.
func TestOrchCommandRunCases(t *testing.T) {
	cases := []struct {
		name          string
		rootFactory   func() *cobra.Command
		argsMap       map[string]any
		wantSubstring string
	}{
		{
			name:        "executes with local flag",
			rootFactory: orchNewRoot,
			argsMap: map[string]any{
				"name":  "items demo",
				"flags": map[string]any{"title": "hi"},
			},
			wantSubstring: "title=hi",
		},
		{
			name:        "applies write gating flag on mutating",
			rootFactory: orchNewRootWithGates,
			argsMap: map[string]any{
				"name":  "items enrich",
				"flags": map[string]any{"yes": true},
			},
			wantSubstring: "applied=true",
		},
		{
			name:        "treats null positional args as absent",
			rootFactory: orchNewRoot,
			argsMap: map[string]any{
				"name":  "items demo",
				"flags": map[string]any{"title": "hi"},
				"args":  nil,
			},
			wantSubstring: "title=hi",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := commandRunHandler(tc.rootFactory)
			req := mcplib.CallToolRequest{}
			req.Params.Arguments = tc.argsMap
			res, err := h(context.Background(), req)
			if err != nil {
				t.Fatalf("handler returned protocol error: %v", err)
			}
			if res.IsError {
				t.Fatalf("unexpected error result: %q", orchResText(res))
			}
			if got := orchResText(res); !strings.Contains(got, tc.wantSubstring) {
				t.Fatalf("run result = %q, want %s", got, tc.wantSubstring)
			}
		})
	}
}

func TestOrchCommandRunRejectsForgedGlobalAndRawFlagArgs(t *testing.T) {
	h := commandRunHandler(orchNewRoot)
	cases := []struct {
		name string
		args map[string]any
	}{
		{
			name: "forged global flag",
			args: map[string]any{"name": "items demo", "flags": map[string]any{"json": true}},
		},
		{
			name: "raw positional flag token",
			args: map[string]any{"name": "items demo", "args": "--secret"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := mcplib.CallToolRequest{}
			req.Params.Arguments = tc.args
			res, err := h(context.Background(), req)
			if err != nil {
				t.Fatalf("handler returned protocol error: %v", err)
			}
			if !res.IsError {
				t.Fatalf("expected error result, got text %q", orchResText(res))
			}
			if strings.TrimSpace(orchResText(res)) == "" {
				t.Fatal("error result text is empty")
			}
		})
	}
}

func TestOrchCommandRunRejectsMissingCommand(t *testing.T) {
	h := commandRunHandler(orchNewRoot)
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"name": "nope nope"}
	res, err := h(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected error result for missing command, got text %q", orchResText(res))
	}
	if strings.TrimSpace(orchResText(res)) == "" {
		t.Fatal("missing-command error result text is empty")
	}
}

// End-to-end proof that write-safety gate flags are reachable through the
// facade for mutating commands (so applies fire) and rejected for read-only
// commands.
func orchNewRootWithGates() *cobra.Command {
	root := &cobra.Command{Use: "zotero", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().Bool("agent", false, "Run in agent mode")
	root.PersistentFlags().Bool("yes", false, "Skip confirmation prompts")
	root.PersistentFlags().Bool("dry-run", false, "Preview only")

	items := &cobra.Command{Use: "items", Short: "Work with items"}
	mut := &cobra.Command{
		Use:   "enrich",
		Short: "Enrich items (mutating)",
		RunE: func(c *cobra.Command, _ []string) error {
			yes, _ := c.Flags().GetBool("yes")
			fmt.Fprintf(c.OutOrStdout(), "applied=%v", yes)
			return nil
		},
	}
	ro := &cobra.Command{
		Use:         "list",
		Short:       "List items (read-only)",
		Annotations: map[string]string{ReadOnlyAnnotation: "true"},
		RunE: func(c *cobra.Command, _ []string) error {
			fmt.Fprint(c.OutOrStdout(), "listed")
			return nil
		},
	}
	items.AddCommand(mut, ro)
	root.AddCommand(items)
	return root
}

func TestOrchCommandSearchDetailExposesWriteGatingForMutating(t *testing.T) {
	h := commandSearchHandler(orchNewRootWithGates)
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"name": "items enrich"}
	res, err := h(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned protocol error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %q", orchResText(res))
	}
	var got struct {
		Flags []struct {
			Name string `json:"name"`
		} `json:"flags"`
	}
	if err := json.Unmarshal([]byte(orchResText(res)), &got); err != nil {
		t.Fatalf("detail result is not JSON: %v; text=%q", err, orchResText(res))
	}
	names := map[string]bool{}
	for _, f := range got.Flags {
		names[f.Name] = true
	}
	if !names["yes"] {
		t.Fatalf("mutating detail flags %#v missing write-gating flag yes", got.Flags)
	}
	if names["agent"] {
		t.Fatal("detail exposed formatting global agent")
	}
}

func TestOrchCommandRunRejectsWriteGatingOnReadOnly(t *testing.T) {
	h := commandRunHandler(orchNewRootWithGates)
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"name":  "items list",
		"flags": map[string]any{"yes": true},
	}
	res, err := h(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected --yes rejected on read-only command, got %q", orchResText(res))
	}
}

// The facade must behave exactly like the mirror: a non-string positional
// argument is refused with a clear error instead of being dropped silently
// by the pre-filter (which used to ignore any non-string args value).
func TestOrchCommandRunRefusesNonStringPositionalArgs(t *testing.T) {
	h := commandRunHandler(orchNewRoot)
	for name, value := range map[string]any{
		"numeric": float64(123),
		"boolean": true,
		"array":   []any{"KEY1"},
	} {
		t.Run(name, func(t *testing.T) {
			req := mcplib.CallToolRequest{}
			req.Params.Arguments = map[string]any{"name": "items demo", "args": value}
			res, err := h(context.Background(), req)
			if err != nil {
				t.Fatalf("handler returned protocol error: %v", err)
			}
			if !res.IsError {
				t.Fatalf("expected error result for %T args, got text %q", value, orchResText(res))
			}
			if !strings.Contains(orchResText(res), "must be a string") {
				t.Fatalf("error result = %q, want it to name the string contract", orchResText(res))
			}
		})
	}
}

// Witness for the hidden-subcommand bypass: a positional that names a hidden
// subcommand must be refused on the facade, while ordinary positionals still
// run. The mirror-side witness lives in inprocess_test.go; both must name
// the command the positionals actually selected.
func TestOrchCommandRunRejectsHiddenSubcommandViaArgs(t *testing.T) {
	newRoot := func() *cobra.Command {
		root := &cobra.Command{Use: "zotio", SilenceUsage: true, SilenceErrors: true}
		root.PersistentFlags().Bool("agent", false, "Run in agent mode")
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
			RunE: func(c *cobra.Command, _ []string) error {
				fmt.Fprint(c.OutOrStdout(), "drifted")
				return nil
			},
		}
		capabilities.AddCommand(drift)
		root.AddCommand(capabilities)
		return root
	}
	h := commandRunHandler(newRoot)
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"name": "capabilities", "args": "drift"}
	res, err := h(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("facade accepted args that resolve to a hidden subcommand, got %q", orchResText(res))
	}
	if got := orchResText(res); !strings.Contains(got, `"capabilities drift"`) || !strings.Contains(got, `"capabilities"`) {
		t.Fatalf("error = %q, want it to name the selected and validated commands", got)
	}
	if !strings.Contains(orchResText(res), "different or hidden command") {
		t.Fatalf("error = %q, want the shared binding rejection", orchResText(res))
	}

	ordinary := mcplib.CallToolRequest{}
	ordinary.Params.Arguments = map[string]any{"name": "capabilities", "args": "some-key"}
	ordinaryRes, err := h(context.Background(), ordinary)
	if err != nil {
		t.Fatalf("handler returned protocol error: %v", err)
	}
	if ordinaryRes.IsError {
		t.Fatalf("facade refused an ordinary positional: %q", orchResText(ordinaryRes))
	}
	if got := orchResText(ordinaryRes); !strings.Contains(got, "capabilities:some-key") {
		t.Fatalf("run result = %q, want the ordinary positional to reach the command", got)
	}
}
