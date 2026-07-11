package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"zotio/internal/cache"
)

func TestPublicDialContextRejectsPrivateIPBeforeDial(t *testing.T) {
	conn, err := publicDialContext(context.Background(), "tcp", "10.0.0.1:443")
	if err == nil {
		if conn != nil {
			_ = conn.Close()
		}
		t.Fatal("publicDialContext accepted private IP dial target, want rejection")
	}
	if conn != nil {
		_ = conn.Close()
		t.Fatal("publicDialContext returned a connection for a private IP dial target")
	}
	if !strings.Contains(err.Error(), "local or private") {
		t.Fatalf("publicDialContext private target error = %v, want local/private rejection", err)
	}
}

func TestIPv4MappedIPv6LiteralClassification(t *testing.T) {
	tests := []struct {
		name    string
		host    string
		private bool
	}{
		{name: "mapped loopback", host: "::ffff:127.0.0.1", private: true},
		{name: "mapped ten slash eight", host: "::ffff:10.23.45.67", private: true},
		{name: "mapped one seventy two private", host: "::ffff:172.16.0.1", private: true},
		{name: "mapped one ninety two private", host: "::ffff:192.168.1.1", private: true},
		{name: "mapped metadata link local", host: "::ffff:169.254.169.254", private: true},
		{name: "mapped public IPv4", host: "::ffff:8.8.8.8", private: false},
		{name: "ordinary public IPv4", host: "8.8.4.4", private: false},
		{name: "ordinary public IPv6", host: "2001:4860:4860::8888", private: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := outboundHostIsPrivate(tt.host); got != tt.private {
				t.Fatalf("outboundHostIsPrivate(%q) = %v, want %v", tt.host, got, tt.private)
			}
		})
	}
}

func TestMappedPrivateIPv6LiteralsRejectedByValidationAndDialResolution(t *testing.T) {
	privateHosts := []string{
		"::ffff:127.0.0.1",
		"::ffff:10.23.45.67",
		"::ffff:172.16.0.1",
		"::ffff:192.168.1.1",
		"::ffff:169.254.169.254",
	}

	for _, host := range privateHosts {
		t.Run(host, func(t *testing.T) {
			if err := validateExternalHTTPURL("https://["+host+"]/resource", false); err == nil {
				t.Fatalf("validateExternalHTTPURL accepted mapped private literal %q", host)
			} else if !strings.Contains(err.Error(), "local or private") {
				t.Fatalf("validateExternalHTTPURL error = %v, want local/private rejection", err)
			}

			conn, err := publicDialContext(context.Background(), "tcp", net.JoinHostPort(host, "443"))
			if conn != nil {
				_ = conn.Close()
				t.Fatalf("publicDialContext returned a connection for mapped private literal %q", host)
			}
			if err == nil {
				t.Fatalf("publicDialContext accepted mapped private literal %q", host)
			}
			if !strings.Contains(err.Error(), "local or private") {
				t.Fatalf("publicDialContext error = %v, want local/private rejection", err)
			}
		})
	}
}

func TestPublicLiteralResolutionUnmapsIPv4MappedIPv6(t *testing.T) {
	tests := []struct {
		name string
		host string
		want string
	}{
		{name: "mapped public IPv4", host: "::ffff:8.8.8.8", want: "8.8.8.8"},
		{name: "ordinary public IPv4", host: "8.8.4.4", want: "8.8.4.4"},
		{name: "ordinary public IPv6", host: "2001:4860:4860::8888", want: "2001:4860:4860::8888"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			urlHost := tt.host
			if strings.Contains(urlHost, ":") {
				urlHost = "[" + urlHost + "]"
			}
			if err := validateExternalHTTPURL("https://"+urlHost+"/resource", false); err != nil {
				t.Fatalf("validateExternalHTTPURL(%q): %v", tt.host, err)
			}
			ips, err := publicOutboundIPs(context.Background(), tt.host)
			if err != nil {
				t.Fatalf("publicOutboundIPs(%q): %v", tt.host, err)
			}
			if len(ips) != 1 || ips[0] != tt.want {
				t.Fatalf("publicOutboundIPs(%q) = %v, want [%s]", tt.host, ips, tt.want)
			}
		})
	}
}

func TestValidateExternalHTTPURLAllowsNonResolvingStoredLink(t *testing.T) {
	oldResolver := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			return nil, errors.New("dns disabled in test")
		},
	}
	t.Cleanup(func() { net.DefaultResolver = oldResolver })

	if err := validateExternalHTTPURL("https://does-not-resolve.example.invalid/resource", false); err != nil {
		t.Fatalf("validateExternalHTTPURL rejected non-resolving stored link: %v", err)
	}
}

func TestExternalFetchHTTPClientPreservesCustomDefaultTransport(t *testing.T) {
	custom := &externalHTTPRoundTripper{
		roundTrip: func(req *http.Request) (*http.Response, error) {
			return externalHTTPTestResponse(req, http.StatusOK, "", "ok"), nil
		},
	}
	oldDefaultClient := http.DefaultClient
	oldDefaultTransport := http.DefaultTransport
	http.DefaultClient = &http.Client{}
	http.DefaultTransport = custom
	t.Cleanup(func() {
		http.DefaultClient = oldDefaultClient
		http.DefaultTransport = oldDefaultTransport
	})

	client := externalFetchHTTPClient(nil, false)
	effectiveTransport := client.Transport
	if effectiveTransport == nil {
		effectiveTransport = http.DefaultTransport
	}
	if effectiveTransport != custom {
		t.Fatalf("effective transport = %T, want injected default transport", effectiveTransport)
	}
	resp, err := client.Get("https://8.8.8.8/resource")
	if err != nil {
		t.Fatalf("GET with injected default transport: %v", err)
	}
	_ = resp.Body.Close()
}

func TestExternalFetchHTTPClientPreservesExplicitCustomTransport(t *testing.T) {
	custom := &externalHTTPRoundTripper{
		roundTrip: func(req *http.Request) (*http.Response, error) {
			return externalHTTPTestResponse(req, http.StatusOK, "", "ok"), nil
		},
	}

	client := externalFetchHTTPClient(&http.Client{Transport: custom}, false)
	if client.Transport != custom {
		t.Fatalf("transport = %T, want explicit custom transport", client.Transport)
	}
	resp, err := client.Get("https://8.8.8.8/resource")
	if err != nil {
		t.Fatalf("GET with explicit custom transport: %v", err)
	}
	_ = resp.Body.Close()
}

func TestExternalFetchHTTPClientInstallsProductionDialGuard(t *testing.T) {
	oldDefaultTransport := http.DefaultTransport
	http.DefaultTransport = &http.Transport{Proxy: http.ProxyFromEnvironment}
	t.Cleanup(func() { http.DefaultTransport = oldDefaultTransport })

	client := externalFetchHTTPClient(&http.Client{}, false)
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http.Transport", client.Transport)
	}
	if transport == http.DefaultTransport {
		t.Fatal("transport was not cloned")
	}
	if transport.Proxy != nil {
		t.Fatal("transport proxy is enabled, want disabled")
	}
	if transport.DialContext == nil {
		t.Fatal("transport dial guard is nil")
	}
	if got, want := reflect.ValueOf(transport.DialContext).Pointer(), reflect.ValueOf(publicDialContext).Pointer(); got != want {
		t.Fatal("transport dial guard is not publicDialContext")
	}
}

func TestDeliverWebhookRejectsPrivateLiteralTarget(t *testing.T) {
	err := deliverWebhook(context.Background(), "http://127.0.0.1:1/hook", []byte(`{"ok":true}`), false)
	if err == nil {
		t.Fatal("deliverWebhook accepted loopback target, want rejection")
	}
	if !strings.Contains(err.Error(), "local or private") {
		t.Fatalf("deliverWebhook loopback error = %v, want local/private rejection", err)
	}
}

func TestDeliverWebhookRevalidatesHostAtDialTime(t *testing.T) {
	var calls int
	oldLookup := publicOutboundIPLookup
	publicOutboundIPLookup = func(ctx context.Context, host string) ([]string, error) {
		calls++
		if calls == 1 {
			return nil, fmt.Errorf("resolving host %q: test DNS unavailable", host)
		}
		return nil, fmt.Errorf("host %q resolves to local or private address 127.0.0.1", host)
	}
	t.Cleanup(func() { publicOutboundIPLookup = oldLookup })

	err := deliverWebhook(context.Background(), "http://rebind.example.test/hook", []byte(`{"ok":true}`), false)
	if err == nil {
		t.Fatal("deliverWebhook accepted host that rebound to loopback, want rejection")
	}
	if calls < 2 {
		t.Fatalf("public outbound lookup calls = %d, want validation plus dial-time lookup", calls)
	}
	if !strings.Contains(err.Error(), "local or private") {
		t.Fatalf("deliverWebhook rebinding error = %v, want local/private rejection", err)
	}
}

func TestPostFeedbackRevalidatesHostAtDialTime(t *testing.T) {
	var calls int
	oldLookup := publicOutboundIPLookup
	publicOutboundIPLookup = func(ctx context.Context, host string) ([]string, error) {
		calls++
		if calls == 1 {
			return nil, fmt.Errorf("resolving host %q: test DNS unavailable", host)
		}
		return nil, fmt.Errorf("host %q resolves to local or private address 127.0.0.1", host)
	}
	t.Cleanup(func() { publicOutboundIPLookup = oldLookup })

	err := postFeedback("https://feedback-rebind.example.test/hook", FeedbackEntry{Text: "hello"})
	if err == nil {
		t.Fatal("postFeedback accepted host that rebound to loopback, want rejection")
	}
	if calls < 2 {
		t.Fatalf("public outbound lookup calls = %d, want validation plus dial-time lookup", calls)
	}
	if !strings.Contains(err.Error(), "local or private") {
		t.Fatalf("postFeedback rebinding error = %v, want local/private rejection", err)
	}
}

func TestDeliverWebhookHonorsContextCancellation(t *testing.T) {
	oldAllowPrivateOutbound := allowPrivateOutboundForTests
	allowPrivateOutboundForTests = true
	t.Cleanup(func() { allowPrivateOutboundForTests = oldAllowPrivateOutbound })

	started := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer func() {
		close(release)
		srv.Close()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- deliverWebhook(ctx, srv.URL, []byte(`{"ok":true}`), false)
	}()

	select {
	case <-started:
	case err := <-errCh:
		t.Fatalf("deliverWebhook returned before cancellation: %v", err)
	case <-time.After(time.Second):
		t.Fatal("deliverWebhook did not start POST")
	}

	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("deliverWebhook succeeded after context cancellation, want error")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("deliverWebhook cancellation error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("deliverWebhook did not abort promptly after context cancellation")
	}
}

func TestSameOriginExternalFetchHTTPClientAllowsSameOriginRedirect(t *testing.T) {
	var paths []string
	transport := externalHTTPRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.RequestURI())
		switch req.URL.Path {
		case "/start":
			return externalHTTPTestResponse(req, http.StatusFound, "https://8.8.8.8:443/next?value=1", ""), nil
		case "/next":
			return externalHTTPTestResponse(req, http.StatusOK, "", "ok"), nil
		default:
			return nil, fmt.Errorf("unexpected path %q", req.URL.Path)
		}
	})

	resp, err := sameOriginExternalFetchHTTPClient(&http.Client{Transport: transport}, false).Get("https://8.8.8.8/start")
	if err != nil {
		t.Fatalf("same-origin redirect: %v", err)
	}
	defer resp.Body.Close()
	if got, want := strings.Join(paths, ","), "/start,/next?value=1"; got != want {
		t.Fatalf("requested paths = %q, want %q", got, want)
	}
}

func TestSameOriginExternalFetchHTTPClientRejectsOriginChangesBeforeTarget(t *testing.T) {
	tests := []struct {
		name     string
		location string
	}{
		{name: "scheme", location: "http://8.8.8.8/target"},
		{name: "host", location: "https://1.1.1.1/target"},
		{name: "port", location: "https://8.8.8.8:444/target"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			targetHits := 0
			transport := externalHTTPRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path != "/start" {
					targetHits++
				}
				return externalHTTPTestResponse(req, http.StatusFound, tt.location, ""), nil
			})

			resp, err := sameOriginExternalFetchHTTPClient(&http.Client{Transport: transport}, false).Get("https://8.8.8.8/start")
			if resp != nil {
				defer resp.Body.Close()
			}
			if err == nil {
				t.Fatalf("redirect to %q succeeded, want cross-origin rejection", tt.location)
			}
			if targetHits != 0 {
				t.Fatalf("redirect target hits = %d, want 0", targetHits)
			}
			if !strings.Contains(err.Error(), "cross-origin") {
				t.Fatalf("redirect error = %v, want cross-origin rejection", err)
			}
		})
	}
}

func TestSameOriginExternalFetchHTTPClientRejectsLoopbackRedirectBeforeTarget(t *testing.T) {
	targetHits := 0
	transport := externalHTTPRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Hostname() == "127.0.0.1" {
			targetHits++
		}
		return externalHTTPTestResponse(req, http.StatusFound, "http://127.0.0.1/target", ""), nil
	})

	resp, err := sameOriginExternalFetchHTTPClient(&http.Client{Transport: transport}, false).Get("http://8.8.8.8/start")
	if resp != nil {
		defer resp.Body.Close()
	}
	if err == nil {
		t.Fatal("redirect to loopback succeeded, want rejection")
	}
	if targetHits != 0 {
		t.Fatalf("loopback target hits = %d, want 0", targetHits)
	}
	if !strings.Contains(err.Error(), "local or private") {
		t.Fatalf("redirect error = %v, want local/private rejection", err)
	}
}

func TestSameOriginExternalFetchHTTPClientCapsRedirectLoop(t *testing.T) {
	requests := 0
	transport := externalHTTPRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		return externalHTTPTestResponse(req, http.StatusFound, "/loop", ""), nil
	})

	resp, err := sameOriginExternalFetchHTTPClient(&http.Client{Transport: transport}, false).Get("http://8.8.8.8/loop")
	if resp != nil {
		defer resp.Body.Close()
	}
	if err == nil {
		t.Fatal("redirect loop succeeded, want rejection")
	}
	if requests != 10 {
		t.Fatalf("requests before redirect rejection = %d, want 10", requests)
	}
	if !strings.Contains(err.Error(), "10 redirects") {
		t.Fatalf("redirect loop error = %v, want redirect cap", err)
	}
}

func TestGetCappedProviderJSONDecodesAndCachesWithCustomTransport(t *testing.T) {
	requests := 0
	transport := externalHTTPRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		return externalHTTPTestResponse(req, http.StatusOK, "", `{"value":"fresh"}`), nil
	})
	pc := &providerJSONCache{store: cache.New(t.TempDir(), time.Hour)}
	type providerResponse struct {
		Value string `json:"value"`
	}

	var first providerResponse
	if err := getCappedProviderJSON(context.Background(), &http.Client{Transport: transport}, providerCrossRef, "https://8.8.8.8/provider", pc, &first); err != nil {
		t.Fatalf("first provider fetch: %v", err)
	}
	if first.Value != "fresh" {
		t.Fatalf("first decoded value = %q, want fresh", first.Value)
	}

	var second providerResponse
	if err := getCappedProviderJSON(context.Background(), &http.Client{Transport: transport}, providerCrossRef, "https://8.8.8.8/provider", pc, &second); err != nil {
		t.Fatalf("cached provider fetch: %v", err)
	}
	if second.Value != "fresh" {
		t.Fatalf("cached decoded value = %q, want fresh", second.Value)
	}
	if requests != 1 {
		t.Fatalf("provider transport requests = %d, want 1 after cache hit", requests)
	}
}

type externalHTTPRoundTripper struct {
	roundTrip func(*http.Request) (*http.Response, error)
}

func (t *externalHTTPRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.roundTrip(req)
}

type externalHTTPRoundTripFunc func(*http.Request) (*http.Response, error)

func (f externalHTTPRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func externalHTTPTestResponse(req *http.Request, status int, location, body string) *http.Response {
	header := make(http.Header)
	if location != "" {
		header.Set("Location", location)
	}
	return &http.Response{
		StatusCode: status,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}
