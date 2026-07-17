// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"zotio/internal/mcp/bound"
	"zotio/internal/store"

	mcplib "github.com/mark3labs/mcp-go/mcp"
)

// TestValidateReadOnlyQuery_AllowsSelectAndWITH pins the contract: the MCP
// sql tool's allowlist accepts SELECT and WITH-prefix queries, including
// CTEs, mixed case, leading whitespace, leading SQL comments, and leading
// statement separators. SELECT-form CTEs ("WITH x AS (SELECT ...) SELECT")
// must work because novel CLI sql commands in the public library accept
// them as legitimate read-only queries; the MCP surface keeps parity.
func TestValidateReadOnlyQuery_AllowsSelectAndWITH(t *testing.T) {
	allowed := []string{
		"SELECT 1",
		"select * from resources",
		"  SELECT 1",
		"\tSELECT 1",
		"\nSELECT 1",
		";SELECT 1",
		"-- comment\nSELECT 1",
		"/* comment */ SELECT 1",
		"/* comment */SELECT 1",
		"/**/SELECT 1",
		"-- one\n-- two\nSELECT 1",
		"/* a *//* b */ SELECT 1",
		"WITH r AS (SELECT 1) SELECT * FROM r",
		"with r as (select 1) select * from r",
	}
	for _, q := range allowed {
		if err := validateReadOnlyQuery(q); err != nil {
			t.Errorf("validateReadOnlyQuery(%q) = %v, want nil", q, err)
		}
	}
}

// TestValidateReadOnlyQuery_RejectsBypassVectors covers the comment-prefix
// bypass class that defeated the earlier prefix-blocklist gate. mode=ro on
// modernc.org/sqlite does not block VACUUM INTO (writes a fresh file) or
// ATTACH DATABASE (opens a separate writable handle), so the gate is the
// only defense against those vectors. A successful bypass at this layer
// would let an MCP-trusting agent silently exfiltrate the local database.
func TestValidateReadOnlyQuery_RejectsBypassVectors(t *testing.T) {
	rejected := []string{
		"VACUUM INTO '/tmp/x.db'",
		"ATTACH DATABASE 'file:/tmp/x.db?mode=rwc' AS evil",
		"INSERT INTO resources VALUES ('x', 'y', '{}')",
		"UPDATE resources SET resource_type = 'evil'",
		"DELETE FROM resources",
		"REPLACE INTO resources VALUES ('seed', 'evil', '{}')",
		"DROP TABLE resources",
		"PRAGMA writable_schema = ON",
		"REINDEX",
		"DETACH DATABASE x",
		"/* x */ VACUUM INTO '/tmp/exfil.db'",
		"/* x */VACUUM INTO '/tmp/exfil.db'",
		"-- x\nVACUUM INTO '/tmp/exfil.db'",
		"/**/VACUUM INTO '/tmp/exfil.db'",
		"/* x */ ATTACH DATABASE 'file:/tmp/x.db?mode=rwc' AS evil",
		"-- x\nATTACH DATABASE '/tmp/x.db' AS evil",
		";VACUUM INTO '/tmp/x.db'",
		"; ; VACUUM INTO '/tmp/x.db'",
		"/* a */ /* b */ INSERT INTO t VALUES (1)",
		"/* outer /* not nested */ */ SELECT 1", // SQLite doesn't nest, so trailing "*/" closes; second SELECT remains. Reject — the gate must err on the side of caution when the leading shape is suspicious.
		"-- only a comment",
		"/* only a comment */",
		"",
		"   ",
		";",
	}
	for _, q := range rejected {
		if err := validateReadOnlyQuery(q); err == nil {
			t.Errorf("validateReadOnlyQuery(%q) = nil, want error", q)
		}
	}
}

// TestValidateReadOnlyQueryRejectsStackedStatements pins the single-statement
// boundary: a SELECT/WITH prefix is still read-only only when no second
// executable SQL statement follows a semicolon. Semicolons inside strings and
// comments remain data, not statement separators.
func TestValidateReadOnlyQueryRejectsStackedStatements(t *testing.T) {
	allowed := []string{
		"SELECT ';' AS literal",
		`SELECT "not;separator"`,
		"SELECT 1; -- trailing comment",
		"SELECT 1; /* trailing comment */",
		"WITH r AS (SELECT ';') SELECT * FROM r;",
	}
	for _, q := range allowed {
		if err := validateReadOnlyQuery(q); err != nil {
			t.Errorf("validateReadOnlyQuery(%q) = %v, want nil", q, err)
		}
	}

	rejected := []string{
		"SELECT 1; VACUUM INTO '/tmp/exfil.db'",
		"SELECT 1; ATTACH DATABASE '/tmp/exfil.db' AS exfil",
		"SELECT 1; INSERT INTO resources VALUES ('x', 'items', '{}')",
		"WITH r AS (SELECT 1) SELECT * FROM r; DELETE FROM resources",
		"SELECT ';'; UPDATE resources SET resource_type = 'items'",
		"SELECT 1 /* ; */; DROP TABLE resources",
	}
	for _, q := range rejected {
		if err := validateReadOnlyQuery(q); err == nil {
			t.Errorf("validateReadOnlyQuery(%q) = nil, want stacked-statement error", q)
		}
	}
}

func TestDBPathUsesNumericZoteroGroup(t *testing.T) {
	home := t.TempDir()
	dataDir := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ZOTERO_DATA_DIR", dataDir)

	t.Setenv("ZOTERO_GROUP", "12345")
	if got, want := dbPath(), filepath.Join(dataDir, "data-group-12345.db"); got != want {
		t.Fatalf("dbPath() with numeric group = %q, want %q", got, want)
	}

	t.Setenv("ZOTERO_GROUP", "team-alpha")
	if got, want := dbPath(), filepath.Join(dataDir, "data.db"); got != want {
		t.Fatalf("dbPath() with non-numeric group = %q, want personal DB %q", got, want)
	}
}

// TestStripLeadingSQLNoise checks the helper directly so a regression in the
// stripping logic (off-by-one on /* */ length, missing newline handling on
// --) surfaces close to the source rather than only via the integration
// behavior of validateReadOnlyQuery.
func TestStripLeadingSQLNoise(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"SELECT 1", "SELECT 1"},
		{"  SELECT 1", "SELECT 1"},
		{"\t\nSELECT 1", "SELECT 1"},
		{";SELECT 1", "SELECT 1"},
		{";; ;SELECT 1", "SELECT 1"},
		{"-- x\nSELECT 1", "SELECT 1"},
		{"-- x\n-- y\nSELECT 1", "SELECT 1"},
		{"/* x */SELECT 1", "SELECT 1"},
		{"/**/SELECT 1", "SELECT 1"},
		{"/* x */ /* y */ SELECT 1", "SELECT 1"},
		{"-- only", ""},
		{"/* only", ""},
		{"", ""},
	}
	for _, c := range cases {
		got := stripLeadingSQLNoise(c.in)
		if !strings.EqualFold(got, c.want) {
			t.Errorf("stripLeadingSQLNoise(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestHandleSQLRecursiveCTEIsRowLimited(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_DATA_DIR", t.TempDir())

	db, err := store.OpenWithContext(context.Background(), dbPath())
	if err != nil {
		t.Fatalf("open writable db: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close writable db: %v", err)
	}

	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"query": "WITH RECURSIVE cnt(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM cnt LIMIT 100000) SELECT x FROM cnt",
	}
	res, err := handleSQL(context.Background(), req)
	if err != nil {
		t.Fatalf("handleSQL protocol error: %v", err)
	}
	if res == nil || res.IsError {
		t.Fatalf("handleSQL result = %+v, want success", res)
	}

	var got struct {
		Rows      []map[string]any `json:"rows"`
		Truncated bool             `json:"truncated"`
		RowLimit  int              `json:"row_limit"`
	}
	text := toolResultText(t, res)
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("decode SQL result %q: %v", text, err)
	}
	if !got.Truncated {
		t.Fatalf("truncated = false, want true")
	}
	if got.RowLimit != sqlRowLimit {
		t.Fatalf("row_limit = %d, want %d", got.RowLimit, sqlRowLimit)
	}
	if len(got.Rows) > sqlRowLimit {
		t.Fatalf("rows returned = %d, want <= %d", len(got.Rows), sqlRowLimit)
	}
	if len(got.Rows) == 0 {
		t.Fatalf("rows returned = 0, want bounded preview rows")
	}
}

func TestHandleSearchBoundsLargeResult(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_DATA_DIR", t.TempDir())

	db, err := store.OpenWithContext(context.Background(), dbPath())
	if err != nil {
		t.Fatalf("open writable db: %v", err)
	}
	bigNote := strings.Repeat("x", 2000)
	items := make([]json.RawMessage, 120)
	for i := range items {
		items[i] = json.RawMessage(fmt.Sprintf(
			`{"key":"B%03d","version":1,"data":{"key":"B%03d","itemType":"journalArticle","title":"Budget needle %03d","abstractNote":"budgetneedle %s"}}`,
			i, i, i, bigNote,
		))
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed items: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close writable db: %v", err)
	}

	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"query": "budgetneedle", "limit": float64(len(items))}
	res, err := handleSearch(context.Background(), req)
	if err != nil {
		t.Fatalf("handleSearch protocol error: %v", err)
	}
	if res == nil || res.IsError {
		t.Fatalf("handleSearch result = %+v, want success", res)
	}

	text := toolResultText(t, res)
	if len(text) > bound.MaxBytes {
		t.Fatalf("bounded response bytes = %d, want <= %d", len(text), bound.MaxBytes)
	}
	var got struct {
		Count     int               `json:"count"`
		Items     []json.RawMessage `json:"items"`
		Truncated bool              `json:"truncated"`
		MaxBytes  int               `json:"max_bytes"`
	}
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("decode bounded search result %q: %v", text, err)
	}
	if got.Count != len(items) {
		t.Fatalf("count = %d, want %d", got.Count, len(items))
	}
	if !got.Truncated {
		t.Fatalf("truncated = false, want true")
	}
	if got.MaxBytes != bound.MaxBytes {
		t.Fatalf("max_bytes = %d, want %d", got.MaxBytes, bound.MaxBytes)
	}
	if len(got.Items) > bound.MaxItems {
		t.Fatalf("items returned = %d, want <= %d", len(got.Items), bound.MaxItems)
	}
}

func toolResultText(t *testing.T, res *mcplib.CallToolResult) string {
	t.Helper()
	if res == nil || len(res.Content) != 1 {
		t.Fatalf("content = %#v, want one text content", res)
	}
	text, ok := res.Content[0].(mcplib.TextContent)
	if !ok {
		t.Fatalf("content[0] = %T, want mcp.TextContent", res.Content[0])
	}
	return text.Text
}

func TestHandleSQLSurfacesIteratorErrorAfterRowLimit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_DATA_DIR", t.TempDir())

	db, err := store.OpenWithContext(context.Background(), dbPath())
	if err != nil {
		t.Fatalf("open writable db: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close writable db: %v", err)
	}

	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"query": "WITH RECURSIVE cnt(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM cnt WHERE x < 5002) SELECT CASE WHEN x = 5002 THEN abs(-9223372036854775808) ELSE x END AS x FROM cnt",
	}
	res, err := handleSQL(context.Background(), req)
	if err != nil {
		t.Fatalf("handleSQL protocol error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("handleSQL result = %+v, want iterator error after row limit", res)
	}
	if got := toolResultText(t, res); !strings.Contains(got, "query failed") {
		t.Fatalf("error result = %q, want query failure", got)
	}
}
