// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"zotio/internal/mutation"
	"zotio/internal/store"
)

// tagDeletePlane is a stateful write plane: /items?tag= pages (limit-capped at
// 100), /items?itemKey= reads, per-item PATCH and array POST. It records every
// write so a test can assert the exact request shape.
type tagDeletePlane struct {
	srv *httptest.Server

	mu       sync.Mutex
	order    []string
	tags     map[string][]map[string]any
	patches  map[string][]map[string]any
	patchVer map[string]string
	posts    [][]map[string]any
	requests int
}

func newTagDeletePlane(t *testing.T, tags map[string][]map[string]any) *tagDeletePlane {
	t.Helper()
	p := &tagDeletePlane{tags: tags, patches: map[string][]map[string]any{}, patchVer: map[string]string{}}
	for key := range tags {
		p.order = append(p.order, key)
	}
	sort.Strings(p.order)
	item := func(key string) map[string]any {
		return map[string]any{"key": key, "version": 7, "data": map[string]any{"key": key, "version": 7, "tags": p.tags[key]}}
	}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.requests++
		w.Header().Set("Content-Type", "application/json")
		q := r.URL.Query()
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/items") && q.Get("tag") != "":
			limit, _ := strconv.Atoi(q.Get("limit"))
			start, _ := strconv.Atoi(q.Get("start"))
			if limit > 100 || limit <= 0 {
				limit = 100
			}
			var carriers []map[string]any
			for _, key := range p.order {
				for _, tg := range p.tags[key] {
					if tg["tag"] == q.Get("tag") {
						carriers = append(carriers, item(key))
						break
					}
				}
			}
			if start > len(carriers) {
				start = len(carriers)
			}
			end := min(start+limit, len(carriers))
			page := carriers[start:end]
			if page == nil {
				page = []map[string]any{}
			}
			_ = json.NewEncoder(w).Encode(page)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/items") && q.Get("itemKey") != "":
			rows := []map[string]any{}
			for _, key := range strings.Split(q.Get("itemKey"), ",") {
				if _, ok := p.tags[key]; ok {
					rows = append(rows, item(key))
				}
			}
			_ = json.NewEncoder(w).Encode(rows)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/items/"):
			key := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			if _, ok := p.tags[key]; !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Last-Modified-Version", "7")
			_ = json.NewEncoder(w).Encode(item(key))
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/items/"):
			key := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			var body struct {
				Tags []map[string]any `json:"tags"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			p.patches[key] = body.Tags
			p.patchVer[key] = r.Header.Get("If-Unmodified-Since-Version")
			p.tags[key] = body.Tags
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/items"):
			var objects []map[string]any
			if err := json.NewDecoder(r.Body).Decode(&objects); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			p.posts = append(p.posts, objects)
			successful := map[string]any{}
			for i, obj := range objects {
				key, _ := obj["key"].(string)
				raw, _ := json.Marshal(obj["tags"])
				var next []map[string]any
				_ = json.Unmarshal(raw, &next)
				p.tags[key] = next
				successful[strconv.Itoa(i)] = map[string]any{"key": key}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"successful": successful, "unchanged": map[string]any{}, "failed": map[string]any{}})
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.String(), http.StatusNotFound)
		}
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *tagDeletePlane) tagNames(key string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, tg := range p.tags[key] {
		out = append(out, fmt.Sprintf("%v/%v", tg["tag"], tg["type"]))
	}
	sort.Strings(out)
	return out
}

func tagObj(name string, typ int) map[string]any {
	return map[string]any{"tag": name, "type": typ}
}

func runTagsTestCmd(t *testing.T, flags *rootFlags, baseURL string, args ...string) (mutation.Envelope, string, error) {
	t.Helper()
	t.Setenv("ZOTERO_BASE_URL", baseURL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	cmd := newTagsCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()
	var env mutation.Envelope
	if out.Len() > 0 {
		_ = json.Unmarshal(out.Bytes(), &env)
	}
	return env, out.String(), err
}

func TestTagsDeleteDryRunPlansOneRemoveOpPerCarrierAndWritesNothing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := newTagDeletePlane(t, map[string][]map[string]any{
		"AAAAAAA1": {tagObj("junk", 0), tagObj("keep", 0)},
		"AAAAAAA2": {tagObj("junk", 1)},
		"AAAAAAA3": {tagObj("keep", 0)},
	})
	env, out, err := runTagsTestCmd(t, &rootFlags{asJSON: true, dryRun: true, maxChanges: -1}, p.srv.URL, "delete", "junk")
	if err != nil {
		t.Fatalf("tags delete --dry-run: %v\n%s", err, out)
	}
	if len(env.Plan.Operations) != 2 {
		t.Fatalf("plan ops = %d, want 2 (one per carrier): %s", len(env.Plan.Operations), out)
	}
	for _, op := range env.Plan.Operations {
		if op.Kind != "tag_remove" {
			t.Errorf("op kind = %q, want tag_remove", op.Kind)
		}
	}
	if len(p.patches) != 0 || len(p.posts) != 0 {
		t.Fatalf("dry-run wrote: patches=%v posts=%v", p.patches, p.posts)
	}
}

func TestTagsDeleteApplyPatchesEachCarrierKeepingOtherTags(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := newTagDeletePlane(t, map[string][]map[string]any{
		"AAAAAAA1": {tagObj("junk", 0), tagObj("keep", 0)},
		"AAAAAAA2": {tagObj("junk", 1), tagObj("other", 1)},
		"AAAAAAA3": {tagObj("keep", 0)},
	})
	_, out, err := runTagsTestCmd(t, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, p.srv.URL, "delete", "junk")
	if err != nil {
		t.Fatalf("tags delete --yes: %v\n%s", err, out)
	}
	if len(p.patches) != 2 {
		t.Fatalf("PATCH count = %d, want 2: %v", len(p.patches), p.patches)
	}
	for key, ver := range p.patchVer {
		if ver != "7" {
			t.Errorf("%s PATCH If-Unmodified-Since-Version = %q, want 7", key, ver)
		}
	}
	if got, want := p.tagNames("AAAAAAA1"), []string{"keep/0"}; !reflect.DeepEqual(got, want) {
		t.Errorf("AAAAAAA1 tags = %v, want %v", got, want)
	}
	if got, want := p.tagNames("AAAAAAA2"), []string{"other/1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("AAAAAAA2 tags = %v, want %v", got, want)
	}
	if _, touched := p.patches["AAAAAAA3"]; touched {
		t.Error("a non-carrier was patched")
	}
}

func TestTagsDeleteAutomaticOnlyLeavesManualTag(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := newTagDeletePlane(t, map[string][]map[string]any{
		"AAAAAAA1": {tagObj("junk", 0), tagObj("junk", 1), tagObj("keep", 0)},
		"AAAAAAA2": {tagObj("junk", 0)},
	})
	_, out, err := runTagsTestCmd(t, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, p.srv.URL, "delete", "junk", "--automatic-only")
	if err != nil {
		t.Fatalf("tags delete --automatic-only: %v\n%s", err, out)
	}
	if got, want := p.tagNames("AAAAAAA1"), []string{"junk/0", "keep/0"}; !reflect.DeepEqual(got, want) {
		t.Errorf("AAAAAAA1 tags = %v, want the manual junk kept: %v", got, want)
	}
	if _, touched := p.patches["AAAAAAA2"]; touched {
		t.Error("an item with only the manual tag was patched")
	}
}

func TestTagsDeletePagesPastOneHundredCarriersAndBatches(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tags := map[string][]map[string]any{}
	for i := range 230 {
		tags[fmt.Sprintf("K%07d", i)] = []map[string]any{tagObj("junk", 0), tagObj("keep", 0)}
	}
	p := newTagDeletePlane(t, tags)
	_, out, err := runTagsTestCmd(t, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, p.srv.URL, "delete", "junk", "--batch")
	if err != nil {
		t.Fatalf("tags delete --batch: %v\n%s", err, out)
	}
	if len(p.posts) != 5 {
		t.Fatalf("POST count = %d, want 5 (230 carriers in chunks of 50)", len(p.posts))
	}
	total := 0
	for _, post := range p.posts {
		if len(post) > 50 {
			t.Errorf("batch of %d objects exceeds 50", len(post))
		}
		total += len(post)
	}
	if total != 230 {
		t.Fatalf("objects written = %d, want all 230 carriers", total)
	}
	for key := range tags {
		if got := p.tagNames(key); !reflect.DeepEqual(got, []string{"keep/0"}) {
			t.Fatalf("%s tags = %v, want only keep", key, got)
		}
	}
}

func TestTagsDeleteUnknownTagSaysNoCarriers(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := newTagDeletePlane(t, map[string][]map[string]any{"AAAAAAA1": {tagObj("keep", 0)}})
	env, out, err := runTagsTestCmd(t, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, p.srv.URL, "delete", "ghost")
	if err != nil {
		t.Fatalf("tags delete ghost: %v\n%s", err, out)
	}
	if !strings.Contains(out, "no item carries this tag") {
		t.Fatalf("output lacks the no-carriers reason: %s", out)
	}
	if env.Result != nil && env.Result.Summary.Applied != 0 {
		t.Fatalf("applied = %d, want 0", env.Result.Summary.Applied)
	}
	if len(p.patches) != 0 {
		t.Fatal("a write was sent for an unknown tag")
	}
}

func TestTagsDeleteJournalUndoRestoresTagAndMirror(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mutationJournalRecorder = recordMutationJournal
	mirrorWriteThrough = applyMirrorWriteThrough
	t.Cleanup(func() { mutationJournalRecorder = nil; mirrorWriteThrough = nil })

	seedWriteThroughItem(t, "AAAAAAA1", `{"key":"AAAAAAA1","version":7,"data":{"key":"AAAAAAA1","itemType":"journalArticle","title":"P","tags":[{"tag":"junk","type":0},{"tag":"keep","type":0}]}}`)
	p := newTagDeletePlane(t, map[string][]map[string]any{
		"AAAAAAA1": {tagObj("junk", 0), tagObj("keep", 0)},
	})
	env, out, err := runTagsTestCmd(t, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, p.srv.URL, "delete", "junk")
	if err != nil {
		t.Fatalf("tags delete: %v\n%s", err, out)
	}
	mirrorTags := func() string { return writeThroughItemField(t, "AAAAAAA1", "$.data.tags") }
	if got := mirrorTags(); strings.Contains(got, "junk") {
		t.Fatalf("mirror tags after delete = %s, want junk gone", got)
	}
	undoTagRunToCompletion(t, journalRunID(t, env))
	if got, want := p.tagNames("AAAAAAA1"), []string{"junk/<nil>", "keep/0"}; !reflect.DeepEqual(got, want) {
		t.Errorf("write plane tags after undo = %v, want %v", got, want)
	}
	if got := mirrorTags(); !strings.Contains(got, "junk") || !strings.Contains(got, "keep") {
		t.Errorf("mirror tags after undo = %s, want junk restored", got)
	}
}

func seedPruneMirror(t *testing.T) {
	t.Helper()
	db, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	items := []string{
		`{"key":"AAAAAAA1","version":1,"data":{"key":"AAAAAAA1","itemType":"journalArticle","tags":[{"tag":"auto-one","type":1},{"tag":"auto-two","type":1},{"tag":"manual-one","type":0}]}}`,
		`{"key":"AAAAAAA2","version":1,"data":{"key":"AAAAAAA2","itemType":"journalArticle","tags":[{"tag":"auto-two","type":1}]}}`,
	}
	for _, raw := range items {
		if _, _, err := db.UpsertBatch("items", []json.RawMessage{json.RawMessage(raw)}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
}

func TestTagsPruneListsLowCountAutomaticTagsFromMirrorWithoutNetwork(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seedPruneMirror(t)
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Error(w, "network used", http.StatusInternalServerError)
	}))
	defer srv.Close()
	_, out, err := runTagsTestCmd(t, &rootFlags{asJSON: true, dryRun: true, maxChanges: -1}, srv.URL, "prune", "--automatic", "--min-items", "2")
	if err != nil {
		t.Fatalf("tags prune: %v\n%s", err, out)
	}
	if hits != 0 {
		t.Fatalf("prune preview made %d network requests", hits)
	}
	if !strings.Contains(out, "auto-one") {
		t.Errorf("output lacks auto-one (1 item < 2): %s", out)
	}
	for _, unwanted := range []string{"auto-two", "manual-one"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("output lists %s, which is not a candidate: %s", unwanted, out)
		}
	}
}

func TestTagsPruneApplyRemovesOnlyThePlannedTag(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seedPruneMirror(t)
	p := newTagDeletePlane(t, map[string][]map[string]any{
		"AAAAAAA1": {tagObj("auto-one", 1), tagObj("auto-two", 1), tagObj("manual-one", 0)},
		"AAAAAAA2": {tagObj("auto-two", 1)},
	})
	_, out, err := runTagsTestCmd(t, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, p.srv.URL, "prune", "--automatic", "--min-items", "2")
	if err != nil {
		t.Fatalf("tags prune --yes: %v\n%s", err, out)
	}
	if got, want := p.tagNames("AAAAAAA1"), []string{"auto-two/1", "manual-one/0"}; !reflect.DeepEqual(got, want) {
		t.Errorf("AAAAAAA1 tags = %v, want %v", got, want)
	}
	if len(p.patches) != 1 {
		t.Errorf("PATCH count = %d, want 1", len(p.patches))
	}
}

func TestTagsPruneNeedsAMirror(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, out, err := runTagsTestCmd(t, &rootFlags{asJSON: true, dryRun: true, maxChanges: -1}, "http://127.0.0.1:1", "prune", "--automatic")
	if err == nil {
		t.Fatalf("prune without a mirror succeeded: %s", out)
	}
	if code := ExitCode(err); code != 9 {
		t.Errorf("exit code = %d, want 9 (precondition)", code)
	}
}
