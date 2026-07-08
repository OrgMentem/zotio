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

func TestOrchCommandRunExecutesWithLocalFlag(t *testing.T) {
	h := commandRunHandler(orchNewRoot)
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"name":  "items demo",
		"flags": map[string]any{"title": "hi"},
	}
	res, err := h(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned protocol error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %q", orchResText(res))
	}
	if got := orchResText(res); !strings.Contains(got, "title=hi") {
		t.Fatalf("run result = %q, want title=hi", got)
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

func TestOrchCommandRunAppliesWriteGatingFlagOnMutating(t *testing.T) {
	h := commandRunHandler(orchNewRootWithGates)
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"name":  "items enrich",
		"flags": map[string]any{"yes": true},
	}
	res, err := h(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned protocol error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %q", orchResText(res))
	}
	if got := orchResText(res); !strings.Contains(got, "applied=true") {
		t.Fatalf("run result = %q, want applied=true (--yes must propagate)", got)
	}
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
