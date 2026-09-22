// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"zotio/internal/mutation"
)

func runReadingListStateTestCmd(t *testing.T, srv *itemTagTestServer, flags *rootFlags, args ...string) mutation.Envelope {
	t.Helper()
	t.Setenv("ZOTERO_BASE_URL", srv.server.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	t.Setenv("ZOTERO_QUEUE_TAG", "to-read")
	cmd := newReadingListCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	var errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("reading-list %v: %v; stderr=%s", args, err, errOut.String())
	}
	var env mutation.Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode mutation envelope %q: %v", out.String(), err)
	}
	return env
}

// readingListTransitionCases is the reading-list state machine itself, shared
// by the applied path and the dry-run path below. seedTags are the item's tags
// before the applied transition, wantAdd/wantRemove the planned tag delta in
// plan order, and preserved the seed tags the transition must leave alone.
var readingListTransitionCases = []struct {
	name       string
	args       []string
	seedTags   []map[string]any
	operation  string
	kind       string
	wantAdd    []string
	wantRemove []string
	preserved  []string
}{
	{
		name:      "add",
		args:      []string{"add", "K1"},
		seedTags:  []map[string]any{{"tag": "existing", "type": float64(0)}},
		operation: "reading-list.add",
		kind:      "reading.enqueue",
		wantAdd:   []string{"to-read"},
		preserved: []string{"existing"},
	},
	{
		name:       "start",
		args:       []string{"start", "K1"},
		seedTags:   []map[string]any{{"tag": "to-read", "type": float64(0)}, {"tag": "keep", "type": float64(0)}},
		operation:  "reading-list.start",
		kind:       "reading.start",
		wantAdd:    []string{"reading"},
		wantRemove: []string{"to-read"},
		preserved:  []string{"keep"},
	},
	{
		name:       "done",
		args:       []string{"done", "K1"},
		seedTags:   []map[string]any{{"tag": "to-read", "type": float64(0)}, {"tag": "reading", "type": float64(0)}, {"tag": "keep", "type": float64(0)}},
		operation:  "reading-list.done",
		kind:       "reading.done",
		wantAdd:    []string{"read"},
		wantRemove: []string{"to-read", "reading"},
		preserved:  []string{"keep"},
	},
}

func TestReadingListTransitionsApplies(t *testing.T) {
	for _, tc := range readingListTransitionCases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newItemTagTestServer(t, map[string]string{"K1": "42"}, map[string][]map[string]any{"K1": tc.seedTags})

			env := runReadingListStateTestCmd(t, srv, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, tc.args...)
			if !env.OK || env.Operation != tc.operation || env.Result == nil || env.Result.Summary.Applied != 1 || env.Result.Items[0].Status != "applied" {
				t.Fatalf("env = %+v, want one applied %s", env, tc.operation)
			}
			if len(env.Plan.Operations) != 1 || env.Plan.Operations[0].Kind != tc.kind {
				t.Fatalf("operations = %+v, want one %s", env.Plan.Operations, tc.kind)
			}
			if srv.patchCounts["K1"] != 1 {
				t.Fatalf("PATCH count = %d, want 1", srv.patchCounts["K1"])
			}
			if srv.patchHeaders["K1"] != "42" {
				t.Errorf("If-Unmodified-Since-Version = %q, want 42", srv.patchHeaders["K1"])
			}
			body := srv.patchBodies["K1"]
			for _, tag := range slices.Concat(tc.wantAdd, tc.preserved) {
				if !patchBodyHasTag(body, tag) {
					t.Errorf("PATCH body = %+v, want tag %q", body, tag)
				}
			}
			for _, tag := range tc.wantRemove {
				if patchBodyHasTag(body, tag) {
					t.Errorf("PATCH body = %+v, want tag %q removed", body, tag)
				}
			}
		})
	}
}

// An idempotent re-run of a reading-list transition must report no_op with the
// transition's own reason and issue no PATCH: replaying `reading-list start`
// on an item that already carries "reading" (or `done` on one that already
// carries "read") must not bump the item version. The consolidated table
// above asserts Applied == 1 for every case, so this witness lives outside
// it: the table cannot express a no-op row.
func TestReadingListIdempotentReRunIsNoOp(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		operation string
		seedTags  []map[string]any
	}{
		{
			name:      "start",
			args:      []string{"start", "K1"},
			operation: "reading-list.start",
			seedTags:  []map[string]any{{"tag": "reading", "type": float64(0)}},
		},
		{
			name:      "done",
			args:      []string{"done", "K1"},
			operation: "reading-list.done",
			seedTags:  []map[string]any{{"tag": "read", "type": float64(0)}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newItemTagTestServer(t, map[string]string{"K1": "42"}, map[string][]map[string]any{"K1": tc.seedTags})

			env := runReadingListStateTestCmd(t, srv, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, tc.args...)
			if !env.OK || env.Operation != tc.operation || env.Result == nil || env.Result.Summary.NoOp != 1 || env.Result.Summary.Applied != 0 {
				t.Fatalf("env = %+v, want one no_op %s", env, tc.operation)
			}
			if env.Result.Items[0].Status != "no_op" {
				t.Fatalf("status = %q, want no_op", env.Result.Items[0].Status)
			}
			if got, _ := env.Result.Items[0].Reason.(string); got != "reading tags already in requested state" {
				t.Fatalf("reason = %q, want the reading-list no_op reason", env.Result.Items[0].Reason)
			}
			if srv.patchCounts["K1"] != 0 {
				t.Fatalf("PATCH count = %d, want 0: an idempotent re-run must not bump the item version", srv.patchCounts["K1"])
			}
		})
	}
}

func TestReadingListPreviewWritesNothing(t *testing.T) {
	srv := newItemTagTestServer(t, map[string]string{"K1": "42"}, map[string][]map[string]any{
		"K1": {{"tag": "to-read", "type": float64(0)}},
	})

	env := runReadingListStateTestCmd(t, srv, &rootFlags{asJSON: true, maxChanges: -1}, "start", "K1")
	if !env.OK || env.Mode != "preview" || env.Result != nil || env.Plan.Summary.Planned != 1 {
		t.Fatalf("env = %+v, want preview plan with one change", env)
	}
	if srv.patchCounts["K1"] != 0 {
		t.Fatalf("PATCH count = %d, want 0", srv.patchCounts["K1"])
	}
}

func TestReadingListBulkKeysFrom(t *testing.T) {
	srv := newItemTagTestServer(t, map[string]string{"K1": "42", "K2": "43"}, map[string][]map[string]any{
		"K1": {{"tag": "to-read", "type": float64(0)}},
		"K2": {{"tag": "to-read", "type": float64(0)}},
	})
	keysPath := filepath.Join(t.TempDir(), "keys.txt")
	if err := os.WriteFile(keysPath, []byte("K1\nK2\n"), 0o600); err != nil {
		t.Fatalf("write keys file: %v", err)
	}

	env := runReadingListStateTestCmd(t, srv, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "start", "--keys-from", keysPath)
	if !env.OK || env.Result == nil || env.Result.Summary.Applied != 2 || len(env.Result.Items) != 2 {
		t.Fatalf("env = %+v, want two applied items", env)
	}
	for _, key := range []string{"K1", "K2"} {
		if srv.patchCounts[key] != 1 {
			t.Fatalf("%s PATCH count = %d, want 1", key, srv.patchCounts[key])
		}
		body := srv.patchBodies[key]
		if patchBodyHasTag(body, "to-read") || !patchBodyHasTag(body, "reading") {
			t.Errorf("%s PATCH body = %+v, want start transition", key, body)
		}
	}
}

func changeSets(changes []mutation.Change) (added, removed []string) {
	for _, change := range changes {
		if tag, ok := change.Add.(string); ok {
			added = append(added, tag)
		}
		if tag, ok := change.Remove.(string); ok {
			removed = append(removed, tag)
		}
	}
	return added, removed
}

// A dry-run client answers every request locally, GET included, so reading the
// live item to diff its tags returned {"dry_run": true} and failed the command
// on "item response missing data object": add, start, and done were all
// unusable with --dry-run. The plan has to come from the transition instead,
// and it must not touch the network at all.
func TestReadingListTransitionsPlanUnderDryRunWithoutCallingTheAPI(t *testing.T) {
	for _, tc := range readingListTransitionCases {
		t.Run(tc.name, func(t *testing.T) {
			// Seeded with the queue tag on purpose, not with tc.seedTags: the
			// dry-run plan is an upper bound, so "add" must still report Add
			// to-read for an item that already carries it.
			srv := newItemTagTestServer(t, map[string]string{"K1": "42"}, map[string][]map[string]any{"K1": {{"tag": "to-read", "type": float64(0)}}})

			env := runReadingListStateTestCmd(t, srv, &rootFlags{asJSON: true, dryRun: true, maxChanges: -1}, tc.args...)
			if !env.OK || env.Operation != tc.operation {
				t.Fatalf("env = %+v, want a successful %s plan", env, tc.operation)
			}
			if env.Mode != "preview" || env.PreviewReason != "dry_run" {
				t.Fatalf("mode = %q/%q, want preview/dry_run", env.Mode, env.PreviewReason)
			}
			if env.Result != nil {
				t.Fatalf("result = %+v, want nothing applied", env.Result)
			}
			if len(env.Plan.Operations) != 1 || env.Plan.Operations[0].Kind != tc.kind {
				t.Fatalf("operations = %+v, want one %s", env.Plan.Operations, tc.kind)
			}

			added, removed := changeSets(env.Plan.Operations[0].Changes)
			if !slices.Equal(added, tc.wantAdd) {
				t.Errorf("added = %v, want %v", added, tc.wantAdd)
			}
			if !slices.Equal(removed, tc.wantRemove) {
				t.Errorf("removed = %v, want %v", removed, tc.wantRemove)
			}

			if srv.getCounts["K1"] != 0 || srv.patchCounts["K1"] != 0 {
				t.Errorf("dry run made %d GET and %d PATCH requests, want none", srv.getCounts["K1"], srv.patchCounts["K1"])
			}
		})
	}
}
