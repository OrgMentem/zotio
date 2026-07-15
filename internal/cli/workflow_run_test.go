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
	return runWorkflowRunTestCmdAtPath(t, writeWorkflowRunTestSpec(t, spec), false, false)
}

func runWorkflowRunApplyTestCmd(t *testing.T, spec workflowRunSpec) (workflowRunReport, string, error) {
	t.Helper()
	return runWorkflowRunTestCmdAtPath(t, writeWorkflowRunTestSpec(t, spec), true, false)
}

func runWorkflowRunTestCmdAtPath(t *testing.T, path string, apply, resume bool) (workflowRunReport, string, error) {
	t.Helper()
	cmd := RootCmd()
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	var out bytes.Buffer
	var errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	args := []string{"--json"}
	if apply {
		args = append(args, "--yes")
	}
	args = append(args, "workflow", "run")
	if resume {
		args = append(args, "--resume")
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

func TestWorkflowRunApplyLeavesCheckpointAfterFailure(t *testing.T) {
	spec := workflowRunSpec{Steps: []workflowRunStepSpec{
		{Args: []string{"version"}},
		{Args: []string{"definitely-not-a-command"}},
	}}
	path := writeWorkflowRunTestSpec(t, spec)
	report, stderr, err := runWorkflowRunTestCmdAtPath(t, path, true, false)
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
	if len(checkpoint.Completed) != 1 || checkpoint.Completed[0] != 1 {
		t.Fatalf("checkpoint completed = %v, want [1]", checkpoint.Completed)
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
		Completed:     []int{1},
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
	if !report.Steps[0].OK || report.Steps[0].Status != "skipped" {
		t.Fatalf("first step = %+v, want skipped", report.Steps[0])
	}
	if !report.Steps[1].OK || report.Steps[1].Status != "ok" {
		t.Fatalf("second step = %+v, want executed ok", report.Steps[1])
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
		output, err := executeWorkflowRunStep([]string{"--help"})
		if err != nil {
			t.Fatalf("executeWorkflowRunStep --help: %v", err)
		}
		if !strings.Contains(output, "Zotero automation CLI") {
			t.Fatalf("workflow step output = %q, want root help in Cobra buffer", output)
		}
	}
}
