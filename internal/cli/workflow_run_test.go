// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
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
	return runWorkflowRunTestCmdAtPath(t, writeWorkflowRunTestSpec(t, spec), false, false)
}

func runWorkflowRunApplyTestCmd(t *testing.T, spec workflowRunSpec) (workflowRunReport, string, error) {
	t.Helper()
	return runWorkflowRunTestCmdAtPath(t, writeWorkflowRunTestSpec(t, spec), true, false)
}

func runWorkflowRunTestCmdAtPath(t *testing.T, path string, apply, resume bool) (workflowRunReport, string, error) {
	t.Helper()
	return runWorkflowRunTestCmdAtPathWithInvocation(t, path, workflowRunInvocation{Yes: apply, Resume: resume})
}

func runWorkflowRunTestCmdAtPathWithVars(t *testing.T, path string, apply, resume bool, vars []string) (workflowRunReport, string, error) {
	t.Helper()
	return runWorkflowRunTestCmdAtPathWithInvocation(t, path, workflowRunInvocation{
		Yes:          apply,
		Resume:       resume,
		VarOverrides: vars,
	})
}

func runWorkflowRunTestCmdAtPathWithInvocation(t *testing.T, path string, inv workflowRunInvocation) (workflowRunReport, string, error) {
	t.Helper()
	cmd := RootCmd()
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	var out bytes.Buffer
	var errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	args := []string{"--json"}
	if inv.Yes {
		args = append(args, "--yes")
	}
	if inv.DryRun {
		args = append(args, "--dry-run")
	}
	if inv.Agent {
		args = append(args, "--agent")
	}
	if inv.NoInput {
		args = append(args, "--no-input")
	}
	args = append(args, "workflow", "run")
	if inv.Resume {
		args = append(args, "--resume")
	}
	for _, variable := range inv.VarOverrides {
		args = append(args, "--var", variable)
	}
	args = append(args, path)
	cmd.SetArgs(args)
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
	if report.Mode != workflowRunModePreview || report.RunID != "" {
		t.Fatalf("preview report = %+v, want mode=%q without run ID", report, workflowRunModePreview)
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

func TestWorkflowRunTransformsStepArgsByMode(t *testing.T) {
	readOnlyArgs := []string{"journal", "list"}
	mutatingArgs := []string{"journal", "undo", "RUN-ID"}
	if !workflowRunStepIsReadOnly(readOnlyArgs) {
		t.Fatalf("read-only step %q was not resolved as read-only", readOnlyArgs)
	}
	if workflowRunStepIsReadOnly(mutatingArgs) {
		t.Fatalf("mutating step %q was resolved as read-only", mutatingArgs)
	}

	readOnlyValueFlagArgs := []string{"items", "list", "--limit", "5", "--format", "bibtex"}
	if !workflowRunStepIsReadOnly(readOnlyValueFlagArgs) {
		t.Fatalf("read-only step with value flags %q was not resolved as read-only", readOnlyValueFlagArgs)
	}

	tests := []struct {
		name     string
		mode     string
		readOnly bool
		args     []string
		want     []string
	}{
		{
			name:     "preview mutating",
			mode:     workflowRunModePreview,
			readOnly: false,
			args:     mutatingArgs,
			want:     []string{"journal", "undo", "RUN-ID", "--dry-run"},
		},
		{
			name:     "preview read only",
			mode:     workflowRunModePreview,
			readOnly: true,
			args:     readOnlyArgs,
			want:     []string{"journal", "list"},
		},
		{
			name:     "apply mutating",
			mode:     workflowRunModeApply,
			readOnly: false,
			args:     mutatingArgs,
			want:     []string{"journal", "undo", "RUN-ID", "--yes"},
		},
		{
			name:     "apply read only",
			mode:     workflowRunModeApply,
			readOnly: true,
			args:     readOnlyArgs,
			want:     []string{"journal", "list", "--yes"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := workflowRunStepArgs(test.mode, test.readOnly, test.args)
			if strings.Join(got, "\x00") != strings.Join(test.want, "\x00") {
				t.Fatalf("workflowRunStepArgs(%q, %t, %q) = %q, want %q", test.mode, test.readOnly, test.args, got, test.want)
			}
		})
	}
}

func TestWorkflowRunRejectsWorkflowOwnedStepFlags(t *testing.T) {
	tests := []struct {
		name string
		arg  string
	}{
		{name: "yes", arg: "--yes"},
		{name: "yes assignment", arg: "--yes=true"},
		{name: "dry run", arg: "--dry-run"},
		{name: "dry run assignment", arg: "--dry-run=true"},
		{name: "resume", arg: "--resume"},
		{name: "resume assignment", arg: "--resume=true"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeWorkflowRunTestSpec(t, workflowRunSpec{Steps: []workflowRunStepSpec{
				{Args: []string{"version", test.arg}},
			}})
			_, err := readWorkflowRunSpec(path)
			if err == nil {
				t.Fatalf("readWorkflowRunSpec(%q) succeeded, want workflow-owned flag rejection", test.arg)
			}
			if !strings.Contains(err.Error(), "workflow owns approval: pass --yes to zotio workflow run itself") {
				t.Fatalf("error = %v, want workflow approval ownership message", err)
			}
		})
	}
}

func TestWorkflowRunApplyReportHasRunID(t *testing.T) {
	report, stderr, err := runWorkflowRunApplyTestCmd(t, workflowRunSpec{Steps: []workflowRunStepSpec{
		{Args: []string{"version"}},
	}})
	if err != nil {
		t.Fatalf("apply workflow: %v; stderr=%s", err, stderr)
	}
	if !report.OK || report.Mode != workflowRunModeApply || report.RunID == "" {
		t.Fatalf("apply report = %+v, want successful apply with run ID", report)
	}
}

func TestWorkflowRunApplyLeavesCheckpointAfterFailure(t *testing.T) {
	spec := workflowRunSpec{
		Vars: map[string]string{"CONFIG": "from-spec"},
		Steps: []workflowRunStepSpec{
			{Name: "source", Args: []string{"version", "--config=${vars.CONFIG}"}},
			{Args: []string{"definitely-not-a-command"}},
		},
	}
	path := writeWorkflowRunTestSpec(t, spec)
	report, stderr, err := runWorkflowRunTestCmdAtPathWithVars(t, path, true, false, []string{"CONFIG=override"})
	if err == nil {
		t.Fatal("apply workflow succeeded, want failure")
	}
	if report.RunID == "" {
		t.Fatalf("apply report has empty run ID: %+v; stderr=%s", report, stderr)
	}

	checkpoint, err := readWorkflowRunCheckpoint(workflowRunCheckpointPath(path))
	if err != nil {
		t.Fatalf("read workflow checkpoint: %v", err)
	}
	if checkpoint.RunID != report.RunID {
		t.Fatalf("checkpoint run ID = %q, want %q", checkpoint.RunID, report.RunID)
	}
	if checkpoint.SchemaVersion != workflowRunCheckpointSchemaVersion || checkpoint.SpecSHA256 == "" {
		t.Fatalf("checkpoint = %+v, want schema version and spec hash", checkpoint)
	}
	if checkpoint.Vars["CONFIG"] != "override" {
		t.Fatalf("checkpoint vars = %v, want resolved override", checkpoint.Vars)
	}
	if len(checkpoint.Completed) != 2 {
		t.Fatalf("checkpoint completed = %v, want completed source and failed step", checkpoint.Completed)
	}
	completed := checkpoint.Completed[0]
	if completed.Index != 1 || completed.Name != "source" || completed.Status != "ok" || completed.Output != report.Steps[0].Output {
		t.Fatalf("checkpoint completed step = %+v, want captured successful source step", completed)
	}
	failed := checkpoint.Completed[1]
	if failed.Index != 2 || failed.Status != "failed" {
		t.Fatalf("checkpoint second step = %+v, want failed step", failed)
	}
}

func TestWorkflowRunApplyRemovesCheckpointAfterSuccess(t *testing.T) {
	spec := workflowRunSpec{Steps: []workflowRunStepSpec{
		{Args: []string{"version"}},
		{Args: []string{"--help"}},
	}}
	path := writeWorkflowRunTestSpec(t, spec)
	report, stderr, err := runWorkflowRunTestCmdAtPath(t, path, true, false)
	if err != nil {
		t.Fatalf("apply workflow: %v; stderr=%s", err, stderr)
	}
	if !report.OK || report.Mode != workflowRunModeApply || report.RunID == "" {
		t.Fatalf("apply report = %+v, want successful apply with run ID", report)
	}
	if _, err := os.Stat(workflowRunCheckpointPath(path)); !os.IsNotExist(err) {
		t.Fatalf("checkpoint stat error = %v, want no checkpoint", err)
	}
}

func TestRunWorkflowRunFilePreviewDoesNotCheckpoint(t *testing.T) {
	path := writeWorkflowRunTestSpec(t, workflowRunSpec{Steps: []workflowRunStepSpec{
		{Args: []string{"version"}},
	}})

	report, err := runWorkflowRunFile(context.Background(), path, workflowRunInvocation{})
	if err != nil {
		t.Fatalf("preview workflow: %v", err)
	}
	if !report.OK || report.Mode != workflowRunModePreview || report.RunID != "" {
		t.Fatalf("preview report = %+v, want successful preview", report)
	}
	if _, err := os.Stat(workflowRunCheckpointPath(path)); !os.IsNotExist(err) {
		t.Fatalf("checkpoint stat error = %v, want no checkpoint", err)
	}
}

func TestRunWorkflowRunFileApplyFailureReturnsReportAndKeepsCheckpoint(t *testing.T) {
	path := writeWorkflowRunTestSpec(t, workflowRunSpec{Steps: []workflowRunStepSpec{
		{Args: []string{"definitely-not-a-command"}},
	}})

	report, err := runWorkflowRunFile(context.Background(), path, workflowRunInvocation{Yes: true})
	if err == nil {
		t.Fatal("apply workflow succeeded, want failure")
	}
	if report.OK || report.Mode != workflowRunModeApply || report.RunID == "" || len(report.Steps) != 1 || report.Steps[0].Status != "failed" {
		t.Fatalf("apply report = %+v, want failed apply report with run ID", report)
	}
	if _, err := os.Stat(workflowRunCheckpointPath(path)); err != nil {
		t.Fatalf("checkpoint stat error = %v, want checkpoint", err)
	}
}

func TestRunWorkflowRunFileResumeWithoutApprovalReturnsUsageError(t *testing.T) {
	path := writeWorkflowRunTestSpec(t, workflowRunSpec{Steps: []workflowRunStepSpec{
		{Args: []string{"version"}},
	}})

	_, err := runWorkflowRunFile(context.Background(), path, workflowRunInvocation{Resume: true})
	if err == nil {
		t.Fatal("preview resume succeeded, want usage error")
	}
	if ExitCode(err) != 2 || !strings.Contains(err.Error(), "requires --yes") {
		t.Fatalf("error = %v, want --resume usage error requiring --yes", err)
	}
}

func TestWorkflowRunResumeRequiresApplyMode(t *testing.T) {
	path := writeWorkflowRunTestSpec(t, workflowRunSpec{Steps: []workflowRunStepSpec{
		{Args: []string{"version"}},
	}})
	_, _, err := runWorkflowRunTestCmdAtPath(t, path, false, true)
	if err == nil {
		t.Fatal("preview resume succeeded, want usage error")
	}
	if ExitCode(err) != 2 || !strings.Contains(err.Error(), "requires --yes") {
		t.Fatalf("error = %v, want --resume usage error requiring --yes", err)
	}
}

func TestWorkflowRunResumeRequiresCheckpoint(t *testing.T) {
	path := writeWorkflowRunTestSpec(t, workflowRunSpec{Steps: []workflowRunStepSpec{
		{Args: []string{"version"}},
	}})
	_, _, err := runWorkflowRunTestCmdAtPath(t, path, true, true)
	if err == nil {
		t.Fatal("resume without checkpoint succeeded")
	}
	if !strings.Contains(err.Error(), "requires an existing checkpoint") {
		t.Fatalf("error = %v, want missing checkpoint error", err)
	}
}

func TestWorkflowRunResumeRejectsChangedSpec(t *testing.T) {
	path := writeWorkflowRunTestSpec(t, workflowRunSpec{Steps: []workflowRunStepSpec{
		{Args: []string{"version"}},
	}})
	checkpoint := workflowRunCheckpoint{
		SchemaVersion: workflowRunCheckpointSchemaVersion,
		RunID:         "workflow-run-id",
		SpecSHA256:    "changed",
	}
	if err := writeWorkflowRunCheckpoint(workflowRunCheckpointPath(path), checkpoint); err != nil {
		t.Fatalf("write workflow checkpoint: %v", err)
	}

	_, _, err := runWorkflowRunTestCmdAtPath(t, path, true, true)
	if err == nil {
		t.Fatal("resume with changed spec succeeded")
	}
	if !strings.Contains(err.Error(), "spec changed since the checkpoint") || !strings.Contains(err.Error(), "delete") {
		t.Fatalf("error = %v, want changed-spec recovery instruction", err)
	}
}

func TestWorkflowRunResumeSkipsCompletedStepsAndReusesRunID(t *testing.T) {
	spec := workflowRunSpec{Steps: []workflowRunStepSpec{
		{Args: []string{"version"}},
		{Args: []string{"--help"}},
	}}
	path := writeWorkflowRunTestSpec(t, spec)
	_, rawSpec, err := readWorkflowRunSpecFile(path)
	if err != nil {
		t.Fatalf("read workflow spec: %v", err)
	}
	checkpoint := workflowRunCheckpoint{
		SchemaVersion: workflowRunCheckpointSchemaVersion,
		RunID:         "workflow-run-id",
		SpecSHA256:    workflowRunSpecSHA256(rawSpec),
		Completed: []workflowRunCheckpointStep{{
			Index:  1,
			Status: "ok",
			Output: "zotio test\n",
		}},
	}
	if err := writeWorkflowRunCheckpoint(workflowRunCheckpointPath(path), checkpoint); err != nil {
		t.Fatalf("write workflow checkpoint: %v", err)
	}

	report, stderr, err := runWorkflowRunTestCmdAtPath(t, path, true, true)
	if err != nil {
		t.Fatalf("resume workflow: %v; stderr=%s", err, stderr)
	}
	if !report.OK || report.Mode != workflowRunModeApply || report.RunID != checkpoint.RunID {
		t.Fatalf("resume report = %+v, want successful apply with reused run ID", report)
	}
	if len(report.Steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(report.Steps))
	}
	if !report.Steps[0].OK || report.Steps[0].Status != "skipped" || report.Steps[0].Reason != "resume" {
		t.Fatalf("first step = %+v, want resume-skipped", report.Steps[0])
	}
	if !report.Steps[1].OK || report.Steps[1].Status != "ok" {
		t.Fatalf("second step = %+v, want executed ok", report.Steps[1])
	}
}

func TestWorkflowRunResumeRetriesFailedStep(t *testing.T) {
	spec := workflowRunSpec{Steps: []workflowRunStepSpec{
		{Args: []string{"version"}},
		{Args: []string{"--help"}},
	}}
	path := writeWorkflowRunTestSpec(t, spec)
	_, rawSpec, err := readWorkflowRunSpecFile(path)
	if err != nil {
		t.Fatalf("read workflow spec: %v", err)
	}
	checkpoint := workflowRunCheckpoint{
		SchemaVersion: workflowRunCheckpointSchemaVersion,
		RunID:         "workflow-run-id",
		SpecSHA256:    workflowRunSpecSHA256(rawSpec),
		Completed: []workflowRunCheckpointStep{
			{Index: 1, Status: "ok", Output: "zotio test\n"},
			{Index: 2, Status: "failed"},
		},
	}
	if err := writeWorkflowRunCheckpoint(workflowRunCheckpointPath(path), checkpoint); err != nil {
		t.Fatalf("write workflow checkpoint: %v", err)
	}

	report, stderr, err := runWorkflowRunTestCmdAtPath(t, path, true, true)
	if err != nil {
		t.Fatalf("resume workflow: %v; stderr=%s", err, stderr)
	}
	if len(report.Steps) != 2 || report.Steps[0].Status != "skipped" || report.Steps[1].Status != "ok" {
		t.Fatalf("resume report = %+v, want completed first step skipped and failed step retried", report)
	}
}

func TestWorkflowRunApplyRefusesInterruptedRunWithoutResume(t *testing.T) {
	path := writeWorkflowRunTestSpec(t, workflowRunSpec{Steps: []workflowRunStepSpec{
		{Args: []string{"version"}},
	}})
	checkpoint := workflowRunCheckpoint{
		SchemaVersion: workflowRunCheckpointSchemaVersion,
		RunID:         "workflow-run-id",
		SpecSHA256:    "unchanged",
	}
	if err := writeWorkflowRunCheckpoint(workflowRunCheckpointPath(path), checkpoint); err != nil {
		t.Fatalf("write workflow checkpoint: %v", err)
	}

	_, _, err := runWorkflowRunTestCmdAtPath(t, path, true, false)
	if err == nil {
		t.Fatal("second apply succeeded while checkpoint exists")
	}
	if !strings.Contains(err.Error(), "interrupted workflow run exists") || !strings.Contains(err.Error(), "--resume") || !strings.Contains(err.Error(), workflowRunCheckpointPath(path)) {
		t.Fatalf("error = %v, want interrupted-run recovery instruction", err)
	}
}

func TestWorkflowRunSpecValidation(t *testing.T) {
	tests := []struct {
		name string
		spec workflowRunSpec
		want string
	}{
		{
			name: "bad variable name",
			spec: workflowRunSpec{
				Vars:  map[string]string{"bad.name": "value"},
				Steps: []workflowRunStepSpec{{Args: []string{"version"}}},
			},
			want: "invalid name",
		},
		{
			name: "duplicate step names",
			spec: workflowRunSpec{Steps: []workflowRunStepSpec{
				{Name: "source", Args: []string{"version"}},
				{Name: "source", Args: []string{"version"}},
			}},
			want: "duplicate name",
		},
		{
			name: "unknown stdin source",
			spec: workflowRunSpec{Steps: []workflowRunStepSpec{
				{Name: "consumer", Args: []string{"version"}, StdinFrom: "missing"},
			}},
			want: "workflow step 1 stdin_from",
		},
		{
			name: "forward stdin source",
			spec: workflowRunSpec{Steps: []workflowRunStepSpec{
				{Name: "consumer", Args: []string{"version"}, StdinFrom: "source"},
				{Name: "source", Args: []string{"version"}},
			}},
			want: "workflow step 1 stdin_from",
		},
		{
			name: "self stdin source",
			spec: workflowRunSpec{Steps: []workflowRunStepSpec{
				{Name: "source", Args: []string{"version"}, StdinFrom: "source"},
			}},
			want: "workflow step 1 stdin_from",
		},
		{
			name: "unknown when source",
			spec: workflowRunSpec{Steps: []workflowRunStepSpec{
				{Name: "consumer", Args: []string{"version"}, When: &workflowRunStepWhen{Step: "missing", Is: "ok"}},
			}},
			want: "workflow step 1 when.step",
		},
		{
			name: "forward when source",
			spec: workflowRunSpec{Steps: []workflowRunStepSpec{
				{Name: "consumer", Args: []string{"version"}, When: &workflowRunStepWhen{Step: "source", Is: "ok"}},
				{Name: "source", Args: []string{"version"}},
			}},
			want: "workflow step 1 when.step",
		},
		{
			name: "self when source",
			spec: workflowRunSpec{Steps: []workflowRunStepSpec{
				{Name: "source", Args: []string{"version"}, When: &workflowRunStepWhen{Step: "source", Is: "ok"}},
			}},
			want: "workflow step 1 when.step",
		},
		{
			name: "invalid when status",
			spec: workflowRunSpec{Steps: []workflowRunStepSpec{
				{Name: "source", Args: []string{"version"}},
				{Args: []string{"version"}, When: &workflowRunStepWhen{Step: "source", Is: "unknown"}},
			}},
			want: "when.is",
		},
		{
			name: "malformed placeholder",
			spec: workflowRunSpec{
				Vars:  map[string]string{"NAME": "value"},
				Steps: []workflowRunStepSpec{{Args: []string{"version", "${vars.NAME"}}},
			},
			want: "invalid placeholder",
		},
		{
			name: "unknown variable placeholder",
			spec: workflowRunSpec{Steps: []workflowRunStepSpec{
				{Args: []string{"version", "${vars.MISSING}"}},
			}},
			want: "invalid placeholder",
		},
		{
			name: "unknown step placeholder",
			spec: workflowRunSpec{Steps: []workflowRunStepSpec{
				{Args: []string{"version", "${steps.missing.output}"}},
			}},
			want: "invalid placeholder",
		},
		{
			name: "forward step placeholder",
			spec: workflowRunSpec{Steps: []workflowRunStepSpec{
				{Name: "consumer", Args: []string{"version", "${steps.source.output}"}},
				{Name: "source", Args: []string{"version"}},
			}},
			want: "invalid placeholder",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeWorkflowRunTestSpec(t, test.spec)
			_, err := readWorkflowRunSpec(path)
			if err == nil {
				t.Fatalf("readWorkflowRunSpec succeeded, want %s rejection", test.name)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestWorkflowRunSubstitutesVarsAndOverrides(t *testing.T) {
	spec := workflowRunSpec{
		Vars: map[string]string{"CONFIG": "from-spec"},
		Steps: []workflowRunStepSpec{
			{Args: []string{"version", "--config=${vars.CONFIG}"}},
		},
	}
	tests := []struct {
		name      string
		overrides []string
		want      string
	}{
		{name: "spec value", want: "from-spec"},
		{name: "override", overrides: []string{"CONFIG=from-flag"}, want: "from-flag"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeWorkflowRunTestSpec(t, spec)
			report, stderr, err := runWorkflowRunTestCmdAtPathWithVars(t, path, false, false, test.overrides)
			if err != nil {
				t.Fatalf("workflow run: %v; stderr=%s", err, stderr)
			}
			if len(report.Steps) != 1 || report.Steps[0].Status != "ok" {
				t.Fatalf("report = %+v, want successful step", report)
			}
			if got, want := report.Steps[0].Args, []string{"version", "--config=" + test.want}; strings.Join(got, "\x00") != strings.Join(want, "\x00") {
				t.Fatalf("executed args = %q, want %q", got, want)
			}
		})
	}
}

func TestWorkflowRunRejectsInvalidVarOverrides(t *testing.T) {
	spec := workflowRunSpec{
		Vars:  map[string]string{"CONFIG": "from-spec"},
		Steps: []workflowRunStepSpec{{Args: []string{"version"}}},
	}
	tests := []struct {
		name      string
		overrides []string
		want      string
	}{
		{name: "undeclared", overrides: []string{"MISSING=value"}, want: "not declared"},
		{name: "missing equals", overrides: []string{"CONFIG"}, want: "must be NAME=value"},
		{name: "empty name", overrides: []string{"=value"}, want: "must be NAME=value"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeWorkflowRunTestSpec(t, spec)
			_, _, err := runWorkflowRunTestCmdAtPathWithVars(t, path, false, false, test.overrides)
			if err == nil {
				t.Fatal("workflow run succeeded, want usage error")
			}
			if ExitCode(err) != 2 || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want usage error containing %q", err, test.want)
			}
		})
	}
}

func TestWorkflowRunSubstitutesEarlierStepOutput(t *testing.T) {
	report, stderr, err := runWorkflowRunTestCmd(t, workflowRunSpec{Steps: []workflowRunStepSpec{
		{Name: "source", Args: []string{"version"}},
		{Name: "consumer", Args: []string{"version", "--config=${steps.source.output}"}},
	}})
	if err != nil {
		t.Fatalf("workflow run: %v; stderr=%s", err, stderr)
	}
	if len(report.Steps) != 2 || report.Steps[0].Status != "ok" || report.Steps[1].Status != "ok" {
		t.Fatalf("report = %+v, want two successful steps", report)
	}
	if got, want := report.Steps[1].Args, []string{"version", "--config=" + strings.TrimSpace(report.Steps[0].Output)}; strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("consumer args = %q, want trimmed source output %q", got, want)
	}
}

func TestExecuteWorkflowRunStepPassesStdin(t *testing.T) {
	root := &cobra.Command{
		Use: "stdin",
		RunE: func(cmd *cobra.Command, _ []string) error {
			data, err := io.ReadAll(cmd.InOrStdin())
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(data)
			return err
		},
	}
	sourceOutput := " raw output\n"
	stdin, err := workflowRunAvailableStepOutput("source", map[string]workflowRunStepResult{
		"source": {Status: "ok", Output: sourceOutput},
	})
	if err != nil {
		t.Fatalf("resolve workflow stdin: %v", err)
	}
	output, _, err := executeWorkflowRunStepWithRoot(context.Background(), root, nil, &stdin)
	if err != nil {
		t.Fatalf("execute workflow step: %v", err)
	}
	if output != sourceOutput {
		t.Fatalf("workflow stdin output = %q, want raw source output", output)
	}
}

func TestWorkflowRunWhenConditions(t *testing.T) {
	t.Run("true runs", func(t *testing.T) {
		report, stderr, err := runWorkflowRunTestCmd(t, workflowRunSpec{Steps: []workflowRunStepSpec{
			{Name: "source", Args: []string{"version"}},
			{Name: "conditional", Args: []string{"version"}, When: &workflowRunStepWhen{Step: "source", Is: "ok"}},
		}})
		if err != nil {
			t.Fatalf("workflow run: %v; stderr=%s", err, stderr)
		}
		if report.Steps[1].Status != "ok" || !report.Steps[1].OK {
			t.Fatalf("conditional step = %+v, want executed ok", report.Steps[1])
		}
	})

	t.Run("false skips", func(t *testing.T) {
		report, _, err := runWorkflowRunTestCmd(t, workflowRunSpec{
			ContinueOnError: true,
			Steps: []workflowRunStepSpec{
				{Name: "source", Args: []string{"definitely-not-a-command"}},
				{Name: "conditional", Args: []string{"version"}, When: &workflowRunStepWhen{Step: "source", Is: "ok"}},
			},
		})
		if err == nil {
			t.Fatal("workflow run succeeded, want source failure")
		}
		if report.Steps[1].Status != "skipped" || !report.Steps[1].OK || report.Steps[1].Reason != "when: source is failed, want ok" {
			t.Fatalf("conditional step = %+v, want condition skip", report.Steps[1])
		}
	})

	t.Run("fail fast does not resurrect", func(t *testing.T) {
		report, _, err := runWorkflowRunTestCmd(t, workflowRunSpec{Steps: []workflowRunStepSpec{
			{Name: "source", Args: []string{"definitely-not-a-command"}},
			{Name: "conditional", Args: []string{"version"}, When: &workflowRunStepWhen{Step: "source", Is: "failed"}},
		}})
		if err == nil {
			t.Fatal("workflow run succeeded, want source failure")
		}
		if report.Steps[1].Status != "not_attempted" || report.Steps[1].OK {
			t.Fatalf("conditional step = %+v, want not attempted after fail-fast", report.Steps[1])
		}
	})
}

func TestWorkflowRunResumeRejectsChangedVariables(t *testing.T) {
	spec := workflowRunSpec{
		Vars:  map[string]string{"CONFIG": "from-spec"},
		Steps: []workflowRunStepSpec{{Args: []string{"version", "--config=${vars.CONFIG}"}}},
	}
	path := writeWorkflowRunTestSpec(t, spec)
	_, rawSpec, err := readWorkflowRunSpecFile(path)
	if err != nil {
		t.Fatalf("read workflow spec: %v", err)
	}
	checkpoint := workflowRunCheckpoint{
		SchemaVersion: workflowRunCheckpointSchemaVersion,
		RunID:         "workflow-run-id",
		SpecSHA256:    workflowRunSpecSHA256(rawSpec),
		Vars:          map[string]string{"CONFIG": "from-spec"},
	}
	if err := writeWorkflowRunCheckpoint(workflowRunCheckpointPath(path), checkpoint); err != nil {
		t.Fatalf("write workflow checkpoint: %v", err)
	}

	_, _, err = runWorkflowRunTestCmdAtPathWithVars(t, path, true, true, []string{"CONFIG=override"})
	if err == nil {
		t.Fatal("resume with changed variables succeeded")
	}
	if !strings.Contains(err.Error(), "approved with different variables") || !strings.Contains(err.Error(), "delete") {
		t.Fatalf("error = %v, want changed-variable recovery instruction", err)
	}
}

func TestWorkflowRunResumeRejectsV1Checkpoint(t *testing.T) {
	path := writeWorkflowRunTestSpec(t, workflowRunSpec{Steps: []workflowRunStepSpec{
		{Args: []string{"version"}},
	}})
	_, rawSpec, err := readWorkflowRunSpecFile(path)
	if err != nil {
		t.Fatalf("read workflow spec: %v", err)
	}
	v1 := []byte(`{"schema_version":1,"run_id":"workflow-run-id","spec_sha256":"` + workflowRunSpecSHA256(rawSpec) + `","completed":[1]}`)
	if err := os.WriteFile(workflowRunCheckpointPath(path), v1, 0o600); err != nil {
		t.Fatalf("write v1 workflow checkpoint: %v", err)
	}

	_, _, err = runWorkflowRunTestCmdAtPath(t, path, true, true)
	if err == nil {
		t.Fatal("resume with v1 checkpoint succeeded")
	}
	if !strings.Contains(err.Error(), "unsupported schema version 1; delete it to start over") {
		t.Fatalf("error = %v, want v1 delete-to-start-over instruction", err)
	}
}

func TestWorkflowRunResumeHydratesCompletedOutput(t *testing.T) {
	spec := workflowRunSpec{Steps: []workflowRunStepSpec{
		{Name: "source", Args: []string{"version"}},
		{Name: "consumer", Args: []string{"version", "--config=${steps.source.output}"}},
	}}
	path := writeWorkflowRunTestSpec(t, spec)
	_, rawSpec, err := readWorkflowRunSpecFile(path)
	if err != nil {
		t.Fatalf("read workflow spec: %v", err)
	}
	checkpoint := workflowRunCheckpoint{
		SchemaVersion: workflowRunCheckpointSchemaVersion,
		RunID:         "workflow-run-id",
		SpecSHA256:    workflowRunSpecSHA256(rawSpec),
		Completed: []workflowRunCheckpointStep{{
			Index:  1,
			Name:   "source",
			Status: "ok",
			Output: "  resumed output \n",
		}},
	}
	if err := writeWorkflowRunCheckpoint(workflowRunCheckpointPath(path), checkpoint); err != nil {
		t.Fatalf("write workflow checkpoint: %v", err)
	}

	report, stderr, err := runWorkflowRunTestCmdAtPath(t, path, true, true)
	if err != nil {
		t.Fatalf("resume workflow: %v; stderr=%s", err, stderr)
	}
	if report.Steps[0].Reason != "resume" || report.Steps[1].Status != "ok" {
		t.Fatalf("resume report = %+v, want resumed source and executed consumer", report)
	}
	if got, want := report.Steps[1].Args, []string{"version", "--config=resumed output"}; strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("consumer args = %q, want hydrated output %q", got, want)
	}
}

func TestWorkflowRunConditionSkipIsCheckpointedBesideFailedStep(t *testing.T) {
	spec := workflowRunSpec{
		ContinueOnError: true,
		Steps: []workflowRunStepSpec{
			{Name: "source", Args: []string{"definitely-not-a-command"}},
			{Name: "conditional", Args: []string{"version"}, When: &workflowRunStepWhen{Step: "source", Is: "ok"}},
		},
	}
	path := writeWorkflowRunTestSpec(t, spec)
	report, _, err := runWorkflowRunTestCmdAtPath(t, path, true, false)
	if err == nil {
		t.Fatal("apply workflow succeeded, want source failure")
	}
	if report.Steps[1].Status != "skipped" || report.Steps[1].Reason != "when: source is failed, want ok" {
		t.Fatalf("conditional report = %+v, want condition skip", report.Steps[1])
	}
	checkpoint, err := readWorkflowRunCheckpoint(workflowRunCheckpointPath(path))
	if err != nil {
		t.Fatalf("read workflow checkpoint: %v", err)
	}
	if len(checkpoint.Completed) != 2 ||
		checkpoint.Completed[0].Index != 1 || checkpoint.Completed[0].Status != "failed" ||
		checkpoint.Completed[1].Index != 2 || checkpoint.Completed[1].Status != "skipped" {
		t.Fatalf("checkpoint completed = %+v, want failed source and condition-skipped step", checkpoint.Completed)
	}
}

func TestRenderWorkflowRunReportPrintsModeAndRunID(t *testing.T) {
	cmd := RootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	report := workflowRunReport{
		Mode:  workflowRunModeApply,
		RunID: "workflow-run-id",
		Steps: []workflowRunStepReport{{
			Index:  1,
			Args:   []string{"version"},
			OK:     true,
			Status: "skipped",
		}},
	}
	if err := renderWorkflowRunReport(cmd, &rootFlags{}, report); err != nil {
		t.Fatalf("render workflow report: %v", err)
	}
	want := "mode\tapply\trun_id\tworkflow-run-id\n1\tversion\tskipped\n"
	if out.String() != want {
		t.Fatalf("rendered workflow report = %q, want %q", out.String(), want)
	}
}

func TestExecuteWorkflowRunStepDoesNotRaceWithConcurrentStdoutWriter(t *testing.T) {
	started := make(chan struct{})
	stop := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)
		close(started)
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = os.Stdout.Write(nil)
			}
		}
	}()
	<-started
	defer func() {
		close(stop)
		<-done
	}()

	for range 25 {
		output, _, err := executeWorkflowRunStep(context.Background(), []string{"--help"}, nil)
		if err != nil {
			t.Fatalf("executeWorkflowRunStep --help: %v", err)
		}
		if !strings.Contains(output, "Zotero automation CLI") {
			t.Fatalf("workflow step output = %q, want root help in Cobra buffer", output)
		}
	}
}

func TestWorkflowRunDryRunWinsOverYes(t *testing.T) {
	path := writeWorkflowRunTestSpec(t, workflowRunSpec{Steps: []workflowRunStepSpec{
		{Args: []string{"version"}},
	}})

	report, stderr, err := runWorkflowRunTestCmdAtPathWithInvocation(t, path, workflowRunInvocation{Yes: true, DryRun: true})
	if err != nil {
		t.Fatalf("workflow --yes --dry-run: %v; stderr=%s", err, stderr)
	}
	if !report.OK || report.Mode != workflowRunModePreview || report.RunID != "" {
		t.Fatalf("report = %+v, want successful preview without run ID", report)
	}
	if _, err := os.Stat(workflowRunCheckpointPath(path)); !os.IsNotExist(err) {
		t.Fatalf("checkpoint stat error = %v, want no checkpoint for dry-run", err)
	}
}

func TestWorkflowRunRejectsFlagsResolvedFromSubstitution(t *testing.T) {
	tests := []struct {
		name string
		spec workflowRunSpec
		want string
	}{
		{
			name: "flag",
			spec: workflowRunSpec{
				Vars:  map[string]string{"VALUE": "--yes"},
				Steps: []workflowRunStepSpec{{Args: []string{"version", "${vars.VALUE}"}}},
			},
			want: "--yes",
		},
		{
			name: "nested workflow",
			spec: workflowRunSpec{
				Vars:  map[string]string{"COMMAND": "workflow"},
				Steps: []workflowRunStepSpec{{Args: []string{"${vars.COMMAND}", "run", "nested.json"}}},
			},
			want: "workflow",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeWorkflowRunTestSpec(t, test.spec)
			report, _, err := runWorkflowRunTestCmdAtPathWithInvocation(t, path, workflowRunInvocation{})
			if err == nil {
				t.Fatal("workflow run succeeded, want resolved argument rejection")
			}
			if len(report.Steps) != 1 || report.Steps[0].Status != "failed" {
				t.Fatalf("report = %+v, want one failed step", report)
			}
			if !strings.Contains(report.Steps[0].Error, "workflow step 1") || !strings.Contains(report.Steps[0].Error, test.want) {
				t.Fatalf("step error = %q, want step and offending token %q", report.Steps[0].Error, test.want)
			}
		})
	}
}

func TestWorkflowRunDataflowUsesFullStdoutWithoutStderr(t *testing.T) {
	payload := strings.Repeat("x", workflowRunOutputLimit+1)
	newRoot := func() *cobra.Command {
		root := &cobra.Command{Use: "test"}
		root.PersistentFlags().Bool("yes", false, "accept")
		root.AddCommand(
			&cobra.Command{
				Use:         "producer",
				Annotations: map[string]string{"mcp:read-only": "true"},
				RunE: func(cmd *cobra.Command, _ []string) error {
					if _, err := cmd.OutOrStdout().Write([]byte(payload)); err != nil {
						return err
					}
					_, err := cmd.ErrOrStderr().Write([]byte("producer diagnostic"))
					return err
				},
			},
			&cobra.Command{
				Use:         "consumer <output>",
				Args:        cobra.ExactArgs(1),
				Annotations: map[string]string{"mcp:read-only": "true"},
				RunE: func(cmd *cobra.Command, args []string) error {
					if args[0] != payload {
						t.Errorf("consumer argument length = %d, want full producer output length %d", len(args[0]), len(payload))
					}
					stdin, err := io.ReadAll(cmd.InOrStdin())
					if err != nil {
						return err
					}
					if string(stdin) != payload {
						t.Errorf("consumer stdin length = %d, want stdout-only producer output length %d", len(stdin), len(payload))
					}
					_, err = cmd.OutOrStdout().Write([]byte("consumed"))
					return err
				},
			},
		)
		return root
	}
	checkpoint := workflowRunCheckpoint{}
	execution := workflowRunExecution{
		Mode:           workflowRunModeApply,
		Checkpoint:     &checkpoint,
		CheckpointPath: filepath.Join(t.TempDir(), "workflow.checkpoint.json"),
	}
	report, err := executeWorkflowRunSpecWithRootFactory(context.Background(), workflowRunSpec{Steps: []workflowRunStepSpec{
		{Name: "source", Args: []string{"producer"}},
		{Name: "consumer", Args: []string{"consumer", "${steps.source.output}"}, StdinFrom: "source"},
	}}, execution, newRoot)
	if err != nil {
		t.Fatalf("execute workflow dataflow: %v", err)
	}
	if !report.OK || len(report.Steps) != 2 {
		t.Fatalf("report = %+v, want two successful steps", report)
	}
	if len(report.Steps[0].Output) != workflowRunOutputLimit || !strings.Contains(report.Steps[0].Output, "[workflow output truncated]") {
		t.Fatalf("rendered source output length = %d, want capped output", len(report.Steps[0].Output))
	}
	if len(checkpoint.Completed) != 2 || checkpoint.Completed[0].Output != payload {
		t.Fatalf("checkpoint source output length = %d, want full output length %d", len(checkpoint.Completed[0].Output), len(payload))
	}
}

func TestWorkflowRunResumeRefusesInProgressStep(t *testing.T) {
	spec := workflowRunSpec{Steps: []workflowRunStepSpec{
		{Name: "mutate", Args: []string{"version"}},
	}}
	path := writeWorkflowRunTestSpec(t, spec)
	_, rawSpec, err := readWorkflowRunSpecFile(path)
	if err != nil {
		t.Fatalf("read workflow spec: %v", err)
	}
	checkpoint := workflowRunCheckpoint{
		SchemaVersion: workflowRunCheckpointSchemaVersion,
		RunID:         "workflow-run-id",
		SpecSHA256:    workflowRunSpecSHA256(rawSpec),
		Completed: []workflowRunCheckpointStep{{
			Index:  1,
			Name:   "mutate",
			Status: "in_progress",
		}},
	}
	if err := writeWorkflowRunCheckpoint(workflowRunCheckpointPath(path), checkpoint); err != nil {
		t.Fatalf("write workflow checkpoint: %v", err)
	}

	_, err = runWorkflowRunFile(context.Background(), path, workflowRunInvocation{Yes: true, Resume: true})
	if err == nil {
		t.Fatal("resume succeeded with an in-progress step")
	}
	if !strings.Contains(err.Error(), "step 1") || !strings.Contains(err.Error(), "inspect or reconcile") || !strings.Contains(err.Error(), "delete") {
		t.Fatalf("resume error = %v, want reconciliation and restart guidance", err)
	}
}

func TestWorkflowRunResumeKeepsConditionallySkippedOutputUnavailable(t *testing.T) {
	spec := workflowRunSpec{Steps: []workflowRunStepSpec{
		{Name: "source", Args: []string{"version"}},
		{Name: "conditional", Args: []string{"version"}, When: &workflowRunStepWhen{Step: "source", Is: "failed"}},
		{Name: "consumer", Args: []string{"version", "--config=${steps.conditional.output}"}},
	}}
	path := writeWorkflowRunTestSpec(t, spec)
	_, rawSpec, err := readWorkflowRunSpecFile(path)
	if err != nil {
		t.Fatalf("read workflow spec: %v", err)
	}
	checkpoint := workflowRunCheckpoint{
		SchemaVersion: workflowRunCheckpointSchemaVersion,
		RunID:         "workflow-run-id",
		SpecSHA256:    workflowRunSpecSHA256(rawSpec),
		Completed: []workflowRunCheckpointStep{
			{Index: 1, Name: "source", Status: "ok", Output: "source output"},
			{Index: 2, Name: "conditional", Status: "skipped"},
		},
	}
	if err := writeWorkflowRunCheckpoint(workflowRunCheckpointPath(path), checkpoint); err != nil {
		t.Fatalf("write workflow checkpoint: %v", err)
	}

	report, err := runWorkflowRunFile(context.Background(), path, workflowRunInvocation{Yes: true, Resume: true})
	if err == nil {
		t.Fatal("resume succeeded with unavailable conditional output")
	}
	if len(report.Steps) != 3 || report.Steps[2].Status != "failed" || !strings.Contains(report.Steps[2].Error, `unavailable (status "skipped")`) {
		t.Fatalf("report = %+v, want consumer failure for skipped output", report)
	}
}

func TestExecuteWorkflowRunSpecWithRunIDRestoresOuterRunID(t *testing.T) {
	previousRunID := activeWorkflowRunID
	activeWorkflowRunID = "outer-run-id"
	t.Cleanup(func() {
		activeWorkflowRunID = previousRunID
	})

	_, err := executeWorkflowRunSpecWithRunID(context.Background(), workflowRunSpec{Steps: []workflowRunStepSpec{
		{Args: []string{"version"}},
	}}, workflowRunExecution{Mode: workflowRunModePreview, RunID: "inner-run-id"})
	if err != nil {
		t.Fatalf("execute nested workflow: %v", err)
	}
	if activeWorkflowRunID != "outer-run-id" {
		t.Fatalf("active workflow run ID = %q, want outer run ID restored", activeWorkflowRunID)
	}
}

func TestWorkflowRunPropagatesAgentAndNoInputToSteps(t *testing.T) {
	newRoot := func() *cobra.Command {
		var agent bool
		var noInput bool
		root := &cobra.Command{Use: "test"}
		root.PersistentFlags().BoolVar(&agent, "agent", false, "agent")
		root.PersistentFlags().BoolVar(&noInput, "no-input", false, "no input")
		root.AddCommand(&cobra.Command{
			Use:         "step",
			Annotations: map[string]string{"mcp:read-only": "true"},
			RunE: func(_ *cobra.Command, _ []string) error {
				if !agent || !noInput {
					t.Errorf("step flags agent=%t noInput=%t, want both true", agent, noInput)
				}
				return nil
			},
		})
		return root
	}
	report, err := executeWorkflowRunSpecWithRootFactory(context.Background(), workflowRunSpec{Steps: []workflowRunStepSpec{
		{Args: []string{"step"}},
	}}, workflowRunExecution{
		Mode:    workflowRunModePreview,
		Agent:   true,
		NoInput: true,
	}, newRoot)
	if err != nil {
		t.Fatalf("execute agent workflow: %v", err)
	}
	if !report.OK || report.Steps[0].Status != "ok" {
		t.Fatalf("report = %+v, want successful propagated step", report)
	}
}

func TestInstallRuntimeHooksInstallsProductionHooks(t *testing.T) {
	previousRecorder := mutationJournalRecorder
	previousMirror := mirrorWriteThrough
	t.Cleanup(func() {
		mutationJournalRecorder = previousRecorder
		mirrorWriteThrough = previousMirror
	})
	mutationJournalRecorder = nil
	mirrorWriteThrough = nil

	InstallRuntimeHooks()
	InstallRuntimeHooks()

	if mutationJournalRecorder == nil || mirrorWriteThrough == nil {
		t.Fatal("runtime hooks were not installed")
	}
}
