// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	neturl "net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"zotio/internal/cliutil"
)

// DeliverSink describes where command output should be routed when
// --deliver is set. Parsed from the sink specifier "scheme:target".
type DeliverSink struct {
	Scheme string
	Target string
}

var (
	// allowPrivateOutboundForTests lets tests point the outbound guard at a
	// loopback httptest server. It is atomic because production dial
	// goroutines read it: an in-flight dial can outlive the test that started
	// it, so a plain bool raced that test's cleanup write under -race.
	allowPrivateOutboundForTests atomic.Bool
	guardedExternalTransports    sync.Map // map[*http.Transport]*http.Transport
)

// publicOutboundPreflightTimeout bounds DNS preflight for optional outbound
// integrations. Dial-time resolution under the already-bounded HTTP request
// stays authoritative; preflight only rejects private literals early.
const publicOutboundPreflightTimeout = 5 * time.Second

// ParseDeliverSinkWithContext parses a --deliver value. Supported schemes:
//
//	stdout          -> default, no redirection
//	file:<path>     -> write output atomically to <path>
//	webhook:<url>   -> POST output body to <url>
//
// Returns an error for unknown schemes with a message naming the
// supported set, so agents see a structured refusal rather than a
// silent misroute. The caller's context bounds webhook DNS preflight so a
// stalled lookup cannot block the command before it runs, even after
// cancellation or --timeout.
func ParseDeliverSinkWithContext(ctx context.Context, spec string) (DeliverSink, error) {
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
		if err := validateExternalHTTPURLWithContext(ctx, target, false); err != nil {
			return DeliverSink{}, fmt.Errorf("--deliver webhook:<url> rejected: %w", err)
		}
	default:
		return DeliverSink{}, fmt.Errorf("unknown --deliver scheme %q (supported: stdout, file, webhook)", scheme)
	}
	return DeliverSink{Scheme: scheme, Target: target}, nil
}

// Deliver routes spooled command output to the configured sink. stdout is a
// no-op because the output already went there through the MultiWriter set up
// in root.go. Nothing is read back into memory: a file sink renames the spool
// into place, a webhook streams it as the request body.
//
// A file sink joins the output collision namespace (ADR-0005): it takes the
// same <canonical target>.lock every primary writer to that path takes. The
// lock is acquired directly rather than through withPathWriterLock because
// delivery runs after Execute returns, when the command's ownership stack is
// already unwound. It is held only across the rename — delivery has no load
// phase on the target — and a busy lock is returned to the caller, whose
// warn-only channel turns it into the documented skip-with-warning.
func Deliver(ctx context.Context, sink DeliverSink, spool *deliverSpool, compact bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	switch sink.Scheme {
	case "", "stdout":
		return nil
	case "file":
		lockPath, canonicalTarget, err := outputWriterLockPath(sink.Target)
		if err != nil {
			return fmt.Errorf("resolving deliver target: %w", err)
		}
		lock, err := cliutil.AcquireWriterLock(lockPath, fmt.Sprintf("delivering to %q", canonicalTarget))
		if err != nil {
			return err
		}
		commitErr := spool.commitFile()
		if releaseErr := lock.Release(); releaseErr != nil {
			return errors.Join(commitErr, fmt.Errorf("releasing writer lock at %q: %w", lockPath, releaseErr))
		}
		return commitErr
	case "webhook":
		return deliverWebhookSpool(ctx, sink.Target, spool, compact)
	default:
		return fmt.Errorf("unsupported deliver sink %q", sink.Scheme)
	}
}

// validateExternalHTTPURL rejects schemes and hosts that would let optional
// outbound integrations probe local/private networks. requireHTTPS is used for
// background telemetry-style sends where plaintext HTTP is never needed.
func validateExternalHTTPURL(raw string, requireHTTPS bool) error {
	return validateExternalHTTPURLWithContext(context.Background(), raw, requireHTTPS)
}

// validateExternalHTTPURLWithContext is validateExternalHTTPURL with the
// caller's context: preflight DNS inherits cancellation and is capped at
// publicOutboundPreflightTimeout so a stalled lookup cannot outlive the
// command. Dial-time resolution under the bounded HTTP request remains the
// authoritative guard.
func validateExternalHTTPURLWithContext(ctx context.Context, raw string, requireHTTPS bool) error {
	u, err := neturl.Parse(strings.TrimSpace(raw))
	if err != nil {
		return errors.New("not a valid URL")
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
	if !allowPrivateOutboundForTests.Load() {
		if outboundHostIsPrivate(host) {
			return fmt.Errorf("host %q is local or private", host)
		}
		if err := resolvePublicOutboundHostWithContext(ctx, host); err != nil {
			return err
		}
	}
	return nil
}
func resolvePublicOutboundHostWithContext(ctx context.Context, host string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	lookupCtx, cancel := context.WithTimeout(ctx, publicOutboundPreflightTimeout)
	defer cancel()
	_, err := publicOutboundIPLookup(lookupCtx, host)
	if lookupCtx.Err() != nil {
		return lookupCtx.Err()
	}
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
		if !allowPrivateOutboundForTests.Load() && outboundHostIsPrivate(addr.String()) {
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
		if !allowPrivateOutboundForTests.Load() && outboundHostIsPrivate(addr.String()) {
			// reject public-looking hostnames that
			// currently resolve to loopback/private/link-local/multicast ranges.
			return nil, fmt.Errorf("host %q resolves to local or private address %s", host, addr)
		}
		ips = append(ips, addr.String())
	}
	return ips, nil
}

func externalHTTPClient(base *http.Client, requireHTTPS bool) *http.Client {
	// Never start from http.DefaultClient: the CheckRedirect assignment below
	// would mutate process-global state shared by every other caller, racing
	// concurrent requests under the in-process MCP server. A nil base is an
	// expected shape (see client.requestHTTPClient), so synthesize a client.
	client := &http.Client{}
	if base != nil {
		copied := *base
		client = &copied
	}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		// re-run the same public-host gate on each
		// redirect target; a safe first URL must not bounce into loopback,
		// RFC1918/link-local, or a disallowed scheme. The request context
		// bounds the preflight lookup so cancellation interrupts it.
		ctx := context.Background()
		if req != nil && req.Context() != nil {
			ctx = req.Context()
		}
		if err := validateExternalHTTPURLWithContext(ctx, req.URL.String(), requireHTTPS); err != nil {
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
	if transport, ok := effectiveTransport.(*http.Transport); ok {
		client.Transport = guardedExternalTransport(transport)
	}
	return client
}

func guardedExternalTransport(base *http.Transport) *http.Transport {
	if cached, ok := guardedExternalTransports.Load(base); ok {
		return cached.(*http.Transport)
	}
	transport := base.Clone()
	// A proxy would resolve the destination outside this process and bypass the
	// destination-address guard, so external fetches always connect directly.
	transport.Proxy = nil
	transport.DialContext = publicDialContext
	actual, _ := guardedExternalTransports.LoadOrStore(base, transport)
	return actual.(*http.Transport)
}

func sameOriginExternalFetchHTTPClient(base *http.Client, requireHTTPS bool) *http.Client {
	client := externalFetchHTTPClient(base, requireHTTPS)
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		ctx := context.Background()
		if req != nil && req.Context() != nil {
			ctx = req.Context()
		}
		if err := validateExternalHTTPURLWithContext(ctx, req.URL.String(), requireHTTPS); err != nil {
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
		return nil, &outboundDialRefusal{err: err}
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
	return nil, &outboundDialRefusal{err: fmt.Errorf("host %q did not resolve to a dialable public address", host)}
}

// outboundDialRefusal marks an error from publicDialContext's own host vetting.
// Its text names only the host and resolved addresses, so outboundRequestError
// keeps it when it rebuilds a redacted message. It is deliberately not a
// net.Error: a policy refusal is not network unreachability, and the
// offline/fallback classification (isNetworkError) must see only what the
// wrapped cause already is.
type outboundDialRefusal struct{ err error }

func (e *outboundDialRefusal) Error() string { return e.err.Error() }
func (e *outboundDialRefusal) Unwrap() error { return e.err }

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

// deliverWebhookSpool posts the spooled output as a streamed,
// length-delimited body without reading it back into memory.
func deliverWebhookSpool(ctx context.Context, url string, spool *deliverSpool, compact bool) error {
	body, err := spool.reader()
	if err != nil {
		return err
	}
	return postDeliverWebhook(ctx, url, body, spool.Len(), compact)
}

func deliverWebhook(ctx context.Context, url string, body []byte, compact bool) error {
	return postDeliverWebhook(ctx, url, bytes.NewReader(body), int64(len(body)), compact)
}

func postDeliverWebhook(ctx context.Context, url string, body io.ReadSeeker, length int64, compact bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	// keep direct helper calls as
	// constrained as the public --deliver parser. The request context bounds
	// the preflight lookup so a stalled DNS cannot outlive cancellation; the
	// 30s client below bounds the dial and POST that follow.
	if err := validateExternalHTTPURLWithContext(ctx, url, false); err != nil {
		return err
	}
	contentType := "application/json"
	if compact {
		contentType = "application/x-ndjson"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return fmt.Errorf("building webhook request: %w", err)
	}
	// net/http cannot size an arbitrary reader, and a webhook receiver that
	// rejects chunked encoding would see an empty body without this. GetBody
	// reuses the seekable spool so net/http never has to buffer the body.
	req.ContentLength = length
	req.GetBody = func() (io.ReadCloser, error) {
		if _, seekErr := body.Seek(0, io.SeekStart); seekErr != nil {
			return nil, seekErr
		}
		return io.NopCloser(body), nil
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
		return outboundRequestError("posting to webhook", url, err)
	}
	defer resp.Body.Close()
	return externalHTTPPostStatusError("webhook", resp)
}

// webhookOrigin reduces a webhook URL to scheme://host[:port] for messages.
// Webhook endpoints carry their bearer credential in the path (Slack, Discord)
// or the query as often as in userinfo, so the origin is the only part that is
// safe to print; it still tells the operator which endpoint failed.
func webhookOrigin(raw string) string {
	u, err := neturl.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "<unparseable webhook URL redacted>"
	}
	return (&neturl.URL{Scheme: u.Scheme, Host: u.Host}).String()
}

// outboundRequestError reports a failed outbound request by the target's origin
// and a cause that cannot quote a URL. Webhook and feedback endpoints carry
// their credential in the URL, and net/http prints URLs in two places: the
// *url.Error it returns names the request URL (with only the password
// stripped), and some causes quote another URL, such as a redirect Location
// that failed to parse. The message names only the origin and a cause from
// urlFreeCause; the original error stays wrapped for errors.Is and errors.As,
// so cancellation and timeouts stay detectable.
func outboundRequestError(operation, target string, err error) error {
	op, cause := "request", err
	var urlErr *neturl.Error
	if errors.As(err, &urlErr) {
		op, cause = urlErr.Op, urlErr.Err
	}
	return &redactedError{
		msg: fmt.Sprintf("%s: %s %q: %s", operation, op, webhookOrigin(target), urlFreeCause(cause)),
		err: err,
	}
}

// urlFreeCause returns the text of a transport cause only when its type cannot
// carry URL text: context ends, connection and DNS failures and dial-time host
// refusals (whose messages name hosts and addresses), TLS failures, and
// truncated responses. Any other cause is withheld, because net/http builds
// some of them from server-supplied URLs that can echo the credential.
func urlFreeCause(cause error) string {
	if cause == nil {
		return "request failed"
	}
	var nested *neturl.Error
	if errors.As(cause, &nested) {
		return outboundCauseWithheld
	}
	var (
		netErr     net.Error
		refusal    *outboundDialRefusal
		certErr    *tls.CertificateVerificationError
		recordErr  tls.RecordHeaderError
		alertErr   tls.AlertError
		unknownErr x509.UnknownAuthorityError
		hostErr    x509.HostnameError
		invalidErr x509.CertificateInvalidError
	)
	switch {
	case errors.Is(cause, context.Canceled),
		errors.Is(cause, context.DeadlineExceeded),
		errors.Is(cause, io.EOF),
		errors.Is(cause, io.ErrUnexpectedEOF),
		errors.As(cause, &netErr),
		errors.As(cause, &refusal),
		errors.As(cause, &certErr),
		errors.As(cause, &recordErr),
		errors.As(cause, &alertErr),
		errors.As(cause, &unknownErr),
		errors.As(cause, &hostErr),
		errors.As(cause, &invalidErr):
		return cause.Error()
	}
	return outboundCauseWithheld
}

const outboundCauseWithheld = "request failed (cause withheld: it may quote a URL)"

// externalHTTPPostStatusError accepts only a 2xx response. Both outbound POST
// helpers install http.ErrUseLastResponse, so net/http hands back the 3xx
// itself instead of following it: a redirect means the body was never
// delivered, and reporting it as success hides a stale endpoint URL. The status
// line is named so the operator sees which redirect to fix. operation keeps the
// callsite's own wording in the message.
func externalHTTPPostStatusError(operation string, resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("%s returned %s", operation, resp.Status)
}
