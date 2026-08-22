// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zotio/internal/client"
	"zotio/internal/config"
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

type journalListEnvelope struct {
	Results []mutation.JournalEntry `json:"results"`
	Meta    map[string]any          `json:"meta"`
}

func decodeJournalList(t *testing.T, raw string) journalListEnvelope {
	t.Helper()
	var env journalListEnvelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("decode journal list: %v", err)
	}
	return env
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
		if err := mutation.WriteEntry(helpersTestJournalDir(t), e); err != nil {
			t.Fatalf("seed journal: %v", err)
		}
	}

	listed := decodeJournalList(t, runJournalCmd(t, "list"))
	if len(listed.Results) != 2 || listed.Results[0].RunID != "run-B" {
		t.Fatalf("list = %+v, want newest-first [run-B, run-A]", listed.Results)
	}
	if listed.Meta["source"] != "local" || listed.Meta["resource_type"] != "journal" {
		t.Fatalf("list meta = %+v, want local journal provenance", listed.Meta)
	}
	if listed.Results[0].Library != "user" || listed.Results[1].Library != "user" {
		t.Fatalf("list libraries = %q/%q, want user/user", listed.Results[0].Library, listed.Results[1].Library)
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
func TestApplyUndoCreateTrashesCreatedKeyWithVersion(t *testing.T) {
	var gotHeader string
	var gotDeleted any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Last-Modified-Version", "7")
			_, _ = w.Write([]byte(`{}`))
		case http.MethodPatch:
			gotHeader = r.Header.Get("If-Unmodified-Since-Version")
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode trash body: %v", err)
			}
			gotDeleted = body["deleted"]
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected", http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()

	c := client.New(&config.Config{BaseURL: srv.URL + "/users/0"}, 0, 0)
	status, _, err := applyUndoCreate(c, "/items/REALKEY1")
	if err != nil || status != "applied" {
		t.Fatalf("applyUndoCreate = (%q, %v), want applied", status, err)
	}
	if gotHeader != "7" || gotDeleted != float64(1) {
		t.Fatalf("trash request header/deleted = %q/%v, want 7/1", gotHeader, gotDeleted)
	}
}

func TestJournalListRendersPrefixWhenTailIncomplete(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	entry := journalTestEntry(t, "run-complete", "items.tags.add")
	dir := helpersTestJournalDir(t)
	if err := mutation.WriteEntry(dir, entry); err != nil {
		t.Fatalf("seed journal: %v", err)
	}
	journalPath := filepath.Join(dir, mutation.JournalFileName)
	f, err := os.OpenFile(journalPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open journal for torn tail: %v", err)
	}
	if _, err := f.WriteString(`{"run_id":`); err != nil {
		_ = f.Close()
		t.Fatalf("append torn journal tail: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close torn journal tail: %v", err)
	}

	flags := &rootFlags{asJSON: true}
	cmd := newJournalCmd(flags)
	cmd.SetArgs([]string{"list"})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	err = cmd.Execute()
	if ExitCode(err) != 13 {
		t.Fatalf("journal list exit code = %d, want 13 (err=%v)", ExitCode(err), err)
	}
	entries := decodeJournalList(t, out.String())
	if len(entries.Results) != 1 || entries.Results[0].RunID != "run-complete" {
		t.Fatalf("rendered entries = %+v, want complete prefix", entries.Results)
	}

	humanCmd := newJournalCmd(&rootFlags{})
	humanCmd.SetArgs([]string{"list"})
	humanCmd.SilenceErrors, humanCmd.SilenceUsage = true, true
	var humanOut bytes.Buffer
	humanCmd.SetOut(&humanOut)
	humanCmd.SetErr(&bytes.Buffer{})
	if err := humanCmd.Execute(); ExitCode(err) != 13 {
		t.Fatalf("human journal list exit code = %d, want 13 (err=%v)", ExitCode(err), err)
	}
	if !strings.Contains(humanOut.String(), "run-complete") {
		t.Fatalf("human rendered prefix = %q, want run-complete", humanOut.String())
	}
}

func TestJournalUndoUsesLegacyPrefixOrReportsTornTail(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	savedGroupID := activeGroupIDLocked()
	t.Cleanup(func() { setActiveGroupID(savedGroupID) })

	setActiveGroupID("")
	dir := helpersTestJournalDir(t)
	entry := journalTestEntry(t, "legacy-complete", "items.tags.add")
	entry.Library = "group:12345"
	if err := mutation.WriteEntry(dir, entry); err != nil {
		t.Fatalf("seed legacy journal: %v", err)
	}
	journalPath := filepath.Join(dir, mutation.JournalFileName)
	f, err := os.OpenFile(journalPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open legacy journal for torn tail: %v", err)
	}
	if _, err := f.WriteString(`{"run_id":`); err != nil {
		_ = f.Close()
		t.Fatalf("append legacy torn tail: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close legacy torn tail: %v", err)
	}

	setActiveGroupID("12345") // The group journal remains missing: definite absence.
	missingCmd := newJournalCmd(&rootFlags{})
	missingCmd.SetArgs([]string{"undo", "missing"})
	missingCmd.SilenceErrors, missingCmd.SilenceUsage = true, true
	missingCmd.SetOut(&bytes.Buffer{})
	missingCmd.SetErr(&bytes.Buffer{})
	err = missingCmd.Execute()
	if ExitCode(err) != 13 {
		t.Fatalf("journal undo exit code = %d, want 13 (err=%v)", ExitCode(err), err)
	}
	var incomplete *mutation.IncompleteJournalError
	if !errors.As(err, &incomplete) {
		t.Fatalf("journal undo error = %v, want IncompleteJournalError", err)
	}

	completeCmd := newJournalCmd(&rootFlags{asJSON: true})
	completeCmd.SetArgs([]string{"undo", "legacy-complete"})
	completeCmd.SilenceErrors, completeCmd.SilenceUsage = true, true
	var out bytes.Buffer
	completeCmd.SetOut(&out)
	completeCmd.SetErr(&bytes.Buffer{})
	if err := completeCmd.Execute(); err != nil {
		t.Fatalf("journal undo of legacy complete prefix: %v", err)
	}
	var env mutation.Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode legacy undo output: %v", err)
	}
	if env.Mode != "preview" || len(env.Plan.Operations) != 1 {
		t.Fatalf("legacy undo envelope = %+v, want one preview operation", env)
	}
}
func TestJournalListFiltersWorkflow(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	matching := journalTestEntry(t, "workflow-match", "items.tags.add")
	matching.WorkflowRunID = "workflow-1"
	other := journalTestEntry(t, "workflow-other", "items.move")
	other.WorkflowRunID = "workflow-2"
	for _, entry := range []mutation.JournalEntry{matching, other} {
		if err := mutation.WriteEntry(helpersTestJournalDir(t), entry); err != nil {
			t.Fatalf("seed journal: %v", err)
		}
	}

	filtered := decodeJournalList(t, runJournalCmd(t, "list", "--workflow", "workflow-1"))
	if len(filtered.Results) != 1 || filtered.Results[0].RunID != "workflow-match" {
		t.Fatalf("filtered list = %+v, want only workflow-match", filtered.Results)
	}

	none := decodeJournalList(t, runJournalCmd(t, "list", "--workflow", "workflow-missing"))
	if len(none.Results) != 0 {
		t.Fatalf("non-matching filtered list = %+v, want none", none.Results)
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

	entries, err := mutation.ListEntries(helpersTestJournalDir(t))
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
	if entries, _ = mutation.ListEntries(helpersTestJournalDir(t)); len(entries) != 1 {
		t.Errorf("preview should not record; entries = %d, want 1", len(entries))
	}
}
func TestRecorderJournalFailureDegradesAppliedRun(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Dir(helpersTestJournalDir(t)), 0o700); err != nil {
		t.Fatalf("creating journal parent: %v", err)
	}
	if err := os.WriteFile(helpersTestJournalDir(t), []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("blocking journal directory: %v", err)
	}

	mutationJournalRecorder = recordMutationJournal
	t.Cleanup(func() { mutationJournalRecorder = nil })
	ops := []mutation.Op{{ID: "op", Key: "K", Kind: "tag_add", Changes: []mutation.Change{{Field: "tags", Add: "x"}}, Apply: func() (string, any, error) {
		return "applied", nil, nil
	}}}
	env, err := runMutation(context.Background(), &rootFlags{yes: true, maxChanges: -1}, "items.tags.add", ops)
	if ExitCode(err) != 13 {
		t.Fatalf("ExitCode(runMutation error) = %d, want 13; err = %v", ExitCode(err), err)
	}
	if env.Result == nil || env.Result.Summary.Applied != 1 {
		t.Fatalf("applied result = %+v, want one applied operation", env.Result)
	}
	if len(env.Warnings) != 1 || !strings.Contains(env.Warnings[0], "applied but not journaled") || !strings.Contains(env.Warnings[0], "creating journal dir") {
		t.Fatalf("warnings = %#v, want journal failure warning", env.Warnings)
	}

	flags := &rootFlags{asJSON: true}
	cmd := newJournalCmd(flags)
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := renderMutation(cmd, flags, env, nil); err != nil {
		t.Fatalf("render mutation: %v", err)
	}
	var rendered mutation.Envelope
	if err := json.Unmarshal(out.Bytes(), &rendered); err != nil {
		t.Fatalf("decode rendered mutation: %v", err)
	}
	if len(rendered.Warnings) != 1 || rendered.Result == nil || rendered.Result.Summary.Applied != 1 {
		t.Fatalf("rendered mutation = %+v, want applied result with warning", rendered)
	}
}

func TestRecorderStampsWorkflowRunID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	savedWorkflowRunID := activeWorkflowRunID
	activeWorkflowRunID = "workflow-1"
	t.Cleanup(func() { activeWorkflowRunID = savedWorkflowRunID })

	env := mutation.Envelope{
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
	}
	if err := recordMutationJournal(&env); err != nil {
		t.Fatalf("record workflow run: %v", err)
	}
	// The run ID must come back through the envelope: without it an agent cannot
	// undo its own write from the write's own response.
	journal, ok := env.Journal.(map[string]any)
	if !ok {
		t.Fatalf("env.Journal = %#v, want the recorded run ID", env.Journal)
	}
	if runID, _ := journal["run_id"].(string); runID == "" {
		t.Fatalf("journal = %#v, want a non-empty run_id", journal)
	}
	if journal["workflow_run_id"] != "workflow-1" {
		t.Errorf("journal workflow_run_id = %v, want workflow-1", journal["workflow_run_id"])
	}

	entries, err := mutation.ListEntries(helpersTestJournalDir(t))
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
	if err := mutation.WriteEntry(helpersTestJournalDir(t), entry); err != nil {
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
	// Under --json the refused DOI op is reported in the envelope, not stderr
	// prose: renderMutation only prints Warnings to stderr for interactive
	// (non-JSON) callers.
	if errOut.Len() != 0 {
		t.Errorf("JSON undo should not print refusal prose to stderr, got %q", errOut.String())
	}
	if len(env.Warnings) != 1 || !strings.Contains(env.Warnings[0], "missing_doi") || !strings.Contains(env.Warnings[0], "op b") {
		t.Errorf("warnings = %#v, want the refused missing_doi op (id b)", env.Warnings)
	}
	if env.OK != true {
		t.Errorf("env.OK = %v, want true: one op reversed, the other refused but the reversible one still previews fine", env.OK)
	}
	refused, ok := env.Journal.(map[string]any)
	if !ok {
		t.Fatalf("env.Journal = %#v, want a map with the structured refusal list", env.Journal)
	}
	if refusedList, _ := refused["refused"].([]any); len(refusedList) != 1 {
		t.Errorf("Journal[\"refused\"] = %#v, want exactly one refusal", refused["refused"])
	}
}

func TestJournalUndoReportsRefusalsInJSON(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	entry := mutation.JournalEntry{
		RunID: "mixed-run", Operation: "items.tags.add", Mode: "apply",
		Ops: []mutation.JournalOp{
			{ID: "a", Key: "K1", Kind: "tag_add", Status: "applied", Changes: []mutation.Change{{Field: "tags", Add: "ml"}}},
			{ID: "b", Key: "K2", Kind: "item_delete", Status: "applied", Changes: []mutation.Change{{Field: "deleted", Add: true}}},
		},
	}
	if err := mutation.WriteEntry(helpersTestJournalDir(t), entry); err != nil {
		t.Fatalf("seed journal: %v", err)
	}

	flags := &rootFlags{asJSON: true} // preview (no --yes)
	cmd := newJournalCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{"undo", "mixed-run"})
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
	if len(env.Plan.Operations) != 1 {
		t.Fatalf("plan = %+v, want only the reversible tag op planned", env.Plan)
	}

	// The refusal must be machine-readable, not just present as free text
	// somewhere in stderr: decode the structured Journal payload and check its
	// fields.
	journal, ok := env.Journal.(map[string]any)
	if !ok {
		t.Fatalf("env.Journal = %#v, want a map carrying the refusal list", env.Journal)
	}
	refusedRaw, ok := journal["refused"].([]any)
	if !ok || len(refusedRaw) != 1 {
		t.Fatalf("Journal[\"refused\"] = %#v, want exactly one refusal", journal["refused"])
	}
	refusal, ok := refusedRaw[0].(map[string]any)
	if !ok {
		t.Fatalf("refusal entry = %#v, want an object", refusedRaw[0])
	}
	if refusal["op_id"] != "b" || refusal["key"] != "K2" || refusal["kind"] != "item_delete" {
		t.Fatalf("refusal = %+v, want op_id=b key=K2 kind=item_delete", refusal)
	}
	reason, _ := refusal["reason"].(string)
	if !strings.Contains(reason, "deleted") {
		t.Fatalf("refusal reason = %q, want it to name the unreversible field", reason)
	}

	if len(env.Warnings) != 1 || !strings.Contains(env.Warnings[0], "item_delete") || !strings.Contains(env.Warnings[0], "op b") {
		t.Fatalf("warnings = %#v, want the refusal named by kind and op id", env.Warnings)
	}
}

func TestJournalUndoAllRefusedEmitsEnvelope(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	entry := mutation.JournalEntry{
		RunID: "all-refused-run", Operation: "items.delete", Mode: "apply",
		Ops: []mutation.JournalOp{
			{ID: "a", Key: "K1", Kind: "item_delete", Status: "applied", Changes: []mutation.Change{{Field: "deleted", Add: true}}},
			{ID: "b", Key: "K2", Kind: "item_delete", Status: "applied", Changes: []mutation.Change{{Field: "deleted", Add: true}}},
		},
	}
	if err := mutation.WriteEntry(helpersTestJournalDir(t), entry); err != nil {
		t.Fatalf("seed journal: %v", err)
	}

	flags := &rootFlags{asJSON: true} // preview (no --yes)
	cmd := newJournalCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{"undo", "all-refused-run"})
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	// A fully-refused run still emits a full envelope on stdout (checked
	// below) but degrades the exit code: a caller that only checks the
	// process result must not see a clean exit from a run that undid nothing.
	err := cmd.Execute()
	if ExitCode(err) != 13 {
		t.Fatalf("undo of fully-refused run exit code = %d (err=%v), want 13 (degraded)", ExitCode(err), err)
	}

	// This is the regression this test guards: today the all-refused path
	// prints "Nothing reversible in run ..." prose instead of JSON, which
	// fails to decode.
	var env mutation.Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("undo output is not valid JSON under --json: %q: %v", out.String(), err)
	}
	if len(env.Plan.Operations) != 0 {
		t.Fatalf("plan = %+v, want zero planned ops: nothing was reversible", env.Plan)
	}
	if env.OK {
		t.Fatalf("env.OK = true, want false: every op was refused, which is not a successful reversal")
	}

	journal, ok := env.Journal.(map[string]any)
	if !ok {
		t.Fatalf("env.Journal = %#v, want a map carrying the refusal list", env.Journal)
	}
	refusedRaw, _ := journal["refused"].([]any)
	if len(refusedRaw) != 2 {
		t.Fatalf("Journal[\"refused\"] = %#v, want both item_delete ops refused", journal["refused"])
	}
	if len(env.Warnings) != 2 {
		t.Fatalf("warnings = %#v, want one entry per refused op", env.Warnings)
	}
}

func TestJournalUndoRefusesLibraryMismatchAndAllowsMatchingScope(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	saved := activeGroupIDLocked()
	defer func() { setActiveGroupID(saved) }()

	setActiveGroupID("")
	personal := journalTestEntry(t, "personal-run", "items.tags.add")
	personal.Library = "" // pre-fix entries had no library field and are personal.
	if err := mutation.WriteEntry(helpersTestJournalDir(t), personal); err != nil {
		t.Fatalf("seed personal journal: %v", err)
	}

	setActiveGroupID("12345")
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

	setActiveGroupID("")
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
	if err := mutation.WriteEntry(helpersTestJournalDir(t), groupEntry); err != nil {
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

	setActiveGroupID("12345")
	if err := mutation.WriteEntry(helpersTestJournalDir(t), groupEntry); err != nil {
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
	if err := mutation.WriteEntry(helpersTestJournalDir(t), entry); err != nil {
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

func TestJournalUndoAutomaticAddPreservesReplacementManualTag(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := newItemTagTestServer(t, map[string]string{"K1": "5"}, map[string][]map[string]any{
		"K1": {{"tag": "ml", "type": float64(0)}, {"tag": "keep", "type": float64(0)}},
	})
	t.Setenv("ZOTERO_BASE_URL", srv.server.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

	entry := mutation.JournalEntry{
		SchemaVersion: mutation.JournalSchemaVersion, RunID: "automatic-add", Operation: "items.tags.add", Mode: "apply", OK: true,
		Timestamp: time.Now(), Summary: mutation.ResultSummary{Attempted: 1, Applied: 1},
		Ops: []mutation.JournalOp{
			{
				ID: "items.tags.add:K1", Key: "K1", Kind: "tag_add", Status: "applied",
				Changes: []mutation.Change{{Field: "tags", Add: "ml", TagType: 1}},
			},
		},
	}
	if err := mutation.WriteEntry(helpersTestJournalDir(t), entry); err != nil {
		t.Fatalf("seed journal: %v", err)
	}

	cmd := newJournalCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{"undo", "automatic-add"})
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
	if !env.OK || env.Result == nil || env.Result.Summary.NoOp != 1 {
		t.Fatalf("undo env = %+v, want no-op preserving manual replacement", env)
	}
	if srv.patchCounts["K1"] != 0 {
		t.Fatalf("PATCH count = %d, want 0", srv.patchCounts["K1"])
	}
}

// applyUndoMembership previously built its header map conditionally
// ("if version > 0"), so an item whose version could not be resolved was
// PATCHed with no If-Unmodified-Since-Version at all. Zotero rejects that with
// an opaque 428, and a server that accepted it would let the reversal clobber a
// concurrent edit. The write must now be refused before it is dispatched.
//
// The positive case is asserted alongside it so the guard cannot regress into
// refusing every reversal.
func TestApplyUndoMembershipRequiresWritePlaneVersion(t *testing.T) {
	for _, tc := range []struct {
		name        string
		version     string
		wantStatus  string
		wantPatched bool
	}{
		{name: "version present", version: "11", wantStatus: "applied", wantPatched: true},
		{name: "version absent", version: "", wantStatus: "failed", wantPatched: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var patches int
			var gotHeader string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					// An absent version means BOTH sources are absent: the local
					// API omits the header and reports an empty object version
					// for items it has never pushed upstream.
					if tc.version != "" {
						w.Header().Set("Last-Modified-Version", tc.version)
					}
					_, _ = w.Write([]byte(`{"key":"K1","data":{"tags":[{"tag":"ml"},{"tag":"keep"}],"collections":[]}}`))
				case http.MethodPatch:
					patches++
					gotHeader = r.Header.Get("If-Unmodified-Since-Version")
					w.WriteHeader(http.StatusNoContent)
				default:
					http.Error(w, "unexpected", http.StatusMethodNotAllowed)
				}
			}))
			defer srv.Close()

			c := client.New(&config.Config{BaseURL: srv.URL + "/users/0"}, 0, 0)
			changes := []mutation.Change{{Field: "tags", Remove: "ml"}}
			status, detail, err := applyUndoMembership(c, "/items/K1", changes)

			if status != tc.wantStatus {
				t.Fatalf("status = %q (detail %v, err %v), want %q", status, detail, err, tc.wantStatus)
			}
			if tc.wantPatched {
				if patches != 1 {
					t.Fatalf("patches = %d, want exactly 1", patches)
				}
				if gotHeader != tc.version {
					t.Errorf("If-Unmodified-Since-Version = %q, want %q", gotHeader, tc.version)
				}
				return
			}
			if patches != 0 {
				t.Fatalf("dispatched %d unguarded PATCH(es); want the write refused before any request", patches)
			}
			if err == nil {
				t.Fatal("want an error naming the missing precondition, got nil")
			}
			if !strings.Contains(err.Error(), "If-Unmodified-Since-Version") {
				t.Errorf("error = %q, want it to name the missing precondition", err.Error())
			}
		})
	}
}

// Both undo helpers read the item they are about to PATCH. That read must come
// from the plane the PATCH lands on: newWriteClient only flattens the resolved
// write base into BaseURL when resolution SUCCEEDS and silently swallows the
// error when it does not, leaving reads on the local desktop API while writes
// still route to api.zotero.org. The two planes version objects independently,
// so a local version is not a valid Web precondition -- it either spuriously
// conflicts or, when the numbers happen to coincide, guards nothing.
//
// For applyUndoMembership the membership state matters as much as the version:
// inverting tags computed from the read plane would overwrite the write plane's
// copy with stale data.
func TestJournalUndoTakesPreconditionFromWritePlane(t *testing.T) {
	newPlanes := func(t *testing.T, onPatch func(*http.Request)) *client.Client {
		t.Helper()
		readServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("undo read the local plane at %s; the precondition must come from the write plane", r.URL.Path)
			http.Error(w, "wrong plane", http.StatusInternalServerError)
		}))
		t.Cleanup(readServer.Close)
		writeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				w.Header().Set("Last-Modified-Version", "900")
				// "upstream" is present only on the write plane, so a body built
				// from it proves the read landed there.
				_, _ = w.Write([]byte(`{"key":"K1","version":900,"data":{"tags":[{"tag":"ml"},{"tag":"upstream"}],"collections":[]}}`))
			case http.MethodPatch:
				onPatch(r)
				w.WriteHeader(http.StatusNoContent)
			default:
				http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
			}
		}))
		t.Cleanup(writeServer.Close)

		c := client.New(&config.Config{BaseURL: readServer.URL + "/users/0"}, 5*time.Second, 0)
		c.NoCache = true
		c.ResolveWriteBase = func(context.Context) (string, error) { return writeServer.URL + "/users/0", nil }
		return c
	}

	t.Run("membership", func(t *testing.T) {
		var precondition string
		var body map[string]any
		patches := 0
		c := newPlanes(t, func(r *http.Request) {
			patches++
			precondition = r.Header.Get("If-Unmodified-Since-Version")
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode patch body: %v", err)
			}
		})

		status, detail, err := applyUndoMembership(c, "/items/K1", []mutation.Change{{Field: "tags", Remove: "ml"}})
		if err != nil || status != "applied" {
			t.Fatalf("applyUndoMembership = (%q, %v, %v), want applied", status, detail, err)
		}
		if patches != 1 {
			t.Fatalf("PATCH count = %d, want 1", patches)
		}
		if precondition != "900" {
			t.Errorf("If-Unmodified-Since-Version = %q, want the write plane's 900", precondition)
		}
		tags, _ := body["tags"].([]any)
		if len(tags) != 1 {
			t.Fatalf("patched tags = %v, want only the write plane's remaining tag", tags)
		}
		if m, _ := tags[0].(map[string]any); m["tag"] != "upstream" {
			t.Errorf("remaining tag = %v, want upstream: the inverse must be computed from the write plane's copy", tags[0])
		}
	})

	t.Run("create", func(t *testing.T) {
		var precondition string
		patches := 0
		c := newPlanes(t, func(r *http.Request) {
			patches++
			precondition = r.Header.Get("If-Unmodified-Since-Version")
		})

		status, detail, err := applyUndoCreate(c, "/items/K1")
		if err != nil || status != "applied" {
			t.Fatalf("applyUndoCreate = (%q, %v, %v), want applied", status, detail, err)
		}
		if patches != 1 {
			t.Fatalf("PATCH count = %d, want 1", patches)
		}
		if precondition != "900" {
			t.Errorf("If-Unmodified-Since-Version = %q, want the write plane's 900", precondition)
		}
	})
}
func TestUndoHelpers(t *testing.T) {
	t.Run("contains", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			items []string
			want  string
			found bool
		}{
			{name: "present", items: []string{"a", "b", "c"}, want: "b", found: true},
			{name: "absent", items: []string{"a", "b", "c"}, want: "z", found: false},
			{name: "empty nil", items: nil, want: "a", found: false},
			{name: "empty slice", items: []string{}, want: "a", found: false},
			{name: "case mismatch", items: []string{"Tag"}, want: "tag", found: false},
			{name: "case exact", items: []string{"Tag", "tag"}, want: "tag", found: true},
			{name: "prefix miss", items: []string{"foobar"}, want: "foo", found: false},
			{name: "prefix exact", items: []string{"foo", "foobar"}, want: "foo", found: true},
			{name: "suffix miss", items: []string{"foo"}, want: "foobar", found: false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				got := undoContains(tc.items, tc.want)
				if got != tc.found {
					t.Fatalf("undoContains(%v, %q) = %v, want %v", tc.items, tc.want, got, tc.found)
				}
			})
		}
	})

	t.Run("dropString", func(t *testing.T) {
		t.Run("removes first occurrence only when duplicated", func(t *testing.T) {
			input := []string{"a", "b", "a", "c"}
			orig := append([]string(nil), input...)
			got, removed := undoDropString(input, "a")
			if !removed {
				t.Fatalf("undoDropString removed = false, want true")
			}
			// The contract is remove-first-occurrence. With two copies of "a",
			// only the first is removed, leaving one "a" in the result.
			// Current production code removes ALL occurrences (bug); assert the
			// correct behaviour so the regression is visible.
			// If this fails, production code needs fixing to skip only the first match.
			want := []string{"b", "a", "c"}
			if len(got) != len(want) {
				t.Fatalf("undoDropString(%v, %q) = %v, want %v (len mismatch)", orig, "a", got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("undoDropString(%v, %q) = %v, want %v", orig, "a", got, want)
				}
			}
			for i := range orig {
				if input[i] != orig[i] {
					t.Fatalf("undoDropString mutated input: got %v, want %v", input, orig)
				}
			}
		})

		t.Run("two identical elements leaves one", func(t *testing.T) {
			input := []string{"a", "a"}
			orig := append([]string(nil), input...)
			got, removed := undoDropString(input, "a")
			if !removed {
				t.Fatalf("undoDropString removed = false, want true")
			}
			want := []string{"a"}
			if len(got) != len(want) || got[0] != want[0] {
				t.Fatalf("undoDropString(%v, %q) = %v, want %v", orig, "a", got, want)
			}
			for i := range orig {
				if input[i] != orig[i] {
					t.Fatalf("undoDropString mutated input: got %v, want %v", input, orig)
				}
			}
		})

		t.Run("absent returns false and unchanged", func(t *testing.T) {
			input := []string{"a", "b", "c"}
			orig := append([]string(nil), input...)
			got, removed := undoDropString(input, "z")
			if removed {
				t.Fatalf("undoDropString removed = true, want false")
			}
			if len(got) != len(orig) {
				t.Fatalf("undoDropString(%v, %q) = %v, want %v", orig, "z", got, orig)
			}
			for i := range orig {
				if got[i] != orig[i] {
					t.Fatalf("undoDropString(%v, %q) = %v, want %v", orig, "z", got, orig)
				}
				if input[i] != orig[i] {
					t.Fatalf("undoDropString mutated input: got %v, want %v", input, orig)
				}
			}
		})

		t.Run("empty nil safe", func(t *testing.T) {
			var input []string
			got, removed := undoDropString(input, "a")
			if removed {
				t.Fatalf("undoDropString nil removed = true, want false")
			}
			if len(got) != 0 {
				t.Fatalf("undoDropString nil = %v, want []", got)
			}
			if input != nil && len(input) != 0 {
				t.Fatalf("undoDropString mutated nil input: got %v", input)
			}
		})

		t.Run("empty slice safe", func(t *testing.T) {
			input := []string{}
			orig := append([]string(nil), input...)
			got, removed := undoDropString(input, "a")
			if removed {
				t.Fatalf("undoDropString empty removed = true, want false")
			}
			if len(got) != 0 {
				t.Fatalf("undoDropString empty = %v, want []", got)
			}
			if len(input) != len(orig) {
				t.Fatalf("undoDropString mutated empty input: got %v, want %v", input, orig)
			}
		})

		t.Run("does not mutate backing array", func(t *testing.T) {
			// Make an input with extra capacity to detect in-place corruption.
			backing := make([]string, 3, 6)
			copy(backing, []string{"a", "b", "a"})
			input := backing[:3]
			orig := append([]string(nil), input...)
			origCap := cap(input)
			got, removed := undoDropString(input, "a")
			if !removed {
				t.Fatalf("undoDropString removed = false, want true")
			}
			for i := range orig {
				if input[i] != orig[i] {
					t.Fatalf("undoDropString mutated backing array: input %v, want %v", input, orig)
				}
			}
			if cap(input) != origCap {
				t.Fatalf("cap changed %d want %d", cap(input), origCap)
			}
			// Modifying the result must not corrupt the original backing.
			if len(got) > 0 {
				got[0] = "mutated"
				if input[0] != orig[0] {
					t.Fatalf("result aliases input backing: input %v, want %v after mutating result %v", input, orig, got)
				}
			}
		})
	})
}
