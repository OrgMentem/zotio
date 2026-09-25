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
	root.PersistentFlags().Bool("no-input", false, "Disable interactive prompts")
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
				Vars map[string]string `json:"vars"`
			}
			if err := json.Unmarshal(raw, &spec); err != nil {
				return err
			}
			agent, err := c.Root().PersistentFlags().GetBool("agent")
			if err != nil {
				return err
			}
			noInput, err := c.Root().PersistentFlags().GetBool("no-input")
			if err != nil {
				return err
			}
			if !agent || !noInput {
				return fmt.Errorf("workflow run must be agent and non-interactive")
			}
			if len(spec.Steps) != 1 {
				return fmt.Errorf("unexpected workflow spec: %s", raw)
			}
			for _, arg := range spec.Steps[0].Args {
				for name, value := range spec.Vars {
					arg = strings.ReplaceAll(arg, "${vars."+name+"}", value)
				}
				if strings.HasPrefix(arg, "-") {
					return fmt.Errorf("substituted workflow argument cannot be a flag: %s", arg)
				}
			}
			if strings.Join(spec.Steps[0].Args, " ") != "inspect" {
				return fmt.Errorf("unexpected workflow spec: %s", raw)
			}
			yes, err := c.Root().PersistentFlags().GetBool("yes")
			if err != nil {
				return err
			}
			report := map[string]any{
				"spec":  json.RawMessage(raw),
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

func TestWorkflowSubmitRejectsHiddenSubcommandViaArgs(t *testing.T) {
	root := workflowSubmitTestRoot()
	capabilities := &cobra.Command{
		Use:  "capabilities",
		Args: cobra.ArbitraryArgs,
		RunE: func(*cobra.Command, []string) error { return nil },
	}
	capabilities.AddCommand(&cobra.Command{
		Use:    "drift",
		Hidden: true,
		RunE:   func(*cobra.Command, []string) error { return nil },
	})
	root.AddCommand(capabilities)

	_, err := workflowSubmitSteps(func() *cobra.Command { return root }, []any{map[string]any{
		"command": "capabilities",
		"args":    "drift",
	}})
	if err == nil {
		t.Fatal("workflow_submit accepted args that resolve to a hidden subcommand")
	}
	if got := err.Error(); !strings.Contains(got, "step 1") || !strings.Contains(got, "different or hidden command") {
		t.Fatalf("error = %q, want step-specific command binding rejection", got)
	}
}

func TestWorkflowSubmitAcceptsEmptyArgs(t *testing.T) {
	h := workflowSubmitHandler(workflowSubmitTestRoot)
	res, err := h(context.Background(), workflowSubmitRequest(map[string]any{
		"steps": []any{map[string]any{
			"command": "inspect",
			"args":    "",
		}},
	}))
	if err != nil {
		t.Fatalf("handler returned protocol error: %v", err)
	}
	if res.IsError {
		t.Fatalf("empty args returned an error: %q", workflowSubmitResText(res))
	}
}

func TestWorkflowSubmitRejectsSubstitutedFlag(t *testing.T) {
	h := workflowSubmitHandler(workflowSubmitTestRoot)
	res, err := h(context.Background(), workflowSubmitRequest(map[string]any{
		"vars": map[string]any{"X": "--group=123"},
		"steps": []any{map[string]any{
			"command": "inspect",
			"args":    "${vars.X}",
		}},
	}))
	if err != nil {
		t.Fatalf("handler returned protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("substituted flag was accepted: %q", workflowSubmitResText(res))
	}
	if got := workflowSubmitResText(res); !strings.Contains(got, "--group=123") {
		t.Fatalf("error = %q, want substituted flag rejection", got)
	}
}

func TestWorkflowSubmitPreviewAndApply(t *testing.T) {
	cases := []struct {
		name      string
		argsMap   map[string]any
		wantMode  string
		wantRunID bool
	}{
		{
			name: "preview cleans temporary files",
			argsMap: map[string]any{
				"steps": []any{map[string]any{"command": "inspect"}},
			},
			wantMode:  "preview",
			wantRunID: false,
		},
		{
			name: "apply reports run id",
			argsMap: map[string]any{
				"yes":   true,
				"steps": []any{map[string]any{"command": "inspect"}},
			},
			wantMode:  "apply",
			wantRunID: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before, err := workflowSubmitTempFiles()
			if err != nil {
				t.Fatalf("list workflow temp files before run: %v", err)
			}
			h := workflowSubmitHandler(workflowSubmitTestRoot)
			res, err := h(context.Background(), workflowSubmitRequest(tc.argsMap))
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
			if report.Mode != tc.wantMode {
				t.Fatalf("report mode = %q, want %s", report.Mode, tc.wantMode)
			}
			if tc.wantRunID && report.RunID == "" {
				t.Fatal("apply report run_id is empty")
			}
			after, err := workflowSubmitTempFiles()
			if err != nil {
				t.Fatalf("list workflow temp files after run: %v", err)
			}
			if !sameWorkflowSubmitTempFiles(before, after) {
				t.Fatalf("workflow temp files after run = %v, want %v", after, before)
			}
		})
	}
}

func TestWorkflowSubmitWhen(t *testing.T) {
	tests := []struct {
		name      string
		steps     []any
		wantWhen  *workflowSubmitStepWhen
		wantError string
	}{
		{
			name: "preserves valid condition",
			steps: []any{map[string]any{
				"command": "inspect",
				"when": map[string]any{
					"step": "prepare",
					"is":   "ok",
				},
			}},
			wantWhen: &workflowSubmitStepWhen{Step: "prepare", Is: "ok"},
		},
		{
			name: "rejects non-object condition",
			steps: []any{
				map[string]any{"command": "inspect"},
				map[string]any{"command": "inspect", "when": "after prepare"},
			},
			wantError: "workflow_submit step 2: when must be an object",
		},
		{
			name: "rejects non-string step",
			steps: []any{
				map[string]any{"command": "inspect"},
				map[string]any{
					"command": "inspect",
					"when":    map[string]any{"step": 1, "is": "ok"},
				},
			},
			wantError: "workflow_submit step 2: when.step must be a string",
		},
		{
			name: "rejects non-string state",
			steps: []any{
				map[string]any{"command": "inspect"},
				map[string]any{
					"command": "inspect",
					"when":    map[string]any{"step": "prepare", "is": true},
				},
			},
			wantError: "workflow_submit step 2: when.is must be a string",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := workflowSubmitHandler(workflowSubmitTestRoot)
			res, err := h(context.Background(), workflowSubmitRequest(map[string]any{
				"steps": tt.steps,
			}))
			if err != nil {
				t.Fatalf("handler returned protocol error: %v", err)
			}
			if tt.wantError != "" {
				if !res.IsError {
					t.Fatalf("expected error result, got %q", workflowSubmitResText(res))
				}
				if got := workflowSubmitResText(res); got != tt.wantError {
					t.Fatalf("error = %q, want %q", got, tt.wantError)
				}
				return
			}
			if res.IsError {
				t.Fatalf("unexpected error result: %q", workflowSubmitResText(res))
			}
			var report struct {
				Spec workflowSubmitSpec `json:"spec"`
			}
			if err := json.Unmarshal([]byte(workflowSubmitResText(res)), &report); err != nil {
				t.Fatalf("workflow result is not a JSON report: %v; text=%q", err, workflowSubmitResText(res))
			}
			if len(report.Spec.Steps) != 1 {
				t.Fatalf("generated spec has %d steps, want 1", len(report.Spec.Steps))
			}
			if got := report.Spec.Steps[0].When; got == nil || *got != *tt.wantWhen {
				t.Fatalf("generated spec when = %#v, want %#v", got, tt.wantWhen)
			}
		})
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
