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
	"sync/atomic"
	"testing"
	"time"

	"zotio/internal/client"
	"zotio/internal/config"
	"zotio/internal/mutation"
)

func journalRecoverySetup(t *testing.T) string {
	t.Helper()
	t.Cleanup(client.SetRetryBackoffBaseForTest(time.Millisecond))
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_DATA_DIR", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	oldJournal, oldOutcome := mutationJournalRecorder, mutationOutcomeRecorder
	mutationJournalRecorder, mutationOutcomeRecorder = recordMutationJournal, recordMutationOutcome
	t.Cleanup(func() { mutationJournalRecorder, mutationOutcomeRecorder = oldJournal, oldOutcome })
	dir, err := journalDir()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestJournalUndoRefusesReplayAfterLaterTagRemoval(t *testing.T) {
	dir := journalRecoverySetup(t)
	const key = "ABCD2345"
	plane := newTagStatefulPlane(t, map[string][]string{key: {"x"}})
	t.Setenv("ZOTERO_BASE_URL", plane.srv.URL+"/users/0")
	c := client.New(&config.Config{BaseURL: plane.srv.URL + "/users/0"}, 5*time.Second, 0)
	c.NoCache = true
	remove := func() string {
		op := mutation.Op{ID: "remove", Key: key, Kind: "tag_remove", Changes: []mutation.Change{{Field: "tags", Remove: "x"}}, Apply: func() (string, any, error) {
			return patchWithVersionGuard(c, "/items/"+key, map[string]any{"tags": []map[string]any{}}, 100)
		}}
		env, err := runMutation(context.Background(), &rootFlags{yes: true, maxChanges: -1}, "items.tags.remove", []mutation.Op{op})
		if err != nil {
			t.Fatal(err)
		}
		return env.RunID
	}
	original := remove()
	undoTagRunToCompletion(t, original)
	if got := plane.sortedTags(key); len(got) != 1 || got[0] != "x" {
		t.Fatalf("tags after undo = %v", got)
	}
	entries, err := mutation.ListEntries(dir)
	if err != nil || len(entries) != 2 || entries[0].Undoes != original {
		t.Fatalf("undo entries=%+v, err=%v", entries, err)
	}
	earlierUndo := entries[0].RunID
	remove()
	cmd := newJournalUndoCmd(&rootFlags{yes: true, asJSON: true, maxChanges: -1})
	cmd.SetArgs([]string{original})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err = cmd.Execute()
	if ExitCode(err) != 9 || !strings.Contains(err.Error(), earlierUndo) {
		t.Fatalf("replay error=%v, exit=%d; want prior undo %s", err, ExitCode(err), earlierUndo)
	}
	if got := plane.sortedTags(key); len(got) != 0 {
		t.Fatalf("replayed undo restored later removal: %v", got)
	}
}

func TestJournalUndoReservationSurvivesInterruptedRun(t *testing.T) {
	dir := journalRecoverySetup(t)
	if err := reserveJournalUndo("interrupted-undo", "original"); err != nil {
		t.Fatal(err)
	}
	entry, err := mutation.ReadEntry(dir, "interrupted-undo")
	if err != nil || !entry.Partial || entry.Undoes != "original" {
		t.Fatalf("reservation=%+v, err=%v", entry, err)
	}
	if err := refuseRecordedUndo("original"); ExitCode(err) != 9 || !strings.Contains(err.Error(), "interrupted-undo") {
		t.Fatalf("reservation did not block replay: %v", err)
	}
}

func TestGuardedPatchCancellationReconcilesAndJournalsUnknown(t *testing.T) {
	for _, readable := range []bool{false, true} {
		t.Run(fmt.Sprintf("readable=%v", readable), func(t *testing.T) {
			dir := journalRecoverySetup(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var reads atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodPatch:
					cancel()
					conn, _, err := w.(http.Hijacker).Hijack()
					if err == nil {
						_ = conn.Close()
					}
				case http.MethodGet:
					reads.Add(1)
					if !readable {
						http.Error(w, "unavailable", http.StatusServiceUnavailable)
						return
					}
					w.Header().Set("Last-Modified-Version", "101")
					_, _ = w.Write([]byte(`{"key":"ABCD2345","version":101,"data":{"tags":[{"tag":"x"}]}}`))
				}
			}))
			defer srv.Close()
			c := client.New(&config.Config{BaseURL: srv.URL + "/users/0"}, 2*time.Second, 0)
			c.NoCache = true
			c.SetContext(ctx)
			op := mutation.Op{ID: "patch", Key: "ABCD2345", Kind: "tag_add", Changes: []mutation.Change{{Field: "tags", Add: "x"}}, Apply: func() (string, any, error) {
				return patchWithVersionGuard(c, "/items/ABCD2345", map[string]any{"tags": []map[string]any{{"tag": "x"}}}, 100)
			}}
			env, _ := runMutation(ctx, &rootFlags{yes: true, maxChanges: -1}, "items.tags.add", []mutation.Op{op})
			if reads.Load() == 0 {
				t.Fatal("reconciliation reused the canceled request context")
			}
			entry, err := mutation.ReadEntry(dir, env.RunID)
			if err != nil || len(entry.Ops) != 1 {
				t.Fatalf("entry=%+v, err=%v", entry, err)
			}
			inverse, refused := mutation.InverseOps(entry)
			if readable {
				if entry.Ops[0].Status != "applied" || len(inverse) != 1 || len(refused) != 0 {
					t.Fatalf("confirmed entry=%+v, inverse=%+v, refused=%+v", entry, inverse, refused)
				}
			} else {
				detail, ok := entry.Ops[0].Reason.(map[string]any)
				if !ok || detail["committed"] != true || detail["outcome"] != "unknown" || detail["requested_fields"] == nil || len(refused) != 1 {
					t.Fatalf("uncertain entry=%+v, inverse=%+v, refused=%+v", entry, inverse, refused)
				}
			}
		})
	}
}

func TestTagsBatchDecodedOutcomesJournalBeforeCancellation(t *testing.T) {
	for _, action := range []string{"add", "remove"} {
		t.Run(action, func(t *testing.T) {
			dir := journalRecoverySetup(t)
			keys := []string{"ABCD2345", "ABCD2346", "ABCD2347"}
			b := newBatchTagServer(t, keys)
			if action == "remove" {
				for _, key := range keys {
					b.existing[key] = []map[string]any{{"tag": "x"}}
				}
			}
			t.Setenv("ZOTERO_BASE_URL", b.server.URL+"/users/0")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			checkpoints := 0
			mutationOutcomeRecorder = func(runID, operation, mode string, op mutation.Op, item mutation.ResultItem) error {
				err := recordMutationOutcome(runID, operation, mode, op, item)
				checkpoints++
				if checkpoints == 1 {
					cancel()
				}
				return err
			}
			cmd := newItemsTagsCmd(&rootFlags{ctx: ctx, yes: true, asJSON: true, maxChanges: -1})
			cmd.SetArgs(append([]string{action, "--batch", "--tag", "x"}, keys...))
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&bytes.Buffer{})
			_ = cmd.ExecuteContext(ctx)
			var env mutation.Envelope
			if err := json.Unmarshal(out.Bytes(), &env); err != nil {
				t.Fatalf("decode %q: %v", out.String(), err)
			}
			entries, err := mutation.ListEntries(dir)
			if err != nil || len(entries) != 1 || checkpoints != 3 || entries[0].Summary.Applied != 3 {
				t.Fatalf("entries=%+v, checkpoints=%d, err=%v", entries, checkpoints, err)
			}
			for _, op := range entries[0].Ops {
				if op.Status != "applied" || op.Reason != nil {
					t.Fatalf("known outcome changed: %+v", op)
				}
			}
			inverse, refused := mutation.InverseOps(entries[0])
			if len(inverse) != 3 || len(refused) != 0 || env.Result == nil || env.Result.Summary.Applied != 3 {
				t.Fatalf("inverse=%+v, refused=%+v, result=%+v", inverse, refused, env.Result)
			}
		})
	}
}

func TestTagsBatchAmbiguousResponseKeepsEveryUndoRefusal(t *testing.T) {
	for _, response := range []string{"502", "unattributable"} {
		t.Run(response, func(t *testing.T) {
			dir := journalRecoverySetup(t)
			keys := []string{"ABCD2345", "ABCD2346", "ABCD2347"}
			var committed atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					rows := make([]map[string]any, 0, len(keys))
					for _, key := range keys {
						rows = append(rows, map[string]any{"key": key, "version": 100, "data": map[string]any{"tags": []map[string]any{{"tag": "x"}}}})
					}
					_ = json.NewEncoder(w).Encode(rows)
					return
				}
				var objects []map[string]any
				if err := json.NewDecoder(r.Body).Decode(&objects); err != nil {
					t.Errorf("decode request: %v", err)
				}
				committed.Add(int32(len(objects)))
				if response == "502" {
					http.Error(w, "lost result after commit", http.StatusBadGateway)
				} else {
					_, _ = w.Write([]byte(`{"unexpected":true}`))
				}
			}))
			defer srv.Close()
			t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
			cmd := newItemsTagsCmd(&rootFlags{yes: true, asJSON: true, maxChanges: -1})
			cmd.SetArgs(append([]string{"remove", "--batch", "--tag", "x"}, keys...))
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			if err := cmd.Execute(); err == nil {
				t.Fatal("ambiguous batch reported success")
			}
			entries, err := mutation.ListEntries(dir)
			if err != nil || len(entries) != 1 || len(entries[0].Ops) != 3 || committed.Load() != 3 {
				t.Fatalf("entries=%+v, committed=%d, err=%v", entries, committed.Load(), err)
			}
			inverse, refused := mutation.InverseOps(entries[0])
			if len(inverse) != 0 || len(refused) != 3 {
				t.Fatalf("inverse=%+v, refused=%+v; no uncertain item may disappear", inverse, refused)
			}
		})
	}
}

func TestJournalUndoRecordsLinkBeforeInverseRequest(t *testing.T) {
	dir := journalRecoverySetup(t)
	original := mutation.JournalEntry{
		SchemaVersion: mutation.JournalSchemaVersion, RunID: "original", Library: "user",
		Operation: "items.tags.remove", Mode: "apply",
		Ops: []mutation.JournalOp{{
			ID: "remove", Key: "ABCD2345", Kind: "tag_remove", Status: "applied",
			Changes: []mutation.Change{{Field: "tags", Remove: "x"}},
		}},
	}
	if err := mutation.WriteEntry(dir, original); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		entries, err := mutation.ListEntries(dir)
		if err != nil || len(entries) != 2 || entries[0].Undoes != original.RunID || !entries[0].Partial {
			t.Errorf("inverse request preceded its durable link: entries=%+v err=%v", entries, err)
		}
		if r.Method == http.MethodGet {
			w.Header().Set("Last-Modified-Version", "100")
			_, _ = w.Write([]byte(`{"key":"ABCD2345","version":100,"data":{"tags":[]}}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	cmd := newJournalUndoCmd(&rootFlags{yes: true, asJSON: true, maxChanges: -1})
	cmd.SetArgs([]string{original.RunID})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 {
		t.Fatalf("requests=%d, want inverse GET and PATCH", requests.Load())
	}
}

func TestJournalUndoPreviewAndCompletedNoWriteAllowRetry(t *testing.T) {
	dir := journalRecoverySetup(t)
	original := mutation.JournalEntry{
		SchemaVersion: mutation.JournalSchemaVersion, RunID: "original", Library: "user",
		Operation: "items.tags.add", Mode: "apply",
		Ops: []mutation.JournalOp{{
			ID: "add", Key: "ABCD2345", Kind: "tag_add", Status: "applied",
			Changes: []mutation.Change{{Field: "tags", Add: "x"}},
		}},
	}
	if err := mutation.WriteEntry(dir, original); err != nil {
		t.Fatal(err)
	}
	var hasTag atomic.Bool
	var patches atomic.Int32
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method == http.MethodGet {
			tags := []map[string]any{}
			if hasTag.Load() {
				tags = append(tags, map[string]any{"tag": "x"})
			}
			w.Header().Set("Last-Modified-Version", "100")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"key": "ABCD2345", "version": 100, "data": map[string]any{"tags": tags},
			})
			return
		}
		patches.Add(1)
		hasTag.Store(false)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	for _, flags := range []*rootFlags{
		{asJSON: true, maxChanges: -1},
		{asJSON: true, yes: true, dryRun: true, maxChanges: -1},
	} {
		cmd := newJournalUndoCmd(flags)
		cmd.SetArgs([]string{original.RunID})
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := mutation.ListEntries(dir)
	if err != nil || len(entries) != 1 || requests.Load() != 0 {
		t.Fatalf("preview wrote or dispatched: entries=%+v requests=%d err=%v", entries, requests.Load(), err)
	}
	for range 2 {
		undoTagRunToCompletion(t, original.RunID)
		if err := refuseRecordedUndo(original.RunID); err != nil {
			t.Fatalf("completed no-write undo blocks retry: %v", err)
		}
	}
	if patches.Load() != 0 {
		t.Fatalf("no-op undo sent %d patches", patches.Load())
	}
	entries, err = mutation.ListEntries(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Undoes != "" || entry.Partial {
			t.Fatalf("completed no-write undo retained its reservation: %+v", entry)
		}
	}
	hasTag.Store(true)
	undoTagRunToCompletion(t, original.RunID)
	if patches.Load() != 1 || hasTag.Load() {
		t.Fatalf("retry did not apply: patches=%d hasTag=%v", patches.Load(), hasTag.Load())
	}
	if err := refuseRecordedUndo(original.RunID); ExitCode(err) != 9 {
		t.Fatalf("applied undo must block another retry: %v", err)
	}
}
