// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cobratree

import (
	"context"
	"fmt"
	"strings"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// RegisterAll walks the user-facing Cobra commands and registers in-process
// MCP tools for commands that are not already covered by typed endpoint tools.
// Takes a factory that builds a fresh command tree, so each tool invocation can
// execute against its own single-use cobra.Command instead of shelling out to a
// companion binary.
func RegisterAll(s *server.MCPServer, rootFactory func() *cobra.Command) {
	if rootFactory == nil {
		return
	}
	root := rootFactory()
	if root == nil {
		return
	}
	walk(root, nil, func(cmd *cobra.Command, path []string) {
		switch classify(cmd) {
		case commandHidden:
			return
		case commandEndpoint, commandFramework:
			return
		}
		if !cmd.Runnable() {
			return
		}

		toolName := toolNameForPath(path)
		if toolName == "" {
			return
		}
		allowedFlags := safeFlagNames(cmd)
		options := []mcplib.ToolOption{mcplib.WithDescription(descriptionFor(cmd))}
		options = append(options, safeToolOptionsForFlags(cmd)...)
		if commandTakesArgs(cmd) {
			// Keep positional arguments for mirrored commands, but do not advertise this
			// as a raw flag escape hatch.
			options = append(options, mcplib.WithString("args", mcplib.Description("Additional positional arguments to append to the command. Raw CLI flags are rejected.")))
		}
		if isMCPReadOnly(cmd) {
			options = append(options, mcplib.WithReadOnlyHintAnnotation(true), mcplib.WithDestructiveHintAnnotation(false))
		}
		s.AddTool(mcplib.NewTool(toolName, options...), safeInProcessHandler(rootFactory, path, allowedFlags))
	})
}

func walk(cmd *cobra.Command, path []string, visit func(*cobra.Command, []string)) {
	for _, child := range cmd.Commands() {
		if child.Hidden || isMCPHidden(child) {
			continue
		}
		childPath := append(append([]string{}, path...), child.Name())
		visit(child, childPath)
		if kind := classify(child); kind != commandHidden && kind != commandFramework {
			walk(child, childPath, visit)
		}
	}
}

func descriptionFor(cmd *cobra.Command) string {
	if cmd.Long != "" {
		return cmd.Long
	}
	if cmd.Short != "" {
		return cmd.Short
	}
	return "Run `" + cmd.CommandPath() + "` through the companion CLI binary."
}

// Command-mirror tools run inside an MCP host, so global
// delivery/configuration escape hatches must not be exposed as tool parameters.
// Hosts should configure those out-of-band or use typed MCP tools.
var unsafeMCPMirrorFlags = map[string]struct{}{
	"config":  {},
	"deliver": {},
	"group":   {},
	"profile": {},
}

// Mirror only command-local (non-inherited) flags. Root/global persistent
// flags (--agent, --json, output formatting,
// confirmation, ops knobs) were re-declared on every one of ~68 mirror tools,
// costing ~80% of the cobratree token surface for zero agent value (the MCP
// layer injects --agent out-of-band; see runMirroredInProcess). One shared
// enumerator drives BOTH schema exposure and the validation allowlist so they
// can never diverge, preserving the argument-safety guard.
//
// Dropping all inherited globals also dropped the write-safety gate flags. But
// --agent injection does NOT imply --yes (root.go:
// "does NOT auto-apply writes"), and the apply gate is `Yes && !DryRun`
// (mutation.ResolveMode), so stripping them made every cobra-only mutation
// workflow (items enrich, duplicates resolve, tags audit fix, …) preview-only
// over MCP — a regression vs. the pre-F-plain surface. This is the F-surgical
// design (Oracle's original pick): the write-safety gate flags stay reachable
// for MUTATING commands only (read-only commands never need them). On the
// default facade surface these add zero standing tokens (they appear only in
// on-demand command_search detail), and the single-enumerator invariant keeps
// validation==exposure intact.
var writeGatingMCPFlags = []string{
	"yes",
	"dry-run",
	"allow-destructive",
	"max-changes",
	"continue-on-error",
	"max-failures",
}

func visitSafeMirrorFlags(cmd *cobra.Command, visit func(*pflag.Flag)) {
	seen := map[string]struct{}{}
	emit := func(flag *pflag.Flag) {
		if flag == nil || flag.Hidden || flag.Deprecated != "" {
			return
		}
		if _, unsafe := unsafeMCPMirrorFlags[flag.Name]; unsafe {
			return
		}
		if _, ok := seen[flag.Name]; ok {
			return
		}
		seen[flag.Name] = struct{}{}
		visit(flag)
	}
	cmd.NonInheritedFlags().VisitAll(emit)
	// Keep the write-safety gate flags reachable for mutating commands so applies
	// remain possible through the MCP surface.
	if !isMCPReadOnly(cmd) {
		for _, name := range writeGatingMCPFlags {
			if flag := cmd.InheritedFlags().Lookup(name); flag != nil {
				emit(flag)
			}
		}
	}
}

func safeFlagNames(cmd *cobra.Command) map[string]struct{} {
	names := map[string]struct{}{}
	visitSafeMirrorFlags(cmd, func(flag *pflag.Flag) {
		names[flag.Name] = struct{}{}
	})
	return names
}

func safeToolOptionsForFlags(cmd *cobra.Command) []mcplib.ToolOption {
	var opts []mcplib.ToolOption
	visitSafeMirrorFlags(cmd, func(flag *pflag.Flag) {
		opts = append(opts, toolOptionForFlag(flag))
	})
	return opts
}

// Validate MCP-supplied arguments before the generated in-process handler
// converts them back to Cobra CLI flags.
func safeInProcessHandler(rootFactory func() *cobra.Command, commandPath []string, allowedFlags map[string]struct{}) server.ToolHandlerFunc {
	inner := inProcessHandler(rootFactory, commandPath)
	return func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		if err := validateMirrorArguments(req.GetArguments(), allowedFlags); err != nil {
			return mcplib.NewToolResultError(err.Error()), nil
		}
		return inner(ctx, req)
	}
}

// Reject hidden globals, unknown schema parameters, and raw flag tokens in
// positional args.
func validateMirrorArguments(args map[string]any, allowedFlags map[string]struct{}) error {
	for name := range unsafeMCPMirrorFlags {
		if _, ok := args[name]; ok {
			return fmt.Errorf("MCP command mirror does not expose --%s; use typed MCP tools or server configuration instead", name)
		}
	}
	for name := range args {
		if name == "args" {
			continue
		}
		if _, ok := allowedFlags[name]; !ok {
			return fmt.Errorf("MCP command mirror does not expose --%s for this command", name)
		}
	}
	raw, _ := args["args"].(string)
	for _, token := range splitShellArgs(raw) {
		if strings.HasPrefix(token, "-") {
			return fmt.Errorf("MCP command mirror args accepts positional arguments only; raw flag %q is not allowed", token)
		}
	}
	return nil
}
