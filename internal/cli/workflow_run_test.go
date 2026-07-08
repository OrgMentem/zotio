// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeWorkflowRunTestSpec(t *testing.T, spec workflowRunSpec) string {
	t.Helper()
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal workflow spec: %v", err)
	}
	path := filepath.Join(t.TempDir(), "workflow.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write workflow spec: %v", err)
	}
	return path
}

func runWorkflowRunTestCmd(t *testing.T, spec workflowRunSpec) (workflowRunReport, string, error) {
	t.Helper()
	path := writeWorkflowRunTestSpec(t, spec)
	cmd := RootCmd()
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	var out bytes.Buffer
	var errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--json", "workflow", "run", path})
	err := cmd.Execute()
	var report workflowRunReport
	if out.Len() > 0 {
		if decodeErr := json.Unmarshal(out.Bytes(), &report); decodeErr != nil {
			t.Fatalf("decode workflow report %q: %v", out.String(), decodeErr)
		}
	}
	return report, errOut.String(), err
}

func TestWorkflowRunReportsSuccessfulSteps(t *testing.T) {
	report, stderr, err := runWorkflowRunTestCmd(t, workflowRunSpec{Steps: []workflowRunStepSpec{
		{Args: []string{"version"}},
		{Args: []string{"--help"}},
	}})
	if err != nil {
		t.Fatalf("workflow run succeeded steps: %v; stderr=%s", err, stderr)
	}
	if !report.OK {
		t.Fatalf("report.OK = false, want true: %+v", report)
	}
	if len(report.Steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(report.Steps))
	}
	for _, step := range report.Steps {
		if !step.OK || step.Status != "ok" || step.Error != "" {
			t.Fatalf("step = %+v, want ok without error", step)
		}
	}
}

func TestWorkflowRunStopsAfterFailureByDefault(t *testing.T) {
	report, _, err := runWorkflowRunTestCmd(t, workflowRunSpec{Steps: []workflowRunStepSpec{
		{Args: []string{"definitely-not-a-command"}},
		{Args: []string{"--help"}},
	}})
	if err == nil {
		t.Fatal("expected workflow error")
	}
	if report.OK {
		t.Fatalf("report.OK = true, want false: %+v", report)
	}
	if len(report.Steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(report.Steps))
	}
	if report.Steps[0].OK || report.Steps[0].Status != "failed" || !strings.Contains(report.Steps[0].Error, "unknown command") {
		t.Fatalf("first step = %+v, want failed unknown command", report.Steps[0])
	}
	if report.Steps[1].OK || report.Steps[1].Status != "not_attempted" || report.Steps[1].Output != "" {
		t.Fatalf("second step = %+v, want not_attempted", report.Steps[1])
	}
}

func TestWorkflowRunContinueOnErrorRunsLaterSteps(t *testing.T) {
	report, _, err := runWorkflowRunTestCmd(t, workflowRunSpec{
		ContinueOnError: true,
		Steps: []workflowRunStepSpec{
			{Args: []string{"definitely-not-a-command"}},
			{Args: []string{"--help"}},
		},
	})
	if err == nil {
		t.Fatal("expected workflow error")
	}
	if report.OK {
		t.Fatalf("report.OK = true, want false: %+v", report)
	}
	if len(report.Steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(report.Steps))
	}
	if report.Steps[0].OK || report.Steps[0].Status != "failed" {
		t.Fatalf("first step = %+v, want failed", report.Steps[0])
	}
	if !report.Steps[1].OK || report.Steps[1].Status != "ok" || report.Steps[1].Output == "" {
		t.Fatalf("second step = %+v, want executed ok", report.Steps[1])
	}
}

func TestWorkflowRunRejectsWorkflowStep(t *testing.T) {
	_, _, err := runWorkflowRunTestCmd(t, workflowRunSpec{Steps: []workflowRunStepSpec{
		{Args: []string{"workflow", "run", "again.json"}},
	}})
	if err == nil {
		t.Fatal("expected workflow recursion rejection")
	}
	if !strings.Contains(err.Error(), "cannot invoke workflow commands") {
		t.Fatalf("error = %v, want workflow recursion rejection", err)
	}
}
