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
	"regexp"
	"strings"
	"time"

	"zotio/internal/mutation"

	"github.com/spf13/cobra"
)

const (
	workflowRunOutputLimit             = 64 * 1024
	workflowRunModePreview             = "preview"
	workflowRunModeApply               = "apply"
	workflowRunCheckpointSchemaVersion = 2
)

var activeWorkflowRunID string

type workflowRunSpec struct {
	Steps           []workflowRunStepSpec `json:"steps"`
	Vars            map[string]string     `json:"vars,omitempty"`
	ContinueOnError bool                  `json:"continue_on_error"`
}

type workflowRunStepSpec struct {
	Name      string               `json:"name,omitempty"`
	Args      []string             `json:"args"`
	StdinFrom string               `json:"stdin_from,omitempty"`
	When      *workflowRunStepWhen `json:"when,omitempty"`
}

type workflowRunStepWhen struct {
	Step string `json:"step"`
	Is   string `json:"is"`
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
	Reason string   `json:"reason,omitempty"`
	Error  string   `json:"error,omitempty"`
	Output string   `json:"output"`
}

func newWorkflowRunCmd(flags *rootFlags) *cobra.Command {
	var resume bool
	var varOverrides []string

	cmd := &cobra.Command{
		Use:   "run <file.json>",
		Short: "Preview or transactionally apply a declarative workflow",
		Long: `Runs a declarative workflow spec in process. By default, mutating steps
are previewed with --dry-run while read-only steps run normally.

Specs may declare top-level "vars" and use ${vars.NAME} in step arguments.
Override declared values with repeatable --var NAME=value. Arguments may also
use ${steps.NAME.output}, the trimmed output of an earlier named step. A step
can pipe an earlier step's raw output with "stdin_from", and "when" can run a
step only when an earlier step is ok, failed, or skipped. In preview mode,
substituted step outputs are preview outputs.

Pass --yes once to apply the whole workflow. Every mutation from that run shares
one journal run ID. If an applied workflow is interrupted, continue it with
--yes --resume; completed steps are skipped from its checkpoint sidecar.`,
		Example: `  zotio workflow run workflow.json
  zotio workflow run workflow.json --var PROJECT=demo
  zotio workflow run workflow.json --yes
  zotio workflow run workflow.json --yes --resume
  {"steps":[{"name":"diagnose","args":["library","health","--json"]},{"name":"fix","args":["items","enrich","--keys-from","-"],"stdin_from":"diagnose"}]}`,
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
			resolvedVars, err := resolveWorkflowRunVars(spec.Vars, varOverrides)
			if err != nil {
				return usageErr(err)
			}
			if resume && !flags.yes {
				return usageErr(fmt.Errorf("--resume requires --yes; resume continues an already-approved run"))
			}

			execution := workflowRunExecution{
				Mode: workflowRunModePreview,
				Vars: resolvedVars,
			}
			if flags.yes {
				checkpointPath := workflowRunCheckpointPath(args[0])
				specSHA256 := workflowRunSpecSHA256(rawSpec)
				checkpoint := workflowRunCheckpoint{
					SchemaVersion: workflowRunCheckpointSchemaVersion,
					SpecSHA256:    specSHA256,
					Vars:          cloneWorkflowRunVars(resolvedVars),
				}

				if resume {
					checkpoint, err = readWorkflowRunCheckpoint(checkpointPath)
					if err != nil {
						if errors.Is(err, os.ErrNotExist) {
							return fmt.Errorf("--resume requires an existing checkpoint at %q", checkpointPath)
						}
						return err
					}
					completed, err := validateWorkflowRunCheckpoint(checkpoint, specSHA256, checkpointPath, len(spec.Steps), resolvedVars)
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
					Vars:           resolvedVars,
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
	cmd.Flags().StringArrayVar(&varOverrides, "var", nil, "Override a declared workflow variable (NAME=value)")
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
	if err := validateWorkflowRunSpec(spec); err != nil {
		return workflowRunSpec{}, nil, err
	}
	return spec, data, nil
}

var workflowRunVariableName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)

func validateWorkflowRunSpec(spec workflowRunSpec) error {
	for name := range spec.Vars {
		if !workflowRunVariableName.MatchString(name) {
			return fmt.Errorf("workflow variable %q has an invalid name", name)
		}
	}

	earlierStepNames := make(map[string]struct{}, len(spec.Steps))
	for i, step := range spec.Steps {
		stepIndex := i + 1
		if step.Name != "" {
			if _, exists := earlierStepNames[step.Name]; exists {
				return fmt.Errorf("workflow step %d has duplicate name %q", stepIndex, step.Name)
			}
		}
		if workflowRunStepInvokesWorkflow(step.Args) {
			return fmt.Errorf("workflow step %d invokes %q; workflow run cannot invoke workflow commands", stepIndex, "workflow")
		}
		for _, arg := range step.Args {
			if workflowRunStepOwnsFlag(arg) {
				return fmt.Errorf("workflow step %d includes %q; the workflow owns approval: pass --yes to zotio workflow run itself", stepIndex, arg)
			}
		}
		if step.StdinFrom != "" {
			if _, exists := earlierStepNames[step.StdinFrom]; !exists {
				return fmt.Errorf("workflow step %d stdin_from %q must name an earlier step", stepIndex, step.StdinFrom)
			}
		}
		if step.When != nil {
			if _, exists := earlierStepNames[step.When.Step]; !exists {
				return fmt.Errorf("workflow step %d when.step %q must name an earlier step", stepIndex, step.When.Step)
			}
			if step.When.Is != "ok" && step.When.Is != "failed" && step.When.Is != "skipped" {
				return fmt.Errorf("workflow step %d when.is %q must be one of ok, failed, skipped", stepIndex, step.When.Is)
			}
		}
		for _, arg := range step.Args {
			if err := validateWorkflowRunStepArgPlaceholders(arg, stepIndex, spec.Vars, earlierStepNames); err != nil {
				return err
			}
		}
		if step.Name != "" {
			earlierStepNames[step.Name] = struct{}{}
		}
	}
	return nil
}

func validateWorkflowRunStepArgPlaceholders(arg string, stepIndex int, vars map[string]string, earlierStepNames map[string]struct{}) error {
	for offset := 0; offset < len(arg); {
		start := strings.Index(arg[offset:], "${")
		if start == -1 {
			return nil
		}
		start += offset
		end := strings.IndexByte(arg[start+2:], '}')
		if end == -1 {
			return workflowRunInvalidPlaceholderError(stepIndex, arg[start:])
		}
		end += start + 2
		placeholder := arg[start : end+1]
		kind, name, ok := workflowRunParsePlaceholder(placeholder)
		if !ok {
			return workflowRunInvalidPlaceholderError(stepIndex, placeholder)
		}
		switch kind {
		case "vars":
			if _, exists := vars[name]; !exists {
				return workflowRunInvalidPlaceholderError(stepIndex, placeholder)
			}
		case "steps":
			if _, exists := earlierStepNames[name]; !exists {
				return workflowRunInvalidPlaceholderError(stepIndex, placeholder)
			}
		}
		offset = end + 1
	}
	return nil
}

func workflowRunInvalidPlaceholderError(stepIndex int, placeholder string) error {
	return fmt.Errorf("workflow step %d has invalid placeholder %q", stepIndex, placeholder)
}

func workflowRunParsePlaceholder(placeholder string) (kind, name string, ok bool) {
	if !strings.HasPrefix(placeholder, "${") || !strings.HasSuffix(placeholder, "}") {
		return "", "", false
	}
	value := placeholder[2 : len(placeholder)-1]
	if strings.HasPrefix(value, "vars.") {
		name = strings.TrimPrefix(value, "vars.")
		return "vars", name, name != ""
	}
	if strings.HasPrefix(value, "steps.") && strings.HasSuffix(value, ".output") {
		name = strings.TrimSuffix(strings.TrimPrefix(value, "steps."), ".output")
		return "steps", name, name != ""
	}
	return "", "", false
}

func resolveWorkflowRunVars(specVars map[string]string, overrides []string) (map[string]string, error) {
	resolved := cloneWorkflowRunVars(specVars)
	for _, override := range overrides {
		name, value, ok := strings.Cut(override, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("--var %q must be NAME=value", override)
		}
		if _, declared := specVars[name]; !declared {
			return nil, fmt.Errorf("--var %q names a variable not declared in the workflow spec", name)
		}
		if resolved == nil {
			resolved = make(map[string]string, len(specVars))
		}
		resolved[name] = value
	}
	return resolved, nil
}

func cloneWorkflowRunVars(vars map[string]string) map[string]string {
	if len(vars) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(vars))
	for name, value := range vars {
		cloned[name] = value
	}
	return cloned
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
	SchemaVersion int                         `json:"schema_version"`
	RunID         string                      `json:"run_id"`
	SpecSHA256    string                      `json:"spec_sha256"`
	Vars          map[string]string           `json:"vars,omitempty"`
	Completed     []workflowRunCheckpointStep `json:"completed"`
}

type workflowRunCheckpointStep struct {
	Index  int    `json:"index"`
	Name   string `json:"name,omitempty"`
	Status string `json:"status"`
	Output string `json:"output"`
}

type workflowRunExecution struct {
	Mode           string
	RunID          string
	Vars           map[string]string
	Completed      []workflowRunCheckpointStep
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
	var header struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return workflowRunCheckpoint{}, fmt.Errorf("parse workflow checkpoint %q: %w", path, err)
	}
	if header.SchemaVersion != workflowRunCheckpointSchemaVersion {
		return workflowRunCheckpoint{SchemaVersion: header.SchemaVersion}, nil
	}
	var checkpoint workflowRunCheckpoint
	if err := json.Unmarshal(data, &checkpoint); err != nil {
		return workflowRunCheckpoint{}, fmt.Errorf("parse workflow checkpoint %q: %w", path, err)
	}
	return checkpoint, nil
}

func writeWorkflowRunCheckpoint(path string, checkpoint workflowRunCheckpoint) error {
	if checkpoint.Completed == nil {
		checkpoint.Completed = make([]workflowRunCheckpointStep, 0)
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

func validateWorkflowRunCheckpoint(checkpoint workflowRunCheckpoint, specSHA256, path string, stepCount int, vars map[string]string) ([]workflowRunCheckpointStep, error) {
	if checkpoint.SchemaVersion != workflowRunCheckpointSchemaVersion {
		return nil, fmt.Errorf("workflow checkpoint %q has unsupported schema version %d; delete it to start over", path, checkpoint.SchemaVersion)
	}
	if checkpoint.RunID == "" {
		return nil, fmt.Errorf("workflow checkpoint %q has no run_id", path)
	}
	if checkpoint.SpecSHA256 != specSHA256 {
		return nil, fmt.Errorf("workflow spec changed since the checkpoint; delete %q to start over", path)
	}
	if !workflowRunVarsEqual(checkpoint.Vars, vars) {
		return nil, fmt.Errorf("workflow checkpoint %q was approved with different variables; delete the checkpoint to start over", path)
	}

	seen := make(map[int]struct{}, len(checkpoint.Completed))
	completed := make([]workflowRunCheckpointStep, 0, len(checkpoint.Completed))
	for _, step := range checkpoint.Completed {
		if step.Index < 1 || step.Index > stepCount {
			return nil, fmt.Errorf("workflow checkpoint %q has invalid completed step %d", path, step.Index)
		}
		if _, exists := seen[step.Index]; exists {
			return nil, fmt.Errorf("workflow checkpoint %q has invalid completed step %d", path, step.Index)
		}
		seen[step.Index] = struct{}{}
		completed = append(completed, step)
	}
	return completed, nil
}

func workflowRunVarsEqual(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for name, leftValue := range left {
		if rightValue, exists := right[name]; !exists || rightValue != leftValue {
			return false
		}
	}
	return true
}

func executeWorkflowRunSpecWithRunID(spec workflowRunSpec, execution workflowRunExecution) (workflowRunReport, error) {
	activeWorkflowRunID = execution.RunID
	defer func() {
		activeWorkflowRunID = ""
	}()
	return executeWorkflowRunSpecWithOptions(spec, execution)
}

type workflowRunStepResult struct {
	Status     string
	Output     string
	FromResume bool
}

func executeWorkflowRunSpecWithOptions(spec workflowRunSpec, execution workflowRunExecution) (workflowRunReport, error) {
	report := workflowRunReport{
		Steps: make([]workflowRunStepReport, 0, len(spec.Steps)),
		OK:    true,
		Mode:  execution.Mode,
		RunID: execution.RunID,
	}
	resolvedVars := execution.Vars
	if resolvedVars == nil {
		resolvedVars = cloneWorkflowRunVars(spec.Vars)
	}
	completedByIndex := make(map[int]workflowRunCheckpointStep, len(execution.Completed))
	stepResults := make(map[string]workflowRunStepResult, len(execution.Completed))
	for _, completed := range execution.Completed {
		completedByIndex[completed.Index] = completed
		if completed.Index < 1 || completed.Index > len(spec.Steps) {
			continue
		}
		name := spec.Steps[completed.Index-1].Name
		if name == "" {
			name = completed.Name
		}
		workflowRunRememberStepResult(stepResults, name, completed.Status, completed.Output, true)
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
		if completed, exists := completedByIndex[stepReport.Index]; exists {
			stepReport.OK = true
			stepReport.Status = "skipped"
			stepReport.Reason = "resume"
			stepReport.Output = completed.Output
			report.Steps = append(report.Steps, stepReport)
			continue
		}
		if step.When != nil {
			actual := "not_attempted"
			if result, exists := stepResults[step.When.Step]; exists {
				actual = result.Status
			}
			if actual != step.When.Is {
				stepReport.OK = true
				stepReport.Status = "skipped"
				stepReport.Reason = fmt.Sprintf("when: %s is %s, want %s", step.When.Step, actual, step.When.Is)
				if err := checkpointWorkflowRunStep(execution, stepReport); err != nil {
					stepReport.OK = false
					stepReport.Status = "failed"
					stepReport.Reason = ""
					stepReport.Error = err.Error()
					report.OK = false
					stopped = true
					executionErr = err
				} else {
					completedByIndex[stepReport.Index] = workflowRunCheckpointStep{
						Index:  stepReport.Index,
						Name:   stepReport.Name,
						Status: stepReport.Status,
						Output: stepReport.Output,
					}
				}
				workflowRunRememberStepResult(stepResults, step.Name, stepReport.Status, stepReport.Output, false)
				report.Steps = append(report.Steps, stepReport)
				continue
			}
		}

		resolvedArgs, err := substituteWorkflowRunStepArgs(step.Args, resolvedVars, stepResults)
		if err != nil {
			stepReport.Status = "failed"
			stepReport.Error = err.Error()
			report.OK = false
			if !spec.ContinueOnError {
				stopped = true
			}
			workflowRunRememberStepResult(stepResults, step.Name, stepReport.Status, stepReport.Output, false)
			report.Steps = append(report.Steps, stepReport)
			continue
		}
		stepReport.Args = resolvedArgs

		var stdin *string
		if step.StdinFrom != "" {
			piped, err := workflowRunAvailableStepOutput(step.StdinFrom, stepResults)
			if err != nil {
				stepReport.Status = "failed"
				stepReport.Error = err.Error()
				report.OK = false
				if !spec.ContinueOnError {
					stopped = true
				}
				workflowRunRememberStepResult(stepResults, step.Name, stepReport.Status, stepReport.Output, false)
				report.Steps = append(report.Steps, stepReport)
				continue
			}
			stdin = &piped
		}

		args := workflowRunStepArgs(execution.Mode, workflowRunStepIsReadOnly(resolvedArgs), resolvedArgs)
		output, err := executeWorkflowRunStep(args, stdin)
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
			if err := checkpointWorkflowRunStep(execution, stepReport); err != nil {
				stepReport.OK = false
				stepReport.Status = "failed"
				stepReport.Error = err.Error()
				report.OK = false
				stopped = true
				executionErr = err
			} else {
				completedByIndex[stepReport.Index] = workflowRunCheckpointStep{
					Index:  stepReport.Index,
					Name:   stepReport.Name,
					Status: stepReport.Status,
					Output: stepReport.Output,
				}
			}
		}
		workflowRunRememberStepResult(stepResults, step.Name, stepReport.Status, stepReport.Output, false)
		report.Steps = append(report.Steps, stepReport)
	}

	return report, executionErr
}

func checkpointWorkflowRunStep(execution workflowRunExecution, report workflowRunStepReport) error {
	if execution.Checkpoint == nil {
		return nil
	}
	checkpointStep := workflowRunCheckpointStep{
		Index:  report.Index,
		Name:   report.Name,
		Status: report.Status,
		Output: report.Output,
	}
	execution.Checkpoint.Completed = append(execution.Checkpoint.Completed, checkpointStep)
	if err := writeWorkflowRunCheckpoint(execution.CheckpointPath, *execution.Checkpoint); err != nil {
		execution.Checkpoint.Completed = execution.Checkpoint.Completed[:len(execution.Checkpoint.Completed)-1]
		return fmt.Errorf("checkpoint workflow step %d: %w", report.Index, err)
	}
	return nil
}

func workflowRunRememberStepResult(results map[string]workflowRunStepResult, name, status, output string, fromResume bool) {
	if name == "" {
		return
	}
	results[name] = workflowRunStepResult{
		Status:     status,
		Output:     output,
		FromResume: fromResume,
	}
}

func workflowRunAvailableStepOutput(name string, results map[string]workflowRunStepResult) (string, error) {
	result, exists := results[name]
	if !exists {
		return "", fmt.Errorf("workflow output from step %q is unavailable (status %q)", name, "not_attempted")
	}
	if result.Status != "ok" && !(result.Status == "skipped" && result.FromResume) {
		return "", fmt.Errorf("workflow output from step %q is unavailable (status %q)", name, result.Status)
	}
	return result.Output, nil
}

func substituteWorkflowRunStepArgs(args []string, vars map[string]string, results map[string]workflowRunStepResult) ([]string, error) {
	substituted := make([]string, len(args))
	for i, arg := range args {
		value, err := substituteWorkflowRunStepArg(arg, vars, results)
		if err != nil {
			return nil, err
		}
		substituted[i] = value
	}
	return substituted, nil
}

func substituteWorkflowRunStepArg(arg string, vars map[string]string, results map[string]workflowRunStepResult) (string, error) {
	var substituted strings.Builder
	substituted.Grow(len(arg))
	for offset := 0; offset < len(arg); {
		start := strings.Index(arg[offset:], "${")
		if start == -1 {
			substituted.WriteString(arg[offset:])
			break
		}
		start += offset
		substituted.WriteString(arg[offset:start])
		end := strings.IndexByte(arg[start+2:], '}')
		if end == -1 {
			return "", fmt.Errorf("invalid workflow placeholder %q", arg[start:])
		}
		end += start + 2
		placeholder := arg[start : end+1]
		kind, name, ok := workflowRunParsePlaceholder(placeholder)
		if !ok {
			return "", fmt.Errorf("invalid workflow placeholder %q", placeholder)
		}
		switch kind {
		case "vars":
			value, exists := vars[name]
			if !exists {
				return "", fmt.Errorf("workflow variable %q is unavailable", name)
			}
			substituted.WriteString(value)
		case "steps":
			output, err := workflowRunAvailableStepOutput(name, results)
			if err != nil {
				return "", err
			}
			substituted.WriteString(strings.TrimSpace(output))
		}
		offset = end + 1
	}
	return substituted.String(), nil
}

// executeWorkflowRunStep runs one argv in-process. A nil stdin inherits the
// process stdin; a non-nil stdin (even empty) replaces it, so a step piped an
// empty upstream output can never block on the terminal.
func executeWorkflowRunStep(args []string, stdin *string) (string, error) {
	return executeWorkflowRunStepWithRoot(RootCmd(), args, stdin)
}

func executeWorkflowRunStepWithRoot(root *cobra.Command, args []string, stdin *string) (string, error) {
	root.SetArgs(args)
	if stdin != nil {
		root.SetIn(strings.NewReader(*stdin))
	}
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
