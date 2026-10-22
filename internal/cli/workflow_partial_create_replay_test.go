// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"zotio/internal/mutation"
)

// quietCreateWorkflowRoot builds a fake root whose "create" step runs a
// two-item items.create mutation (first applied, second failed) through the
// real runMutation and renders it with the step's own flags, so --quiet empties
// stdout exactly as it does for the real command. runs counts executions.
func quietCreateWorkflowRoot(runs *int, applied int) func() *cobra.Command {
	return func() *cobra.Command {
		flags := &rootFlags{maxChanges: -1}
		root := &cobra.Command{Use: "zotio", SilenceErrors: true, SilenceUsage: true}
		root.PersistentFlags().BoolVar(&flags.yes, "yes", false, "")
		root.PersistentFlags().BoolVar(&flags.quiet, "quiet", false, "")
		root.AddCommand(&cobra.Command{
			Use: "create",
			RunE: func(cmd *cobra.Command, _ []string) error {
				*runs++
				ops := []mutation.Op{
					{ID: "items.create:1", Kind: "item_create", Changes: []mutation.Change{{Field: "item", Add: "a"}}},
					{ID: "items.create:2", Kind: "item_create", Changes: []mutation.Change{{Field: "item", Add: "b"}}},
				}
				for i := range ops {
					index := i
					ops[i].Apply = func() (string, any, error) {
						if index < applied {
							return "applied", map[string]any{"key": "NEWKEY1"}, nil
						}
						return "failed", "rejected", nil
					}
				}
				env, runErr := runMutation(cmd.Context(), flags, "items.create", ops, func(o *mutation.Options) { o.ContinueOnError = true })
				return renderMutationResult(cmd, flags, env, runErr, nil)
			},
		})
		return root
	}
}

// A failed create step that already applied part of its batch must refuse an
// automatic resume even when the step suppressed its envelope with --quiet:
// replaying the same step would post the confirmed successes a second time.
// The decision rests on the engine's result, not on what the step printed.
func TestWorkflowRunQuietPartialCreateFailureBlocksResume(t *testing.T) {
	for _, quiet := range []bool{false, true} {
		t.Run(fmt.Sprintf("quiet=%t", quiet), func(t *testing.T) {
			args := []string{"create"}
			if quiet {
				args = append(args, "--quiet")
			}
			spec := workflowRunSpec{Steps: []workflowRunStepSpec{{Name: "make", Args: args}}}
			runs := 0
			newRoot := quietCreateWorkflowRoot(&runs, 1)
			checkpoint := workflowRunCheckpoint{}
			execution := workflowRunExecution{
				Mode:           workflowRunModeApply,
				Checkpoint:     &checkpoint,
				CheckpointPath: filepath.Join(t.TempDir(), "workflow.checkpoint.json"),
			}
			report, err := executeWorkflowRunSpecWithRootFactory(context.Background(), spec, execution, newRoot)
			if err == nil || !strings.Contains(err.Error(), "refusing automatic resume") {
				t.Fatalf("first run error = %v, want the partial-create refusal; report=%+v", err, report)
			}
			if len(checkpoint.Completed) != 1 || checkpoint.Completed[0].Status != "blocked" {
				t.Fatalf("checkpoint = %+v, want the step recorded as blocked, not failed", checkpoint.Completed)
			}
			if quiet && checkpoint.Completed[0].Output != "" {
				t.Fatalf("checkpoint output = %q, want empty under --quiet (the evidence must not come from stdout)", checkpoint.Completed[0].Output)
			}

			// --resume: the blocked step halts the run instead of posting again.
			resumed := workflowRunExecution{
				Mode:           workflowRunModeApply,
				Completed:      checkpoint.Completed,
				Checkpoint:     &checkpoint,
				CheckpointPath: execution.CheckpointPath,
			}
			if _, err := executeWorkflowRunSpecWithRootFactory(context.Background(), spec, resumed, newRoot); err == nil || !strings.Contains(err.Error(), "refusing automatic resume") {
				t.Fatalf("resume error = %v, want the refusal", err)
			}
			if runs != 1 {
				t.Fatalf("create step ran %d times, want 1: resume replayed a committed create", runs)
			}
		})
	}
}

// A failed step that applied nothing keeps the old resume behavior: it is
// checkpointed failed, not blocked, and --resume runs it again.
func TestWorkflowRunFailedStepWithoutAppliesResumes(t *testing.T) {
	spec := workflowRunSpec{Steps: []workflowRunStepSpec{{Name: "make", Args: []string{"create", "--quiet"}}}}
	runs := 0
	newRoot := quietCreateWorkflowRoot(&runs, 0)
	checkpoint := workflowRunCheckpoint{}
	execution := workflowRunExecution{
		Mode:           workflowRunModeApply,
		Checkpoint:     &checkpoint,
		CheckpointPath: filepath.Join(t.TempDir(), "workflow.checkpoint.json"),
	}
	if report, err := executeWorkflowRunSpecWithRootFactory(context.Background(), spec, execution, newRoot); err != nil || report.OK {
		t.Fatalf("first run report ok=%t err=%v, want a failed step without a refusal", report.OK, err)
	}
	if len(checkpoint.Completed) != 1 || checkpoint.Completed[0].Status != "failed" {
		t.Fatalf("checkpoint = %+v, want the step recorded as failed", checkpoint.Completed)
	}
	resumed := workflowRunExecution{Mode: workflowRunModeApply, Completed: checkpoint.Completed, Checkpoint: &checkpoint, CheckpointPath: execution.CheckpointPath}
	_, _ = executeWorkflowRunSpecWithRootFactory(context.Background(), spec, resumed, newRoot)
	if runs != 2 {
		t.Fatalf("create step ran %d times, want 2: a step that applied nothing must be retried", runs)
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
