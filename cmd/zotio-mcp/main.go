// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"zotio/internal/cli"
	mcptools "zotio/internal/mcp"

	"github.com/mark3labs/mcp-go/server"
)

const defaultHTTPAddr = "127.0.0.1:7777" // Default streamable HTTP to loopback-only.

func main() {
	// Advertise resource + prompt capabilities alongside tools so hosts can
	// discover Zotero context and guided workflows.
	s := server.NewMCPServer(
		"Zotero",
		cli.Version(),
		server.WithToolCapabilities(false),
		server.WithResourceCapabilities(false, true),
		server.WithPromptCapabilities(true),
	)

	mcptools.RegisterTools(s)
	// First-class MCP resources and prompts.
	mcptools.RegisterResources(s)
	mcptools.RegisterPrompts(s)

	// The streamable-HTTP transport lets one binary serve stdio locally and HTTP
	// when hosted in a container/remote sandbox. Transport selection:
	// --transport flag, then ZOTIO_MCP_TRANSPORT env, then stdio (preserves prior
	// behavior). The stdio path uses the hardened NewStdioServer path below so
	// process stdout can be redirected away from the JSON-RPC stream.
	transport := flag.String("transport", defaultTransport(), "MCP transport: stdio | http")
	addr := flag.String("addr", defaultHTTPAddr, "bind address for http transport (host:port or :port)")
	mcpAuthToken := flag.String("mcp-auth-token", "", "refuse bearer tokens on the command line; use ZOTIO_MCP_TOKEN or --mcp-auth-token-file")
	mcpAuthTokenFile := flag.String("mcp-auth-token-file", "", "path to a file containing the bearer token required by the http transport (falls back to ZOTIO_MCP_TOKEN; auto-generated if unset)")
	allowUnauth := flag.Bool("allow-unauthenticated", false, "disable bearer-token auth for the http transport (NOT recommended; loopback is not a per-user boundary)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Printf("zotio-mcp %s\n", cli.Version())
		return
	}

	// Announce the env-selected library and profile so MCP installs (which
	// configure these via env, not flags) are
	// verifiable from the host's server log.
	fmt.Fprintf(os.Stderr, "zotio-mcp %s\n", cli.Version())
	if g := os.Getenv("ZOTERO_GROUP"); g != "" {
		fmt.Fprintf(os.Stderr, "Zotero MCP: group library %s\n", g)
	} else {
		fmt.Fprintln(os.Stderr, "Zotero MCP: personal library")
	}
	if p := os.Getenv("ZOTERO_PROFILE"); p != "" {
		fmt.Fprintf(os.Stderr, "Zotero MCP: profile %s\n", p)
	}

	// Stdio MCP uses stdout as the JSON-RPC transport. Mirrored Cobra commands
	// still contain legacy direct os.Stdout
	// writes, so route accidental process-stdout chatter to stderr and pass the
	// original stdout handle only to the MCP transport. Harmless for HTTP (where
	// command output is captured per-tool), so applied before the switch.
	stdout := os.Stdout
	os.Stdout = os.Stderr

	switch strings.ToLower(*transport) {
	case "stdio":
		stdio := server.NewStdioServer(s)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
		defer signal.Stop(signals)
		go func() {
			<-signals
			cancel()
		}()
		if err := stdio.Listen(ctx, os.Stdin, stdout); err != nil {
			fmt.Fprintf(os.Stderr, "MCP server error: %v\n", err)
			os.Exit(1)
		}
	case "http":
		// Loopback is not a per-user boundary: a co-resident local user (or a
		// non-browser local client)
		// can reach the port. Require a bearer token by default (auto-generated
		// and printed once) unless --allow-unauthenticated is passed.
		authToken, tokSource, tokGenerated, tokErr := resolveMCPAuthToken(*mcpAuthToken, *mcpAuthTokenFile, *allowUnauth)
		if tokErr != nil {
			fmt.Fprintf(os.Stderr, "MCP server error: cannot resolve auth token: %v\n", tokErr)
			os.Exit(1)
		}
		httpSrv := newHardenedStreamableHTTPServer(s, *addr, authToken)
		if !isLoopbackHTTPAddr(*addr) { // Warn when the MCP HTTP surface is exposed off-host.
			fmt.Fprintf(os.Stderr, "WARNING: Zotero MCP HTTP surface at %s is reachable by other hosts; it exposes typed tools with read+write access to the user's Zotero account, and the operator is responsible for network controls.\n", *addr)
		}
		switch {
		case authToken == "":
			fmt.Fprintln(os.Stderr, "WARNING: MCP HTTP transport is running WITHOUT authentication (--allow-unauthenticated); any local process or reachable host can invoke read+write tools.")
		case tokGenerated:
			fmt.Fprintf(os.Stderr, "zotio-mcp: HTTP transport requires a bearer token. Generated one for this run (pin it via ZOTIO_MCP_TOKEN or --mcp-auth-token-file):\n  Authorization: Bearer %s\n", authToken)
		default:
			fmt.Fprintf(os.Stderr, "zotio-mcp: HTTP transport requires a bearer token (source: %s).\n", tokSource)
		}
		fmt.Fprintf(os.Stderr, "zotio-mcp serving MCP over streamable HTTP at %s\n", *addr)

		errs := make(chan error, 1)
		go func() {
			errs <- httpSrv.Start(*addr)
		}()

		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
		defer signal.Stop(signals)

		select {
		case sig := <-signals:
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := httpSrv.Shutdown(shutdownCtx); err != nil {
				fmt.Fprintf(os.Stderr, "MCP server shutdown after %s failed: %v\n", sig, err)
				os.Exit(1)
			}
		case err := <-errs:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				fmt.Fprintf(os.Stderr, "MCP server error: %v\n", err)
				os.Exit(1)
			}
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown --transport %q (supported: stdio, http)\n", *transport)
		os.Exit(2)
	}
}

// defaultTransport reads ZOTIO_MCP_TRANSPORT env when set, otherwise falls
// back to "stdio" so running the binary with no args keeps today's behavior.
func defaultTransport() string {
	if t := os.Getenv("ZOTIO_MCP_TRANSPORT"); t != "" {
		return t
	}
	return "stdio"
}

const maxMCPHTTPBodyBytes = 4 << 20 // Cap POST bodies before mcp-go's io.ReadAll.

// Route mcp-go through a custom HTTP server so POST bodies are capped,
// browser-origin requests are constrained, and the transport can drain via
// StreamableHTTPServer.Shutdown.
func newHardenedStreamableHTTPServer(mcpServer *server.MCPServer, addr, authToken string) *server.StreamableHTTPServer {
	var httpSrv *server.StreamableHTTPServer
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if err := validateMCPHTTPRequest(addr, r); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		// Authenticate every request before mcp-go sees it. Empty token means auth
		// was explicitly disabled via
		// --allow-unauthenticated.
		if authToken != "" && !validBearerToken(r, authToken) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "missing or invalid bearer token", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPost {
			r.Body = http.MaxBytesReader(w, r.Body, maxMCPHTTPBodyBytes)
		}
		httpSrv.ServeHTTP(w, r)
	})
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	httpSrv = server.NewStreamableHTTPServer(mcpServer, server.WithStreamableHTTPServer(httpServer))
	return httpSrv
}

// Local MCP HTTP servers are reachable from a browser. Validate Host and
// Origin before mcp-go sees the request so loopback
// CSRF/DNS-rebinding attempts cannot ride an ambient local server.
func validateMCPHTTPRequest(addr string, r *http.Request) error {
	if !hostAllowedForMCP(addr, r.Host) {
		return fmt.Errorf("forbidden Host header")
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return nil
	}
	u, err := url.ParseRequestURI(origin)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("forbidden Origin header")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("forbidden Origin header")
	}
	if !originAllowedForMCP(addr, u) {
		return fmt.Errorf("forbidden Origin header")
	}
	return nil
}

func originAllowedForMCP(addr string, u *url.URL) bool {
	hostport := u.Host
	if u.Port() == "" {
		hostport = net.JoinHostPort(u.Hostname(), defaultPortForScheme(u.Scheme))
	}
	return hostAllowedForMCP(addr, hostport)
}

func defaultPortForScheme(scheme string) string {
	switch scheme {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

func hostAllowedForMCP(addr, hostport string) bool {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	reqHost, reqPort, err := net.SplitHostPort(hostport)
	if err != nil {
		reqHost = hostport
		reqPort = port
	}
	host = strings.Trim(host, "[]")
	reqHost = strings.Trim(reqHost, "[]")
	if port != "" && reqPort != "" && port != reqPort {
		return false
	}
	if strings.EqualFold(reqHost, host) {
		return true
	}
	if isLoopbackHost(host) && isLoopbackHost(reqHost) {
		return true
	}
	return false
}

func isLoopbackHost(host string) bool {
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Classify only explicit loopback bind hosts as local.
func isLoopbackHTTPAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsUnspecified() {
		return false
	}
	return ip.IsLoopback()
}

// validBearerToken reports whether r carries the expected bearer token, using a
// constant-time comparison.
func validBearerToken(r *http.Request, token string) bool {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return false
	}
	got := strings.TrimSpace(h[len(prefix):])
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

// resolveMCPAuthToken picks the bearer token the HTTP transport enforces:
// --mcp-auth-token-file, then ZOTIO_MCP_TOKEN, else a freshly generated one.
// It returns an empty token (auth disabled) only when allowUnauth is set.
func resolveMCPAuthToken(flagToken, tokenFile string, allowUnauth bool) (token, source string, generated bool, err error) {
	if allowUnauth {
		return "", "", false, nil
	}
	if flagToken != "" {
		return "", "", false, errors.New("refusing token on command line; use ZOTIO_MCP_TOKEN or --mcp-auth-token-file")
	}
	if tokenFile != "" {
		tokenBytes, err := os.ReadFile(tokenFile)
		if err != nil {
			return "", "", false, err
		}
		token := strings.TrimRight(string(tokenBytes), "\r\n")
		if token == "" {
			return "", "", false, errors.New("token file is empty")
		}
		return token, "flag:--mcp-auth-token-file", false, nil
	}
	if v := os.Getenv("ZOTIO_MCP_TOKEN"); v != "" {
		return v, "env:ZOTIO_MCP_TOKEN", false, nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", false, err
	}
	return hex.EncodeToString(buf), "generated", true, nil
}
