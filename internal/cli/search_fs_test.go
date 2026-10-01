// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zotio/internal/client"
	"zotio/internal/store"
)

// fsSeedScopedFulltextStore seeds three parents whose PDFs all mention
// "calibration". FTS rank orders them OUT > IN2 > IN1 (term frequency against
// document length), so the strongest hit is the one outside collection COLL.
func fsSeedScopedFulltextStore(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "search-scope.db")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := db.UpsertBatch("items", []json.RawMessage{
		json.RawMessage(`{"key":"IN1","version":1,"data":{"key":"IN1","itemType":"journalArticle","title":"Alpha Paper","collections":["COLL"],"tags":[{"tag":"to-read"}]}}`),
		json.RawMessage(`{"key":"IN2","version":1,"data":{"key":"IN2","itemType":"journalArticle","title":"Beta Paper","collections":["COLL"]}}`),
		json.RawMessage(`{"key":"OUT","version":1,"data":{"key":"OUT","itemType":"journalArticle","title":"Gamma Paper"}}`),
		json.RawMessage(`{"key":"AIN1","version":1,"data":{"key":"AIN1","itemType":"attachment","parentItem":"IN1","contentType":"application/pdf"}}`),
		json.RawMessage(`{"key":"AIN2","version":1,"data":{"key":"AIN2","itemType":"attachment","parentItem":"IN2","contentType":"application/pdf"}}`),
		json.RawMessage(`{"key":"AOUT","version":1,"data":{"key":"AOUT","itemType":"attachment","parentItem":"OUT","contentType":"application/pdf"}}`),
	}); err != nil {
		t.Fatalf("seed items: %v", err)
	}
	if _, err := db.UpsertKeyed("fulltext", []string{"AIN1", "AIN2", "AOUT"}, []json.RawMessage{
		json.RawMessage(`{"content":"A long methods section that mentions calibration once among many other unrelated words about sampling, results, discussion, limitations and future work."}`),
		json.RawMessage(`{"content":"calibration calibration drift in the second paper body"}`),
		json.RawMessage(`{"content":"calibration calibration calibration"}`),
	}); err != nil {
		t.Fatalf("seed full text: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	return dbPath
}

func fsRunSearch(t *testing.T, flags *rootFlags, args ...string) (string, error) {
	t.Helper()
	cmd := newSearchCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// fsSearchItemKeys runs a JSON full-text search and returns the parent item
// keys in output order.
func fsSearchItemKeys(t *testing.T, args ...string) []string {
	t.Helper()
	out, err := fsRunSearch(t, &rootFlags{asJSON: true, dataSource: "local", timeout: time.Second}, args...)
	if err != nil {
		t.Fatalf("search %v: %v (exit %d, out=%s)", args, err, ExitCode(err), out)
	}
	var got struct {
		Results []struct {
			ItemKey string `json:"item_key"`
		} `json:"results"`
		Meta struct {
			Source       string `json:"source"`
			ResourceType string `json:"resource_type"`
		} `json:"meta"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode search output %q: %v", out, err)
	}
	if got.Results == nil {
		t.Fatalf("search %v output %q has no results array", args, out)
	}
	if got.Meta.Source != "local" || got.Meta.ResourceType != "fulltext" {
		t.Fatalf("meta = %+v, want local fulltext provenance", got.Meta)
	}
	keys := make([]string, 0, len(got.Results))
	for _, r := range got.Results {
		keys = append(keys, r.ItemKey)
	}
	return keys
}

// The cohort filter must run before rank ordering and LIMIT. Filtering the
// global top hit afterwards would return nothing for --limit 1, because the
// best match in the whole library (OUT) is outside the collection.
func TestFsSearchFulltextScopeFiltersBeforeLimit(t *testing.T) {
	dbPath := fsSeedScopedFulltextStore(t)

	if got := fsSearchItemKeys(t, "calibration", "--fulltext", "--db", dbPath, "--limit", "1"); strings.Join(got, ",") != "OUT" {
		t.Fatalf("unscoped --limit 1 = %v, want [OUT] (the fixture's rank order is wrong)", got)
	}
	if got := fsSearchItemKeys(t, "calibration", "--fulltext", "--db", dbPath, "--scope", "collection:COLL", "--limit", "1"); strings.Join(got, ",") != "IN2" {
		t.Fatalf("--scope collection:COLL --limit 1 = %v, want [IN2], the best hit inside the collection", got)
	}
	if got := fsSearchItemKeys(t, "calibration", "--fulltext", "--db", dbPath, "--scope", "collection:COLL"); strings.Join(got, ",") != "IN2,IN1" {
		t.Fatalf("--scope collection:COLL = %v, want [IN2 IN1] in rank order", got)
	}
}

func TestFsSearchFulltextScopeArms(t *testing.T) {
	dbPath := fsSeedScopedFulltextStore(t)
	for _, tc := range []struct {
		scope string
		want  string
	}{
		{scope: "tag:to-read", want: "IN1"},
		{scope: "item:IN2", want: "IN2"},
		{scope: "query:Alpha", want: "IN1"},
		{scope: "library", want: "OUT,IN2,IN1"},
		// An empty cohort is an empty answer, never the unscoped one.
		{scope: "collection:NOSUCH", want: ""},
	} {
		t.Run(tc.scope, func(t *testing.T) {
			got := fsSearchItemKeys(t, "calibration", "--fulltext", "--db", dbPath, "--scope", tc.scope)
			if strings.Join(got, ",") != tc.want {
				t.Fatalf("--scope %s = %v, want [%s]", tc.scope, got, tc.want)
			}
		})
	}
}

func TestFsSearchScopeUsageErrors(t *testing.T) {
	dbPath := fsSeedScopedFulltextStore(t)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "without --fulltext", args: []string{"calibration", "--db", dbPath, "--scope", "collection:COLL"}, want: "--scope requires --fulltext"},
		{name: "unknown arm", args: []string{"calibration", "--fulltext", "--db", dbPath, "--scope", "bogus:X"}, want: `unknown scope type "bogus"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := fsRunSearch(t, &rootFlags{asJSON: true, dataSource: "local"}, tc.args...)
			if err == nil {
				t.Fatalf("search accepted %v and printed %q", tc.args, out)
			}
			if ExitCode(err) != 2 || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v (exit %d), want usage error containing %q", err, ExitCode(err), tc.want)
			}
		})
	}
}

// saved-search:KEY has no local mirror. With the desktop API unavailable the
// search refuses (exit 9) instead of answering for the whole library; with it
// available the search covers exactly the saved search's items.
func TestFsSearchFulltextSavedSearchScope(t *testing.T) {
	dbPath := fsSeedScopedFulltextStore(t)
	t.Setenv("HOME", t.TempDir())

	t.Run("unavailable", func(t *testing.T) {
		old := savedSearchLiveClient
		savedSearchLiveClient = func(context.Context, *rootFlags) (*client.Client, string, error) {
			return nil, "Zotero desktop local API is not reachable", nil
		}
		t.Cleanup(func() { savedSearchLiveClient = old })

		out, err := fsRunSearch(t, &rootFlags{asJSON: true, dataSource: "local", timeout: time.Second},
			"calibration", "--fulltext", "--db", dbPath, "--scope", "saved-search:SS1")
		if err == nil {
			t.Fatalf("saved-search scope resolved without the live local API: %s", out)
		}
		if ExitCode(err) != 9 {
			t.Fatalf("ExitCode = %d (%v), want 9 (precondition)", ExitCode(err), err)
		}
		var env preconditionUnmetEnvelope
		if derr := json.Unmarshal([]byte(out), &env); derr != nil {
			t.Fatalf("decode envelope %q: %v", out, derr)
		}
		if env.Kind != "precondition_unmet" || env.Precondition != preconditionLiveLocalAPI {
			t.Fatalf("envelope = %+v, want precondition_unmet for %s", env, preconditionLiveLocalAPI)
		}
		if strings.Contains(out, `"item_key"`) {
			t.Fatalf("refusal also printed search results: %s", out)
		}
	})

	t.Run("available", func(t *testing.T) {
		stubSavedSearchLocalAPI(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/users/0/searches/SS1/items" {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(`[{"key":"IN1","data":{}}]`))
		})
		got := fsSearchItemKeys(t, "calibration", "--fulltext", "--db", dbPath, "--scope", "saved-search:SS1", "--limit", "1")
		if strings.Join(got, ",") != "IN1" {
			t.Fatalf("--scope saved-search:SS1 --limit 1 = %v, want [IN1]", got)
		}
	})
}

// Under --group all each library resolves the cohort against its own mirror.
// Both libraries hold a collection keyed COLL; resolving the cohort once and
// reusing its keys would drop the group's member and keep nothing there.
func TestFsSearchFulltextScopeGroupAllIsolation(t *testing.T) {
	fx := newFanoutFixture(t, "")
	dataDir := isolateFanoutEnv(t, fx.server.URL+"/users/0")
	synced := time.Now().Add(-time.Hour).Truncate(time.Second)
	seedFanoutLibraryStore(t, dataDir, "data.db", synced, []json.RawMessage{
		json.RawMessage(`{"key":"P1","version":1,"data":{"key":"P1","itemType":"journalArticle","title":"Personal In","collections":["COLL"]}}`),
		json.RawMessage(`{"key":"P2","version":1,"data":{"key":"P2","itemType":"journalArticle","title":"Personal Out"}}`),
		json.RawMessage(`{"key":"PA1","version":1,"data":{"key":"PA1","itemType":"attachment","parentItem":"P1"}}`),
		json.RawMessage(`{"key":"PA2","version":1,"data":{"key":"PA2","itemType":"attachment","parentItem":"P2"}}`),
	})
	seedFanoutLibraryStore(t, dataDir, "data-group-99.db", synced, []json.RawMessage{
		json.RawMessage(`{"key":"L1","version":1,"data":{"key":"L1","itemType":"book","title":"Lab In","collections":["COLL"]}}`),
		json.RawMessage(`{"key":"L2","version":1,"data":{"key":"L2","itemType":"book","title":"Lab Out"}}`),
		json.RawMessage(`{"key":"LA1","version":1,"data":{"key":"LA1","itemType":"attachment","parentItem":"L1"}}`),
		json.RawMessage(`{"key":"LA2","version":1,"data":{"key":"LA2","itemType":"attachment","parentItem":"L2"}}`),
	})
	for dbFile, attachments := range map[string][]string{"data.db": {"PA1", "PA2"}, "data-group-99.db": {"LA1", "LA2"}} {
		db, err := store.OpenWithContext(context.Background(), filepath.Join(dataDir, dbFile))
		if err != nil {
			t.Fatalf("open %s: %v", dbFile, err)
		}
		if _, err := db.UpsertKeyed("fulltext", attachments, []json.RawMessage{
			json.RawMessage(`{"content":"calibration passage"}`),
			json.RawMessage(`{"content":"calibration passage"}`),
		}); err != nil {
			t.Fatalf("seed full text in %s: %v", dbFile, err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close %s: %v", dbFile, err)
		}
	}

	out, _, err := runFanoutCmd(t, "search", "calibration", "--fulltext", "--scope", "collection:COLL", "--group", "all", "--json", "--data-source", "local")
	if err != nil {
		t.Fatalf("search --fulltext --scope --group all: %v (out=%s)", err, out)
	}
	report := decodeFanoutReport(t, out)
	got := map[string]string{}
	for _, raw := range report.Results {
		var row struct {
			ItemKey string        `json:"item_key"`
			Library fanoutLibrary `json:"library"`
		}
		if err := json.Unmarshal(raw, &row); err != nil {
			t.Fatalf("decode aggregated row %s: %v", raw, err)
		}
		got[row.ItemKey] = row.Library.ID
	}
	if len(got) != 2 || got["P1"] != "0" || got["L1"] != "99" {
		t.Fatalf("aggregated hits = %v, want P1 from library 0 and L1 from library 99 only", got)
	}
}
