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
	"sort"
	"strings"
	"testing"
	"time"

	"zotio/internal/config"
	"zotio/internal/connector"
	"zotio/internal/mutation"
	"zotio/internal/store"
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

// itemsCreateFixtureItem is one item the fixture's read plane reports once the
// connector has committed it.
type itemsCreateFixtureItem struct{ key, title string }

// itemsCreateConnectorFixture stands in for Zotero desktop: the connector
// endpoints items create writes through, plus the read-plane endpoints key
// recovery (/items/top) and the fallback mirror refresh (/items) use.
type itemsCreateConnectorFixture struct {
	srv            *httptest.Server
	filingStatus   int
	surfaced       []itemsCreateFixtureItem
	saveItems      int
	updateSessions int
	savedIDs       []string
	savedTitles    []string
	saveSession    string
	filedSession   string
	filedTarget    string
	refreshMethods []string
	recoveryReqs   []string
}

// newItemsCreateConnectorFixture serves a desktop that accepts every save and,
// when filingStatus is non-zero, rejects the follow-up target filing with it.
//
// The fixture is reached by redirecting dials for the desktop connector port
// (see redirectConnectorDialsToFixture) because this route builds its client
// through flags.newConnector(), which accepts only 127.0.0.1:23119, and not
// through the connectorForCreate seam the single-item route uses.
func newItemsCreateConnectorFixture(t *testing.T, surfaced []itemsCreateFixtureItem, filingStatus int) *itemsCreateConnectorFixture {
	t.Helper()
	fixture := &itemsCreateConnectorFixture{filingStatus: filingStatus, surfaced: surfaced}
	fixture.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/connector/ping":
			w.WriteHeader(http.StatusOK)
		case "/connector/saveItems":
			fixture.saveItems++
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
			fixture.saveSession = payload.SessionID
			for _, item := range payload.Items {
				fixture.savedIDs = append(fixture.savedIDs, item.ID)
				fixture.savedTitles = append(fixture.savedTitles, item.Title)
			}
			w.WriteHeader(http.StatusCreated)
		case "/connector/updateSession":
			fixture.updateSessions++
			var payload struct {
				SessionID string `json:"sessionID"`
				Target    string `json:"target"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("decode updateSession body: %v", err)
			}
			fixture.filedSession = payload.SessionID
			fixture.filedTarget = payload.Target
			if fixture.filingStatus != 0 {
				http.Error(w, "simulated target filing failure", fixture.filingStatus)
				return
			}
			w.WriteHeader(http.StatusOK)
		case "/api/users/0/items":
			// refreshItemsFromLocalAPI's incremental store sync.
			fixture.refreshMethods = append(fixture.refreshMethods, r.Method)
			w.Header().Set("Last-Modified-Version", "1")
			_ = json.NewEncoder(w).Encode([]any{})
		case "/api/users/0/items/top":
			// confirmConnectorCreate's per-item key recovery. Its sorted,
			// bounded shape is what keeps recovery from matching a stale
			// same-title item, so record the exact request it sends.
			fixture.recoveryReqs = append(fixture.recoveryReqs, r.Method+" "+r.URL.Path+"?"+r.URL.Query().Encode())
			rows := []any{}
			added := time.Now().UTC().Format(time.RFC3339)
			for _, item := range fixture.surfaced {
				rows = append(rows, map[string]any{
					"key": item.key, "title": item.title,
					"itemType": "journalArticle", "dateAdded": added,
				})
			}
			_ = json.NewEncoder(w).Encode(rows)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fixture.srv.Close)
	redirectConnectorDialsToFixture(t, fixture.srv)
	return fixture
}

// isolateItemsCreateConnectorEnv clears the ambient Zotero environment.
// config.Load applies ZOTERO_BASE_URL AFTER reading configPath, so an inherited
// value silently redirects the test's base URL away from the fixture, and an
// inherited key would let a Web write escape the fixture entirely.
func isolateItemsCreateConnectorEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
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
}

// runItemsCreate drives the real command and returns its stdout and error.
func runItemsCreate(t *testing.T, flags *rootFlags, args ...string) (string, error) {
	t.Helper()
	cmd := newItemsCreateCmd(flags)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// decodeSingleJSONObject fails unless stdout is exactly one JSON object. Two
// objects would mean the command emitted a second, route-specific payload
// alongside the mutation envelope.
func decodeSingleJSONObject(t *testing.T, stdout string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(stdout))
	var got map[string]any
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("stdout is not one JSON object: %v; stdout=%q", err, stdout)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		t.Fatalf("stdout carries a second JSON value (%+v, err=%v), want exactly one object", trailing, err)
	}
	return got
}

// TestItemsCreateConnectorBatchFilingFailureIsAppliedAndJournaled drives the
// real command through the connector route when conn.SaveItems commits and the
// follow-up conn.UpdateSession target filing fails. There is no transaction
// across the two calls, so the items exist: the run must be journaled with the
// recovered keys, every item must report applied, and the error must say to
// retry the filing rather than the create -- a caller that re-creates instead of
// re-files duplicates every item in the library.
func TestItemsCreateConnectorBatchFilingFailureIsAppliedAndJournaled(t *testing.T) {
	const target = "C1234567"
	created := []itemsCreateFixtureItem{
		{key: "BATCHIT1", title: "Batch Filing One"},
		{key: "BATCHIT2", title: "Batch Filing Two"},
	}
	for _, tc := range []struct {
		name     string
		surfaced []itemsCreateFixtureItem
		wantKeys []string
		wantErr  string
		denyErr  string
	}{
		// Both recovery outcomes of the same failure: /items/top has already
		// surfaced the committed items, or has not surfaced them yet.
		{
			name:     "keys recovered",
			surfaced: created,
			wantKeys: []string{"BATCHIT1", "BATCHIT2"},
			wantErr:  "retry filing only, do not re-create the item",
			denyErr:  "retry filing, not creation",
		},
		{
			name:     "no keys recovered",
			surfaced: nil,
			wantKeys: []string{"", ""},
			wantErr:  "items remain; retry filing, not creation",
			denyErr:  "keys [",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateItemsCreateConnectorEnv(t)
			mutationJournalRecorder = recordMutationJournal
			t.Cleanup(func() { mutationJournalRecorder = nil })
			fixture := newItemsCreateConnectorFixture(t, tc.surfaced, http.StatusInternalServerError)

			flags := &rootFlags{
				asJSON: true, yes: true, via: "connector", connectorTarget: target,
				timeout: 5 * time.Second, maxChanges: -1,
				configPath: testConfigFile(t, "http://127.0.0.1:23119/api/users/0"),
			}
			stdout, err := runItemsCreate(t, flags, "--items", fmt.Sprintf(
				`[{"itemType":"journalArticle","title":%q},{"itemType":"journalArticle","title":%q}]`,
				created[0].title, created[1].title))

			if err == nil {
				t.Fatalf("items create succeeded; want a filing failure. stdout=%s", stdout)
			}
			for _, want := range []string{"created 2 item(s) via connector", tc.wantErr} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error = %q, want it to contain %q", err, want)
				}
			}
			if strings.Contains(err.Error(), tc.denyErr) {
				t.Fatalf("error = %q, must not contain %q for this recovery outcome", err, tc.denyErr)
			}

			var env mutation.Envelope
			if decErr := json.Unmarshal([]byte(stdout), &env); decErr != nil {
				t.Fatalf("stdout is not a mutation envelope: %v; stdout=%q", decErr, stdout)
			}
			decodeSingleJSONObject(t, stdout)
			if env.Result == nil || env.Result.Summary.Applied != 2 || env.Result.Summary.Failed != 0 {
				t.Fatalf("result = %+v, want 2 applied and 0 failed -- SaveItems committed", env.Result)
			}
			for i, item := range env.Result.Items {
				if item.Status != "applied" {
					t.Fatalf("item %d status = %q, want applied", i, item.Status)
				}
				if item.Key != tc.wantKeys[i] {
					t.Fatalf("item %d key = %q, want %q", i, item.Key, tc.wantKeys[i])
				}
				reason, ok := item.Reason.(map[string]any)
				if !ok {
					t.Fatalf("item %d reason = %+v, want an object", i, item.Reason)
				}
				if reason["via"] != "connector" || reason["filing_failed"] != true || reason["target"] != target {
					t.Fatalf("item %d reason = %+v, want the connector filing-failure detail", i, reason)
				}
				if reason["session"] != fixture.saveSession {
					t.Fatalf("item %d session = %v, want the session the batch was saved into (%q)", i, reason["session"], fixture.saveSession)
				}
				if message, _ := reason["message"].(string); !strings.Contains(message, "retry filing only") {
					t.Fatalf("item %d message = %v, want retry-filing guidance", i, reason["message"])
				}
				if filingError, _ := reason["filing_error"].(string); !strings.Contains(filingError, "connector updateSession: HTTP 500") {
					t.Fatalf("item %d filing_error = %v, want the updateSession failure", i, reason["filing_error"])
				}
			}
			if !strings.Contains(err.Error(), fixture.saveSession) {
				t.Fatalf("error = %q, want it to name session %q so the envelope and the error correlate", err, fixture.saveSession)
			}

			// The committed batch must be journaled. Before this change the
			// connector route ran outside runMutation, so a filing failure left
			// no record at all and `journal undo` had nothing to reverse.
			entries, listErr := mutation.ListEntries(helpersTestJournalDir(t))
			if listErr != nil {
				t.Fatalf("list journal entries: %v", listErr)
			}
			if len(entries) != 1 {
				t.Fatalf("journal entries = %d, want 1 recorded run for a committed connector batch", len(entries))
			}
			if entries[0].Summary.Applied != 2 {
				t.Fatalf("journaled summary = %+v, want 2 applied", entries[0].Summary)
			}
			for i, op := range entries[0].Ops {
				if op.Key != tc.wantKeys[i] {
					t.Fatalf("journaled op %d key = %q, want %q", i, op.Key, tc.wantKeys[i])
				}
			}

			if fixture.saveItems != 1 {
				t.Fatalf("saveItems requests = %d, want exactly 1 -- a filing failure must never re-create the batch", fixture.saveItems)
			}
			if fixture.updateSessions != 1 {
				t.Fatalf("updateSession requests = %d, want exactly 1", fixture.updateSessions)
			}
			if fixture.filedTarget != target {
				t.Fatalf("updateSession target = %q, want %q", fixture.filedTarget, target)
			}
			if len(fixture.savedIDs) != 2 {
				t.Fatalf("saveItems connector ids = %v, want exactly two ids in one call", fixture.savedIDs)
			}
			if fixture.savedIDs[0] == "" || fixture.savedIDs[1] == "" {
				t.Fatalf("saveItems connector ids = %v, want every item to carry a non-empty connector id", fixture.savedIDs)
			}
			if fixture.savedIDs[0] == fixture.savedIDs[1] {
				t.Fatalf("saveItems connector ids = %v, want two distinct ids", fixture.savedIDs)
			}
			if fmt.Sprint(fixture.savedTitles) != fmt.Sprint([]string{created[0].title, created[1].title}) {
				t.Fatalf("saveItems titles = %v, want both items in one call", fixture.savedTitles)
			}
			// The filing must run against the session the batch was saved into:
			// filing another session files nothing from the committed batch, yet
			// still fails exactly like this fixture's forced 500.
			if fixture.saveSession == "" || fixture.saveSession != fixture.filedSession {
				t.Fatalf("updateSession sessionID = %q, want the saveItems session %q", fixture.filedSession, fixture.saveSession)
			}
			// Key recovery is one sorted, bounded lookup per committed item; the
			// recovery window is collapsed to zero above, so it cannot re-poll.
			wantRecovery := fmt.Sprintf("GET /api/users/0/items/top?direction=desc&limit=%d&sort=dateAdded", recentItemLookupLimit)
			if len(fixture.recoveryReqs) != len(created) {
				t.Fatalf("key-recovery requests = %v, want one per committed item (%d)", fixture.recoveryReqs, len(created))
			}
			for _, req := range fixture.recoveryReqs {
				if req != wantRecovery {
					t.Fatalf("key-recovery request = %q, want %q -- an unsorted or unbounded lookup can match a stale same-title item", req, wantRecovery)
				}
			}
			// The whole-resource resync is the fallback for a create whose key
			// never resolved; write-through covers the rest without a network
			// call, so it must not run when every key came back.
			wantRefresh := 0
			if tc.wantKeys[0] == "" {
				wantRefresh = 1
			}
			if len(fixture.refreshMethods) < wantRefresh {
				t.Fatalf("store refreshes = %v, want at least %d", fixture.refreshMethods, wantRefresh)
			}
			if wantRefresh == 0 && len(fixture.refreshMethods) != 0 {
				t.Fatalf("store refreshes = %v, want none: write-through already mirrored every created item", fixture.refreshMethods)
			}
		})
	}
}

// jsonShape renders a decoded JSON value's key structure and value types,
// discarding the values. Two envelopes with the same shape can be parsed by one
// piece of agent code; two that differ force the caller to branch on the route,
// which is the defect this guards.
//
// A heterogeneous array collapses to the union of its element shapes joined by
// "|", so a field that is sometimes a string and sometimes an array is reported
// as the union it is rather than silently matching whichever element came first.
func jsonShape(value any) string {
	switch typed := value.(type) {
	case map[string]any:
		fields := make([]string, 0, len(typed))
		for field := range typed {
			fields = append(fields, field+":"+jsonShape(typed[field]))
		}
		sort.Strings(fields)
		return "{" + strings.Join(fields, ",") + "}"
	case []any:
		seen := map[string]bool{}
		shapes := make([]string, 0, len(typed))
		for _, element := range typed {
			shape := jsonShape(element)
			if !seen[shape] {
				seen[shape] = true
				shapes = append(shapes, shape)
			}
		}
		sort.Strings(shapes)
		return "[" + strings.Join(shapes, "|") + "]"
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "bool"
	case nil:
		return "null"
	default:
		return fmt.Sprintf("%T", value)
	}
}

// TestItemsCreateEnvelopeShapeIsIdenticalAcrossRoutes is the route-parity
// contract: one command, one output shape. The connector route used to print
// {via,status,count,keys,session,key} while the Web route printed
// {action,resource,path,status,success,data}, so an agent could not parse
// `items create` without first working out which plane had accepted the write —
// something the output itself did not reliably say.
func TestItemsCreateEnvelopeShapeIsIdenticalAcrossRoutes(t *testing.T) {
	const items = `[{"itemType":"journalArticle","title":"Route Parity One"},{"itemType":"journalArticle","title":"Route Parity Two"}]`
	shapes := map[string]string{}
	decoded := map[string]map[string]any{}

	t.Run("web", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":{"0":"WEBKEY01","1":"WEBKEY02"},"successful":{},"unchanged":{},"failed":{}}`))
		}))
		t.Cleanup(srv.Close)
		t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

		flags := &rootFlags{asJSON: true, yes: true, via: "web", maxChanges: -1}
		stdout, err := runItemsCreate(t, flags, "--items", items)
		if err != nil {
			t.Fatalf("items create via web: %v; stdout=%s", err, stdout)
		}
		decoded["web"] = decodeSingleJSONObject(t, stdout)
		shapes["web"] = jsonShape(decoded["web"])
	})

	t.Run("connector", func(t *testing.T) {
		isolateItemsCreateConnectorEnv(t)
		newItemsCreateConnectorFixture(t, []itemsCreateFixtureItem{
			{key: "CONKEY01", title: "Route Parity One"},
			{key: "CONKEY02", title: "Route Parity Two"},
		}, 0)

		flags := &rootFlags{
			asJSON: true, yes: true, via: "connector", maxChanges: -1,
			timeout: 5 * time.Second, configPath: testConfigFile(t, "http://127.0.0.1:23119/api/users/0"),
		}
		stdout, err := runItemsCreate(t, flags, "--items", items)
		if err != nil {
			t.Fatalf("items create via connector: %v; stdout=%s", err, stdout)
		}
		decoded["connector"] = decodeSingleJSONObject(t, stdout)
		shapes["connector"] = jsonShape(decoded["connector"])
	})

	if len(shapes) != 2 {
		t.Fatalf("only %d route(s) produced an envelope: %v", len(shapes), shapes)
	}
	if shapes["web"] != shapes["connector"] {
		t.Fatalf("envelope shapes differ.\n  web:       %s\n  connector: %s", shapes["web"], shapes["connector"])
	}

	// Field by field on the parts an agent reads, so a future change that keeps
	// the shape identical while moving the meaning still fails.
	for _, route := range []string{"web", "connector"} {
		env := decoded[route]
		if env["operation"] != "items.create" || env["mode"] != "apply" || env["ok"] != true {
			t.Fatalf("%s envelope = %+v, want operation=items.create mode=apply ok=true", route, env)
		}
		result, ok := env["result"].(map[string]any)
		if !ok {
			t.Fatalf("%s envelope has no result object: %+v", route, env)
		}
		resultItems, ok := result["items"].([]any)
		if !ok || len(resultItems) != 2 {
			t.Fatalf("%s result items = %+v, want 2", route, result["items"])
		}
		for i, raw := range resultItems {
			item, ok := raw.(map[string]any)
			if !ok {
				t.Fatalf("%s result item %d = %+v, want an object", route, i, raw)
			}
			if item["status"] != "applied" {
				t.Fatalf("%s result item %d status = %v, want applied", route, i, item["status"])
			}
			// The created key is the one field every caller needs, and it must
			// be a plain string on both routes. The connector route used to
			// report it as string|array|null depending on how many keys it had
			// recovered -- a union no agent can rely on.
			key, ok := item["key"].(string)
			if !ok || key == "" {
				t.Fatalf("%s result item %d key = %#v, want a non-empty string", route, i, item["key"])
			}
			reason, ok := item["reason"].(map[string]any)
			if !ok {
				t.Fatalf("%s result item %d reason = %+v, want an object", route, i, item["reason"])
			}
			if _, ok := reason["key"].(string); !ok {
				t.Fatalf("%s result item %d reason key = %#v, want a string", route, i, reason["key"])
			}
			if _, ok := reason["via"].(string); !ok {
				t.Fatalf("%s result item %d reason via = %#v, want a string", route, i, reason["via"])
			}
		}
	}
	if via := decoded["web"]["result"].(map[string]any)["items"].([]any)[0].(map[string]any)["reason"].(map[string]any)["via"]; via != "web" {
		t.Fatalf("web route reported via = %v, want web", via)
	}
	if via := decoded["connector"]["result"].(map[string]any)["items"].([]any)[0].(map[string]any)["reason"].(map[string]any)["via"]; via != "connector" {
		t.Fatalf("connector route reported via = %v, want connector", via)
	}
}

// TestItemsCreateConnectorRouteJournalsWithoutAPIKey holds the connector route's
// reason to exist: it writes to the desktop over localhost, so it must keep
// working with no Zotero Web API key configured (doctor reports Web writes as
// unavailable in exactly this state). Routing it through the mutation engine
// must not introduce a key requirement, and the run must now be journaled --
// before this change a connector batch left no journal entry and no undo handle.
func TestItemsCreateConnectorRouteJournalsWithoutAPIKey(t *testing.T) {
	isolateItemsCreateConnectorEnv(t)
	mutationJournalRecorder = recordMutationJournal
	t.Cleanup(func() { mutationJournalRecorder = nil })
	fixture := newItemsCreateConnectorFixture(t, []itemsCreateFixtureItem{
		{key: "NOKEYIT1", title: "Keyless One"},
		{key: "NOKEYIT2", title: "Keyless Two"},
	}, 0)

	configPath := testConfigFile(t, "http://127.0.0.1:23119/api/users/0")
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load test config: %v", err)
	}
	if cfg.AuthHeader() != "" {
		t.Fatalf("test config carries an API key (%q); this test must run with none", cfg.AuthSource)
	}

	flags := &rootFlags{
		asJSON: true, yes: true, via: "connector", maxChanges: -1,
		timeout: 5 * time.Second, configPath: configPath,
	}
	stdout, err := runItemsCreate(t, flags,
		"--items", `[{"itemType":"journalArticle","title":"Keyless One"},{"itemType":"journalArticle","title":"Keyless Two"}]`)
	if err != nil {
		t.Fatalf("items create via connector without an API key: %v; stdout=%s", err, stdout)
	}
	if fixture.saveItems != 1 {
		t.Fatalf("saveItems requests = %d, want exactly 1", fixture.saveItems)
	}
	if fixture.updateSessions != 0 {
		t.Fatalf("updateSession requests = %d, want 0 when no --connector-target is set", fixture.updateSessions)
	}

	var env mutation.Envelope
	if decErr := json.Unmarshal([]byte(stdout), &env); decErr != nil {
		t.Fatalf("stdout is not a mutation envelope: %v; stdout=%q", decErr, stdout)
	}
	if env.Result == nil || env.Result.Summary.Applied != 2 {
		t.Fatalf("result = %+v, want 2 applied", env.Result)
	}

	entries, listErr := mutation.ListEntries(helpersTestJournalDir(t))
	if listErr != nil {
		t.Fatalf("list journal entries: %v", listErr)
	}
	if len(entries) != 1 {
		t.Fatalf("journal entries = %d, want 1 -- a connector create must be undoable", len(entries))
	}
	gotKeys := []string{}
	for _, op := range entries[0].Ops {
		gotKeys = append(gotKeys, op.Key)
	}
	if fmt.Sprint(gotKeys) != fmt.Sprint([]string{"NOKEYIT1", "NOKEYIT2"}) {
		t.Fatalf("journaled keys = %v, want the two recovered Zotero keys", gotKeys)
	}
}

// TestItemsCreateMirrorsCreatedItemsForLocalReads is the read-your-writes
// contract for creates on both routes: dev/roadmap.md recorded "creates ...
// reconcile on the next sync" as a Phase 8 limitation, so an item created a
// second ago was invisible to `--data-source local` until a sync ran.
func TestItemsCreateMirrorsCreatedItemsForLocalReads(t *testing.T) {
	for _, tc := range []struct {
		route string
		key   string
	}{
		{route: "web", key: "WEBMIRR1"},
		{route: "connector", key: "CONMIRR1"},
	} {
		t.Run(tc.route, func(t *testing.T) {
			const title = "Mirrored On Create"
			var flags *rootFlags
			if tc.route == "web" {
				t.Setenv("HOME", t.TempDir())
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprintf(w, `{"success":{"0":%q},"successful":{},"unchanged":{},"failed":{}}`, tc.key)
				}))
				t.Cleanup(srv.Close)
				t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
				flags = &rootFlags{asJSON: true, yes: true, via: "web", maxChanges: -1}
			} else {
				isolateItemsCreateConnectorEnv(t)
				newItemsCreateConnectorFixture(t, []itemsCreateFixtureItem{{key: tc.key, title: title}}, 0)
				flags = &rootFlags{
					asJSON: true, yes: true, via: "connector", maxChanges: -1,
					timeout: 5 * time.Second, configPath: testConfigFile(t, "http://127.0.0.1:23119/api/users/0"),
				}
			}
			// The mirror only exists once something has synced; seed an
			// unrelated row so openExistingStoreForWrite finds a database.
			seedWriteThroughItem(t, "OTHER001", `{"key":"OTHER001","version":1,"data":{"key":"OTHER001","itemType":"book","title":"Already Synced"}}`)
			mirrorWriteThrough = applyMirrorWriteThrough
			t.Cleanup(func() { mirrorWriteThrough = nil })

			stdout, err := runItemsCreate(t, flags, "--items",
				fmt.Sprintf(`[{"itemType":"journalArticle","title":%q}]`, title))
			if err != nil {
				t.Fatalf("items create via %s: %v; stdout=%s", tc.route, err, stdout)
			}

			db, openErr := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
			if openErr != nil {
				t.Fatalf("reopen store: %v", openErr)
			}
			defer db.Close()
			rows, queryErr := (localQueryStore{db}).QueryRaw(
				"SELECT json_extract(data,'$.data.title') AS title, json_extract(data,'$.data.itemType') AS item_type, json_extract(data,'$.version') AS version FROM resources WHERE resource_type='items' AND id=?", tc.key)
			if queryErr != nil || len(rows) != 1 {
				t.Fatalf("created item %s is not in the mirror without a sync: rows=%v err=%v", tc.key, rows, queryErr)
			}
			if got := sqlStringValue(rows[0]["title"]); got != title {
				t.Fatalf("mirrored title = %q, want %q", got, title)
			}
			if got := sqlStringValue(rows[0]["item_type"]); got != "journalArticle" {
				t.Fatalf("mirrored itemType = %q, want journalArticle", got)
			}
			// No version: Zotero assigns one this process never sees, and the
			// store's version-monotonic upsert guard only lets the authoritative
			// synced row replace this placeholder while the placeholder has none.
			if rows[0]["version"] != nil {
				t.Fatalf("mirrored row carries version %v, want none so the next sync's row always wins", rows[0]["version"])
			}
			// The connector correlation id is a save-session handle, not a
			// Zotero field; it must never reach the mirror.
			idRows, idErr := (localQueryStore{db}).QueryRaw(
				"SELECT json_extract(data,'$.data.id') AS connector_id FROM resources WHERE resource_type='items' AND id=?", tc.key)
			if idErr != nil {
				t.Fatalf("read back connector id: %v", idErr)
			}
			if idRows[0]["connector_id"] != nil {
				t.Fatalf("mirrored row carries the connector correlation id %v", idRows[0]["connector_id"])
			}

			// The acceptance is a local READ, not a row: drive the same
			// resolveLocal dispatch `--data-source local` uses.
			listed, _, readErr := resolveLocal(context.Background(), "items", true, "/items", nil, "test")
			if readErr != nil {
				t.Fatalf("local items read: %v", readErr)
			}
			var localItems []map[string]any
			if unmarshalErr := json.Unmarshal(listed, &localItems); unmarshalErr != nil {
				t.Fatalf("local items read is not a list: %v; %s", unmarshalErr, listed)
			}
			found := false
			for _, item := range localItems {
				if item["key"] == tc.key {
					found = true
				}
			}
			if !found {
				t.Fatalf("`--data-source local` does not see %s without a sync; got %s", tc.key, listed)
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
