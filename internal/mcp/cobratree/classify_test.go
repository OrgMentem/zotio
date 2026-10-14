// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cobratree

import (
	"testing"

	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"

	"zotio/internal/cli"
)

func TestClassifyEndpointAsNovelMCPCommand(t *testing.T) {
	// Endpoint-annotated commands are ordinary CLI metadata since ADR-0003
	// retired the typed endpoint tools; the walker mirrors them as novel
	// commands. The annotation key is a plain string: no cobratree constant
	// names it.
	cmd := &cobra.Command{
		Use:         "list",
		Annotations: map[string]string{"zotio:endpoint": "items.list"},
		Run:         func(*cobra.Command, []string) {},
	}
	if got := classify(cmd); got != commandNovel {
		t.Fatalf("classify(endpoint command) = %v, want commandNovel", got)
	}
}

// frameworkCommands names top-level commands by full path. A nested command
// that shares a leaf name with one (`annotations search` vs `search`) is still
// a mirrored command.
func TestClassifyMatchesFrameworkCommandsByFullPath(t *testing.T) {
	run := func(*cobra.Command, []string) {}
	root := &cobra.Command{Use: "zotio"}
	top := &cobra.Command{Use: "search <query>", Run: run}
	group := &cobra.Command{Use: "annotations"}
	nested := &cobra.Command{Use: "search <query>", Run: run}
	group.AddCommand(nested)
	root.AddCommand(top, group)

	if got := classify(top); got != commandFramework {
		t.Errorf("classify(search) = %v, want commandFramework", got)
	}
	if got := classify(nested); got != commandNovel {
		t.Errorf("classify(annotations search) = %v, want commandNovel", got)
	}
}

func TestMirrorExposesAnnotationsSearch(t *testing.T) {
	s := server.NewMCPServer("test", "0.0.0")
	RegisterAll(s, cli.RootCmd)
	if s.GetTool("annotations_search") == nil {
		t.Error("annotations search is read-only and documented but is not a mirrored tool")
	}
	if s.GetTool("search") != nil {
		t.Error("the top-level search command must stay out of the mirror; the typed search tool covers it")
	}
}
