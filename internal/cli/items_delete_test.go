// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Zotero requires If-Unmodified-Since-Version on DELETE; items/collections
// delete must fetch the current version and send it (else HTTP 428).

package cli

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func runDeleteCmd(t *testing.T, cmd interface {
	SetOut(io.Writer)
	SetErr(io.Writer)
	SetArgs([]string)
	Execute() error
}, baseURL string, args ...string) error {
	t.Helper()
	t.Setenv("ZOTERO_BASE_URL", baseURL+"/users/0")
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	return cmd.Execute()
}

func deleteVersionServer(t *testing.T, version string) (*httptest.Server, *string) {
	t.Helper()
	sent := new(string)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Last-Modified-Version", version)
			_, _ = w.Write([]byte(`{"key":"K","version":` + version + `,"data":{}}`))
		case http.MethodDelete:
			*sent = r.Header.Get("If-Unmodified-Since-Version")
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected", http.StatusMethodNotAllowed)
		}
	}))
	return srv, sent
}

func TestItemsDeleteSendsVersionHeader(t *testing.T) {
	srv, sent := deleteVersionServer(t, "42")
	defer srv.Close()
	cmd := newItemsDeleteCmd(&rootFlags{asJSON: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	if err := runDeleteCmd(t, cmd, srv.URL, "K"); err != nil {
		t.Fatalf("items delete: %v", err)
	}
	if *sent != "42" {
		t.Errorf("If-Unmodified-Since-Version = %q, want 42", *sent)
	}
}

func TestCollectionsDeleteSendsVersionHeader(t *testing.T) {
	srv, sent := deleteVersionServer(t, "7")
	defer srv.Close()
	cmd := newCollectionsDeleteCmd(&rootFlags{asJSON: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	if err := runDeleteCmd(t, cmd, srv.URL, "K"); err != nil {
		t.Fatalf("collections delete: %v", err)
	}
	if *sent != "7" {
		t.Errorf("If-Unmodified-Since-Version = %q, want 7", *sent)
	}
}
