// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"zotio/internal/cli"
	"zotio/internal/cliutil"
	"zotio/internal/mcp/bound"
	"zotio/internal/mcp/cobratree"
	"zotio/internal/store"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// RegisterTools registers the MCP framework tools plus the selected Cobra command surface.
func RegisterTools(s *server.MCPServer) {
	s.AddTool(
		mcplib.NewTool("search",
			mcplib.WithDescription("Full-text search across all synced data. Faster than paginating list endpoints. Requires sync first. Large responses are bounded to the MCP tool-result budget."),
			mcplib.WithString("query", mcplib.Required(), mcplib.Description("Search query (supports FTS5 syntax: AND, OR, NOT, quotes for phrases)")),
			mcplib.WithNumber("limit", mcplib.Description("Max results (default 25)")),
			mcplib.WithReadOnlyHintAnnotation(true),
			mcplib.WithDestructiveHintAnnotation(false),
		),
		handleSearch,
	)
	// SQL tool — ad-hoc analysis on synced data without API calls
	s.AddTool(
		mcplib.NewTool("sql",
			mcplib.WithDescription("Run read-only SQL against local database. Use for ad-hoc analysis, aggregations, and joins across synced resources. Requires sync first. Returns a JSON object with rows, truncated, and row_limit; large responses are bounded to the MCP tool-result budget."),
			mcplib.WithString("query", mcplib.Required(), mcplib.Description("SQL query (SELECT or WITH...SELECT). Tables match resource names.")),
			mcplib.WithReadOnlyHintAnnotation(true),
			mcplib.WithDestructiveHintAnnotation(false),
		),
		handleSQL,
	)

	// Context tool — front-loaded domain knowledge for agents.
	// Call this first to understand the API taxonomy, query patterns, and capabilities.
	s.AddTool(
		mcplib.NewTool("context",
			mcplib.WithDescription("Get API domain context: resource taxonomy, auth requirements, query tips, and unique capabilities. Call this first."),
			mcplib.WithReadOnlyHintAnnotation(true),
			mcplib.WithDestructiveHintAnnotation(false),
		),
		handleContext,
	)

	// Command surface for the rest of the Cobra tree. ZOTIO_MCP_SURFACE selects
	// the shape. Default "facade" collapses the command tree behind a
	// command_search + command_run pair (~93% fewer tokens at connect, all
	// commands reachable on demand). "mirror" registers one lean MCP tool per
	// command (global flags stripped). Both run in-process against a fresh tree
	// and share the arg-safety guard.
	surface := os.Getenv("ZOTIO_MCP_SURFACE")
	if strings.EqualFold(strings.TrimSpace(surface), "mirror") {
		cobratree.RegisterAll(s, cli.RootCmd)
	} else {
		cobratree.RegisterOrchestration(s, cli.RootCmd)
	}
}

func dbPath() string {
	dataDir, err := cliutil.KindDir(cliutil.PathKindData)
	if err != nil {
		home, _ := os.UserHomeDir()
		dataDir = filepath.Join(home, ".local", "share", cliutil.AppName())
	}
	file := "data.db"
	if groupID := strings.TrimSpace(os.Getenv("ZOTERO_GROUP")); groupID != "" && isDigits(groupID) {
		// Native MCP sql/search/resources must read the same group-scoped mirror
		// selected for cobratree commands.
		file = "data-group-" + groupID + ".db"
	}
	return filepath.Join(dataDir, file)
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Note: MCP tools use their own dbPath() because they are in a separate package (main, not cli).
// The CLI's defaultDBPath() in the cli package uses the same canonical path.

func handleSearch(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	args := req.GetArguments()
	query, ok := args["query"].(string)
	if !ok || query == "" {
		return mcplib.NewToolResultError("query is required"), nil
	}

	limit := 25
	if v, ok := args["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}

	db, err := store.OpenReadOnly(dbPath())
	if err != nil {
		return mcplib.NewToolResultError(fmt.Sprintf("opening database: %v", err)), nil
	}
	defer db.Close()

	results, err := db.Search(query, limit)
	if err != nil {
		return mcplib.NewToolResultError(fmt.Sprintf("search failed: %v", err)), nil
	}

	data, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return mcplib.NewToolResultError(fmt.Sprintf("encoding search results: %v", err)), nil
	}
	return mcplib.NewToolResultText(bound.EndpointResponse("GET", data)), nil
}

// validateReadOnlyQuery gates the MCP sql tool. The agent contract advertised
// to the host is ReadOnlyHintAnnotation(true); a false annotation on a
// mutating tool lets MCP hosts auto-approve writes and is treated as a real
// bug per the project's agent-native security model.
//
// The gate is an allowlist (SELECT or WITH only) applied AFTER stripping the
// leading whitespace, line comments, block comments, and semicolons that
// SQLite itself ignores before parsing. A naive HasPrefix check on a
// keyword blocklist is bypassable by prefixing the dangerous statement with
// "/* x */" or "-- x\n" — TrimSpace strips outer whitespace but does not
// understand SQL comment syntax. Combined with the empirical fact that
// modernc.org/sqlite's mode=ro does NOT block VACUUM INTO (writes a snapshot
// to a new file) or ATTACH DATABASE (opens a separate writable handle),
// such a bypass produces silent exfiltration to an attacker-chosen path.
//
// SELECT and WITH are the only allowed leading keywords. WITH supports
// SELECT-form CTEs; CTE-wrapped writes ("WITH x AS (...) INSERT ...") are
// caught by OpenReadOnly's mode=ro one layer down. PRAGMA, ATTACH, VACUUM,
// and every other DDL/DML keyword fail at this gate before reaching SQLite.
func validateReadOnlyQuery(query string) error {
	trimmed := stripLeadingSQLNoise(query)
	upper := strings.ToUpper(trimmed)
	if !strings.HasPrefix(upper, "SELECT") && !strings.HasPrefix(upper, "WITH") {
		return fmt.Errorf("only SELECT queries are allowed")
	}
	if hasAdditionalSQLStatement(trimmed) {
		// modernc.org/sqlite executes semicolon-stacked statements in one Query
		// call, so a SELECT prefix is
		// insufficient. Allow one SELECT/WITH statement with trailing comments
		// or separators only; reject any second executable statement.
		return fmt.Errorf("only a single SELECT statement is allowed")
	}
	return nil
}

func hasAdditionalSQLStatement(query string) bool {
	inSingle := false
	inDouble := false
	inLineComment := false
	inBlockComment := false
	for i := 0; i < len(query); i++ {
		c := query[i]
		next := byte(0)
		if i+1 < len(query) {
			next = query[i+1]
		}
		switch {
		case inLineComment:
			if c == '\n' {
				inLineComment = false
			}
		case inBlockComment:
			if c == '*' && next == '/' {
				inBlockComment = false
				i++
			}
		case inSingle:
			if c == '\'' {
				if next == '\'' {
					i++
				} else {
					inSingle = false
				}
			}
		case inDouble:
			if c == '"' {
				if next == '"' {
					i++
				} else {
					inDouble = false
				}
			}
		default:
			switch {
			case c == '-' && next == '-':
				inLineComment = true
				i++
			case c == '/' && next == '*':
				inBlockComment = true
				i++
			case c == '\'':
				inSingle = true
			case c == '"':
				inDouble = true
			case c == ';':
				return stripLeadingSQLNoise(query[i+1:]) != ""
			}
		}
	}
	return false
}

// stripLeadingSQLNoise removes leading whitespace, SQL line comments
// (-- to end of line), block comments (/* ... */), and statement
// separators (;) from query. SQLite skips these before parsing the first
// keyword, so a security gate that does not strip them mismatches what the
// driver actually executes.
func stripLeadingSQLNoise(query string) string {
	for {
		query = strings.TrimLeft(query, " \t\r\n;")
		switch {
		case strings.HasPrefix(query, "--"):
			if idx := strings.IndexByte(query, '\n'); idx >= 0 {
				query = query[idx+1:]
				continue
			}
			return ""
		case strings.HasPrefix(query, "/*"):
			if idx := strings.Index(query[2:], "*/"); idx >= 0 {
				query = query[2+idx+2:]
				continue
			}
			return ""
		default:
			return query
		}
	}
}

const (
	sqlQueryTimeout = 15 * time.Second
	sqlRowLimit     = 5000
)

type sqlResultEnvelope struct {
	Rows      []map[string]any `json:"rows"`
	Truncated bool             `json:"truncated"`
	RowLimit  int              `json:"row_limit"`
}

func handleSQL(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	args := req.GetArguments()
	query, ok := args["query"].(string)
	if !ok || query == "" {
		return mcplib.NewToolResultError("query is required"), nil
	}

	if err := validateReadOnlyQuery(query); err != nil {
		return mcplib.NewToolResultError(err.Error()), nil
	}

	db, err := store.OpenReadOnly(dbPath())
	if err != nil {
		return mcplib.NewToolResultError(fmt.Sprintf("opening database: %v", err)), nil
	}
	defer db.Close()

	queryCtx, cancel := context.WithTimeout(ctx, sqlQueryTimeout)
	defer cancel()

	rows, err := db.QueryContext(queryCtx, query)
	if err != nil {
		return mcplib.NewToolResultError(fmt.Sprintf("query failed: %v", err)), nil
	}
	defer rows.Close()

	cols, _ := rows.Columns()
	results := make([]map[string]any, 0)
	truncated := false
	for rows.Next() {
		if len(results) >= sqlRowLimit {
			truncated = true
			break
		}
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return mcplib.NewToolResultError(fmt.Sprintf("scanning row: %v", err)), nil
		}
		row := make(map[string]any)
		for i, col := range cols {
			row[col] = values[i]
		}
		results = append(results, row)
	}
	if !truncated {
		if err := rows.Err(); err != nil {
			return mcplib.NewToolResultError(fmt.Sprintf("query failed: %v", err)), nil
		}
	}

	payload := sqlResultEnvelope{
		Rows:      results,
		Truncated: truncated,
		RowLimit:  sqlRowLimit,
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return mcplib.NewToolResultError(fmt.Sprintf("encoding query results: %v", err)), nil
	}
	return mcplib.NewToolResultText(bound.EndpointResponse("GET", data)), nil
}

func handleContext(_ context.Context, _ mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	// Single source of truth shared with the zotero://context MCP resource
	// (see resources.go domainContext).
	data, _ := json.MarshalIndent(domainContext(), "", "  ")
	return mcplib.NewToolResultText(string(data)), nil
}

// RegisterNovelFeatureTools is kept as a compatibility no-op for older MCP
// mains. New generated mains call RegisterTools only; RegisterTools now
// includes the runtime Cobra-tree mirror.
func RegisterNovelFeatureTools(s *server.MCPServer) {
	_ = s
}
