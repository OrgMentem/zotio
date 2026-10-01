// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cobratree

import (
	"context"
	"encoding/json"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"

	"zotio/internal/cli"
)

// TestMroMirrorNeverAdvertisesFileWritersReadOnly registers the real command
// tree on the mirror surface and checks every tool's hints against the MCP
// spec: a command annotated mcp:writes-files may replace or delete a file, so
// it must not claim readOnlyHint=true and must claim destructiveHint=true,
// even though it keeps the in-repo mcp:read-only annotation. Plain read-only
// commands keep readOnlyHint=true.
func TestMroMirrorNeverAdvertisesFileWritersReadOnly(t *testing.T) {
	s := server.NewMCPServer("test", "0.0.0")
	RegisterAll(s, cli.RootCmd)

	fileWriters := map[string]bool{}
	readOnly := 0
	walk(cli.RootCmd(), nil, func(cmd *cobra.Command, path []string) {
		if classify(cmd) != commandNovel || !cmd.Runnable() {
			return
		}
		name := toolNameForPath(path)
		tool := s.GetTool(name)
		if tool == nil {
			t.Errorf("mirrored command %v has no tool %q", path, name)
			return
		}
		hints := tool.Tool.Annotations
		switch {
		case writesFiles(cmd):
			fileWriters[name] = true
			if hints.ReadOnlyHint == nil || *hints.ReadOnlyHint {
				t.Errorf("%s writes files but readOnlyHint = %v, want false", name, mroBoolPtr(hints.ReadOnlyHint))
			}
			if hints.DestructiveHint == nil || !*hints.DestructiveHint {
				t.Errorf("%s can replace or delete files but destructiveHint = %v, want true", name, mroBoolPtr(hints.DestructiveHint))
			}
		case isMCPReadOnly(cmd):
			readOnly++
			if hints.ReadOnlyHint == nil || !*hints.ReadOnlyHint {
				t.Errorf("%s is read-only and writes no file but readOnlyHint = %v, want true", name, mroBoolPtr(hints.ReadOnlyHint))
			}
		}
	})
	if readOnly == 0 {
		t.Fatal("no plain read-only commands were mirrored; the walk is broken")
	}
	// The commands the file-writing findings named must be caught by the rule.
	for _, name := range []string{
		"demo",
		"export_snapshot",
		"import_monitor",
		"import_discover",
		"collections_bundle",
		"annotations_export",
		"library_wrapped",
		"creators_audit",
	} {
		if !fileWriters[name] {
			t.Errorf("%s is not marked as a file writer on the mirror; it would be advertised read-only", name)
		}
	}
}

// TestMroFacadeSearchReportsFileWriters checks the facade side: command_search
// detail and the command list both say a file-writing read command writes
// files, and a plain read command does not.
func TestMroFacadeSearchReportsFileWriters(t *testing.T) {
	h := commandSearchHandler(cli.RootCmd)

	detail := func(name string) orchestrationCommandDetail {
		t.Helper()
		req := mcplib.CallToolRequest{}
		req.Params.Arguments = map[string]any{"name": name}
		res, err := h(context.Background(), req)
		if err != nil || res.IsError {
			t.Fatalf("command_search %q: err=%v result=%q", name, err, orchResText(res))
		}
		var out orchestrationCommandDetail
		if err := json.Unmarshal([]byte(orchResText(res)), &out); err != nil {
			t.Fatalf("decode command_search %q: %v", name, err)
		}
		return out
	}
	if got := detail("export snapshot"); !got.WritesFiles {
		t.Errorf("command_search export snapshot = %+v, want writesFiles=true", got)
	}
	for _, name := range []string{"export", "creators audit"} {
		if got := detail(name); !got.WritesFiles {
			t.Errorf("command_search %s = %+v, want writesFiles=true", name, got)
		}
	}
	if got := detail("library stats"); got.WritesFiles {
		t.Errorf("command_search library stats = %+v, want writesFiles=false", got)
	}

	res, err := h(context.Background(), mcplib.CallToolRequest{})
	if err != nil || res.IsError {
		t.Fatalf("command_search list: err=%v result=%q", err, orchResText(res))
	}
	var list []orchestrationCommandSummary
	if err := json.Unmarshal([]byte(orchResText(res)), &list); err != nil {
		t.Fatalf("decode command_search list: %v", err)
	}
	seen := false
	for _, c := range list {
		if c.Name == "demo" {
			seen = true
			if !c.WritesFiles {
				t.Errorf("command_search list demo = %+v, want writesFiles=true", c)
			}
		}
	}
	if !seen {
		t.Fatal("command_search list has no demo command")
	}
}

func mroBoolPtr(p *bool) any {
	if p == nil {
		return nil
	}
	return *p
}
