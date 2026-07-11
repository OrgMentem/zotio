// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DeliverSink describes where command output should be routed when
// --deliver is set. Parsed from the sink specifier "scheme:target".
type DeliverSink struct {
	Scheme string
	Target string
}

var allowPrivateOutboundForTests bool

// ParseDeliverSink parses a --deliver value. Supported schemes:
//
//	stdout          -> default, no redirection
//	file:<path>     -> write output atomically to <path>
//	webhook:<url>   -> POST output body to <url>
//
// Returns an error for unknown schemes with a message naming the
// supported set, so agents see a structured refusal rather than a
// silent misroute.
func ParseDeliverSink(spec string) (DeliverSink, error) {
	if spec == "" || spec == "stdout" {
		return DeliverSink{Scheme: "stdout"}, nil
	}
	idx := strings.Index(spec, ":")
	if idx == -1 {
		return DeliverSink{}, fmt.Errorf("unknown --deliver sink %q: expected scheme:target (supported: stdout, file:<path>, webhook:<url>)", spec)
	}
	scheme := spec[:idx]
	target := spec[idx+1:]
	switch scheme {
	case "file":
		if target == "" {
			return DeliverSink{}, fmt.Errorf("--deliver file:<path> requires a path")
		}
	case "webhook":
		// reject private/internal
		// webhook targets before any command output can be POSTed to them.
		if err := validateExternalHTTPURL(target, false); err != nil {
			return DeliverSink{}, fmt.Errorf("--deliver webhook:<url> rejected: %w", err)
		}
	default:
		return DeliverSink{}, fmt.Errorf("unknown --deliver scheme %q (supported: stdout, file, webhook)", scheme)
	}
	return DeliverSink{Scheme: scheme, Target: target}, nil
}

// Deliver routes a captured output buffer to the configured sink. stdout
// is a no-op because the buffer has already been streamed to stdout via
// the MultiWriter set up in root.go.
func Deliver(ctx context.Context, sink DeliverSink, body []byte, compact bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	switch sink.Scheme {
	case "", "stdout":
		return nil
	case "file":
		return deliverFile(sink.Target, body)
	case "webhook":
		return deliverWebhook(ctx, sink.Target, body, compact)
	default:
		return fmt.Errorf("unsupported deliver sink %q", sink.Scheme)
	}
}

func deliverFile(path string, body []byte) error {
	// Atomic write: tmp + rename. Protects agents from seeing a partial
	// file if the process is interrupted mid-write.
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating deliver dir: %w", err)
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return fmt.Errorf("writing deliver tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replacing deliver file: %w", err)
	}
	return nil
}

// validateExternalHTTPURL rejects schemes and hosts that would let optional
// outbound integrations probe local/private networks. requireHTTPS is used for
// background telemetry-style sends where plaintext HTTP is never needed.
func validateExternalHTTPURL(raw string, requireHTTPS bool) error {
	u, err := neturl.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("invalid URL %q: %w", raw, err)
	}
	scheme := strings.ToLower(u.Scheme)
	if requireHTTPS {
		if scheme != "https" {
			return fmt.Errorf("requires an https:// URL")
		}
	} else if scheme != "http" && scheme != "https" {
		return fmt.Errorf("requires an http:// or https:// URL")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("URL must include a host")
	}
	if !allowPrivateOutboundForTests {
		if outboundHostIsPrivate(host) {
			return fmt.Errorf("host %q is local or private", host)
		}
		if err := resolvePublicOutboundHost(host); err != nil {
			return err
		}
	}
	return nil
}

func resolvePublicOutboundHost(host string) error {
	_, err := publicOutboundIPLookup(context.Background(), host)
	if err != nil && strings.HasPrefix(err.Error(), "resolving host ") {
		// URL validation is also used for stored links that are not fetched
		// immediately. DNS failures are allowed there; fetches are bound to a
		// vetted address later by publicDialContext.
		return nil
	}
	return err
}

var publicOutboundIPLookup = publicOutboundIPs

func publicOutboundIPs(ctx context.Context, host string) ([]string, error) {
	if addr, err := netip.ParseAddr(strings.TrimSuffix(host, ".")); err == nil {
		addr = addr.Unmap()
		if !allowPrivateOutboundForTests && outboundHostIsPrivate(addr.String()) {
			return nil, fmt.Errorf("host %q is local or private", host)
		}
		return []string{addr.String()}, nil
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(addrs) == 0 {
		return nil, fmt.Errorf("resolving host %q: %w", host, err)
	}
	ips := make([]string, 0, len(addrs))
	for _, resolved := range addrs {
		addr, ok := netip.AddrFromSlice(resolved.IP)
		if !ok {
			return nil, fmt.Errorf("host %q resolved to invalid address %q", host, resolved.IP)
		}
		addr = addr.Unmap()
		if !allowPrivateOutboundForTests && outboundHostIsPrivate(addr.String()) {
			// reject public-looking hostnames that
			// currently resolve to loopback/private/link-local/multicast ranges.
			return nil, fmt.Errorf("host %q resolves to local or private address %s", host, addr)
		}
		ips = append(ips, addr.String())
	}
	return ips, nil
}

func externalHTTPClient(base *http.Client, requireHTTPS bool) *http.Client {
	client := http.DefaultClient
	if base != nil {
		copied := *base
		client = &copied
	}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		// re-run the same public-host gate on each
		// redirect target; a safe first URL must not bounce into loopback,
		// RFC1918/link-local, or a disallowed scheme.
		if err := validateExternalHTTPURL(req.URL.String(), requireHTTPS); err != nil {
			return err
		}
		return nil
	}
	return client
}

func externalFetchHTTPClient(base *http.Client, requireHTTPS bool) *http.Client {
	client := externalHTTPClient(base, requireHTTPS)
	effectiveTransport := client.Transport
	if effectiveTransport == nil {
		effectiveTransport = http.DefaultTransport
	}
	transport, ok := effectiveTransport.(*http.Transport)
	if ok {
		transport = transport.Clone()
	}
	if ok {
		transport.Proxy = nil
		transport.DialContext = publicDialContext
		client.Transport = transport
	}
	return client
}

func sameOriginExternalFetchHTTPClient(base *http.Client, requireHTTPS bool) *http.Client {
	client := externalFetchHTTPClient(base, requireHTTPS)
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if err := validateExternalHTTPURL(req.URL.String(), requireHTTPS); err != nil {
			return err
		}
		if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 redirects")
		}
		if len(via) == 0 {
			return fmt.Errorf("refusing redirect without an initial request")
		}
		initialScheme, initialHost, initialPort := normalizedExternalHTTPOrigin(via[0].URL)
		redirectScheme, redirectHost, redirectPort := normalizedExternalHTTPOrigin(req.URL)
		if redirectScheme != initialScheme || redirectHost != initialHost || redirectPort != initialPort {
			return fmt.Errorf("refusing cross-origin redirect from %s to %s", via[0].URL, req.URL)
		}
		return nil
	}
	return client
}

func normalizedExternalHTTPOrigin(u *neturl.URL) (scheme, hostname, port string) {
	scheme = strings.ToLower(u.Scheme)
	hostname = strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	port = u.Port()
	if port == "" {
		switch scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}
	return scheme, hostname, port
}

func publicDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ips, err := publicOutboundIPLookup(ctx, host)
	if err != nil {
		return nil, err
	}
	var lastErr error
	var dialer net.Dialer
	for _, ip := range ips {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip, port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("host %q did not resolve to a dialable public address", host)
}

func outboundHostIsPrivate(host string) bool {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	switch h {
	case "", "localhost", "ip6-localhost", "ip6-loopback":
		return true
	}
	if strings.HasSuffix(h, ".localhost") || strings.HasSuffix(h, ".local") {
		return true
	}
	if addr, err := netip.ParseAddr(h); err == nil {
		addr = addr.Unmap()
		cgn := netip.MustParsePrefix("100.64.0.0/10")
		return addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() || cgn.Contains(addr)
	}
	// Single-label names usually resolve only inside a private DNS suffix.
	return !strings.Contains(h, ".")
}

func deliverWebhook(ctx context.Context, url string, body []byte, compact bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	// keep direct helper calls as
	// constrained as the public --deliver parser.
	if err := validateExternalHTTPURL(url, false); err != nil {
		return err
	}
	contentType := "application/json"
	if compact {
		contentType = "application/x-ndjson"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building webhook request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", "zotio/deliver")

	client := externalFetchHTTPClient(&http.Client{Timeout: 30 * time.Second}, false)
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		// do not follow webhook
		// redirects; a public URL must not bounce into a private service.
		return http.ErrUseLastResponse
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("posting to webhook: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("webhook returned %s", resp.Status)
	}
	return nil
}
