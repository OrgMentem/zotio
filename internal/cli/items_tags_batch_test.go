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
	// existing seeds each item's current tags, so a test can mix no-op items
	// into a batch.
	existing map[string][]map[string]any
	// reject maps an item key to the code the array write reports for it.
	reject map[string]int
	// staleVersion makes the server enforce the precondition for real: any
	// object whose submitted version is not the item's current version is
	// rejected with 412, the way Zotero does.
	enforceVersion bool
	// failPost makes the array write itself fail at the transport level.
	failPost bool

	posts    [][]map[string]any
	getCount int
}

func newBatchTagServer(t *testing.T, keys []string) *batchTagServer {
	t.Helper()
	b := &batchTagServer{
		versions: make(map[string]int, len(keys)),
		existing: map[string][]map[string]any{},
		reject:   map[string]int{},
	}
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
				"data": map[string]any{"key": key, "version": version, "tags": b.tagsFor(key)},
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/items"):
			if b.failPost {
				http.Error(w, "upstream exploded", http.StatusBadGateway)
				return
			}
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
				// Enforce the precondition the way Zotero does, so a test can
				// prove the submitted version is honoured rather than merely
				// serialized.
				if b.enforceVersion {
					submitted, _ := obj["version"].(float64)
					if int(submitted) != b.versions[key] {
						failed[strconv.Itoa(i)] = map[string]any{
							"key": key, "code": 412, "message": "Item has been modified since specified version",
						}
						continue
					}
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

// tagsFor reports an item's seeded tags, defaulting to none.
func (b *batchTagServer) tagsFor(key string) []map[string]any {
	if tags, ok := b.existing[key]; ok {
		return tags
	}
	return []map[string]any{}
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
	// A conflict exits 1, the contract measured live for the per-item path in
	// dev/field-report-2026-08-30-conflict-contract-live.md. Adding --batch
	// must not downgrade it to the generic degraded exit 13. Asserting the
	// server's message here instead would pin whichever error happened to
	// surface; the message belongs on the item, checked below.
	if code := ExitCode(err); code != 1 {
		t.Errorf("exit code = %d, want 1 for a pure conflict", code)
	}
	reason := fmt.Sprint(byKey["K2"].Reason)
	if !strings.Contains(reason, "rejected K2") {
		t.Errorf("K2 reason = %q, want the server's own message for that object", reason)
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

// The alignment invariant: a no-op item holds no slot in the request, so the
// object array and the op list are indexed by different counters. If they ever
// drift, an item reads another item's outcome — silently. This is the case the
// original suite could not reach, because its fixture gave every item zero tags.
func TestItemsTagsBatchKeepsAlignmentWhenAnItemIsANoOp(t *testing.T) {
	keys := []string{"K1", "K2", "K3"}
	b := newBatchTagServer(t, keys)
	b.existing["K2"] = []map[string]any{{"tag": "sweep"}} // already tagged
	b.reject["K3"] = 400                                  // must land on K3, not K2

	env, err := runBatchTagCmd(t, b, append([]string{"add", "--batch", "--tag", "sweep"}, keys...)...)
	if err == nil {
		t.Fatal("a rejected object must make the run report an error")
	}
	if len(b.posts) != 1 || len(b.posts[0]) != 2 {
		t.Fatalf("request carried %v objects, want exactly 2 (K2 is a no-op)", b.posts)
	}
	if b.posts[0][0]["key"] != "K1" || b.posts[0][1]["key"] != "K3" {
		t.Fatalf("objects = %v/%v, want K1 then K3", b.posts[0][0]["key"], b.posts[0][1]["key"])
	}
	byKey := map[string]mutation.ResultItem{}
	for _, item := range env.Result.Items {
		byKey[item.Key] = item
	}
	if got := byKey["K2"].Status; got != "no_op" {
		t.Errorf("K2 status = %q, want no_op", got)
	}
	if got := byKey["K3"].Status; got != "failed" {
		t.Errorf("K3 status = %q, want failed; a non-412 rejection is not a conflict", got)
	}
	if got := byKey["K1"].Status; got != "applied" {
		t.Errorf("K1 status = %q, want applied", got)
	}
	// The rejection is reported on the item it belongs to, naming the item
	// rather than a position in an array the operator never sees. Alignment
	// is what makes that possible: K3's failure must not land on K2.
	if reason := fmt.Sprint(byKey["K3"].Reason); !strings.Contains(reason, "rejected K3") {
		t.Errorf("K3 reason = %q, want its own server message", reason)
	}
	if byKey["K2"].Reason != nil && strings.Contains(fmt.Sprint(byKey["K2"].Reason), "rejected") {
		t.Errorf("K2 reason = %v, want no rejection on the no-op item", byKey["K2"].Reason)
	}
}

// The submitted version must be a precondition the server can enforce, not a
// field that merely appears in the payload.
func TestItemsTagsBatchStaleVersionBecomesThatItemsConflict(t *testing.T) {
	keys := []string{"K1", "K2"}
	b := newBatchTagServer(t, keys)
	b.enforceVersion = true

	env, err := runBatchTagCmd(t, b, append([]string{"add", "--batch", "--tag", "sweep"}, keys...)...)
	if err != nil {
		t.Fatalf("a batch sending current versions must succeed: %v", err)
	}
	if env.Result.Summary.Applied != 2 {
		t.Fatalf("summary = %+v, want both applied under an enforcing server", env.Result.Summary)
	}

	// Now move the server's version out from under the planning read.
	b2 := newBatchTagServer(t, keys)
	b2.enforceVersion = true
	b2.versions["K2"] = 9999
	b2.existing["K2"] = nil
	// The planning GET reports 9999 too, so shift only after planning by
	// rejecting K2's submitted version explicitly.
	b2.reject["K2"] = 412
	env2, err2 := runBatchTagCmd(t, b2, append([]string{"add", "--batch", "--tag", "sweep"}, keys...)...)
	if ExitCode(err2) != 1 {
		t.Errorf("exit code = %d, want 1 for a lost precondition", ExitCode(err2))
	}
	if env2.Result.Summary.Conflicts != 1 {
		t.Errorf("summary = %+v, want one conflict", env2.Result.Summary)
	}
}

// A failed request must fail its items loudly, not report them applied.
func TestItemsTagsBatchTransportFailureFailsEveryItemInTheRequest(t *testing.T) {
	keys := []string{"K1", "K2", "K3"}
	b := newBatchTagServer(t, keys)
	b.failPost = true

	env, err := runBatchTagCmd(t, b, append([]string{"add", "--batch", "--tag", "sweep"}, keys...)...)
	if err == nil {
		t.Fatal("a failed request must make the run report an error")
	}
	if env.Result == nil || env.Result.Summary.Failed != 3 || env.Result.Summary.Applied != 0 {
		t.Fatalf("summary = %+v, want all three failed and none applied", env.Result)
	}
	if env.OK {
		t.Error("envelope reports ok for a run whose only request failed")
	}
}

// A response naming an index the request cannot own means no object's outcome
// is known. Reporting the rest as applied would turn an unparseable response
// into a success envelope.
func TestItemsTagsBatchUnattributableIndexFailsClosed(t *testing.T) {
	// Enough keys for a second chunk: the guard must compare inside the
	// chunk, because a naive start+offset bound overflows and silently
	// admits the bogus index once start is past zero.
	keys := make([]string, 0, 60)
	for i := range 60 {
		keys = append(keys, fmt.Sprintf("K%03d", i))
	}
	b := newBatchTagServer(t, keys)
	b.server.Close()
	b.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			key := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			w.Header().Set("Last-Modified-Version", "100")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"key": key, "version": 100,
				"data": map[string]any{"key": key, "version": 100, "tags": []any{}},
			})
			return
		}
		// An index far outside the submitted array, which also overflows a
		// naive start+offset bound check.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"successful": map[string]any{},
			"unchanged":  map[string]any{},
			"failed": map[string]any{
				"9223372036854775807": map[string]any{"key": "??", "code": 400, "message": "nonsense"},
			},
		})
	}))
	t.Cleanup(b.server.Close)

	env, err := runBatchTagCmd(t, b, append([]string{"add", "--batch", "--tag", "sweep"}, keys...)...)
	if err == nil {
		t.Fatal("an unattributable response must make the run report an error")
	}
	if env.OK {
		t.Error("envelope reports ok:true for a response that could not be attributed")
	}
	for _, item := range env.Result.Items {
		if item.Status == "applied" {
			t.Errorf("%s reported applied though no outcome could be attributed", item.Key)
		}
	}
}

// remove --batch had no coverage at all.
func TestItemsTagsBatchRemoveSendsRemainingTags(t *testing.T) {
	b := newBatchTagServer(t, []string{"K1"})
	b.existing["K1"] = []map[string]any{{"tag": "keep"}, {"tag": "drop"}}

	env, err := runBatchTagCmd(t, b, "remove", "--batch", "--tag", "drop", "K1")
	if err != nil {
		t.Fatalf("batch remove: %v", err)
	}
	if env.Result.Summary.Applied != 1 {
		t.Fatalf("summary = %+v, want one applied", env.Result.Summary)
	}
	if len(b.posts) != 1 {
		t.Fatalf("write requests = %d, want 1", len(b.posts))
	}
	tags, _ := b.posts[0][0]["tags"].([]any)
	if len(tags) != 1 {
		t.Fatalf("submitted tags = %v, want only the surviving tag", b.posts[0][0]["tags"])
	}
	if name, _ := tags[0].(map[string]any)["tag"].(string); name != "keep" {
		t.Errorf("surviving tag = %q, want keep", name)
	}
}

func TestItemsTagsBatchRefusesExplicitFailFast(t *testing.T) {
	b := newBatchTagServer(t, []string{"K1"})
	t.Setenv("ZOTERO_BASE_URL", b.server.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	// The whole root, because --continue-on-error is a persistent root flag
	// and the refusal reads whether the caller set it.
	root := newRootCmd(&rootFlags{})
	root.SilenceErrors, root.SilenceUsage = true, true
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"items", "tags", "add", "--batch", "--continue-on-error=false", "--yes", "--json", "--tag", "sweep", "K1"})

	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "--continue-on-error=false cannot be honoured with --batch") {
		t.Fatalf("err = %v, want a refusal naming both flags", err)
	}
	if len(b.posts) != 0 {
		t.Errorf("refused command still wrote %d request(s)", len(b.posts))
	}
}
