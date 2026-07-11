// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// covers client do, retry, cache, sanitization, and cache-key behavior.

package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"zotio/internal/config"
)

type clientRoundTripFunc func(*http.Request) (*http.Response, error)

func (f clientRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func clientTestNewClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	c := New(&config.Config{BaseURL: baseURL}, 5*time.Second, 0)
	c.BaseURL = baseURL
	return c
}

func TestDoReturnsSuccessBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ok" {
			t.Fatalf("path = %q, want /ok", r.URL.Path)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	c := clientTestNewClient(t, server.URL)
	got, status, err := c.do(context.Background(), http.MethodGet, "/ok", nil, nil, nil)
	if err != nil {
		t.Fatalf("do returned error: %v", err)
	}
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}
	if !bytes.Equal(got, []byte(`{"ok":true}`)) {
		t.Fatalf("body = %s, want %s", got, `{"ok":true}`)
	}
}

func TestDoClientErrorReturnsAPIErrorWithoutRetry(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Error(w, "missing", http.StatusNotFound)
	}))
	defer server.Close()

	c := clientTestNewClient(t, server.URL)
	_, status, err := c.do(context.Background(), http.MethodGet, "/missing", nil, nil, nil)
	if err == nil {
		t.Fatal("do returned nil error for 404")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", status, http.StatusNotFound)
	}
	if apiErr.StatusCode != http.StatusNotFound {
		t.Fatalf("APIError.StatusCode = %d, want %d", apiErr.StatusCode, http.StatusNotFound)
	}
	if apiErr.Method != http.MethodGet || apiErr.Path != "/missing" {
		t.Fatalf("APIError request = %s %s, want GET /missing", apiErr.Method, apiErr.Path)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("server hits = %d, want 1", got)
	}
}

func TestDoRetriesServerErrorThenSucceeds(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			http.Error(w, "try again", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"retried":true}`))
	}))
	defer server.Close()

	c := clientTestNewClient(t, server.URL)
	got, status, err := c.do(context.Background(), http.MethodGet, "/retry", nil, nil, nil)
	if err != nil {
		t.Fatalf("do returned error after retry: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if !bytes.Equal(got, []byte(`{"retried":true}`)) {
		t.Fatalf("body = %s, want retry success body", got)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("server hits = %d, want 2", got)
	}
}

func TestGetCachesAndMutationInvalidatesCache(t *testing.T) {
	var getHits int32
	var mutationHits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			count := atomic.AddInt32(&getHits, 1)
			_, _ = w.Write([]byte(`{"version":` + strconv.Itoa(int(count)) + `}`))
		case http.MethodPost:
			atomic.AddInt32(&mutationHits, 1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"mutated":true}`))
		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	c := clientTestNewClient(t, server.URL)
	c.cacheDir = t.TempDir()
	params := map[string]string{"q": "one"}

	first, err := c.Get("/items", params)
	if err != nil {
		t.Fatalf("first Get returned error: %v", err)
	}
	second, err := c.Get("/items", params)
	if err != nil {
		t.Fatalf("second Get returned error: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("cached body = %s, want first body %s", second, first)
	}
	if got := atomic.LoadInt32(&getHits); got != 1 {
		t.Fatalf("GET hits before mutation = %d, want 1", got)
	}

	if _, _, err := c.Post("/items", map[string]string{"title": "new"}); err != nil {
		t.Fatalf("Post returned error: %v", err)
	}
	if got := atomic.LoadInt32(&mutationHits); got != 1 {
		t.Fatalf("mutation hits = %d, want 1", got)
	}

	third, err := c.Get("/items", params)
	if err != nil {
		t.Fatalf("third Get returned error: %v", err)
	}
	if bytes.Equal(third, first) {
		t.Fatalf("third body = %s, want a refreshed response after mutation", third)
	}
	if got := atomic.LoadInt32(&getHits); got != 2 {
		t.Fatalf("GET hits after mutation = %d, want 2", got)
	}
}

func TestSanitizeJSONResponse(t *testing.T) {
	clean := []byte(`{"items":[1]}`)
	if got := sanitizeJSONResponse(clean); !bytes.Equal(got, clean) {
		t.Fatalf("clean JSON sanitized to %q, want unchanged %q", got, clean)
	}
	if got := sanitizeJSONResponse(sanitizeJSONResponse(clean)); !bytes.Equal(got, clean) {
		t.Fatalf("sanitize is not idempotent for clean JSON: got %q", got)
	}

	cases := []struct {
		name string
		in   []byte
		want []byte
	}{
		{name: "bom and xssi newline", in: []byte("\xEF\xBB\xBF)]}'\n \t\r\n{\"ok\":true}"), want: []byte(`{"ok":true}`)},
		{name: "xssi without newline", in: []byte(")]}'   {\"ok\":true}"), want: []byte(`{"ok":true}`)},
		{name: "angular prefix", in: []byte("{}&& \n[1]"), want: []byte(`[1]`)},
		{name: "for loop prefix", in: []byte("for(;;);\t{\"x\":1}"), want: []byte(`{"x":1}`)},
		{name: "while loop prefix", in: []byte("while(1);\r\nnull"), want: []byte(`null`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeJSONResponse(tc.in); !bytes.Equal(got, tc.want) {
				t.Fatalf("sanitizeJSONResponse(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCacheKeyDeterministicAndVaries(t *testing.T) {
	c := clientTestNewClient(t, "http://example.test")
	params := map[string]string{"b": "2", "a": "1"}
	reordered := map[string]string{"a": "1", "b": "2"}

	first := c.cacheKey("/items", params, nil)
	if second := c.cacheKey("/items", params, nil); second != first {
		t.Fatalf("cacheKey is not deterministic: %q then %q", first, second)
	}
	if got := c.cacheKey("/items", reordered, nil); got != first {
		t.Fatalf("cacheKey depends on map iteration/order: %q vs %q", got, first)
	}
	if got := c.cacheKey("/other", params, nil); got == first {
		t.Fatal("cacheKey did not change when path changed")
	}
	if got := c.cacheKey("/items", map[string]string{"a": "1", "b": "3"}, nil); got == first {
		t.Fatal("cacheKey did not change when params changed")
	}
	headers := map[string]string{"X-B": "2", "X-A": "1"}
	reorderedHeaders := map[string]string{"X-A": "1", "X-B": "2"}
	withHeaders := c.cacheKey("/items", params, headers)
	if got := c.cacheKey("/items", params, reorderedHeaders); got != withHeaders {
		t.Fatalf("cacheKey depends on header map iteration/order: %q vs %q", got, withHeaders)
	}
	if withHeaders == first {
		t.Fatal("cacheKey did not change when headers were added")
	}
	if got := c.cacheKey("/items", params, map[string]string{"X-A": "changed", "X-B": "2"}); got == withHeaders {
		t.Fatal("cacheKey did not change when header value changed")
	}
}

func TestReadWriteCacheHonorsFreshness(t *testing.T) {
	c := clientTestNewClient(t, "http://example.test")
	c.cacheDir = t.TempDir()
	params := map[string]string{"q": "cache"}
	want := []byte(`{"cached":true}`)

	c.writeCache("/items", params, nil, want)
	got, ok := c.readCache("/items", params, nil)
	if !ok {
		t.Fatal("readCache missed immediately after writeCache")
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("cached body = %s, want %s", got, want)
	}

	cacheFile := filepath.Join(c.cacheDir, c.cacheKey("/items", params, nil)+".json")
	old := time.Now().Add(-6 * time.Minute)
	if err := os.Chtimes(cacheFile, old, old); err != nil {
		t.Fatalf("aging cache file: %v", err)
	}
	if got, ok := c.readCache("/items", params, nil); ok {
		t.Fatalf("readCache hit expired cache with body %s", got)
	}
}

func TestCheckRedirectRequiresSameOrigin(t *testing.T) {
	initial := &http.Request{URL: &url.URL{Scheme: "https", Host: "example.test"}}
	tests := []struct {
		name    string
		target  *url.URL
		wantErr bool
	}{
		{name: "same origin", target: &url.URL{Scheme: "HTTPS", Host: "Example.test:443"}},
		{name: "scheme change", target: &url.URL{Scheme: "http", Host: "example.test:443"}, wantErr: true},
		{name: "hostname change", target: &url.URL{Scheme: "https", Host: "other.test"}, wantErr: true},
		{name: "effective port change", target: &url.URL{Scheme: "https", Host: "example.test:444"}, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := &http.Request{URL: tc.target, Header: make(http.Header)}
			req.Header.Set("Zotero-API-Key", "secret")
			req.Header.Set("Zotero-API-Version", "3")

			err := checkRedirect(req, []*http.Request{initial})
			if tc.wantErr && err == nil {
				t.Fatal("checkRedirect returned nil, want cross-origin rejection")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("checkRedirect returned error: %v", err)
			}
			if !tc.wantErr {
				if got := req.Header.Get("Zotero-API-Key"); got != "secret" {
					t.Fatalf("Zotero-API-Key = %q, want retained", got)
				}
				if got := req.Header.Get("Zotero-API-Version"); got != "3" {
					t.Fatalf("Zotero-API-Version = %q, want retained", got)
				}
			}
		})
	}
}

func TestSameOriginRedirectsRetainHeadersAndMutationBodies(t *testing.T) {
	tests := []struct {
		name   string
		method string
		status int
		body   any
	}{
		{name: "GET", method: http.MethodGet, status: http.StatusFound},
		{name: "POST 307", method: http.MethodPost, status: http.StatusTemporaryRedirect, body: map[string]string{"title": "redirect-safe"}},
		{name: "POST 308", method: http.MethodPost, status: http.StatusPermanentRedirect, body: map[string]string{"title": "redirect-safe"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var callerRedirects int32
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/start":
					http.Redirect(w, r, server.URL+"/target", tc.status)
				case "/target":
					if r.Method != tc.method {
						t.Errorf("redirected method = %s, want %s", r.Method, tc.method)
					}
					if got := r.Header.Get("Zotero-API-Version"); got != "3" {
						t.Errorf("redirected Zotero-API-Version = %q, want 3", got)
					}
					if tc.body != nil {
						var got map[string]string
						if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
							t.Errorf("decoding redirected body: %v", err)
						} else if got["title"] != "redirect-safe" {
							t.Errorf("redirected body title = %q, want redirect-safe", got["title"])
						}
					}
					_, _ = w.Write([]byte(`{"ok":true}`))
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			c := clientTestNewClient(t, server.URL)
			c.HTTPClient = &http.Client{
				Timeout: 5 * time.Second,
				CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
					atomic.AddInt32(&callerRedirects, 1)
					return nil
				},
			}
			c.NoCache = true
			c.Config.Headers = map[string]string{"Zotero-API-Version": "3"}
			got, status, _, err := c.doRequest(context.Background(), tc.method, "/start", nil, tc.body, nil)
			if err != nil {
				t.Fatalf("doRequest returned error: %v", err)
			}
			if status != http.StatusOK {
				t.Fatalf("status = %d, want %d", status, http.StatusOK)
			}
			if !bytes.Equal(got, []byte(`{"ok":true}`)) {
				t.Fatalf("body = %s, want redirect success body", got)
			}
			if got := atomic.LoadInt32(&callerRedirects); got != 1 {
				t.Fatalf("caller redirect callback calls = %d, want 1", got)
			}
		})
	}
}

func TestCrossOriginRedirectsNeverReachTarget(t *testing.T) {
	tests := []struct {
		name   string
		method string
		status int
		body   any
	}{
		{name: "GET", method: http.MethodGet, status: http.StatusFound},
		{name: "POST 307", method: http.MethodPost, status: http.StatusTemporaryRedirect, body: map[string]string{"title": "must-not-leak"}},
		{name: "POST 308", method: http.MethodPost, status: http.StatusPermanentRedirect, body: map[string]string{"title": "must-not-leak"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var targetHits int32
			var callerRedirects int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&targetHits, 1)
				_, _ = w.Write([]byte(`{"leaked":true}`))
			}))
			defer target.Close()

			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target.URL+"/connector", tc.status)
			}))
			defer source.Close()

			c := clientTestNewClient(t, source.URL)
			c.HTTPClient = &http.Client{
				CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
					atomic.AddInt32(&callerRedirects, 1)
					return nil
				},
			}
			c.NoCache = true
			_, _, _, err := c.doRequest(context.Background(), tc.method, "/start", nil, tc.body, nil)
			if err == nil {
				t.Fatal("doRequest returned nil, want cross-origin redirect rejection")
			}
			if got := atomic.LoadInt32(&targetHits); got != 0 {
				t.Fatalf("cross-origin target requests = %d, want 0", got)
			}
			if got := atomic.LoadInt32(&callerRedirects); got != 0 {
				t.Fatalf("caller redirect callback calls = %d, want 0 before mandatory rejection", got)
			}
		})
	}
}

func TestCallerRedirectURLMutationRechecksSameOrigin(t *testing.T) {
	t.Run("cross-origin mutation rejected", func(t *testing.T) {
		var targetHits int32
		var targetBody string
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&targetHits, 1)
			body, _ := io.ReadAll(r.Body)
			targetBody = string(body)
			_, _ = w.Write([]byte(`{"leaked":true}`))
		}))
		defer target.Close()

		var source *httptest.Server
		source = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, source.URL+"/original-target", http.StatusTemporaryRedirect)
		}))
		defer source.Close()

		mutatedURL, err := url.Parse(target.URL + "/escaped")
		if err != nil {
			t.Fatalf("parsing cross-origin mutation URL: %v", err)
		}
		var callerRedirects int32
		c := clientTestNewClient(t, source.URL)
		c.NoCache = true
		c.HTTPClient = &http.Client{
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				atomic.AddInt32(&callerRedirects, 1)
				req.URL = mutatedURL
				via[0].URL = mutatedURL
				return nil
			},
		}

		_, _, _, err = c.doRequest(context.Background(), http.MethodPost, "/start", nil, map[string]string{"title": "must-not-leave-origin"}, nil)
		if err == nil {
			t.Fatal("doRequest returned nil, want mutated cross-origin redirect rejection")
		}
		if got := atomic.LoadInt32(&callerRedirects); got == 0 {
			t.Fatal("caller redirect callback was not invoked")
		}
		if got := atomic.LoadInt32(&targetHits); got != 0 {
			t.Fatalf("mutated cross-origin target requests = %d, want 0", got)
		}
		if targetBody != "" {
			t.Fatalf("mutation body left origin: %q", targetBody)
		}
	})

	t.Run("same-origin mutation allowed", func(t *testing.T) {
		var targetHits int32
		var receivedTitle string
		var source *httptest.Server
		source = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/start":
				http.Redirect(w, r, source.URL+"/original-target", http.StatusTemporaryRedirect)
			case "/mutated-target":
				atomic.AddInt32(&targetHits, 1)
				var body map[string]string
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decoding same-origin mutation body: %v", err)
				}
				receivedTitle = body["title"]
				_, _ = w.Write([]byte(`{"ok":true}`))
			default:
				http.NotFound(w, r)
			}
		}))
		defer source.Close()

		mutatedURL, err := url.Parse(source.URL + "/mutated-target")
		if err != nil {
			t.Fatalf("parsing same-origin mutation URL: %v", err)
		}
		var callerRedirects int32
		c := clientTestNewClient(t, source.URL)
		c.NoCache = true
		c.HTTPClient = &http.Client{
			CheckRedirect: func(req *http.Request, _ []*http.Request) error {
				atomic.AddInt32(&callerRedirects, 1)
				req.URL = mutatedURL
				return nil
			},
		}

		got, status, _, err := c.doRequest(context.Background(), http.MethodPost, "/start", nil, map[string]string{"title": "same-origin-body"}, nil)
		if err != nil {
			t.Fatalf("doRequest returned error: %v", err)
		}
		if status != http.StatusOK {
			t.Fatalf("status = %d, want %d", status, http.StatusOK)
		}
		if !bytes.Equal(got, []byte(`{"ok":true}`)) {
			t.Fatalf("body = %s, want same-origin mutation response", got)
		}
		if got := atomic.LoadInt32(&callerRedirects); got != 1 {
			t.Fatalf("caller redirect callback calls = %d, want 1", got)
		}
		if got := atomic.LoadInt32(&targetHits); got != 1 {
			t.Fatalf("same-origin mutated target requests = %d, want 1", got)
		}
		if receivedTitle != "same-origin-body" {
			t.Fatalf("same-origin mutation body title = %q, want same-origin-body", receivedTitle)
		}
	})
}

func TestCallerErrUseLastResponseStillRejectsFinalRedirect(t *testing.T) {
	var targetHits int32
	var callerRedirects int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start":
			http.Redirect(w, r, server.URL+"/target", http.StatusTemporaryRedirect)
		case "/target":
			atomic.AddInt32(&targetHits, 1)
			_, _ = w.Write([]byte(`{"unexpected":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	c := clientTestNewClient(t, server.URL)
	c.NoCache = true
	c.HTTPClient = &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			atomic.AddInt32(&callerRedirects, 1)
			return http.ErrUseLastResponse
		},
	}

	_, status, _, err := c.doRequest(context.Background(), http.MethodPost, "/start", nil, map[string]string{"title": "stay"}, nil)
	if err == nil {
		t.Fatal("doRequest returned nil for final 307 response")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if status != http.StatusTemporaryRedirect || apiErr.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("redirect status = %d (APIError %d), want 307", status, apiErr.StatusCode)
	}
	if got := atomic.LoadInt32(&callerRedirects); got != 1 {
		t.Fatalf("caller redirect callback calls = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&targetHits); got != 0 {
		t.Fatalf("redirect target requests = %d, want 0", got)
	}
}

func TestRequestBoundaryPreservesInjectedTransportTimeoutAndJar(t *testing.T) {
	baseURL, err := url.Parse("https://example.test")
	if err != nil {
		t.Fatalf("parsing base URL: %v", err)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("creating cookie jar: %v", err)
	}
	jar.SetCookies(baseURL, []*http.Cookie{{
		Name:     "session",
		Value:    "preserved",
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	}})

	var transportCalls int32
	transport := clientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		atomic.AddInt32(&transportCalls, 1)
		if got := req.Header.Get("Cookie"); got != "session=preserved" {
			t.Errorf("Cookie header = %q, want injected jar cookie", got)
		}
		deadline, ok := req.Context().Deadline()
		if !ok {
			t.Error("request context has no deadline from injected client timeout")
		} else if remaining := time.Until(deadline); remaining <= 0 || remaining > 2*time.Second {
			t.Errorf("request deadline remaining = %s, want within injected 2s timeout", remaining)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			Request:    req,
		}, nil
	})

	injected := &http.Client{Transport: transport, Timeout: 2 * time.Second, Jar: jar}
	c := clientTestNewClient(t, baseURL.String())
	c.NoCache = true
	c.HTTPClient = injected
	got, status, _, err := c.doRequest(context.Background(), http.MethodGet, "/items", nil, nil, nil)
	if err != nil {
		t.Fatalf("doRequest returned error: %v", err)
	}
	if status != http.StatusOK || !bytes.Equal(got, []byte(`{"ok":true}`)) {
		t.Fatalf("response = status %d body %s, want 200 success", status, got)
	}
	if got := atomic.LoadInt32(&transportCalls); got != 1 {
		t.Fatalf("injected transport calls = %d, want 1", got)
	}
	if c.HTTPClient != injected || injected.Timeout != 2*time.Second || injected.Jar != jar {
		t.Fatal("request boundary mutated the injected HTTP client")
	}
}

func TestGetWithHeadersCacheKeyIncludesHeaders(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit := atomic.AddInt32(&hits, 1)
		payload, _ := json.Marshal(map[string]any{"variant": r.Header.Get("X-Zotio-Variant"), "hit": hit})
		_, _ = w.Write(payload)
	}))
	defer server.Close()

	c := clientTestNewClient(t, server.URL)
	c.cacheDir = t.TempDir()
	params := map[string]string{"q": "same"}

	first, err := c.GetWithHeaders("/items", params, map[string]string{"X-Zotio-Variant": "one"})
	if err != nil {
		t.Fatalf("first GetWithHeaders returned error: %v", err)
	}
	second, err := c.GetWithHeaders("/items", params, map[string]string{"X-Zotio-Variant": "two"})
	if err != nil {
		t.Fatalf("second GetWithHeaders returned error: %v", err)
	}
	if bytes.Equal(first, second) {
		t.Fatalf("second response = %s, want distinct response for different request headers", second)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("server hits = %d, want 2 for different request headers", got)
	}
}
