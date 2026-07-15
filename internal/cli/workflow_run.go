// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// InstallRuntimeHooks installs the mutation hooks used by production entry
// points. Reinstalling the same hooks is safe for MCP startup and CLI calls.
func InstallRuntimeHooks() {
	mutationJournalRecorder = recordMutationJournal
	mirrorWriteThrough = applyMirrorWriteThrough
}

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

// workflowRunInvocation carries the caller-owned approval and resume knobs.
type workflowRunInvocation struct {
	Yes          bool
	DryRun       bool
	Resume       bool
	Agent        bool
	NoInput      bool
	VarOverrides []string // NAME=value, same syntax as --var
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
			report, err := runWorkflowRunFile(cmd.Context(), args[0], workflowRunInvocation{
				Yes:          flags.yes,
				DryRun:       flags.dryRun,
				Resume:       resume,
				Agent:        flags.agent,
				NoInput:      flags.noInput,
				VarOverrides: varOverrides,
			})
			if len(report.Steps) > 0 {
				if renderErr := renderWorkflowRunReport(cmd, flags, report); renderErr != nil {
					return renderErr
				}
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&resume, "resume", false, "Resume an interrupted applied workflow from its checkpoint sidecar")
	cmd.Flags().StringArrayVar(&varOverrides, "var", nil, "Override a declared workflow variable (NAME=value)")
	return cmd
}

// runWorkflowRunFile executes the full workflow lifecycle without rendering.
func runWorkflowRunFile(ctx context.Context, specPath string, inv workflowRunInvocation) (workflowRunReport, error) {
	spec, rawSpec, err := readWorkflowRunSpecFile(specPath)
	if err != nil {
		return workflowRunReport{}, err
	}
	resolvedVars, err := resolveWorkflowRunVars(spec.Vars, inv.VarOverrides)
	if err != nil {
		return workflowRunReport{}, usageErr(err)
	}
	if inv.Resume && (!inv.Yes || inv.DryRun) {
		return workflowRunReport{}, usageErr(fmt.Errorf("--resume requires --yes without --dry-run; resume continues an already-approved run"))
	}

	execution := workflowRunExecution{
		Mode: workflowRunModePreview,
		Vars: resolvedVars,
	}
	if inv.Yes && !inv.DryRun {
		checkpointPath := workflowRunCheckpointPath(specPath)
		specSHA256 := workflowRunSpecSHA256(rawSpec)
		checkpoint := workflowRunCheckpoint{
			SchemaVersion: workflowRunCheckpointSchemaVersion,
			SpecSHA256:    specSHA256,
			Vars:          cloneWorkflowRunVars(resolvedVars),
		}

		if inv.Resume {
			checkpoint, err = readWorkflowRunCheckpoint(checkpointPath)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return workflowRunReport{}, fmt.Errorf("--resume requires an existing checkpoint at %q", checkpointPath)
				}
				return workflowRunReport{}, err
			}
			completed, err := validateWorkflowRunCheckpoint(checkpoint, specSHA256, checkpointPath, len(spec.Steps), resolvedVars)
			if err != nil {
				return workflowRunReport{}, err
			}
			execution.Completed = completed
		} else {
			exists, err := workflowRunCheckpointExists(checkpointPath)
			if err != nil {
				return workflowRunReport{}, err
			}
			if exists {
				return workflowRunReport{}, fmt.Errorf("an interrupted workflow run exists at %q; pass --resume to continue or delete %q to start over", checkpointPath, checkpointPath)
			}
			checkpoint.RunID = mutation.NewRunID(time.Now())
			if err := writeWorkflowRunCheckpoint(checkpointPath, checkpoint); err != nil {
				return workflowRunReport{}, err
			}
		}

		execution = workflowRunExecution{
			Mode:           workflowRunModeApply,
			RunID:          checkpoint.RunID,
			Vars:           resolvedVars,
			Agent:          inv.Agent,
			NoInput:        inv.NoInput,
			Completed:      execution.Completed,
			Checkpoint:     &checkpoint,
			CheckpointPath: checkpointPath,
		}
	}

	var report workflowRunReport
	var executionErr error
	if execution.Mode == workflowRunModeApply {
		report, executionErr = executeWorkflowRunSpecWithRunID(ctx, spec, execution)
	} else {
		execution.Agent = inv.Agent
		execution.NoInput = inv.NoInput
		report, executionErr = executeWorkflowRunSpecWithOptions(ctx, spec, execution)
	}
	if executionErr == nil && execution.Mode == workflowRunModeApply && report.OK {
		if err := os.Remove(execution.CheckpointPath); err != nil && !os.IsNotExist(err) {
			executionErr = fmt.Errorf("remove workflow checkpoint %q: %w", execution.CheckpointPath, err)
		}
	}
	if executionErr != nil {
		return report, executionErr
	}
	if !report.OK {
		return report, fmt.Errorf("workflow failed: %d step(s) failed", workflowRunFailureCount(report))
	}
	return report, nil
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
	return workflowRunStepIsReadOnlyWithRoot(RootCmd(), args)
}

func workflowRunStepIsReadOnlyWithRoot(root *cobra.Command, args []string) bool {
	command, _, err := root.Find(workflowRunStepPositionals(args))
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
			return workflowRunStepInsertFlags(args, "--dry-run")
		}
	case workflowRunModeApply:
		return workflowRunStepInsertFlags(args, "--yes")
	}
	return args
}

// workflowRunStepInsertFlags inserts runner-owned flags before the first "--"
// argument terminator (or at the end when absent) so Cobra parses them as
// flags, not positionals, even when the step itself passes "--".
func workflowRunStepInsertFlags(args []string, flags ...string) []string {
	if len(flags) == 0 {
		return args
	}
	term := len(args)
	for i, a := range args {
		if a == "--" {
			term = i
			break
		}
	}
	out := make([]string, 0, len(args)+len(flags))
	out = append(out, args[:term]...)
	out = append(out, flags...)
	out = append(out, args[term:]...)
	return out
}

func workflowRunSpecSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
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
	Agent          bool
	NoInput        bool
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

	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary workflow checkpoint in %q: %w", filepath.Dir(path), err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = temp.Close()
		}
		_ = os.Remove(temp.Name())
	}()
	if err := temp.Chmod(0o600); err != nil {
		return fmt.Errorf("set workflow checkpoint permissions: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		return fmt.Errorf("write temporary workflow checkpoint: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("sync temporary workflow checkpoint: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temporary workflow checkpoint: %w", err)
	}
	closed = true
	if err := os.Rename(temp.Name(), path); err != nil {
		return fmt.Errorf("replace workflow checkpoint %q: %w", path, err)
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
		if step.Status == "in_progress" {
			return nil, fmt.Errorf("workflow checkpoint %q records step %d (%q) as in progress; inspect or reconcile that step before resuming, or delete %q to start over", path, step.Index, step.Name, path)
		}
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

func executeWorkflowRunSpecWithRunID(ctx context.Context, spec workflowRunSpec, execution workflowRunExecution) (workflowRunReport, error) {
	previousRunID := activeWorkflowRunID
	activeWorkflowRunID = execution.RunID
	defer func() {
		activeWorkflowRunID = previousRunID
	}()
	return executeWorkflowRunSpecWithOptions(ctx, spec, execution)
}

type workflowRunStepResult struct {
	Status         string
	OriginalStatus string
	Output         string
}

func executeWorkflowRunSpecWithOptions(ctx context.Context, spec workflowRunSpec, execution workflowRunExecution) (workflowRunReport, error) {
	return executeWorkflowRunSpecWithRootFactory(ctx, spec, execution, RootCmd)
}

func executeWorkflowRunSpecWithRootFactory(ctx context.Context, spec workflowRunSpec, execution workflowRunExecution, newRoot func() *cobra.Command) (workflowRunReport, error) {
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
		workflowRunRememberStepResult(stepResults, name, completed.Status, completed.Output)
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
			stepReport.Output = capWorkflowRunOutput(completed.Output)
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
				if err := checkpointWorkflowRunStep(execution, stepReport.Index, stepReport.Name, stepReport.Status, stepReport.Output); err != nil {
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
				workflowRunRememberStepResult(stepResults, step.Name, stepReport.Status, stepReport.Output)
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
			workflowRunRememberStepResult(stepResults, step.Name, stepReport.Status, stepReport.Output)
			report.Steps = append(report.Steps, stepReport)
			continue
		}
		if err := validateWorkflowRunResolvedStepArgs(newRoot(), stepReport.Index, step.Args, resolvedArgs); err != nil {
			stepReport.Status = "failed"
			stepReport.Error = err.Error()
			report.OK = false
			if !spec.ContinueOnError {
				stopped = true
			}
			workflowRunRememberStepResult(stepResults, step.Name, stepReport.Status, stepReport.Output)
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
				workflowRunRememberStepResult(stepResults, step.Name, stepReport.Status, stepReport.Output)
				report.Steps = append(report.Steps, stepReport)
				continue
			}
			stdin = &piped
		}

		root := newRoot()
		args := workflowRunStepArgs(execution.Mode, workflowRunStepIsReadOnlyWithRoot(root, resolvedArgs), resolvedArgs)
		args = workflowRunStepAppendRuntimeFlags(args, execution.Agent, execution.NoInput)
		if execution.Mode == workflowRunModeApply {
			if err := checkpointWorkflowRunStep(execution, stepReport.Index, stepReport.Name, "in_progress", ""); err != nil {
				stepReport.Status = "failed"
				stepReport.Error = err.Error()
				report.OK = false
				stopped = true
				executionErr = err
				workflowRunRememberStepResult(stepResults, step.Name, stepReport.Status, stepReport.Output)
				report.Steps = append(report.Steps, stepReport)
				continue
			}
		}

		output, stderr, err := executeWorkflowRunStepWithRoot(ctx, root, args, stdin)
		stepReport.Output = capWorkflowRunOutput(output)
		if err != nil {
			stepReport.Status = "failed"
			stepReport.Error = workflowRunStepExecutionError(err, stderr)
			report.OK = false
			if !spec.ContinueOnError {
				stopped = true
			}
		} else {
			stepReport.OK = true
			stepReport.Status = "ok"
			if err := checkpointWorkflowRunStep(execution, stepReport.Index, stepReport.Name, stepReport.Status, output); err != nil {
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
					Output: output,
				}
			}
		}
		workflowRunRememberStepResult(stepResults, step.Name, stepReport.Status, output)
		report.Steps = append(report.Steps, stepReport)
	}

	return report, executionErr
}

func checkpointWorkflowRunStep(execution workflowRunExecution, index int, name, status, output string) error {
	if execution.Checkpoint == nil {
		return nil
	}
	checkpointStep := workflowRunCheckpointStep{
		Index:  index,
		Name:   name,
		Status: status,
		Output: output,
	}
	for i, completed := range execution.Checkpoint.Completed {
		if completed.Index != index {
			continue
		}
		execution.Checkpoint.Completed[i] = checkpointStep
		if err := writeWorkflowRunCheckpoint(execution.CheckpointPath, *execution.Checkpoint); err != nil {
			execution.Checkpoint.Completed[i] = completed
			return fmt.Errorf("checkpoint workflow step %d: %w", index, err)
		}
		return nil
	}

	execution.Checkpoint.Completed = append(execution.Checkpoint.Completed, checkpointStep)
	if err := writeWorkflowRunCheckpoint(execution.CheckpointPath, *execution.Checkpoint); err != nil {
		execution.Checkpoint.Completed = execution.Checkpoint.Completed[:len(execution.Checkpoint.Completed)-1]
		return fmt.Errorf("checkpoint workflow step %d: %w", index, err)
	}
	return nil
}

func workflowRunRememberStepResult(results map[string]workflowRunStepResult, name, status, output string) {
	if name == "" {
		return
	}
	results[name] = workflowRunStepResult{
		Status:         status,
		OriginalStatus: status,
		Output:         output,
	}
}

func workflowRunAvailableStepOutput(name string, results map[string]workflowRunStepResult) (string, error) {
	result, exists := results[name]
	if !exists {
		return "", fmt.Errorf("workflow output from step %q is unavailable (status %q)", name, "not_attempted")
	}
	status := result.OriginalStatus
	if status == "" {
		status = result.Status
	}
	if status != "ok" {
		return "", fmt.Errorf("workflow output from step %q is unavailable (status %q)", name, status)
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

func validateWorkflowRunResolvedStepArgs(root *cobra.Command, stepIndex int, literalArgs, resolvedArgs []string) error {
	for i, arg := range resolvedArgs {
		literal := literalArgs[i]
		if strings.HasPrefix(arg, "-") && !strings.HasPrefix(literal, "-") {
			return fmt.Errorf("workflow step %d resolves %q as a flag; substitutions are data, not flags", stepIndex, arg)
		}
		// A substitution may fill a flag's value but never build or rename the
		// flag itself: --tag=${vars.T} is allowed, --${vars.F} is not.
		if strings.HasPrefix(literal, "-") {
			litName, resName := workflowRunFlagName(literal), workflowRunFlagName(arg)
			if strings.Contains(litName, "${") || litName != resName {
				return fmt.Errorf("workflow step %d resolves flag %q from a substitution; only flag values may be substituted", stepIndex, arg)
			}
		}
	}
	if workflowRunStepInvokesWorkflow(resolvedArgs) {
		return fmt.Errorf("workflow step %d invokes %q; workflow run cannot invoke workflow commands", stepIndex, "workflow")
	}
	// Substitution must not change which command a step runs: a ${...} in a
	// command-selecting positional could otherwise redirect to a different or
	// MCP-hidden subcommand (e.g. capabilities -> capabilities drift). Compare
	// only the resolved command identity — an unknown command resolves the same
	// way for both and falls through to execution's own "unknown command" error.
	litCmd, _, _ := root.Find(workflowRunStepPositionals(literalArgs))
	resCmd, _, _ := root.Find(workflowRunStepPositionals(resolvedArgs))
	if litCmd != resCmd {
		return fmt.Errorf("workflow step %d resolves to a different command after substitution; command-selecting arguments cannot be substituted", stepIndex)
	}
	return nil
}

func workflowRunFlagName(tok string) string {
	if i := strings.IndexByte(tok, '='); i >= 0 {
		return tok[:i]
	}
	return tok
}

func workflowRunStepAppendRuntimeFlags(args []string, agent, noInput bool) []string {
	flags := make([]string, 0, 2)
	if agent {
		flags = append(flags, "--agent")
	}
	if noInput {
		flags = append(flags, "--no-input")
	}
	return workflowRunStepInsertFlags(args, flags...)
}

func workflowRunStepExecutionError(err error, stderr string) string {
	diagnostic := strings.TrimSpace(stderr)
	if diagnostic == "" {
		return err.Error()
	}
	return fmt.Sprintf("%v: %s", err, diagnostic)
}

// executeWorkflowRunStep runs one argv in-process. A nil stdin inherits the
// process stdin; a non-nil stdin (even empty) replaces it, so a step piped an
// empty upstream output can never block on the terminal.
func executeWorkflowRunStep(ctx context.Context, args []string, stdin *string) (string, string, error) {
	return executeWorkflowRunStepWithRoot(ctx, RootCmd(), args, stdin)
}

func executeWorkflowRunStepWithRoot(ctx context.Context, root *cobra.Command, args []string, stdin *string) (string, string, error) {
	root.SetArgs(args)
	if stdin != nil {
		root.SetIn(strings.NewReader(*stdin))
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)

	savedNoColor := noColor
	savedHumanFriendly := humanFriendly
	savedGroup := activeGroupID

	err := root.ExecuteContext(ctx)

	noColor = savedNoColor
	humanFriendly = savedHumanFriendly
	activeGroupID = savedGroup

	return stdout.String(), stderr.String(), err
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
