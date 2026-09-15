// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"zotio/internal/mutation"
)

// batchTagServer answers per-item reads and one array write, recording every
// request so a test can assert the request count and the per-object payload.
type batchTagServer struct {
	server *httptest.Server

	versions map[string]int
	// reject maps an item key to the code the array write reports for it.
	reject map[string]int

	posts    [][]map[string]any
	getCount int
}

func newBatchTagServer(t *testing.T, keys []string) *batchTagServer {
	t.Helper()
	b := &batchTagServer{versions: make(map[string]int, len(keys)), reject: map[string]int{}}
	for i, key := range keys {
		b.versions[key] = 100 + i
	}
	b.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/items/"):
			key := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			version, ok := b.versions[key]
			if !ok {
				http.NotFound(w, r)
				return
			}
			b.getCount++
			w.Header().Set("Last-Modified-Version", strconv.Itoa(version))
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"key": key, "version": version,
				"data": map[string]any{"key": key, "version": version, "tags": []any{}},
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/items"):
			var objects []map[string]any
			if err := json.NewDecoder(r.Body).Decode(&objects); err != nil {
				t.Errorf("decode array write: %v", err)
			}
			b.posts = append(b.posts, objects)
			successful := map[string]any{}
			failed := map[string]any{}
			for i, obj := range objects {
				key, _ := obj["key"].(string)
				if code, bad := b.reject[key]; bad {
					failed[strconv.Itoa(i)] = map[string]any{
						"key": key, "code": code, "message": "rejected " + key,
					}
					continue
				}
				successful[strconv.Itoa(i)] = map[string]any{"key": key}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"successful": successful, "unchanged": map[string]any{}, "failed": failed,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(b.server.Close)
	return b
}

func runBatchTagCmd(t *testing.T, b *batchTagServer, args ...string) (mutation.Envelope, error) {
	t.Helper()
	t.Setenv("ZOTERO_BASE_URL", b.server.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	cmd := newItemsTagsCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
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

// The whole point of the flag: N items must cost one write request, not N,
// and each object must carry its own precondition.
func TestItemsTagsBatchSendsOneRequestWithPerItemVersions(t *testing.T) {
	keys := []string{"K1", "K2", "K3"}
	b := newBatchTagServer(t, keys)

	env, err := runBatchTagCmd(t, b, append([]string{"add", "--batch", "--tag", "sweep"}, keys...)...)
	if err != nil {
		t.Fatalf("batch add: %v", err)
	}
	if env.Result == nil || env.Result.Summary.Applied != 3 {
		t.Fatalf("summary = %+v, want three applied", env.Result)
	}
	if len(b.posts) != 1 {
		t.Fatalf("write requests = %d, want 1 for three items", len(b.posts))
	}
	if got := len(b.posts[0]); got != 3 {
		t.Fatalf("objects in request = %d, want 3", got)
	}
	for i, obj := range b.posts[0] {
		key := keys[i]
		if obj["key"] != key {
			t.Errorf("object %d key = %v, want %s", i, obj["key"], key)
		}
		// A per-object version is the only precondition available to a
		// heterogeneous batch; without it the write silently overwrites.
		if got := fmt.Sprint(obj["version"]); got != strconv.Itoa(b.versions[key]) {
			t.Errorf("object %d version = %v, want %d", i, obj["version"], b.versions[key])
		}
	}
}

// Attribution is the claim the whole design rests on: a rejected object must
// land on its own item, as its own conflict, without disturbing its neighbours.
func TestItemsTagsBatchAttributesRejectionToItsOwnItem(t *testing.T) {
	keys := []string{"K1", "K2", "K3"}
	b := newBatchTagServer(t, keys)
	b.reject["K2"] = 412

	env, err := runBatchTagCmd(t, b, append([]string{"add", "--batch", "--tag", "sweep"}, keys...)...)
	if err == nil {
		t.Fatal("a rejected object must make the run report an error")
	}
	if env.Result == nil {
		t.Fatalf("no result envelope; err = %v", err)
	}
	byKey := map[string]mutation.ResultItem{}
	for _, item := range env.Result.Items {
		byKey[item.Key] = item
	}
	if got := byKey["K2"].Status; got != "conflict" {
		t.Errorf("K2 status = %q, want conflict (code 412 is a lost precondition)", got)
	}
	if got := byKey["K1"].Status; got != "applied" {
		t.Errorf("K1 status = %q, want applied; a batch-mate's rejection must not taint it", got)
	}
	if got := byKey["K3"].Status; got != "applied" {
		t.Errorf("K3 status = %q, want applied; fail-fast must not report a sent object as not_attempted", got)
	}
	if env.Result.Summary.Conflicts != 1 || env.Result.Summary.Applied != 2 {
		t.Errorf("summary = %+v, want 2 applied and 1 conflict", env.Result.Summary)
	}
	if !strings.Contains(err.Error(), "rejected K2") {
		t.Errorf("error = %v, want the server's own message for the rejected object", err)
	}
}

// Zotero refuses more than 50 objects per request, so the work must split.
func TestItemsTagsBatchSplitsAtZoteroCeiling(t *testing.T) {
	keys := make([]string, 0, 120)
	for i := range 120 {
		keys = append(keys, fmt.Sprintf("K%03d", i))
	}
	b := newBatchTagServer(t, keys)

	env, err := runBatchTagCmd(t, b, append([]string{"add", "--batch", "--tag", "sweep"}, keys...)...)
	if err != nil {
		t.Fatalf("batch add: %v", err)
	}
	if env.Result == nil || env.Result.Summary.Applied != 120 {
		t.Fatalf("summary = %+v, want 120 applied", env.Result)
	}
	if len(b.posts) != 3 {
		t.Fatalf("write requests = %d, want 3 (50+50+20)", len(b.posts))
	}
	for i, want := range []int{50, 50, 20} {
		if got := len(b.posts[i]); got != want {
			t.Errorf("request %d carried %d objects, want %d", i, got, want)
		}
	}
	// The saving is in writes only; each item is still read once.
	if b.getCount != 120 {
		t.Errorf("reads = %d, want one per item", b.getCount)
	}
}

// --max-failures promises to stop early. A batch cannot, so the combination is
// refused rather than silently ignored.
func TestItemsTagsBatchRefusesMaxFailures(t *testing.T) {
	b := newBatchTagServer(t, []string{"K1"})
	t.Setenv("ZOTERO_BASE_URL", b.server.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	cmd := newItemsTagsCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1, maxFailures: 2})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"add", "--batch", "--tag", "sweep", "K1"})

	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--max-failures cannot be honoured with --batch") {
		t.Fatalf("err = %v, want a refusal naming both flags", err)
	}
	if len(b.posts) != 0 {
		t.Errorf("refused command still wrote %d request(s)", len(b.posts))
	}
}
