// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"zotio/internal/mutation"
	"zotio/internal/store"
)

// dupExecResolve runs `items duplicates <args>` without the root preflight and
// returns the decoded envelope, the raw stdout, and the command's own error, so
// a test can assert the exit a caller sees as well as what was printed.
func dupExecResolve(t *testing.T, srv *duplicateResolveTestServer, flags *rootFlags, args ...string) (mutation.Envelope, string, error) {
	t.Helper()
	if srv != nil {
		t.Setenv("ZOTERO_BASE_URL", srv.server.URL+"/users/0")
	}
	cmd := newItemsDuplicatesCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()
	var env mutation.Envelope
	if out.Len() > 0 {
		if decodeErr := json.Unmarshal(out.Bytes(), &env); decodeErr != nil {
			t.Fatalf("decode output %q: %v (execute err %v; stderr=%s)", out.String(), decodeErr, err, errOut.String())
		}
	}
	return env, out.String(), err
}

// dupTrashInMirror records key as trashed the way `items delete` does: a trash
// row beside the live row, which stays until the next sync reconciles them.
func dupTrashInMirror(t *testing.T, key string) {
	t.Helper()
	db, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	mirrorTrashedItem(db, localQueryStore{Store: db}, key)
	if _, err := db.GetContext(context.Background(), "items-trash", key); err != nil {
		t.Fatalf("trash row for %s: %v", key, err)
	}
	if _, err := db.GetContext(context.Background(), "items", key); err != nil {
		t.Fatalf("live row for %s must stay until sync: %v", key, err)
	}
}

// A trashed copy can be the richest record in its group. It must never be the
// merge target, and a group that is one live record once the trash is set
// aside is not a merge: merging onto the trashed copy and then trashing the
// live one leaves the library with no live copy of the item.
func TestDupResolvePlanSetsTrashedCopiesAside(t *testing.T) {
	const (
		lean    = `{"key":"A","version":1,"data":{"key":"A","itemType":"journalArticle","title":"Same","DOI":"10/x"}}`
		rich    = `{"key":"B","version":2,"data":{"key":"B","itemType":"journalArticle","title":"Same","DOI":"10/x","abstractNote":"a","date":"2020","publicationTitle":"J","volume":"1"}}`
		richDel = `{"key":"B","version":2,"data":{"key":"B","itemType":"journalArticle","title":"Same","DOI":"10/x","abstractNote":"a","date":"2020","publicationTitle":"J","volume":"1","deleted":1}}`
		medium  = `{"key":"C","version":3,"data":{"key":"C","itemType":"journalArticle","title":"Same","DOI":"10/x","abstractNote":"a"}}`
	)
	for _, tc := range []struct {
		name      string
		items     []string
		trashKeys []string
		wantOps   []string
	}{
		{name: "trashed through zotio leaves one live copy", items: []string{lean, rich}, trashKeys: []string{"B"}},
		{name: "payload marked deleted leaves one live copy", items: []string{lean, richDel}},
		{
			name:      "live pair merges without the trashed copy",
			items:     []string{lean, rich, medium},
			trashKeys: []string{"B"},
			wantOps:   []string{"items.duplicates.resolve:C:A"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raws := make([]json.RawMessage, 0, len(tc.items))
			for _, item := range tc.items {
				raws = append(raws, json.RawMessage(item))
			}
			seedDuplicateResolveStore(t, raws)
			for _, key := range tc.trashKeys {
				dupTrashInMirror(t, key)
			}

			env, _, err := dupExecResolve(t, nil, &rootFlags{asJSON: true, maxChanges: -1}, "resolve", "--doi")
			if err != nil {
				t.Fatalf("preview: %v", err)
			}
			got := make([]string, 0, len(env.Plan.Operations))
			for _, op := range env.Plan.Operations {
				got = append(got, op.ID)
			}
			if !slices.Equal(got, tc.wantOps) {
				t.Fatalf("planned ops = %v, want %v", got, tc.wantOps)
			}
		})
	}
}

// The mirror can lag the trash: an item trashed in Zotero after the last sync
// still reads as live there and can win the merge-target ranking. The write
// plane decides at apply time, and a trashed target stops the merge before
// anything is written, so the live copy is never trashed.
func TestDupResolveApplyRefusesTrashedMergeTarget(t *testing.T) {
	seedDuplicateResolveStore(t, []json.RawMessage{
		json.RawMessage(`{"key":"A","version":10,"data":{"key":"A","itemType":"journalArticle","title":"Same","DOI":"10/x","collections":["C1"]}}`),
		json.RawMessage(`{"key":"B","version":11,"data":{"key":"B","itemType":"journalArticle","title":"Same","DOI":"10/x","abstractNote":"a","collections":["C2"]}}`),
	})
	srv := newDuplicateResolveTestServer(t, map[string]int{"A": 10, "B": 12}, map[string]map[string]any{
		"A": {"key": "A", "itemType": "journalArticle", "title": "Same", "DOI": "10/x", "collections": []any{"C1"}},
		"B": {"key": "B", "itemType": "journalArticle", "title": "Same", "DOI": "10/x", "abstractNote": "a", "collections": []any{"C2"}, "deleted": 1},
	})

	env, _, err := dupExecResolve(t, srv, &rootFlags{asJSON: true, yes: true, maxChanges: -1, allowDestructive: true}, "resolve", "--doi")
	if srv.patchCounts["A"] != 0 || srv.patchCounts["B"] != 0 {
		t.Fatalf("PATCH counts = A %d B %d, want none: the live copy A must not be trashed into a trashed target", srv.patchCounts["A"], srv.patchCounts["B"])
	}
	if err == nil || ExitCode(err) == 0 {
		t.Fatalf("err = %v, want a non-zero exit for a merge that was refused", err)
	}
	if env.OK || env.Result == nil || len(env.Result.Items) != 1 || env.Result.Items[0].Status != "conflict" {
		t.Fatalf("env = %+v, want one conflict and ok=false", env)
	}
	if reason := fmt.Sprint(env.Result.Items[0].Reason); !strings.Contains(reason, "B") || !strings.Contains(reason, "trash") {
		t.Fatalf("reason = %q, want it to name the trashed target B", reason)
	}
}

// A library that was never synced has no mirror to plan from. An empty,
// successful plan would tell a script that deduplication finished when nothing
// was inspected, so the run refuses with the synced_store precondition, in
// preview as well as apply.
func TestDupResolveRefusesWithoutSyncedMirror(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "preview", args: []string{"--json", "items", "duplicates", "resolve"}},
		{name: "apply", args: []string{"--json", "--yes", "--allow-destructive", "items", "duplicates", "resolve"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, _, out, _ := newPreflightTestRoot(t)
			root.SetArgs(tc.args)
			err := root.Execute()
			assertPreconditionExitCode(t, err, 9)
			dupAssertSyncedStoreRefusal(t, out.String())
		})
	}
	t.Run("command without preflight", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
		_, out, err := dupExecResolve(t, nil, &rootFlags{asJSON: true, maxChanges: -1}, "resolve")
		assertPreconditionExitCode(t, err, 9)
		dupAssertSyncedStoreRefusal(t, out)
	})
}

func dupAssertSyncedStoreRefusal(t *testing.T, out string) {
	t.Helper()
	var env preconditionUnmetEnvelope
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("output is not a precondition_unmet envelope; decode %q: %v", out, err)
	}
	if env.Kind != "precondition_unmet" || env.Precondition != preconditionSyncedStore || env.Capability != "items duplicates resolve" {
		t.Fatalf("envelope = %+v, want precondition_unmet/%s for items duplicates resolve", env, preconditionSyncedStore)
	}
}
