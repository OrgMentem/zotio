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
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"zotio/internal/client"
	"zotio/internal/config"
	"zotio/internal/store"
)

func TestParseScopeSpec(t *testing.T) {
	cases := []struct {
		expr     string
		wantType string
		wantVal  string
		wantErr  bool
	}{
		{"library", "library", "", false},
		{"collection:ABC", "collection", "ABC", false},
		{"tag:to-read", "tag", "to-read", false},
		{"item:XYZ", "item", "XYZ", false},
		{"query:psychological safety: a review", "query", "psychological safety: a review", false},
		{"saved-search:S1", "saved-search", "S1", false},
		{"bogus:x", "", "", true},
		{"collection:", "", "", true},
		{"nope", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			spec, err := parseScopeSpec(tc.expr)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseScopeSpec(%q) = %+v, want error", tc.expr, spec)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseScopeSpec(%q): %v", tc.expr, err)
			}
			if spec.Type != tc.wantType || spec.Value != tc.wantVal {
				t.Errorf("parseScopeSpec(%q) = {%q,%q}, want {%q,%q}", tc.expr, spec.Type, spec.Value, tc.wantType, tc.wantVal)
			}
		})
	}
}

func seedScopeStore(t *testing.T) localQueryStore {
	t.Helper()
	db, err := store.OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	items := []json.RawMessage{
		json.RawMessage(`{"key":"P1","version":1,"data":{"key":"P1","itemType":"journalArticle","title":"One","collections":["COLL1"],"tags":[{"tag":"AI"}]}}`),
		json.RawMessage(`{"key":"P2","version":1,"data":{"key":"P2","itemType":"journalArticle","title":"Two"}}`),
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return localQueryStore{db}
}

func TestResolveScope(t *testing.T) {
	db := seedScopeStore(t)

	t.Run("library", func(t *testing.T) {
		r, err := resolveScope(db, scopeSpec{Type: "library"})
		if err != nil {
			t.Fatal(err)
		}
		if !r.All {
			t.Errorf("library scope should set All=true, got %+v", r)
		}
	})

	t.Run("collection", func(t *testing.T) {
		r, err := resolveScope(db, scopeSpec{Type: "collection", Value: "COLL1"})
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Keys) != 1 || r.Keys[0] != "P1" {
			t.Errorf("collection:COLL1 keys = %v, want [P1]", r.Keys)
		}
	})

	t.Run("tag", func(t *testing.T) {
		r, err := resolveScope(db, scopeSpec{Type: "tag", Value: "AI"})
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Keys) != 1 || r.Keys[0] != "P1" {
			t.Errorf("tag:AI keys = %v, want [P1]", r.Keys)
		}
	})

	t.Run("item", func(t *testing.T) {
		r, err := resolveScope(db, scopeSpec{Type: "item", Value: "ZZ"})
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Keys) != 1 || r.Keys[0] != "ZZ" {
			t.Errorf("item:ZZ keys = %v, want [ZZ]", r.Keys)
		}
	})

	// resolveScope alone is the local layer: it only marks saved-search as
	// needing the live API. resolveScopeLive executes it (see below).
	t.Run("saved-search-precondition", func(t *testing.T) {
		r, err := resolveScope(db, scopeSpec{Type: "saved-search", Value: "S1"})
		if err != nil {
			t.Fatal(err)
		}
		if r.Precondition != "live_local_api" {
			t.Errorf("saved-search precondition = %q, want live_local_api", r.Precondition)
		}
		if len(r.Keys) != 0 {
			t.Errorf("saved-search should resolve no local keys, got %v", r.Keys)
		}
	})
}

// stubSavedSearchLocalAPI stands in for a reachable desktop local API: the
// saved-search read client targets an httptest server serving handler under
// /api/users/0. The plane gate itself is covered by the non-local case below
// and by TestCheckLiveLocalAPIPreconditionClassifiesTheAPIPlane.
func stubSavedSearchLocalAPI(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	oldAllow := allowPrivateOutboundForTests.Load()
	allowPrivateOutboundForTests.Store(true)
	t.Cleanup(func() { allowPrivateOutboundForTests.Store(oldAllow) })
	old := savedSearchLiveClient
	savedSearchLiveClient = func(context.Context, *rootFlags) (*client.Client, string, error) {
		c := client.New(&config.Config{BaseURL: ts.URL + "/api/users/0"}, time.Second, 0)
		c.NoCache = true
		return c, "", nil
	}
	t.Cleanup(func() { savedSearchLiveClient = old })
}

// savedSearchPages serves /searches/S1/items: 100 parents then two more, one
// child attachment mixed in on the second page.
func savedSearchPages(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/users/0/searches/S1/items" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if got := r.URL.Query().Get("limit"); got != "100" {
			t.Errorf("limit = %q, want 100", got)
		}
		rows := []map[string]any{}
		switch r.URL.Query().Get("start") {
		case "0":
			for i := range 100 {
				rows = append(rows, map[string]any{"key": fmt.Sprintf("K%03d", i), "data": map[string]any{}})
			}
		case "100":
			rows = append(rows,
				map[string]any{"key": "K100", "data": map[string]any{}},
				map[string]any{"key": "CHILD", "data": map[string]any{"parentItem": "K100"}},
				map[string]any{"key": "K101", "data": map[string]any{}},
			)
		}
		_ = json.NewEncoder(w).Encode(rows)
	}
}

func TestResolveScopeLiveSavedSearch(t *testing.T) {
	db := seedScopeStore(t)
	spec := scopeSpec{Type: "saved-search", Value: "S1"}

	t.Run("pages all keys in order", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		stubSavedSearchLocalAPI(t, savedSearchPages(t))
		flags := &rootFlags{}
		r, err := resolveScopeLive(context.Background(), flags, db, spec)
		if err != nil {
			t.Fatalf("resolveScopeLive: %v", err)
		}
		if r.Precondition != "" || r.Expr != "saved-search:S1" || r.Type != "saved-search" {
			t.Fatalf("result = %+v, want met precondition and saved-search:S1", r)
		}
		want := make([]string, 0, 102)
		for i := range 102 {
			want = append(want, fmt.Sprintf("K%03d", i))
		}
		if strings.Join(r.Keys, ",") != strings.Join(want, ",") {
			t.Fatalf("keys = %v (%d), want K000..K101 without the child", r.Keys, len(r.Keys))
		}
	})

	t.Run("non-local base refuses with envelope", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("ZOTIO_DEMO", "")
		t.Setenv("ZOTERO_BASE_URL", "")
		flags := &rootFlags{configPath: testConfigFile(t, "https://api.zotero.org/users/123"), asJSON: true}
		r, err := resolveScopeLive(context.Background(), flags, db, spec)
		if err != nil {
			t.Fatalf("resolveScopeLive: %v", err)
		}
		if r.Precondition != preconditionLiveLocalAPI {
			t.Fatalf("precondition = %q, want live_local_api", r.Precondition)
		}
		var out bytes.Buffer
		refusal := scopePreconditionErr(context.Background(), &out, flags, "library health", r)
		if code := ExitCode(refusal); code != 9 {
			t.Fatalf("exit code = %d (%v), want 9", code, refusal)
		}
		var env preconditionUnmetEnvelope
		if err := json.Unmarshal(out.Bytes(), &env); err != nil {
			t.Fatalf("envelope: %v\n%s", err, out.String())
		}
		if env.Kind != "precondition_unmet" || env.Precondition != preconditionLiveLocalAPI || !strings.Contains(env.Detail, "not the Zotero desktop local API") {
			t.Fatalf("envelope = %+v", env)
		}
	})

	t.Run("cross-page duplicate key errors", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		stubSavedSearchLocalAPI(t, func(w http.ResponseWriter, r *http.Request) {
			rows := make([]map[string]any, 0, 100)
			for i := range 100 {
				rows = append(rows, map[string]any{"key": fmt.Sprintf("K%03d", i)})
			}
			_ = json.NewEncoder(w).Encode(rows) // every start returns page one
		})
		if _, err := resolveScopeLive(context.Background(), &rootFlags{}, db, spec); err == nil || !strings.Contains(err.Error(), "duplicate key") {
			t.Fatalf("err = %v, want duplicate key refusal", err)
		}
	})

	t.Run("unknown search key names the key", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		stubSavedSearchLocalAPI(t, savedSearchPages(t))
		_, err := resolveScopeLive(context.Background(), &rootFlags{}, db, scopeSpec{Type: "saved-search", Value: "NOPE"})
		if err == nil || !strings.Contains(err.Error(), "saved search NOPE not found") {
			t.Fatalf("err = %v, want not-found naming NOPE", err)
		}
	})
}

func TestLibraryHealthScopeFiltersToCohort(t *testing.T) {
	db := seedHealthStore(t)
	// P1 is the bare article that triggers citekey_missing/missing_*; scope to it.
	scope, err := resolveScope(db, scopeSpec{Type: "item", Value: "P1"})
	if err != nil {
		t.Fatalf("resolveScope: %v", err)
	}
	report, err := assembleHealthReport(db, newHealthCtx("all", false), "all", healthPresets["all"], "", scope)
	if err != nil {
		t.Fatalf("assembleHealthReport: %v", err)
	}
	if report.Scope.Expr != "item:P1" {
		t.Errorf("scope expr = %q, want item:P1", report.Scope.Expr)
	}
	for _, f := range report.Findings {
		if f.ItemKey != "" && f.ItemKey != "P1" {
			t.Errorf("scoped run leaked a finding for %q (kind %s)", f.ItemKey, f.Kind)
		}
	}
	// The C1/C2 citekey_conflict (not in scope) must be filtered out.
	for _, f := range report.Findings {
		if f.Kind == "citekey_conflict" {
			t.Errorf("citekey_conflict (C1/C2) should be filtered out of an item:P1 scope")
		}
	}
}

// resolveScope's query path must enumerate the full match cohort, not the
// interactive Store.Search default of 50. Regression for the limit-convention
// collision (limit 0 meant "no limit" to resolveScope but 50 to Store.Search).
func TestResolveScopeQueryReturnsAllMatches(t *testing.T) {
	db, err := store.OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	const want = 60
	items := make([]json.RawMessage, 0, want)
	for i := range want {
		key := fmt.Sprintf("Q%03d", i)
		items = append(items, json.RawMessage(fmt.Sprintf(
			`{"key":%q,"version":1,"data":{"key":%q,"itemType":"journalArticle","title":"zqzquux corpus paper"}}`, key, key)))
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed: %v", err)
	}

	r, err := resolveScope(localQueryStore{db}, scopeSpec{Type: "query", Value: "zqzquux"})
	if err != nil {
		t.Fatalf("resolveScope: %v", err)
	}
	if len(r.Keys) != want {
		t.Errorf("query scope resolved %d keys, want %d (cohort must not be capped at 50)", len(r.Keys), want)
	}
}

// walkScopeFlags collects every --scope flag registered anywhere in the command
// tree, keyed by command path, so a new adopter is covered without being listed
// here by hand.
func walkScopeFlags(t *testing.T) map[string]*pflag.Flag {
	t.Helper()
	found := map[string]*pflag.Flag{}
	var walk func(*cobra.Command)
	walk = func(cmd *cobra.Command) {
		if f := cmd.Flags().Lookup("scope"); f != nil {
			found[cmd.CommandPath()] = f
		}
		for _, child := range cmd.Commands() {
			walk(child)
		}
	}
	walk(newRootCmd(&rootFlags{}))
	if len(found) == 0 {
		t.Fatal("no --scope flags found; the walk is wrong, not the tree")
	}
	return found
}

// One grammar means one help string. Four separately worded copies of this
// flag had already drifted apart (three named `library` but not
// `saved-search`, one the reverse, two used `collection:<key>` instead of
// `collection:KEY`), so a reader comparing two --help outputs saw two
// grammars. This is the gate that stops the fifth wording: a command may
// append command-specific truth the shared string cannot carry, but it may not
// restate the grammar in its own words.
func TestScopeFlagUsesTheOneCanonicalHelpString(t *testing.T) {
	for path, flag := range walkScopeFlags(t) {
		t.Run(path, func(t *testing.T) {
			switch {
			case flag.Usage == scopeFlagUsage,
				flag.Usage == scopeFlagUsageDefaultLibrary,
				flag.Usage == scopeFlagUsageRequired:
			case strings.HasPrefix(flag.Usage, scopeFlagUsage+" ("):
				// A command-specific suffix is allowed; a reworded grammar is not.
			default:
				t.Errorf("--scope usage = %q\nwant scopeFlagUsage (scope.go), optionally with a command-specific suffix appended", flag.Usage)
			}
		})
	}
}

// The two defaults are a real fork, not drift: a command that parses --scope
// unconditionally needs a parseable expression, and a command that reconciles
// an older selection flag against --scope needs to tell "unset" from
// "library". Any third default would be one of those two mislabelled.
func TestScopeFlagUsesACanonicalDefault(t *testing.T) {
	for path, flag := range walkScopeFlags(t) {
		t.Run(path, func(t *testing.T) {
			if flag.DefValue != scopeFlagDefaultLibrary && flag.DefValue != scopeFlagDefaultUnset {
				t.Errorf("--scope default = %q, want %q or %q (scope.go)", flag.DefValue, scopeFlagDefaultLibrary, scopeFlagDefaultUnset)
			}
			// A non-empty default has to parse: cobra hands it straight to
			// parseScopeSpec on a run that omits the flag.
			if flag.DefValue != "" {
				if _, err := parseScopeSpec(flag.DefValue); err != nil {
					t.Errorf("default %q does not parse: %v", flag.DefValue, err)
				}
			}
		})
	}
}

// Every arm the shared help string advertises must be an arm the parser
// accepts. A string that names a scope type parseScopeSpec rejects is a
// documented flag that cannot be used.
func TestScopeFlagUsageNamesOnlyRealArms(t *testing.T) {
	arms, ok := strings.CutPrefix(scopeFlagUsage, "Item cohort: ")
	if !ok {
		t.Fatalf("scopeFlagUsage = %q, want it to start with the cohort prefix", scopeFlagUsage)
	}
	seen := map[string]bool{}
	for _, arm := range strings.Split(arms, "|") {
		arm = strings.TrimSpace(arm)
		spec, err := parseScopeSpec(arm)
		if err != nil {
			t.Errorf("advertised arm %q is rejected by parseScopeSpec: %v", arm, err)
			continue
		}
		seen[spec.Type] = true
	}
	for _, want := range []string{"library", "collection", "tag", "item", "query", "saved-search"} {
		if !seen[want] {
			t.Errorf("scopeFlagUsage does not advertise the %q arm, which parseScopeSpec accepts", want)
		}
	}
}
