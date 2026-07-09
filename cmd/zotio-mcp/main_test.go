package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestIsLoopbackHTTPAddr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		addr string
		want bool
	}{
		{name: "ipv4 loopback", addr: "127.0.0.1:7777", want: true},
		{name: "localhost", addr: "localhost:7777", want: true},
		{name: "empty host wildcard", addr: ":7777", want: false},
		{name: "ipv4 wildcard", addr: "0.0.0.0:7777", want: false},
		{name: "ipv6 loopback", addr: "[::1]:7777", want: true},
		{name: "remote ipv4", addr: "1.2.3.4:7777", want: false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isLoopbackHTTPAddr(tt.addr); got != tt.want {
				t.Fatalf("isLoopbackHTTPAddr(%q) = %v, want %v", tt.addr, got, tt.want)
			}
		})
	}
}

func TestValidateMCPHTTPRequestAllowsSameLoopbackOrigin(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:7777/mcp", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "localhost:7777"
	req.Header.Set("Origin", "http://localhost:7777")

	if err := validateMCPHTTPRequest("127.0.0.1:7777", req); err != nil {
		t.Fatalf("validateMCPHTTPRequest rejected same loopback host/origin: %v", err)
	}
}

func TestValidateMCPHTTPRequestRejectsOriginWithoutMatchingExplicitPort(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:7777/mcp", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "127.0.0.1:7777"
	req.Header.Set("Origin", "http://127.0.0.1")

	if err := validateMCPHTTPRequest("127.0.0.1:7777", req); err == nil || !strings.Contains(err.Error(), "Origin") {
		t.Fatalf("origin without explicit matching port error = %v, want forbidden Origin", err)
	}
}

func TestValidateMCPHTTPRequestRejectsForeignHostAndOrigin(t *testing.T) {
	hostReq, err := http.NewRequest(http.MethodPost, "http://evil.example:7777/mcp", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	hostReq.Host = "evil.example:7777"
	if err := validateMCPHTTPRequest("127.0.0.1:7777", hostReq); err == nil || !strings.Contains(err.Error(), "Host") {
		t.Fatalf("foreign Host error = %v, want forbidden Host", err)
	}

	originReq, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:7777/mcp", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	originReq.Host = "127.0.0.1:7777"
	originReq.Header.Set("Origin", "https://attacker.example")
	if err := validateMCPHTTPRequest("127.0.0.1:7777", originReq); err == nil || !strings.Contains(err.Error(), "Origin") {
		t.Fatalf("foreign Origin error = %v, want forbidden Origin", err)
	}
}

func TestValidBearerToken(t *testing.T) {
	t.Parallel()

	const expectedToken = "abcd"
	tests := []struct {
		name          string
		header        string
		expectedToken string
		want          bool
	}{
		{name: "exact bearer token", header: "Bearer " + expectedToken, expectedToken: expectedToken, want: true},
		{name: "missing header", expectedToken: expectedToken, want: false},
		{name: "wrong token", header: "Bearer wrong", expectedToken: expectedToken, want: false},
		{name: "lowercase bearer prefix", header: "bearer " + expectedToken, expectedToken: expectedToken, want: true},
		{name: "token whitespace is trimmed", header: "Bearer \t " + expectedToken + " \n", expectedToken: expectedToken, want: true},
		{name: "present token shorter than expected", header: "Bearer abc", expectedToken: expectedToken, want: false},
		{name: "empty expected token accepts trimmed empty credential", header: "Bearer    ", want: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/mcp", nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}

			if got := validBearerToken(req, tt.expectedToken); got != tt.want {
				t.Fatalf("validBearerToken(%q, %q) = %v, want %v", tt.header, tt.expectedToken, got, tt.want)
			}
		})
	}
}

func TestResolveMCPAuthToken(t *testing.T) {
	t.Run("allow unauthenticated disables token", func(t *testing.T) {
		t.Setenv("ZOTIO_MCP_TOKEN", "env-token")

		gotToken, gotSource, gotGen, err := resolveMCPAuthToken("literal-token", "", true)
		if err != nil {
			t.Fatalf("resolveMCPAuthToken returned error: %v", err)
		}
		if gotToken != "" || gotSource != "" || gotGen {
			t.Fatalf("resolveMCPAuthToken() = token %q source %q generated %v, want disabled auth", gotToken, gotSource, gotGen)
		}
	})

	t.Run("token file wins over environment", func(t *testing.T) {
		t.Setenv("ZOTIO_MCP_TOKEN", "env-token")
		tokenFile := writeMCPTokenFile(t, "file-token\n")

		gotToken, gotSource, gotGen, err := resolveMCPAuthToken("", tokenFile, false)
		if err != nil {
			t.Fatalf("resolveMCPAuthToken returned error: %v", err)
		}
		if gotToken != "file-token" {
			t.Fatalf("token = %q, want %q", gotToken, "file-token")
		}
		if gotSource != "flag:--mcp-auth-token-file" {
			t.Fatalf("source = %q, want %q", gotSource, "flag:--mcp-auth-token-file")
		}
		if gotGen {
			t.Fatal("generated = true, want false")
		}
	})

	t.Run("literal flag value is refused", func(t *testing.T) {
		t.Setenv("ZOTIO_MCP_TOKEN", "")

		gotToken, gotSource, gotGen, err := resolveMCPAuthToken("literal-token", "", false)
		if err == nil {
			t.Fatal("resolveMCPAuthToken returned nil error, want refusal")
		}
		if gotToken != "" || gotSource != "" || gotGen {
			t.Fatalf("resolveMCPAuthToken() = token %q source %q generated %v, want empty result on error", gotToken, gotSource, gotGen)
		}
		if !strings.Contains(err.Error(), "refusing token on command line; use ZOTIO_MCP_TOKEN or --mcp-auth-token-file") {
			t.Fatalf("error = %q, want command-line token guidance", err)
		}
	})

	t.Run("environment token used without file", func(t *testing.T) {
		t.Setenv("ZOTIO_MCP_TOKEN", "env-token")

		gotToken, gotSource, gotGen, err := resolveMCPAuthToken("", "", false)
		if err != nil {
			t.Fatalf("resolveMCPAuthToken returned error: %v", err)
		}
		if gotToken != "env-token" {
			t.Fatalf("token = %q, want %q", gotToken, "env-token")
		}
		if gotSource != "env:ZOTIO_MCP_TOKEN" {
			t.Fatalf("source = %q, want %q", gotSource, "env:ZOTIO_MCP_TOKEN")
		}
		if gotGen {
			t.Fatal("generated = true, want false")
		}
	})

	t.Run("empty token file is refused", func(t *testing.T) {
		t.Setenv("ZOTIO_MCP_TOKEN", "env-token")
		tokenFile := writeMCPTokenFile(t, "\n")

		_, _, _, err := resolveMCPAuthToken("", tokenFile, false)
		if err == nil {
			t.Fatal("resolveMCPAuthToken returned nil error, want empty file error")
		}
		if !strings.Contains(err.Error(), "token file is empty") {
			t.Fatalf("error = %q, want empty token file error", err)
		}
	})

	t.Run("missing configured token generates random token", func(t *testing.T) {
		t.Setenv("ZOTIO_MCP_TOKEN", "")

		gotToken, gotSource, gotGen, err := resolveMCPAuthToken("", "", false)
		if err != nil {
			t.Fatalf("resolveMCPAuthToken returned error: %v", err)
		}
		if gotSource != "generated" || !gotGen {
			t.Fatalf("generated result source=%q generated=%v, want source=%q generated=true", gotSource, gotGen, "generated")
		}
		assertGeneratedMCPToken(t, gotToken)

		nextToken, nextSource, nextGen, err := resolveMCPAuthToken("", "", false)
		if err != nil {
			t.Fatalf("second resolveMCPAuthToken returned error: %v", err)
		}
		if nextSource != "generated" || !nextGen {
			t.Fatalf("second generated result source=%q generated=%v, want source=%q generated=true", nextSource, nextGen, "generated")
		}
		assertGeneratedMCPToken(t, nextToken)
		if nextToken == gotToken {
			t.Fatalf("two generated tokens matched: %q", gotToken)
		}
	})
}

func writeMCPTokenFile(t *testing.T, token string) string {
	t.Helper()

	path := t.TempDir() + "/token"
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	return path
}

func assertGeneratedMCPToken(t *testing.T, token string) {
	t.Helper()

	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(token) {
		t.Fatalf("generated token = %q, want 64 lowercase hex characters", token)
	}
}
