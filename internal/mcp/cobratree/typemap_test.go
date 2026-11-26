// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cobratree

import (
	"reflect"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/spf13/cobra"
)

func TestMirrorSchemaMapsRepeatableStringFlagsToArrays(t *testing.T) {
	cmd := &cobra.Command{Use: "sync", Run: func(*cobra.Command, []string) {}}
	cmd.Flags().StringSlice("resources", nil, "Resources to sync")
	cmd.Flags().StringArray("tag", nil, "Tags to write")

	tool := mcplib.NewTool("sync", safeToolOptionsForFlags(cmd)...)
	for _, name := range []string{"resources", "tag"} {
		property, ok := tool.InputSchema.Properties[name].(map[string]any)
		if !ok {
			t.Fatalf("mirror schema property %q = %T, want object", name, tool.InputSchema.Properties[name])
		}
		if got := property["type"]; got != "array" {
			t.Errorf("mirror schema property %q type = %v, want array", name, got)
		}
		items, ok := property["items"].(map[string]any)
		if !ok || items["type"] != "string" {
			t.Errorf("mirror schema property %q items = %#v, want string items", name, property["items"])
		}
	}

	if got, want := cliArgsFromMCP(map[string]any{"resources": "all"}), []string{"--resources=all"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("single string repeatable flag argv = %#v, want %#v", got, want)
	}
}

func TestMirrorSchemaMapsIntegerFlagsToInteger(t *testing.T) {
	cmd := &cobra.Command{Use: "sync", Run: func(*cobra.Command, []string) {}}
	cmd.Flags().Int("max-changes", 0, "")
	cmd.Flags().Uint16("port", 0, "")
	cmd.Flags().Float64("ratio", 0, "")

	tool := mcplib.NewTool("sync", safeToolOptionsForFlags(cmd)...)
	want := map[string]string{"max-changes": "integer", "port": "integer", "ratio": "number"}
	for name, typ := range want {
		property, ok := tool.InputSchema.Properties[name].(map[string]any)
		if !ok {
			t.Fatalf("schema property %q = %T, want object", name, tool.InputSchema.Properties[name])
		}
		if property["type"] != typ {
			t.Errorf("schema property %q type = %v, want %s", name, property["type"], typ)
		}
	}
}
