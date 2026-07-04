package main

import (
	"net/http"
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
