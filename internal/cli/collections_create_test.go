// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zotio/internal/mutation"
)

func TestCollectionsCreateReportsBatchWriteFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"successful":{},"success":{},"unchanged":{},"failed":{"0":{"code":400,"message":"name is required"}}}`))
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	cmd := newCollectionsCreateCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetErr(io.Discard)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--name", "Example"})
	err := cmd.Execute()
	if err == nil || ExitCode(err) != 13 {
		t.Fatalf("collections create error = %v, exit=%d; want degraded failure", err, ExitCode(err))
	}
	for _, want := range []string{"index 0", "code 400", "name is required"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("collections create error = %q, want %q", err, want)
		}
	}
	if out.Len() != 0 {
		t.Fatalf("collections output = %q, must not report a failed batch as successful", out.String())
	}
}

// TestCollectionsCreatePartialBatchIsJournaled proves the fix for the
// write-safety defect where recordMutationJournal (Applied == 0 skips
// recording) erased the journal entry for an entirely successful sub-batch
// just because one sibling element in the same POST was rejected. Zotero
// answers a batch write with HTTP 200 even when it rejects some elements, and
// the elements it did not reject were still created in the library -- so the
// run must still be journaled, with an accurate applied/failed split.
func TestCollectionsCreatePartialBatchIsJournaled(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mutationJournalRecorder = recordMutationJournal
	t.Cleanup(func() { mutationJournalRecorder = nil })

	requestCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":{"0":"C1","2":"C3"},"successful":{},"unchanged":{},"failed":{"1":{"code":400,"message":"name is required"}}}`))
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	cmd := newCollectionsCreateCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetErr(io.Discard)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader(`[{"name":"a"},{"parentCollection":"X"},{"name":"c"}]`))
	cmd.SetArgs([]string{"--stdin"})
	err := cmd.Execute()
	if err == nil || ExitCode(err) != 13 {
		t.Fatalf("collections create error = %v, exit=%d; want degraded failure", err, ExitCode(err))
	}
	if requestCount != 1 {
		t.Fatalf("requests = %d, want exactly 1 batched POST for a 3-collection body", requestCount)
	}

	entries, listErr := mutation.ListEntries(helpersTestJournalDir(t))
	if listErr != nil {
		t.Fatalf("list journal entries: %v", listErr)
	}
	if len(entries) != 1 {
		t.Fatalf("journal entries = %d, want 1 recorded run even though the batch partially failed", len(entries))
	}
	if entries[0].Summary.Applied != 2 || entries[0].Summary.Failed != 1 {
		t.Fatalf("journaled summary = %+v, want 2 applied and 1 failed", entries[0].Summary)
	}
}
