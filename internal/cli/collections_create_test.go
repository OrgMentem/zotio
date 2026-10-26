// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"zotio/internal/client"
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
			setupCreateRouteJournal(t)
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
			// Nothing applied, so this is not the partial-write exit 13.
			if err == nil || ExitCode(err) != 1 {
				t.Fatalf("collections create error = %v, exit=%d; want general failure (exit 1)", err, ExitCode(err))
			}
			for _, want := range tc.wantSubstr {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("collections create error = %q, want %q", err, want)
				}
			}
			// Explicit rejections fail; an unproven response preserves unknown evidence.
			var env mutation.Envelope
			if err := json.Unmarshal(out.Bytes(), &env); err != nil {
				t.Fatalf("decode failure output %q: %v", out.String(), err)
			}
			if env.OK || env.Result == nil || env.Result.Summary.Applied != 0 {
				t.Fatalf("envelope = %+v, must not report a failed batch as successful", env)
			}
			if tc.name == "non_envelope_body" {
				assertCreateRouteUnknown(t, env)
			} else if env.Result.Summary.Failed != 1 {
				t.Fatalf("explicit rejection must fail: %+v", env.Result)
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
		`[{"name":"a"},{"name":"b"},{"name":"c"}]`,
		`{"success":{"0":"C1","2":"C3"},"successful":{},"unchanged":{},"failed":{"1":{"code":400,"message":"name is required"}}}`)
}

// TestCollectionsCreateLostResponseIsNotRetryable proves a create whose
// response is lost is not reported as "unreachable, retry". The POST carries a
// write token derived from the body, so a replay answers 412 instead of
// creating a second collection, and the command reports a committed-unknown
// conflict naming the collection rather than inviting a duplicating retry.
func TestCollectionsCreateLostResponseIsNotRetryable(t *testing.T) {
	for _, tc := range []struct {
		name    string
		replay  func(w http.ResponseWriter)
		wantErr string
	}{
		{
			name: "replay_answers_412",
			replay: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusPreconditionFailed)
				_, _ = w.Write([]byte("Write token already submitted"))
			},
			wantErr: "Write token already submitted",
		},
		{
			name:    "every_attempt_dropped",
			replay:  nil,
			wantErr: "ambiguous",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(client.SetRetryBackoffBaseForTest(time.Millisecond))
			// The handler can still be running a dropped attempt when the
			// command returns, so every access to the recorded requests locks.
			var mu sync.Mutex
			var tokens []string
			var bodies []string
			recorded := func() ([]string, []string) {
				mu.Lock()
				defer mu.Unlock()
				return append([]string(nil), tokens...), append([]string(nil), bodies...)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/users/0/collections" {
					http.NotFound(w, r)
					return
				}
				raw, _ := io.ReadAll(r.Body)
				mu.Lock()
				tokens = append(tokens, r.Header.Get("Zotero-Write-Token"))
				bodies = append(bodies, string(raw))
				attempts := len(tokens)
				mu.Unlock()
				if attempts > 1 && tc.replay != nil {
					tc.replay(w)
					return
				}
				// The first request commits on the server in a real outage;
				// the stub only needs to lose the response.
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Errorf("hijack: %v", err)
					return
				}
				_ = conn.Close()
			}))
			defer srv.Close()
			t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

			run := func() error {
				cmd := newCollectionsCreateCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1})
				cmd.SilenceErrors, cmd.SilenceUsage = true, true
				cmd.SetErr(io.Discard)
				cmd.SetOut(io.Discard)
				cmd.SetArgs([]string{"--name", "Reading list"})
				return cmd.Execute()
			}
			err := run()
			if err == nil {
				t.Fatal("collections create succeeded although no response was ever received")
			}
			sent, sentBodies := recorded()
			if ExitCode(err) == 9 || strings.Contains(err.Error(), "network connection and retry") {
				t.Fatalf("lost response reported as a retryable precondition: exit=%d err=%v", ExitCode(err), err)
			}
			for _, want := range []string{"refusing automatic retry", `"Reading list"`, "Zotero-Write-Token", tc.wantErr} {
				if want == "Zotero-Write-Token" {
					want = "write token " + sent[0]
				}
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q lacks %q", err, want)
				}
			}
			if len(sent) == 0 || sent[0] == "" {
				t.Fatalf("POST /collections carried no Zotero-Write-Token: %q", sent)
			}
			for i, token := range sent {
				if token != sent[0] || sentBodies[i] != sentBodies[0] {
					t.Fatalf("attempt %d changed token/body: %q vs %q", i, token, sent[0])
				}
			}
			first := sent[0]
			mu.Lock()
			tokens, bodies = nil, nil
			mu.Unlock()
			_ = run()
			again, _ := recorded()
			if len(again) == 0 || again[0] != first {
				t.Fatalf("a second identical run sent token %q, want the same token %q", again, first)
			}
		})
	}
}
