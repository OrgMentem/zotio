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
	"net"
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

// TestItemsCreateConnectorBatchFilingFailureReportsRecoverableKeys drives the
// real command through the batch connector branch that stays UNJOURNALLED
// (it runs outside runMutation): conn.SaveItems commits, the follow-up
// conn.UpdateSession target filing fails, and the only durable record of the
// committed write is the JSON this branch prints plus the retry guidance in
// its error. Both must survive, and the batch must not be re-sent -- a caller
// that re-creates instead of re-filing duplicates every item in the library.
//
// The fixture is reached by redirecting dials for the desktop connector port
// (see redirectConnectorDialsToFixture) because this branch builds its client
// through flags.newConnector(), which accepts only 127.0.0.1:23119, and not
// through the connectorForCreate seam the single-item route uses.
func TestItemsCreateConnectorBatchFilingFailureReportsRecoverableKeys(t *testing.T) {
	const target = "C1234567"
	created := []struct{ key, title string }{
		{key: "BATCHIT1", title: "Batch Filing One"},
		{key: "BATCHIT2", title: "Batch Filing Two"},
	}
	for _, tc := range []struct {
		name     string
		surfaced bool
		wantKeys []any
		wantErr  string
		denyErr  string
	}{
		// Both recovery outcomes of the same failure: /items/top has already
		// surfaced the committed items, or has not surfaced them yet.
		{
			name:     "keys recovered",
			surfaced: true,
			wantKeys: []any{"BATCHIT1", "BATCHIT2"},
			wantErr:  "retry filing only, do not re-create the item",
			denyErr:  "retry filing, not creation",
		},
		{
			name:     "no keys recovered",
			surfaced: false,
			wantKeys: nil,
			wantErr:  "items remain; retry filing, not creation",
			denyErr:  "keys [",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			// config.Load applies ZOTERO_BASE_URL AFTER reading configPath, so an
			// inherited value silently redirects this test's base URL away from
			// the fixture. Clear the ambient credentials too.
			t.Setenv("ZOTIO_DEMO", "")
			t.Setenv("ZOTERO_BASE_URL", "")
			t.Setenv("ZOTERO_API_KEY", "")
			oldWindow, oldInterval := connectorCreateRecoveryWindow, connectorCreateRecoveryInterval
			connectorCreateRecoveryWindow = 0
			connectorCreateRecoveryInterval = time.Millisecond
			t.Cleanup(func() {
				connectorCreateRecoveryWindow = oldWindow
				connectorCreateRecoveryInterval = oldInterval
			})

			var saveItems, updateSessions int
			var savedIDs, savedTitles []string
			var gotTarget string
			var saveSession, filedSession string
			var refreshMethods, recoveryRequests []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/connector/ping":
					w.WriteHeader(http.StatusOK)
				case "/connector/saveItems":
					saveItems++
					var payload struct {
						SessionID string `json:"sessionID"`
						Items     []struct {
							ID    string `json:"id"`
							Title string `json:"title"`
						} `json:"items"`
					}
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Errorf("decode saveItems body: %v", err)
					}
					saveSession = payload.SessionID
					for _, item := range payload.Items {
						savedIDs = append(savedIDs, item.ID)
						savedTitles = append(savedTitles, item.Title)
					}
					w.WriteHeader(http.StatusCreated)
				case "/connector/updateSession":
					updateSessions++
					var payload struct {
						SessionID string `json:"sessionID"`
						Target    string `json:"target"`
					}
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Errorf("decode updateSession body: %v", err)
					}
					filedSession = payload.SessionID
					gotTarget = payload.Target
					http.Error(w, "simulated target filing failure", http.StatusInternalServerError)
				case "/api/users/0/items":
					// refreshItemsFromLocalAPI's incremental store sync.
					refreshMethods = append(refreshMethods, r.Method)
					w.Header().Set("Last-Modified-Version", "1")
					_ = json.NewEncoder(w).Encode([]any{})
				case "/api/users/0/items/top":
					// confirmConnectorCreate's per-item key recovery. Its sorted,
					// bounded shape is what keeps recovery from matching a stale
					// same-title item, so record the exact request it sends.
					recoveryRequests = append(recoveryRequests, r.Method+" "+r.URL.Path+"?"+r.URL.Query().Encode())
					rows := []any{}
					if tc.surfaced {
						added := time.Now().UTC().Format(time.RFC3339)
						for _, item := range created {
							rows = append(rows, map[string]any{
								"key": item.key, "title": item.title,
								"itemType": "journalArticle", "dateAdded": added,
							})
						}
					}
					_ = json.NewEncoder(w).Encode(rows)
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(srv.Close)
			redirectConnectorDialsToFixture(t, srv)

			flags := &rootFlags{
				asJSON: true, yes: true, via: "connector", connectorTarget: target,
				timeout: 5 * time.Second, maxChanges: -1,
				configPath: testConfigFile(t, "http://127.0.0.1:23119/api/users/0"),
			}
			cmd := newItemsCreateCmd(flags)
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(io.Discard)
			cmd.SilenceErrors, cmd.SilenceUsage = true, true
			cmd.SetArgs([]string{"--items", fmt.Sprintf(
				`[{"itemType":"journalArticle","title":%q},{"itemType":"journalArticle","title":%q}]`,
				created[0].title, created[1].title)})
			err := cmd.Execute()

			if err == nil {
				t.Fatalf("items create succeeded; want a filing failure. stdout=%s", out.String())
			}
			for _, want := range []string{"created 2 item(s) via connector", tc.wantErr} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error = %q, want it to contain %q", err, want)
				}
			}
			if strings.Contains(err.Error(), tc.denyErr) {
				t.Fatalf("error = %q, must not contain %q for this recovery outcome", err, tc.denyErr)
			}

			dec := json.NewDecoder(&out)
			var got map[string]any
			if decErr := dec.Decode(&got); decErr != nil {
				t.Fatalf("stdout is not one JSON object: %v; stdout=%q", decErr, out.String())
			}
			var trailing any
			if decErr := dec.Decode(&trailing); decErr != io.EOF {
				t.Fatalf("stdout carries a second JSON value (%+v, err=%v), want exactly one object", trailing, decErr)
			}
			if got["filing_failed"] != true {
				t.Fatalf("filing_failed = %v, want true", got["filing_failed"])
			}
			if got["status"] != "created" {
				t.Fatalf("status = %v, want created (SaveItems committed)", got["status"])
			}
			if got["count"] != float64(2) {
				t.Fatalf("count = %v, want 2", got["count"])
			}
			if got["via"] != "connector" {
				t.Fatalf("via = %v, want connector", got["via"])
			}
			if got["target"] != target {
				t.Fatalf("target = %v, want %q", got["target"], target)
			}
			session, _ := got["session"].(string)
			if session == "" {
				t.Fatalf("session = %v, want the connector save session that holds the committed items", got["session"])
			}
			if !strings.Contains(err.Error(), session) {
				t.Fatalf("error = %q, want it to name session %q so the JSON and the error correlate", err, session)
			}
			filingError, _ := got["filing_error"].(string)
			if !strings.Contains(filingError, "connector updateSession: HTTP 500") {
				t.Fatalf("filing_error = %q, want the updateSession failure", got["filing_error"])
			}
			message, _ := got["message"].(string)
			if !strings.Contains(message, "retry filing only") {
				t.Fatalf("message = %q, want retry-filing guidance", got["message"])
			}
			// A nil recovered slice marshals to JSON null (items_create.go:169),
			// so "keys" must be PRESENT and null. A payload that omits the field
			// entirely is a different, undocumented shape.
			keys, present := got["keys"]
			if !present {
				t.Fatalf("keys field absent from %v, want it present on every filing-failure payload", got)
			}
			if tc.wantKeys == nil {
				if keys != nil {
					t.Fatalf("keys = %v, want null when no committed item has surfaced yet", keys)
				}
			} else if gotKeys, ok := keys.([]any); !ok || fmt.Sprint(gotKeys) != fmt.Sprint(tc.wantKeys) {
				t.Fatalf("keys = %v, want %v", keys, tc.wantKeys)
			}
			for _, key := range tc.wantKeys {
				if !strings.Contains(err.Error(), key.(string)) {
					t.Fatalf("error = %q, want it to name recovered key %v", err, key)
				}
			}

			if saveItems != 1 {
				t.Fatalf("saveItems requests = %d, want exactly 1 -- a filing failure must never re-create the batch", saveItems)
			}
			if updateSessions != 1 {
				t.Fatalf("updateSession requests = %d, want exactly 1", updateSessions)
			}
			if gotTarget != target {
				t.Fatalf("updateSession target = %q, want %q", gotTarget, target)
			}
			if len(savedIDs) != 2 {
				t.Fatalf("saveItems connector ids = %v, want exactly two ids in one call", savedIDs)
			}
			if savedIDs[0] == "" || savedIDs[1] == "" {
				t.Fatalf("saveItems connector ids = %v, want every item to carry a non-empty connector id", savedIDs)
			}
			if savedIDs[0] == savedIDs[1] {
				t.Fatalf("saveItems connector ids = %v, want two distinct ids", savedIDs)
			}
			if fmt.Sprint(savedTitles) != fmt.Sprint([]string{created[0].title, created[1].title}) {
				t.Fatalf("saveItems titles = %v, want both items in one call", savedTitles)
			}

			// The filing must run against the session the batch was saved into:
			// filing another session files nothing from the committed batch, yet
			// still fails exactly like this fixture's forced 500.
			if saveSession == "" || filedSession == "" {
				t.Fatalf("saveItems sessionID = %q, updateSession sessionID = %q, want both non-empty", saveSession, filedSession)
			}
			if saveSession != filedSession {
				t.Fatalf("updateSession sessionID = %q, want the saveItems session %q", filedSession, saveSession)
			}
			if saveSession != session {
				t.Fatalf("reported session = %q, want the session the batch was saved into (%q)", session, saveSession)
			}
			// The mirror refresh is best-effort, so require at least one GET
			// rather than an exact count: it must have run, but its page count
			// belongs to sync, not to this contract.
			if len(refreshMethods) == 0 {
				t.Fatal("no request to /api/users/0/items; a committed batch must refresh the local mirror even when filing fails")
			}
			for _, method := range refreshMethods {
				if method != http.MethodGet {
					t.Fatalf("store refresh method = %q, want GET", method)
				}
			}
			// Key recovery is one sorted, bounded lookup per committed item; the
			// recovery window is collapsed to zero above, so it cannot re-poll.
			wantRecovery := fmt.Sprintf("GET /api/users/0/items/top?direction=desc&limit=%d&sort=dateAdded", recentItemLookupLimit)
			if len(recoveryRequests) != len(created) {
				t.Fatalf("key-recovery requests = %v, want one per committed item (%d)", recoveryRequests, len(created))
			}
			for _, req := range recoveryRequests {
				if req != wantRecovery {
					t.Fatalf("key-recovery request = %q, want %q -- an unsorted or unbounded lookup can match a stale same-title item", req, wantRecovery)
				}
			}
		})
	}
}

// redirectConnectorDialsToFixture points every dial for the Zotero desktop
// ports at srv and refuses any other address, so a connector command runs
// hermetically against the fixture.
//
// flags.newConnector() hardcodes 127.0.0.1:23119, so a command-level connector
// test cannot be handed an httptest port. Binding 23119 is not an option
// either: a running Zotero desktop owns it, which turns the test into a skip
// on a developer machine -- and a dial that did reach the real desktop would
// create real items in the operator's library.
func redirectConnectorDialsToFixture(t *testing.T, srv *httptest.Server) {
	t.Helper()
	fixture := srv.Listener.Addr().String()
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	redirect := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			switch addr {
			case "127.0.0.1:23119", "localhost:23119", "[::1]:23119":
				return dialer.DialContext(ctx, network, fixture)
			default:
				return nil, fmt.Errorf("test transport refused a dial to %s", addr)
			}
		},
	}
	old := http.DefaultTransport
	http.DefaultTransport = redirect
	t.Cleanup(func() {
		http.DefaultTransport = old
		redirect.CloseIdleConnections()
	})
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
