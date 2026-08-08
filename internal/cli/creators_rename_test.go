// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"zotio/internal/mutation"
)

func runCreatorsRenameTestCmd(t *testing.T, flags *rootFlags, baseURL string, args ...string) (mutation.Envelope, error) {
	t.Helper()
	t.Setenv("ZOTERO_BASE_URL", baseURL+"/users/0")
	cmd := newCreatorsRenameCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	var env mutation.Envelope
	if out.Len() > 0 {
		if decodeErr := json.Unmarshal(out.Bytes(), &env); decodeErr != nil {
			t.Fatalf("decode envelope %q: %v", out.String(), decodeErr)
		}
	}
	return env, err
}

// TestCreatorsRenamePreviewBuildsOneOpPerAffectedItem proves discovery finds
// every item carrying the exact display name --from names (in both creator
// shapes) and only those items, without touching the network.
func TestCreatorsRenamePreviewBuildsOneOpPerAffectedItem(t *testing.T) {
	seedCreatorAuditFixStore(t, []json.RawMessage{
		json.RawMessage(`{"key":"K1","version":3,"data":{"key":"K1","version":3,"itemType":"journalArticle","title":"Split name","creators":[{"creatorType":"author","firstName":"Adam J","lastName":"Rock"}]}}`),
		json.RawMessage(`{"key":"K2","version":4,"data":{"key":"K2","version":4,"itemType":"journalArticle","title":"Name-only","creators":[{"creatorType":"author","name":"Adam J Rock"}]}}`),
		json.RawMessage(`{"key":"K3","version":5,"data":{"key":"K3","version":5,"itemType":"journalArticle","title":"Unrelated author","creators":[{"creatorType":"author","firstName":"Someone","lastName":"Else"}]}}`),
	})
	srv := newCreatorAuditFixServer(t, nil)

	env, err := runCreatorsRenameTestCmd(t, &rootFlags{asJSON: true, maxChanges: -1}, srv.server.URL, "--from", "Adam J Rock", "--to", "Adam J. Rock")
	if err != nil {
		t.Fatalf("creators rename preview: %v", err)
	}
	if srv.requests != 0 {
		t.Fatalf("preview made %d Zotero request(s), want 0", srv.requests)
	}
	if !env.OK || env.Mode != "preview" || env.Result != nil {
		t.Fatalf("preview envelope = %+v, want ok preview without result", env)
	}
	if got := plannedCreatorKeys(env); !reflect.DeepEqual(got, []string{"K1", "K2"}) {
		t.Fatalf("planned keys = %v, want [K1 K2] (K3 does not carry the alias)", got)
	}
	for _, op := range env.Plan.Operations {
		if op.Kind != "creator_rename" || op.Destructive {
			t.Fatalf("op %+v, want non-destructive creator_rename", op)
		}
		assertCreatorChange(t, op.Changes, "Adam J Rock", "Adam J. Rock")
	}
}

// TestCreatorsRenameApplyUsesWritePlanePreconditionAndPreservesShape proves
// apply resolves the If-Unmodified-Since-Version precondition from the write
// plane (not the local mirror's own, different version), and that the PATCHed
// creators array keeps order, creatorType, and each creator's original shape
// (split name vs. single name field) intact aside from the renamed one.
func TestCreatorsRenameApplyUsesWritePlanePreconditionAndPreservesShape(t *testing.T) {
	seedCreatorAuditFixStore(t, []json.RawMessage{
		json.RawMessage(`{"key":"K1","version":5,"data":{"key":"K1","version":5,"itemType":"journalArticle","title":"Two creators","creators":[{"creatorType":"editor","firstName":"Alice","lastName":"Ng"},{"creatorType":"author","firstName":"Adam J","lastName":"Rock"}]}}`),
		json.RawMessage(`{"key":"K2","version":7,"data":{"key":"K2","version":7,"itemType":"journalArticle","title":"Name-only","creators":[{"creatorType":"author","name":"Adam J Rock"}]}}`),
	})
	srv := newCreatorAuditFixServer(t, []json.RawMessage{
		// Deliberately different versions than the local mirror: apply must
		// use these, not the mirror's 5/7.
		json.RawMessage(`{"key":"K1","version":105,"data":{"key":"K1","creators":[{"creatorType":"editor","firstName":"Alice","lastName":"Ng"},{"creatorType":"author","firstName":"Adam J","lastName":"Rock"}]}}`),
		json.RawMessage(`{"key":"K2","version":107,"data":{"key":"K2","creators":[{"creatorType":"author","name":"Adam J Rock"}]}}`),
	})
	oldMirror := mirrorWriteThrough
	mirrorWriteThrough = applyMirrorWriteThrough
	t.Cleanup(func() { mirrorWriteThrough = oldMirror })

	env, err := runCreatorsRenameTestCmd(t, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, srv.server.URL, "--from", "Adam J Rock", "--to", "Adam J. Rock")
	if err != nil {
		t.Fatalf("creators rename apply: %v", err)
	}
	if !env.OK || env.Mode != "apply" || env.Result == nil || env.Result.Summary.Applied != 2 {
		t.Fatalf("apply envelope = %+v, want two applied writes", env)
	}

	assertCreatorPatch(t, srv.patches["K1"], "105", []map[string]string{
		{"creatorType": "editor", "firstName": "Alice", "lastName": "Ng"},
		{"creatorType": "author", "firstName": "Adam J.", "lastName": "Rock"},
	})
	assertCreatorPatch(t, srv.patches["K2"], "107", []map[string]string{
		{"creatorType": "author", "name": "Adam J. Rock"},
	})
	assertStoredCreators(t, "K1", []map[string]string{
		{"creatorType": "editor", "firstName": "Alice", "lastName": "Ng"},
		{"creatorType": "author", "firstName": "Adam J.", "lastName": "Rock"},
	})
	assertStoredCreators(t, "K2", []map[string]string{
		{"creatorType": "author", "name": "Adam J. Rock"},
	})
}

// TestCreatorsRenameNoOpWhenWritePlaneNoLongerCarriesName proves that when the
// write plane has drifted since the plan was built (the alias is no longer
// there), apply reports a structured no_op instead of silently doing nothing
// or corrupting the item, and never sends a PATCH.
func TestCreatorsRenameNoOpWhenWritePlaneNoLongerCarriesName(t *testing.T) {
	seedCreatorAuditFixStore(t, []json.RawMessage{
		json.RawMessage(`{"key":"K1","version":3,"data":{"key":"K1","version":3,"itemType":"journalArticle","title":"Drifted","creators":[{"creatorType":"author","firstName":"Adam J","lastName":"Rock"}]}}`),
	})
	srv := newCreatorAuditFixServer(t, []json.RawMessage{
		// Someone already renamed this on the write plane by the time apply runs.
		json.RawMessage(`{"key":"K1","version":203,"data":{"key":"K1","creators":[{"creatorType":"author","firstName":"Adam J.","lastName":"Rock"}]}}`),
	})

	env, err := runCreatorsRenameTestCmd(t, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, srv.server.URL, "--from", "Adam J Rock", "--to", "Adam J. Rock")
	if err != nil {
		t.Fatalf("creators rename apply: %v", err)
	}
	if !env.OK || env.Result == nil || env.Result.Summary.NoOp != 1 || env.Result.Summary.Applied != 0 {
		t.Fatalf("apply envelope = %+v, want a single no_op and no applies", env)
	}
	if len(srv.patches) != 0 {
		t.Fatalf("patches = %#v, want none sent for a no_op", srv.patches)
	}
	reason, ok := env.Result.Items[0].Reason.(map[string]any)
	if !ok {
		t.Fatalf("reason = %#v, want structured object", env.Result.Items[0].Reason)
	}
	if reason["code"] != "creator_absent" {
		t.Fatalf("reason code = %v, want creator_absent", reason["code"])
	}
	if msg, _ := reason["message"].(string); msg == "" {
		t.Fatalf("reason message empty: %#v", reason)
	}
}

// TestCreatorsRenameRefusesWriteWithoutWritePlaneVersion proves apply never
// PATCHes when the write plane carries no resolvable version: sending a
// creators PATCH with no If-Unmodified-Since-Version silently defeats the
// precondition Zotero relies on to detect concurrent writes.
func TestCreatorsRenameRefusesWriteWithoutWritePlaneVersion(t *testing.T) {
	seedCreatorAuditFixStore(t, []json.RawMessage{
		json.RawMessage(`{"key":"K1","version":3,"data":{"key":"K1","version":3,"itemType":"journalArticle","title":"Never pushed","creators":[{"creatorType":"author","firstName":"Adam J","lastName":"Rock"}]}}`),
	})
	srv := newCreatorAuditFixServer(t, []json.RawMessage{
		// No "version" property and no Last-Modified-Version header (the
		// server never sets one): objectVersion resolves to 0.
		json.RawMessage(`{"key":"K1","data":{"key":"K1","creators":[{"creatorType":"author","firstName":"Adam J","lastName":"Rock"}]}}`),
	})

	env, err := runCreatorsRenameTestCmd(t, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, srv.server.URL, "--from", "Adam J Rock", "--to", "Adam J. Rock")
	if err == nil {
		t.Fatal("creators rename apply succeeded, want an error for the unresolvable precondition")
	}
	if env.OK || env.Result == nil || env.Result.Summary.Failed != 1 {
		t.Fatalf("apply envelope = %+v, want one failed item", env)
	}
	if len(srv.patches) != 0 {
		t.Fatalf("patches = %#v, want none sent without a precondition", srv.patches)
	}
}
