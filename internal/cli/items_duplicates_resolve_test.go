// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"zotio/internal/mutation"
	"zotio/internal/store"
)

type duplicateResolveTestServer struct {
	server       *httptest.Server
	versions     map[string]int
	items        map[string]map[string]any
	patchBodies  map[string]map[string]any
	patchHeaders map[string]string
	patchCounts  map[string]int
	// failPatchKeys, when true for a key, makes that key's PATCH return 500
	// instead of succeeding — simulates one write in a multi-write op
	// succeeding while a later one fails.
	failPatchKeys map[string]bool
}

func newDuplicateResolveTestServer(t *testing.T, versions map[string]int, items map[string]map[string]any) *duplicateResolveTestServer {
	t.Helper()
	ts := &duplicateResolveTestServer{
		versions:      versions,
		items:         items,
		patchBodies:   map[string]map[string]any{},
		patchHeaders:  map[string]string{},
		patchCounts:   map[string]int{},
		failPatchKeys: map[string]bool{},
	}
	ts.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for key, dataObj := range ts.items {
			if r.URL.Path != "/users/0/items/"+key {
				continue
			}
			switch r.Method {
			case http.MethodGet:
				version := ts.versions[key]
				w.Header().Set("Last-Modified-Version", fmt.Sprintf("%d", version))
				_ = json.NewEncoder(w).Encode(map[string]any{"key": key, "version": version, "data": dataObj})
			case http.MethodPatch:
				ts.patchCounts[key]++
				if ts.failPatchKeys[key] {
					// 400 (not 5xx) so the client's server-error retry loop
					// doesn't slow the test down retrying a deterministic failure.
					http.Error(w, "simulated failure", http.StatusBadRequest)
					return
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode patch body: %v", err)
				}
				ts.patchBodies[key] = body
				ts.patchHeaders[key] = r.Header.Get("If-Unmodified-Since-Version")
				for bodyKey, value := range body {
					dataObj[bodyKey] = value
				}
				w.WriteHeader(http.StatusNoContent)
			default:
				http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
			}
			return
		}
		http.Error(w, "unexpected path", http.StatusNotFound)
	}))
	t.Cleanup(ts.server.Close)
	return ts
}

func seedDuplicateResolveStore(t *testing.T, items []json.RawMessage) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	db, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed items: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
}

func runItemsDuplicatesResolveTestCmd(t *testing.T, srv *duplicateResolveTestServer, flags *rootFlags, args ...string) mutation.Envelope {
	t.Helper()
	env, _ := runItemsDuplicatesResolveTestCmdWithStderr(t, srv, flags, args...)
	return env
}

func runItemsDuplicatesResolveTestCmdWithStderr(t *testing.T, srv *duplicateResolveTestServer, flags *rootFlags, args ...string) (mutation.Envelope, string) {
	t.Helper()
	t.Setenv("ZOTERO_BASE_URL", srv.server.URL+"/users/0")
	cmd := newItemsDuplicatesCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	var errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("items duplicates %v: %v; stderr=%s", args, err, errOut.String())
	}
	var env mutation.Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode mutation envelope %q: %v", out.String(), err)
	}
	return env, errOut.String()
}

func TestItemsDuplicatesResolvePreviewWritesNothing(t *testing.T) {
	seedDuplicateResolveStore(t, []json.RawMessage{
		json.RawMessage(`{"key":"K1","version":10,"data":{"key":"K1","itemType":"journalArticle","title":"Same","DOI":"10/example","collections":["C1"],"tags":[{"tag":"keep"}]}}`),
		json.RawMessage(`{"key":"K2","version":11,"data":{"key":"K2","itemType":"journalArticle","title":"Same","DOI":"10/example","collections":["C2"],"tags":[{"tag":"dup"}]}}`),
	})
	srv := newDuplicateResolveTestServer(t, map[string]int{"K1": 10, "K2": 11}, map[string]map[string]any{
		"K1": {"key": "K1", "itemType": "journalArticle", "title": "Same", "DOI": "10/example", "collections": []any{"C1"}, "tags": []any{map[string]any{"tag": "keep"}}},
		"K2": {"key": "K2", "itemType": "journalArticle", "title": "Same", "DOI": "10/example", "collections": []any{"C2"}, "tags": []any{map[string]any{"tag": "dup"}}},
	})

	env := runItemsDuplicatesResolveTestCmd(t, srv, &rootFlags{asJSON: true, maxChanges: -1}, "resolve", "--doi")
	if !env.OK || env.Mode != "preview" || env.Result != nil || env.Plan.Summary.Planned != 1 {
		t.Fatalf("env = %+v, want preview with one planned merge", env)
	}
	if len(env.Plan.Operations) != 1 || env.Plan.Operations[0].Kind != "duplicate_merge" || env.Plan.Operations[0].Key != "K2" {
		t.Fatalf("ops = %+v, want K2 duplicate_merge", env.Plan.Operations)
	}
	if !duplicateResolvePlanHasAdd(env.Plan.Operations[0].Changes, "collections", "C2") || !duplicateResolvePlanHasAdd(env.Plan.Operations[0].Changes, "deleted", float64(1)) {
		t.Errorf("changes = %+v, want collection merge and trash note", env.Plan.Operations[0].Changes)
	}
	if srv.patchCounts["K1"] != 0 || srv.patchCounts["K2"] != 0 {
		t.Fatalf("preview PATCH counts = master %d dup %d, want 0", srv.patchCounts["K1"], srv.patchCounts["K2"])
	}
}

func TestItemsDuplicatesResolveApplyMergesCollectionsAndTrashesDuplicate(t *testing.T) {
	seedDuplicateResolveStore(t, []json.RawMessage{
		json.RawMessage(`{"key":"K1","version":10,"data":{"key":"K1","itemType":"journalArticle","title":"Same","DOI":"10/example","abstractNote":"present","collections":["C1"],"tags":[{"tag":"keep"}]}}`),
		json.RawMessage(`{"key":"K2","version":11,"data":{"key":"K2","itemType":"journalArticle","title":"Same","DOI":"10/example","collections":["C2"],"tags":[{"tag":"dup"}]}}`),
	})
	srv := newDuplicateResolveTestServer(t, map[string]int{"K1": 10, "K2": 11}, map[string]map[string]any{
		"K1": {"key": "K1", "itemType": "journalArticle", "title": "Same", "DOI": "10/example", "abstractNote": "present", "collections": []any{"C1"}, "tags": []any{map[string]any{"tag": "keep"}}},
		"K2": {"key": "K2", "itemType": "journalArticle", "title": "Same", "DOI": "10/example", "collections": []any{"C2"}, "tags": []any{map[string]any{"tag": "dup"}}},
	})

	env := runItemsDuplicatesResolveTestCmd(t, srv, &rootFlags{asJSON: true, yes: true, maxChanges: -1, allowDestructive: true}, "resolve", "--doi")
	if !env.OK || env.Mode != "apply" || env.Result == nil || env.Result.Summary.Applied != 1 || env.Result.Items[0].Status != "applied" {
		t.Fatalf("env = %+v, want one applied merge", env)
	}
	if srv.patchCounts["K1"] != 1 || srv.patchCounts["K2"] != 1 {
		t.Fatalf("PATCH counts = master %d dup %d, want 1 each", srv.patchCounts["K1"], srv.patchCounts["K2"])
	}
	if srv.patchHeaders["K1"] != "10" || srv.patchHeaders["K2"] != "11" {
		t.Fatalf("headers = master %q dup %q, want 10/11", srv.patchHeaders["K1"], srv.patchHeaders["K2"])
	}
	if !duplicateResolveBodyHasString(srv.patchBodies["K1"], "collections", "C1") || !duplicateResolveBodyHasString(srv.patchBodies["K1"], "collections", "C2") {
		t.Errorf("master PATCH collections = %+v, want C1+C2", srv.patchBodies["K1"])
	}
	if !duplicateResolveBodyHasTag(srv.patchBodies["K1"], "keep") || !duplicateResolveBodyHasTag(srv.patchBodies["K1"], "dup") {
		t.Errorf("master PATCH tags = %+v, want keep+dup", srv.patchBodies["K1"])
	}
	if srv.patchBodies["K2"]["deleted"] != float64(1) {
		t.Errorf("dup PATCH body = %+v, want deleted=1", srv.patchBodies["K2"])
	}
}

// Default duplicate resolution is DOI-only, with title matching opt-in warning coverage.
func TestItemsDuplicatesResolveDefaultsToDOIOnlyAndWarnsForTitle(t *testing.T) {
	for _, tt := range []struct {
		name     string
		args     []string
		wantKeys []string
		wantWarn bool
	}{
		{name: "default", args: []string{"resolve"}, wantKeys: []string{"K2"}},
		{name: "title", args: []string{"resolve", "--title"}, wantKeys: []string{"T2"}, wantWarn: true},
		{name: "doi and title", args: []string{"resolve", "--doi", "--title"}, wantKeys: []string{"K2", "T2"}, wantWarn: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			seedDuplicateResolveStore(t, []json.RawMessage{
				json.RawMessage(`{"key":"K1","version":10,"data":{"key":"K1","itemType":"journalArticle","title":"Shared DOI A","DOI":"10/example","collections":["C1"]}}`),
				json.RawMessage(`{"key":"K2","version":11,"data":{"key":"K2","itemType":"journalArticle","title":"Shared DOI B","DOI":"10/example","collections":["C2"]}}`),
				json.RawMessage(`{"key":"T1","version":20,"data":{"key":"T1","itemType":"journalArticle","title":"Week 3","collections":["C3"]}}`),
				json.RawMessage(`{"key":"T2","version":21,"data":{"key":"T2","itemType":"journalArticle","title":"Week 3","collections":["C4"]}}`),
			})
			srv := newDuplicateResolveTestServer(t, map[string]int{}, map[string]map[string]any{})

			env, stderr := runItemsDuplicatesResolveTestCmdWithStderr(t, srv, &rootFlags{asJSON: true, maxChanges: -1}, tt.args...)
			if !env.OK || env.Mode != "preview" {
				t.Fatalf("env = %+v, want preview", env)
			}
			if got := duplicateResolvePlanKeys(env.Plan.Operations); fmt.Sprint(got) != fmt.Sprint(tt.wantKeys) {
				t.Fatalf("plan keys = %v, want %v; ops=%+v", got, tt.wantKeys, env.Plan.Operations)
			}
			hasWarn := strings.Contains(stderr, duplicateResolveTitleWarning)
			if hasWarn != tt.wantWarn {
				t.Fatalf("stderr = %q, warning presence = %v, want %v", stderr, hasWarn, tt.wantWarn)
			}
		})
	}
}

func TestItemsDuplicatesResolveNoDuplicatesEmptyPlan(t *testing.T) {
	seedDuplicateResolveStore(t, []json.RawMessage{
		json.RawMessage(`{"key":"K1","version":1,"data":{"key":"K1","itemType":"journalArticle","title":"One","DOI":"10/one"}}`),
		json.RawMessage(`{"key":"K2","version":2,"data":{"key":"K2","itemType":"journalArticle","title":"Two","DOI":"10/two"}}`),
	})
	srv := newDuplicateResolveTestServer(t, map[string]int{}, map[string]map[string]any{})

	env := runItemsDuplicatesResolveTestCmd(t, srv, &rootFlags{asJSON: true, maxChanges: -1}, "resolve")
	if !env.OK || env.Mode != "preview" || env.Plan.Summary.Selected != 0 || env.Plan.Summary.Planned != 0 || len(env.Plan.Operations) != 0 {
		t.Fatalf("env = %+v, want empty preview plan", env)
	}
}

func TestDuplicateResolveRowKeysJSONPayload(t *testing.T) {
	t.Run("malformed", func(t *testing.T) {
		if _, err := duplicateResolveRowKeys(map[string]any{"keys": `["K1",`}); err == nil {
			t.Fatal("duplicateResolveRowKeys() error = nil, want malformed JSON error")
		}
	})

	t.Run("sorted", func(t *testing.T) {
		got, err := duplicateResolveRowKeys(map[string]any{"keys": `["K2","K1"]`})
		if err != nil {
			t.Fatalf("duplicateResolveRowKeys() error = %v", err)
		}
		if fmt.Sprint(got) != "[K1 K2]" {
			t.Fatalf("duplicateResolveRowKeys() = %v, want [K1 K2]", got)
		}
	})
}

func duplicateResolvePlanKeys(ops []mutation.Op) []string {
	keys := make([]string, 0, len(ops))
	for _, op := range ops {
		keys = append(keys, op.Key)
	}
	return keys
}

func duplicateResolvePlanHasAdd(changes []mutation.Change, field string, want any) bool {
	for _, change := range changes {
		if change.Field != field {
			continue
		}
		if fmt.Sprint(change.Add) == fmt.Sprint(want) {
			return true
		}
		values, ok := change.Add.([]any)
		if !ok {
			continue
		}
		for _, value := range values {
			if fmt.Sprint(value) == fmt.Sprint(want) {
				return true
			}
		}
	}
	return false
}

func duplicateResolveBodyHasString(body map[string]any, field string, want string) bool {
	values, ok := body[field].([]any)
	if !ok {
		return false
	}
	for _, value := range values {
		if got, ok := value.(string); ok && got == want {
			return true
		}
	}
	return false
}

func duplicateResolveBodyHasTag(body map[string]any, want string) bool {
	values, ok := body["tags"].([]any)
	if !ok {
		return false
	}
	for _, value := range values {
		tagObj, ok := value.(map[string]any)
		if !ok {
			continue
		}
		if got, ok := tagObj["tag"].(string); ok && got == want {
			return true
		}
	}
	return false
}

// TestDuplicatesResolvePartialFailureIsJournaled covers the review-flagged
// gap: applyDuplicateResolve's merge-target PATCH succeeds, then the
// duplicate-trash PATCH fails. That is a real, committed write to the merge
// target — it must land in the journal (Applied >= 1), and both the run's own
// output and the journal must make the half-applied state (target updated,
// duplicate NOT trashed) unmistakable rather than reading as a clean success.
func TestDuplicatesResolvePartialFailureIsJournaled(t *testing.T) {
	seedDuplicateResolveStore(t, []json.RawMessage{
		json.RawMessage(`{"key":"K1","version":10,"data":{"key":"K1","itemType":"journalArticle","title":"Same","DOI":"10/example","collections":["C1"],"tags":[{"tag":"keep"}]}}`),
		json.RawMessage(`{"key":"K2","version":11,"data":{"key":"K2","itemType":"journalArticle","title":"Same","DOI":"10/example","collections":["C2"],"tags":[{"tag":"dup"}]}}`),
	})
	srv := newDuplicateResolveTestServer(t, map[string]int{"K1": 10, "K2": 11}, map[string]map[string]any{
		"K1": {"key": "K1", "itemType": "journalArticle", "title": "Same", "DOI": "10/example", "collections": []any{"C1"}, "tags": []any{map[string]any{"tag": "keep"}}},
		"K2": {"key": "K2", "itemType": "journalArticle", "title": "Same", "DOI": "10/example", "collections": []any{"C2"}, "tags": []any{map[string]any{"tag": "dup"}}},
	})
	// K1 is the merge target (most complete/DOI-bearing candidate stays master
	// deterministically here since both share a DOI and K1 sorts first); its
	// PATCH is left to succeed. K2 is the duplicate: its trash PATCH is forced
	// to fail, simulating the first write landing and the second failing.
	srv.failPatchKeys["K2"] = true

	// journal_test.go:272-300 pattern: wire the real recorder and an isolated
	// HOME so this run actually appends to (and we can read back) a journal.
	mutationJournalRecorder = recordMutationJournal
	t.Cleanup(func() { mutationJournalRecorder = nil })

	// rootFlags{} defaults MaxChanges to 0, which EffectiveMaxChanges/CheckGates
	// treats as a cap of zero (not "use the default"); -1 opts into the
	// package default cap instead.
	env := runItemsDuplicatesResolveTestCmd(t, srv, &rootFlags{asJSON: true, yes: true, maxChanges: -1, allowDestructive: true}, "resolve", "--doi")

	// The merge target's write actually landed: the op is reported "applied"
	// (not "failed") precisely so the engine counts it and the run gets
	// journaled — see applyDuplicateResolve's half-applied branch.
	if !env.OK || env.Mode != "apply" || env.Result == nil {
		t.Fatalf("env = %+v, want a successful apply run", env)
	}
	if env.Result.Summary.Applied != 1 || env.Result.Summary.Failed != 0 {
		t.Fatalf("summary = %+v, want Applied=1 Failed=0", env.Result.Summary)
	}
	if len(env.Result.Items) != 1 || env.Result.Items[0].Status != "applied" {
		t.Fatalf("items = %+v, want one applied item", env.Result.Items)
	}

	// The failure output must name which sub-write failed and that the
	// target update already landed.
	reason := fmt.Sprint(env.Result.Items[0].Reason)
	for _, want := range []string{"K1", "K2", "trash"} {
		if !strings.Contains(reason, want) {
			t.Errorf("result reason = %q, want it to mention %q", reason, want)
		}
	}
	if len(env.Warnings) != 1 || env.Warnings[0] != reason {
		t.Fatalf("warnings = %+v, want the same half-applied detail surfaced as a warning", env.Warnings)
	}

	// Both PATCHes were attempted exactly once: the target update committed,
	// the trash attempt failed.
	if srv.patchCounts["K1"] != 1 || srv.patchCounts["K2"] != 1 {
		t.Fatalf("PATCH counts = master %d dup %d, want 1 each", srv.patchCounts["K1"], srv.patchCounts["K2"])
	}
	if !duplicateResolveBodyHasString(srv.patchBodies["K1"], "collections", "C2") || !duplicateResolveBodyHasTag(srv.patchBodies["K1"], "dup") {
		t.Errorf("master PATCH body = %+v, want the merged collections/tags to have landed", srv.patchBodies["K1"])
	}
	if _, dupPatched := srv.patchBodies["K2"]; dupPatched {
		t.Errorf("dup PATCH body recorded for K2, want the failed trash PATCH to have no successful body")
	}

	// A committed write must never be absent from the journal, and the
	// journal itself (not just this run's own output) must mark the merge as
	// half-applied rather than a clean duplicate_merge.
	entries, err := mutation.ListEntries(helpersTestJournalDir(t))
	if err != nil {
		t.Fatalf("list journal entries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("journal entries = %d, want exactly 1", len(entries))
	}
	entry := entries[0]
	if entry.Summary.Applied != 1 {
		t.Fatalf("journal entry summary = %+v, want Applied=1", entry.Summary)
	}
	if len(entry.Ops) != 1 || entry.Ops[0].Status != "applied" || entry.Ops[0].Key != "K2" {
		t.Fatalf("journal ops = %+v, want one applied op for K2", entry.Ops)
	}
	if entry.Ops[0].Kind != duplicateResolveMergePartialKind {
		t.Fatalf("journal op kind = %q, want %q so `journal show` marks the half-applied state", entry.Ops[0].Kind, duplicateResolveMergePartialKind)
	}
}
