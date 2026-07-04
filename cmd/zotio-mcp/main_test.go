package main

import (
	"net/http"
	"net/http/httptest"
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
	tests := []struct {
		name        string
		flagToken   string
		envToken    string
		allowUnauth bool
		wantToken   string
		wantSource  string
		wantGen     bool
	}{
		{name: "allow unauthenticated disables token", envToken: "env-token", allowUnauth: true},
		{name: "flag token wins", flagToken: "flag-token", envToken: "env-token", wantToken: "flag-token", wantSource: "flag:--mcp-auth-token"},
		{name: "environment token used without flag", envToken: "env-token", wantToken: "env-token", wantSource: "env:ZOTIO_MCP_TOKEN"},
		{name: "missing configured token generates random token", wantSource: "generated", wantGen: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ZOTIO_MCP_TOKEN", tt.envToken)

			gotToken, gotSource, gotGen, err := resolveMCPAuthToken(tt.flagToken, tt.allowUnauth)
			if err != nil {
				t.Fatalf("resolveMCPAuthToken returned error: %v", err)
			}
			if gotSource != tt.wantSource {
				t.Fatalf("source = %q, want %q", gotSource, tt.wantSource)
			}
			if gotGen != tt.wantGen {
				t.Fatalf("generated = %v, want %v", gotGen, tt.wantGen)
			}

			if tt.wantGen {
				assertGeneratedMCPToken(t, gotToken)

				nextToken, nextSource, nextGen, err := resolveMCPAuthToken(tt.flagToken, tt.allowUnauth)
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
				return
			}

			if gotToken != tt.wantToken {
				t.Fatalf("token = %q, want %q", gotToken, tt.wantToken)
			}
		})
	}
}

func assertGeneratedMCPToken(t *testing.T, token string) {
	t.Helper()

	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(token) {
		t.Fatalf("generated token = %q, want 64 lowercase hex characters", token)
	}
}
