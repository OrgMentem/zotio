// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cobratree

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zotio/internal/cli"

	"github.com/spf13/cobra"
)

func TestWorkflowMcpSubmitPreservesTemporarySpecPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "space dir\\with 'quote")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"TMPDIR", "TEMP", "TMP"} {
		t.Setenv(name, dir)
	}
	var received string
	factory := func() *cobra.Command {
		root := workflowSubmitTestRoot()
		run, _, err := root.Find([]string{"workflow", "run"})
		if err != nil {
			t.Fatal(err)
		}
		original := run.RunE
		run.RunE = func(cmd *cobra.Command, args []string) error {
			received = args[0]
			return original(cmd, args)
		}
		return root
	}
	result, err := workflowSubmitHandler(factory)(context.Background(), workflowSubmitRequest(map[string]any{
		"steps": []any{map[string]any{"command": "inspect"}},
	}))
	if err != nil || result.IsError {
		t.Fatalf("submit error=%v result=%+v", err, result)
	}
	if filepath.Dir(received) != dir {
		t.Fatalf("received path=%q, want directory %q", received, dir)
	}
	if _, err := os.Stat(received); !os.IsNotExist(err) {
		t.Fatalf("temporary spec remains: %v", err)
	}
}

func TestWorkflowMcpPreTokenizedWindowsPath(t *testing.T) {
	want := `C:\Users\John Smith\Temp\zotio-workflow.json`
	var got string
	factory := func() *cobra.Command {
		root := &cobra.Command{Use: "zotio"}
		workflow := &cobra.Command{Use: "workflow"}
		workflow.AddCommand(&cobra.Command{
			Use: "run", Args: cobra.ExactArgs(1),
			Run: func(_ *cobra.Command, args []string) { got = args[0] },
		})
		root.AddCommand(workflow)
		return root
	}
	result := runMirroredWithPositionals(context.Background(), factory, []string{"workflow", "run"}, nil, []string{want})
	if result.IsError || got != want {
		t.Fatalf("got=%q want=%q result=%+v", got, want, result)
	}
}

func TestWorkflowMcpSubmitCannotUseStepDestructiveConsent(t *testing.T) {
	result, err := workflowSubmitHandler(cli.RootCmd)(context.Background(), workflowSubmitRequest(map[string]any{
		"yes": true,
		"steps": []any{map[string]any{
			"name": "merge", "command": "creators rename",
			"flags": map[string]any{"from": "Old Name", "to": "New Name", "allow-destructive": true},
		}},
	}))
	if err != nil || !result.IsError {
		t.Fatalf("submit error=%v result=%+v", err, result)
	}
	text := workflowSubmitResText(result)
	if !strings.Contains(text, "merge") || !strings.Contains(text, "--allow-destructive on workflow run itself") {
		t.Fatalf("unexpected consent refusal: %s", text)
	}
}

func TestWorkflowMcpSubmitForwardsInvocationConsent(t *testing.T) {
	var allowed bool
	factory := func() *cobra.Command {
		root := workflowSubmitTestRoot()
		run, _, err := root.Find([]string{"workflow", "run"})
		if err != nil {
			t.Fatal(err)
		}
		run.Flags().Bool("allow-destructive", false, "")
		original := run.RunE
		run.RunE = func(cmd *cobra.Command, args []string) error {
			allowed, err = cmd.Flags().GetBool("allow-destructive")
			if err != nil {
				return err
			}
			return original(cmd, args)
		}
		return root
	}
	result, err := workflowSubmitHandler(factory)(context.Background(), workflowSubmitRequest(map[string]any{
		"yes": true, "allow-destructive": true,
		"steps": []any{map[string]any{"command": "inspect"}},
	}))
	if err != nil || result.IsError || !allowed {
		t.Fatalf("consent=%v error=%v result=%+v", allowed, err, result)
	}
}
