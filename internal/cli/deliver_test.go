// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A webhook 3xx is never followed, so only a 2xx may be reported as delivered.
func TestDeliverWebhookStatusHandling(t *testing.T) {
	requireRefusedRedirects(t, false, "/hook", []redirectStatusCase{
		{name: "moved permanently", status: http.StatusMovedPermanently, wantErr: "webhook returned 301"},
		{name: "found", status: http.StatusFound, wantErr: "webhook returned 302"},
		{name: "permanent redirect", status: http.StatusPermanentRedirect, wantErr: "webhook returned 308"},
		{name: "ok", status: http.StatusOK},
		{name: "no content", status: http.StatusNoContent},
	}, func(_ *testing.T, _ *httptest.Server, url string) error {
		return deliverWebhook(context.Background(), url, []byte(`{"ok":true}`), false)
	})
}
