// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cobratree

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestClassifyEndpointAsNovelMCPCommand(t *testing.T) {
	cmd := &cobra.Command{
		Use:         "list",
		Annotations: map[string]string{EndpointAnnotation: "items.list"},
		Run:         func(*cobra.Command, []string) {},
	}
	if got := classify(cmd); got != commandNovel {
		t.Fatalf("classify(endpoint command) = %v, want commandNovel", got)
	}
}
