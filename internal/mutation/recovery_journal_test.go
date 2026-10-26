// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package mutation

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRunUnknownApplyStatusFailsClosed(t *testing.T) {
	secondCalled := false
	ops := []Op{
		{ID: "first", Key: "ABCD2345", Changes: []Change{{Field: "tags", Add: "x"}}, Apply: func() (string, any, error) {
			return "bogus", nil, nil
		}},
		{ID: "second", Key: "ABCD2346", Changes: []Change{{Field: "tags", Add: "x"}}, Apply: func() (string, any, error) {
			secondCalled = true
			return "applied", nil, nil
		}},
	}
	env, err := RunObserved(Options{Yes: true, MaxChanges: -1}, "items.tags.add", ops, nil)
	if !errors.Is(err, ErrIncomplete) || env.OK || env.Result == nil {
		t.Fatalf("run = %+v, error = %v; want incomplete failure", env, err)
	}
	if env.Result.Summary.Failed != 1 || env.Result.Summary.NotAttempted != 1 || secondCalled {
		t.Fatalf("summary = %+v, second called = %v", env.Result.Summary, secondCalled)
	}
	item := env.Result.Items[0]
	reason, _ := item.Reason.(string)
	if item.Status != "failed" || !strings.Contains(reason, `unexpected status "bogus"`) {
		t.Fatalf("first result = %+v", item)
	}
}

func TestJournalUnknownWriteSurvivesCheckpointAndRefusesUndo(t *testing.T) {
	op := Op{ID: "first", Key: "ABCD2345", Kind: "tag_add", Changes: []Change{{Field: "tags", Add: "x"}}, Undoes: "original"}
	item := ResultItem{OpID: op.ID, Key: op.Key, Status: "failed", Reason: map[string]any{
		"committed": true, "outcome": "unknown", "message": "connection lost",
		"requested_fields": map[string]any{"tags": []map[string]any{{"tag": "x"}}},
	}}
	checkpoint, ok := BuildJournalCheckpoint("undo-run", "journal.undo", "apply", op, item, time.Now())
	if !ok || checkpoint.Undoes != "original" {
		t.Fatalf("checkpoint = %+v, ok=%v", checkpoint, ok)
	}
	dir := t.TempDir()
	if err := WriteEntry(dir, checkpoint); err != nil {
		t.Fatal(err)
	}
	entry, err := ReadEntry(dir, "undo-run")
	if err != nil || !entry.Partial || entry.Undoes != "original" || entry.Summary.Failed != 1 || entry.Summary.Attempted != 1 {
		t.Fatalf("entry = %+v, err=%v", entry, err)
	}
	inverse, refused := InverseOps(entry)
	if len(inverse) != 0 || len(refused) != 1 || refused[0].Key != op.Key || !strings.Contains(refused[0].Reason, "outcome is unknown") {
		t.Fatalf("inverse = %+v, refused = %+v", inverse, refused)
	}
	env := Envelope{RunID: "undo-run", Undoes: "original", Plan: Plan{Operations: []Op{op}}, Result: &Result{Items: []ResultItem{item}}}
	final, ok := BuildJournalEntry(env, time.Now())
	if !ok || final.Undoes != "original" || final.Ops[0].Reason == nil {
		t.Fatalf("final = %+v, ok=%v", final, ok)
	}
}

func TestRunObservedIncludesAttemptedNoOp(t *testing.T) {
	ops := []Op{
		{ID: "planned-no-op"},
		{ID: "attempted-no-op", Changes: []Change{{Field: "tags", Add: "x"}}, Apply: func() (string, any, error) {
			return "no_op", nil, nil
		}},
	}
	var observed []string
	_, err := RunObserved(Options{Yes: true, MaxChanges: -1}, "items.tags.add", ops, func(op Op, item ResultItem) {
		observed = append(observed, op.ID+":"+item.Status)
	})
	if err != nil || len(observed) != 1 || observed[0] != "attempted-no-op:no_op" {
		t.Fatalf("observed=%v, err=%v", observed, err)
	}
}
