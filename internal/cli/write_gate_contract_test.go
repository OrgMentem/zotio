// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Regression coverage for the write-safety claims shipped in 5702f40/4b1b84a:
// every CRUD apply must be journaled, a no-op must never be, and journal undo
// must refuse the exact op/field shapes those CRUD commands actually emit.

package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spf13/cobra"

	"zotio/internal/mutation"
)

// TestCrudAppliesAreJournaled proves the headline claim that all CRUD applies
// are journaled: items update, items delete, and collections update each
// route their write through runMutation, so a successful --yes run is
// recorded. If any of these commands were changed to call the write client
// directly (bypassing runMutation), this fails with zero journal entries.
func TestCrudAppliesAreJournaled(t *testing.T) {
	cases := []struct {
		name      string
		operation string
		build     func(flags *rootFlags) *cobra.Command
		args      []string
		method    string
	}{
		{name: "items update", operation: "items.update", build: newItemsUpdateCmd, args: []string{"K1", "--title", "Updated"}, method: http.MethodPatch},
		{name: "items delete", operation: "items.delete", build: newItemsDeleteCmd, args: []string{"K2"}, method: http.MethodDelete},
		{name: "collections update", operation: "collections.update", build: newCollectionsUpdateCmd, args: []string{"K3", "--name", "Updated"}, method: http.MethodPut},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			mutationJournalRecorder = recordMutationJournal
			t.Cleanup(func() { mutationJournalRecorder = nil })

			// The delete/update paths read the item's current version before
			// the mutating request (Zotero requires If-Unmodified-Since-Version);
			// answer that GET, then accept the one mutating verb under test.
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					w.Header().Set("Last-Modified-Version", "7")
					_, _ = w.Write([]byte(`{}`))
				case tc.method:
					w.WriteHeader(http.StatusNoContent)
				default:
					http.Error(w, "unexpected method "+r.Method, http.StatusMethodNotAllowed)
				}
			}))
			t.Cleanup(srv.Close)
			t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

			flags := &rootFlags{asJSON: true, yes: true, maxChanges: -1}
			cmd := tc.build(flags)
			cmd.SilenceErrors, cmd.SilenceUsage = true, true
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs(tc.args)
			if err := cmd.Execute(); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}

			entries, err := mutation.ListEntries(helpersTestJournalDir(t))
			if err != nil {
				t.Fatalf("list journal entries: %v", err)
			}
			if len(entries) != 1 {
				t.Fatalf("%s: journal entries = %d, want 1", tc.name, len(entries))
			}
			if entries[0].Operation != tc.operation {
				t.Fatalf("%s: entry operation = %q, want %q", tc.name, entries[0].Operation, tc.operation)
			}
			if entries[0].Summary.Applied != 1 {
				t.Fatalf("%s: entry summary.applied = %d, want 1", tc.name, entries[0].Summary.Applied)
			}
		})
	}
}

// TestItemsDeleteNoOpIsNotJournaled proves recordMutationJournal's applied-only
// guard: when the pre-write version GET 404s, items delete --yes reports a
// writeNoop result with Summary.Applied == 0, and must write no journal entry
// at all -- nothing changed, so there is nothing to record or undo.
func TestItemsDeleteNoOpIsNotJournaled(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mutationJournalRecorder = recordMutationJournal
	t.Cleanup(func() { mutationJournalRecorder = nil })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "unexpected method "+r.Method, http.StatusMethodNotAllowed)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	flags := &rootFlags{asJSON: true, yes: true, maxChanges: -1}
	cmd := newItemsDeleteCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"GONE"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("items delete: %v", err)
	}

	entries, err := mutation.ListEntries(helpersTestJournalDir(t))
	if err != nil {
		t.Fatalf("list journal entries: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("journal entries = %d, want 0 for a no-op delete", len(entries))
	}
}

// TestJournalUndoRefusesCrudOps is the safety net behind "journaled but not
// undoable": every Kind/Change shape the shipped CRUD commands actually
// record -- item_update/title, item_delete/deleted, item_create/item, and
// collection_delete/collection -- must land in InverseOps' refused list with
// no inverse op produced. reversibleFields only recognizes "tags"/"collections"
// membership toggles; if any of these fields were ever folded into that set,
// journal undo would silently fabricate an inverse for an op it cannot safely
// reverse (a title overwrite, a trash, a whole-item create, a whole-collection
// delete).
func TestJournalUndoRefusesCrudOps(t *testing.T) {
	cases := []struct {
		kind   string
		change mutation.Change
	}{
		{kind: "item_update", change: mutation.Change{Field: "title", Add: "x"}},
		{kind: "item_delete", change: mutation.Change{Field: "deleted", Add: true}},
		{kind: "item_create", change: mutation.Change{Field: "item", Add: map[string]any{"title": "x"}}},
		{kind: "collection_delete", change: mutation.Change{Field: "collection", Add: true}},
	}

	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			entry := mutation.JournalEntry{
				Ops: []mutation.JournalOp{{
					ID:      "op",
					Key:     "K",
					Kind:    tc.kind,
					Status:  "applied",
					Changes: []mutation.Change{tc.change},
				}},
			}
			inverse, refused := mutation.InverseOps(entry)
			if len(inverse) != 0 {
				t.Fatalf("inverse = %+v, want none for a non-reversible %s change", inverse, tc.kind)
			}
			if len(refused) != 1 {
				t.Fatalf("refused = %+v, want exactly one refusal for %s", refused, tc.kind)
			}
			if refused[0].Kind != tc.kind {
				t.Errorf("refused kind = %q, want %q", refused[0].Kind, tc.kind)
			}
		})
	}
}

// TestItemsUpdateChangesUseReplaceSafeFieldNames pins the naming contract
// documented on itemsUpdateChanges: tags/collections are whole-list REPLACEs,
// so they must be recorded as "tags_set"/"collections_set", never the bare
// "tags"/"collections" names reversibleFields treats as reversible per-item
// membership toggles. Emitting "tags" would let journal undo invert a
// full-list REPLACE into a bogus per-tag removal -- undo has no way to know
// the whole list was replaced rather than one tag added.
func TestItemsUpdateChangesUseReplaceSafeFieldNames(t *testing.T) {
	body := map[string]any{
		"title":       "New Title",
		"tags":        []map[string]any{{"tag": "ml"}},
		"collections": []string{"COL1"},
		"version":     3,
	}
	changes := itemsUpdateChanges(body)

	byField := map[string]mutation.Change{}
	for _, c := range changes {
		byField[c.Field] = c
	}

	if _, ok := byField["tags"]; ok {
		t.Error(`itemsUpdateChanges emitted Field:"tags"; journal undo would treat a whole-list replace as a reversible per-tag toggle`)
	}
	if _, ok := byField["collections"]; ok {
		t.Error(`itemsUpdateChanges emitted Field:"collections"; journal undo would treat a whole-list replace as a reversible per-collection toggle`)
	}
	if _, ok := byField["title"]; !ok {
		t.Error("itemsUpdateChanges dropped the title change")
	}
	if _, ok := byField["tags_set"]; !ok {
		t.Error(`itemsUpdateChanges did not emit Field:"tags_set" for a tags update`)
	}
	if _, ok := byField["collections_set"]; !ok {
		t.Error(`itemsUpdateChanges did not emit Field:"collections_set" for a collections update`)
	}
	if _, ok := byField["version"]; ok {
		t.Error("itemsUpdateChanges emitted a change for the version precondition, which is not a user-visible edit")
	}
	if len(changes) != 3 {
		t.Fatalf("changes = %+v, want 3 (title, tags_set, collections_set; version excluded)", changes)
	}

	// The safety property the naming buys: every Change itemsUpdateChanges can
	// produce must be refused by InvertChange, not silently inverted.
	for _, c := range changes {
		if _, ok := mutation.InvertChange(c); ok {
			t.Errorf("InvertChange(%+v) = ok, want refused (itemsUpdateChanges output must never be treated as reversible)", c)
		}
	}
}
