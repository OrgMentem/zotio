// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Undoing a tag rename must restore the item to its pre-run tags.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"zotio/internal/client"
	"zotio/internal/config"
	"zotio/internal/mutation"
)

// tagStatefulPlane is a write plane whose PATCH replaces the stored tag list,
// so a rename followed by an undo is observable as the final state.
type tagStatefulPlane struct {
	srv     *httptest.Server
	mu      sync.Mutex
	tags    map[string][]string
	version int
}

func newTagStatefulPlane(t *testing.T, tags map[string][]string) *tagStatefulPlane {
	t.Helper()
	plane := &tagStatefulPlane{tags: tags, version: 100}
	plane.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plane.mu.Lock()
		defer plane.mu.Unlock()
		key := strings.TrimPrefix(r.URL.Path, "/users/0/items/")
		current, ok := plane.tags[key]
		if !ok {
			http.Error(w, "unknown item "+key, http.StatusNotFound)
			return
		}
		switch r.Method {
		case http.MethodGet:
			list := make([]map[string]any, 0, len(current))
			for _, name := range current {
				list = append(list, map[string]any{"tag": name, "type": 0})
			}
			body, _ := json.Marshal(map[string]any{
				"key": key, "version": plane.version,
				"data": map[string]any{"key": key, "tags": list},
			})
			w.Header().Set("Last-Modified-Version", "100")
			_, _ = w.Write(body)
		case http.MethodPatch:
			var body struct {
				Tags []struct {
					Tag string `json:"tag"`
				} `json:"tags"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			next := make([]string, 0, len(body.Tags))
			for _, tag := range body.Tags {
				next = append(next, tag.Tag)
			}
			plane.tags[key] = next
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(plane.srv.Close)
	return plane
}

func (p *tagStatefulPlane) sortedTags(key string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := append([]string(nil), p.tags[key]...)
	sort.Strings(out)
	return out
}

func undoTagRunToCompletion(t *testing.T, runID string) {
	t.Helper()
	cmd := newJournalCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{"undo", runID})
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("journal undo %s: %v; stdout=%s stderr=%s", runID, err, out.String(), errOut.String())
	}
}

func journalRunID(t *testing.T, env mutation.Envelope) string {
	t.Helper()
	journal, _ := env.Journal.(map[string]any)
	runID, _ := journal["run_id"].(string)
	if runID == "" {
		t.Fatalf("envelope journal = %#v, want a recorded run_id", env.Journal)
	}
	return runID
}

// A rename merges into a target tag the item already carries, so undo must put
// the alias back and leave that target tag where the user had it. Inverting the
// recorded Remove=alias/Add=target pair would delete the user's own target tag.
// An item that did not carry the target tag must lose it again.
func TestTagRenameUndoKeepsTargetTagTheItemAlreadyHad(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	plane := newTagStatefulPlane(t, map[string][]string{
		"HASBOTH1": {"ml", "machine-learning", "keep"},
		"ONLYOLD1": {"ml", "keep"},
	})
	t.Setenv("ZOTERO_BASE_URL", plane.srv.URL+"/users/0")
	mutationJournalRecorder = recordMutationJournal
	t.Cleanup(func() { mutationJournalRecorder = nil })

	c := client.New(&config.Config{BaseURL: plane.srv.URL + "/users/0"}, 5*time.Second, 0)
	c.NoCache = true
	planned := `[
		{"key":"HASBOTH1","version":3,"data":{"tags":[{"tag":"ml","type":0},{"tag":"machine-learning","type":0},{"tag":"keep","type":0}]}},
		{"key":"ONLYOLD1","version":4,"data":{"tags":[{"tag":"ml","type":0},{"tag":"keep","type":0}]}}
	]`
	updates, err := buildTagRenameUpdates(json.RawMessage(planned), "ml", "machine-learning")
	if err != nil {
		t.Fatalf("buildTagRenameUpdates: %v", err)
	}
	ops := buildTagRenameOps(updates, "ml", "machine-learning", func(update tagRenameUpdate) (string, any, error) {
		return applyTagRenameUpdate(c, update)
	})
	env, err := runMutation(context.Background(), &rootFlags{yes: true, maxChanges: -1}, "tags.rename", ops)
	if err != nil || env.Result == nil || env.Result.Summary.Applied != 2 {
		t.Fatalf("rename = %+v, err %v, want two applied", env.Result, err)
	}
	for key, want := range map[string][]string{
		"HASBOTH1": {"keep", "machine-learning"},
		"ONLYOLD1": {"keep", "machine-learning"},
	} {
		if got := plane.sortedTags(key); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s tags after rename = %v, want %v", key, got, want)
		}
	}

	undoTagRunToCompletion(t, journalRunID(t, env))

	for key, want := range map[string][]string{
		"HASBOTH1": {"keep", "machine-learning", "ml"},
		"ONLYOLD1": {"keep", "ml"},
	} {
		if got := plane.sortedTags(key); !reflect.DeepEqual(got, want) {
			t.Errorf("%s tags after undo = %v, want the pre-rename %v", key, got, want)
		}
	}
}

// A rename journaled before renames recorded their own inverse carries only the
// Remove=alias/Add=target pair, which cannot say whether the target tag was
// already there. Undo must refuse it rather than guess.
func TestJournalUndoRefusesLegacyTagRename(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	t.Setenv("ZOTERO_BASE_URL", "http://127.0.0.1:1/users/0")

	entry := mutation.JournalEntry{
		SchemaVersion: mutation.JournalSchemaVersion, RunID: "legacy-rename", Operation: "tags.rename", Mode: "apply", OK: true,
		Timestamp: time.Now(), Summary: mutation.ResultSummary{Attempted: 1, Applied: 1},
		Ops: []mutation.JournalOp{{
			ID: "tags.rename:K1", Key: "K1", Kind: "tag_rename", Status: "applied",
			Changes: []mutation.Change{{Field: "tags", Remove: "ml", Add: "machine-learning"}},
		}},
	}
	if err := mutation.WriteEntry(helpersTestJournalDir(t), entry); err != nil {
		t.Fatalf("seed journal: %v", err)
	}

	cmd := newJournalCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{"undo", "legacy-rename"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("undo of a legacy rename exited 0, want a refusal; stdout=%s", out.String())
	}
	var env mutation.Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	if env.OK || len(env.Plan.Operations) != 0 {
		t.Fatalf("undo env = %+v, want no planned reversal", env)
	}
	journal, _ := env.Journal.(map[string]any)
	refused, _ := journal["refused"].([]any)
	if len(refused) != 1 {
		t.Fatalf("journal = %#v, want exactly one refusal", env.Journal)
	}
}
