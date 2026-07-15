// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

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

	runA := journalTestEntry(t, "run-A", "items.tags.add")
	runA.WorkflowRunID = "workflow-1"
	runB := journalTestEntry(t, "run-B", "items.move")
	for _, e := range []mutation.JournalEntry{runA, runB} {
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
	if listed[0].Library != "user" || listed[1].Library != "user" {
		t.Fatalf("list libraries = %q/%q, want user/user", listed[0].Library, listed[1].Library)
	}

	var shown mutation.JournalEntry
	if err := json.Unmarshal([]byte(runJournalCmd(t, "show", "run-A")), &shown); err != nil {
		t.Fatalf("decode show: %v", err)
	}
	if shown.Operation != "items.tags.add" || len(shown.Ops) != 1 || shown.Ops[0].Status != "applied" {
		t.Errorf("show run-A = %+v", shown)
	}
	if shown.Library != "user" {
		t.Errorf("show library = %q, want user", shown.Library)
	}
	if shown.WorkflowRunID != "workflow-1" {
		t.Errorf("show workflow run ID = %q, want workflow-1", shown.WorkflowRunID)
	}

	humanListCmd := newJournalCmd(&rootFlags{})
	humanListCmd.SetArgs([]string{"list"})
	var humanList bytes.Buffer
	humanListCmd.SetOut(&humanList)
	humanListCmd.SetErr(&bytes.Buffer{})
	if err := humanListCmd.Execute(); err != nil {
		t.Fatalf("human journal list: %v", err)
	}
	if !strings.Contains(humanList.String(), "user") {
		t.Fatalf("human list = %q, want library column value", humanList.String())
	}
	if !strings.Contains(humanList.String(), "workflow-1") {
		t.Fatalf("human list = %q, want workflow run ID", humanList.String())
	}

	humanShowCmd := newJournalCmd(&rootFlags{})
	humanShowCmd.SetArgs([]string{"show", "run-A"})
	var humanShow bytes.Buffer
	humanShowCmd.SetOut(&humanShow)
	humanShowCmd.SetErr(&bytes.Buffer{})
	if err := humanShowCmd.Execute(); err != nil {
		t.Fatalf("human journal show: %v", err)
	}
	if !strings.Contains(humanShow.String(), "user") {
		t.Fatalf("human show = %q, want library value", humanShow.String())
	}
	if !strings.Contains(humanShow.String(), "workflow-1") {
		t.Fatalf("human show = %q, want workflow run ID", humanShow.String())
	}
}

func TestJournalListFiltersWorkflow(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	matching := journalTestEntry(t, "workflow-match", "items.tags.add")
	matching.WorkflowRunID = "workflow-1"
	other := journalTestEntry(t, "workflow-other", "items.move")
	other.WorkflowRunID = "workflow-2"
	for _, entry := range []mutation.JournalEntry{matching, other} {
		if err := mutation.WriteEntry(journalDir(), entry); err != nil {
			t.Fatalf("seed journal: %v", err)
		}
	}

	var filtered []mutation.JournalEntry
	if err := json.Unmarshal([]byte(runJournalCmd(t, "list", "--workflow", "workflow-1")), &filtered); err != nil {
		t.Fatalf("decode filtered list: %v", err)
	}
	if len(filtered) != 1 || filtered[0].RunID != "workflow-match" {
		t.Fatalf("filtered list = %+v, want only workflow-match", filtered)
	}

	var none []mutation.JournalEntry
	if err := json.Unmarshal([]byte(runJournalCmd(t, "list", "--workflow", "workflow-missing")), &none); err != nil {
		t.Fatalf("decode non-matching list: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("non-matching filtered list = %+v, want none", none)
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
	if entries[0].Library != "user" {
		t.Fatalf("recorded library = %q, want user", entries[0].Library)
	}

	// A preview (no --yes) must not record.
	if _, err := runMutation(context.Background(), &rootFlags{maxChanges: -1}, "items.tags.add", ops); err != nil {
		t.Fatalf("preview runMutation: %v", err)
	}
	if entries, _ = mutation.ListEntries(journalDir()); len(entries) != 1 {
		t.Errorf("preview should not record; entries = %d, want 1", len(entries))
	}
}

func TestRecorderStampsWorkflowRunID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	savedWorkflowRunID := activeWorkflowRunID
	activeWorkflowRunID = "workflow-1"
	t.Cleanup(func() { activeWorkflowRunID = savedWorkflowRunID })

	recordMutationJournal(mutation.Envelope{
		Operation: "items.tags.add",
		Mode:      "apply",
		OK:        true,
		Plan: mutation.Plan{Operations: []mutation.Op{
			{ID: "o1", Key: "K1", Kind: "tag_add", Changes: []mutation.Change{{Field: "tags", Add: "ml"}}},
		}},
		Result: &mutation.Result{
			Summary: mutation.ResultSummary{Attempted: 1, Applied: 1},
			Items:   []mutation.ResultItem{{OpID: "o1", Key: "K1", Status: "applied"}},
		},
	})

	entries, err := mutation.ListEntries(journalDir())
	if err != nil {
		t.Fatalf("list recorded workflow run: %v", err)
	}
	if len(entries) != 1 || entries[0].WorkflowRunID != "workflow-1" {
		t.Fatalf("recorded entries = %+v, want workflow run ID workflow-1", entries)
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

func TestJournalUndoRefusesLibraryMismatchAndAllowsMatchingScope(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	saved := activeGroupID
	defer func() { activeGroupID = saved }()

	activeGroupID = ""
	personal := journalTestEntry(t, "personal-run", "items.tags.add")
	personal.Library = "" // pre-fix entries had no library field and are personal.
	if err := mutation.WriteEntry(journalDir(), personal); err != nil {
		t.Fatalf("seed personal journal: %v", err)
	}

	activeGroupID = "12345"
	groupCmd := newJournalCmd(&rootFlags{asJSON: true})
	groupCmd.SilenceErrors, groupCmd.SilenceUsage = true, true
	groupCmd.SetArgs([]string{"undo", "personal-run"})
	var groupOut, groupErr bytes.Buffer
	groupCmd.SetOut(&groupOut)
	groupCmd.SetErr(&groupErr)
	err := groupCmd.Execute()
	if err == nil {
		t.Fatalf("group undo of personal entry succeeded; output=%s stderr=%s", groupOut.String(), groupErr.String())
	}
	if msg := err.Error(); !strings.Contains(msg, "journal library mismatch") || !strings.Contains(msg, "user") || !strings.Contains(msg, "group 12345") {
		t.Fatalf("group mismatch error = %q, want user/group mismatch", msg)
	}

	activeGroupID = ""
	personalCmd := newJournalCmd(&rootFlags{asJSON: true})
	personalCmd.SilenceErrors, personalCmd.SilenceUsage = true, true
	personalCmd.SetArgs([]string{"undo", "personal-run"})
	var personalOut bytes.Buffer
	personalCmd.SetOut(&personalOut)
	personalCmd.SetErr(&bytes.Buffer{})
	if err := personalCmd.Execute(); err != nil {
		t.Fatalf("personal undo preview: %v", err)
	}
	var personalEnv mutation.Envelope
	if err := json.Unmarshal(personalOut.Bytes(), &personalEnv); err != nil {
		t.Fatalf("decode personal undo %q: %v", personalOut.String(), err)
	}
	if personalEnv.Mode != "preview" || len(personalEnv.Plan.Operations) != 1 {
		t.Fatalf("personal undo env = %+v, want one preview op", personalEnv)
	}

	groupEntry := journalTestEntry(t, "group-run", "items.tags.add")
	groupEntry.Library = "group:12345"
	if err := mutation.WriteEntry(journalDir(), groupEntry); err != nil {
		t.Fatalf("seed personal dir with group journal entry: %v", err)
	}
	personalMismatchCmd := newJournalCmd(&rootFlags{asJSON: true})
	personalMismatchCmd.SilenceErrors, personalMismatchCmd.SilenceUsage = true, true
	personalMismatchCmd.SetArgs([]string{"undo", "group-run"})
	personalMismatchCmd.SetOut(&bytes.Buffer{})
	personalMismatchCmd.SetErr(&bytes.Buffer{})
	err = personalMismatchCmd.Execute()
	if err == nil {
		t.Fatal("personal undo of group entry succeeded")
	}
	if msg := err.Error(); !strings.Contains(msg, "journal library mismatch") || !strings.Contains(msg, "group 12345") || !strings.Contains(msg, "user") {
		t.Fatalf("personal mismatch error = %q, want group/user mismatch", msg)
	}

	activeGroupID = "12345"
	if err := mutation.WriteEntry(journalDir(), groupEntry); err != nil {
		t.Fatalf("seed group journal: %v", err)
	}
	matchingGroupCmd := newJournalCmd(&rootFlags{asJSON: true})
	matchingGroupCmd.SilenceErrors, matchingGroupCmd.SilenceUsage = true, true
	matchingGroupCmd.SetArgs([]string{"undo", "group-run"})
	var matchingGroupOut bytes.Buffer
	matchingGroupCmd.SetOut(&matchingGroupOut)
	matchingGroupCmd.SetErr(&bytes.Buffer{})
	if err := matchingGroupCmd.Execute(); err != nil {
		t.Fatalf("group undo preview: %v", err)
	}
	var groupEnv mutation.Envelope
	if err := json.Unmarshal(matchingGroupOut.Bytes(), &groupEnv); err != nil {
		t.Fatalf("decode group undo %q: %v", matchingGroupOut.String(), err)
	}
	if groupEnv.Mode != "preview" || len(groupEnv.Plan.Operations) != 1 {
		t.Fatalf("group undo env = %+v, want one preview op", groupEnv)
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
