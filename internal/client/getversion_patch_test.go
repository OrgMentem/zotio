// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean static-audit): GetWithVersion surfaces the Zotero
// Last-Modified-Version response header (parsed as int) for version-based
// incremental sync; missing/unparseable headers must yield 0, not an error.

package client

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"zotero-pp-cli/internal/config"
)

func TestGetWithVersion(t *testing.T) {
	cases := []struct {
		name   string
		header string
		set    bool
		want   int
	}{
		{name: "numeric", header: "4521", set: true, want: 4521},
		{name: "absent", set: false, want: 0},
		{name: "non-numeric", header: "abc", set: true, want: 0},
		{name: "padded", header: "  77 ", set: true, want: 77},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.set {
					w.Header().Set("Last-Modified-Version", tc.header)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`[]`))
			}))
			defer srv.Close()

			c := New(&config.Config{BaseURL: srv.URL}, 5*time.Second, 0)
			c.BaseURL = srv.URL
			c.NoCache = true

			body, v, err := c.GetWithVersion("/items", nil)
			if err != nil {
				t.Fatalf("GetWithVersion error: %v", err)
			}
			if v != tc.want {
				t.Errorf("version = %d, want %d", v, tc.want)
			}
			if string(body) != `[]` {
				t.Errorf("body = %q, want []", string(body))
			}
		})
	}
}
