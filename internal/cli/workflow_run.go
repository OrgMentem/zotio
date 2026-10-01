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
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"zotio/internal/mutation"

	"github.com/spf13/cobra"
)

const (
	workflowRunOutputLimit             = 64 * 1024
	workflowRunModePreview             = "preview"
	workflowRunModeApply               = "apply"
	workflowRunCheckpointSchemaVersion = 3
	// workflowRunCheckpointSchemaVersionV2 checkpoints predate trigger
	// batches. v3 only added the optional "trigger" member, so a v2 sidecar
	// still resumes as a v3 checkpoint without a batch.
	workflowRunCheckpointSchemaVersionV2 = 2
	// workflowRunTriggerMaxEvents bounds the change batch a triggered run can
	// read. A larger batch is not cut down to a partial key list: the caller
	// skips the run (see tailWorkflowTrigger).
	workflowRunTriggerMaxEvents = 500
	// workflowRunSelectorMaxLength and workflowRunSelectorMaxSteps bound a
	// JSON selector (see parseWorkflowRunSelector).
	workflowRunSelectorMaxLength = 256
	workflowRunSelectorMaxSteps  = 32
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
	Name      string   `json:"name,omitempty"`
	Args      []string `json:"args"`
	StdinFrom string   `json:"stdin_from,omitempty"`
	// StdinSelect pipes JSON-selected values from the stdin_from step's
	// output, one per line, instead of the raw output.
	StdinSelect string `json:"stdin_select,omitempty"`
	// StdinTrigger pipes JSON-selected values from the trigger batch, one per
	// line. It excludes stdin_from.
	StdinTrigger string               `json:"stdin_trigger,omitempty"`
	When         *workflowRunStepWhen `json:"when,omitempty"`
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
	// Trigger is the change batch that started this run. It is read-only
	// data for the steps and never approves a write: only Yes applies.
	Trigger *workflowRunTrigger
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
		Short: "Preview or apply a declarative multi-step workflow",
		Long: `Runs a declarative workflow spec in process. By default, mutating steps
are previewed with --dry-run while read-only steps run normally.

Specs may declare top-level "vars" and use ${vars.NAME} in step arguments.
Override declared values with repeatable --var NAME=value. Arguments may also
use ${steps.NAME.output}, the trimmed output of an earlier named step, or
${steps.NAME.json:PATH}, one string, number, or boolean selected from that
step's JSON output. A step can pipe an earlier step's raw output with
"stdin_from"; add "stdin_select": "PATH" to pipe the selected JSON values
instead, one per line. A missing, malformed, or incompatible selection fails
the step, and a selection with no values skips it. "when" can run a step only
when an earlier step is ok, failed, or skipped. In preview mode, substituted
step outputs are preview outputs.

A run started by tail --workflow can read its change batch: the selector
${trigger.json:PATH} and "stdin_trigger": "PATH" select from
{"source", "resource", "events": [{"event", "resource", "key"}],
"upsert_keys", "delete_keys"}. "stdin_trigger": "upsert_keys" feeds
--keys-from -. The batch is data; it never approves a write.

Pass --yes once to apply the whole workflow. Steps apply in order, and every
mutation from that run shares one journal run ID (zotio journal list --workflow
<run-id>). There is no rollback: if a step fails, writes from earlier steps
stay applied. The run stops at the first failed step unless the spec sets
"continue_on_error". A failed or interrupted applied run keeps its checkpoint
sidecar; continue it with --yes --resume, which skips the completed steps.`,
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
	// Only a spec that reads trigger values receives the batch, so a spec
	// that reads none runs and checkpoints exactly like an untriggered run.
	readsTrigger := workflowRunSpecReadsTrigger(spec)
	var trigger *workflowRunTrigger
	if readsTrigger {
		trigger = inv.Trigger
		if trigger == nil && !inv.Resume {
			return workflowRunReport{}, preconditionErr(fmt.Errorf("workflow spec %q reads trigger values (${trigger.json:...} or stdin_trigger), but this run has no trigger batch; start it with tail --workflow, or continue an interrupted triggered run with --yes --resume", specPath))
		}
	}

	execution := workflowRunExecution{
		Mode:    workflowRunModePreview,
		Vars:    resolvedVars,
		Trigger: trigger,
	}
	if inv.Yes && !inv.DryRun {
		checkpointPath := workflowRunCheckpointPath(specPath)
		specSHA256 := workflowRunSpecSHA256(rawSpec)
		checkpoint := workflowRunCheckpoint{
			SchemaVersion: workflowRunCheckpointSchemaVersion,
			SpecSHA256:    specSHA256,
			Vars:          cloneWorkflowRunVars(resolvedVars),
			Trigger:       trigger,
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
			// Resume replays the batch the run was approved with, never a
			// newer one.
			if readsTrigger {
				if checkpoint.Trigger == nil {
					return workflowRunReport{}, fmt.Errorf("workflow checkpoint %q has no trigger batch, but the spec reads trigger values; delete %q to start over", checkpointPath, checkpointPath)
				}
				trigger = checkpoint.Trigger
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
			Trigger:        trigger,
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
		if step.StdinSelect != "" {
			if step.StdinFrom == "" {
				return fmt.Errorf("workflow step %d stdin_select needs stdin_from to name the step whose JSON output it selects from", stepIndex)
			}
			if _, err := parseWorkflowRunSelector(step.StdinSelect); err != nil {
				return fmt.Errorf("workflow step %d stdin_select: %w", stepIndex, err)
			}
		}
		if step.StdinTrigger != "" {
			if step.StdinFrom != "" {
				return fmt.Errorf("workflow step %d sets both stdin_from and stdin_trigger; a step reads stdin from one source", stepIndex)
			}
			if _, err := parseWorkflowRunSelector(step.StdinTrigger); err != nil {
				return fmt.Errorf("workflow step %d stdin_trigger: %w", stepIndex, err)
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
		text := arg[start : end+1]
		placeholder, ok := workflowRunParsePlaceholder(text)
		if !ok {
			return workflowRunInvalidPlaceholderError(stepIndex, text)
		}
		switch placeholder.kind {
		case "vars":
			if _, exists := vars[placeholder.name]; !exists {
				return workflowRunInvalidPlaceholderError(stepIndex, text)
			}
		case "steps":
			if _, exists := earlierStepNames[placeholder.name]; !exists {
				return workflowRunInvalidPlaceholderError(stepIndex, text)
			}
		}
		if placeholder.selector != "" {
			if _, err := parseWorkflowRunArgumentSelector(placeholder.selector); err != nil {
				return fmt.Errorf("workflow step %d placeholder %q: %w", stepIndex, text, err)
			}
		}
		offset = end + 1
	}
	return nil
}

func workflowRunInvalidPlaceholderError(stepIndex int, placeholder string) error {
	return fmt.Errorf("workflow step %d has invalid placeholder %q", stepIndex, placeholder)
}

// workflowRunPlaceholder is one parsed ${...} reference in a step argument.
type workflowRunPlaceholder struct {
	kind     string // "vars", "steps", or "trigger"
	name     string // variable or step name; empty for trigger
	selector string // JSON selector; empty for vars and raw step output
}

func workflowRunParsePlaceholder(text string) (workflowRunPlaceholder, bool) {
	if !strings.HasPrefix(text, "${") || !strings.HasSuffix(text, "}") {
		return workflowRunPlaceholder{}, false
	}
	value := text[2 : len(text)-1]
	if strings.HasPrefix(value, "vars.") {
		name := strings.TrimPrefix(value, "vars.")
		return workflowRunPlaceholder{kind: "vars", name: name}, name != ""
	}
	if selector, ok := strings.CutPrefix(value, "trigger.json:"); ok {
		return workflowRunPlaceholder{kind: "trigger", selector: selector}, selector != ""
	}
	if rest, ok := strings.CutPrefix(value, "steps."); ok {
		if name, selector, ok := strings.Cut(rest, ".json:"); ok {
			return workflowRunPlaceholder{kind: "steps", name: name, selector: selector}, name != "" && selector != ""
		}
	}
	if strings.HasPrefix(value, "steps.") && strings.HasSuffix(value, ".output") {
		name := strings.TrimSuffix(strings.TrimPrefix(value, "steps."), ".output")
		return workflowRunPlaceholder{kind: "steps", name: name}, name != ""
	}
	return workflowRunPlaceholder{}, false
}

// workflowRunSpecReadsTrigger reports whether any step reads the trigger
// batch. A validated spec has only well-formed placeholders, so a
// "${trigger." prefix is always a trigger selector.
func workflowRunSpecReadsTrigger(spec workflowRunSpec) bool {
	for _, step := range spec.Steps {
		if step.StdinTrigger != "" {
			return true
		}
		for _, arg := range step.Args {
			if strings.Contains(arg, "${trigger.") {
				return true
			}
		}
	}
	return false
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

func workflowRunStepPositionals(args []string) []string {
	return workflowRunStepPositionalsWithRoot(RootCmd(), args)
}

// workflowRunStepPositionalsWithRoot retains command-selecting arguments while
// discarding flags and their values. The current command's FlagSet is consulted
// before consuming a following token, so a value such as `5` in
// `items list --limit 5` cannot be mistaken for a positional command argument.
func workflowRunStepPositionalsWithRoot(root *cobra.Command, args []string) []string {
	positionals := make([]string, 0, len(args))
	command := root
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return append(positionals, args[i+1:]...)
		}
		if strings.HasPrefix(arg, "-") {
			if !strings.Contains(arg, "=") && workflowRunStepFlagConsumesValue(command, arg) && i+1 < len(args) {
				i++
			}
			continue
		}
		positionals = append(positionals, arg)
		if resolved, _, err := root.Find(positionals); err == nil && resolved != nil {
			command = resolved
		}
	}
	return positionals
}

func workflowRunStepFlagConsumesValue(command *cobra.Command, arg string) bool {
	if strings.HasPrefix(arg, "--") {
		name := strings.TrimPrefix(arg, "--")
		if flag := command.LocalFlags().Lookup(name); flag != nil {
			return flag.NoOptDefVal == ""
		}
		if flag := command.InheritedFlags().Lookup(name); flag != nil {
			return flag.NoOptDefVal == ""
		}
		return false
	}
	if len(arg) != 2 {
		return false
	}
	name := arg[1:]
	if flag := command.LocalFlags().ShorthandLookup(name); flag != nil {
		return flag.NoOptDefVal == ""
	}
	if flag := command.InheritedFlags().ShorthandLookup(name); flag != nil {
		return flag.NoOptDefVal == ""
	}
	return false
}

func workflowRunStepIsReadOnlyWithRoot(root *cobra.Command, args []string) bool {
	command, _, err := root.Find(workflowRunStepPositionalsWithRoot(root, args))
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
	Trigger       *workflowRunTrigger         `json:"trigger,omitempty"`
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
	Trigger        *workflowRunTrigger
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
	if header.SchemaVersion != workflowRunCheckpointSchemaVersion && header.SchemaVersion != workflowRunCheckpointSchemaVersionV2 {
		return workflowRunCheckpoint{SchemaVersion: header.SchemaVersion}, nil
	}
	var checkpoint workflowRunCheckpoint
	if err := json.Unmarshal(data, &checkpoint); err != nil {
		return workflowRunCheckpoint{}, fmt.Errorf("parse workflow checkpoint %q: %w", path, err)
	}
	if header.SchemaVersion == workflowRunCheckpointSchemaVersionV2 {
		// A v2 run never carried a trigger batch.
		checkpoint.Trigger = nil
		checkpoint.SchemaVersion = workflowRunCheckpointSchemaVersion
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
		// "blocked" is a failed run with confirmed applied sub-operations: the
		// step must not replay automatically, so it behaves like "failed" for
		// retry selection and halts a --resume with its checkpointed output.
		if completed.Status != "failed" && completed.Status != "blocked" {
			completedByIndex[completed.Index] = completed
		}
		if completed.Index < 1 || completed.Index > len(spec.Steps) {
			continue
		}
		name := spec.Steps[completed.Index-1].Name
		if name == "" {
			name = completed.Name
		}
		workflowRunRememberStepResult(stepResults, name, completed.Status, completed.Output)
	}

	triggerDocument, err := workflowRunTriggerDocument(execution.Trigger)
	if err != nil {
		return report, err
	}

	stopped := false
	var executionErr error
	// skipStep records a step that deliberately does not run. It is ok and
	// checkpointed, so --resume does not run it, and its output stays
	// unavailable to later steps.
	skipStep := func(stepReport workflowRunStepReport, reason string) {
		stepReport.OK = true
		stepReport.Status = "skipped"
		stepReport.Reason = reason
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
		workflowRunRememberStepResult(stepResults, stepReport.Name, stepReport.Status, stepReport.Output)
		report.Steps = append(report.Steps, stepReport)
	}
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
		// A "blocked" step already applied part of its batch and refused an
		// automatic replay: surface the checkpointed refusal instead of
		// re-executing the step and duplicating the confirmed items.
		for _, completed := range execution.Completed {
			if completed.Index != stepReport.Index || completed.Status != "blocked" {
				continue
			}
			stepReport.Status = "failed"
			stepReport.Output = capWorkflowRunOutput(completed.Output)
			stepReport.Error = fmt.Sprintf("refusing automatic resume of step %d: its checkpointed output already applied items; reconcile those items in Zotero and edit the step to create only the missing items before resuming (checkpoint %q)", stepReport.Index, execution.CheckpointPath)
			report.OK = false
			stopped = true
			executionErr = fmt.Errorf("%s", stepReport.Error)
			break
		}
		if stopped {
			workflowRunRememberStepResult(stepResults, step.Name, stepReport.Status, stepReport.Output)
			report.Steps = append(report.Steps, stepReport)
			continue
		}
		if step.When != nil {
			actual := "not_attempted"
			if result, exists := stepResults[step.When.Step]; exists {
				actual = result.Status
			}
			if actual != step.When.Is {
				skipStep(stepReport, fmt.Sprintf("when: %s is %s, want %s", step.When.Step, actual, step.When.Is))
				continue
			}
		}

		resolvedArgs, err := substituteWorkflowRunStepArgs(step.Args, resolvedVars, stepResults, triggerDocument)
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

		// A selection that yields no values skips the step: an empty list
		// piped into --keys-from - must never reach the command.
		stdin, emptySelection, err := workflowRunStepStdin(step, stepResults, triggerDocument)
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
		if emptySelection != "" {
			skipStep(stepReport, emptySelection)
			continue
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
			// A failed create step may still have committed part of its batch
			// (items create, import file, import apply report per-index
			// outcomes inside a non-zero run). Replaying the same step on
			// --resume would post the confirmed successes a second time, so
			// refuse an automatic replay and keep the original failure output
			// for reconciliation instead of only the latest attempt.
			if partial := workflowRunConfirmedCreateOutcomes(output); partial != "" {
				refusal := fmt.Sprintf("refusing automatic resume of step %d: it already applied %s; reconcile those items in Zotero (keep them or delete them) and edit the step to create only the missing items before resuming", stepReport.Index, partial)
				stepReport.Error = fmt.Sprintf("%s: %s", refusal, stepReport.Error)
				if checkpointErr := checkpointWorkflowRunStep(execution, stepReport.Index, stepReport.Name, "blocked", output); checkpointErr != nil {
					stepReport.Error = fmt.Sprintf("%s; %v", stepReport.Error, checkpointErr)
					stopped = true
					executionErr = checkpointErr
				} else {
					stopped = true
					executionErr = fmt.Errorf("%s", refusal)
				}
			} else if checkpointErr := checkpointWorkflowRunStep(execution, stepReport.Index, stepReport.Name, stepReport.Status, output); checkpointErr != nil {
				stepReport.Error = fmt.Sprintf("%s; %v", stepReport.Error, checkpointErr)
				stopped = true
				executionErr = checkpointErr
			}
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

// workflowRunConfirmedCreateOutcomes inspects a failed step's mutation output
// for confirmed applied sub-operations: an items create / import file / import
// apply run uses ContinueOnError, so a non-zero run can still carry per-index
// successes that Zotero committed. A non-empty return names those successes
// for the resume refusal; anything unparseable or without an applied count
// returns "" and the step resumes as before.
func workflowRunConfirmedCreateOutcomes(output string) string {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return ""
	}
	var env struct {
		Operation string `json:"operation"`
		Result    *struct {
			Summary struct {
				Applied int `json:"applied"`
			} `json:"summary"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(trimmed), &env); err != nil {
		return ""
	}
	switch env.Operation {
	case "items.create", "import.file", "import.apply":
	default:
		return ""
	}
	if env.Result == nil || env.Result.Summary.Applied <= 0 {
		return ""
	}
	return fmt.Sprintf("%d %s item(s)", env.Result.Summary.Applied, env.Operation)
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

func substituteWorkflowRunStepArgs(args []string, vars map[string]string, results map[string]workflowRunStepResult, triggerDocument any) ([]string, error) {
	substituted := make([]string, len(args))
	for i, arg := range args {
		value, err := substituteWorkflowRunStepArg(arg, vars, results, triggerDocument)
		if err != nil {
			return nil, err
		}
		substituted[i] = value
	}
	return substituted, nil
}

func substituteWorkflowRunStepArg(arg string, vars map[string]string, results map[string]workflowRunStepResult, triggerDocument any) (string, error) {
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
		text := arg[start : end+1]
		placeholder, ok := workflowRunParsePlaceholder(text)
		if !ok {
			return "", fmt.Errorf("invalid workflow placeholder %q", text)
		}
		value, err := resolveWorkflowRunPlaceholder(placeholder, vars, results, triggerDocument)
		if err != nil {
			return "", err
		}
		substituted.WriteString(value)
		offset = end + 1
	}
	return substituted.String(), nil
}

// errWorkflowRunNoTrigger refuses a trigger read in a run without a batch.
var errWorkflowRunNoTrigger = errors.New("workflow trigger values are unavailable: this run has no trigger batch")

func resolveWorkflowRunPlaceholder(placeholder workflowRunPlaceholder, vars map[string]string, results map[string]workflowRunStepResult, triggerDocument any) (string, error) {
	switch placeholder.kind {
	case "vars":
		value, exists := vars[placeholder.name]
		if !exists {
			return "", fmt.Errorf("workflow variable %q is unavailable", placeholder.name)
		}
		return value, nil
	case "steps":
		output, err := workflowRunAvailableStepOutput(placeholder.name, results)
		if err != nil {
			return "", err
		}
		if placeholder.selector == "" {
			return strings.TrimSpace(output), nil
		}
		document, err := decodeWorkflowRunJSON(output)
		var value string
		if err == nil {
			value, err = workflowRunSelectArgument(document, placeholder.selector)
		}
		if err != nil {
			return "", fmt.Errorf("select %q from step %q output: %w", placeholder.selector, placeholder.name, err)
		}
		return value, nil
	case "trigger":
		if triggerDocument == nil {
			return "", errWorkflowRunNoTrigger
		}
		value, err := workflowRunSelectArgument(triggerDocument, placeholder.selector)
		if err != nil {
			return "", fmt.Errorf("select %q from the trigger batch: %w", placeholder.selector, err)
		}
		return value, nil
	}
	return "", fmt.Errorf("invalid workflow placeholder kind %q", placeholder.kind)
}

// workflowRunStepStdin resolves a step's stdin: the raw stdin_from output,
// its stdin_select values, or stdin_trigger values, one value per line. A
// selection with no values returns a skip reason instead of stdin.
func workflowRunStepStdin(step workflowRunStepSpec, results map[string]workflowRunStepResult, triggerDocument any) (*string, string, error) {
	if step.StdinTrigger != "" {
		if triggerDocument == nil {
			return nil, "", errWorkflowRunNoTrigger
		}
		lines, count, err := workflowRunSelectLines(triggerDocument, step.StdinTrigger)
		if err != nil {
			return nil, "", fmt.Errorf("stdin_trigger: select %q from the trigger batch: %w", step.StdinTrigger, err)
		}
		if count == 0 {
			return nil, "stdin_trigger selected no values", nil
		}
		return &lines, "", nil
	}
	if step.StdinFrom == "" {
		return nil, "", nil
	}
	output, err := workflowRunAvailableStepOutput(step.StdinFrom, results)
	if err != nil {
		return nil, "", err
	}
	if step.StdinSelect == "" {
		return &output, "", nil
	}
	document, err := decodeWorkflowRunJSON(output)
	var lines string
	var count int
	if err == nil {
		lines, count, err = workflowRunSelectLines(document, step.StdinSelect)
	}
	if err != nil {
		return nil, "", fmt.Errorf("stdin_select: select %q from step %q output: %w", step.StdinSelect, step.StdinFrom, err)
	}
	if count == 0 {
		return nil, "stdin_select selected no values", nil
	}
	return &lines, "", nil
}

// workflowRunTriggerEvent is one change event: the type ("upsert" or
// "delete"), the resource, and the key that tail emitted for it.
type workflowRunTriggerEvent struct {
	Event    string `json:"event"`
	Resource string `json:"resource"`
	Key      string `json:"key"`
}

// workflowRunTrigger is the read-only change batch that started a run. Steps
// read it through ${trigger.json:PATH} and stdin_trigger. It is checkpointed
// so --resume replays the same batch.
type workflowRunTrigger struct {
	Source   string                    `json:"source"`
	Resource string                    `json:"resource"`
	Events   []workflowRunTriggerEvent `json:"events"`
	// UpsertKeys lists each upserted key once, in event order. A key that is
	// also deleted in the batch is only in DeleteKeys.
	UpsertKeys []string `json:"upsert_keys"`
	DeleteKeys []string `json:"delete_keys"`
}

// newWorkflowRunTrigger builds a bounded trigger batch. It refuses a batch
// above workflowRunTriggerMaxEvents rather than truncate it, because a
// partial key list would silently leave changed records out.
func newWorkflowRunTrigger(source, resource string, events []workflowRunTriggerEvent) (*workflowRunTrigger, error) {
	if len(events) > workflowRunTriggerMaxEvents {
		return nil, fmt.Errorf("the batch has %d change events, above the %d-event limit for workflows that read trigger values", len(events), workflowRunTriggerMaxEvents)
	}
	trigger := &workflowRunTrigger{
		Source:     source,
		Resource:   resource,
		Events:     append(make([]workflowRunTriggerEvent, 0, len(events)), events...),
		UpsertKeys: []string{},
		DeleteKeys: []string{},
	}
	deleted := make(map[string]struct{})
	for _, event := range events {
		if event.Event != "delete" {
			continue
		}
		if _, seen := deleted[event.Key]; !seen {
			deleted[event.Key] = struct{}{}
			trigger.DeleteKeys = append(trigger.DeleteKeys, event.Key)
		}
	}
	upserted := make(map[string]struct{})
	for _, event := range events {
		if event.Event != "upsert" {
			continue
		}
		if _, gone := deleted[event.Key]; gone {
			continue
		}
		if _, seen := upserted[event.Key]; !seen {
			upserted[event.Key] = struct{}{}
			trigger.UpsertKeys = append(trigger.UpsertKeys, event.Key)
		}
	}
	return trigger, nil
}

// workflowRunTriggerDocument returns the batch as the generic JSON value the
// selectors read, or nil for a run without a batch.
func workflowRunTriggerDocument(trigger *workflowRunTrigger) (any, error) {
	if trigger == nil {
		return nil, nil
	}
	data, err := json.Marshal(trigger)
	if err != nil {
		return nil, fmt.Errorf("encode workflow trigger batch: %w", err)
	}
	return decodeWorkflowRunJSON(string(data))
}

// decodeWorkflowRunJSON parses step output that must be exactly one JSON
// value. Numbers stay json.Number so a selected key or ID keeps its text.
func decodeWorkflowRunJSON(text string) (any, error) {
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	var document any
	if err := decoder.Decode(&document); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("the output is empty, not JSON")
		}
		return nil, fmt.Errorf("the output is not valid JSON: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("the output has more content after its JSON value")
	}
	return document, nil
}

const (
	workflowRunSelectField = iota
	workflowRunSelectIndex
	workflowRunSelectEach
	workflowRunSelectFilter
)

type workflowRunSelectorStep struct {
	kind  int
	token string // source text, for errors
	name  string // object field, or the field a filter compares
	value string // filter value
	index int
}

// workflowRunSelector is a parsed JSON selector. The grammar is bounded:
//
//	PATH    = SEGMENT *("." SEGMENT)
//	SEGMENT = FIELD *("[" (INDEX | "" | FIELD "=" VALUE) "]")
//	FIELD   = 1*(ALPHA | DIGIT | "_" | "-")
//
// Only the first segment may omit FIELD, to index a top-level array. [N]
// takes element N, [] takes every element, and [field=value] keeps the
// object elements whose field is a string, number, or boolean equal to
// value. [] and [field=value] can select several values (fanOut).
type workflowRunSelector struct {
	steps  []workflowRunSelectorStep
	fanOut bool
}

func parseWorkflowRunSelector(text string) (workflowRunSelector, error) {
	var selector workflowRunSelector
	if text == "" {
		return selector, errors.New("JSON selector is empty")
	}
	if len(text) > workflowRunSelectorMaxLength {
		return selector, fmt.Errorf("JSON selector %q is longer than %d characters", text, workflowRunSelectorMaxLength)
	}
	for i := 0; i < len(text); {
		start := i
		for i < len(text) && workflowRunSelectorNameByte(text[i]) {
			i++
		}
		if i > start {
			selector.steps = append(selector.steps, workflowRunSelectorStep{kind: workflowRunSelectField, token: text[start:i], name: text[start:i]})
		} else if start != 0 || text[i] != '[' {
			return selector, fmt.Errorf("JSON selector %q needs a field name at offset %d", text, start)
		}
		for i < len(text) && text[i] == '[' {
			end := strings.IndexByte(text[i:], ']')
			if end == -1 {
				return selector, fmt.Errorf("JSON selector %q has an unclosed [", text)
			}
			step, err := parseWorkflowRunSelectorBracket(text[i : i+end+1])
			if err != nil {
				return selector, fmt.Errorf("JSON selector %q: %w", text, err)
			}
			if step.kind == workflowRunSelectEach || step.kind == workflowRunSelectFilter {
				selector.fanOut = true
			}
			selector.steps = append(selector.steps, step)
			i += end + 1
		}
		if i == len(text) {
			break
		}
		if text[i] != '.' {
			return selector, fmt.Errorf("JSON selector %q has an unexpected %q at offset %d", text, text[i], i)
		}
		i++
		if i == len(text) {
			return selector, fmt.Errorf("JSON selector %q ends with a dot", text)
		}
	}
	if len(selector.steps) > workflowRunSelectorMaxSteps {
		return selector, fmt.Errorf("JSON selector %q has more than %d steps", text, workflowRunSelectorMaxSteps)
	}
	return selector, nil
}

// parseWorkflowRunArgumentSelector parses a selector for one argument value,
// which must select exactly one value.
func parseWorkflowRunArgumentSelector(text string) (workflowRunSelector, error) {
	selector, err := parseWorkflowRunSelector(text)
	if err != nil {
		return selector, err
	}
	if selector.fanOut {
		return selector, fmt.Errorf("JSON selector %q can select several values, but an argument takes one; use [N], or pipe a list with stdin_select or stdin_trigger", text)
	}
	return selector, nil
}

func parseWorkflowRunSelectorBracket(token string) (workflowRunSelectorStep, error) {
	step := workflowRunSelectorStep{token: token}
	inner := token[1 : len(token)-1]
	if inner == "" {
		step.kind = workflowRunSelectEach
		return step, nil
	}
	if name, value, ok := strings.Cut(inner, "="); ok {
		if !workflowRunSelectorName(name) || value == "" || strings.ContainsAny(value, "[{}") {
			return step, fmt.Errorf("%s must be [field=value] with a field name and a non-empty value", token)
		}
		step.kind = workflowRunSelectFilter
		step.name = name
		step.value = value
		return step, nil
	}
	if len(inner) > 6 || !workflowRunSelectorDigits(inner) {
		return step, fmt.Errorf("%s must be [], [N] with N below 1000000, or [field=value]", token)
	}
	index, err := strconv.Atoi(inner)
	if err != nil {
		return step, fmt.Errorf("%s: %w", token, err)
	}
	step.kind = workflowRunSelectIndex
	step.index = index
	return step, nil
}

func workflowRunSelectorNameByte(c byte) bool {
	return c == '_' || c == '-' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func workflowRunSelectorName(text string) bool {
	if text == "" {
		return false
	}
	for i := range len(text) {
		if !workflowRunSelectorNameByte(text[i]) {
			return false
		}
	}
	return true
}

func workflowRunSelectorDigits(text string) bool {
	for i := range len(text) {
		if text[i] < '0' || text[i] > '9' {
			return false
		}
	}
	return text != ""
}

// selectFrom applies the selector to a decoded JSON value. A missing field,
// an out-of-range index, or a value of the wrong JSON type fails closed; only
// a filter that matches no element yields an empty result.
func (s workflowRunSelector) selectFrom(document any) ([]any, error) {
	current := []any{document}
	for _, step := range s.steps {
		next := make([]any, 0, len(current))
		for _, value := range current {
			if step.kind == workflowRunSelectField {
				object, ok := value.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("field %q needs an object, found %s", step.name, workflowRunJSONKind(value))
				}
				member, ok := object[step.name]
				if !ok {
					return nil, fmt.Errorf("field %q is missing", step.name)
				}
				next = append(next, member)
				continue
			}
			array, ok := value.([]any)
			if !ok {
				return nil, fmt.Errorf("%s needs an array, found %s", step.token, workflowRunJSONKind(value))
			}
			switch step.kind {
			case workflowRunSelectIndex:
				if step.index >= len(array) {
					return nil, fmt.Errorf("%s is out of range: the array has %d element(s)", step.token, len(array))
				}
				next = append(next, array[step.index])
			case workflowRunSelectEach:
				next = append(next, array...)
			case workflowRunSelectFilter:
				for _, element := range array {
					object, ok := element.(map[string]any)
					if !ok {
						return nil, fmt.Errorf("%s needs an array of objects, found an element that is %s", step.token, workflowRunJSONKind(element))
					}
					if text, ok := workflowRunSelectorScalarText(object[step.name]); ok && text == step.value {
						next = append(next, element)
					}
				}
			}
		}
		current = next
	}
	return current, nil
}

// workflowRunSelectArgument selects the one string, number, or boolean an
// argument value takes. Command and flag identity are checked after
// substitution (validateWorkflowRunResolvedStepArgs), so a selected value
// can fill a value but never choose a command or a flag.
func workflowRunSelectArgument(document any, selectorText string) (string, error) {
	selector, err := parseWorkflowRunArgumentSelector(selectorText)
	if err != nil {
		return "", err
	}
	values, err := selector.selectFrom(document)
	if err != nil {
		return "", err
	}
	text, ok := workflowRunSelectorScalarText(values[0])
	if !ok {
		return "", fmt.Errorf("selected %s, but an argument takes a string, number, or boolean", workflowRunJSONKind(values[0]))
	}
	return text, nil
}

// workflowRunSelectLines renders the selected values one per line for stdin.
// A selected array contributes its elements. Every line value must be a
// string, number, or boolean without a line break.
func workflowRunSelectLines(document any, selectorText string) (string, int, error) {
	selector, err := parseWorkflowRunSelector(selectorText)
	if err != nil {
		return "", 0, err
	}
	values, err := selector.selectFrom(document)
	if err != nil {
		return "", 0, err
	}
	var lines strings.Builder
	count := 0
	addLine := func(value any) error {
		text, ok := workflowRunSelectorScalarText(value)
		if !ok {
			return fmt.Errorf("selected %s, but stdin takes strings, numbers, or booleans, one per line", workflowRunJSONKind(value))
		}
		if strings.ContainsAny(text, "\r\n") {
			return errors.New("selected a value with a line break, but stdin takes one value per line")
		}
		lines.WriteString(text)
		lines.WriteByte('\n')
		count++
		return nil
	}
	for _, value := range values {
		if elements, ok := value.([]any); ok {
			for _, element := range elements {
				if err := addLine(element); err != nil {
					return "", 0, err
				}
			}
			continue
		}
		if err := addLine(value); err != nil {
			return "", 0, err
		}
	}
	return lines.String(), count, nil
}

func workflowRunSelectorScalarText(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, true
	case json.Number:
		return typed.String(), true
	case bool:
		return strconv.FormatBool(typed), true
	}
	return "", false
}

func workflowRunJSONKind(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "an object"
	case []any:
		return "an array"
	case string:
		return "a string"
	case json.Number:
		return "a number"
	case bool:
		return "a boolean"
	}
	return fmt.Sprintf("%T", value)
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
	litCmd, _, _ := root.Find(workflowRunStepPositionalsWithRoot(root, literalArgs))
	resCmd, _, _ := root.Find(workflowRunStepPositionalsWithRoot(root, resolvedArgs))
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

func executeWorkflowRunStepWithRoot(ctx context.Context, root *cobra.Command, args []string, stdin *string) (string, string, error) {
	root.SetArgs(args)
	if stdin != nil {
		root.SetIn(strings.NewReader(*stdin))
	}
	var stdout ceilingBuffer
	var stderr ceilingBuffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)

	restore := snapshotCLIGlobals()
	defer restore()
	// ExecuteRootInProcess, not ExecuteContext: a step's args are Cobra args and
	// may carry the global --deliver, whose spool this process would otherwise
	// keep open and never unlink.
	err := ExecuteRootInProcess(ctx, root)

	// A command that ignores its write errors would otherwise finish with
	// silently truncated output, and truncated output here is not cosmetic:
	// it feeds ${steps.<name>.output}, StdinFrom, and the resume checkpoint.
	// Failing the step is the only honest outcome. Do not materialize the
	// retained prefix: it has already exceeded the workflow's contract and a
	// failed step's output is unavailable to downstream dataflow.
	if stdout.overflowed || stderr.overflowed {
		return "", "", errWorkflowStepOutputTooLarge()
	}
	return stdout.String(), stderr.String(), err
}

// workflowRunStepOutputCeiling is a tripwire, not a budget. Step output is a
// contract -- substitution, piping, and resume all read the full text, so it
// cannot be capped the way a display value can (workflowRunOutputLimit trims
// only what the report renders). What it can do is refuse to grow without
// bound: past this point the run was heading for an OOM, and a named error
// beats being killed by the allocator. A var so tests can trip it without
// allocating a quarter gigabyte.
var workflowRunStepOutputCeiling = 256 << 20

func errWorkflowStepOutputTooLarge() error {
	return fmt.Errorf(
		"workflow step produced more than %d MiB of output; step output is piped and checkpointed in full, so narrow the step (--limit, --select, --compact) or write to a file instead",
		workflowRunStepOutputCeiling>>20)
}

type ceilingBuffer struct {
	buf        bytes.Buffer
	overflowed bool
}

func (b *ceilingBuffer) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > workflowRunStepOutputCeiling {
		b.overflowed = true
		return 0, errWorkflowStepOutputTooLarge()
	}
	return b.buf.Write(p)
}

func (b *ceilingBuffer) String() string { return b.buf.String() }

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
