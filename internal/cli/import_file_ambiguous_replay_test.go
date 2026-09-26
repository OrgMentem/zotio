// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Retrying an import file batch after a lost response must report a committed
// conflict, not a plain failure: the first POST may already have committed
// every record, and a second identical POST would duplicate them.
func TestImportFileLostBatchResponseReportsCommittedConflict(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mutationJournalRecorder = recordMutationJournal
	t.Cleanup(func() { mutationJournalRecorder = nil })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Zotero-Write-Token") == "" {
			t.Error("batch POST arrived without Zotero-Write-Token; the retry cannot replay the same token")
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("test server does not support hijacking")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	filePath := writeImportFixture(t, "@article{example,\n  title = {Example}\n}\n")
	env, raw, err := runImportFile(t, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, filePath)
	if err == nil {
		t.Fatalf("import error = nil, want committed-conflict refusal (%s)", raw)
	}
	// The engine reports per-op outcomes in the envelope; the top-level error
	// is the generic "mutation incomplete" wrapper.
	if env.Result == nil || len(env.Result.Items) != 1 || env.Result.Items[0].Status != "conflict" {
		t.Fatalf("result = %s, want one committed conflict", raw)
	}
	reason, _ := env.Result.Items[0].Reason.(map[string]any)
	msg, _ := reason["message"].(string)
	if !strings.Contains(msg, "refusing automatic retry") {
		t.Fatalf("conflict reason = %v, want the refusal message (%s)", env.Result.Items[0].Reason, raw)
	}
	if reason["committed"] != true || env.Journal["run_id"] == nil {
		t.Fatalf("ambiguous batch lost journal evidence: reason=%v journal=%v", reason, env.Journal)
	}
}

// A server may commit the POST before answering 500. The result must not
// encourage replay as though that HTTP response proved the records absent.
func TestImportFileServerErrorReportsAmbiguousConflict(t *testing.T) {
	fastRetryBackoff(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `internal error`, http.StatusInternalServerError)
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	filePath := writeImportFixture(t, "@article{example,\n  title = {Example}\n}\n")
	env, raw, err := runImportFile(t, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, filePath)
	if err == nil {
		t.Fatalf("import error = nil, want conflict (%s)", raw)
	}
	if env.Result == nil || len(env.Result.Items) != 1 || env.Result.Items[0].Status != "conflict" {
		t.Fatalf("result = %s, want ambiguous conflict", raw)
	}
	reason, _ := env.Result.Items[0].Reason.(map[string]any)
	msg, _ := reason["message"].(string)
	if !strings.Contains(msg, "refusing automatic retry") || !strings.Contains(msg, "500") {
		t.Fatalf("conflict reason = %v, want refusal with HTTP 500 (%s)", reason, raw)
	}
}
