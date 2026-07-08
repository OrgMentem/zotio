// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
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

func TestReadingListAddAppliesQueueTag(t *testing.T) {
	srv := newItemTagTestServer(t, map[string]string{"K1": "42"}, map[string][]map[string]any{
		"K1": {{"tag": "existing", "type": float64(0)}},
	})

	env := runReadingListStateTestCmd(t, srv, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "add", "K1")
	if !env.OK || env.Operation != "reading-list.add" || env.Result == nil || env.Result.Summary.Applied != 1 || env.Result.Items[0].Status != "applied" {
		t.Fatalf("env = %+v, want one applied add", env)
	}
	if env.Plan.Operations[0].Kind != "reading.enqueue" {
		t.Fatalf("kind = %q, want reading.enqueue", env.Plan.Operations[0].Kind)
	}
	if srv.patchCounts["K1"] != 1 {
		t.Fatalf("PATCH count = %d, want 1", srv.patchCounts["K1"])
	}
	if srv.patchHeaders["K1"] != "42" {
		t.Errorf("If-Unmodified-Since-Version = %q, want 42", srv.patchHeaders["K1"])
	}
	if !patchBodyHasTag(srv.patchBodies["K1"], "to-read") {
		t.Errorf("PATCH body = %+v, want queue tag", srv.patchBodies["K1"])
	}
}

func TestReadingListStartSwapsQueueToReading(t *testing.T) {
	srv := newItemTagTestServer(t, map[string]string{"K1": "42"}, map[string][]map[string]any{
		"K1": {{"tag": "to-read", "type": float64(0)}, {"tag": "keep", "type": float64(0)}},
	})

	env := runReadingListStateTestCmd(t, srv, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "start", "K1")
	if !env.OK || env.Operation != "reading-list.start" || env.Result == nil || env.Result.Summary.Applied != 1 || env.Result.Items[0].Status != "applied" {
		t.Fatalf("env = %+v, want one applied start", env)
	}
	if srv.patchCounts["K1"] != 1 {
		t.Fatalf("PATCH count = %d, want 1", srv.patchCounts["K1"])
	}
	body := srv.patchBodies["K1"]
	if patchBodyHasTag(body, "to-read") || !patchBodyHasTag(body, "reading") || !patchBodyHasTag(body, "keep") {
		t.Errorf("PATCH body = %+v, want queue removed, reading added, keep preserved", body)
	}
}

func TestReadingListDoneSetsReadAndRemovesActiveTags(t *testing.T) {
	srv := newItemTagTestServer(t, map[string]string{"K1": "42"}, map[string][]map[string]any{
		"K1": {{"tag": "to-read", "type": float64(0)}, {"tag": "reading", "type": float64(0)}, {"tag": "keep", "type": float64(0)}},
	})

	env := runReadingListStateTestCmd(t, srv, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "done", "K1")
	if !env.OK || env.Operation != "reading-list.done" || env.Result == nil || env.Result.Summary.Applied != 1 || env.Result.Items[0].Status != "applied" {
		t.Fatalf("env = %+v, want one applied done", env)
	}
	body := srv.patchBodies["K1"]
	if patchBodyHasTag(body, "to-read") || patchBodyHasTag(body, "reading") || !patchBodyHasTag(body, "read") || !patchBodyHasTag(body, "keep") {
		t.Errorf("PATCH body = %+v, want queue/reading removed, read added, keep preserved", body)
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
