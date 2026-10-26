// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zotio/internal/mutation"

	"github.com/spf13/cobra"
)

func TestWorkflowMcpInvocationDestructiveConsent(t *testing.T) {
	for _, flag := range []string{"--allow-destructive", "--allow-destructive=true", "--allow-destructive=${vars.CONSENT}"} {
		t.Run(flag, func(t *testing.T) {
			spec := workflowRunSpec{
				Vars: map[string]string{"CONSENT": "true"},
				Steps: []workflowRunStepSpec{
					{Name: "first", Args: []string{"version"}},
					{Name: "merge", Args: []string{"version", flag}},
				},
			}
			data, err := json.Marshal(spec)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "workflow.json")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err = runWorkflowRunFile(context.Background(), path, workflowRunInvocation{Yes: true})
			if ExitCode(err) != 9 || !strings.Contains(err.Error(), "merge") || !strings.Contains(err.Error(), "--allow-destructive") {
				t.Fatalf("apply error = %v, want named destructive consent refusal", err)
			}
			if _, err := os.Stat(workflowRunCheckpointPath(path)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("refused run created a checkpoint: %v", err)
			}
			cmd := RootCmd()
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs([]string{"workflow", "run", path, "--yes", "--allow-destructive", "--json"})
			if err := cmd.ExecuteContext(context.Background()); err != nil {
				t.Fatalf("invocation consent failed: %v; output=%s", err, out.String())
			}
		})
	}
}

func TestWorkflowMcpConsentPrecedesEveryStep(t *testing.T) {
	calls := 0
	rootFactory := func() *cobra.Command {
		calls++
		return &cobra.Command{Use: "zotio"}
	}
	report, err := executeWorkflowRunSpecWithRootFactory(context.Background(), workflowRunSpec{
		Steps: []workflowRunStepSpec{
			{Name: "first", Args: []string{"version"}},
			{Name: "delete", Args: []string{"version", "--allow-destructive"}},
			{Name: "merge", Args: []string{"version", "--allow-destructive"}},
		},
	}, workflowRunExecution{Mode: workflowRunModeApply}, rootFactory)
	if ExitCode(err) != 9 || report.OK || calls != 0 || !strings.Contains(err.Error(), "delete") || !strings.Contains(err.Error(), "merge") {
		t.Fatalf("report=%+v error=%v root calls=%d", report, err, calls)
	}
}

func TestWorkflowMcpHumanPreviewIncludesQuietDestructivePlan(t *testing.T) {
	factory := func() *cobra.Command {
		root := &cobra.Command{Use: "zotio"}
		root.PersistentFlags().Bool("dry-run", false, "")
		root.PersistentFlags().Bool("allow-destructive", false, "")
		root.AddCommand(&cobra.Command{
			Use: "merge", Annotations: map[string]string{"mcp:read-only": "false"},
			RunE: func(cmd *cobra.Command, _ []string) error {
				recordWorkflowRunMutation(cmd.Context(), &mutation.Envelope{Plan: mutation.Plan{
					Summary:    mutation.PlanSummary{Destructive: 1},
					Operations: []mutation.Op{{ID: "merge", Key: "ABCD2345", Destructive: true}},
				}})
				return nil // Quiet output must not hide the destructive plan.
			},
		})
		return root
	}
	report, err := executeWorkflowRunSpecWithRootFactory(context.Background(), workflowRunSpec{
		Steps: []workflowRunStepSpec{{Name: "merge", Args: []string{"merge", "--allow-destructive"}}},
	}, workflowRunExecution{Mode: workflowRunModePreview}, factory)
	if err != nil || !report.OK {
		t.Fatalf("report=%+v error=%v", report, err)
	}
	if len(report.DestructiveSteps) != 1 || !report.Steps[0].Destructive || len(report.Steps[0].Plan) != 1 {
		t.Fatalf("missing destructive plan: %+v", report)
	}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := renderWorkflowRunReport(cmd, &rootFlags{maxChanges: -1}, report); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"destructive", "plan:", "ABCD2345"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("preview missing %q: %s", want, out.String())
		}
	}
}

func TestWorkflowMcpCheckpointSyncsDirectoryAfterRename(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workflow.checkpoint.json")
	previous := workflowRunSyncDir
	t.Cleanup(func() { workflowRunSyncDir = previous })
	calls := 0
	syncErr := errors.New("directory sync failed")
	workflowRunSyncDir = func(dir string) error {
		calls++
		if dir != filepath.Dir(path) {
			t.Fatalf("directory = %q", dir)
		}
		checkpoint, err := readWorkflowRunCheckpoint(path)
		if err != nil || checkpoint.RunID != "durable-run" {
			t.Fatalf("sync before rename: checkpoint=%+v error=%v", checkpoint, err)
		}
		return syncErr
	}
	err := writeWorkflowRunCheckpoint(path, workflowRunCheckpoint{SchemaVersion: workflowRunCheckpointSchemaVersion, RunID: "durable-run"})
	if calls != 1 || !errors.Is(err, syncErr) {
		t.Fatalf("sync calls=%d error=%v", calls, err)
	}
}
