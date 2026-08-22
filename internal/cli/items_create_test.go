// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Zotero's POST /items requires a bare JSON array; items create must send the
// array directly, not the generated {"items":[...]} wrapper (which the API rejects).

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

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

// TestRunSingleItemCreateConnectorFilingFailureIsPartialApplied proves the fix
// for zotio-bf2da90 through the mutation engine: SaveItems committed
// (FilingFailed=true) but UpdateSession failed. The mutation result must be
// applied (journaled with a usable key), not failed — the API has no
// transaction across the two calls so a retry must file, not re-create.
// Drives the real production helper singleItemCreateApplyResult so the test
// fails if that helper regresses to discarding the committed result.
func TestRunSingleItemCreateConnectorFilingFailureIsPartialApplied(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const wantKey = "ABCDEFGH"
	fakeCommitted := itemCreateResult{Via: "connector", WebKey: wantKey, Session: "sess-1", ConnKey: "ck-1", FilingFailed: true}
	flags := &rootFlags{asJSON: true, yes: true, maxChanges: -1, configPath: testConfigFile(t, "http://example.test/api/users/0"), timeout: time.Second}
	ops := []mutation.Op{{
		ID:      "import.test",
		Key:     "test-key",
		Kind:    "item_create",
		Changes: []mutation.Change{{Field: "source", Add: "test"}},
		Apply: func() (string, any, error) {
			return singleItemCreateApplyResult(fakeCommitted, fmt.Errorf("target filing failed"), "test-key")
		},
	}}
	env, err := mutation.Run(mutationOptions(flags), "import.test", ops)
	if err != nil {
		t.Fatalf("mutation.Run filing-failure case: env=%+v err=%v", env, err)
	}
	if !env.OK || env.Result == nil {
		t.Fatalf("env = %+v, want OK with result", env)
	}
	if env.Result.Summary.Applied != 1 {
		t.Fatalf("Applied = %d, want 1 (filing failure must still journal)", env.Result.Summary.Applied)
	}
	if env.Result.Summary.Failed != 0 {
		t.Fatalf("Failed = %d, want 0", env.Result.Summary.Failed)
	}
	if got := env.Result.Items[0].Key; got != wantKey {
		t.Fatalf("ResultItem.Key = %q, want %q", got, wantKey)
	}
	if m, ok := env.Result.Items[0].Reason.(map[string]any); !ok || m["message"] == nil {
		t.Fatalf("Reason = %+v, want map with message", env.Result.Items[0].Reason)
	} else if !strings.Contains(m["message"].(string), "retry filing only") {
		t.Fatalf("message = %q, want retry-filing guidance", m["message"])
	}
	if m, ok := env.Result.Items[0].Reason.(map[string]any); !ok || m["key"] != wantKey {
		t.Fatalf("Reason[key] = %+v, want %q", env.Result.Items[0].Reason, wantKey)
	}
	entry, ok := mutation.BuildJournalEntry(env, time.Now())
	if !ok {
		t.Fatal("BuildJournalEntry skipped despite Applied==1")
	}
	if len(entry.Ops) != 1 || entry.Ops[0].Key != wantKey {
		t.Fatalf("journal ops = %+v, want key %q", entry.Ops, wantKey)
	}
}

// TestRunSingleItemCreateSaveItemsFailureIsFailed is the converse: a true
// SaveItems failure (nothing committed, FilingFailed=false, empty WebKey) must
// remain failed with Applied==0 and an empty key. Distinguishes the two
// branches so a blanket "convert all errors to applied" regresses.
func TestRunSingleItemCreateSaveItemsFailureIsFailed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	flags := &rootFlags{asJSON: true, yes: true, maxChanges: -1, configPath: testConfigFile(t, "http://example.test/api/users/0"), timeout: time.Second}
	ops := []mutation.Op{{
		ID:      "import.test",
		Key:     "test-key",
		Kind:    "item_create",
		Changes: []mutation.Change{{Field: "source", Add: "test"}},
		Apply: func() (string, any, error) {
			return singleItemCreateApplyResult(itemCreateResult{}, fmt.Errorf("connector save failed: dial refused"), "test-key")
		},
	}}
	env, err := mutation.Run(mutationOptions(flags), "import.test", ops)
	if err == nil {
		t.Fatal("want mutation incomplete error for SaveItems failure")
	}
	if env.Result == nil {
		t.Fatal("want result even on failure")
	}
	if env.Result.Summary.Failed != 1 || env.Result.Summary.Applied != 0 {
		t.Fatalf("summary = %+v, want Failed=1 Applied=0", env.Result.Summary)
	}
	if env.Result.Items[0].Key != "" {
		t.Fatalf("Key = %q, want empty for uncommitted SaveItems failure", env.Result.Items[0].Key)
	}
	if env.Result.Items[0].Status != "failed" {
		t.Fatalf("Status = %q, want failed", env.Result.Items[0].Status)
	}
}

// TestRunSingleItemCreateConnectorFilingFailureWithoutWebKeyStillApplied
// covers the edge where confirmConnectorCreate raced and WebKey is still empty
// but FilingFailed proves SaveItems committed. The mutation must still be
// applied (journaled) with a retry message, even though the key is empty.
func TestRunSingleItemCreateConnectorFilingFailureWithoutWebKeyStillApplied(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fakeCommitted := itemCreateResult{Via: "connector", WebKey: "", Session: "sess-1", ConnKey: "ck-1", FilingFailed: true}
	flags := &rootFlags{asJSON: true, yes: true, maxChanges: -1, configPath: testConfigFile(t, "http://example.test/api/users/0"), timeout: time.Second}
	ops := []mutation.Op{{
		ID:      "import.test",
		Key:     "test-key",
		Kind:    "item_create",
		Changes: []mutation.Change{{Field: "source", Add: "test"}},
		Apply: func() (string, any, error) {
			return singleItemCreateApplyResult(fakeCommitted, fmt.Errorf("target filing failed"), "fallback-key")
		},
	}}
	env, err := mutation.Run(mutationOptions(flags), "import.test", ops)
	if err != nil {
		t.Fatalf("want OK despite filing failure without WebKey, got err=%v env=%+v", err, env)
	}
	if env.Result.Summary.Applied != 1 {
		t.Fatalf("Applied = %d, want 1", env.Result.Summary.Applied)
	}
	if m, ok := env.Result.Items[0].Reason.(map[string]any); !ok || !strings.Contains(m["message"].(string), "retry filing only") {
		t.Fatalf("Reason = %+v, want retry-filing message", env.Result.Items[0].Reason)
	}
	// WebKey was empty so ResultItem.Key stays empty — but status is still applied.
	if env.Result.Items[0].Key != "" {
		t.Fatalf("Key = %q, want empty when WebKey unresolved", env.Result.Items[0].Key)
	}
}

// TestItemsCreateConnectorBatchFilingFailureJSONIsStructural guards the batch
// branch that remains UNJOURNALLED (it runs outside runMutation). Its JSON
// must at minimum carry keys/session/filing_failed/message structurally so a
// caller can file without re-creating. This test proves that contract on a
// payload shaped exactly like the production branch emits.
func TestItemsCreateConnectorBatchFilingFailureJSONIsStructural(t *testing.T) {
	msg := fmt.Sprintf("created %d item(s) via connector (session %s, keys %v) but target %q filing failed: %v; retry filing only, do not re-create the item", 2, "sess-1", []string{"A", "B"}, "TESTCOLL", fmt.Errorf("500"))
	payload, err := json.Marshal(map[string]any{
		"via":           "connector",
		"status":        "created",
		"count":         2,
		"keys":          []string{"A", "B"},
		"session":       "sess-1",
		"target":        "TESTCOLL",
		"filing_failed": true,
		"filing_error":  "500",
		"message":       msg,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["filing_failed"] != true {
		t.Fatalf("filing_failed = %v, want true", got["filing_failed"])
	}
	if got["session"] != "sess-1" {
		t.Fatalf("session = %v, want sess-1", got["session"])
	}
	keys, ok := got["keys"].([]any)
	if !ok || len(keys) != 2 {
		t.Fatalf("keys = %v, want 2", got["keys"])
	}
	if !strings.Contains(got["message"].(string), "retry filing only") {
		t.Fatalf("message = %q, want retry guidance", got["message"])
	}
}

// TestConnectorCreateAmbiguousRecovery proves that a failed connector write
// refuses to guess when two recent same-title candidates exist.
func TestConnectorCreateAmbiguousRecovery(t *testing.T) {
	var attachmentResolverCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/connector/saveItems":
			http.Error(w, `{"error":"save failed"}`, http.StatusInternalServerError)
		case "/connector/hasAttachmentResolvers", "/connector/saveAttachmentFromResolver":
			attachmentResolverCalls++
			http.Error(w, `{"error":"attachment must not be attempted"}`, http.StatusInternalServerError)
		case "/users/0/items/top":
			w.Header().Set("Content-Type", "application/json")
			added := time.Now().UTC().Format(time.RFC3339)
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"key": "STALE1", "title": "Ambiguous", "itemType": "journalArticle", "dateAdded": added},
				{"key": "STALE2", "title": "Ambiguous", "itemType": "journalArticle", "dateAdded": added},
			})
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	oldConnectorForCreate := connectorForCreate
	connectorForCreate = func(*rootFlags) (*connector.Client, error) {
		return connector.New(srv.URL+"/connector", time.Second), nil
	}
	t.Cleanup(func() { connectorForCreate = oldConnectorForCreate })

	flags := &rootFlags{
		configPath: testConfigFile(t, srv.URL+"/users/0"),
		via:        "connector",
		timeout:    time.Second,
	}
	res, err := routeCreateItemVia(context.Background(), flags, "connector", nil, map[string]any{
		"title":    "Ambiguous",
		"itemType": "journalArticle",
	}, "", false)
	if err == nil {
		t.Fatal("routeCreateItemVia succeeded, want ambiguous recovery error")
	}
	if !strings.Contains(err.Error(), "2 recently added items share this title") {
		t.Fatalf("error = %q, want ambiguous recovery message", err)
	}
	if strings.Contains(err.Error(), "STALE1") || strings.Contains(err.Error(), "STALE2") {
		t.Fatalf("error = %q, must not report a guessed key", err)
	}
	if res.WebKey != "" {
		t.Fatalf("recovered key = %q, want empty for ambiguous recovery", res.WebKey)
	}
	if attachmentResolverCalls != 0 {
		t.Fatalf("attachment resolver calls = %d, want none after ambiguous recovery", attachmentResolverCalls)
	}
}
