// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"strings"
	"testing"
)

// A failed create step that already applied part of its batch must refuse an
// automatic resume: replaying the same step would post the confirmed successes
// a second time.
func TestWorkflowRunPartialCreateFailureBlocksResume(t *testing.T) {
	output := `{"operation":"items.create","mode":"apply","result":{"summary":{"applied":1,"failed":1}}}`
	if got := workflowRunConfirmedCreateOutcomes(output); got == "" {
		t.Fatal("workflowRunConfirmedCreateOutcomes(items.create 1 applied) = empty, want the applied count")
	}
}

// A failed step without confirmed applies keeps the old resume behavior.
func TestWorkflowRunFailedStepWithoutAppliesResumes(t *testing.T) {
	for _, output := range []string{
		``,
		`not json`,
		`{"operation":"items.create","mode":"apply","result":{"summary":{"applied":0,"failed":2}}}`,
		`{"operation":"items.list","mode":"apply","result":{"summary":{"applied":2,"failed":0}}}`,
	} {
		if got := workflowRunConfirmedCreateOutcomes(output); got != "" {
			t.Fatalf("workflowRunConfirmedCreateOutcomes(%q) = %q, want empty", output, got)
		}
	}
}

// A checkpointed blocked step must halt a --resume instead of re-executing
// the confirmed batch.
func TestWorkflowRunResumeHaltsOnBlockedPartialCreate(t *testing.T) {
	spec := workflowRunSpec{Steps: []workflowRunStepSpec{{Args: []string{"version"}}}}
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
			Status: "blocked",
			Output: `{"operation":"items.create","mode":"apply","result":{"summary":{"applied":1,"failed":1}}}`,
		}},
	}
	if err := writeWorkflowRunCheckpoint(workflowRunCheckpointPath(path), checkpoint); err != nil {
		t.Fatalf("write workflow checkpoint: %v", err)
	}
	_, _, err = runWorkflowRunTestCmdAtPath(t, path, true, true)
	if err == nil || !strings.Contains(err.Error(), "refusing automatic resume") {
		t.Fatalf("resume error = %v, want the partial-create refusal", err)
	}
}
