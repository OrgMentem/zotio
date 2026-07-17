// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cobratree

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"
)

var mirroredCommandMu sync.Mutex

// inProcessHandler runs a mirrored Cobra command in-process via the shared
// runMirroredInProcess core, so the MCP server works without a companion zotio
// binary on PATH.
func inProcessHandler(rootFactory func() *cobra.Command, commandPath []string) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		return runMirroredInProcess(ctx, rootFactory, commandPath, req.GetArguments()), nil
	}
}

// runMirroredInProcess builds a fresh command tree (cobra.Command state is
// single-use) and runs the mirrored command in-process. Inject --agent (when
// the root defines it) so mirror tools always return
// structured, non-interactive output regardless of which flags the MCP schema
// exposes. This is the out-of-band mechanism that lets the schema drop
// --agent/--json and the other global formatting/confirmation flags. Shared by
// the command mirror (inProcessHandler) and the orchestration facade (command_run).
func runMirroredInProcess(ctx context.Context, rootFactory func() *cobra.Command, commandPath []string, args map[string]any) *mcplib.CallToolResult {
	// CLI package state (notably the group-selected local DB/API prefix) is still
	// process-global. Serialize the
	// in-process mirror so concurrent HTTP MCP requests cannot cross-contaminate
	// library scope while commands run.
	mirroredCommandMu.Lock()
	defer mirroredCommandMu.Unlock()
	root := rootFactory()
	if root == nil {
		return mcplib.NewToolResultError("failed to build command tree")
	}
	finalArgs := append([]string{}, commandPath...)
	if root.PersistentFlags().Lookup("agent") != nil || root.Flags().Lookup("agent") != nil {
		finalArgs = append(finalArgs, "--agent")
	}
	finalArgs = append(finalArgs, cliArgsFromMCP(args)...)
	if raw, _ := args["args"].(string); strings.TrimSpace(raw) != "" {
		finalArgs = append(finalArgs, splitShellArgs(raw)...)
	}
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(finalArgs)
	if err := root.ExecuteContext(ctx); err != nil {
		return mcplib.NewToolResultError(buf.String() + "\n" + err.Error())
	}
	return mcplib.NewToolResultText(buf.String())
}

func cliArgsFromMCP(args map[string]any) []string {
	keys := make([]string, 0, len(args))
	for k := range args {
		if k != "args" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	var out []string
	for _, k := range keys {
		v := args[k]
		switch tv := v.(type) {
		case bool:
			if tv {
				out = append(out, "--"+k)
			} else {
				out = append(out, "--"+k+"=false")
			}
		case float64:
			out = append(out, "--"+k, strconv.FormatFloat(tv, 'f', -1, 64))
		case string:
			if tv != "" {
				out = append(out, "--"+k, tv)
			}
		case []any:
			for _, item := range tv {
				out = append(out, "--"+k, fmt.Sprintf("%v", item))
			}
		default:
			if v != nil {
				out = append(out, "--"+k, fmt.Sprintf("%v", v))
			}
		}
	}
	return out
}

// splitShellArgs whitespace-splits with shell-safe double- and single-quoted
// token preservation and backslash escapes.
func splitShellArgs(s string) []string {
	var tokens []string
	var cur []rune
	inSingleQuote := false
	inDoubleQuote := false
	escaped := false
	hasToken := false

	for _, r := range s {
		switch {
		case escaped:
			cur = append(cur, r)
			hasToken = true
			escaped = false
		case r == '\\' && !inSingleQuote:
			escaped = true
			hasToken = true
		case r == '\'' && !inDoubleQuote:
			inSingleQuote = !inSingleQuote
			hasToken = true
		case r == '"' && !inSingleQuote:
			inDoubleQuote = !inDoubleQuote
			hasToken = true
		case (r == ' ' || r == '\t') && !inSingleQuote && !inDoubleQuote:
			if hasToken {
				tokens = append(tokens, string(cur))
				cur = cur[:0]
				hasToken = false
			}
		default:
			cur = append(cur, r)
			hasToken = true
		}
	}
	if escaped {
		cur = append(cur, '\\')
	}
	if hasToken {
		tokens = append(tokens, string(cur))
	}
	return tokens
}
