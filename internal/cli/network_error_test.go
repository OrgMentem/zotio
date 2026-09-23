// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"zotio/internal/client"
)

func TestNetworkErrorServerBodyDoesNotServeMirror(t *testing.T) {
	for _, body := range []string{"connection refused", "i/o timeout"} {
		t.Run(body, func(t *testing.T) {
			seedLocalQueryPlannerDB(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/users/0/items" {
					t.Errorf("request path = %q, want /api/users/0/items", r.URL.Path)
				}
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()
			t.Setenv("ZOTERO_BASE_URL", srv.URL+"/api/users/0")

			flags := &rootFlags{asJSON: true, dataSource: "auto", noCache: true, timeout: time.Second}
			cmd := newItemsListCmd(flags)
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&bytes.Buffer{})
			err := cmd.Execute()
			var apiError *client.APIError
			if !errors.As(err, &apiError) || apiError.StatusCode != http.StatusForbidden || ExitCode(err) != 4 {
				t.Fatalf("items list error = %v, exit = %d, want HTTP 403 and auth exit 4", err, ExitCode(err))
			}
			if !strings.Contains(err.Error(), body) {
				t.Fatalf("items list error = %q, want server response %q", err, body)
			}
			if out.Len() != 0 {
				t.Fatalf("items list output = %q, want no mirrored rows or api_unreachable provenance", out.String())
			}
		})
	}
}

func TestNetworkErrorRefusedConnectionServesMirror(t *testing.T) {
	seedLocalQueryPlannerDB(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	t.Setenv("ZOTERO_BASE_URL", "http://"+address+"/api/users/0")

	flags := &rootFlags{asJSON: true, dataSource: "auto", noCache: true, timeout: time.Second}
	cmd := newItemsListCmd(flags)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("items list on refused connection: %v", err)
	}
	var envelope struct {
		Results []struct {
			Key string `json:"key"`
		} `json:"results"`
		Meta DataProvenance `json:"meta"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatalf("decode items list %q: %v", out.String(), err)
	}
	if envelope.Meta.Source != "local" || envelope.Meta.Reason != "api_unreachable" {
		t.Fatalf("provenance = %+v, want local api_unreachable", envelope.Meta)
	}
	keys := make(map[string]bool, len(envelope.Results))
	for _, item := range envelope.Results {
		keys[item.Key] = true
	}
	if len(envelope.Results) != 3 || !keys["A"] || !keys["B"] || !keys["C"] {
		t.Fatalf("mirror keys = %v, want A, B, C", keys)
	}
}
