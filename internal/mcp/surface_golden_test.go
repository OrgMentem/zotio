// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// MCP surface is a contract; accidental drift must fail CI; intentional changes regenerate with -update.
// See ADR-0001 and ADR-0003 for the agent-native surface and drift-gate rationale.

package mcp

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/server"

	"zotio/internal/cli"
)

var updateSurfaceGoldens = flag.Bool("update", false, "regenerate MCP surface golden files")

type goldenSurfaceTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	InputSchema any    `json:"inputSchema"`
}

func TestMCPSurfaceGolden(t *testing.T) {
	tests := []struct {
		name       string
		envSurface string
		golden     string
		assert     func(*testing.T, []goldenSurfaceTool)
	}{
		{
			name:   "facade",
			golden: "surface_facade.golden.json",
			assert: assertFacadeSurface,
		},
		{
			name:       "mirror",
			envSurface: "mirror",
			golden:     "surface_mirror.golden.json",
			assert:     assertMirrorSurface,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.envSurface == "" {
				t.Setenv("ZOTIO_MCP_SURFACE", "")
			} else {
				t.Setenv("ZOTIO_MCP_SURFACE", tt.envSurface)
			}

			s := server.NewMCPServer(
				"Zotero",
				cli.Version(),
				server.WithToolCapabilities(false),
				server.WithResourceCapabilities(false, true),
				server.WithPromptCapabilities(true),
			)
			RegisterTools(s)

			tools := collectSurfaceTools(t, s)
			sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
			tt.assert(t, tools)

			actual, err := json.MarshalIndent(tools, "", "  ")
			if err != nil {
				t.Fatalf("marshal surface: %v", err)
			}
			actual = append(actual, '\n')

			goldenPath := filepath.Join("testdata", tt.golden)
			if *updateSurfaceGoldens {
				if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
					t.Fatalf("create testdata: %v", err)
				}
				if err := os.WriteFile(goldenPath, actual, 0o644); err != nil {
					t.Fatalf("update golden %s: %v", goldenPath, err)
				}
				return
			}

			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden %s: %v (run go test ./internal/mcp -run TestMCPSurfaceGolden -update)", goldenPath, err)
			}
			if string(actual) != string(want) {
				t.Fatalf(
					"MCP %s surface golden mismatch at %s\n%s\nRegenerate after reviewing the surface change with: go test ./internal/mcp -run TestMCPSurfaceGolden -update",
					tt.name,
					goldenPath,
					firstDiffLine(string(want), string(actual)),
				)
			}
		})
	}
}

func collectSurfaceTools(t *testing.T, s *server.MCPServer) []goldenSurfaceTool {
	t.Helper()

	rpc(t, s, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    "zotio-surface-golden-test",
			"version": "0.0.0",
		},
	})

	var out []goldenSurfaceTool
	var cursor string
	for {
		var params any
		if cursor != "" {
			params = map[string]any{"cursor": cursor}
		}
		result := rpc(t, s, "tools/list", params)
		rawTools, ok := result["tools"].([]any)
		if !ok {
			t.Fatalf("tools/list returned no tools array: %#v", result)
		}
		for _, rawTool := range rawTools {
			tool, ok := rawTool.(map[string]any)
			if !ok {
				t.Fatalf("tools/list tool is not an object: %#v", rawTool)
			}
			name, ok := tool["name"].(string)
			if !ok || name == "" {
				t.Fatalf("tools/list tool has no name: %#v", tool)
			}
			description, _ := tool["description"].(string)
			schema, ok := tool["inputSchema"]
			if !ok {
				t.Fatalf("tools/list tool %q has no inputSchema", name)
			}
			out = append(out, goldenSurfaceTool{
				Name:        name,
				Description: description,
				InputSchema: schema,
			})
		}

		nextCursor, _ := result["nextCursor"].(string)
		if nextCursor == "" {
			break
		}
		if nextCursor == cursor {
			t.Fatalf("tools/list pagination repeated cursor %q", cursor)
		}
		cursor = nextCursor
	}
	return out
}

func assertFacadeSurface(t *testing.T, tools []goldenSurfaceTool) {
	t.Helper()
	want := []string{"command_run", "command_search", "context", "search", "sql"}
	got := surfaceToolNames(tools)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("facade tools = %v, want exactly %v", got, want)
	}
}

func assertMirrorSurface(t *testing.T, tools []goldenSurfaceTool) {
	t.Helper()
	got := surfaceToolNames(tools)
	set := make(map[string]bool, len(got))
	for _, name := range got {
		set[name] = true
	}
	if len(got) <= 30 {
		t.Fatalf("mirror tools count = %d, want > 30", len(got))
	}
	if set["command_run"] {
		t.Fatalf("mirror surface unexpectedly contains command_run")
	}
	for _, want := range []string{"context", "search", "sql"} {
		if !set[want] {
			t.Fatalf("mirror surface missing %q (got %v)", want, got)
		}
	}
}

func surfaceToolNames(tools []goldenSurfaceTool) []string {
	names := make([]string, len(tools))
	for i, tool := range tools {
		names[i] = tool.Name
	}
	sort.Strings(names)
	return names
}

func firstDiffLine(want, got string) string {
	wantLines := strings.Split(want, "\n")
	gotLines := strings.Split(got, "\n")
	limit := len(wantLines)
	if len(gotLines) < limit {
		limit = len(gotLines)
	}
	for i := 0; i < limit; i++ {
		if wantLines[i] != gotLines[i] {
			return fmt.Sprintf("first differing line %d:\nwant: %s\n got: %s", i+1, wantLines[i], gotLines[i])
		}
	}
	return fmt.Sprintf("line count differs: want %d lines, got %d lines", len(wantLines), len(gotLines))
}
