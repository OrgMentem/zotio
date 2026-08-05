// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Zotero's POST /items requires a bare JSON array; items create must send the
// array directly, not the generated {"items":[...]} wrapper (which the API rejects).

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"zotio/internal/connector"
	"zotio/internal/mutation"
)

func TestItemsCreateSendsBareArray(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":{"0":"NEWKEY11"},"successful":{},"unchanged":{},"failed":{}}`))
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	cmd := newItemsCreateCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--items", `[{"itemType":"journalArticle","title":"x"}]`})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("items create: %v", err)
	}

	// Zotero requires a bare array; unmarshaling into a slice must succeed.
	var arr []map[string]any
	if err := json.Unmarshal(gotBody, &arr); err != nil {
		t.Fatalf("create body is not a JSON array: %s (%v)", gotBody, err)
	}
	if len(arr) != 1 || arr[0]["itemType"] != "journalArticle" {
		t.Errorf("unexpected create body: %s", gotBody)
	}
}

func TestItemsCreateConnectorDryRunDoesNotWrite(t *testing.T) {
	oldPing := connectorPing
	defer func() { connectorPing = oldPing }()
	var connectorChecks int
	connectorPing = func(ctx context.Context, c *connector.Client) error {
		connectorChecks++
		return nil
	}

	flags := &rootFlags{asJSON: true, via: "connector", configPath: testConfigFile(t, "http://localhost:23119/api/users/0"), dryRun: true}
	cmd := newItemsCreateCmd(flags)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--items", `[{"itemType":"book","title":"dry"}]`})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("items create dry-run: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode output: %v; %s", err, out.String())
	}
	if got["mode"] != "preview" || got["preview_reason"] != "dry_run" {
		t.Fatalf("output = %+v, want dry-run preview envelope", got)
	}
	plan, ok := got["plan"].(map[string]any)
	if !ok {
		t.Fatalf("output = %+v, want preview plan", got)
	}
	operations, ok := plan["operations"].([]any)
	if !ok || len(operations) != 1 {
		t.Fatalf("plan = %+v, want one planned create operation", plan)
	}
	if connectorChecks != 0 {
		t.Fatalf("connector checks = %d, want no connector access in preview", connectorChecks)
	}
}

func TestItemsCreateReportsBatchWriteFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"successful":{},"success":{},"unchanged":{},"failed":{"0":{"code":400,"message":"itemType is required"}}}`))
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	cmd := newItemsCreateCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--items", `[{"title":"x"}]`})
	err := cmd.Execute()
	if err == nil || ExitCode(err) != 13 {
		t.Fatalf("items create error = %v, exit=%d; want degraded failure", err, ExitCode(err))
	}
	for _, want := range []string{"index 0", "code 400", "itemType is required"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("items create error = %q, want %q", err, want)
		}
	}
}

// TestItemsCreatePartialBatchIsJournaled proves the fix for the write-safety
// defect where recordMutationJournal (Applied == 0 skips recording) erased
// the journal entry for an entirely successful sub-batch just because one
// sibling element in the same POST was rejected. Zotero answers a batch
// write with HTTP 200 even when it rejects some elements, and the elements it
// did not reject were still created in the library -- so the run must still
// be journaled, with an accurate applied/failed split.
func TestItemsCreatePartialBatchIsJournaled(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mutationJournalRecorder = recordMutationJournal
	t.Cleanup(func() { mutationJournalRecorder = nil })

	requestCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":{"0":"K1","2":"K3"},"successful":{},"unchanged":{},"failed":{"1":{"code":400,"message":"itemType is required"}}}`))
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	cmd := newItemsCreateCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetErr(io.Discard)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--items", `[{"itemType":"journalArticle","title":"a"},{"title":"b"},{"itemType":"journalArticle","title":"c"}]`})
	err := cmd.Execute()
	if err == nil || ExitCode(err) != 13 {
		t.Fatalf("items create error = %v, exit=%d; want degraded failure", err, ExitCode(err))
	}
	if requestCount != 1 {
		t.Fatalf("requests = %d, want exactly 1 batched POST for a 3-item body", requestCount)
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

// TestItemsCreateReadsStdinFromCommandReader guards the MCP stdin-hijack fix:
// under a stdio MCP server, os.Stdin IS the JSON-RPC transport, so --stdin
// must read cmd.InOrStdin(), never the process stdin.
func TestItemsCreateReadsStdinFromCommandReader(t *testing.T) {
	var gotBody []byte
	requestCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":{"0":"NEWKEY11"},"successful":{},"unchanged":{},"failed":{}}`))
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	// Point the real process stdin at an already-closed pipe. If the command
	// fell back to os.Stdin it would read zero bytes (immediate EOF) and fail
	// to parse JSON, instead of reading the item supplied via cmd.SetIn below.
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	if err := pw.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	origStdin := os.Stdin
	os.Stdin = pr
	t.Cleanup(func() {
		os.Stdin = origStdin
		pr.Close()
	})

	cmd := newItemsCreateCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetIn(strings.NewReader(`[{"itemType":"journalArticle","title":"From cmd.SetIn"}]`))
	cmd.SetArgs([]string{"--stdin"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("items create from stdin: %v (%s)", err, out.String())
	}

	if requestCount != 1 {
		t.Fatalf("requests = %d, want exactly 1 -- command must read cmd.InOrStdin(), not the process stdin", requestCount)
	}
	var arr []map[string]any
	if err := json.Unmarshal(gotBody, &arr); err != nil {
		t.Fatalf("create body is not a JSON array: %s (%v)", gotBody, err)
	}
	if len(arr) != 1 || arr[0]["title"] != "From cmd.SetIn" {
		t.Fatalf("posted body = %s, want the item supplied via cmd.SetIn", gotBody)
	}
}

func TestItemsCreateAcceptsSingleObjectResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"key":"NEWKEY11","version":1}`))
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	cmd := newItemsCreateCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--items", `[{"itemType":"journalArticle","title":"x"}]`})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("items create with a single-object response: %v", err)
	}
}

// TestItemsCreateChargesEachItemAgainstMaxChanges guards against the
// mutation.CheckGates blind spot where a single batched Op (one Op, N
// Changes) charges an N-item array as 1 planned operation, letting
// --max-changes sail past regardless of how many items are in the array.
// Deleting the pre-write itemsCreatePreflightOps gate check must make this
// test fail: a 3-item array with --max-changes 2 must be refused before any
// HTTP request is issued.
func TestItemsCreateChargesEachItemAgainstMaxChanges(t *testing.T) {
	requestCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":{"0":"K1","1":"K2","2":"K3"},"successful":{},"unchanged":{},"failed":{}}`))
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	cmd := newItemsCreateCmd(&rootFlags{asJSON: true, yes: true, maxChanges: 2})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--items", `[{"itemType":"journalArticle","title":"a"},{"itemType":"journalArticle","title":"b"},{"itemType":"journalArticle","title":"c"}]`})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("items create apply succeeded, want max_changes_exceeded refusal")
	}
	if !strings.Contains(err.Error(), "planned 3 change(s)") || !strings.Contains(err.Error(), "cap of 2") {
		t.Fatalf("error = %q, want the per-item count and cap", err)
	}
	if requestCount != 0 {
		t.Fatalf("requests = %d, want 0 -- refusal must happen before any network call", requestCount)
	}
}

// TestItemsCreateBatchesUnderCapIntoOnePost proves the gate added for
// TestItemsCreateChargesEachItemAgainstMaxChanges charges N items against
// --max-changes without splitting the actual write: a 3-item array under the
// cap still results in exactly one POST carrying all three items.
func TestItemsCreateBatchesUnderCapIntoOnePost(t *testing.T) {
	requestCount := 0
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":{"0":"K1","1":"K2","2":"K3"},"successful":{},"unchanged":{},"failed":{}}`))
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	cmd := newItemsCreateCmd(&rootFlags{asJSON: true, yes: true, maxChanges: 3})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--items", `[{"itemType":"journalArticle","title":"a"},{"itemType":"journalArticle","title":"b"},{"itemType":"journalArticle","title":"c"}]`})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("items create under cap: %v", err)
	}
	if requestCount != 1 {
		t.Fatalf("requests = %d, want exactly 1 batched POST", requestCount)
	}
	var arr []map[string]any
	if err := json.Unmarshal(gotBody, &arr); err != nil {
		t.Fatalf("create body is not a JSON array: %s (%v)", gotBody, err)
	}
	if len(arr) != 3 {
		t.Fatalf("batched body has %d items, want 3 sent in one request", len(arr))
	}
}

// TestItemsCreateConnectorRouteChargesEachItemAgainstMaxChanges proves the
// same pre-write gate protects the desktop-connector branch, which writes via
// conn.SaveItems and (before this fix) never reached any --max-changes check
// at all. A 3-item array with --max-changes 2 routed through the connector
// must be refused before the connector is ever contacted.
func TestItemsCreateConnectorRouteChargesEachItemAgainstMaxChanges(t *testing.T) {
	oldPing := connectorPing
	defer func() { connectorPing = oldPing }()
	var connectorChecks int
	connectorPing = func(ctx context.Context, c *connector.Client) error {
		connectorChecks++
		return nil
	}

	flags := &rootFlags{asJSON: true, via: "connector", yes: true, maxChanges: 2, configPath: testConfigFile(t, "http://localhost:23119/api/users/0")}
	cmd := newItemsCreateCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--items", `[{"itemType":"book","title":"a"},{"itemType":"book","title":"b"},{"itemType":"book","title":"c"}]`})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("items create connector apply succeeded, want max_changes_exceeded refusal")
	}
	if !strings.Contains(err.Error(), "planned 3 change(s)") || !strings.Contains(err.Error(), "cap of 2") {
		t.Fatalf("error = %q, want the per-item count and cap", err)
	}
	if connectorChecks != 0 {
		t.Fatalf("connector checks = %d, want 0 -- refusal must happen before the connector is contacted", connectorChecks)
	}
}
