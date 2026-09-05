// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// postDeliverWebhook installs http.ErrUseLastResponse so a webhook URL cannot
// bounce into a private service. net/http therefore hands back the 3xx itself
// and the body is never delivered, so only a 2xx may be reported as success.
func TestDeliverWebhookStatusHandling(t *testing.T) {
	oldAllowPrivateOutbound := allowPrivateOutboundForTests
	allowPrivateOutboundForTests = true
	t.Cleanup(func() { allowPrivateOutboundForTests = oldAllowPrivateOutbound })

	tests := []struct {
		name    string
		status  int
		wantErr string
	}{
		{name: "moved permanently", status: http.StatusMovedPermanently, wantErr: "webhook returned 301"},
		{name: "found", status: http.StatusFound, wantErr: "webhook returned 302"},
		{name: "permanent redirect", status: http.StatusPermanentRedirect, wantErr: "webhook returned 308"},
		{name: "ok", status: http.StatusOK},
		{name: "no content", status: http.StatusNoContent},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var paths []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				if r.URL.Path == "/hook" {
					w.Header().Set("Location", "/moved")
					w.WriteHeader(tt.status)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(srv.Close)

			err := deliverWebhook(context.Background(), srv.URL+"/hook", []byte(`{"ok":true}`), false)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("deliverWebhook(%d) = %v, want success", tt.status, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("deliverWebhook reported %d as delivered, want an error naming the status", tt.status)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("deliverWebhook error = %v, want it to contain %q", err, tt.wantErr)
			}
			if len(paths) != 1 || paths[0] != "/hook" {
				t.Fatalf("request paths = %v, want the refused redirect not to be followed", paths)
			}
		})
	}
}
