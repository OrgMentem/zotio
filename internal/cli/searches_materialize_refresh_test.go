// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"zotio/internal/mutation"
)

func TestSearchesMaterializeRefresh(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		flags      rootFlags
		wantKinds  []string
		wantWrites []string
	}{
		{"preview", nil, rootFlags{asJSON: true, maxChanges: -1}, []string{"collection_add"}, nil},
		{"prune_dry_run", []string{"--prune"}, rootFlags{asJSON: true, yes: true, dryRun: true, maxChanges: -1}, []string{"collection_add", "collection_remove"}, nil},
		{"prune_apply", []string{"--prune"}, rootFlags{asJSON: true, yes: true, maxChanges: -1}, []string{"collection_add", "collection_remove"}, []string{"A", "C"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var patches []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/users/0/searches/SK/items":
					_, _ = fmt.Fprint(w, `[{"key":"A"},{"key":"B"}]`)
				case "/users/0/collections/TARGET/items":
					_, _ = fmt.Fprint(w, `[{"key":"B","data":{"collections":["TARGET"]}},{"key":"C","data":{"collections":["OTHER","TARGET"]}}]`)
				case "/users/0/items/A", "/users/0/items/C":
					key := strings.TrimPrefix(r.URL.Path, "/users/0/items/")
					switch r.Method {
					case http.MethodGet:
						w.Header().Set("Last-Modified-Version", "12")
						collections := `["OTHER"]`
						if key == "C" {
							collections = `["OTHER","TARGET"]`
						}
						_, _ = fmt.Fprintf(w, `{"key":%q,"version":12,"data":{"collections":%s}}`, key, collections)
					case http.MethodPatch:
						if r.Header.Get("If-Unmodified-Since-Version") != "12" {
							t.Errorf("missing version precondition for %s", key)
						}
						var body struct {
							Collections []string `json:"collections"`
						}
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Errorf("decode PATCH: %v", err)
						}
						want := []string{"OTHER", "TARGET"}
						if key == "C" {
							want = []string{"OTHER"}
						}
						if !reflect.DeepEqual(body.Collections, want) {
							t.Errorf("PATCH %s collections = %v, want %v", key, body.Collections, want)
						}
						patches = append(patches, key)
						w.WriteHeader(http.StatusNoContent)
					default:
						http.Error(w, "method", http.StatusMethodNotAllowed)
					}
				default:
					http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
				}
			}))
			defer srv.Close()
			t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
			t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
			if len(tc.wantWrites) != 0 {
				// Record the applied run as production does: the envelope must
				// keep the run ID that undoes a prune beside the membership summary.
				t.Setenv("HOME", t.TempDir())
				mutationJournalRecorder = recordMutationJournal
				t.Cleanup(func() { mutationJournalRecorder = nil })
			}
			args := append([]string{"SK", "--to", "TARGET"}, tc.args...)
			env := writePlaneTestMustRunMutationCmd(t, "searches materialize", newSearchesMaterializeCmd, &tc.flags, args...)
			var keys, kinds []string
			for _, op := range env.Plan.Operations {
				keys = append(keys, op.Key)
				kinds = append(kinds, op.Kind)
			}
			wantKeys := []string{"A"}
			if len(tc.wantKinds) == 2 {
				wantKeys = append(wantKeys, "C")
			}
			if !reflect.DeepEqual(keys, wantKeys) || !reflect.DeepEqual(kinds, tc.wantKinds) {
				t.Fatalf("plan keys/kinds = %v/%v, want %v/%v", keys, kinds, wantKeys, tc.wantKinds)
			}
			if len(kinds) == 2 {
				remove := env.Plan.Operations[1]
				if remove.Destructive || len(remove.Changes) != 1 || remove.Changes[0].Remove != "TARGET" {
					t.Errorf("removal not reversible like items move --from: %+v", remove)
				}
			}
			summary, ok := env.Journal.(map[string]any)
			if !ok || summary["unchanged_count"] != float64(1) || summary["stale_count"] != float64(1) || !reflect.DeepEqual(summary["stale_keys"], []any{"C"}) {
				t.Fatalf("membership summary = %+v", env.Journal)
			}
			if len(tc.wantKinds) == 1 && !strings.Contains(fmt.Sprint(summary["prune_hint"]), "--prune") {
				t.Errorf("missing prune instruction: %+v", summary)
			}
			if !reflect.DeepEqual(patches, tc.wantWrites) {
				t.Fatalf("PATCHes = %v, want %v", patches, tc.wantWrites)
			}
			if len(tc.wantWrites) != 0 && (env.Result == nil || env.Result.Summary.Applied != 2) {
				t.Fatalf("apply result = %+v", env.Result)
			}
			if len(tc.wantWrites) != 0 {
				if runID, _ := summary["run_id"].(string); runID == "" {
					t.Fatalf("journal = %+v, want the applied run_id kept beside the membership summary", summary)
				}
			}
		})
	}
}

func TestSearchesMaterializePruneOnlySingleOperationText(t *testing.T) {
	for _, tc := range []struct {
		name      string
		yes       bool
		wantText  string
		wantPatch int
	}{
		{"preview", false, "would remove C from TARGET", 0},
		{"apply", true, "removed C from TARGET", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var patches int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/users/0/searches/SK/items":
					_, _ = fmt.Fprint(w, `[{"key":"A","data":{"collections":["TARGET"]}}]`)
				case "/users/0/collections/TARGET/items":
					_, _ = fmt.Fprint(w, `[{"key":"A","data":{"collections":["TARGET"]}},{"key":"C","data":{"collections":["TARGET"]}}]`)
				case "/users/0/items/C":
					switch r.Method {
					case http.MethodGet:
						w.Header().Set("Last-Modified-Version", "12")
						_, _ = fmt.Fprint(w, `{"key":"C","data":{"collections":["TARGET"]}}`)
					case http.MethodPatch:
						patches++
						w.WriteHeader(http.StatusNoContent)
					}
				default:
					http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
				}
			}))
			defer srv.Close()
			t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
			t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
			env := writePlaneTestMustRunMutationCmd(t, "searches materialize", newSearchesMaterializeCmd,
				&rootFlags{asJSON: true, yes: tc.yes, maxChanges: -1}, "SK", "--to", "TARGET", "--prune")
			if len(env.Plan.Operations) != 1 || env.Plan.Operations[0].Key != "C" || env.Plan.Operations[0].Kind != "collection_remove" || patches != tc.wantPatch {
				t.Fatalf("plan=%+v patches=%d, want one C removal and %d writes", env.Plan, patches, tc.wantPatch)
			}
			if text := searchesMaterializeSingleLine("TARGET")(env); text != tc.wantText {
				t.Fatalf("terminal line = %q, want %q", text, tc.wantText)
			}
		})
	}
}

func TestSearchesMaterializeSkipsChildrenAndUnfiledCollectionRows(t *testing.T) {
	var patches int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			patches++
			w.WriteHeader(http.StatusNoContent)
			return
		}
		switch r.URL.Path {
		case "/users/0/searches/SK/items":
			_, _ = fmt.Fprint(w, `[{"key":"P","data":{"itemType":"book","collections":["TARGET"]}},{"key":"CH","data":{"itemType":"attachment","parentItem":"P","collections":[]}}]`)
		case "/users/0/collections/TARGET/items":
			_, _ = fmt.Fprint(w, `[{"key":"P","data":{"collections":["TARGET"]}},{"key":"G","data":{"itemType":"attachment","parentItem":"UNRELATED","collections":[]}}]`)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	env := writePlaneTestMustRunMutationCmd(t, "searches materialize", newSearchesMaterializeCmd,
		&rootFlags{asJSON: true, yes: true, maxChanges: -1}, "SK", "--to", "TARGET", "--prune")
	summary, ok := env.Journal.(map[string]any)
	if !ok || len(env.Plan.Operations) != 0 || summary["unchanged_count"] != float64(1) ||
		summary["stale_count"] != float64(0) || summary["skipped_child_count"] != float64(1) ||
		!reflect.DeepEqual(summary["skipped_child_keys"], []any{"CH"}) ||
		summary["skipped_child_hint"] != "Child items cannot be filed; include their parents in the saved search to file them." || patches != 0 {
		t.Fatalf("child-only rows must not become mutations: plan=%+v summary=%+v patches=%d", env.Plan, env.Journal, patches)
	}
}

func TestSearchesMaterializeRefusesUnsafePruneAndUnreadableCollection(t *testing.T) {
	fastRetryBackoff(t)
	for _, tc := range []struct {
		name, collection string
		search           []string
		prune            bool
		want             string
		code             int
	}{
		{"empty_search", `[{"key":"C","data":{"collections":["TARGET"]}}]`, nil, true, "pruning would empty collection", 9},
		{"collection_missing", "missing", []string{"A"}, false, "cannot read target collection", 3},
		{"collection_failure", "unavailable", []string{"A"}, true, "cannot read target collection", 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var patches int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPatch {
					patches++
					w.WriteHeader(http.StatusNoContent)
					return
				}
				switch r.URL.Path {
				case "/users/0/searches/SK/items":
					items := make([]map[string]string, 0, len(tc.search))
					for _, key := range tc.search {
						items = append(items, map[string]string{"key": key})
					}
					_, _ = fmt.Fprint(w, searchMaterializeJSON(t, items))
				case "/users/0/collections/TARGET/items":
					switch tc.collection {
					case "missing":
						http.Error(w, "not found", http.StatusNotFound)
					case "unavailable":
						http.Error(w, "unavailable", http.StatusServiceUnavailable)
					default:
						_, _ = fmt.Fprint(w, tc.collection)
					}
				default:
					http.Error(w, "unexpected", http.StatusNotFound)
				}
			}))
			defer srv.Close()
			t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
			t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
			cmd := newSearchesMaterializeCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1})
			cmd.SilenceErrors, cmd.SilenceUsage = true, true
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&bytes.Buffer{})
			args := []string{"SK", "--to", "TARGET"}
			if tc.prune {
				args = append(args, "--prune")
			}
			cmd.SetArgs(args)
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), tc.want) || ExitCode(err) != tc.code || patches != 0 || strings.Contains(out.String(), `"mode":"apply"`) {
				t.Fatalf("err=%v code=%d patches=%d output=%q; want refusal %q code %d and zero writes", err, ExitCode(err), patches, out.String(), tc.want, tc.code)
			}
		})
	}
}

func TestSearchesMaterializeEmptySearchReportsStaleWithoutPrune(t *testing.T) {
	srv := newSearchesMaterializeTestServer(t, nil, map[string]string{"C": "12"}, map[string][]string{"C": {"TARGET"}})
	env := writePlaneTestMustRunMutationCmd(t, "searches materialize", newSearchesMaterializeCmd, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "SK", "--to", "TARGET")
	summary, ok := env.Journal.(map[string]any)
	if !ok || !reflect.DeepEqual(summary["stale_keys"], []any{"C"}) || summary["stale_count"] != float64(1) || len(env.Plan.Operations) != 0 || srv.patchCounts["C"] != 0 {
		t.Fatalf("empty search must leave stale C alone, plan=%+v summary=%+v patches=%v", env.Plan, env.Journal, srv.patchCounts)
	}
}

func TestSearchesMaterializePruneEmptyCollectionIsSafe(t *testing.T) {
	newSearchesMaterializeTestServer(t, nil, map[string]string{}, map[string][]string{})
	env := writePlaneTestMustRunMutationCmd(t, "searches materialize", newSearchesMaterializeCmd, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "SK", "--to", "TARGET", "--prune")
	if !env.OK || env.Mode != "apply" || len(env.Plan.Operations) != 0 || env.Result == nil || env.Result.Summary.Applied != 0 {
		t.Fatalf("empty search and empty collection should allow an empty apply: %+v", env)
	}
}

func TestSearchesMaterializeCollectionPaginationAndChangeCap(t *testing.T) {
	collection := make([]string, zoteroPageMax+1)
	for i := range collection {
		collection[i] = fmt.Sprintf("C%03d", i)
	}
	var starts []int
	var patches int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			patches++
			w.WriteHeader(http.StatusNoContent)
			return
		}
		switch r.URL.Path {
		case "/users/0/searches/SK/items":
			_, _ = fmt.Fprint(w, `[{"key":"A"}]`)
		case "/users/0/collections/TARGET/items":
			start, _ := strconv.Atoi(r.URL.Query().Get("start"))
			starts = append(starts, start)
			if r.URL.Query().Get("limit") != "100" {
				t.Errorf("collection page limit = %s", r.URL.Query().Get("limit"))
			}
			end := start + zoteroPageMax
			if end > len(collection) {
				end = len(collection)
			}
			if start >= len(collection) {
				_, _ = fmt.Fprint(w, `[]`)
				return
			}
			items := make([]map[string]any, 0, end-start)
			for _, key := range collection[start:end] {
				items = append(items, map[string]any{"key": key, "data": map[string]any{"collections": []string{"TARGET"}}})
			}
			_, _ = fmt.Fprint(w, searchMaterializeJSON(t, items))
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	env := writePlaneTestMustRunMutationCmd(t, "searches materialize", newSearchesMaterializeCmd, &rootFlags{asJSON: true, dryRun: true, yes: true, maxChanges: -1}, "SK", "--to", "TARGET", "--prune")
	if env.Plan.Summary.Planned != zoteroPageMax+2 || !reflect.DeepEqual(starts, []int{0, zoteroPageMax}) {
		t.Fatalf("planned=%d collection starts=%v", env.Plan.Summary.Planned, starts)
	}
	// With --yes, the shared mutation gate checks adds and removals together.
	cmd := newSearchesMaterializeCmd(&rootFlags{asJSON: true, yes: true, maxChanges: 1})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"SK", "--to", "TARGET", "--prune"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "max_changes_exceeded") {
		t.Fatalf("expected combined change cap refusal, err=%v output=%q", err, out.String())
	}
	var refused mutation.Envelope
	if err := json.Unmarshal(out.Bytes(), &refused); err != nil {
		t.Fatalf("decode refused envelope %q: %v", out.String(), err)
	}
	if refused.OK || refused.Error == nil || refused.Error.Code != "max_changes_exceeded" {
		t.Fatalf("refused envelope = %+v, want max_changes_exceeded", refused)
	}
	if refused.Plan.Summary.Planned != zoteroPageMax+2 || len(refused.Plan.Operations) != zoteroPageMax+2 {
		t.Fatalf("refused plan = summary %+v len %d, want %d planned item writes", refused.Plan.Summary, len(refused.Plan.Operations), zoteroPageMax+2)
	}
	if patches != 0 {
		t.Fatalf("refused apply made %d PATCH request(s), want 0", patches)
	}
}

func TestSearchesMaterializeCollectionCrossPageRepeatRefuses(t *testing.T) {
	keys := make([]map[string]string, zoteroPageMax)
	for i := range keys {
		keys[i] = map[string]string{"key": fmt.Sprintf("C%03d", i)}
	}
	var collectionPages int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/0/searches/SK/items":
			_, _ = fmt.Fprint(w, `[{"key":"A"}]`)
		case "/users/0/collections/TARGET/items":
			collectionPages++
			_, _ = fmt.Fprint(w, searchMaterializeJSON(t, keys))
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	cmd := newSearchesMaterializeCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"SK", "--to", "TARGET", "--prune"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "pagination for collection TARGET ignored start 100") || collectionPages != 2 {
		t.Fatalf("repeating collection pages should refuse: err=%v pages=%d", err, collectionPages)
	}
}
