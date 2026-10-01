// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"strings"
	"testing"
)

// fxcBadLocationHandler answers every request with a redirect whose Location
// does not parse. net/http then fails the request with a cause that quotes the
// whole Location, so a receiver can echo a credential back into the error.
func fxcBadLocationHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Location", "/hooks/FXC-LOCATION-SECRET%zz")
	w.WriteHeader(http.StatusFound)
}

// A failed webhook delivery warns on stderr. The warning may name the origin
// but neither the request URL's path nor a URL quoted inside the cause.
func TestFxcWebhookWarningOmitsURLQuotedInCause(t *testing.T) {
	oldAllow := allowPrivateOutboundForTests.Load()
	allowPrivateOutboundForTests.Store(true)
	t.Cleanup(func() { allowPrivateOutboundForTests.Store(oldAllow) })

	srv := httptest.NewServer(http.HandlerFunc(fxcBadLocationHandler))
	t.Cleanup(srv.Close)
	base, err := neturl.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	sink := DeliverSink{Scheme: "webhook", Target: srv.URL + "/hooks/FXC-PATH-SECRET"}
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
	if want := "warning: deliver to webhook:http://" + base.Host + " failed: posting to webhook"; !strings.Contains(warning, want) {
		t.Fatalf("stderr = %q, want %q", warning, want)
	}
	if strings.Contains(warning, "SECRET") {
		t.Fatalf("stderr leaked a credential: %q", warning)
	}
}

// feedback --send reports a failed POST in the error and the JSON result. The
// report may name the origin but no URL text from the request or the cause.
func TestFxcFeedbackSendErrorOmitsURLQuotedInCause(t *testing.T) {
	useWriterLockTestHome(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(fxcBadLocationHandler))
	t.Cleanup(srv.Close)
	oldAllow := allowPrivateOutboundForTests.Load()
	allowPrivateOutboundForTests.Store(true)
	t.Cleanup(func() { allowPrivateOutboundForTests.Store(oldAllow) })
	oldTransport := http.DefaultTransport
	http.DefaultTransport = srv.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	t.Setenv("ZOTERO_FEEDBACK_AUTO_SEND", "")
	t.Setenv("ZOTERO_FEEDBACK_ENDPOINT", srv.URL+"/hooks/FXC-PATH-SECRET")

	for _, args := range [][]string{{"--json", "--send", "hi"}, {"--send", "hi"}} {
		stdout, stderr, err := fbRunFeedback(t, args...)
		if code := fbExitCode(err); code != 5 {
			t.Fatalf("%v exit = %d, want 5; err = %v", args, code, err)
		}
		if !strings.Contains(err.Error(), "posting feedback: Post \""+srv.URL+"\"") {
			t.Fatalf("%v error = %q, want it to name the origin", args, err)
		}
		for stream, text := range map[string]string{"stdout": stdout, "stderr": stderr, "error": err.Error()} {
			if strings.Contains(text, "SECRET") {
				t.Fatalf("%v %s leaked a credential: %q", args, stream, text)
			}
		}
	}
}
