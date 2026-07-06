// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// PATCH(glean 1b05b22e): verify path-parameter percent-encoding blocks path-segment injection.

package mcp

import (
	"context"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
)

// TestMCPPathValueNeutralizesInjection proves the makeAPIHandler path-parameter
// substitution can no longer be steered to a different endpoint by a value that
// contains URL path metacharacters, while valid Zotero keys pass through byte-for-byte.
func TestMCPPathValueNeutralizesInjection(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"valid_key_unchanged", "ABCD1234", "ABCD1234"},
		{"slash_escaped", "ABC/items", "ABC%2Fitems"},
		{"traversal_escaped", "../../keys", "..%2F..%2Fkeys"},
		{"space_escaped", "a b", "a%20b"},
		{"query_escaped", "K?format=json", "K%3Fformat=json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mcpPathValue(tc.in); got != tc.want {
				t.Errorf("mcpPathValue(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestMakeAPIHandlerRejectsTypedWritesBeforeLoadingConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	handler := makeAPIHandler("POST", "/collections", nil, nil)
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"name": "should not matter"}

	res, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned protocol error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("handler result = %+v, want MCP error result", res)
	}
	text := mcpToolResultText(t, res)
	if !strings.Contains(text, "typed MCP endpoint writes are disabled") {
		t.Fatalf("error result text = %q, want typed write refusal", text)
	}
	if strings.Contains(text, "loading config") {
		t.Fatalf("write refusal tried to create/load a client before rejecting: %q", text)
	}
}

func mcpToolResultText(t *testing.T, res *mcplib.CallToolResult) string {
	t.Helper()
	if res == nil || len(res.Content) == 0 {
		return ""
	}
	tc, ok := mcplib.AsTextContent(res.Content[0])
	if !ok {
		t.Fatalf("content[0] is not text: %T", res.Content[0])
	}
	return tc.Text
}
