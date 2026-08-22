// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Zotero requires If-Unmodified-Since-Version on PATCH; items restore must fetch
// the current version and send it (else HTTP 428). Unlike delete, a missing item
// (404 on the version read) is a genuine error for restore, not a no-op: an item
// that doesn't exist can never be restored.

package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestItemsRestoreSendsVersionHeader(t *testing.T) {
	var getIssued, patchIssued bool
	sent := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if patchIssued {
				t.Error("GET issued after PATCH; want GET-then-PATCH ordering")
			}
			getIssued = true
			w.Header().Set("Last-Modified-Version", "42")
			_, _ = w.Write([]byte(`{"key":"K","version":42,"data":{}}`))
		case http.MethodPatch:
			if !getIssued {
				t.Error("PATCH issued before GET; want GET-then-PATCH ordering")
			}
			patchIssued = true
			sent = r.Header.Get("If-Unmodified-Since-Version")
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected", http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()

	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	cmd := newItemsRestoreCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"K"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("items restore: %v", err)
	}
	if !getIssued || !patchIssued {
		t.Fatalf("getIssued=%v patchIssued=%v, want both true", getIssued, patchIssued)
	}
	if sent != "42" {
		t.Errorf("If-Unmodified-Since-Version = %q, want 42", sent)
	}
}

func TestItemsRestoreAbortsWhenVersionReadFails(t *testing.T) {
	fastRetryBackoff(t)
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
	cmd := newItemsRestoreCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"K"})
	err := cmd.Execute()
	if ExitCode(err) != 5 {
		t.Fatalf("ExitCode(restore error) = %d, want 5; err = %v", ExitCode(err), err)
	}
	if patchIssued {
		t.Fatal("PATCH issued after failed version read")
	}
}

func TestItemsRestoreMissingItemIsAnError(t *testing.T) {
	patchIssued := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			http.Error(w, "missing", http.StatusNotFound)
		case http.MethodPatch:
			patchIssued = true
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected", http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()

	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	cmd := newItemsRestoreCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"K"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("restore of nonexistent item: want error, got nil")
	}
	if ExitCode(err) != 3 {
		t.Fatalf("ExitCode(restore missing item) = %d, want 3 (not found); err = %v", ExitCode(err), err)
	}
	if patchIssued {
		t.Fatal("PATCH issued for a nonexistent item")
	}
}
