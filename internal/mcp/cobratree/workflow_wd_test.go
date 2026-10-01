// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cobratree

import (
	"context"
	"strings"
	"testing"

	"zotio/internal/cli"
)

// workflow_submit forwards stdin_select to the CLI runner, which validates
// the selector before any step runs, exactly as for a local spec file.
func TestWdWorkflowSubmitValidatesStdinSelectLikeTheCLI(t *testing.T) {
	h := workflowSubmitHandler(cli.RootCmd)
	res, err := h(context.Background(), workflowSubmitRequest(map[string]any{
		"steps": []any{
			map[string]any{"command": "capabilities", "name": "src"},
			map[string]any{"command": "capabilities", "stdin_from": "src", "stdin_select": "findings[kind=missing_doi"},
		},
	}))
	if err != nil {
		t.Fatalf("handler returned protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("malformed stdin_select was accepted: %q", workflowSubmitResText(res))
	}
	if got := workflowSubmitResText(res); !strings.Contains(got, "stdin_select") || !strings.Contains(got, "unclosed [") {
		t.Fatalf("error = %q, want the runner's stdin_select rejection", got)
	}
}

func TestWdWorkflowSubmitRejectsNonStringStdinSelect(t *testing.T) {
	_, err := workflowSubmitSteps(workflowSubmitTestRoot, []any{
		map[string]any{"command": "inspect", "name": "src"},
		map[string]any{"command": "inspect", "stdin_from": "src", "stdin_select": 3},
	})
	if err == nil || err.Error() != "workflow_submit step 2: stdin_select must be a string" {
		t.Fatalf("error = %v, want a typed stdin_select rejection", err)
	}
}
