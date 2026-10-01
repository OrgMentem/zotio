// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"os"
	"strings"
	"testing"
)

// Webhook endpoints carry their credential in the userinfo, path, or query.
// A failed delivery warns on stderr, which supervisors and CI logs collect, so
// the warning may name the endpoint's origin and the cause but never the
// credential-bearing parts — neither in the target it names nor inside the
// transport error net/http builds from the full request URL.
func TestRfWebhookDeliveryFailureWarningOmitsCredentials(t *testing.T) {
	oldAllowPrivateOutbound := allowPrivateOutboundForTests.Load()
	allowPrivateOutboundForTests.Store(true)
	t.Cleanup(func() { allowPrivateOutboundForTests.Store(oldAllowPrivateOutbound) })

	secrets := []string{"RF-USER-SECRET", "RF-PATH-SECRET", "RF-QUERY-SECRET"}
	for _, tc := range []struct {
		name      string
		closed    bool
		wantCause string
	}{
		{name: "non-2xx response", wantCause: "webhook returned 500"},
		{name: "transport failure", closed: true, wantCause: "posting to webhook"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			}))
			t.Cleanup(srv.Close)
			base, err := neturl.Parse(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			if tc.closed {
				srv.Close()
			}
			target := "http://" + secrets[0] + "@" + base.Host + "/hooks/" + secrets[1] + "?token=" + secrets[2]
			sink := DeliverSink{Scheme: "webhook", Target: target}
			spool, err := newDeliverSpool(sink)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(spool.cleanup)
			if _, err := io.WriteString(spool, `{"ok":true}`); err != nil {
				t.Fatal(err)
			}

			warning := rfCaptureStderr(t, func() {
				deliverCapturedOutput(nil, context.Background(), sink, spool, false)
			})
			wantPrefix := "warning: deliver to webhook:http://" + base.Host + " failed:"
			if !strings.Contains(warning, wantPrefix) || !strings.Contains(warning, tc.wantCause) {
				t.Fatalf("stderr = %q, want %q naming %q", warning, wantPrefix, tc.wantCause)
			}
			for _, secret := range secrets {
				if strings.Contains(warning, secret) {
					t.Fatalf("stderr leaked %q: %q", secret, warning)
				}
			}
		})
	}
}

// rfCaptureStderr runs fn with os.Stderr redirected and returns what it wrote.
func rfCaptureStderr(t *testing.T, fn func()) string {
	t.Helper()
	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	defer func() {
		os.Stderr = oldStderr
		_ = r.Close()
	}()
	fn()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stderr = oldStderr
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}
