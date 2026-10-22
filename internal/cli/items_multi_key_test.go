// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Multi-key `items delete` / `items restore` and `items trash empty`.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"zotio/internal/mutation"
	"zotio/internal/store"
)

type multiPlaneItem struct {
	version      int
	deleted      bool
	dateModified string
}

type libWrite struct {
	method, key, version string
	deleted              any
}

// multiPlane is a fake Zotero write plane holding several items.
type multiPlane struct {
	mu     sync.Mutex
	items  map[string]*multiPlaneItem
	writes []libWrite
	reads  int
	failOn map[string]bool // keys whose PATCH answers 412
}

func newMultiPlane(t *testing.T, items map[string]*multiPlaneItem) *multiPlane {
	t.Helper()
	p := &multiPlane{items: items, failOn: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(srv.Close)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	return p
}

func (p *multiPlane) serve(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	rest := strings.TrimPrefix(r.URL.Path, "/users/0/items/")
	if r.Method == http.MethodGet && rest == "trash" {
		p.reads++
		rows := []map[string]any{}
		if r.URL.Query().Get("start") == "0" {
			for key, it := range p.items {
				if it.deleted {
					rows = append(rows, map[string]any{"key": key, "version": it.version,
						"data": map[string]any{"key": key, "version": it.version, "deleted": 1, "dateModified": it.dateModified}})
				}
			}
		}
		_ = json.NewEncoder(w).Encode(rows)
		return
	}
	it, ok := p.items[rest]
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		p.reads++
		w.Header().Set("Last-Modified-Version", strconv.Itoa(it.version))
		deleted := 0
		if it.deleted {
			deleted = 1
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"key": rest, "version": it.version, "data": map[string]any{"key": rest, "version": it.version, "deleted": deleted}})
	case http.MethodPatch:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		p.writes = append(p.writes, libWrite{r.Method, rest, r.Header.Get("If-Unmodified-Since-Version"), body["deleted"]})
		if p.failOn[rest] {
			http.Error(w, "stale", http.StatusPreconditionFailed)
			return
		}
		it.deleted = body["deleted"] == float64(1)
		it.version++
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		p.writes = append(p.writes, libWrite{r.Method, rest, r.Header.Get("If-Unmodified-Since-Version"), nil})
		delete(p.items, rest)
		w.WriteHeader(http.StatusNoContent)
	}
}

func (p *multiPlane) writeCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.writes)
}

func (p *multiPlane) resetWrites() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.writes = nil
}

func threeLiveItems() map[string]*multiPlaneItem {
	return map[string]*multiPlaneItem{"ITEMKEY2": {version: 11}, "ITEMKEY3": {version: 12}, "ITEMKEY4": {version: 13}}
}

// runKeyed executes a command with stdin and returns the decoded envelope.
func runKeyed(t *testing.T, cmd *cobra.Command, stdin string, args ...string) (mutation.Envelope, error) {
	t.Helper()
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(args)
	err := cmd.Execute()
	var env mutation.Envelope
	if out.Len() > 0 {
		_ = json.Unmarshal(out.Bytes(), &env)
	}
	return env, err
}

func withJournal(t *testing.T) {
	t.Helper()
	mutationJournalRecorder = recordMutationJournal
	t.Cleanup(func() { mutationJournalRecorder = nil })
}

func TestItemsDeleteManyKeysIsOnePlanAndOneJournalRun(t *testing.T) {
	p := newMultiPlane(t, threeLiveItems())
	withJournal(t)

	env, err := runKeyed(t, newItemsDeleteCmd(&rootFlags{asJSON: true, dryRun: true, maxChanges: -1}), "", "ITEMKEY2", "ITEMKEY3", "ITEMKEY4")
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if len(env.Plan.Operations) != 3 {
		t.Fatalf("plan ops = %d, want 3", len(env.Plan.Operations))
	}
	for _, op := range env.Plan.Operations {
		if op.Kind != "item_trash" {
			t.Errorf("op %s kind = %q, want item_trash", op.Key, op.Kind)
		}
	}
	if p.writeCount() != 0 {
		t.Fatalf("dry-run wrote %d times", p.writeCount())
	}

	for name, args := range map[string][]string{
		"positional": {"ITEMKEY2", "ITEMKEY3", "ITEMKEY4"},
		"stdin":      {"--keys-from", "-"},
	} {
		t.Run(name, func(t *testing.T) {
			for i, key := range []string{"ITEMKEY2", "ITEMKEY3", "ITEMKEY4"} {
				p.items[key] = &multiPlaneItem{version: 11 + i}
			}
			p.resetWrites()
			env, err := runKeyed(t, newItemsDeleteCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1}), "ITEMKEY2\nITEMKEY3\nITEMKEY4\n", args...)
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			if env.Result == nil || env.Result.Summary.Applied != 3 {
				t.Fatalf("result = %+v, want 3 applied", env.Result)
			}
			if id := journalRunID(t, env); id == "" {
				t.Fatalf("journal = %v, want one run id", env.Journal)
			}
			if len(p.writes) != 3 {
				t.Fatalf("writes = %+v, want 3", p.writes)
			}
			for i, key := range []string{"ITEMKEY2", "ITEMKEY3", "ITEMKEY4"} {
				w := p.writes[i]
				if w.method != http.MethodPatch || w.key != key || w.deleted != float64(1) || w.version != strconv.Itoa(11+i) {
					t.Errorf("write %d = %+v, want version-guarded PATCH deleted=1 on %s", i, w, key)
				}
			}
		})
	}
}

func TestItemsDeleteAndRestoreKeyListContract(t *testing.T) {
	p := newMultiPlane(t, threeLiveItems())
	builders := map[string]func() *cobra.Command{
		"delete":  func() *cobra.Command { return newItemsDeleteCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1}) },
		"restore": func() *cobra.Command { return newItemsRestoreCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1}) },
	}
	for _, tt := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "no keys", args: nil, wantErr: "no item keys provided"},
		{name: "mixed", args: []string{"ITEMKEY2", "--keys-from", "-"}, wantErr: "cannot be combined"},
	} {
		for cmdName, build := range builders {
			_, err := runKeyed(t, build(), "ITEMKEY3\n", tt.args...)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("%s %s: err = %v, want it to contain %q", cmdName, tt.name, err, tt.wantErr)
			} else if ExitCode(err) != 2 {
				t.Errorf("%s %s: exit = %d, want 2 (usage)", cmdName, tt.name, ExitCode(err))
			}
		}
	}
	// A malformed key is refused by the root argument validator, naming the key.
	zaIsolate(t)
	for _, verb := range []string{"delete", "restore"} {
		_, err := zaExecuteRoot(t, "items", verb, "ITEMKEY2", "bad key!")
		if err == nil || !strings.Contains(err.Error(), "bad key!") || ExitCode(err) != 2 {
			t.Errorf("items %s with a bad key: err = %v, want a usage error naming it", verb, err)
		}
	}
	if p.writeCount() != 0 || p.reads != 0 {
		t.Errorf("refused argument lists reached the library: reads=%d writes=%d", p.reads, p.writeCount())
	}
}

func TestItemsDeleteMaxChangesCapsTheRun(t *testing.T) {
	p := newMultiPlane(t, threeLiveItems())
	_, err := runKeyed(t, newItemsDeleteCmd(&rootFlags{asJSON: true, yes: true, maxChanges: 2}), "", "ITEMKEY2", "ITEMKEY3", "ITEMKEY4")
	if err == nil {
		t.Fatal("3 keys with --max-changes 2 applied; want refusal")
	}
	if p.writeCount() != 0 {
		t.Errorf("refused run wrote %d times", p.writeCount())
	}
}

func TestItemsDeleteOneFailingKeyDoesNotHideTheRest(t *testing.T) {
	p := newMultiPlane(t, threeLiveItems())
	p.failOn["ITEMKEY3"] = true
	env, err := runKeyed(t, newItemsDeleteCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1, continueOnError: true}), "", "ITEMKEY2", "ITEMKEY3", "ITEMKEY4")
	if err == nil {
		t.Fatal("a 412 on ITEMKEY3 returned nil error")
	}
	if env.Result == nil || len(env.Result.Items) != 3 {
		t.Fatalf("result = %+v, want all three keys reported", env.Result)
	}
	got := map[string]string{}
	for _, it := range env.Result.Items {
		got[it.OpID] = it.Status
	}
	want := map[string]string{"items.delete:ITEMKEY2": "applied", "items.delete:ITEMKEY3": "conflict", "items.delete:ITEMKEY4": "applied"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s status = %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
}

func TestItemsRestoreKeysFromReversesAndBulkUndoRestoresAll(t *testing.T) {
	p := newMultiPlane(t, threeLiveItems())
	withJournal(t)

	tenv, err := runKeyed(t, newItemsDeleteCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1}), "", "ITEMKEY2", "ITEMKEY3", "ITEMKEY4")
	if err != nil {
		t.Fatalf("trash: %v", err)
	}
	runID := journalRunID(t, tenv)
	if runID == "" {
		t.Fatalf("no run id in %+v", tenv.Journal)
	}

	// Undo of the one bulk run restores every key.
	undo := mustUndoJournalRun(t, runID)
	if undo.Result == nil || undo.Result.Summary.Applied != 3 {
		t.Fatalf("undo = %+v, want 3 applied", undo.Result)
	}
	for key, it := range p.items {
		if it.deleted {
			t.Errorf("%s still trashed after undo", key)
		}
	}

	// Trash again, then restore with --keys-from.
	for _, it := range p.items {
		it.deleted = true
	}
	p.resetWrites()
	renv, err := runKeyed(t, newItemsRestoreCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1}), `["ITEMKEY2","ITEMKEY3","ITEMKEY4"]`, "--keys-from", "-")
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if renv.Result == nil || renv.Result.Summary.Applied != 3 || len(p.writes) != 3 {
		t.Fatalf("restore result = %+v writes = %+v, want 3 PATCHes", renv.Result, p.writes)
	}
	for _, w := range p.writes {
		if w.method != http.MethodPatch || w.deleted != float64(0) {
			t.Errorf("write = %+v, want PATCH deleted=0", w)
		}
	}
}

func trashEmptyFixture() map[string]*multiPlaneItem {
	old := time.Now().AddDate(0, 0, -90).UTC().Format(time.RFC3339)
	recent := time.Now().AddDate(0, 0, -2).UTC().Format(time.RFC3339)
	return map[string]*multiPlaneItem{
		"DEADAAA2": {version: 21, deleted: true, dateModified: old},
		"DEADAAA3": {version: 22, deleted: true, dateModified: old},
		"FRESHAA2": {version: 23, deleted: true, dateModified: recent},
		"LIVEAAA2": {version: 24},
	}
}

func TestItemsTrashEmptyListsOnlyAgedTrashAndNeedsAllowDestructive(t *testing.T) {
	p := newMultiPlane(t, trashEmptyFixture())

	env, err := runKeyed(t, newItemsTrashEmptyCmd(&rootFlags{asJSON: true, dryRun: true, allowDestructive: true, maxChanges: -1}), "", "--older-than", "30d")
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	planned := map[string]bool{}
	for _, op := range env.Plan.Operations {
		planned[op.Key] = true
		if op.Kind != "item_delete" || !op.Destructive {
			t.Errorf("op %s = kind %q destructive %v, want destructive item_delete", op.Key, op.Kind, op.Destructive)
		}
	}
	if len(planned) != 2 || !planned["DEADAAA2"] || !planned["DEADAAA3"] {
		t.Fatalf("planned keys = %v, want only DEADAAA2 and DEADAAA3", planned)
	}
	if p.writeCount() != 0 {
		t.Fatalf("dry-run wrote %d times", p.writeCount())
	}

	// Without --allow-destructive both the preview and the apply are refused
	// (the engine gate every destructive op shares) before any write.
	if _, err := runKeyed(t, newItemsTrashEmptyCmd(&rootFlags{asJSON: true, dryRun: true, maxChanges: -1}), "", "--older-than", "30d"); err == nil {
		t.Fatal("preview without --allow-destructive succeeded")
	}
	if _, err := runKeyed(t, newItemsTrashEmptyCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1}), "", "--older-than", "30d"); err == nil {
		t.Fatal("apply without --allow-destructive succeeded")
	}
	if p.writeCount() != 0 {
		t.Fatalf("refused apply wrote %d times", p.writeCount())
	}
}

func TestItemsTrashEmptyAppliesVersionGuardedDeletesWithTombstones(t *testing.T) {
	p := newMultiPlane(t, trashEmptyFixture())
	withJournal(t)
	mirrorWriteThrough = applyMirrorWriteThrough
	t.Cleanup(func() { mirrorWriteThrough = nil })

	db, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
	if err != nil {
		t.Fatal(err)
	}
	var rows []json.RawMessage
	for _, key := range []string{"DEADAAA2", "DEADAAA3", "FRESHAA2"} {
		rows = append(rows, json.RawMessage(`{"key":"`+key+`","version":20,"data":{"key":"`+key+`","itemType":"book","title":"T"}}`))
	}
	if _, _, err := db.UpsertBatch("items", rows); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	env, err := runKeyed(t, newItemsTrashEmptyCmd(&rootFlags{asJSON: true, yes: true, allowDestructive: true, maxChanges: -1}), "", "--older-than", "30d")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if env.Result == nil || env.Result.Summary.Applied != 2 {
		t.Fatalf("result = %+v, want 2 applied", env.Result)
	}
	versions := map[string]string{}
	for _, w := range p.writes {
		if w.method != http.MethodDelete {
			t.Errorf("write = %+v, want DELETE", w)
		}
		versions[w.key] = w.version
	}
	if len(versions) != 2 || versions["DEADAAA2"] != "21" || versions["DEADAAA3"] != "22" {
		t.Fatalf("deletes = %v, want DEADAAA2@21 and DEADAAA3@22 only", versions)
	}
	if _, kept := p.items["FRESHAA2"]; !kept {
		t.Error("recent trash entry was deleted")
	}

	d, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, key := range []string{"DEADAAA2", "DEADAAA3"} {
		marked, err := d.PendingDeletion("items", key)
		if err != nil || !marked {
			t.Errorf("tombstone for %s = %v (err %v), want written (ADR-0007)", key, marked, err)
		}
	}
	if marked, _ := d.PendingDeletion("items", "FRESHAA2"); marked {
		t.Error("FRESHAA2 has a tombstone but was not deleted")
	}
}

func TestItemsTrashEmptyRejectsBadAgeAndKeysOutsideTheTrash(t *testing.T) {
	p := newMultiPlane(t, trashEmptyFixture())
	flags := func() *rootFlags {
		return &rootFlags{asJSON: true, dryRun: true, allowDestructive: true, maxChanges: -1}
	}
	if _, err := runKeyed(t, newItemsTrashEmptyCmd(flags()), "", "--older-than", "soon"); err == nil || ExitCode(err) != 2 {
		t.Errorf("bad --older-than err = %v, want usage error", err)
	}
	_, err := runKeyed(t, newItemsTrashEmptyCmd(flags()), "DEADAAA2\nLIVEAAA2\n", "--keys-from", "-")
	if err == nil || !strings.Contains(err.Error(), "LIVEAAA2") || ExitCode(err) != 2 {
		t.Errorf("key outside the trash err = %v, want usage error naming LIVE", err)
	}
	if p.writeCount() != 0 {
		t.Errorf("wrote %d times", p.writeCount())
	}
}
