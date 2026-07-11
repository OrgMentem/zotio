// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"zotio/internal/cliutil"
	"zotio/internal/config"
)

const (
	maxZoteroResponseBytes = 64 << 20
	defaultZoteroBaseURL   = "http://localhost:23119/api/users/0"
)

// default client calls inherit cancellation from
// process interrupts so Ctrl-C/SIGTERM abort in-flight HTTP work promptly.
var (
	interruptCtxOnce sync.Once
	interruptCtx     context.Context
)

func sigintContext() context.Context {
	interruptCtxOnce.Do(func() {
		interruptCtx, _ = signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	})
	return interruptCtx
}

type Client struct {
	BaseURL    string
	Config     *config.Config
	HTTPClient *http.Client
	DryRun     bool
	NoCache    bool
	cacheDir   string
	limiter    *cliutil.AdaptiveLimiter
	// base context for wrapper calls; tests may replace it.
	ctx context.Context
	// WriteBaseURL, when set, receives all non-GET requests while reads continue to
	// use BaseURL — the Zotero local API is read-only, so writes route to the Web
	// API. ResolveWriteBase lazily computes it on the first write (kept in the CLI
	// layer so the client stays generic); writeRouteOnce guards that resolution.
	WriteBaseURL     string
	ResolveWriteBase func(context.Context) (string, error)
	writeRouteOnce   sync.Once
	// protect lazy hybrid write-route resolution.
	writeRouteMu sync.RWMutex
}

// APIError carries HTTP status information for structured exit codes.
type APIError struct {
	Method     string
	Path       string
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s %s returned HTTP %d: %s", e.Method, e.Path, e.StatusCode, e.Body)
}

func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	if !sameOrigin(req.URL, via[0].URL) {
		return fmt.Errorf("refusing cross-origin redirect")
	}
	return nil
}

func sameOrigin(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}
	return strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		effectivePort(a) == effectivePort(b)
}

func effectivePort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

func newHTTPClient(timeout time.Duration, jar http.CookieJar) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		Jar:           jar,
		CheckRedirect: checkRedirect,
	}
}

func (c *Client) requestHTTPClient() *http.Client {
	selected := c.HTTPClient
	if selected == nil {
		selected = http.DefaultClient
	}

	client := *selected
	callerCheckRedirect := selected.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) == 0 {
			return nil
		}
		if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 redirects")
		}
		initialURL := *via[0].URL
		if !sameOrigin(req.URL, &initialURL) {
			return fmt.Errorf("refusing cross-origin redirect")
		}
		if callerCheckRedirect != nil {
			if err := callerCheckRedirect(req, via); err != nil {
				return err
			}
			if !sameOrigin(req.URL, &initialURL) {
				return fmt.Errorf("refusing cross-origin redirect")
			}
		}
		return nil
	}
	return &client
}

func New(cfg *config.Config, timeout time.Duration, rateLimit float64) *Client {
	homeDir, _ := os.UserHomeDir()
	cacheDir := filepath.Join(homeDir, ".cache", "zotio")
	httpClient := newHTTPClient(timeout, nil)
	baseURL := sanitizeClientBaseURL(cfg.BaseURL)
	return &Client{
		BaseURL:    baseURL,
		Config:     cfg,
		HTTPClient: httpClient,
		cacheDir:   cacheDir,
		limiter:    cliutil.NewAdaptiveLimiter(rateLimit),
		ctx:        sigintContext(),
	}
}

// CloneForRead returns a read-only client targeting baseURL, sharing the config,
// HTTP client, rate limiter, and cancellation context but with fresh
// synchronization state. A Client must never be copied by value because it holds
// a sync.Once and RWMutex; global schema endpoints need the library prefix
// stripped from BaseURL, so clone explicitly instead.
func (c *Client) CloneForRead(baseURL string) *Client {
	return &Client{
		BaseURL:    baseURL,
		Config:     c.Config,
		HTTPClient: c.HTTPClient,
		DryRun:     c.DryRun,
		NoCache:    c.NoCache,
		cacheDir:   c.cacheDir,
		limiter:    c.limiter,
		ctx:        c.ctx,
	}
}

func (c *Client) baseCtx() context.Context {
	// tolerate zero-value clients while still giving
	// normal clients a SIGINT/SIGTERM-cancellable context.
	if c != nil && c.ctx != nil {
		return c.ctx
	}
	return context.Background()
}

// RateLimit returns the current effective rate limit in req/s. Returns 0 if disabled.
func (c *Client) RateLimit() float64 {
	return c.limiter.Rate()
}

// public wrappers keep their signatures while using
// the client base context so interrupts cancel their HTTP work.
func (c *Client) Get(path string, params map[string]string) (json.RawMessage, error) {
	return c.GetWithHeaders(path, params, nil)
}

func (c *Client) GetWithHeaders(path string, params map[string]string, headers map[string]string) (json.RawMessage, error) {
	// Check cache for GET requests
	if !c.NoCache && !c.DryRun && c.cacheDir != "" {
		if cached, ok := c.readCache(path, params, headers); ok {
			return cached, nil
		}
	}
	result, _, err := c.do(c.baseCtx(), "GET", path, params, nil, headers)
	if err == nil && !c.NoCache && !c.DryRun && c.cacheDir != "" {
		c.writeCache(path, params, headers, result)
	}
	return result, err
}

func (c *Client) ProbeGet(path string) (int, error) {
	_, status, err := c.do(c.baseCtx(), "GET", path, nil, nil, nil)
	return status, err
}

func (c *Client) cacheKey(path string, params map[string]string, headers map[string]string) string {
	key := path
	key += "|base_url=" + c.BaseURL
	if c.Config != nil {
		key += "|auth_source=" + c.Config.AuthSource
		if authHeader := c.Config.AuthHeader(); authHeader != "" {
			authHash := sha256.Sum256([]byte(c.Config.AuthHeader()))
			key += "|auth=" + hex.EncodeToString(authHash[:8])
		}
		if c.Config.Path != "" {
			key += "|config_path=" + c.Config.Path
		}
	}
	paramKeys := make([]string, 0, len(params))
	for k := range params {
		paramKeys = append(paramKeys, k)
	}
	sort.Strings(paramKeys)
	for _, k := range paramKeys {
		key += k + "=" + params[k]
	}
	headerKeys := make([]string, 0, len(headers))
	for k := range headers {
		headerKeys = append(headerKeys, k)
	}
	sort.Strings(headerKeys)
	for _, k := range headerKeys {
		key += "|header:" + k + "=" + headers[k]
	}
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:8])
}

func (c *Client) readCache(path string, params map[string]string, headers map[string]string) (json.RawMessage, bool) {
	cacheFile := filepath.Join(c.cacheDir, c.cacheKey(path, params, headers)+".json")
	info, err := os.Stat(cacheFile)
	if err != nil || time.Since(info.ModTime()) > 5*time.Minute {
		return nil, false
	}
	data, err := os.ReadFile(cacheFile)
	if err != nil {
		return nil, false
	}
	return json.RawMessage(data), true
}

func (c *Client) writeCache(path string, params map[string]string, headers map[string]string, data json.RawMessage) {
	// cached Zotero API payloads
	// contain private library metadata, so keep the directory and files private
	// even when they already existed with older world-readable permissions.
	_ = os.MkdirAll(c.cacheDir, 0o700)
	_ = os.Chmod(c.cacheDir, 0o700)
	cacheFile := filepath.Join(c.cacheDir, c.cacheKey(path, params, headers)+".json")
	_ = os.WriteFile(cacheFile, []byte(data), 0o600)
	_ = os.Chmod(cacheFile, 0o600)
}

// invalidateCache wholesale-removes the cache directory so the next read
// after a mutation cannot return a stale snapshot. Selective per-resource
// invalidation rejected: cache keys are opaque sha256 hashes.
func (c *Client) invalidateCache() {
	if c.cacheDir == "" {
		return
	}
	_ = os.RemoveAll(c.cacheDir)
}

func (c *Client) Post(path string, body any) (json.RawMessage, int, error) {
	return c.do(c.baseCtx(), "POST", path, nil, body, nil)
}

func (c *Client) PostWithHeaders(path string, body any, headers map[string]string) (json.RawMessage, int, error) {
	return c.do(c.baseCtx(), "POST", path, nil, body, headers)
}

func (c *Client) Delete(path string) (json.RawMessage, int, error) {
	return c.do(c.baseCtx(), "DELETE", path, nil, nil, nil)
}

func (c *Client) DeleteWithHeaders(path string, headers map[string]string) (json.RawMessage, int, error) {
	return c.do(c.baseCtx(), "DELETE", path, nil, nil, headers)
}

func (c *Client) Put(path string, body any) (json.RawMessage, int, error) {
	return c.do(c.baseCtx(), "PUT", path, nil, body, nil)
}

func (c *Client) PutWithHeaders(path string, body any, headers map[string]string) (json.RawMessage, int, error) {
	return c.do(c.baseCtx(), "PUT", path, nil, body, headers)
}

func (c *Client) Patch(path string, body any) (json.RawMessage, int, error) {
	return c.do(c.baseCtx(), "PATCH", path, nil, body, nil)
}

func (c *Client) PatchWithHeaders(path string, body any, headers map[string]string) (json.RawMessage, int, error) {
	return c.do(c.baseCtx(), "PATCH", path, nil, body, headers)
}

// do executes an HTTP request. headerOverrides, when non-nil, override global
// RequiredHeaders for this specific request (used for per-endpoint API versioning).
func (c *Client) doRequest(ctx context.Context, method, path string, params map[string]string, body any, headerOverrides map[string]string) (json.RawMessage, int, http.Header, error) {
	// all network construction below requires a
	// non-nil context so callers can cancel request creation, dialing, and reads.
	if ctx == nil {
		ctx = context.Background()
	}
	targetURL := c.baseURLFor(ctx, method) + path

	var bodyBytes []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, nil, fmt.Errorf("marshaling body: %w", err)
		}
		bodyBytes = b
	}

	// Resolve auth material before the dry-run branch so --dry-run can preview
	// exactly what would be sent. Uses only cached credentials; a token that
	// requires a network refresh will be re-fetched on the live request path,
	// not during dry-run.
	authHeader, err := c.authHeader()
	if err != nil {
		return nil, 0, nil, err
	}

	// Build the request for dry-run display or actual execution
	if c.DryRun {
		respBody, status, derr := c.dryRun(method, targetURL, path, params, bodyBytes, headerOverrides, authHeader)
		return respBody, status, nil, derr
	}

	const maxRetries = 3
	var lastErr error

	for attempt := 0; attempt <= maxRetries; attempt++ {
		// proactive rate limiting must honor context
		// cancellation before dialing.
		c.limiter.WaitContext(ctx)
		if err := ctx.Err(); err != nil {
			return nil, 0, nil, err
		}
		var bodyReader io.Reader
		if bodyBytes != nil {
			bodyReader = strings.NewReader(string(bodyBytes))
		}

		req, err := http.NewRequestWithContext(ctx, method, targetURL, bodyReader)
		if err != nil {
			return nil, 0, nil, fmt.Errorf("creating request: %w", err)
		}
		if bodyBytes != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		if params != nil {
			q := req.URL.Query()
			for k, v := range params {
				if v != "" {
					q.Set(k, v)
				}
			}
			req.URL.RawQuery = q.Encode()
		}

		// only attach the Zotero API
		// key to trusted Zotero/local API origins, so a hostile ZOTERO_BASE_URL
		// override cannot harvest credentials.
		if authHeader != "" && shouldSendZoteroAuth(req.URL) {
			req.Header.Set("Zotero-API-Key", authHeader)
		}
		if c.Config != nil {
			for k, v := range c.Config.Headers {
				req.Header.Set(k, v)
			}
		}
		// Per-endpoint header overrides (e.g., different API version per resource)
		for k, v := range headerOverrides {
			req.Header.Set(k, v)
		}
		// also strip any custom
		// config/override auth headers from untrusted base URLs.
		if !shouldSendZoteroAuth(req.URL) {
			req.Header.Del("Zotero-API-Key")
			req.Header.Del("Authorization")
		}
		if req.Header.Get("User-Agent") == "" {
			req.Header.Set("User-Agent", "zotio/0.1.0")
		}

		resp, err := c.requestHTTPClient().Do(req)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, 0, nil, fmt.Errorf("%s %s: %w", method, path, ctxErr)
			}
			lastErr = fmt.Errorf("%s %s: %w", method, path, err)
			continue
		}

		// cap API response bodies
		// before buffering them for cache/error handling.
		respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxZoteroResponseBytes+1))
		resp.Body.Close()
		if err != nil {
			return nil, 0, nil, fmt.Errorf("reading response: %w", err)
		}
		if int64(len(respBody)) > maxZoteroResponseBytes {
			return nil, 0, nil, fmt.Errorf("response exceeded %d bytes", maxZoteroResponseBytes)
		}
		respBody = sanitizeJSONResponse(respBody)

		// Only 2xx responses are successful. In particular, a caller's
		// ErrUseLastResponse must not turn a refused redirect into success.
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			c.limiter.OnSuccess()
			if method != http.MethodGet && !c.DryRun {
				c.invalidateCache()
			}
			return json.RawMessage(respBody), resp.StatusCode, resp.Header, nil
		}

		apiErr := &APIError{
			Method:     method,
			Path:       path,
			StatusCode: resp.StatusCode,
			Body:       truncateBody(respBody),
		}

		// Rate limited - adjust adaptive limiter and retry
		if resp.StatusCode == 429 && attempt < maxRetries {
			c.limiter.OnRateLimit()
			wait := cliutil.RetryAfter(resp)
			fmt.Fprintf(os.Stderr, "rate limited, waiting %s (attempt %d/%d, rate adjusted to %.1f req/s)\n", wait, attempt+1, maxRetries, c.limiter.Rate())
			if err := sleepWithContext(ctx, wait); err != nil {
				return nil, 0, nil, err
			}
			lastErr = apiErr
			continue
		}

		// Server error - retry with backoff. 501 Not Implemented is never transient
		// (e.g. writes against the read-only Zotero local API), so don't retry it.
		// avoid a pointless 3x backoff storm on local-API write rejections.
		if resp.StatusCode >= 500 && resp.StatusCode != 501 && attempt < maxRetries {
			wait := time.Duration(math.Pow(2, float64(attempt))) * time.Second
			fmt.Fprintf(os.Stderr, "server error %d, retrying in %s (attempt %d/%d)\n", resp.StatusCode, wait, attempt+1, maxRetries)
			if err := sleepWithContext(ctx, wait); err != nil {
				return nil, 0, nil, err
			}
			lastErr = apiErr
			continue
		}

		// Client error or retries exhausted - return the error
		return nil, resp.StatusCode, resp.Header, apiErr
	}

	return nil, 0, nil, lastErr
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	// retry and Retry-After waits must unblock when
	// the owning request context is canceled by Ctrl-C or tests.
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func shouldSendZoteroAuth(u *url.URL) bool {
	if u == nil {
		return false
	}
	// Local Zotero HTTP does not need the Web API key; only the canonical HTTPS
	// Web API should receive it.
	return u.Scheme == "https" && strings.EqualFold(u.Hostname(), "api.zotero.org")
}

func sanitizeClientBaseURL(raw string) string {
	base := strings.TrimRight(raw, "/")
	u, err := url.Parse(base)
	if err == nil && trustedZoteroBaseURL(u) {
		return base
	}
	// reject hostile base URL
	// overrides before any API traffic is routed to an attacker-controlled host.
	fmt.Fprintf(os.Stderr, "warning: ignoring untrusted Zotero base URL %q; using %s\n", raw, defaultZoteroBaseURL)
	return defaultZoteroBaseURL
}

func trustedZoteroBaseURL(u *url.URL) bool {
	if u == nil {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if u.Scheme == "https" && host == "api.zotero.org" {
		return true
	}
	if u.Scheme != "http" {
		return false
	}
	if host == "localhost" {
		return true
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.IsLoopback()
	}
	return false
}

// baseURLFor returns the base URL for a request: writes (non-GET) route to the
// resolved WriteBaseURL when hybrid routing is configured; reads use BaseURL. The
// write base is resolved lazily on first use.
func (c *Client) baseURLFor(ctx context.Context, method string) string {
	if method == http.MethodGet || method == http.MethodHead {
		return c.BaseURL
	}
	c.resolveWriteRoute(ctx)
	c.writeRouteMu.RLock()
	writeBase := c.WriteBaseURL
	c.writeRouteMu.RUnlock()
	if writeBase != "" {
		return writeBase
	}
	return c.BaseURL
}

// resolveWriteRoute runs the CLI-provided write-base resolver at most once. On
// success it sets WriteBaseURL and prints a one-time notice; on failure it leaves
// WriteBaseURL empty so the write falls through to BaseURL (and the read-only guard).
func (c *Client) resolveWriteRoute(ctx context.Context) {
	if c.ResolveWriteBase == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.writeRouteMu.RLock()
	resolved := c.WriteBaseURL != ""
	c.writeRouteMu.RUnlock()
	if resolved {
		return
	}
	c.writeRouteOnce.Do(func() {
		base, err := c.ResolveWriteBase(ctx)
		if err != nil || base == "" {
			return
		}
		c.writeRouteMu.Lock()
		c.WriteBaseURL = base
		c.writeRouteMu.Unlock()
		fmt.Fprintf(os.Stderr, "→ writing via Zotero Web API: %s (reads stay local)\n", base)
	})
}

// do executes an HTTP request and discards response headers, wrapping doRequest
// for the many callers that do not need them.
func (c *Client) do(ctx context.Context, method, path string, params map[string]string, body any, headerOverrides map[string]string) (json.RawMessage, int, error) {
	// Verify-mode transport gate: under ZOTIO_VERIFY=1 (without the
	// ZOTIO_VERIFY_LIVE_HTTP=1 opt-in), a mutating verb returns a synthetic
	// envelope and never dials, mints auth, or touches the cache.
	if isMutatingVerb(method) && cliutil.IsVerifyEnv() && !cliutil.IsVerifyLiveHTTPEnv() {
		return verifyShortCircuitEnvelope(method, path), http.StatusOK, nil
	}
	respBody, status, _, err := c.doRequest(ctx, method, path, params, body, headerOverrides)
	return respBody, status, err
}

// doRead is do() without the verify-mode mutating-verb gate, for read-only
// operations that ride a mutating verb on the wire (POST-based search,
// GraphQL queries, JSON-RPC reads).
func (c *Client) doRead(ctx context.Context, method, path string, params map[string]string, body any, headerOverrides map[string]string) (json.RawMessage, int, error) {
	respBody, status, _, err := c.doRequest(ctx, method, path, params, body, headerOverrides)
	return respBody, status, err
}

// isMutatingVerb reports whether the HTTP method writes server state. Used by
// do()'s verify-mode short-circuit to gate dial-out.
func isMutatingVerb(method string) bool {
	switch method {
	case "DELETE", "POST", "PUT", "PATCH":
		return true
	}
	return false
}

// verifyShortCircuitEnvelope returns the synthetic JSON body that stands in
// for a real mutating response when do() short-circuits in verify mode. The
// __pp_verify_synthetic__ sentinel is namespace-reserved so consumers can key
// on one obvious field.
func verifyShortCircuitEnvelope(method, path string) json.RawMessage {
	body, _ := json.Marshal(map[string]any{
		"__pp_verify_synthetic__": true,
		"status":                  "noop",
		"reason":                  "verify_short_circuit",
		"method":                  method,
		"path":                    path,
	})
	return json.RawMessage(body)
}

// GetWithVersion performs a GET and returns the body plus the Zotero
// Last-Modified-Version response header parsed as an int (0 when absent or
// unparseable). Version-based incremental sync needs the response header that
// the cached Get/do path discards. Bypasses the read cache so the caller always
// observes a live value. Live header-reading helpers use the same cancellable
// base context as the public Get wrapper.
func (c *Client) GetWithVersion(path string, params map[string]string) (json.RawMessage, int, error) {
	respBody, _, hdr, err := c.doRequest(c.baseCtx(), "GET", path, params, nil, nil)
	if err != nil {
		return nil, 0, err
	}
	return respBody, parseLastModifiedVersion(hdr), nil
}

// GetWithHeader performs a GET and returns the body plus the trimmed value of the
// named response header (empty when absent). exposes arbitrary response
// headers (e.g. Zotero-Schema-Version) that the cached Get path discards; bypasses
// the read cache like GetWithVersion so the caller observes a live value.
func (c *Client) GetWithHeader(path string, params map[string]string, header string) (json.RawMessage, string, error) {
	respBody, _, hdr, err := c.doRequest(c.baseCtx(), "GET", path, params, nil, nil)
	if err != nil {
		return nil, "", err
	}
	if hdr == nil {
		return respBody, "", nil
	}
	return respBody, strings.TrimSpace(hdr.Get(header)), nil
}

// parseLastModifiedVersion extracts the Zotero Last-Modified-Version header as
// an int, returning 0 when missing or unparseable.
func parseLastModifiedVersion(h http.Header) int {
	if h == nil {
		return 0
	}
	v, err := strconv.Atoi(strings.TrimSpace(h.Get("Last-Modified-Version")))
	if err != nil {
		return 0
	}
	return v
}

// dryRun prints the outgoing request exactly as the live path would send it,
// using the auth material already resolved in `do()`. Never triggers a network
// call — the caller is responsible for passing cached auth material only.
func (c *Client) dryRun(method, targetURL, path string, params map[string]string, body []byte, headerOverrides map[string]string, authHeader string) (json.RawMessage, int, error) {
	fmt.Fprintf(os.Stderr, "%s %s\n", method, targetURL)
	queryPrinted := false
	if params != nil {
		keys := make([]string, 0, len(params))
		for k := range params {
			if params[k] != "" {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			sep := "?"
			if queryPrinted {
				sep = "&"
			}
			fmt.Fprintf(os.Stderr, "  %s%s=%s\n", sep, k, params[k])
			queryPrinted = true
		}
	}
	_ = queryPrinted
	if body != nil {
		var pretty json.RawMessage
		if json.Unmarshal(body, &pretty) == nil {
			enc := json.NewEncoder(os.Stderr)
			enc.SetIndent("  ", "  ")
			fmt.Fprintf(os.Stderr, "  Body:\n")
			_ = enc.Encode(pretty)
		}
	}
	if authHeader != "" {
		fmt.Fprintf(os.Stderr, "  %s: %s\n", "Zotero-API-Key", maskToken(authHeader))
	}
	fmt.Fprintf(os.Stderr, "\n(dry run - no request sent)\n")
	return json.RawMessage(`{"dry_run": true}`), 0, nil
}

func (c *Client) ConfiguredTimeout() time.Duration {
	if c.HTTPClient != nil && c.HTTPClient.Timeout > 0 {
		return c.HTTPClient.Timeout
	}
	return 30 * time.Second
}

func (c *Client) authHeader() (string, error) {
	if c.Config == nil {
		return "", nil
	}
	if c.Config.AccessToken != "" && !c.Config.TokenExpiry.IsZero() && time.Now().After(c.Config.TokenExpiry) && c.Config.RefreshToken != "" {
		if err := c.refreshAccessToken(); err != nil {
			return "", err
		}
	}
	return c.Config.AuthHeader(), nil
}

func (c *Client) refreshAccessToken() error {
	if c.Config == nil || c.Config.RefreshToken == "" {
		return nil
	}
	// zotio authenticates with an API key (Zotero-API-Key header), not OAuth2.
	// There is no OAuth refresh endpoint to call here, so fail loudly instead of
	// silently letting a stale token cause an unexplained 401.
	return fmt.Errorf("token refresh is not supported: zotio uses API-key auth (set ZOTERO_API_KEY)")
}

// sanitizeJSONResponse strips known JSONP/XSSI prefixes and UTF-8 BOM from
// response bodies so that downstream JSON parsing succeeds. For clean JSON
// responses these checks are no-ops.
func sanitizeJSONResponse(body []byte) []byte {
	// UTF-8 BOM
	body = bytes.TrimPrefix(body, []byte("\xEF\xBB\xBF"))

	// JSONP/XSSI prefixes, ordered longest-first where prefixes overlap
	prefixes := [][]byte{
		[]byte(")]}'\n"),
		[]byte(")]}'"),
		[]byte("{}&&"),
		[]byte("for(;;);"),
		[]byte("while(1);"),
	}
	for _, p := range prefixes {
		if bytes.HasPrefix(body, p) {
			body = bytes.TrimPrefix(body, p)
			body = bytes.TrimLeft(body, " \t\r\n")
			break
		}
	}
	return body
}

// maskToken redacts a token for safe display, revealing the last 4 characters
// only when the token is long enough that those 4 chars are a small fraction.
// Short tokens (<12) are fully masked so the visible suffix cannot expose most
// of the secret.
func maskToken(token string) string {
	if token == "" {
		return ""
	}
	if len(token) < 12 {
		return "****"
	}
	return "****" + token[len(token)-4:]
}

func truncateBody(b []byte) string {
	const maxBytes = 4096
	if len(b) <= maxBytes {
		return string(b)
	}
	return strings.ToValidUTF8(string(b[:maxBytes]), "") + "..."
}
