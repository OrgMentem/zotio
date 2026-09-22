// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cobratree

import (
	"testing"

	"github.com/spf13/cobra"
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
