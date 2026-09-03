// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
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
	"sync/atomic"
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

// retryBackoffBaseNanos controls the base for the 5xx exponential backoff
// (1s, 2s, 4s). It is an atomic so concurrent tests can shorten it without
// adding another unsynchronised process-global (see zotio-f13931dc198dc1b5).
var retryBackoffBaseNanos atomic.Int64

func init() {
	retryBackoffBaseNanos.Store(int64(time.Second))
}

func retryBackoffBase() time.Duration {
	return time.Duration(retryBackoffBaseNanos.Load())
}

// SetRetryBackoffBaseForTest lets tests shorten the 5xx retry backoff and the
// 429 Retry-After fallback without sleeping real seconds. It is exported
// because Go has no test-only export and package cli's tests need it; the
// repo already carries this pattern (zoteroprefs.LoadAcrossForTest,
// cliutil.CredentialsFilePath). The returned restore func must be called
// (typically via t.Cleanup) to reset the global for subsequent tests.
func SetRetryBackoffBaseForTest(d time.Duration) (restore func()) {
	prev := retryBackoffBaseNanos.Load()
	retryBackoffBaseNanos.Store(int64(d))
	return func() { retryBackoffBaseNanos.Store(prev) }
}

func sigintContext() context.Context {
	interruptCtxOnce.Do(func() {
		interruptCtx, _ = signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	})
	return interruptCtx
}

// InterruptContext returns the process-wide context cancelled on Ctrl-C/SIGTERM.
// CLI and MCP entry points use it as the root command context so cancellation
// propagates to client HTTP work through cmd.Context().
func InterruptContext() context.Context {
	return sigintContext()
}

type Client struct {
	BaseURL    string
	Config     *config.Config
	HTTPClient *http.Client
	DryRun     bool
	NoCache    bool
	cacheDir   string
	limiter    *cliutil.AdaptiveLimiter
	// base context for wrapper calls; tests may replace it. It is atomic because
	// one Client is shared across sync workers and fanout goroutines while
	// SetContext can rebind it, and CloneForRead hands the same value to further
	// clients. Every other mutable shared field here is already locked or atomic.
	ctx atomic.Pointer[context.Context]
	// WriteBaseURL, when set, receives all non-GET requests while reads continue to
	// use BaseURL — the Zotero local API is read-only, so writes route to the Web
	// API. ResolveWriteBase lazily computes it on the first write (kept in the CLI
	// layer so the client stays generic); writeRouteMu serializes that resolution.
	//
	// ResolveWriteBase must never call back into the same Client. writeRouteMu is
	// held for the whole resolution and is not reentrant, so a resolver that
	// issued a request through this client would deadlock; use a separate HTTP
	// client, as internal/cli's resolveWebWriteBase does. Holding the lock across
	// the round trip is deliberate single-flight: concurrent writes then share one
	// keys/current lookup and one cfg.SaveUserID config save instead of racing N
	// whole-file saves (see ADR-0005 and resolveWebWriteBaseWithoutPersist).
	WriteBaseURL     string
	ResolveWriteBase func(context.Context) (string, error)
	// protect lazy hybrid write-route resolution.
	writeRouteMu       sync.RWMutex
	writeRouteErr      error
	writeRouteWarnOnce sync.Once
	// cacheMu serializes cache invalidation with the final publication of a
	// fetched response. It deliberately does not cover cache reads or HTTP I/O.
	cacheMu sync.Mutex
	// cacheGeneration is the highest generation this process has issued for any
	// cache namespace. It stays a single counter rather than one per namespace
	// because it only gates an optional publication: an in-flight GET whose
	// token predates any local invalidation skips its cache write, and skipping
	// one write for an unrelated resource type costs a re-fetch, not staleness.
	cacheGeneration uint64
	// cachePublishBeforeWrite is a test-only hook run after the shared
	// generation check while the publication lock is held.
	cachePublishBeforeWrite func()
	// cacheWarnOnce ensures a failing response cache warns at most once per
	// client instead of once per uncached GET.
	cacheWarnOnce sync.Once
	// cacheInvalidateWarnOnce warns at most once when post-mutation cache
	// invalidation fails; the mutation still succeeded, so this is not an error.
	cacheInvalidateWarnOnce sync.Once
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

// AmbiguousWriteError means the request reached the transport, but no response
// proves whether the server committed it. Callers must reconcile before retrying.
type AmbiguousWriteError struct {
	Method   string
	Path     string
	Attempts int
	Err      error
}

func (e *AmbiguousWriteError) Error() string {
	return fmt.Sprintf("%s %s write outcome is ambiguous after %d attempt(s): %v", e.Method, e.Path, e.Attempts, e.Err)
}

func (e *AmbiguousWriteError) Unwrap() error {
	return e.Err
}

func IsAmbiguousWriteError(err error) bool {
	var ambiguous *AmbiguousWriteError
	return errors.As(err, &ambiguous)
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

type dispatchTrackingTransport struct {
	base       http.RoundTripper
	dispatched *atomic.Bool
}

func (t dispatchTrackingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.dispatched.Store(true)
	return t.base.RoundTrip(req)
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
	homeDir, homeErr := os.UserHomeDir()
	cacheDir := filepath.Join(homeDir, ".cache", "zotio")
	if homeErr != nil || homeDir == "" {
		fallback := filepath.Join(os.TempDir(), "zotio")
		fmt.Fprintf(os.Stderr, "warning: could not resolve home directory for cache (%v); using %s\n", homeErr, fallback)
		cacheDir = fallback
	}
	httpClient := newHTTPClient(timeout, nil)
	baseURL := sanitizeClientBaseURL(cfg.BaseURL)
	c := &Client{
		BaseURL:    baseURL,
		Config:     cfg,
		HTTPClient: httpClient,
		cacheDir:   cacheDir,
		limiter:    cliutil.NewAdaptiveLimiter(rateLimit),
	}
	c.SetContext(sigintContext())
	return c
}

// CloneForRead returns a read-only client targeting baseURL, sharing the config,
// HTTP client, rate limiter, and cancellation context but with fresh
// synchronization state. A Client must never be copied by value because it holds
// sync.Once values and mutexes; global schema endpoints need the library prefix
// stripped from BaseURL, so clone explicitly instead.
func (c *Client) CloneForRead(baseURL string) *Client {
	clone := &Client{
		BaseURL:    baseURL,
		Config:     c.Config,
		HTTPClient: c.HTTPClient,
		DryRun:     c.DryRun,
		NoCache:    c.NoCache,
		cacheDir:   c.cacheDir,
		limiter:    c.limiter,
	}
	clone.SetContext(c.Context())
	return clone
}

// Context returns the base context used by the signature-stable wrappers. It is
// never nil, so a caller that borrows the context (see SetContext) can always
// restore what it found.
func (c *Client) Context() context.Context {
	return c.baseCtx()
}

func (c *Client) baseCtx() context.Context {
	// tolerate zero-value clients while still giving
	// normal clients a SIGINT/SIGTERM-cancellable context.
	if c == nil {
		return context.Background()
	}
	if ctx := c.ctx.Load(); ctx != nil && *ctx != nil {
		return *ctx
	}
	return context.Background()
}

// SetContext replaces the client's base context used by the signature-stable
// wrappers (Get/Post/...). Entry points pass cmd.Context() so per-command
// deadlines and MCP request cancellation abort in-flight HTTP work, not only
// process interrupts. A nil ctx is ignored, preserving the interrupt default.
//
// The context is owned by whoever built the client. A callee that installs a
// shorter deadline for one route MUST capture Context() first and restore it,
// otherwise it hands the caller back a client bound to a cancelled context.
func (c *Client) SetContext(ctx context.Context) {
	if c == nil || ctx == nil {
		return
	}
	c.ctx.Store(&ctx)
}

// RateLimit returns the current effective rate limit in req/s. Returns 0 if disabled.
func (c *Client) RateLimit() float64 {
	return c.limiter.Rate()
}

// Plane identifies which Zotero API this client reads from. The local desktop API
// and api.zotero.org number object versions independently, so anything persisting
// a version cursor must record which plane issued it.
func (c *Client) Plane() string {
	return c.BaseURL
}

// public wrappers keep their signatures while using the client base context
// (seeded from cmd.Context() via SetContext) so interrupts, per-command
// deadlines, and MCP request cancellation all cancel their HTTP work.
func (c *Client) Get(path string, params map[string]string) (json.RawMessage, error) {
	return c.GetWithHeaders(path, params, nil)
}

func (c *Client) GetWithHeaders(path string, params map[string]string, headers map[string]string) (json.RawMessage, error) {
	return c.getWithHeadersContext(c.baseCtx(), path, params, headers)
}

// GetWithHeadersContext is GetWithHeaders honoring a caller-provided context.
// A nil ctx falls back to the client base context.
func (c *Client) GetWithHeadersContext(ctx context.Context, path string, params map[string]string, headers map[string]string) (json.RawMessage, error) {
	return c.getWithHeadersContext(ctx, path, params, headers)
}

// GetContext is Get honoring a caller-provided context, for callers fanning out
// under a cancellable context (e.g. FanoutRun) that must abort in-flight fetches
// on cancellation. A nil ctx falls back to the client base context.
func (c *Client) GetContext(ctx context.Context, path string, params map[string]string) (json.RawMessage, error) {
	return c.getWithHeadersContext(ctx, path, params, nil)
}

func (c *Client) getWithHeadersContext(ctx context.Context, path string, params map[string]string, headers map[string]string) (json.RawMessage, error) {
	if ctx == nil {
		ctx = c.baseCtx()
	}
	// A path whose resource type this build does not recognize is deliberately
	// uncacheable. Invalidation is per resource type now, so an unclassified
	// entry could only ever be dropped by the unclassified-write full clear; a
	// classified write (PATCH /items) would leave it in place and a later read
	// would serve pre-write bytes. Refusing to cache it costs one re-fetch.
	namespace, classified := cacheNamespace(path)
	cacheable := classified && !c.NoCache && !c.DryRun && c.cacheDir != ""
	var generation cacheGenerationToken
	if cacheable {
		// Capture before either a cache lookup or HTTP work. A mutation advances
		// this namespace's generation before removing its entries, preventing
		// this GET from publishing a response that predates that mutation.
		var snapshotErr error
		generation, snapshotErr = c.cacheGenerationSnapshot(namespace)
		if snapshotErr != nil {
			// A cache-generation marker we cannot read cannot safely coordinate
			// this process with other clients, so bypass this optional cache.
			cacheable = false
		} else if cached, ok := c.readCache(generation, path, params, headers); ok {
			return cached, nil
		}
	}
	result, _, err := c.do(ctx, "GET", path, params, nil, headers)
	if err == nil && cacheable {
		if werr := c.writeCacheAtGeneration(generation, path, params, headers, result); werr != nil {
			c.cacheWarnOnce.Do(func() {
				fmt.Fprintf(os.Stderr, "warning: caching response failed (%v); continuing without response cache\n", werr)
			})
		}
	}
	return result, err
}

// ProbeGet issues a GET and discards the body, returning only the HTTP STATUS
// code. It is for reachability and capability checks (does this endpoint exist,
// is this plane up), where the body is irrelevant and a non-2xx is the answer
// rather than a failure. It bypasses the response cache, since a cached status
// would defeat the purpose.
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
		key += "|param:" + k + "=" + params[k]
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

// cacheInvalidationTargets maps the Zotero resource type a mutation wrote to
// every resource type whose cached reads that write can change. It doubles as
// the namespace allowlist: a leading path segment that is not a key here is
// unclassified.
//
// The map was derived by enumerating it in the direction that finds misses:
// for each cached READ, which writes can change its body — including a write
// made under a different library prefix. Asking the opposite question, what
// each write touches, hid two edges (`collections` -> `searches` and
// `items` -> `groups`) through a whole review pass. When an edge is arguable,
// it belongs in the map: over-invalidation costs one re-fetch of a cache that
// is regenerable by definition, while under-invalidation serves pre-write data.
//
// Read by read, with the write that reaches it:
//
//   - `/items*` (also `/items/<k>/children`, `/tags`, `/fulltext`): item
//     writes; collection writes, because membership is an item field; tag
//     writes, because `tags rename` and `tags delete` rewrite the owning items.
//   - `/collections*` (also `/collections/<k>/items`, `/collections/<k>/tags`):
//     collection writes; item writes, which change a collection's item list and
//     its `meta.numItems`; tag writes, which change the collection's tag list.
//   - `/tags`: tag writes; item writes, because zotio writes tags by PATCHing
//     the owning item (`internal/cli/items_tags_write.go`, `tags_rename.go`),
//     so an item write changes the library tag vocabulary.
//   - `/searches/<k>/items`: search writes; item and tag writes, because saved
//     searches are recomputed from item state; collection writes, because a
//     search scoped to a collection follows that collection and its recursive
//     descendants, so `collections delete` changes what `searches run` returns.
//   - `/groups` under the user prefix: item writes. This is the shape that hides
//     misses, because the written path and the stale read live under different
//     library prefixes: `items create --group G` writes `/items` with the base
//     URL rewritten to `/groups/G` (see internal/cli/helpers.go), while the
//     stale read is `/groups` under `/users/<id>`, whose `meta.numItems` counts
//     those items. Cache keys hash the base URL but namespace directories do
//     not, so clearing a namespace clears it for every library at once, which
//     is exactly what this edge needs.
//   - `/deleted`: every deletion. Zotero's delete log spans items, collections,
//     searches, tags and settings, so all five clear it.
//   - `/publications/items`: item writes; it is a filtered item projection.
//   - `/settings`: settings writes.
//   - `/keys/current` and the schema endpoints (`/itemTypes`, `/itemFields`,
//     ...): nothing zotio writes changes them. Key metadata is account state,
//     and the schema endpoints are global Zotero metadata outside the library,
//     so they clear only themselves.
//
// Every entry must list itself: invalidateCache uses this list verbatim.
var cacheInvalidationTargets = map[string][]string{
	"items":                {"items", "collections", "tags", "searches", "fulltext", "publications", "groups", "deleted"},
	"collections":          {"collections", "items", "searches", "deleted"},
	"tags":                 {"tags", "items", "collections", "searches", "deleted"},
	"searches":             {"searches", "deleted"},
	"fulltext":             {"fulltext", "items"},
	"publications":         {"publications", "items"},
	"settings":             {"settings", "deleted"},
	"deleted":              {"deleted"},
	"groups":               {"groups"},
	"keys":                 {"keys"},
	"itemTypes":            {"itemTypes"},
	"itemFields":           {"itemFields"},
	"itemTypeFields":       {"itemTypeFields"},
	"itemTypeCreatorTypes": {"itemTypeCreatorTypes"},
	"creatorFields":        {"creatorFields"},
}

// cacheNamespace derives a request's cache namespace from the leading segment
// of its path, which is the Zotero resource type: /items/ABCD/children yields
// "items", /tags yields "tags". It reports false for a segment absent from
// cacheInvalidationTargets.
//
// The set is an allowlist rather than "whatever the first segment happens to
// be" for three reasons: the dependency graph above is only defined for types
// this build knows; an unknown type has no safe invalidation scope, only a full
// clear; and an allowlist keeps the segment usable as a directory name, with no
// path traversal, no reserved Windows name, and no two namespaces colliding
// under the case-insensitive filesystems on macOS and Windows.
func cacheNamespace(path string) (string, bool) {
	segment := strings.TrimPrefix(path, "/")
	if end := strings.IndexAny(segment, "/?#"); end >= 0 {
		segment = segment[:end]
	}
	if _, known := cacheInvalidationTargets[segment]; !known {
		return "", false
	}
	return segment, true
}

// cacheFilePath locates one cached response. The namespace subdirectory is what
// makes selective invalidation possible at all: cacheKey is a truncated sha256
// of path, base URL, auth, params and headers, so the resource type cannot be
// recovered from the file name — it has to be recorded in the layout.
func (c *Client) cacheFilePath(namespace, path string, params map[string]string, headers map[string]string) string {
	return filepath.Join(c.cacheDir, namespace, c.cacheKey(path, params, headers)+".json")
}

// cacheEntryVersion prefixes every cache file. A cached response is framed as
// "<version> <generation> <length> <crc32c>\n<body>".
//
// The generation tag is what closes the invalidation race: invalidateCache
// publishes the new generation before it removes any file, and readers take no
// publication lock, so a GET can snapshot the new generation and still find a
// file written before the mutation — inside the RemoveAll window, or forever if
// the RemoveAll partly failed. An entry's mtime and JSON validity cannot detect
// that; a recorded generation can.
//
// Widening the publication lock to cover readers was rejected: it would
// serialize every read behind every writer to protect a window one RemoveAll
// long, and it still would not cover a partly failed RemoveAll, whose stale
// files outlive the lock.
//
// The length and checksum make a damaged file fail closed. A cache hit skips
// HTTP entirely and its bytes are returned to the caller AS the API response,
// so a frame whose header parses but whose body is short — say
// `zotio-cache/1 3 512 …` over `[{"key":"A"` — would render a truncated library
// and report success. Nothing else on the read path can catch that: the atomic
// rename protects against a torn write by this build, but the marker publication
// deliberately does not fsync, so a crash or a full disk can still leave a
// renamed file whose bytes are not the ones we wrote. The length catches
// truncation and extension deterministically; crc32c (hardware-accelerated on
// both release architectures) catches same-length corruption for one pass over
// bytes the caller is about to parse anyway. Validating the body as JSON instead
// would cost a full parse and still accept a fragment that happens to parse.
//
// The frame is a line prefix instead of a JSON envelope so the read path can
// return the body as a subslice of the file bytes. A large item list is then
// neither re-encoded on write nor unmarshaled and re-marshaled on read, and the
// bytes a caller sees stay byte-identical to the live response, which a
// json.RawMessage round trip would silently compact.
const cacheEntryVersion = "zotio-cache/1"

// cacheEntryChecksum is Castagnoli CRC-32, the polynomial with SSE 4.2 and
// ARMv8 CRC instruction support, so the integrity check costs a single fast
// pass rather than a second JSON parse.
var cacheEntryChecksum = crc32.MakeTable(crc32.Castagnoli)

func encodeCacheEntry(generation uint64, body json.RawMessage) []byte {
	header := cacheEntryVersion + " " + strconv.FormatUint(generation, 10) +
		" " + strconv.Itoa(len(body)) +
		" " + strconv.FormatUint(uint64(crc32.Checksum(body, cacheEntryChecksum)), 10) + "\n"
	framed := make([]byte, 0, len(header)+len(body))
	framed = append(framed, header...)
	return append(framed, body...)
}

// decodeCacheEntry splits a cache file into its generation tag and body,
// reporting false for anything this build did not write byte for byte.
func decodeCacheEntry(data []byte) (uint64, json.RawMessage, bool) {
	newline := bytes.IndexByte(data, '\n')
	if newline < 0 {
		return 0, nil, false
	}
	fields := strings.Split(string(data[:newline]), " ")
	if len(fields) != 4 || fields[0] != cacheEntryVersion {
		return 0, nil, false
	}
	generation, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0, nil, false
	}
	length, err := strconv.Atoi(fields[2])
	if err != nil {
		return 0, nil, false
	}
	checksum, err := strconv.ParseUint(fields[3], 10, 32)
	if err != nil {
		return 0, nil, false
	}
	body := data[newline+1:]
	if len(body) == 0 || len(body) != length {
		return 0, nil, false
	}
	if crc32.Checksum(body, cacheEntryChecksum) != uint32(checksum) {
		return 0, nil, false
	}
	return generation, json.RawMessage(body), true
}

func (c *Client) readCache(generation cacheGenerationToken, path string, params map[string]string, headers map[string]string) (json.RawMessage, bool) {
	cacheFile := c.cacheFilePath(generation.namespace, path, params, headers)
	info, err := os.Stat(cacheFile)
	if err != nil {
		return nil, false
	}
	if time.Since(info.ModTime()) > 5*time.Minute {
		_ = os.Remove(cacheFile)
		return nil, false
	}
	data, err := os.ReadFile(cacheFile)
	if err != nil {
		return nil, false
	}
	entryGeneration, body, ok := decodeCacheEntry(data)
	if !ok {
		return nil, false
	}
	if entryGeneration < generation.marker {
		// Written before the last invalidation of this namespace. That the file
		// still exists means this read either raced the removal or is looking at
		// a removal that failed; either way the bytes predate the mutation.
		return nil, false
	}
	return body, true
}

// cacheGenerationToken is the snapshot a GET takes before it looks at the
// cache: which namespace it reads, the process-shared generation of that
// namespace, and this process's own counter.
type cacheGenerationToken struct {
	namespace string
	memory    uint64
	marker    uint64
}

func (c *Client) cacheGenerationMarkerPath() string {
	return c.cacheDir + ".generation"
}

func (c *Client) cachePublicationLockPath() string {
	return c.cacheDir + ".publish.lock"
}

// readCacheGenerations reads the process-shared generation marker: a JSON
// object mapping cache namespace to its current generation. Unknown keys are
// preserved by callers rewriting the whole map, so a namespace this build does
// not know cannot have its counter rolled back.
func (c *Client) readCacheGenerations() (map[string]uint64, error) {
	data, err := os.ReadFile(c.cacheGenerationMarkerPath())
	if errors.Is(err, os.ErrNotExist) {
		return map[string]uint64{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading cache generation marker: %w", err)
	}
	trimmed := strings.TrimSpace(string(data))
	generations := map[string]uint64{}
	jsonErr := json.Unmarshal([]byte(trimmed), &generations)
	if jsonErr == nil {
		if generations == nil {
			// Top-level JSON `null` unmarshals into a nil map without error, and
			// a nil map panics on assignment in invalidateCache — a damaged
			// marker would crash every write, not just fail one. Treat it as
			// unreadable so callers take their fail-closed path.
			return nil, fmt.Errorf("parsing cache generation marker: %s holds JSON null", c.cacheGenerationMarkerPath())
		}
		return generations, nil
	}
	// A marker written before the cache was namespaced holds a single decimal
	// counter. Read it as a floor for every namespace instead of rejecting it:
	// an unreadable marker disables the response cache for the whole install
	// and makes every mutation warn, and the read path cannot repair the file
	// because it holds no publication lock. Only the counter has to carry over
	// — pre-namespace entries are flat files under cacheDir that this build
	// never looks at.
	legacy, legacyErr := strconv.ParseUint(trimmed, 10, 64)
	if legacyErr != nil {
		return nil, fmt.Errorf("parsing cache generation marker: %w", errors.Join(jsonErr, legacyErr))
	}
	generations = make(map[string]uint64, len(cacheInvalidationTargets))
	for namespace := range cacheInvalidationTargets {
		generations[namespace] = legacy
	}
	return generations, nil
}

func (c *Client) cacheGenerationSnapshot(namespace string) (cacheGenerationToken, error) {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()

	generations, err := c.readCacheGenerations()
	if err != nil {
		return cacheGenerationToken{}, err
	}
	return cacheGenerationToken{namespace: namespace, memory: c.cacheGeneration, marker: generations[namespace]}, nil
}

func (c *Client) acquireCachePublicationLock(operation string, wait time.Duration) (*cliutil.WriterLock, error) {
	deadline := time.Now().Add(wait)
	for {
		lock, err := cliutil.AcquireWriterLock(c.cachePublicationLockPath(), operation)
		if err == nil {
			return lock, nil
		}
		var busy *cliutil.WriterLockBusyError
		if !errors.As(err, &busy) || wait <= 0 || !time.Now().Before(deadline) {
			return nil, err
		}
		time.Sleep(time.Millisecond)
	}
}

func (c *Client) writeCacheAtGeneration(generation cacheGenerationToken, path string, params map[string]string, headers map[string]string, data json.RawMessage) error {
	if generation.namespace == "" {
		// An entry written flat into cacheDir is exactly the file layout a
		// pre-namespace zotio reads, so it would hand this framed body to a
		// caller as an API response, and no selective invalidation could reach
		// it. Callers derive the namespace before deciding a GET is cacheable;
		// an empty one is a programming error, not a cache miss to swallow.
		return fmt.Errorf("caching %s: no cache namespace in generation token", path)
	}
	// Never cache an empty list. zotio reads the local desktop API while writes
	// route to api.zotero.org, so for a few seconds after a write a filtered
	// query legitimately returns nothing — and caching that pinned the emptiness
	// for the full TTL, long after the read plane had caught up. An empty result
	// is also the cheapest possible re-fetch, so there is nothing to protect.
	if isEmptyJSONList(data) {
		return nil
	}
	// Chmod as well as MkdirAll: cached Zotero API payloads contain private
	// library metadata, so keep the directory and files private even when they
	// already existed with older world-readable permissions. Both levels are
	// chmodded because MkdirAll leaves an existing directory's mode alone and
	// cacheDir can predate this build.
	//
	// Directory preparation is intentionally outside cacheMu. The mutex only
	// protects the generation check and atomic file publication; it never
	// serializes network requests.
	namespaceDir := filepath.Join(c.cacheDir, generation.namespace)
	if err := os.MkdirAll(namespaceDir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(c.cacheDir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(namespaceDir, 0o700); err != nil {
		return err
	}
	cacheFile := c.cacheFilePath(generation.namespace, path, params, headers)

	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	if generation.memory != c.cacheGeneration {
		return nil
	}

	lock, err := c.acquireCachePublicationLock("publishing response cache", 0)
	if err != nil {
		var busy *cliutil.WriterLockBusyError
		if errors.As(err, &busy) {
			// Cache publication is optional; leave the successful GET uncached
			// rather than making it wait behind another process's publication.
			return nil
		}
		return err
	}
	generations, markerErr := c.readCacheGenerations()
	if markerErr != nil {
		return errors.Join(markerErr, lock.Release())
	}
	marker := generations[generation.namespace]
	if generation.marker != marker {
		return lock.Release()
	}
	// Hold both locks through rename so neither a local nor another process's
	// invalidation can advance the marker between this check and publication.
	if c.cachePublishBeforeWrite != nil {
		c.cachePublishBeforeWrite()
	}
	writeErr := cliutil.AtomicWriteFile(cacheFile, encodeCacheEntry(marker, data), 0o600, 0o700)
	return errors.Join(writeErr, lock.Release())
}

// isEmptyJSONList reports whether a response body is an empty JSON array,
// ignoring surrounding whitespace.
func isEmptyJSONList(data json.RawMessage) bool {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) < 2 || trimmed[0] != '[' {
		return false
	}
	return len(bytes.TrimSpace(trimmed[1:len(trimmed)-1])) == 0 && trimmed[len(trimmed)-1] == ']'
}

// invalidateCache drops the cached reads that a mutation of path can have
// changed, so the next read cannot return a pre-mutation snapshot.
//
// The rule: clear the mutation's own resource type plus the resource types
// whose reads project its state, per cacheInvalidationTargets. A path whose
// resource type this build does not recognize clears the whole cache directory
// and advances every namespace — under-invalidating an unclassified write would
// silently serve pre-write data, which is far worse than dropping a cache that
// is regenerable by definition. Note the scale of the item edge set: an item
// write clears almost every library namespace, because tags, collection
// membership, search results and fulltext all live on items. The selectivity
// pays off on non-item writes, which leave unrelated resource types cached.
//
// This replaced a wholesale clear of the entire cache on every mutation, which
// the opaque sha256 cache keys used to force; entries now live under a
// subdirectory named for their resource type, so the type is recoverable from
// the layout without decoding a key.
//
// Only the namespaces being cleared advance their generation. A single global
// counter would strand an untouched resource type's entries behind an
// advanced marker, turning every selective clear into a full one.
func (c *Client) invalidateCache(path string) error {
	if c.cacheDir == "" {
		return nil
	}

	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()

	namespace, classified := cacheNamespace(path)
	targets := cacheInvalidationTargets[namespace]
	if !classified {
		targets = make([]string, 0, len(cacheInvalidationTargets))
		for known := range cacheInvalidationTargets {
			targets = append(targets, known)
		}
	}

	lock, err := c.acquireCachePublicationLock("invalidating response cache", 250*time.Millisecond)
	if err != nil {
		return err
	}
	generations, markerErr := c.readCacheGenerations()
	if markerErr != nil {
		// Fail closed. A marker this process cannot read cannot be advanced, so
		// nothing would stop a later read from serving a pre-mutation entry;
		// remove the whole cache directory instead of leaving entries behind a
		// marker nobody can interpret. The marker itself is deliberately NOT
		// repaired: rewriting it with a counter this process invented could
		// publish a value below one already recorded in a surviving entry, and
		// that entry would then pass the generation check. Leaving it unreadable
		// keeps every other client bypassing the cache instead. The error still
		// propagates, so the caller warns.
		return errors.Join(markerErr, os.RemoveAll(c.cacheDir), lock.Release())
	}
	// One generation above every counter this process has issued and every
	// counter the targets currently hold, so a marker that somehow went
	// backwards cannot let this process reissue a generation whose entries are
	// still on disk.
	next := c.cacheGeneration
	for _, target := range targets {
		if generations[target] > next {
			next = generations[target]
		}
	}
	next++
	for _, target := range targets {
		generations[target] = next
	}
	marker, markerErr := json.Marshal(generations)
	if markerErr != nil {
		return errors.Join(markerErr, lock.Release())
	}
	// AtomicWriteFile intentionally does not fsync; this marker only prevents
	// stale cache publication and the response cache is regenerable.
	if markerErr = cliutil.AtomicWriteFile(c.cacheGenerationMarkerPath(), marker, 0o600, 0o700); markerErr != nil {
		return errors.Join(markerErr, lock.Release())
	}
	// Advance in memory after publishing the process-shared marker and before
	// removal. GETs holding the prior token must skip their later writes.
	c.cacheGeneration = next
	if !classified {
		return errors.Join(os.RemoveAll(c.cacheDir), lock.Release())
	}
	var removeErr error
	for _, target := range targets {
		removeErr = errors.Join(removeErr, os.RemoveAll(filepath.Join(c.cacheDir, target)))
	}
	c.removeLegacyFlatCacheEntries()
	return errors.Join(removeErr, lock.Release())
}

// removeLegacyFlatCacheEntries deletes the pre-namespace cache layout: entries
// written directly into cacheDir as <key>.json, before entries moved one level
// down into a per-resource-type subdirectory.
//
// They have to be swept explicitly. This build never reads them, so their TTL
// removal in readCache never runs, and a selective invalidation only removes
// namespace subdirectories — so after an upgrade a flat entry for every read
// the user had cached would sit in ~/.cache/zotio until an unclassified write
// happened to force a full clear, or forever. Sweeping here bounds it: the
// first mutation after the upgrade clears them, and every later sweep finds an
// empty listing. Only regular top-level *.json files match, which the new
// layout never creates; the generation marker and publication lock are
// siblings of cacheDir, not entries inside it.
//
// Errors are deliberately ignored rather than joined into the invalidation
// result. These files are unreachable, so failing to delete one is not a
// staleness risk, and reporting it would raise the caller's "a later read may
// return stale data" warning for a housekeeping problem.
func (c *Client) removeLegacyFlatCacheEntries() {
	entries, err := os.ReadDir(c.cacheDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		_ = os.Remove(filepath.Join(c.cacheDir, entry.Name()))
	}
}

// RawBody carries a pre-encoded request payload with an explicit content type.
// doRequest sends it verbatim instead of JSON-marshaling it, for endpoints that
// consume non-JSON bodies (e.g. Zotero's form-encoded file-upload protocol).
type RawBody struct {
	ContentType string
	Data        []byte
}

// The mutating verbs below all return (body, status, error), where the int is
// the HTTP STATUS CODE the server replied with.
//
// That is worth stating because this type also exposes reads returning
// (body, int, error) where the int is a Zotero OBJECT VERSION, not a status:
// GetWithVersion and GetFromWriteBaseWithVersion. The two shapes are identical
// to the compiler, so a caller that confuses them type-checks cleanly and then
// misreads the number - a version of 0 looks like "no status", and a high
// version looks like nothing at all. Read the method you are calling.
//
// Status is returned rather than folded into the error because a Zotero write
// can fail meaningfully with a body: 412 carries the conflict, 409 the target
// state, and 200 with a per-object failures map is a partial success.

// Post sends a JSON body. The int is the HTTP status code.
func (c *Client) Post(path string, body any) (json.RawMessage, int, error) {
	return c.do(c.baseCtx(), "POST", path, nil, body, nil)
}

// PostFormWithHeaders sends application/x-www-form-urlencoded values, for the
// Zotero file-upload protocol endpoints that reject JSON bodies. The int is the
// HTTP status code.
func (c *Client) PostFormWithHeaders(path string, form url.Values, headers map[string]string) (json.RawMessage, int, error) {
	body := RawBody{ContentType: "application/x-www-form-urlencoded", Data: []byte(form.Encode())}
	return c.do(c.baseCtx(), "POST", path, nil, body, headers)
}

// PostWithHeaders sends a JSON body with per-request headers. The int is the
// HTTP status code.
func (c *Client) PostWithHeaders(path string, body any, headers map[string]string) (json.RawMessage, int, error) {
	return c.do(c.baseCtx(), "POST", path, nil, body, headers)
}

// Delete removes the object at path. The int is the HTTP status code.
func (c *Client) Delete(path string) (json.RawMessage, int, error) {
	return c.do(c.baseCtx(), "DELETE", path, nil, nil, nil)
}

// DeleteWithHeaders removes the object at path, carrying per-request headers
// such as If-Unmodified-Since-Version. The int is the HTTP status code.
func (c *Client) DeleteWithHeaders(path string, headers map[string]string) (json.RawMessage, int, error) {
	return c.do(c.baseCtx(), "DELETE", path, nil, nil, headers)
}

// Put replaces the object at path. The int is the HTTP status code.
func (c *Client) Put(path string, body any) (json.RawMessage, int, error) {
	return c.do(c.baseCtx(), "PUT", path, nil, body, nil)
}

// PutWithHeaders replaces the object at path, carrying per-request headers.
// The int is the HTTP status code.
func (c *Client) PutWithHeaders(path string, body any, headers map[string]string) (json.RawMessage, int, error) {
	return c.do(c.baseCtx(), "PUT", path, nil, body, headers)
}

// Patch updates fields of the object at path. The int is the HTTP status code.
func (c *Client) Patch(path string, body any) (json.RawMessage, int, error) {
	return c.do(c.baseCtx(), "PATCH", path, nil, body, nil)
}

// PatchWithHeaders updates fields of the object at path, carrying per-request
// headers such as If-Unmodified-Since-Version. The int is the HTTP status code.
func (c *Client) PatchWithHeaders(path string, body any, headers map[string]string) (json.RawMessage, int, error) {
	return c.do(c.baseCtx(), "PATCH", path, nil, body, headers)
}

// do executes an HTTP request. headerOverrides, when non-nil, override global
// RequiredHeaders for this specific request (used for per-endpoint API versioning).
func (c *Client) doRequest(ctx context.Context, method, path string, params map[string]string, body any, headerOverrides map[string]string) (json.RawMessage, int, http.Header, error) {
	return c.doRequestOnBase(ctx, "", method, path, params, body, headerOverrides)
}

// doRequestOnBase is doRequest against an explicit base URL. An empty
// baseOverride keeps the normal read/write classification in baseURLFor; only a
// caller that must reach a specific plane (see GetFromWriteBaseWithVersion)
// passes one.
func (c *Client) doRequestOnBase(ctx context.Context, baseOverride, method, path string, params map[string]string, body any, headerOverrides map[string]string) (_ json.RawMessage, _ int, _ http.Header, retErr error) {
	// all network construction below requires a
	// non-nil context so callers can cancel request creation, dialing, and reads.
	if ctx == nil {
		ctx = context.Background()
	}
	// A mutating request that fails after dispatch may still have committed
	// server-side (a retried 5xx whose success response was lost, a write-token
	// replay 412, or a dropped response), so drop cached reads on any error:
	// reconciliation re-reads must observe fresh state. Harmless on the rare
	// pre-dispatch failure — it only forces a cache miss on the next read.
	mutationSucceeded := false
	defer func() {
		if retErr != nil && !mutationSucceeded && method != http.MethodGet && !c.DryRun {
			if err := c.invalidateCache(path); err != nil {
				retErr = errors.Join(retErr, err)
			}
		}
	}()
	base := baseOverride
	if base == "" {
		base = c.baseURLFor(ctx, method)
		// If write-route resolution failed but the base is still the local
		// read plane, surface the resolver error when the local API rejects
		// the write, so the user sees the real cause (expired key, network
		// failure resolving keys/current) instead of "local API is read-only".
		// The write still goes to the local API so a transient resolver error
		// (context deadline) can be retried on the next write per the contract.
	}
	targetURL := base + path
	var bodyBytes []byte
	bodyContentType := "application/json"
	if body != nil {
		if raw, ok := body.(RawBody); ok {
			bodyBytes = raw.Data
			if raw.ContentType != "" {
				bodyContentType = raw.ContentType
			}
		} else {
			b, err := json.Marshal(body)
			if err != nil {
				return nil, 0, nil, fmt.Errorf("marshaling body: %w", err)
			}
			bodyBytes = b
		}
	}

	// Resolve auth material before the dry-run branch so --dry-run can preview
	// exactly what would be sent. Uses only cached credentials; a token that
	// requires a network refresh will be re-fetched on the live request path,
	// not during dry-run.
	authHeader, err := c.authHeader()
	if err != nil {
		return nil, 0, nil, err
	}

	// Build the request for dry-run display or actual execution.
	//
	// --dry-run means "do not CHANGE anything on the server", not "do not
	// TALK to the server". Only mutating verbs take the short-circuit below;
	// GET/HEAD always execute live. A preview command (e.g. "vault resolve")
	// reads current state before describing a would-be mutation, and that
	// read has to be real or the preview describes a fiction. This also
	// means a read error under --dry-run (a 404 for something deleted
	// upstream, a 500, ...) surfaces normally instead of being swallowed by
	// a fabricated always-success sentinel — which is exactly the property a
	// preview needs to detect remote drift. Reuses baseURLFor's read/write
	// classification (isMutatingMethod) rather than a second, divergent list.
	//
	// Because GET/HEAD never take this branch anymore, Get/GetWithHeaders/
	// GetWithHeadersContext (which discard the status int) can no longer
	// receive the synthetic dry-run body at all, so the historical hazard of
	// a fabricated response being indistinguishable from a real one through
	// those callers is closed by construction, not by a status-code
	// convention those callers would have to opt into.
	if c.DryRun && isMutatingMethod(method) {
		respBody, status, derr := c.dryRun(method, targetURL, path, params, bodyBytes, headerOverrides, authHeader)
		return respBody, status, nil, derr
	}

	const maxRetries = 3
	var lastErr error
	var ambiguousFailures []error
	wrapAmbiguous := func(attempts int, final error) error {
		if !isMutatingMethod(method) || len(ambiguousFailures) == 0 {
			return final
		}
		causes := make([]error, 0, len(ambiguousFailures)+1)
		if final != nil {
			causes = append(causes, final)
		}
		causes = append(causes, ambiguousFailures...)
		return &AmbiguousWriteError{
			Method: method, Path: path, Attempts: attempts, Err: errors.Join(causes...),
		}
	}

	for attempt := 0; attempt <= maxRetries; attempt++ {
		// proactive rate limiting must honor context
		// cancellation before dialing.
		c.limiter.WaitContext(ctx)
		if err := ctx.Err(); err != nil {
			return nil, 0, nil, wrapAmbiguous(attempt, err)
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
			req.Header.Set("Content-Type", bodyContentType)
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
		// Rehearsal affordance: force the precondition stale so a live run can
		// provoke Zotero's own 412 and exercise the conflict contract end to
		// end. Applied here, after headerOverrides, because this is the single
		// point every write's If-Unmodified-Since-Version passes through — and
		// only when the request already carries one, so it can never ADD a
		// precondition to a write that deliberately omits it, nor turn an
		// unguarded write into a guarded one. No-op unless
		// ZOTIO_TEST_STALE_VERSION is set. See cliutil.StaleVersionEnvVar.
		if req.Header.Get("If-Unmodified-Since-Version") != "" {
			if stale, ok := cliutil.StaleVersionOverride(); ok {
				req.Header.Set("If-Unmodified-Since-Version", strconv.Itoa(stale))
			}
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
		if err := ctx.Err(); err != nil {
			return nil, 0, nil, wrapAmbiguous(attempt, err)
		}
		// A conditional precondition makes a write at-most-once, but does not
		// make an ambiguous response safe to replay. Transport and 5xx retries
		// therefore require GET/HEAD or Zotero's write token, whose endpoint
		// owner can reconcile the token-replay response. An explicit 429 is not
		// ambiguous, so conditional requests can still retry that response.
		ambiguousRetrySafe := method == http.MethodGet || method == http.MethodHead ||
			req.Header.Get("Zotero-Write-Token") != ""
		rateLimitRetrySafe := ambiguousRetrySafe ||
			req.Header.Get("If-Unmodified-Since-Version") != "" ||
			req.Header.Get("If-Match") != "" ||
			req.Header.Get("If-None-Match") != ""
		httpClient := c.requestHTTPClient()
		var dispatched atomic.Bool
		transport := httpClient.Transport
		if transport == nil {
			transport = http.DefaultTransport
		}
		httpClient.Transport = dispatchTrackingTransport{base: transport, dispatched: &dispatched}
		resp, err := httpClient.Do(req)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				if isMutatingMethod(method) && dispatched.Load() {
					return nil, 0, nil, &AmbiguousWriteError{
						Method: method, Path: path, Attempts: attempt + 1,
						Err: fmt.Errorf("%s %s: %w", method, path, ctxErr),
					}
				}
				return nil, 0, nil, fmt.Errorf("%s %s: %w", method, path, ctxErr)
			}
			lastErr = fmt.Errorf("%s %s: %w", method, path, err)
			if isMutatingMethod(method) {
				ambiguousFailures = append(ambiguousFailures, lastErr)
			}
			if !ambiguousRetrySafe {
				if isMutatingMethod(method) {
					return nil, 0, nil, &AmbiguousWriteError{Method: method, Path: path, Attempts: attempt + 1, Err: lastErr}
				}
				return nil, 0, nil, lastErr
			}
			continue
		}

		// cap API response bodies
		// before buffering them for cache/error handling.
		respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxZoteroResponseBytes+1))
		resp.Body.Close()
		if err != nil {
			readErr := fmt.Errorf("reading response: %w", err)
			if isMutatingMethod(method) {
				return nil, 0, nil, &AmbiguousWriteError{Method: method, Path: path, Attempts: attempt + 1, Err: readErr}
			}
			return nil, 0, nil, readErr
		}
		if int64(len(respBody)) > maxZoteroResponseBytes {
			limitErr := fmt.Errorf("response exceeded %d bytes", maxZoteroResponseBytes)
			if isMutatingMethod(method) {
				return nil, 0, nil, &AmbiguousWriteError{Method: method, Path: path, Attempts: attempt + 1, Err: limitErr}
			}
			return nil, 0, nil, limitErr
		}
		respBody = sanitizeJSONResponse(respBody)

		// Only 2xx responses are successful. In particular, a caller's
		// ErrUseLastResponse must not turn a refused redirect into success.
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			c.limiter.OnSuccess()
			if method != http.MethodGet && !c.DryRun {
				mutationSucceeded = true
				if ierr := c.invalidateCache(path); ierr != nil {
					// The mutation applied. A failed cache invalidation must NOT
					// be returned as an error: callers check err before status and
					// would treat the successful write as failed, risking a retry
					// that duplicates a create. Warn once (de-silencing the stale-
					// cache risk) and return success.
					c.cacheInvalidateWarnOnce.Do(func() {
						fmt.Fprintf(os.Stderr, "warning: cache invalidation after successful %s %s failed (%v); a later read may return stale data until the cache at %s is cleared\n", method, path, ierr, c.cacheDir)
					})
				}
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
		if rateLimitRetrySafe && resp.StatusCode == 429 && attempt < maxRetries {
			c.limiter.OnRateLimit()
			wait, synthetic := cliutil.RetryAfterOrFallback(resp)
			// A server-supplied Retry-After is honoured exactly as sent. Only
			// the synthetic 5s fallback (missing, unparseable or non-positive
			// header) is ours to scale, so tests that hit it pay 5x the short
			// base instead of five real seconds. Production is unchanged: the
			// default base is 1s, so the fallback stays 5s.
			if synthetic {
				wait = 5 * retryBackoffBase()
			}
			fmt.Fprintf(os.Stderr, "rate limited, waiting %s (attempt %d/%d, rate adjusted to %.1f req/s)\n", wait, attempt+1, maxRetries, c.limiter.Rate())
			if err := sleepWithContext(ctx, wait); err != nil {
				return nil, 0, nil, wrapAmbiguous(attempt+1, err)
			}
			lastErr = apiErr
			continue
		}

		// Server error - retry with backoff. 501 Not Implemented is never transient
		// (e.g. writes against the read-only Zotero local API), so don't retry it.
		// avoid a pointless 3x backoff storm on local-API write rejections.
		if ambiguousRetrySafe && resp.StatusCode >= 500 && resp.StatusCode != 501 && attempt < maxRetries {
			lastErr = apiErr
			if isMutatingMethod(method) {
				ambiguousFailures = append(ambiguousFailures, apiErr)
			}
			wait := time.Duration(math.Pow(2, float64(attempt))) * retryBackoffBase()
			fmt.Fprintf(os.Stderr, "server error %d, retrying in %s (attempt %d/%d)\n", resp.StatusCode, wait, attempt+1, maxRetries)
			if err := sleepWithContext(ctx, wait); err != nil {
				return nil, 0, nil, wrapAmbiguous(attempt+1, err)
			}
			continue
		}

		// Client error or retries exhausted - return the error. When the write
		// was routed to the local API only because write-route resolution failed,
		// wrap the local rejection with the resolver error so the diagnosis names
		// the real cause instead of "local API is read-only".
		if isLocalWriteRejection(apiErr.Body) {
			c.writeRouteMu.RLock()
			routeErr := c.writeRouteErr
			hasRoute := c.WriteBaseURL != ""
			c.writeRouteMu.RUnlock()
			if !hasRoute && routeErr != nil && base == c.BaseURL {
				return nil, resp.StatusCode, resp.Header, fmt.Errorf("could not resolve Zotero Web API write route: %w: %w", routeErr, apiErr)
			}
		}
		// wrapAmbiguous returns apiErr unchanged when this request has no
		// ambiguous prior attempt, so one return covers both cases. Comparing
		// the two values instead would be an error identity test.
		return nil, resp.StatusCode, resp.Header, wrapAmbiguous(attempt+1, apiErr)
	}

	return nil, 0, nil, wrapAmbiguous(maxRetries+1, lastErr)
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
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

// isMutatingMethod reports whether method changes server state. GET and HEAD
// are the only safe verbs in play here; every other verb (POST, PUT, PATCH,
// DELETE, ...) mutates and must be routed to the write base URL and, under
// --dry-run, short-circuited instead of dispatched. Single source of truth
// for that classification — baseURLFor's write routing and doRequest's
// dry-run gate both defer to it instead of keeping their own lists in sync.
func isMutatingMethod(method string) bool {
	return method != http.MethodGet && method != http.MethodHead
}

func isLocalWriteRejection(body string) bool {
	return strings.Contains(body, "Endpoint does not support method") ||
		strings.Contains(body, "Method not implemented")
}

// baseURLFor returns the base URL for a request: writes (non-GET) route to the
// resolved WriteBaseURL when hybrid routing is configured; reads use BaseURL. The
// write base is resolved lazily on first use.
func (c *Client) baseURLFor(ctx context.Context, method string) string {
	if !isMutatingMethod(method) {
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

// resolveWriteRoute runs the CLI-provided write-base resolver until it succeeds.
// On success it sets WriteBaseURL and prints a one-time notice; on failure it
// leaves WriteBaseURL empty so a later write retries resolution instead of
// permanently latching the read-only fallback.
func (c *Client) resolveWriteRoute(ctx context.Context) {
	if c.ResolveWriteBase == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// Fast path: already resolved.
	c.writeRouteMu.RLock()
	resolved := c.WriteBaseURL != ""
	c.writeRouteMu.RUnlock()
	if resolved {
		return
	}
	// Slow path: serialize resolution under the write lock. Unlike sync.Once,
	// a transient failure (network timeout, empty result) does not consume the
	// one-and-only attempt — only a successful, non-empty result is recorded,
	// so the next write retries. Reads never reach here (baseURLFor short-
	// circuits GET/HEAD), so holding the lock during the resolver only
	// serializes concurrent writes, which is the intended behavior.
	c.writeRouteMu.Lock()
	defer c.writeRouteMu.Unlock()
	if c.WriteBaseURL != "" {
		return
	}
	base, err := c.ResolveWriteBase(ctx)
	if err != nil {
		c.writeRouteErr = err
		c.writeRouteWarnOnce.Do(func() {
			fmt.Fprintf(os.Stderr, "warning: could not resolve Zotero Web API write route: %v\n", err)
		})
		return
	}
	if base == "" {
		return
	}
	c.WriteBaseURL = base
	c.writeRouteErr = nil
	fmt.Fprintf(os.Stderr, "→ writing via Zotero Web API: %s (reads stay local)\n", base)
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

// isMutatingVerb reports whether the HTTP method writes server state. Used by
// do()'s verify-mode short-circuit to gate dial-out. Deliberately a fixed
// positive list of the four write verbs Zotero's API actually uses (not the
// negative "anything but GET/HEAD" test in isMutatingMethod): verify mode
// short-circuits ONLY known writes, so an unrecognized/custom verb dials
// through instead of being silently swallowed by a generic catch-all.
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
// observes a live value, which is also why callers needing a live read with no
// interest in the version use this and discard it. Live header-reading helpers
// use the same cancellable base context as the public Get wrapper.
//
// The int is a Zotero OBJECT VERSION, not an HTTP status code, even though the
// mutating verbs return the same (body, int, error) shape with a status in that
// position. A caller that confuses the two compiles cleanly.
func (c *Client) GetWithVersion(path string, params map[string]string) (json.RawMessage, int, error) {
	return c.GetWithVersionContext(c.baseCtx(), path, params)
}

// GetWithVersionContext is GetWithVersion honoring a caller-provided context, so
// callers fanning out under a cancellable context (sync workers, FanoutRun)
// abort in-flight requests promptly on cancellation instead of only on process
// SIGINT. A nil ctx falls back to the client base context.
func (c *Client) GetWithVersionContext(ctx context.Context, path string, params map[string]string) (json.RawMessage, int, error) {
	if ctx == nil {
		ctx = c.baseCtx()
	}
	respBody, _, hdr, err := c.doRequest(ctx, "GET", path, params, nil, nil)
	if err != nil {
		return nil, 0, err
	}
	return respBody, parseLastModifiedVersion(hdr), nil
}

// GetFromWriteBaseWithVersion reads path from the plane writes go to, returning
// the body and that plane's object version.
//
// Reads normally stay on BaseURL, but a key-based Zotero write cannot use the
// read plane's version: Zotero requires an If-Unmodified-Since-Version
// precondition, version numbers are per-plane, and the local API reports an
// empty version (and empty Last-Modified-Version header) for items it has never
// pushed upstream. Both coerce to 0, i.e. no precondition, which Zotero rejects.
// The only source of the write plane's version is the write plane.
//
// It fails rather than falling back to the read plane when the write route
// cannot be resolved: a precondition read from the wrong plane is worse than a
// failed write, because the version spaces are unrelated and Zotero would either
// reject the PATCH with an opaque 412 or guard it against a meaningless number.
// GetFromWriteBaseWithVersion reads path from the write plane using the client's
// own SIGINT/SIGTERM-cancellable context. Callers holding a request-scoped
// context should use GetFromWriteBaseWithVersionContext.
func (c *Client) GetFromWriteBaseWithVersion(path string, params map[string]string) (json.RawMessage, int, error) {
	return c.GetFromWriteBaseWithVersionContext(c.baseCtx(), path, params)
}

func (c *Client) GetFromWriteBaseWithVersionContext(ctx context.Context, path string, params map[string]string) (json.RawMessage, int, error) {
	if ctx == nil {
		ctx = c.baseCtx()
	}
	base, err := c.writeBaseForRead(ctx)
	if err != nil {
		return nil, 0, err
	}
	respBody, _, hdr, err := c.doRequestOnBase(ctx, base, "GET", path, params, nil, nil)
	if err != nil {
		return nil, 0, err
	}
	version := parseLastModifiedVersion(hdr)
	if version == 0 {
		// Zotero also carries the version as an object property; prefer the
		// header but do not give up the precondition when only the body has it.
		version = objectVersion(respBody)
	}
	return respBody, version, nil
}

// writeBaseForRead resolves the base a version-bearing read must use, or errors.
// An empty return means BaseURL is already the write plane — either there is no
// hybrid routing, or the CLI's eager resolution already flattened the resolved
// write base into BaseURL and cleared the resolver.
func (c *Client) writeBaseForRead(ctx context.Context) (string, error) {
	c.writeRouteMu.RLock()
	base, pending := c.WriteBaseURL, c.ResolveWriteBase != nil
	c.writeRouteMu.RUnlock()
	if base != "" {
		return base, nil
	}
	if !pending {
		return "", nil
	}
	// Hybrid routing is configured but unresolved. Resolve it now; a failure
	// here must not degrade into reading the local plane.
	c.resolveWriteRoute(ctx)
	c.writeRouteMu.RLock()
	base = c.WriteBaseURL
	routeErr := c.writeRouteErr
	c.writeRouteMu.RUnlock()
	if base == "" {
		if routeErr != nil {
			return "", fmt.Errorf("could not resolve Zotero Web API write base: %w", routeErr)
		}
		return "", errors.New("could not resolve the Zotero Web API write base; refusing to take a write precondition from the local read plane")
	}
	return base, nil
}

// objectVersion reads the "version" property from a Zotero object response.
// Tolerates the empty string the local API returns for never-pushed objects.
func objectVersion(body json.RawMessage) int {
	var object struct {
		Version json.Number `json:"version"`
	}
	if err := json.Unmarshal(body, &object); err != nil {
		return 0
	}
	version, err := object.Version.Int64()
	if err != nil {
		return 0
	}
	return int(version)
}

// GetWithHeader performs a GET and returns the body plus the trimmed value of the
// named response header (empty when absent). exposes arbitrary response
// headers (e.g. Zotero-Schema-Version) that the cached Get path discards; bypasses
// the read cache like GetWithVersion so the caller observes a live value.
func (c *Client) GetWithHeader(path string, params map[string]string, header string) (json.RawMessage, string, error) {
	return c.GetWithHeaderContext(c.baseCtx(), path, params, header)
}

// GetWithHeaderContext is GetWithHeader honoring a caller-provided context.
// A nil ctx falls back to the client base context.
func (c *Client) GetWithHeaderContext(ctx context.Context, path string, params map[string]string, header string) (json.RawMessage, string, error) {
	if ctx == nil {
		ctx = c.baseCtx()
	}
	respBody, _, hdr, err := c.doRequest(ctx, "GET", path, params, nil, nil)
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
