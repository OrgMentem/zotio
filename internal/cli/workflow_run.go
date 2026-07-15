// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"zotio/internal/mutation"

	"github.com/spf13/cobra"
)

const (
	workflowRunOutputLimit             = 64 * 1024
	workflowRunModePreview             = "preview"
	workflowRunModeApply               = "apply"
	workflowRunCheckpointSchemaVersion = 1
)

var activeWorkflowRunID string

type workflowRunSpec struct {
	Steps           []workflowRunStepSpec `json:"steps"`
	ContinueOnError bool                  `json:"continue_on_error"`
}

type workflowRunStepSpec struct {
	Name string   `json:"name,omitempty"`
	Args []string `json:"args"`
}

type workflowRunReport struct {
	Steps []workflowRunStepReport `json:"steps"`
	OK    bool                    `json:"ok"`
	Mode  string                  `json:"mode"`
	RunID string                  `json:"run_id,omitempty"`
}

type workflowRunStepReport struct {
	Index  int      `json:"index"`
	Name   string   `json:"name,omitempty"`
	Args   []string `json:"args"`
	OK     bool     `json:"ok"`
	Status string   `json:"status"`
	Error  string   `json:"error,omitempty"`
	Output string   `json:"output"`
}

func newWorkflowRunCmd(flags *rootFlags) *cobra.Command {
	var resume bool

	cmd := &cobra.Command{
		Use:   "run <file.json>",
		Short: "Preview or transactionally apply a declarative workflow",
		Long: `Runs a declarative workflow spec in process. By default, mutating steps
are previewed with --dry-run while read-only steps run normally.

Pass --yes once to apply the whole workflow. Every mutation from that run shares
one journal run ID. If an applied workflow is interrupted, continue it with
--yes --resume; completed steps are skipped from its checkpoint sidecar.`,
		Example: `  zotio workflow run workflow.json
  zotio workflow run workflow.json --yes
  zotio workflow run workflow.json --yes --resume`,
		Args: cobra.ExactArgs(1),
		// Workflow specs can execute arbitrary CLI argument vectors. Keep this
		// local-file runner off the MCP command surface rather than bypassing its
		// per-command flag allowlist.
		Annotations: map[string]string{
			"mcp:hidden":                       "true",
			"mcp:read-only":                    "false",
			"zotio:destructive":                "false",
			"zotio:supports-dry-run":           "true",
			"zotio:requires-allow-destructive": "false",
			"zotio:default-max-changes":        "500",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			spec, rawSpec, err := readWorkflowRunSpecFile(args[0])
			if err != nil {
				return err
			}
			if resume && !flags.yes {
				return usageErr(fmt.Errorf("--resume requires --yes; resume continues an already-approved run"))
			}

			execution := workflowRunExecution{Mode: workflowRunModePreview}
			if flags.yes {
				checkpointPath := workflowRunCheckpointPath(args[0])
				specSHA256 := workflowRunSpecSHA256(rawSpec)
				checkpoint := workflowRunCheckpoint{
					SchemaVersion: workflowRunCheckpointSchemaVersion,
					SpecSHA256:    specSHA256,
				}

				if resume {
					checkpoint, err = readWorkflowRunCheckpoint(checkpointPath)
					if err != nil {
						if errors.Is(err, os.ErrNotExist) {
							return fmt.Errorf("--resume requires an existing checkpoint at %q", checkpointPath)
						}
						return err
					}
					completed, err := validateWorkflowRunCheckpoint(checkpoint, specSHA256, checkpointPath, len(spec.Steps))
					if err != nil {
						return err
					}
					execution.Completed = completed
				} else {
					exists, err := workflowRunCheckpointExists(checkpointPath)
					if err != nil {
						return err
					}
					if exists {
						return fmt.Errorf("an interrupted workflow run exists at %q; pass --resume to continue or delete %q to start over", checkpointPath, checkpointPath)
					}
					checkpoint.RunID = mutation.NewRunID(time.Now())
					if err := writeWorkflowRunCheckpoint(checkpointPath, checkpoint); err != nil {
						return err
					}
				}

				execution = workflowRunExecution{
					Mode:           workflowRunModeApply,
					RunID:          checkpoint.RunID,
					Completed:      execution.Completed,
					Checkpoint:     &checkpoint,
					CheckpointPath: checkpointPath,
				}
			}

			var report workflowRunReport
			var executionErr error
			if execution.Mode == workflowRunModeApply {
				report, executionErr = executeWorkflowRunSpecWithRunID(spec, execution)
			} else {
				report, executionErr = executeWorkflowRunSpecWithOptions(spec, execution)
			}
			if executionErr == nil && execution.Mode == workflowRunModeApply && report.OK {
				if err := os.Remove(execution.CheckpointPath); err != nil && !os.IsNotExist(err) {
					executionErr = fmt.Errorf("remove workflow checkpoint %q: %w", execution.CheckpointPath, err)
				}
			}
			if err := renderWorkflowRunReport(cmd, flags, report); err != nil {
				return err
			}
			if executionErr != nil {
				return executionErr
			}
			if !report.OK {
				return fmt.Errorf("workflow failed: %d step(s) failed", workflowRunFailureCount(report))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&resume, "resume", false, "Resume an interrupted applied workflow from its checkpoint sidecar")
	return cmd
}

func readWorkflowRunSpec(path string) (workflowRunSpec, error) {
	spec, _, err := readWorkflowRunSpecFile(path)
	return spec, err
}

func readWorkflowRunSpecFile(path string) (workflowRunSpec, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return workflowRunSpec{}, nil, fmt.Errorf("workflow spec file %q does not exist", path)
		}
		return workflowRunSpec{}, nil, fmt.Errorf("read workflow spec %q: %w", path, err)
	}

	var spec workflowRunSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return workflowRunSpec{}, nil, fmt.Errorf("parse workflow spec %q: %w", path, err)
	}
	if len(spec.Steps) == 0 {
		return workflowRunSpec{}, nil, fmt.Errorf("workflow spec %q must contain at least one step", path)
	}
	for i, step := range spec.Steps {
		if workflowRunStepInvokesWorkflow(step.Args) {
			return workflowRunSpec{}, nil, fmt.Errorf("workflow step %d invokes %q; workflow run cannot invoke workflow commands", i+1, "workflow")
		}
		for _, arg := range step.Args {
			if workflowRunStepOwnsFlag(arg) {
				return workflowRunSpec{}, nil, fmt.Errorf("workflow step %d includes %q; the workflow owns approval: pass --yes to zotio workflow run itself", i+1, arg)
			}
		}
	}
	return spec, data, nil
}

func workflowRunStepOwnsFlag(arg string) bool {
	for _, flag := range []string{"--yes", "--dry-run", "--resume"} {
		if arg == flag || strings.HasPrefix(arg, flag+"=") {
			return true
		}
	}
	return false
}

func workflowRunStepInvokesWorkflow(args []string) bool {
	positionals := workflowRunStepPositionals(args)
	return len(positionals) > 0 && positionals[0] == "workflow"
}

var workflowRunRootFlagsWithValue = map[string]struct{}{
	"--config":           {},
	"--timeout":          {},
	"--max-changes":      {},
	"--max-failures":     {},
	"--select":           {},
	"--data-source":      {},
	"--profile":          {},
	"--deliver":          {},
	"--rate-limit":       {},
	"--group":            {},
	"--via":              {},
	"--connector-target": {},
}

func workflowRunStepPositionals(args []string) []string {
	positionals := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return append(positionals, args[i+1:]...)
		}
		if strings.HasPrefix(arg, "-") {
			if strings.Contains(arg, "=") {
				continue
			}
			if _, ok := workflowRunRootFlagsWithValue[arg]; ok {
				i++
			}
			continue
		}
		positionals = append(positionals, arg)
	}
	return positionals
}

func workflowRunStepIsReadOnly(args []string) bool {
	command, _, err := RootCmd().Find(workflowRunStepPositionals(args))
	if err != nil || command == nil {
		return false
	}
	for ; command != nil; command = command.Parent() {
		if readOnly, ok := command.Annotations["mcp:read-only"]; ok {
			return readOnly == "true"
		}
	}
	return false
}

func workflowRunStepArgs(mode string, readOnly bool, args []string) []string {
	switch mode {
	case workflowRunModePreview:
		if !readOnly {
			return workflowRunStepAppendArg(args, "--dry-run")
		}
	case workflowRunModeApply:
		return workflowRunStepAppendArg(args, "--yes")
	}
	return args
}

func workflowRunStepAppendArg(args []string, arg string) []string {
	transformed := make([]string, len(args), len(args)+1)
	copy(transformed, args)
	return append(transformed, arg)
}

func workflowRunSpecSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func executeWorkflowRunSpec(spec workflowRunSpec) workflowRunReport {
	report, _ := executeWorkflowRunSpecWithOptions(spec, workflowRunExecution{Mode: workflowRunModePreview})
	return report
}

type workflowRunCheckpoint struct {
	SchemaVersion int    `json:"schema_version"`
	RunID         string `json:"run_id"`
	SpecSHA256    string `json:"spec_sha256"`
	Completed     []int  `json:"completed"`
}

type workflowRunExecution struct {
	Mode           string
	RunID          string
	Completed      map[int]bool
	Checkpoint     *workflowRunCheckpoint
	CheckpointPath string
}

func workflowRunCheckpointPath(specPath string) string {
	return specPath + ".checkpoint.json"
}

func workflowRunCheckpointExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, fmt.Errorf("stat workflow checkpoint %q: %w", path, err)
}

func readWorkflowRunCheckpoint(path string) (workflowRunCheckpoint, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return workflowRunCheckpoint{}, fmt.Errorf("read workflow checkpoint %q: %w", path, err)
	}
	var checkpoint workflowRunCheckpoint
	if err := json.Unmarshal(data, &checkpoint); err != nil {
		return workflowRunCheckpoint{}, fmt.Errorf("parse workflow checkpoint %q: %w", path, err)
	}
	return checkpoint, nil
}

func writeWorkflowRunCheckpoint(path string, checkpoint workflowRunCheckpoint) error {
	if checkpoint.Completed == nil {
		checkpoint.Completed = make([]int, 0)
	}
	data, err := json.MarshalIndent(checkpoint, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal workflow checkpoint: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write workflow checkpoint %q: %w", path, err)
	}
	return nil
}

func validateWorkflowRunCheckpoint(checkpoint workflowRunCheckpoint, specSHA256, path string, stepCount int) (map[int]bool, error) {
	if checkpoint.SchemaVersion != workflowRunCheckpointSchemaVersion {
		return nil, fmt.Errorf("workflow checkpoint %q has unsupported schema version %d", path, checkpoint.SchemaVersion)
	}
	if checkpoint.RunID == "" {
		return nil, fmt.Errorf("workflow checkpoint %q has no run_id", path)
	}
	if checkpoint.SpecSHA256 != specSHA256 {
		return nil, fmt.Errorf("workflow spec changed since the checkpoint; delete %q to start over", path)
	}

	completed := make(map[int]bool, len(checkpoint.Completed))
	for _, index := range checkpoint.Completed {
		if index < 1 || index > stepCount || completed[index] {
			return nil, fmt.Errorf("workflow checkpoint %q has invalid completed step %d", path, index)
		}
		completed[index] = true
	}
	return completed, nil
}

func executeWorkflowRunSpecWithRunID(spec workflowRunSpec, execution workflowRunExecution) (workflowRunReport, error) {
	activeWorkflowRunID = execution.RunID
	defer func() {
		activeWorkflowRunID = ""
	}()
	return executeWorkflowRunSpecWithOptions(spec, execution)
}

func executeWorkflowRunSpecWithOptions(spec workflowRunSpec, execution workflowRunExecution) (workflowRunReport, error) {
	report := workflowRunReport{
		Steps: make([]workflowRunStepReport, 0, len(spec.Steps)),
		OK:    true,
		Mode:  execution.Mode,
		RunID: execution.RunID,
	}
	stopped := false
	var executionErr error

	for i, step := range spec.Steps {
		stepReport := workflowRunStepReport{
			Index:  i + 1,
			Name:   step.Name,
			Args:   append([]string(nil), step.Args...),
			Status: "not_attempted",
		}
		if stopped {
			report.OK = false
			report.Steps = append(report.Steps, stepReport)
			continue
		}
		if execution.Completed[stepReport.Index] {
			stepReport.OK = true
			stepReport.Status = "skipped"
			report.Steps = append(report.Steps, stepReport)
			continue
		}

		args := workflowRunStepArgs(execution.Mode, workflowRunStepIsReadOnly(step.Args), step.Args)
		output, err := executeWorkflowRunStep(args)
		stepReport.Output = capWorkflowRunOutput(output)
		if err != nil {
			stepReport.Status = "failed"
			stepReport.Error = err.Error()
			report.OK = false
			if !spec.ContinueOnError {
				stopped = true
			}
		} else {
			stepReport.OK = true
			stepReport.Status = "ok"
			if execution.Checkpoint != nil {
				execution.Checkpoint.Completed = append(execution.Checkpoint.Completed, stepReport.Index)
				if err := writeWorkflowRunCheckpoint(execution.CheckpointPath, *execution.Checkpoint); err != nil {
					execution.Checkpoint.Completed = execution.Checkpoint.Completed[:len(execution.Checkpoint.Completed)-1]
					stepReport.OK = false
					stepReport.Status = "failed"
					stepReport.Error = err.Error()
					report.OK = false
					stopped = true
					executionErr = fmt.Errorf("checkpoint workflow step %d: %w", stepReport.Index, err)
				} else if execution.Completed != nil {
					execution.Completed[stepReport.Index] = true
				}
			}
		}
		report.Steps = append(report.Steps, stepReport)
	}

	return report, executionErr
}

func executeWorkflowRunStep(args []string) (string, error) {
	root := RootCmd()
	root.SetArgs(args)
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)

	savedNoColor := noColor
	savedHumanFriendly := humanFriendly
	savedGroup := activeGroupID

	err := root.Execute()

	noColor = savedNoColor
	humanFriendly = savedHumanFriendly
	activeGroupID = savedGroup

	return buf.String(), err
}

func capWorkflowRunOutput(output string) string {
	if len(output) <= workflowRunOutputLimit {
		return output
	}
	marker := "\n[workflow output truncated]\n"
	keep := workflowRunOutputLimit - len(marker)
	if keep < 0 {
		keep = workflowRunOutputLimit
		marker = ""
	}
	return output[:keep] + marker
}

func renderWorkflowRunReport(cmd *cobra.Command, flags *rootFlags, report workflowRunReport) error {
	if flags.asJSON || flags.compact || flags.selectFields != "" || flags.csv {
		data, err := json.Marshal(report)
		if err != nil {
			return err
		}
		return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), flags)
	}

	out := cmd.OutOrStdout()
	if report.RunID == "" {
		fmt.Fprintf(out, "mode\t%s\n", report.Mode)
	} else {
		fmt.Fprintf(out, "mode\t%s\trun_id\t%s\n", report.Mode, report.RunID)
	}
	for _, step := range report.Steps {
		label := step.Name
		if label == "" {
			label = strings.Join(step.Args, " ")
		}
		if label == "" {
			label = "(root)"
		}
		if step.Error != "" {
			fmt.Fprintf(out, "%d\t%s\t%s: %s\n", step.Index, label, step.Status, step.Error)
		} else {
			fmt.Fprintf(out, "%d\t%s\t%s\n", step.Index, label, step.Status)
		}
	}
	return nil
}

func workflowRunFailureCount(report workflowRunReport) int {
	failures := 0
	for _, step := range report.Steps {
		if step.Status == "failed" {
			failures++
		}
	}
	return failures
}
