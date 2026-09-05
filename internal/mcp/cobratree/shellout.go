// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cobratree

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"

	"zotio/internal/cli"
	"zotio/internal/mcp/bound"
)

// mirroredCommandSlot serializes in-process mirrored command execution. It is
// a one-deep channel rather than a sync.Mutex because acquisition must honor
// context cancellation, and the only way to wait on a mutex is to block a
// goroutine. The mutex version parked one goroutine per waiter plus a second
// one per cancelled waiter to absorb the eventual lock hand-off, so a burst of
// MCP callers that time out while a long import held the lock leaked two
// goroutines each until the holder finished.
var mirroredCommandSlot = make(chan struct{}, 1)

// acquireMirroredSlot takes the mirrored-command slot, honoring context
// cancellation. Cancellation is O(1) and spawns nothing: the caller simply
// stops selecting on the send.
func acquireMirroredSlot(ctx context.Context) error {
	if ctx == nil {
		mirroredCommandSlot <- struct{}{}
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case mirroredCommandSlot <- struct{}{}:
		// A select with two ready cases picks at random, so re-check: a caller
		// cancelled while waiting must not proceed to run the command.
		if err := ctx.Err(); err != nil {
			releaseMirroredSlot()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func releaseMirroredSlot() {
	<-mirroredCommandSlot
}

const maxMirroredErrorBytes = 4096

// StateGuard snapshots process-global CLI state before mirrored command
// execution and returns a function that restores it. The MCP package sets it
// to avoid an import cycle with the CLI package.
var StateGuard func() (restore func())

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
func runMirroredInProcess(ctx context.Context, rootFactory func() *cobra.Command, commandPath []string, args map[string]any) (result *mcplib.CallToolResult) {
	// CLI package state (notably the group-selected local DB/API prefix) is still
	// process-global. Serialize the
	// in-process mirror so concurrent HTTP MCP requests cannot cross-contaminate
	// library scope while commands run.
	if err := acquireMirroredSlot(ctx); err != nil {
		return mcplib.NewToolResultError(err.Error())
	}
	defer releaseMirroredSlot()
	if StateGuard != nil {
		restore := StateGuard()
		defer restore()
	}

	var buf boundedCapture
	defer func() {
		if panicValue := recover(); panicValue != nil {
			result = mcplib.NewToolResultError(mirroredErrorText(&buf, fmt.Sprintf("panic: %v", panicValue)))
		}
	}()

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
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(finalArgs)
	// cli.ExecuteRootInProcess, not root.ExecuteContext: mirrored args are
	// arbitrary and may include the global --deliver, whose spool is opened
	// during flag handling and closed only by that wrapper. A bare Cobra tree
	// (the tests' echo root) allocates nothing, so the wrapper is a plain
	// execution for it.
	if err := cli.ExecuteRootInProcess(ctx, root); err != nil {
		return mcplib.NewToolResultError(mirroredErrorText(&buf, err.Error()))
	}
	// The capture retains only the transport cap, while the formatter reserves
	// enough room for opaque-data framing. JSON is classified before a
	// truncation preview can make opaque data look structured.
	return mcplib.NewToolResultText(bound.LibraryTextCapture(buf.String(), buf.Total(), bound.MaxBytes))
}

// mirroredErrorText keeps a failing command's library output inside the same
// bounded, nonce-delimited transport boundary as its successful output, while
// leaving the command failure outside the data block.
func mirroredErrorText(buf *boundedCapture, detail string) string {
	suffix := "\n" + bound.NeutralizeControls(detail)
	if len(suffix) > maxMirroredErrorBytes {
		end := maxMirroredErrorBytes - len("...")
		for end > 0 && !utf8.RuneStart(suffix[end]) {
			end--
		}
		suffix = suffix[:end] + "..."
	}
	return bound.LibraryTextCapture(buf.String(), buf.Total(), bound.MaxBytes-len(suffix)) + suffix
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
