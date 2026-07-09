package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
