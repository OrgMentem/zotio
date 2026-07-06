// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// PATCH(glean roadmap-phase3): cover the journal commands and the apply-time
// recorder hook (with HOME isolated so writes land in a temp dir).

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zotio/internal/mutation"
)

func runJournalCmd(t *testing.T, args ...string) string {
	t.Helper()
	flags := &rootFlags{asJSON: true}
	cmd := newJournalCmd(flags)
	cmd.SetArgs(args)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("journal %v: %v", args, err)
	}
	return out.String()
}

func journalTestEntry(t *testing.T, runID, op string) mutation.JournalEntry {
	t.Helper()
	env := mutation.Envelope{
		Operation: op, Mode: "apply", OK: true,
		Plan:   mutation.Plan{Operations: []mutation.Op{{ID: "o1", Key: "K1", Kind: "tag_add", Changes: []mutation.Change{{Field: "tags", Add: "ml"}}}}},
		Result: &mutation.Result{Summary: mutation.ResultSummary{Attempted: 1, Applied: 1}, Items: []mutation.ResultItem{{OpID: "o1", Key: "K1", Status: "applied"}}},
	}
	e, ok := mutation.BuildJournalEntry(env, time.Now())
	if !ok {
		t.Fatal("BuildJournalEntry returned ok=false for an apply envelope")
	}
	e.RunID = runID
	return e
}

func TestJournalListEmpty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	flags := &rootFlags{} // human output
	cmd := newJournalCmd(flags)
	cmd.SetArgs([]string{"list"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("journal list: %v", err)
	}
	if got := out.String(); len(got) < 2 || got[:2] != "No" {
		t.Errorf("empty journal list = %q, want a 'No ... recorded' notice", got)
	}
}

func TestJournalListAndShow(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	for _, e := range []mutation.JournalEntry{
		journalTestEntry(t, "run-A", "items.tags.add"),
		journalTestEntry(t, "run-B", "items.move"),
	} {
		if err := mutation.WriteEntry(journalDir(), e); err != nil {
			t.Fatalf("seed journal: %v", err)
		}
	}

	var listed []mutation.JournalEntry
	if err := json.Unmarshal([]byte(runJournalCmd(t, "list")), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listed) != 2 || listed[0].RunID != "run-B" {
		t.Fatalf("list = %+v, want newest-first [run-B, run-A]", listed)
	}

	var shown mutation.JournalEntry
	if err := json.Unmarshal([]byte(runJournalCmd(t, "show", "run-A")), &shown); err != nil {
		t.Fatalf("decode show: %v", err)
	}
	if shown.Operation != "items.tags.add" || len(shown.Ops) != 1 || shown.Ops[0].Status != "applied" {
		t.Errorf("show run-A = %+v", shown)
	}
}

func TestRecorderWritesAppliedRun(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mutationJournalRecorder = recordMutationJournal
	t.Cleanup(func() { mutationJournalRecorder = nil })

	ops := []mutation.Op{{ID: "op", Key: "K", Kind: "tag_add", Changes: []mutation.Change{{Field: "tags", Add: "x"}}, Apply: func() (string, any, error) { return "applied", nil, nil }}}
	if _, err := runMutation(context.Background(), &rootFlags{yes: true, maxChanges: -1}, "items.tags.add", ops); err != nil {
		t.Fatalf("runMutation: %v", err)
	}

	entries, err := mutation.ListEntries(journalDir())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 1 || entries[0].Operation != "items.tags.add" || entries[0].Summary.Applied != 1 {
		t.Fatalf("recorded entries = %+v, want one applied items.tags.add run", entries)
	}

	// A preview (no --yes) must not record.
	if _, err := runMutation(context.Background(), &rootFlags{maxChanges: -1}, "items.tags.add", ops); err != nil {
		t.Fatalf("preview runMutation: %v", err)
	}
	if entries, _ = mutation.ListEntries(journalDir()); len(entries) != 1 {
		t.Errorf("preview should not record; entries = %d, want 1", len(entries))
	}
}

func TestJournalUndoPreviewPlan(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	entry := mutation.JournalEntry{
		RunID: "r2", Operation: "items.tags.add", Mode: "apply",
		Ops: []mutation.JournalOp{
			{ID: "a", Key: "K1", Kind: "tag_add", Status: "applied", Changes: []mutation.Change{{Field: "tags", Add: "ml"}}},
			{ID: "b", Key: "K2", Kind: "missing_doi", Status: "applied", Changes: []mutation.Change{{Field: "DOI", Add: "10/x"}}},
		},
	}
	if err := mutation.WriteEntry(journalDir(), entry); err != nil {
		t.Fatalf("seed journal: %v", err)
	}

	flags := &rootFlags{asJSON: true} // preview (no --yes)
	cmd := newJournalCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{"undo", "r2"})
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("undo preview: %v; stderr=%s", err, errOut.String())
	}

	var env mutation.Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	if env.Mode != "preview" || len(env.Plan.Operations) != 1 {
		t.Fatalf("plan = %+v, want one reversible op in preview", env.Plan)
	}
	op := env.Plan.Operations[0]
	if op.Kind != "undo.tag_add" || len(op.Changes) != 1 || op.Changes[0].Field != "tags" || op.Changes[0].Remove != "ml" {
		t.Errorf("inverse op = %+v, want undo.tag_add removing ml", op)
	}
	if !strings.Contains(errOut.String(), "missing_doi") {
		t.Errorf("stderr should report the refused DOI op: %q", errOut.String())
	}
}

func TestJournalUndoAppliesTagReversal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := newItemTagTestServer(t, map[string]string{"K1": "5"}, map[string][]map[string]any{
		"K1": {{"tag": "ml"}, {"tag": "keep"}},
	})
	t.Setenv("ZOTERO_BASE_URL", srv.server.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

	entry := mutation.JournalEntry{
		SchemaVersion: mutation.JournalSchemaVersion, RunID: "r1", Operation: "items.tags.add", Mode: "apply", OK: true,
		Timestamp: time.Now(), Summary: mutation.ResultSummary{Attempted: 1, Applied: 1},
		Ops: []mutation.JournalOp{
			{ID: "items.tags.add:K1", Key: "K1", Kind: "tag_add", Status: "applied", Changes: []mutation.Change{{Field: "tags", Add: "ml"}}},
		},
	}
	if err := mutation.WriteEntry(journalDir(), entry); err != nil {
		t.Fatalf("seed journal: %v", err)
	}

	flags := &rootFlags{asJSON: true, yes: true, maxChanges: -1}
	cmd := newJournalCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{"undo", "r1"})
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("undo apply: %v; stderr=%s", err, errOut.String())
	}

	var env mutation.Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	if !env.OK || env.Result == nil || env.Result.Summary.Applied != 1 {
		t.Fatalf("undo env = %+v, want one applied reversal", env)
	}

	body, ok := srv.patchBodies["K1"]
	if !ok {
		t.Fatal("expected a PATCH to K1")
	}
	tags, _ := body["tags"].([]any)
	if len(tags) != 1 {
		t.Fatalf("patched tags = %v, want only [keep] after removing ml", tags)
	}
	if m, _ := tags[0].(map[string]any); m["tag"] != "keep" {
		t.Errorf("remaining tag = %v, want keep", tags[0])
	}
}
