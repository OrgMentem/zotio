// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestItemsUpdateAbortsWhenVersionReadFails(t *testing.T) {
	patchIssued := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			http.Error(w, "version service unavailable", http.StatusServiceUnavailable)
		case http.MethodPatch:
			patchIssued = true
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected", http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()

	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	cmd := newItemsUpdateCmd(&rootFlags{asJSON: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"K", "--title", "updated"})
	err := cmd.Execute()
	if ExitCode(err) != 5 {
		t.Fatalf("ExitCode(update error) = %d, want 5; err = %v", ExitCode(err), err)
	}
	if patchIssued {
		t.Fatal("PATCH issued after failed version read")
	}
}
