// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"zotio/internal/mutation"
)

// runPartialBatchJournalingCase exercises the shared partial-batch invariant:
// Zotero answers a batch write with HTTP 200 even when it rejects some
// elements, and the elements it did not reject were still created in the
// library -- so the run must still be journaled, with an accurate
// applied/failed split. The collections and items create suites share this
// harness so a journal API change breaks one helper, not two copies.
func runPartialBatchJournalingCase(t *testing.T, label string, newCmd func(*rootFlags) *cobra.Command, args []string, stdinBody, responseBody string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	mutationJournalRecorder = recordMutationJournal
	t.Cleanup(func() { mutationJournalRecorder = nil })

	requestCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responseBody))
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	cmd := newCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetErr(io.Discard)
	var out bytes.Buffer
	cmd.SetOut(&out)
	if stdinBody != "" {
		cmd.SetIn(strings.NewReader(stdinBody))
	}
	cmd.SetArgs(args)
	err := cmd.Execute()
	if err == nil || ExitCode(err) != 13 {
		t.Fatalf("%s error = %v, exit=%d; want degraded failure", label, err, ExitCode(err))
	}
	if requestCount != 1 {
		t.Fatalf("requests = %d, want exactly 1 batched POST for a 3-element body", requestCount)
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

func TestCollectionsCreateFailClosedOnBadBatchResponse(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantSubstr []string
	}{
		{
			name:       "per_element_failure",
			body:       `{"successful":{},"success":{},"unchanged":{},"failed":{"0":{"code":400,"message":"name is required"}}}`,
			wantSubstr: []string{"index 0", "code 400", "name is required"},
		},
		{
			// A 2xx body that is not the batch envelope proves nothing: a proxy error
			// page must fail with unknown outcome, never report the collection applied.
			name: "non_envelope_body",
			body: `{"error":"proxy error"}`,
			wantSubstr: []string{
				"collections create",
				"not a batch envelope",
				"outcome of the 1 collection(s) in that request is unknown",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
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
			for _, want := range tc.wantSubstr {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("collections create error = %q, want %q", err, want)
				}
			}
			if out.Len() != 0 {
				t.Fatalf("collections output = %q, must not report a failed batch as successful", out.String())
			}
		})
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
	runPartialBatchJournalingCase(t, "collections create", newCollectionsCreateCmd,
		[]string{"--stdin"},
		`[{"name":"a"},{"parentCollection":"X"},{"name":"c"}]`,
		`{"success":{"0":"C1","2":"C3"},"successful":{},"unchanged":{},"failed":{"1":{"code":400,"message":"name is required"}}}`)
}
