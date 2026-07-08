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
func Deliver(sink DeliverSink, body []byte, compact bool) error {
	switch sink.Scheme {
	case "", "stdout":
		return nil
	case "file":
		return deliverFile(sink.Target, body)
	case "webhook":
		return deliverWebhook(sink.Target, body, compact)
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
	_, err := publicOutboundIPs(context.Background(), host)
	if err != nil && strings.HasPrefix(err.Error(), "resolving host ") {
		// URL validation is also used for stored links that are not fetched
		// immediately. DNS failures are allowed there; fetches are bound to a
		// vetted address later by publicDialContext.
		return nil
	}
	return err
}

func publicOutboundIPs(ctx context.Context, host string) ([]string, error) {
	if addr, err := netip.ParseAddr(strings.TrimSuffix(host, ".")); err == nil {
		if outboundHostIsPrivate(addr.String()) {
			return nil, fmt.Errorf("host %q is local or private", host)
		}
		return []string{addr.String()}, nil
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(addrs) == 0 {
		return nil, fmt.Errorf("resolving host %q: %w", host, err)
	}
	ips := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		if outboundHostIsPrivate(addr.IP.String()) {
			// reject public-looking hostnames that
			// currently resolve to loopback/private/link-local/multicast ranges.
			return nil, fmt.Errorf("host %q resolves to local or private address %s", host, addr.IP)
		}
		ips = append(ips, addr.IP.String())
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
	if client.Transport == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.DialContext = publicDialContext
		client.Transport = transport
	}
	return client
}

func publicDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ips, err := publicOutboundIPs(ctx, host)
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
		cgn := netip.MustParsePrefix("100.64.0.0/10")
		return addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() || cgn.Contains(addr)
	}
	// Single-label names usually resolve only inside a private DNS suffix.
	return !strings.Contains(h, ".")
}

func deliverWebhook(url string, body []byte, compact bool) error {
	// keep direct helper calls as
	// constrained as the public --deliver parser.
	if err := validateExternalHTTPURL(url, false); err != nil {
		return err
	}
	contentType := "application/json"
	if compact {
		contentType = "application/x-ndjson"
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building webhook request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", "zotio/deliver")

	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		// do not follow webhook
		// redirects; a public URL must not bounce into a private service.
		return http.ErrUseLastResponse
	}}
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
