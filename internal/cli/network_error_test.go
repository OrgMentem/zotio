// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestNetworkErrorTruncatedBodyServesMirror(t *testing.T) {
	seedLocalQueryPlannerDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"key":"A"`))
		w.(http.Flusher).Flush()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/api/users/0")

	flags := &rootFlags{asJSON: true, dataSource: "auto", noCache: true, timeout: time.Second}
	cmd := newItemsListCmd(flags)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("items list after truncated body: %v", err)
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

func TestNetworkErrorTruncatedErrorStatusDoesNotServeMirror(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			seedLocalQueryPlannerDB(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", "1000")
				w.WriteHeader(status)
				_, _ = w.Write([]byte("short body"))
				w.(http.Flusher).Flush()
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Errorf("hijack: %v", err)
					return
				}
				_ = conn.Close()
			}))
			defer srv.Close()
			t.Setenv("ZOTERO_BASE_URL", srv.URL+"/api/users/0")

			flags := &rootFlags{asJSON: true, dataSource: "auto", noCache: true, timeout: time.Second}
			cmd := newItemsListCmd(flags)
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&bytes.Buffer{})
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), "reading response: unexpected EOF") {
				t.Fatalf("items list error = %v, want truncated response error", err)
			}
			if out.Len() != 0 {
				t.Fatalf("items list output = %q, want no mirror rows or api_unreachable", out.String())
			}
		})
	}
}

func TestNetworkErrorInvalidGzipDoesNotServeMirror(t *testing.T) {
	seedLocalQueryPlannerDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write([]byte("not gzip data"))
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/api/users/0")

	flags := &rootFlags{asJSON: true, dataSource: "auto", noCache: true, timeout: time.Second}
	cmd := newItemsListCmd(flags)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "gzip: invalid header") {
		t.Fatalf("items list error = %v, want invalid gzip error", err)
	}
	if out.Len() != 0 {
		t.Fatalf("items list output = %q, want no mirror rows or api_unreachable", out.String())
	}
}

type cancellationReadBody struct {
	ctx     context.Context
	started chan struct{}
}

func (b *cancellationReadBody) Read([]byte) (int, error) {
	close(b.started)
	<-b.ctx.Done()
	return 0, io.ErrUnexpectedEOF
}

func (b *cancellationReadBody) Close() error { return nil }

func TestNetworkErrorCancellationDuringBodyReadDoesNotServeMirror(t *testing.T) {
	seedLocalQueryPlannerDB(t)
	flags := &rootFlags{dataSource: "auto", noCache: true, timeout: time.Second}
	c, err := flags.newClient()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started := make(chan struct{})
	c.HTTPClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       &cancellationReadBody{ctx: ctx, started: started},
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})
	done := make(chan error, 1)
	go func() {
		_, _, err := resolveRead(ctx, c, flags, "items", true, "/items", nil, nil)
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("auto read after cancellation = %v, want context cancellation without mirror fallback", err)
	}
}

func TestNetworkErrorCompleteMalformedBodyDoesNotServeMirror(t *testing.T) {
	seedLocalQueryPlannerDB(t)
	body := []byte(`[{"key":"A"`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "11")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/api/users/0")

	flags := &rootFlags{asJSON: true, dataSource: "auto", noCache: true, timeout: time.Second}
	cmd := newItemsListCmd(flags)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	if err != nil && out.Len() != 0 {
		t.Fatalf("items list error = %v with unexpected output %q", err, out.String())
	}
	if strings.Contains(out.String(), "api_unreachable") {
		t.Fatalf("items list served mirror on malformed complete body: %q", out.String())
	}
	if err == nil {
		var envelope struct {
			Meta DataProvenance `json:"meta"`
		}
		if decodeErr := json.Unmarshal(out.Bytes(), &envelope); decodeErr != nil {
			t.Fatalf("decode items list %q: %v", out.String(), decodeErr)
		}
		if envelope.Meta.Source != "live" || envelope.Meta.Reason != "" {
			t.Fatalf("provenance = %+v, want live without fallback", envelope.Meta)
		}
	}
}

func TestNetworkErrorProxyConnectRefusalServesMirror(t *testing.T) {
	seedLocalQueryPlannerDB(t)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			t.Errorf("proxy method = %s, want CONNECT", r.Method)
		}
		w.WriteHeader(http.StatusProxyAuthRequired)
	}))
	defer proxy.Close()
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZOTERO_BASE_URL", "https://api.zotero.org/users/0")
	flags := &rootFlags{asJSON: true, dataSource: "auto", noCache: true, timeout: time.Second}
	c, err := flags.newClient()
	if err != nil {
		t.Fatal(err)
	}
	transport := c.HTTPClient.Transport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyURL(proxyURL)
	c.HTTPClient.Transport = transport
	data, prov, err := resolveRead(t.Context(), c, flags, "items", false, "/items", nil, nil)
	if err != nil {
		t.Fatalf("items list after proxy CONNECT refusal: %v", err)
	}
	if prov.Source != "local" || prov.Reason != "api_unreachable" {
		t.Fatalf("provenance = %+v, want local api_unreachable", prov)
	}
	var items []struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(data, &items); err != nil {
		t.Fatalf("decode mirror items %q: %v", data, err)
	}
	keys := make(map[string]bool, len(items))
	for _, item := range items {
		keys[item.Key] = true
	}
	if len(items) != 3 || !keys["A"] || !keys["B"] || !keys["C"] {
		t.Fatalf("mirror keys = %v, want A, B, C", keys)
	}
}
