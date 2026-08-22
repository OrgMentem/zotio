// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zotio/internal/mutation"
)

func TestItemsUpdateAbortsWhenVersionReadFails(t *testing.T) {
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
	cmd := newItemsUpdateCmd(&rootFlags{asJSON: true, yes: true})
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

// replacePathParam already percent-encodes the key; pre-escaping it here would
// double-encode path metacharacters and target a different resource.
func TestItemsUpdateEscapesItemKeyExactlyOnce(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Last-Modified-Version", "3")
			_, _ = w.Write([]byte(`{"key":"A/B","version":3,"data":{"key":"A/B","version":3}}`))
		case http.MethodPatch:
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected", http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()

	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	cmd := newItemsUpdateCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"A/B", "--title", "updated"})
	_ = cmd.Execute()

	if want := "/users/0/items/A%2FB"; gotPath != want {
		t.Fatalf("request path = %q, want %q (single escape)", gotPath, want)
	}
}
func TestItemsUpdateStdinUsesMutationEnvelopeAndJournalID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mutationJournalRecorder = recordMutationJournal
	t.Cleanup(func() { mutationJournalRecorder = nil })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Last-Modified-Version", "11")
			_, _ = w.Write([]byte(`{}`))
		case http.MethodPatch:
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected", http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	flags := &rootFlags{asJSON: true, yes: true, maxChanges: -1}
	cmd := newItemsUpdateCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{"ITEM1", "--stdin"})
	cmd.SetIn(strings.NewReader(`{"title":"updated"}`))
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("items update --stdin: %v", err)
	}

	var env mutation.Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode mutation envelope: %v (output=%s)", err, out.String())
	}
	if env.Result == nil || len(env.Result.Items) != 1 || env.Result.Items[0].Key != "ITEM1" {
		t.Fatalf("result = %+v, want result.items[0].key ITEM1", env.Result)
	}
	journal, ok := env.Journal.(map[string]any)
	if !ok || journal["run_id"] == nil || journal["run_id"] == "" {
		t.Fatalf("journal = %#v, want run_id", env.Journal)
	}
}

func TestItemsUpdateUsesApplyTimeWritePlaneVersion(t *testing.T) {
	var getCount int
	var patchHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			getCount++
			ver := "5"
			if getCount == 2 {
				ver = "9"
			}
			w.Header().Set("Last-Modified-Version", ver)
			_, _ = w.Write([]byte(`{"key":"K","version":` + ver + `,"data":{"key":"K","version":` + ver + `}}`))
		case http.MethodPatch:
			patchHeader = r.Header.Get("If-Unmodified-Since-Version")
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected", http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	cmd := newItemsUpdateCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"K", "--title", "updated"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("items update: %v (output %s)", err, out.String())
	}
	if patchHeader != "9" {
		t.Fatalf("If-Unmodified-Since-Version = %q, want %q (apply-time); getCount=%d", patchHeader, "9", getCount)
	}
	if getCount != 2 {
		t.Fatalf("GET count = %d, want 2 (plan + apply)", getCount)
	}
	var env mutation.Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v (output %s)", err, out.String())
	}
	if len(env.Plan.Operations) != 1 || env.Plan.Operations[0].ExpectedVersion != 5 {
		t.Fatalf("plan ExpectedVersion = %v, want 5 (plan-time)", env.Plan.Operations[0].ExpectedVersion)
	}
	if env.Result == nil || len(env.Result.Items) != 1 || env.Result.Items[0].Status != "applied" {
		t.Fatalf("result = %+v, want single applied item", env.Result)
	}
}

func TestItemsUpdateMapsPreconditionFailedToConflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Last-Modified-Version", "7")
			_, _ = w.Write([]byte(`{"key":"K","version":7,"data":{"key":"K","version":7}}`))
		case http.MethodPatch:
			http.Error(w, "stale version", http.StatusPreconditionFailed)
		default:
			http.Error(w, "unexpected", http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	flags := &rootFlags{asJSON: true, yes: true, maxChanges: -1}
	cmd := newItemsUpdateCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"K", "--title", "updated"})
	err := cmd.Execute()
	if err == nil {
		t.Fatalf("items update with 412 = nil error, want failure")
	}
	var env mutation.Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v (output %s) err %v", err, out.String(), err)
	}
	if env.Result == nil || len(env.Result.Items) != 1 {
		t.Fatalf("result = %+v, want one item", env.Result)
	}
	if got := env.Result.Items[0].Status; got != "conflict" {
		t.Fatalf("result status = %q, want %q (412 must map to conflict, not generic failed); reason=%v", got, "conflict", env.Result.Items[0].Reason)
	}
}

func TestItemsUpdateMapsPreconditionRequiredToConflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Last-Modified-Version", "7")
			_, _ = w.Write([]byte(`{"key":"K","version":7,"data":{"key":"K","version":7}}`))
		case http.MethodPatch:
			http.Error(w, "missing precondition", http.StatusPreconditionRequired)
		default:
			http.Error(w, "unexpected", http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	cmd := newItemsUpdateCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"K", "--title", "updated"})
	_ = cmd.Execute()
	var env mutation.Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env.Result == nil || env.Result.Items[0].Status != "conflict" {
		t.Fatalf("result status = %v, want conflict for 428; envelope=%s", env.Result, out.String())
	}
}
