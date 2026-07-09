// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cobratree

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"unicode/utf8"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"zotio/internal/cli"
)

// Command-orchestration facade: search+run over the Cobra tree, an alternative
// to RegisterAll selected by ZOTIO_MCP_SURFACE.
func RegisterOrchestration(s *server.MCPServer, rootFactory func() *cobra.Command) {
	s.AddTool(mcplib.NewTool("command_search",
		mcplib.WithDescription("Search and inspect mirrorable Cobra commands exposed through the command orchestration facade."),
		mcplib.WithString("query", mcplib.Description("Case-insensitive text to match against command names and summaries.")),
		mcplib.WithString("name", mcplib.Description("Exact space-separated command path to inspect, such as \"items enrich\".")),
		mcplib.WithReadOnlyHintAnnotation(true),
		mcplib.WithDestructiveHintAnnotation(false),
	), commandSearchHandler(rootFactory))

	s.AddTool(mcplib.NewTool("command_run",
		mcplib.WithDescription("Run one mirrorable Cobra command by its space-separated command path."),
		mcplib.WithString("name", mcplib.Required(), mcplib.Description("Exact space-separated command path to run, such as \"items enrich\".")),
		mcplib.WithObject("flags", mcplib.Description("Safe flags to pass by name: command-local flags plus, for mutating commands, the write-safety gate flags (yes, dry-run, allow-destructive, max-changes, continue-on-error, max-failures) — pass {\"yes\": true} to apply a write. Inspect available flags via command_search."), mcplib.AdditionalProperties(true)),
		mcplib.WithString("args", mcplib.Description("Additional positional arguments only; raw flags rejected.")),
		mcplib.WithDestructiveHintAnnotation(true),
	), commandRunHandler(rootFactory))
}

func commandSearchHandler(rootFactory func() *cobra.Command) server.ToolHandlerFunc {
	return func(_ context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		args := req.GetArguments()
		if name, ok := args["name"].(string); ok && name != "" {
			cmd, _, ok := findMirrorableCommand(rootFactory, name)
			if !ok {
				return mcplib.NewToolResultError("mirrorable command not found: " + name), nil
			}
			operation, requires, destructive := orchestrationCapability(cmd, name)
			out := orchestrationCommandDetail{
				Name:        name,
				Description: descriptionFor(cmd),
				TakesArgs:   commandTakesArgs(cmd),
				Operation:   operation,
				Requires:    requires,
				Destructive: destructive,
				Flags:       []orchestrationFlagDetail{},
			}
			visitSafeMirrorFlags(cmd, func(flag *pflag.Flag) {
				out.Flags = append(out.Flags, orchestrationFlagDetail{
					Name:        flag.Name,
					Type:        flag.Value.Type(),
					Required:    isRequired(flag),
					Description: flagDescription(flag),
				})
			})
			buf, err := json.Marshal(out)
			if err != nil {
				return mcplib.NewToolResultError(err.Error()), nil
			}
			return mcplib.NewToolResultText(string(buf)), nil
		}

		query, _ := args["query"].(string)
		needle := strings.ToLower(query)
		commands := listMirrorableCommands(rootFactory)
		out := make([]orchestrationCommandSummary, 0, len(commands))
		for _, command := range commands {
			if needle != "" && !strings.Contains(strings.ToLower(command.Name+" "+command.Summary), needle) {
				continue
			}
			out = append(out, command)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		buf, err := json.Marshal(out)
		if err != nil {
			return mcplib.NewToolResultError(err.Error()), nil
		}
		return mcplib.NewToolResultText(string(buf)), nil
	}
}

func commandRunHandler(rootFactory func() *cobra.Command) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		args := req.GetArguments()
		name, _ := args["name"].(string)
		if name == "" {
			return mcplib.NewToolResultError("command_run requires name"), nil
		}
		cmd, path, ok := findMirrorableCommand(rootFactory, name)
		if !ok {
			return mcplib.NewToolResultError("mirrorable command not found: " + name), nil
		}

		execArgs := map[string]any{}
		if rawFlags, ok := args["flags"]; ok && rawFlags != nil {
			flags, ok := rawFlags.(map[string]any)
			if !ok {
				return mcplib.NewToolResultError("command_run flags must be an object"), nil
			}
			for key, value := range flags {
				execArgs[key] = value
			}
		}
		if rawArgs, ok := args["args"].(string); ok && rawArgs != "" {
			execArgs["args"] = rawArgs
		}

		allowed := safeFlagNames(cmd)
		if err := validateMirrorArguments(execArgs, allowed); err != nil {
			return mcplib.NewToolResultError(err.Error()), nil
		}
		return runMirroredInProcess(ctx, rootFactory, path, execArgs), nil
	}
}

type orchestrationCommandSummary struct {
	Name        string   `json:"name"`
	Summary     string   `json:"summary"`
	Operation   string   `json:"operation"`
	Requires    []string `json:"requires,omitempty"`
	Destructive bool     `json:"destructive"`
}

type orchestrationCommandDetail struct {
	Name        string                    `json:"name"`
	Description string                    `json:"description"`
	TakesArgs   bool                      `json:"takesArgs"`
	Operation   string                    `json:"operation"`
	Requires    []string                  `json:"requires,omitempty"`
	Destructive bool                      `json:"destructive"`
	Flags       []orchestrationFlagDetail `json:"flags"`
}

type orchestrationFlagDetail struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Required    bool   `json:"required"`
	Description string `json:"description"`
}

func listMirrorableCommands(rootFactory func() *cobra.Command) []orchestrationCommandSummary {
	root := orchestrationRoot(rootFactory)
	if root == nil {
		return nil
	}
	var out []orchestrationCommandSummary
	walk(root, nil, func(cmd *cobra.Command, path []string) {
		if !isMirrorableCommand(cmd) {
			return
		}
		joined := strings.Join(path, " ")
		operation, requires, destructive := orchestrationCapability(cmd, joined)
		out = append(out, orchestrationCommandSummary{
			Name:        joined,
			Summary:     firstLine(descriptionFor(cmd)),
			Operation:   operation,
			Requires:    requires,
			Destructive: destructive,
		})
	})
	return out
}

// orchestrationCapability derives the command's operation kind, declared
// preconditions, and destructiveness the same way the capability registry does:
// operation comes from the mcp:read-only annotation unless a registry override
// names one explicitly; requires/destructive come from the override. Keeping
// this in lockstep with cli.buildCapabilityRegistry keeps the facade honest.
func orchestrationCapability(cmd *cobra.Command, path string) (operation string, requires []string, destructive bool) {
	operation = "other"
	if isMCPReadOnly(cmd) {
		operation = "read"
	}
	if ov, reqs, d, ok := cli.CommandOverrideCapability(path); ok {
		if ov != "" {
			operation = ov
		}
		requires = reqs
		destructive = d
	}
	return operation, requires, destructive
}

func findMirrorableCommand(rootFactory func() *cobra.Command, name string) (*cobra.Command, []string, bool) {
	root := orchestrationRoot(rootFactory)
	if root == nil {
		return nil, nil, false
	}
	var found *cobra.Command
	var foundPath []string
	walk(root, nil, func(cmd *cobra.Command, path []string) {
		if found != nil || !isMirrorableCommand(cmd) || strings.Join(path, " ") != name {
			return
		}
		found = cmd
		foundPath = append([]string{}, path...)
	})
	return found, foundPath, found != nil
}

func orchestrationRoot(rootFactory func() *cobra.Command) *cobra.Command {
	if rootFactory == nil {
		return nil
	}
	return rootFactory()
}

func isMirrorableCommand(cmd *cobra.Command) bool {
	if cmd == nil || !cmd.Runnable() {
		return false
	}
	switch classify(cmd) {
	case commandHidden, commandEndpoint, commandFramework:
		return false
	default:
		return true
	}
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	line = strings.TrimSpace(line)
	if utf8.RuneCountInString(line) <= 120 {
		return line
	}
	runes := []rune(line)
	return string(runes[:120])
}
