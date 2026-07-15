// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cobratree

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/spf13/cobra"
)

func workflowSubmitTestRoot() *cobra.Command {
	root := &cobra.Command{Use: "zotio", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().Bool("agent", false, "Run in agent mode")
	root.PersistentFlags().Bool("yes", false, "Apply changes")
	root.PersistentFlags().String("config", "", "Configuration path")

	inspect := &cobra.Command{
		Use:         "inspect",
		Short:       "Inspect state",
		Annotations: map[string]string{ReadOnlyAnnotation: "true"},
		RunE: func(c *cobra.Command, _ []string) error {
			fmt.Fprint(c.OutOrStdout(), "inspect")
			return nil
		},
	}
	workflowRun := &cobra.Command{
		Use:   "run <file.json>",
		Short: "Run a workflow",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			raw, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			var spec struct {
				Steps []struct {
					Args []string `json:"args"`
				} `json:"steps"`
			}
			if err := json.Unmarshal(raw, &spec); err != nil {
				return err
			}
			if len(spec.Steps) != 1 || strings.Join(spec.Steps[0].Args, " ") != "inspect" {
				return fmt.Errorf("unexpected workflow spec: %s", raw)
			}
			yes, err := c.Root().PersistentFlags().GetBool("yes")
			if err != nil {
				return err
			}
			report := map[string]any{
				"steps": []map[string]any{{"status": "ok"}},
				"ok":    true,
				"mode":  "preview",
			}
			if yes {
				if err := os.WriteFile(args[0]+".checkpoint.json", []byte(`{"interrupted":true}`), 0o600); err != nil {
					return err
				}
				report["mode"] = "apply"
				report["run_id"] = "test-run-id"
			}
			return json.NewEncoder(c.OutOrStdout()).Encode(report)
		},
	}
	workflow := &cobra.Command{Use: "workflow", Short: "Workflow commands"}
	workflow.AddCommand(workflowRun)
	root.AddCommand(inspect, workflow)
	return root
}

func workflowSubmitResText(res *mcplib.CallToolResult) string {
	if res == nil {
		return ""
	}
	for _, content := range res.Content {
		if text, ok := content.(mcplib.TextContent); ok {
			return text.Text
		}
	}
	return ""
}

func workflowSubmitRequest(args map[string]any) mcplib.CallToolRequest {
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = args
	return req
}

func TestWorkflowSubmitRejectsUnknownCommand(t *testing.T) {
	h := workflowSubmitHandler(workflowSubmitTestRoot)
	res, err := h(context.Background(), workflowSubmitRequest(map[string]any{
		"steps": []any{map[string]any{"command": "missing command"}},
	}))
	if err != nil {
		t.Fatalf("handler returned protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected error result, got %q", workflowSubmitResText(res))
	}
	if got := workflowSubmitResText(res); !strings.Contains(got, "step 1") || !strings.Contains(got, "missing command") {
		t.Fatalf("error = %q, want step and command name", got)
	}
}

func TestWorkflowSubmitRejectsForbiddenFlag(t *testing.T) {
	h := workflowSubmitHandler(workflowSubmitTestRoot)
	res, err := h(context.Background(), workflowSubmitRequest(map[string]any{
		"steps": []any{map[string]any{
			"command": "inspect",
			"flags":   map[string]any{"config": "outside-the-facade"},
		}},
	}))
	if err != nil {
		t.Fatalf("handler returned protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected error result, got %q", workflowSubmitResText(res))
	}
	if got := workflowSubmitResText(res); !strings.Contains(got, "step 1") || !strings.Contains(got, "--config") {
		t.Fatalf("error = %q, want step-specific config rejection", got)
	}
}

func TestWorkflowSubmitExecutesPreviewAndCleansTemporaryFiles(t *testing.T) {
	before, err := workflowSubmitTempFiles()
	if err != nil {
		t.Fatalf("list workflow temp files before run: %v", err)
	}

	h := workflowSubmitHandler(workflowSubmitTestRoot)
	res, err := h(context.Background(), workflowSubmitRequest(map[string]any{
		"steps": []any{map[string]any{"command": "inspect"}},
	}))
	if err != nil {
		t.Fatalf("handler returned protocol error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %q", workflowSubmitResText(res))
	}
	var report struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal([]byte(workflowSubmitResText(res)), &report); err != nil {
		t.Fatalf("workflow result is not a JSON report: %v; text=%q", err, workflowSubmitResText(res))
	}
	if report.Mode != "preview" {
		t.Fatalf("report mode = %q, want preview", report.Mode)
	}

	after, err := workflowSubmitTempFiles()
	if err != nil {
		t.Fatalf("list workflow temp files after run: %v", err)
	}
	if !sameWorkflowSubmitTempFiles(before, after) {
		t.Fatalf("workflow temp files after run = %v, want %v", after, before)
	}
}

func TestWorkflowSubmitAppliesWithRunID(t *testing.T) {
	before, err := workflowSubmitTempFiles()
	if err != nil {
		t.Fatalf("list workflow temp files before run: %v", err)
	}

	h := workflowSubmitHandler(workflowSubmitTestRoot)
	res, err := h(context.Background(), workflowSubmitRequest(map[string]any{
		"yes":   true,
		"steps": []any{map[string]any{"command": "inspect"}},
	}))
	if err != nil {
		t.Fatalf("handler returned protocol error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %q", workflowSubmitResText(res))
	}
	var report struct {
		Mode  string `json:"mode"`
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal([]byte(workflowSubmitResText(res)), &report); err != nil {
		t.Fatalf("workflow result is not a JSON report: %v; text=%q", err, workflowSubmitResText(res))
	}
	if report.Mode != "apply" {
		t.Fatalf("report mode = %q, want apply", report.Mode)
	}
	if report.RunID == "" {
		t.Fatal("apply report run_id is empty")
	}
	after, err := workflowSubmitTempFiles()
	if err != nil {
		t.Fatalf("list workflow temp files after run: %v", err)
	}
	if !sameWorkflowSubmitTempFiles(before, after) {
		t.Fatalf("workflow temp files after run = %v, want %v", after, before)
	}
}

func workflowSubmitTempFiles() (map[string]struct{}, error) {
	paths, err := filepath.Glob(filepath.Join(os.TempDir(), "zotio-workflow-*.json*"))
	if err != nil {
		return nil, err
	}
	files := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		files[path] = struct{}{}
	}
	return files, nil
}

func sameWorkflowSubmitTempFiles(left, right map[string]struct{}) bool {
	if len(left) != len(right) {
		return false
	}
	for path := range left {
		if _, ok := right[path]; !ok {
			return false
		}
	}
	return true
}
