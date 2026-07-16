// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cobratree

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"
)

type workflowSubmitSpec struct {
	Steps           []workflowSubmitStep `json:"steps"`
	Vars            map[string]string    `json:"vars,omitempty"`
	ContinueOnError bool                 `json:"continue_on_error"`
}

type workflowSubmitStep struct {
	Name      string                  `json:"name,omitempty"`
	Args      []string                `json:"args"`
	StdinFrom string                  `json:"stdin_from,omitempty"`
	When      *workflowSubmitStepWhen `json:"when,omitempty"`
}

type workflowSubmitStepWhen struct {
	Step string `json:"step"`
	Is   string `json:"is"`
}

// RegisterWorkflowSubmit adds the validated inline workflow surface shared by
// both MCP command surfaces. Local workflow files remain hidden because their
// nested argv has no equivalent per-command safety guard.
func RegisterWorkflowSubmit(s *server.MCPServer, rootFactory func() *cobra.Command) {
	s.AddTool(mcplib.NewTool("workflow_submit",
		mcplib.WithDescription("Submit an inline multi-step workflow executed by zotio's transactional runner — previews unless yes; one approval, one journal run id; steps validated per-command exactly like command_run."),
		mcplib.WithBoolean("yes", mcplib.Description("Apply the full workflow once instead of previewing it.")),
		mcplib.WithBoolean("continue_on_error", mcplib.Description("Continue executing later steps after a step fails.")),
		mcplib.WithObject("vars", mcplib.Description("Workflow variable values."), mcplib.AdditionalProperties(map[string]any{"type": "string"})),
		mcplib.WithArray("steps", mcplib.Required(), mcplib.Description("Validated workflow steps."), mcplib.Items(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command":    map[string]any{"type": "string", "description": "Mirrorable command path, such as items enrich."},
				"flags":      map[string]any{"type": "object", "description": "Safe command flags by name.", "additionalProperties": true},
				"args":       map[string]any{"type": "string", "description": "Positional arguments only."},
				"name":       map[string]any{"type": "string", "description": "Optional step name."},
				"stdin_from": map[string]any{"type": "string", "description": "Name of an earlier step whose output becomes stdin."},
				"when": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"step": map[string]any{"type": "string"},
						"is":   map[string]any{"type": "string"},
					},
					"required":             []string{"step", "is"},
					"additionalProperties": false,
				},
			},
			"required":             []string{"command"},
			"additionalProperties": false,
		})),
		mcplib.WithDestructiveHintAnnotation(true),
	), workflowSubmitHandler(rootFactory))
}

func workflowSubmitHandler(rootFactory func() *cobra.Command) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		args := req.GetArguments()
		yes, err := workflowSubmitBool(args, "yes")
		if err != nil {
			return mcplib.NewToolResultError(err.Error()), nil
		}
		continueOnError, err := workflowSubmitBool(args, "continue_on_error")
		if err != nil {
			return mcplib.NewToolResultError(err.Error()), nil
		}
		vars, err := workflowSubmitVars(args["vars"])
		if err != nil {
			return mcplib.NewToolResultError(err.Error()), nil
		}
		steps, err := workflowSubmitSteps(rootFactory, args["steps"])
		if err != nil {
			return mcplib.NewToolResultError(err.Error()), nil
		}

		spec := workflowSubmitSpec{
			Steps:           steps,
			Vars:            vars,
			ContinueOnError: continueOnError,
		}
		file, err := os.CreateTemp("", "zotio-workflow-*.json")
		if err != nil {
			return mcplib.NewToolResultError(fmt.Sprintf("create workflow spec: %v", err)), nil
		}
		specPath := file.Name()
		defer func() {
			_ = os.Remove(specPath)
			_ = os.Remove(specPath + ".checkpoint.json")
		}()
		if err := json.NewEncoder(file).Encode(spec); err != nil {
			_ = file.Close()
			return mcplib.NewToolResultError(fmt.Sprintf("write workflow spec: %v", err)), nil
		}
		if err := file.Close(); err != nil {
			return mcplib.NewToolResultError(fmt.Sprintf("close workflow spec: %v", err)), nil
		}

		execArgs := map[string]any{
			"args":     specPath,
			"agent":    true,
			"no-input": true,
		}
		if yes {
			execArgs["yes"] = true
		}
		return runMirroredInProcess(ctx, rootFactory, []string{"workflow", "run"}, execArgs), nil
	}
}

func workflowSubmitSteps(rootFactory func() *cobra.Command, raw any) ([]workflowSubmitStep, error) {
	rawSteps, ok := raw.([]any)
	if !ok || len(rawSteps) == 0 {
		return nil, fmt.Errorf("workflow_submit requires a non-empty steps array")
	}
	steps := make([]workflowSubmitStep, 0, len(rawSteps))
	for index, rawStep := range rawSteps {
		stepArgs, ok := rawStep.(map[string]any)
		if !ok {
			return nil, workflowSubmitStepError(index, "must be an object")
		}
		command, ok := stepArgs["command"].(string)
		if !ok || command == "" {
			return nil, workflowSubmitStepError(index, "requires command")
		}
		cmd, path, ok := findMirrorableCommand(rootFactory, command)
		if !ok {
			return nil, workflowSubmitStepError(index, "mirrorable command not found: "+command)
		}

		execArgs := map[string]any{}
		if rawFlags, ok := stepArgs["flags"]; ok && rawFlags != nil {
			flags, ok := rawFlags.(map[string]any)
			if !ok {
				return nil, workflowSubmitStepError(index, "flags must be an object")
			}
			for key, value := range flags {
				execArgs[key] = value
			}
		}
		if rawArgs, exists := stepArgs["args"]; exists && rawArgs != nil {
			positionalArgs, ok := rawArgs.(string)
			if !ok {
				return nil, workflowSubmitStepError(index, "args must be a string")
			}
			if positionalArgs != "" {
				execArgs["args"] = positionalArgs
			}
		}

		allowed := safeFlagNames(cmd)
		if err := validateMirrorArguments(execArgs, allowed); err != nil {
			return nil, workflowSubmitStepError(index, err.Error())
		}

		name, err := workflowSubmitOptionalString(stepArgs, "name")
		if err != nil {
			return nil, workflowSubmitStepError(index, err.Error())
		}
		stdinFrom, err := workflowSubmitOptionalString(stepArgs, "stdin_from")
		if err != nil {
			return nil, workflowSubmitStepError(index, err.Error())
		}
		when, err := workflowSubmitWhen(stepArgs["when"])
		if err != nil {
			return nil, workflowSubmitStepError(index, err.Error())
		}

		positionalPath := append([]string{}, path...)
		argv := append([]string{}, path...)
		argv = append(argv, cliArgsFromMCP(execArgs)...)
		if positionalArgs, _ := execArgs["args"].(string); positionalArgs != "" {
			positionals := splitShellArgs(positionalArgs)
			positionalPath = append(positionalPath, positionals...)
			argv = append(argv, positionals...)
		}
		if !workflowSubmitBindsCommand(rootFactory, path, positionalPath) {
			return nil, workflowSubmitStepError(index, "positional args resolve to a different or hidden command")
		}

		steps = append(steps, workflowSubmitStep{
			Name:      name,
			Args:      argv,
			StdinFrom: stdinFrom,
			When:      when,
		})
	}
	return steps, nil
}

// workflowSubmitBindsCommand prevents positional arguments from descending
// into a different command than the mirrorable command initially validated.
func workflowSubmitBindsCommand(rootFactory func() *cobra.Command, expectedPath, positionalPath []string) bool {
	if rootFactory == nil {
		return false
	}
	root := orchestrationRoot(rootFactory)
	if root == nil {
		return false
	}
	resolved, _, err := root.Find(positionalPath)
	if err != nil || !isMirrorableCommand(resolved) {
		return false
	}
	expected, _, ok := findMirrorableCommand(func() *cobra.Command { return root }, strings.Join(expectedPath, " "))
	return ok && resolved == expected
}

func workflowSubmitVars(raw any) (map[string]string, error) {
	if raw == nil {
		return nil, nil
	}
	values, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("workflow_submit vars must be an object of string values")
	}
	vars := make(map[string]string, len(values))
	for name, rawValue := range values {
		value, ok := rawValue.(string)
		if !ok {
			return nil, fmt.Errorf("workflow_submit vars.%s must be a string", name)
		}
		vars[name] = value
	}
	return vars, nil
}

func workflowSubmitBool(args map[string]any, name string) (bool, error) {
	raw, exists := args[name]
	if !exists || raw == nil {
		return false, nil
	}
	value, ok := raw.(bool)
	if !ok {
		return false, fmt.Errorf("workflow_submit %s must be a boolean", name)
	}
	return value, nil
}

func workflowSubmitOptionalString(args map[string]any, name string) (string, error) {
	raw, exists := args[name]
	if !exists || raw == nil {
		return "", nil
	}
	value, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", name)
	}
	return value, nil
}

func workflowSubmitWhen(raw any) (*workflowSubmitStepWhen, error) {
	if raw == nil {
		return nil, nil
	}
	values, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("when must be an object")
	}
	step, ok := values["step"].(string)
	if !ok {
		return nil, fmt.Errorf("when.step must be a string")
	}
	is, ok := values["is"].(string)
	if !ok {
		return nil, fmt.Errorf("when.is must be a string")
	}
	return &workflowSubmitStepWhen{Step: step, Is: is}, nil
}

func workflowSubmitStepError(index int, message string) error {
	return fmt.Errorf("workflow_submit step %d: %s", index+1, message)
}
