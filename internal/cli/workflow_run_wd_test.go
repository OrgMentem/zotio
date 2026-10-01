// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// wdStepCall records one execution of a fake mutating step.
type wdStepCall struct {
	Command string
	Args    []string
	Note    string
	Stdin   string
	DryRun  bool
}

// wdWorkflowRoot builds a small command tree: "producer" prints a fixed
// output and is read-only; "consumer" reads stdin and "echo" reads only its
// arguments. Both are mutating, so a preview inserts --dry-run.
func wdWorkflowRoot(producerOutput string, calls *[]wdStepCall) func() *cobra.Command {
	return func() *cobra.Command {
		root := &cobra.Command{Use: "test", SilenceErrors: true, SilenceUsage: true}
		root.PersistentFlags().Bool("yes", false, "apply")
		root.PersistentFlags().Bool("dry-run", false, "preview")
		root.PersistentFlags().String("note", "", "a value flag")
		record := func(cmd *cobra.Command, args []string, stdin string) error {
			dryRun, err := cmd.Flags().GetBool("dry-run")
			if err != nil {
				return err
			}
			note, err := cmd.Flags().GetString("note")
			if err != nil {
				return err
			}
			*calls = append(*calls, wdStepCall{Command: cmd.Name(), Args: args, Note: note, Stdin: stdin, DryRun: dryRun})
			return nil
		}
		root.AddCommand(
			&cobra.Command{
				Use:         "producer",
				Annotations: map[string]string{"mcp:read-only": "true"},
				RunE: func(cmd *cobra.Command, _ []string) error {
					_, err := io.WriteString(cmd.OutOrStdout(), producerOutput)
					return err
				},
			},
			&cobra.Command{
				Use: "consumer",
				RunE: func(cmd *cobra.Command, args []string) error {
					stdin, err := io.ReadAll(cmd.InOrStdin())
					if err != nil {
						return err
					}
					return record(cmd, args, string(stdin))
				},
			},
			&cobra.Command{
				Use:  "echo",
				Args: cobra.ArbitraryArgs,
				RunE: func(cmd *cobra.Command, args []string) error {
					return record(cmd, args, "")
				},
			},
		)
		return root
	}
}

const wdHealthOutput = `{"findings":[
  {"kind":"missing_doi","item_key":"AAAA1111"},
  {"kind":"missing_abstract","item_key":"CCCC3333"},
  {"kind":"missing_doi","item_key":"BBBB2222"}
],"result":{"key":"KEY12345","count":12,"items":[{"key":"X"}]}}`

func wdRunFakeWorkflow(t *testing.T, producerOutput string, execution workflowRunExecution, steps ...workflowRunStepSpec) (workflowRunReport, []wdStepCall, error) {
	t.Helper()
	var calls []wdStepCall
	spec := workflowRunSpec{Steps: append([]workflowRunStepSpec{{Name: "src", Args: []string{"producer"}}}, steps...)}
	if execution.Mode == "" {
		execution.Mode = workflowRunModePreview
	}
	report, err := executeWorkflowRunSpecWithRootFactory(context.Background(), spec, execution, wdWorkflowRoot(producerOutput, &calls))
	return report, calls, err
}

// A diagnostics step emits two finding kinds; the next step must receive only
// the item keys of the selected kind, one per line, ready for --keys-from -.
func TestWdStdinSelectPipesOnlyTheSelectedFindingKind(t *testing.T) {
	report, calls, err := wdRunFakeWorkflow(t, wdHealthOutput, workflowRunExecution{},
		workflowRunStepSpec{Name: "fix", Args: []string{"consumer"}, StdinFrom: "src", StdinSelect: "findings[kind=missing_doi].item_key"},
	)
	if err != nil || !report.OK {
		t.Fatalf("workflow = %+v, %v; want success", report, err)
	}
	if len(calls) != 1 {
		t.Fatalf("consumer calls = %+v, want one", calls)
	}
	if got, want := calls[0].Stdin, "AAAA1111\nBBBB2222\n"; got != want {
		t.Fatalf("consumer stdin = %q, want only the missing_doi keys %q", got, want)
	}
}

func TestWdArgumentSelectorFillsValuesAndKeepsNumberText(t *testing.T) {
	report, calls, err := wdRunFakeWorkflow(t, wdHealthOutput, workflowRunExecution{},
		workflowRunStepSpec{Name: "use", Args: []string{"echo", "--note=${steps.src.json:result.key}", "${steps.src.json:result.count}", "${steps.src.json:findings[1].item_key}"}},
	)
	if err != nil || !report.OK {
		t.Fatalf("workflow = %+v, %v; want success", report, err)
	}
	if len(calls) != 1 {
		t.Fatalf("echo calls = %+v, want one", calls)
	}
	if calls[0].Note != "KEY12345" {
		t.Errorf("--note = %q, want the selected key", calls[0].Note)
	}
	if got := strings.Join(calls[0].Args, ","); got != "12,CCCC3333" {
		t.Errorf("positional args = %q, want the number text and the indexed key", got)
	}
}

// Raw ${steps.NAME.output} and stdin_from keep their behavior next to the
// selector: trimmed text in an argument, untouched bytes on stdin.
func TestWdRawOutputAndStdinFromAreUnchanged(t *testing.T) {
	const raw = "  not json at all \n"
	report, calls, err := wdRunFakeWorkflow(t, raw, workflowRunExecution{},
		workflowRunStepSpec{Name: "pipe", Args: []string{"consumer"}, StdinFrom: "src"},
		workflowRunStepSpec{Name: "use", Args: []string{"echo", "--note=${steps.src.output}"}},
	)
	if err != nil || !report.OK {
		t.Fatalf("workflow = %+v, %v; want success", report, err)
	}
	if len(calls) != 2 || calls[0].Stdin != raw || calls[1].Note != "not json at all" {
		t.Fatalf("calls = %+v, want raw stdin and trimmed argument", calls)
	}
}

func TestWdSelectorFailsClosed(t *testing.T) {
	tests := []struct {
		name   string
		output string
		step   workflowRunStepSpec
		want   string
	}{
		{
			name:   "malformed JSON",
			output: "Plan: 2 planned\n",
			step:   workflowRunStepSpec{Name: "use", Args: []string{"echo", "--note=${steps.src.json:result.key}"}},
			want:   "not valid JSON",
		},
		{
			name:   "trailing content",
			output: `{"result":{"key":"A"}} {"result":{"key":"B"}}`,
			step:   workflowRunStepSpec{Name: "use", Args: []string{"echo", "--note=${steps.src.json:result.key}"}},
			want:   "more content after its JSON value",
		},
		{
			name:   "missing field",
			output: wdHealthOutput,
			step:   workflowRunStepSpec{Name: "use", Args: []string{"echo", "--note=${steps.src.json:result.missing}"}},
			want:   `field "missing" is missing`,
		},
		{
			name:   "index out of range",
			output: wdHealthOutput,
			step:   workflowRunStepSpec{Name: "use", Args: []string{"echo", "--note=${steps.src.json:findings[9].item_key}"}},
			want:   "out of range",
		},
		{
			name:   "object into argument",
			output: wdHealthOutput,
			step:   workflowRunStepSpec{Name: "use", Args: []string{"echo", "--note=${steps.src.json:result}"}},
			want:   "selected an object",
		},
		{
			name:   "objects into stdin",
			output: wdHealthOutput,
			step:   workflowRunStepSpec{Name: "use", Args: []string{"consumer"}, StdinFrom: "src", StdinSelect: "result.items"},
			want:   "selected an object, but stdin takes",
		},
		{
			name:   "field on an array",
			output: wdHealthOutput,
			step:   workflowRunStepSpec{Name: "use", Args: []string{"consumer"}, StdinFrom: "src", StdinSelect: "findings.item_key"},
			want:   `field "item_key" needs an object, found an array`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			report, calls, err := wdRunFakeWorkflow(t, test.output, workflowRunExecution{}, test.step)
			if err != nil {
				t.Fatalf("execution error = %v; a selection failure must fail the step, not the run setup", err)
			}
			if report.OK || len(report.Steps) != 2 || report.Steps[1].Status != "failed" {
				t.Fatalf("report = %+v, want the selecting step failed", report)
			}
			if !strings.Contains(report.Steps[1].Error, test.want) {
				t.Fatalf("step error = %q, want %q", report.Steps[1].Error, test.want)
			}
			if len(calls) != 0 {
				t.Fatalf("calls = %+v, want the command never run", calls)
			}
		})
	}
}

// An empty selection must not reach --keys-from -: some commands treat an
// empty key list as "no filter".
func TestWdEmptySelectionSkipsTheStep(t *testing.T) {
	report, calls, err := wdRunFakeWorkflow(t, wdHealthOutput, workflowRunExecution{},
		workflowRunStepSpec{Name: "fix", Args: []string{"consumer"}, StdinFrom: "src", StdinSelect: "findings[kind=missing_pdf].item_key"},
	)
	if err != nil || !report.OK {
		t.Fatalf("workflow = %+v, %v; want success", report, err)
	}
	if got := report.Steps[1]; got.Status != "skipped" || got.Reason != "stdin_select selected no values" {
		t.Fatalf("fix step = %+v, want skipped for an empty selection", got)
	}
	if len(calls) != 0 {
		t.Fatalf("calls = %+v, want the consumer never run", calls)
	}
}

func TestWdSelectorSpecValidation(t *testing.T) {
	tests := []struct {
		name string
		step workflowRunStepSpec
		want string
	}{
		{
			name: "stdin_select without stdin_from",
			step: workflowRunStepSpec{Args: []string{"version"}, StdinSelect: "result.key"},
			want: "stdin_select needs stdin_from",
		},
		{
			name: "unclosed bracket",
			step: workflowRunStepSpec{Args: []string{"version"}, StdinFrom: "src", StdinSelect: "findings[kind=missing_doi"},
			want: "unclosed [",
		},
		{
			name: "bad bracket",
			step: workflowRunStepSpec{Args: []string{"version"}, StdinFrom: "src", StdinSelect: "findings[-1]"},
			want: "must be [], [N]",
		},
		{
			name: "trailing dot",
			step: workflowRunStepSpec{Args: []string{"version"}, StdinFrom: "src", StdinSelect: "result."},
			want: "ends with a dot",
		},
		{
			name: "list into argument",
			step: workflowRunStepSpec{Args: []string{"version", "--config=${steps.src.json:findings[].item_key}"}},
			want: "an argument takes one",
		},
		{
			name: "empty argument selector",
			step: workflowRunStepSpec{Args: []string{"version", "--config=${steps.src.json:}"}},
			want: "invalid placeholder",
		},
		{
			name: "unknown step in selector",
			step: workflowRunStepSpec{Args: []string{"version", "--config=${steps.missing.json:result.key}"}},
			want: "invalid placeholder",
		},
		{
			name: "two stdin sources",
			step: workflowRunStepSpec{Args: []string{"version"}, StdinFrom: "src", StdinTrigger: "upsert_keys"},
			want: "both stdin_from and stdin_trigger",
		},
		{
			name: "malformed trigger selector",
			step: workflowRunStepSpec{Args: []string{"version"}, StdinTrigger: "events[event=]"},
			want: "stdin_trigger",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeWorkflowRunTestSpec(t, workflowRunSpec{Steps: []workflowRunStepSpec{
				{Name: "src", Args: []string{"version"}},
				test.step,
			}})
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

// The checkpoint keeps the raw output; a resumed run recomputes the selection
// from it instead of storing selected values.
func TestWdResumeRecomputesSelectionFromCheckpointedOutput(t *testing.T) {
	spec := workflowRunSpec{Steps: []workflowRunStepSpec{
		{Name: "src", Args: []string{"version"}},
		{Name: "use", Args: []string{"version", "--config=${steps.src.json:result.key}"}},
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
			Name:   "src",
			Status: "ok",
			Output: `{"result":{"key":"RESUMED1"}}` + "\n",
		}},
	}
	if err := writeWorkflowRunCheckpoint(workflowRunCheckpointPath(path), checkpoint); err != nil {
		t.Fatalf("write workflow checkpoint: %v", err)
	}

	report, err := runWorkflowRunFile(context.Background(), path, workflowRunInvocation{Yes: true, Resume: true})
	if err != nil {
		t.Fatalf("resume workflow: %v", err)
	}
	if report.Steps[0].Reason != "resume" || report.Steps[1].Status != "ok" {
		t.Fatalf("resume report = %+v, want resumed source and executed consumer", report)
	}
	if got, want := strings.Join(report.Steps[1].Args, " "), "version --config=RESUMED1"; got != want {
		t.Fatalf("consumer args = %q, want %q", got, want)
	}
}

// A selected value is data: it may fill a value, but it can never pick the
// command or a flag that runs.
func TestWdSelectedValueCannotChangeCommandOrFlag(t *testing.T) {
	const output = `{"sub":"consumer","flag":"--yes","cmd":"workflow"}`
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "subcommand", args: []string{"${steps.src.json:sub}"}, want: "different command"},
		{name: "flag", args: []string{"echo", "${steps.src.json:flag}"}, want: "as a flag"},
		{name: "workflow command", args: []string{"${steps.src.json:cmd}", "run", "nested.json"}, want: `invokes "workflow"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			report, calls, err := wdRunFakeWorkflow(t, output, workflowRunExecution{
				Completed: []workflowRunCheckpointStep{{Index: 1, Name: "src", Status: "ok", Output: output}},
			}, workflowRunStepSpec{Name: "use", Args: test.args})
			if err != nil {
				t.Fatalf("execution error = %v", err)
			}
			if len(report.Steps) != 2 || report.Steps[1].Status != "failed" || !strings.Contains(report.Steps[1].Error, test.want) {
				t.Fatalf("report = %+v, want the step refused with %q", report, test.want)
			}
			if len(calls) != 0 {
				t.Fatalf("calls = %+v, want nothing run", calls)
			}
		})
	}
}

func wdTrigger(t *testing.T, events ...workflowRunTriggerEvent) *workflowRunTrigger {
	t.Helper()
	trigger, err := newWorkflowRunTrigger("tail", "items", events)
	if err != nil {
		t.Fatalf("build trigger: %v", err)
	}
	return trigger
}

func wdEvent(event, key string) workflowRunTriggerEvent {
	return workflowRunTriggerEvent{Event: event, Resource: "items", Key: key}
}

// Upserted and deleted identities stay separate, each key appears once, and
// a key deleted in the same batch is not offered as an upsert. The batch is
// data: a preview still runs the mutating steps with --dry-run.
func TestWdTriggerKeysReachStepsSeparatelyInPreview(t *testing.T) {
	trigger := wdTrigger(t,
		wdEvent("upsert", "AAAA1111"),
		wdEvent("delete", "DDDD4444"),
		wdEvent("upsert", "AAAA1111"),
		wdEvent("upsert", "BBBB2222"),
		wdEvent("delete", "BBBB2222"),
	)
	report, calls, err := wdRunFakeWorkflow(t, "{}", workflowRunExecution{Trigger: trigger},
		workflowRunStepSpec{Name: "changed", Args: []string{"consumer"}, StdinTrigger: "upsert_keys"},
		workflowRunStepSpec{Name: "removed", Args: []string{"consumer"}, StdinTrigger: "delete_keys"},
		workflowRunStepSpec{Name: "first", Args: []string{"echo", "--note=${trigger.json:events[1].event}", "${trigger.json:events[1].key}", "${trigger.json:resource}"}},
	)
	if err != nil || !report.OK {
		t.Fatalf("workflow = %+v, %v; want success", report, err)
	}
	if len(calls) != 3 {
		t.Fatalf("calls = %+v, want three steps run", calls)
	}
	if calls[0].Stdin != "AAAA1111\n" {
		t.Errorf("upsert consumer stdin = %q, want only the surviving upsert", calls[0].Stdin)
	}
	if calls[1].Stdin != "DDDD4444\nBBBB2222\n" {
		t.Errorf("delete consumer stdin = %q, want the deleted keys", calls[1].Stdin)
	}
	if calls[2].Note != "delete" || strings.Join(calls[2].Args, ",") != "DDDD4444,items" {
		t.Errorf("event values = %+v, want the second event's type, key, and resource", calls[2])
	}
	for _, call := range calls {
		if !call.DryRun {
			t.Errorf("call %+v ran without --dry-run; a trigger must never approve a write", call)
		}
	}
}

// A batch without upserts (here a lone deletion) must not run an upsert
// consumer with an empty key list.
func TestWdTriggerWithoutUpsertsSkipsTheKeyConsumer(t *testing.T) {
	trigger := wdTrigger(t, wdEvent("delete", "DDDD4444"))
	report, calls, err := wdRunFakeWorkflow(t, "{}", workflowRunExecution{Trigger: trigger},
		workflowRunStepSpec{Name: "changed", Args: []string{"consumer"}, StdinTrigger: "upsert_keys"},
	)
	if err != nil || !report.OK {
		t.Fatalf("workflow = %+v, %v; want success", report, err)
	}
	if got := report.Steps[1]; got.Status != "skipped" || got.Reason != "stdin_trigger selected no values" {
		t.Fatalf("changed step = %+v, want skipped", got)
	}
	if len(calls) != 0 {
		t.Fatalf("calls = %+v, want the consumer never run", calls)
	}
}

func TestWdTriggerBatchBound(t *testing.T) {
	events := make([]workflowRunTriggerEvent, workflowRunTriggerMaxEvents+1)
	for i := range events {
		events[i] = wdEvent("upsert", fmt.Sprintf("K%07d", i))
	}
	if _, err := newWorkflowRunTrigger("tail", "items", events[:workflowRunTriggerMaxEvents]); err != nil {
		t.Fatalf("batch at the bound: %v", err)
	}
	trigger, reason := tailWorkflowTrigger("items", tailChangeBatch{Events: events})
	if trigger != nil || !strings.Contains(reason, fmt.Sprint(workflowRunTriggerMaxEvents)) {
		t.Fatalf("oversized batch = %+v, %q; want a skip naming the limit", trigger, reason)
	}
	trigger, reason = tailWorkflowTrigger("items", tailChangeBatch{Baseline: true, Events: events[:1]})
	if trigger != nil || !strings.Contains(reason, "not changes") {
		t.Fatalf("baseline batch = %+v, %q; want a skip", trigger, reason)
	}
}

func TestWdTriggerSpecWithoutBatchIsAPrecondition(t *testing.T) {
	path := writeWorkflowRunTestSpec(t, workflowRunSpec{Steps: []workflowRunStepSpec{
		{Name: "use", Args: []string{"version", "--config=${trigger.json:upsert_keys[0]}"}},
	}})
	report, err := runWorkflowRunFile(context.Background(), path, workflowRunInvocation{})
	var cliErr *cliError
	if !errors.As(err, &cliErr) || cliErr.code != 9 {
		t.Fatalf("error = %v, want precondition exit 9", err)
	}
	if len(report.Steps) != 0 {
		t.Fatalf("report = %+v, want no step run", report)
	}
}

// The checkpoint stores the batch the run was approved with; a resume reads
// that batch, not one handed to the resuming call.
func TestWdTriggerResumeReplaysTheCheckpointedBatch(t *testing.T) {
	spec := workflowRunSpec{Steps: []workflowRunStepSpec{
		{Name: "use", Args: []string{"version", "--config=${trigger.json:upsert_keys[0]}"}},
		{Name: "boom", Args: []string{"definitely-not-a-command"}},
	}}
	path := writeWorkflowRunTestSpec(t, spec)

	_, err := runWorkflowRunFile(context.Background(), path, workflowRunInvocation{
		Yes:     true,
		Trigger: wdTrigger(t, wdEvent("upsert", "FIRST111")),
	})
	if err == nil {
		t.Fatal("apply succeeded, want the failing step to keep the checkpoint")
	}
	checkpoint, err := readWorkflowRunCheckpoint(workflowRunCheckpointPath(path))
	if err != nil {
		t.Fatalf("read workflow checkpoint: %v", err)
	}
	if checkpoint.SchemaVersion != workflowRunCheckpointSchemaVersion || checkpoint.Trigger == nil ||
		strings.Join(checkpoint.Trigger.UpsertKeys, ",") != "FIRST111" {
		t.Fatalf("checkpoint = %+v, want the trigger batch stored", checkpoint)
	}

	// Retry the first step too, so the resumed run reads the batch again.
	checkpoint.Completed = nil
	if err := writeWorkflowRunCheckpoint(workflowRunCheckpointPath(path), checkpoint); err != nil {
		t.Fatalf("rewrite workflow checkpoint: %v", err)
	}
	report, err := runWorkflowRunFile(context.Background(), path, workflowRunInvocation{
		Yes:     true,
		Resume:  true,
		Trigger: wdTrigger(t, wdEvent("upsert", "NEWER222")),
	})
	if err == nil {
		t.Fatal("resume succeeded, want the failing step to fail again")
	}
	if got, want := strings.Join(report.Steps[0].Args, " "), "version --config=FIRST111"; got != want {
		t.Fatalf("resumed args = %q, want the checkpointed batch %q", got, want)
	}
}

// A spec that reads no trigger values runs exactly as before: no batch is
// stored, and a resume needs none.
func TestWdSpecWithoutTriggerValuesIgnoresTheBatch(t *testing.T) {
	path := writeWorkflowRunTestSpec(t, workflowRunSpec{Steps: []workflowRunStepSpec{
		{Name: "ok", Args: []string{"version"}},
		{Name: "boom", Args: []string{"definitely-not-a-command"}},
	}})
	if _, err := runWorkflowRunFile(context.Background(), path, workflowRunInvocation{
		Yes:     true,
		Trigger: wdTrigger(t, wdEvent("upsert", "AAAA1111")),
	}); err == nil {
		t.Fatal("apply succeeded, want the failing step to keep the checkpoint")
	}
	checkpoint, err := readWorkflowRunCheckpoint(workflowRunCheckpointPath(path))
	if err != nil {
		t.Fatalf("read workflow checkpoint: %v", err)
	}
	if checkpoint.Trigger != nil {
		t.Fatalf("checkpoint trigger = %+v, want none for a spec that reads no trigger values", checkpoint.Trigger)
	}
}

// Checkpoints written before trigger batches (schema v2) still resume.
func TestWdV2CheckpointStillResumes(t *testing.T) {
	path := writeWorkflowRunTestSpec(t, workflowRunSpec{Steps: []workflowRunStepSpec{
		{Name: "first", Args: []string{"version"}},
		{Name: "second", Args: []string{"version"}},
	}})
	_, rawSpec, err := readWorkflowRunSpecFile(path)
	if err != nil {
		t.Fatalf("read workflow spec: %v", err)
	}
	v2 := `{"schema_version":2,"run_id":"workflow-run-id","spec_sha256":"` + workflowRunSpecSHA256(rawSpec) +
		`","completed":[{"index":1,"name":"first","status":"ok","output":"done"}]}`
	if err := os.WriteFile(workflowRunCheckpointPath(path), []byte(v2), 0o600); err != nil {
		t.Fatalf("write v2 checkpoint: %v", err)
	}
	report, err := runWorkflowRunFile(context.Background(), path, workflowRunInvocation{Yes: true, Resume: true})
	if err != nil {
		t.Fatalf("resume v2 checkpoint: %v", err)
	}
	if report.RunID != "workflow-run-id" || report.Steps[0].Reason != "resume" || report.Steps[1].Status != "ok" {
		t.Fatalf("report = %+v, want the v2 run resumed", report)
	}
	if _, err := os.Stat(workflowRunCheckpointPath(path)); !os.IsNotExist(err) {
		t.Fatalf("checkpoint after success: %v, want removed", err)
	}
}
