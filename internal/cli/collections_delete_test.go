// Copyright 2026 OrgMentem. Licensed under MIT.

package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"

	"zotio/internal/mutation"
)

// An applied `collections delete` renders the standard mutation envelope, as
// `items delete` does, instead of the empty 204 body of the DELETE.
func TestCollectionsDeleteAppliedRendersMutationEnvelope(t *testing.T) {
	srv, _ := deleteVersionServer(t, "7")
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	cmd := newCollectionsDeleteCmd(&rootFlags{yes: true, allowDestructive: true, maxChanges: -1})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"CLCTAB23"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("collections delete: %v", err)
	}
	if out.Len() == 0 {
		t.Fatal("applied collections delete printed nothing")
	}

	var env mutation.Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode output %q: %v", out.String(), err)
	}
	if env.Operation != "collections.delete" || !env.OK {
		t.Fatalf("envelope operation = %q, ok = %v, want collections.delete and ok", env.Operation, env.OK)
	}
	if env.Result == nil || len(env.Result.Items) != 1 {
		t.Fatalf("envelope result = %+v, want one item", env.Result)
	}
	item := env.Result.Items[0]
	if item.Key != "CLCTAB23" || item.Status != "applied" {
		t.Errorf("result item = key %q status %q, want CLCTAB23 applied", item.Key, item.Status)
	}
}
