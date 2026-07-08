// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Verify path-parameter percent-encoding blocks path-segment injection.

package mcp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
)

// TestMCPPathValueEscapesPathSegments proves the makeAPIHandler path-parameter
// substitution can no longer be steered to a different endpoint by a value that
// contains URL path metacharacters, while valid Zotero keys pass through byte-for-byte.
func TestMCPPathValueEscapesPathSegments(t *testing.T) {
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

func TestMakeAPIHandlerRedactsConfiguredAPIKeyInErrorResults(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 429, 418} {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			home := mcpTestIsolateConfigEnv(t)
			key := fmt.Sprintf("BAREKEY%d9f8e7d6c", status)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if status == http.StatusTooManyRequests {
					w.Header().Set("Retry-After", "1")
				}
				http.Error(w, "upstream reflected credential "+key, status)
			}))
			t.Cleanup(srv.Close)

			configPath := filepath.Join(home, ".config", "zotio", "config.toml")
			if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
				t.Fatalf("mkdir config dir: %v", err)
			}
			body := fmt.Sprintf("base_url = %q\napi_key = %q\n", srv.URL, key)
			if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}

			handler := makeAPIHandler("GET", "/items", nil, nil)
			req := mcplib.CallToolRequest{}
			res, err := handler(context.Background(), req)
			if err != nil {
				t.Fatalf("handler returned protocol error: %v", err)
			}
			if res == nil || !res.IsError {
				t.Fatalf("handler result = %+v, want MCP error result", res)
			}
			text := mcpToolResultText(t, res)
			if strings.Contains(text, key) {
				t.Fatalf("tool result leaked API key for HTTP %d: %q", status, text)
			}
		})
	}
}

func mcpTestIsolateConfigEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, name := range []string{
		"ZOTERO_API_KEY",
		"ZOTERO_CONFIG",
		"ZOTERO_HOME",
		"ZOTERO_CONFIG_DIR",
		"ZOTERO_DATA_DIR",
		"XDG_CONFIG_HOME",
		"XDG_DATA_HOME",
		"ZOTIO_DEMO",
	} {
		t.Setenv(name, "")
	}
	return home
}
