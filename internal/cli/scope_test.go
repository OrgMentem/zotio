// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
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

// seedScopeKeysStore mirrors one bare journal article per key.
func seedScopeKeysStore(t *testing.T, keys ...string) localQueryStore {
	t.Helper()
	db, err := store.OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	items := make([]json.RawMessage, 0, len(keys))
	for _, key := range keys {
		items = append(items, json.RawMessage(fmt.Sprintf(`{"key":%q,"version":1,"data":{"key":%q,"itemType":"journalArticle","title":"T"}}`, key, key)))
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
		return client.New(&config.Config{BaseURL: ts.URL + "/api/users/0"}, time.Second, 0), "", nil
	}
	t.Cleanup(func() { savedSearchLiveClient = old })
}

// routeLocalAPIToHandler runs the REAL saved-search client (plane gate,
// reachability probe, response cache) against handler: the configured base is
// the desktop local API and every dial to port 23119 lands on an httptest
// server, so nothing binds the port a running Zotero owns. HOME is a fresh
// temp dir, so the response cache is enabled and empty.
func routeLocalAPIToHandler(t *testing.T, handler http.Handler) {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	fixture := ts.Listener.Addr().String()
	dialer := &net.Dialer{Timeout: time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			switch addr {
			case "localhost:23119", "127.0.0.1:23119", "[::1]:23119":
				return dialer.DialContext(ctx, network, fixture)
			default:
				return nil, fmt.Errorf("scope test refused a dial to %s", addr)
			}
		},
	}
	oldTransport := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() {
		http.DefaultTransport = oldTransport
		transport.CloseIdleConnections()
	})
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTIO_DEMO", "0")
	t.Setenv("ZOTERO_BASE_URL", "http://localhost:23119/api/users/0")
	t.Setenv("ZOTERO_API_KEY", "")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
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
		want := make([]string, 0, 102)
		for i := range 102 {
			want = append(want, fmt.Sprintf("K%03d", i))
		}
		flags := &rootFlags{}
		r, err := resolveScopeLive(context.Background(), flags, seedScopeKeysStore(t, want...), spec)
		if err != nil {
			t.Fatalf("resolveScopeLive: %v", err)
		}
		if r.Precondition != "" || r.Expr != "saved-search:S1" || r.Type != "saved-search" {
			t.Fatalf("result = %+v, want met precondition and saved-search:S1", r)
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

	// A live member the mirror lacks would be skipped by every mirror-based
	// adopter; `library health` could then pass over items it never read.
	t.Run("unsynced members refuse with exit 12", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		stubSavedSearchLocalAPI(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`[{"key":"P1","data":{}},{"key":"MISS1","data":{}},{"key":"TR1","data":{}}]`))
		})
		// TR1 is mirrored only as trashed: no adopter reads items-trash, so
		// it counts as missing.
		if _, _, err := db.UpsertBatch("items-trash", []json.RawMessage{
			json.RawMessage(`{"key":"TR1","version":2,"data":{"key":"TR1","itemType":"journalArticle","title":"Trashed","deleted":1}}`),
		}); err != nil {
			t.Fatalf("seed trash: %v", err)
		}
		r, err := resolveScopeLive(context.Background(), &rootFlags{}, db, spec)
		if code := ExitCode(err); err == nil || code != 12 {
			t.Fatalf("result = %+v, err = %v (exit %d), want freshness refusal exit 12", r, err, code)
		}
		if msg := err.Error(); !strings.Contains(msg, "returns 2 item(s)") || !strings.Contains(msg, "MISS1") || !strings.Contains(msg, "TR1") {
			t.Fatalf("err = %q, want the 2 unsynced keys MISS1 and TR1", msg)
		}
	})

	// items bibliography resolves with no mirror and renders live data, so
	// the mirror check must not refuse it.
	t.Run("no mirror resolves unsynced members", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		stubSavedSearchLocalAPI(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`[{"key":"MISS1","data":{}}]`))
		})
		r, err := resolveScopeLive(context.Background(), &rootFlags{}, localQueryStore{}, spec)
		if err != nil || strings.Join(r.Keys, ",") != "MISS1" {
			t.Fatalf("result = %+v, err = %v, want MISS1 with no mirror", r, err)
		}
	})
}

// The membership read must bypass the response cache: a saved search changes
// whenever an item starts or stops matching, and a cached page up to 5
// minutes old let `creators rename` plan writes against a former cohort.
func TestResolveScopeLiveSavedSearchMembershipIsNotCached(t *testing.T) {
	var members atomic.Value
	members.Store(`[{"key":"OLD1","data":{}}]`)
	var reads atomic.Int32
	routeLocalAPIToHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/users/0/searches/S1/items" {
			reads.Add(1)
			_, _ = w.Write([]byte(members.Load().(string)))
			return
		}
		_, _ = w.Write([]byte(`{}`)) // reachability probe
	}))
	spec := scopeSpec{Type: "saved-search", Value: "S1"}
	flags := &rootFlags{timeout: 5 * time.Second}

	first, err := resolveScopeLive(context.Background(), flags, localQueryStore{}, spec)
	if err != nil || first.Precondition != "" || strings.Join(first.Keys, ",") != "OLD1" {
		t.Fatalf("first resolve = %+v, err = %v, want OLD1", first, err)
	}
	members.Store(`[{"key":"NEW1","data":{}}]`)
	second, err := resolveScopeLive(context.Background(), flags, localQueryStore{}, spec)
	if err != nil || strings.Join(second.Keys, ",") != "NEW1" {
		t.Fatalf("second resolve = %+v, err = %v (membership reads %d), want NEW1 from a fresh read", second, err, reads.Load())
	}
}

// An MCP resource passes nil flags. The probe and the paged read must still
// honor the request context: a canceled request against a stalled desktop
// returns at once instead of hanging the MCP server.
func TestResolveScopeLiveNilFlagsHonorsRequestContext(t *testing.T) {
	release := make(chan struct{})
	routeLocalAPIToHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	// Registered after the server, so it runs first and unblocks handlers
	// before ts.Close waits for them.
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	type outcome struct {
		r   scopeResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		r, err := resolveScopeLive(ctx, nil, localQueryStore{}, scopeSpec{Type: "saved-search", Value: "S1"})
		done <- outcome{r, err}
	}()
	select {
	case got := <-done:
		if got.err != nil || got.r.Precondition != preconditionLiveLocalAPI {
			t.Fatalf("result = %+v, err = %v, want unmet live_local_api", got.r, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("resolveScopeLive with nil flags ignored the canceled request context")
	}
}

// Zotero answers 403 while "Allow other applications" is off. That is the
// live_local_api precondition (exit 9 with remediation), not a bare exit 1.
func TestResolveScopeLiveSavedSearchForbiddenIsUnmetPrecondition(t *testing.T) {
	routeLocalAPIToHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Local API is not enabled", http.StatusForbidden)
	}))
	flags := &rootFlags{timeout: 5 * time.Second}
	r, err := resolveScopeLive(context.Background(), flags, localQueryStore{}, scopeSpec{Type: "saved-search", Value: "S1"})
	if err != nil {
		t.Fatalf("resolveScopeLive: %v (exit %d), want unmet precondition", err, ExitCode(err))
	}
	if r.Precondition != preconditionLiveLocalAPI || !strings.Contains(r.PreconditionDetail, `"Allow other applications on this computer to communicate with Zotero"`) {
		t.Fatalf("result = %+v, want live_local_api naming the Allow other applications setting", r)
	}
	if code := ExitCode(scopePreconditionErr(context.Background(), nil, flags, "library health", r)); code != 9 {
		t.Fatalf("refusal exit = %d, want 9", code)
	}
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
	if !strings.HasPrefix(scopeFlagUsage, "Item cohort: "+scopeArmsUsage) {
		t.Fatalf("scopeFlagUsage = %q, want it to start with the cohort prefix and scopeArmsUsage", scopeFlagUsage)
	}
	seen := map[string]bool{}
	for _, arm := range strings.Split(scopeArmsUsage, "|") {
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

// The help string documents AND, so its own example has to be an expression
// the parser reads as an intersection of the two arms it names.
func TestScopeFlagUsageDocumentsAnd(t *testing.T) {
	const example = "collection:KEY AND tag:NAME"
	if !strings.Contains(scopeFlagUsage, "AND") || !strings.Contains(scopeFlagUsage, example) {
		t.Fatalf("scopeFlagUsage = %q, want it to document AND with the example %q", scopeFlagUsage, example)
	}
	spec, err := parseScopeSpec(example)
	if err != nil {
		t.Fatalf("parseScopeSpec(%q): %v", example, err)
	}
	if spec.Type != scopeTypeIntersection || len(spec.Arms) != 2 {
		t.Fatalf("parseScopeSpec(%q) = %+v, want a two-arm intersection", example, spec)
	}
}

func TestParseScopeSpecIntersection(t *testing.T) {
	cases := []struct {
		expr     string
		wantType string
		wantVal  string
		wantArms []string
		wantErr  string
	}{
		// Single arms parse exactly as they always did.
		{expr: "collection:C", wantType: "collection", wantVal: "C"},
		{expr: "library", wantType: "library"},
		{expr: "query:psychological safety: a review", wantType: "query", wantVal: "psychological safety: a review"},
		// AND only splits before a recognized arm; everywhere else it is text.
		{expr: "query:trust AND safety", wantType: "query", wantVal: "trust AND safety"},
		{expr: "tag:rock AND roll", wantType: "tag", wantVal: "rock AND roll"},
		{expr: "tag:rock and roll", wantType: "tag", wantVal: "rock and roll"},
		{expr: "tag:ANDROID", wantType: "tag", wantVal: "ANDROID"},
		// Intersections.
		{expr: "collection:C AND tag:T", wantType: scopeTypeIntersection, wantArms: []string{"collection:C", "tag:T"}},
		{expr: "  collection:C   AND   tag:T  ", wantType: scopeTypeIntersection, wantArms: []string{"collection:C", "tag:T"}},
		{expr: "query:trust AND safety AND tag:x", wantType: scopeTypeIntersection, wantArms: []string{"query:trust AND safety", "tag:x"}},
		{expr: "query:psychological safety: a review AND collection:C", wantType: scopeTypeIntersection, wantArms: []string{"query:psychological safety: a review", "collection:C"}},
		{expr: "collection:C AND tag:T AND item:K AND saved-search:S", wantType: scopeTypeIntersection, wantArms: []string{"collection:C", "tag:T", "item:K", "saved-search:S"}},
		{expr: "tag:x AND tag:x", wantType: scopeTypeIntersection, wantArms: []string{"tag:x", "tag:x"}},
		// Errors.
		{expr: "library AND tag:x", wantErr: "cannot combine library"},
		{expr: "tag:x AND library", wantErr: "cannot combine library"},
		{expr: "collection:C AND tag:", wantErr: `scope "tag" requires a value`},
		{expr: "collection: AND tag:T", wantErr: `scope "collection" requires a value`},
		{expr: "collection:C AND AND tag:T", wantErr: "empty operand"},
		{expr: "bogus:x AND tag:y", wantErr: `unknown scope type "bogus"`},
	}
	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			spec, err := parseScopeSpec(tc.expr)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parseScopeSpec(%q) = %+v, %v, want error containing %q", tc.expr, spec, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseScopeSpec(%q): %v", tc.expr, err)
			}
			if spec.Type != tc.wantType || spec.Value != tc.wantVal {
				t.Fatalf("parseScopeSpec(%q) = {%q,%q}, want {%q,%q}", tc.expr, spec.Type, spec.Value, tc.wantType, tc.wantVal)
			}
			var arms []string
			for _, arm := range spec.Arms {
				arms = append(arms, arm.expr())
			}
			if strings.Join(arms, "|") != strings.Join(tc.wantArms, "|") {
				t.Fatalf("parseScopeSpec(%q) arms = %q, want %q", tc.expr, arms, tc.wantArms)
			}
		})
	}
}

// seedIntersectionStore mirrors a cohort where every set operation has a
// distinct answer:
//
//	K1: C1        T1 T2   title "zqfoo"
//	K2: C1 C2     T1      title "zqfoo"
//	K3:    C2     T1 T2
//	K4: C1                title "zqfoo"
//	K5:                   (in no collection, no tag)
func seedIntersectionStore(t *testing.T) localQueryStore {
	t.Helper()
	db, err := store.OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	items := []json.RawMessage{
		json.RawMessage(`{"key":"K1","version":1,"data":{"key":"K1","itemType":"journalArticle","title":"zqfoo one","collections":["C1"],"tags":[{"tag":"T1"},{"tag":"T2"}]}}`),
		json.RawMessage(`{"key":"K2","version":1,"data":{"key":"K2","itemType":"journalArticle","title":"zqfoo two","collections":["C1","C2"],"tags":[{"tag":"T1"}]}}`),
		json.RawMessage(`{"key":"K3","version":1,"data":{"key":"K3","itemType":"journalArticle","title":"other three","collections":["C2"],"tags":[{"tag":"T1"},{"tag":"T2"}]}}`),
		json.RawMessage(`{"key":"K4","version":1,"data":{"key":"K4","itemType":"journalArticle","title":"zqfoo four","collections":["C1"]}}`),
		json.RawMessage(`{"key":"K5","version":1,"data":{"key":"K5","itemType":"journalArticle","title":"other five"}}`),
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return localQueryStore{db}
}

func mustParseScope(t *testing.T, expr string) scopeSpec {
	t.Helper()
	spec, err := parseScopeSpec(expr)
	if err != nil {
		t.Fatalf("parseScopeSpec(%q): %v", expr, err)
	}
	return spec
}

func TestResolveScopeIntersection(t *testing.T) {
	db := seedIntersectionStore(t)
	cases := []struct {
		expr string
		want string
	}{
		{"collection:C1 AND tag:T1", "K1,K2"},
		{"tag:T1 AND collection:C1", "K1,K2"},
		{"collection:C1 AND collection:C2", "K2"},
		{"collection:C1 AND tag:T1 AND tag:T2", "K1"},
		{"collection:C2 AND tag:T1", "K2,K3"},
		{"query:zqfoo AND tag:T1", "K1,K2"},
		{"collection:C1 AND query:zqfoo", "K1,K2,K4"},
		{"item:K2 AND tag:T1", "K2"},
		{"item:K4 AND tag:T1", ""},
		{"collection:C1 AND tag:NOPE", ""},
		// A repeated operand changes nothing.
		{"collection:C1 AND tag:T1 AND collection:C1 AND tag:T1", "K1,K2"},
		{"tag:T1 AND tag:T1", "K1,K2,K3"},
	}
	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			r, err := resolveScope(db, mustParseScope(t, tc.expr))
			if err != nil {
				t.Fatalf("resolveScope: %v", err)
			}
			if r.Precondition != "" || r.All {
				t.Fatalf("result = %+v, want a concrete key set", r)
			}
			if got := strings.Join(r.Keys, ","); got != tc.want {
				t.Fatalf("keys = %q, want %q", got, tc.want)
			}
			if r.Keys == nil {
				t.Fatal("Keys is nil, want a non-nil empty slice for an empty intersection")
			}
		})
	}

	t.Run("provenance records the full expression and every arm", func(t *testing.T) {
		r, err := resolveScope(db, mustParseScope(t, "  collection:C1   AND tag:T1 AND collection:C1 "))
		if err != nil {
			t.Fatalf("resolveScope: %v", err)
		}
		if r.Expr != "collection:C1 AND tag:T1 AND collection:C1" || r.Type != scopeTypeIntersection {
			t.Fatalf("result = %+v, want the canonical full expression and intersection type", r)
		}
		if got := strings.Join(r.Arms, "|"); got != "collection:C1|tag:T1|collection:C1" {
			t.Fatalf("arms = %q, want every arm as written", got)
		}
	})

	t.Run("a single arm keeps its old result", func(t *testing.T) {
		r, err := resolveScope(db, mustParseScope(t, "collection:C1"))
		if err != nil {
			t.Fatalf("resolveScope: %v", err)
		}
		if r.Expr != "collection:C1" || r.Type != "collection" || r.Arms != nil {
			t.Fatalf("result = %+v, want the plain collection result with no arms", r)
		}
	})

	t.Run("a hand-built intersection with library is refused", func(t *testing.T) {
		spec := scopeSpec{Type: scopeTypeIntersection, Arms: []scopeSpec{{Type: "library"}, {Type: "tag", Value: "T1"}}}
		if _, err := resolveScope(db, spec); err == nil {
			t.Fatal("intersection with a library operand resolved")
		}
	})
}

// A saved search nobody can evaluate is not an empty set. Intersecting it with
// anything would otherwise report a clean empty cohort, and a health gate over
// it would pass vacuously.
func TestResolveScopeIntersectionSavedSearchOperandNeverEmpty(t *testing.T) {
	db := seedIntersectionStore(t)

	t.Run("local resolver marks the precondition", func(t *testing.T) {
		r, err := resolveScope(db, mustParseScope(t, "collection:C1 AND saved-search:S1"))
		if err != nil {
			t.Fatalf("resolveScope: %v", err)
		}
		if r.Precondition != preconditionLiveLocalAPI || len(r.Keys) != 0 || r.Expr != "collection:C1 AND saved-search:S1" {
			t.Fatalf("result = %+v, want live_local_api unmet over the full expression", r)
		}
	})

	t.Run("non-local base refuses with exit 9 even when the other operand is empty", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("ZOTIO_DEMO", "")
		t.Setenv("ZOTERO_BASE_URL", "")
		flags := &rootFlags{configPath: testConfigFile(t, "https://api.zotero.org/users/123"), asJSON: true}
		for _, expr := range []string{"collection:C1 AND saved-search:S1", "tag:NOPE AND saved-search:S1", "saved-search:S1 AND tag:NOPE"} {
			r, err := resolveScopeLive(context.Background(), flags, db, mustParseScope(t, expr))
			if err != nil {
				t.Fatalf("%s: resolveScopeLive: %v", expr, err)
			}
			if r.Precondition != preconditionLiveLocalAPI || len(r.Keys) != 0 {
				t.Fatalf("%s: result = %+v, want unmet live_local_api and no keys", expr, r)
			}
			refusal := scopePreconditionErr(context.Background(), nil, flags, "library health", r)
			if code := ExitCode(refusal); code != 9 {
				t.Fatalf("%s: refusal exit = %d (%v), want 9", expr, code, refusal)
			}
			if !strings.Contains(refusal.Error(), expr) {
				t.Fatalf("%s: refusal = %q, want it to name the whole expression", expr, refusal)
			}
		}
	})

	t.Run("forbidden desktop API is the same precondition", func(t *testing.T) {
		routeLocalAPIToHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "Local API is not enabled", http.StatusForbidden)
		}))
		flags := &rootFlags{timeout: 5 * time.Second}
		r, err := resolveScopeLive(context.Background(), flags, db, mustParseScope(t, "tag:T1 AND saved-search:S1"))
		if err != nil || r.Precondition != preconditionLiveLocalAPI || r.PreconditionDetail == "" {
			t.Fatalf("result = %+v, err = %v, want unmet live_local_api with its detail", r, err)
		}
	})

	t.Run("a live saved search intersects with the local arms", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		stubSavedSearchLocalAPI(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`[{"key":"K1","data":{}},{"key":"K3","data":{}},{"key":"K4","data":{}}]`))
		})
		r, err := resolveScopeLive(context.Background(), &rootFlags{}, db, mustParseScope(t, "collection:C1 AND saved-search:S1"))
		if err != nil {
			t.Fatalf("resolveScopeLive: %v", err)
		}
		if r.Precondition != "" || strings.Join(r.Keys, ",") != "K1,K4" {
			t.Fatalf("result = %+v, want K1,K4 (C1 members the search also returns)", r)
		}
	})

	t.Run("an unknown saved search is an error, not an empty operand", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		stubSavedSearchLocalAPI(t, savedSearchPages(t))
		_, err := resolveScopeLive(context.Background(), &rootFlags{}, db, mustParseScope(t, "tag:T1 AND saved-search:NOPE"))
		if err == nil || !strings.Contains(err.Error(), "saved search NOPE not found") {
			t.Fatalf("err = %v, want not-found naming NOPE", err)
		}
	})
}

func TestReconcileScopeFlagsWithIntersection(t *testing.T) {
	coll := scopeSugarFor("collection", "collection", "C1")
	tag := scopeSugarFor("tag", "tag", "T1")

	// A legacy flag that names one arm of the intersection agrees with it:
	// the scope already requires that cohort.
	got, err := reconcileScopeFlags("collection:C1 AND tag:T1", coll, tag)
	if err != nil || got != "collection:C1 AND tag:T1" {
		t.Fatalf("reconcile = %q, %v, want the intersection back with both flags agreeing", got, err)
	}

	// A legacy flag outside the arms still disagrees, and the error says how to fix it.
	_, err = reconcileScopeFlags("collection:C1 AND query:foo", coll, tag)
	if err == nil || ExitCode(err) != 2 {
		t.Fatalf("err = %v (exit %d), want a usage error for --tag outside the arms", err, ExitCode(err))
	}
	if !strings.Contains(err.Error(), "--tag and --scope disagree") || !strings.Contains(err.Error(), "name tag:T1 as an arm of --scope") {
		t.Fatalf("err = %q, want the disagreement naming the arm to add", err)
	}

	// A single-arm scope behaves as before: only an exact match agrees.
	if _, err := reconcileScopeFlags("collection:C2", coll); err == nil {
		t.Fatal("--collection C1 agreed with --scope collection:C2")
	}
	if got, err := reconcileScopeFlags("collection:C1", coll); err != nil || got != "collection:C1" {
		t.Fatalf("reconcile = %q, %v, want exact match accepted", got, err)
	}

	// An expression that does not parse has no arms, so a flag never agrees with it.
	if _, err := reconcileScopeFlags("collection:C1 AND library", coll); err == nil {
		t.Fatal("--collection agreed with an unparseable --scope")
	}
}

// The consumer-visible case: `items audit --scope 'collection:A AND tag:read'`
// counts and lists only the items in both, not either set alone.
func TestItemsAuditScopeIntersectionCountsOnlyTheIntersection(t *testing.T) {
	auditIsolateEnv(t, "")
	auditSeedDB(t, []json.RawMessage{
		json.RawMessage(`{"key":"BOTH1","version":1,"data":{"key":"BOTH1","itemType":"journalArticle","title":"Alpha","dateAdded":"2020-01-01T00:00:00Z","collections":["COLA"],"tags":[{"tag":"read"}]}}`),
		json.RawMessage(`{"key":"BOTH2","version":1,"data":{"key":"BOTH2","itemType":"journalArticle","title":"Beta","dateAdded":"2020-01-02T00:00:00Z","collections":["COLA","COLB"],"tags":[{"tag":"read"}]}}`),
		json.RawMessage(`{"key":"COLONLY","version":1,"data":{"key":"COLONLY","itemType":"journalArticle","title":"Gamma","dateAdded":"2020-01-03T00:00:00Z","collections":["COLA"]}}`),
		json.RawMessage(`{"key":"TAGONLY","version":1,"data":{"key":"TAGONLY","itemType":"journalArticle","title":"Delta","dateAdded":"2020-01-04T00:00:00Z","collections":["COLB"],"tags":[{"tag":"read"}]}}`),
		json.RawMessage(`{"key":"NEITHER","version":1,"data":{"key":"NEITHER","itemType":"journalArticle","title":"Epsilon","dateAdded":"2030-01-01T00:00:00Z"}}`),
	})
	flags := &rootFlags{asJSON: true, timeout: 2 * time.Second}

	summaryOut, _, err := runItemsAudit(t, flags, "--scope", "collection:COLA AND tag:read")
	if err != nil {
		t.Fatalf("intersection summary: %v", err)
	}
	var summary itemsAuditSummary
	if err := json.Unmarshal([]byte(summaryOut), &summary); err != nil {
		t.Fatalf("decode summary %q: %v", summaryOut, err)
	}
	if summary.TopLevelItems != 2 || summary.MissingDOI != 2 || summary.MissingTags != 0 {
		t.Fatalf("summary = %+v, want top_level_items 2, missing_doi 2, missing_tags 0 (BOTH1, BOTH2 only)", summary)
	}

	out, _, err := runItemsAudit(t, flags, "--missing-doi", "--scope", "collection:COLA AND tag:read", "--limit", "1")
	if err != nil {
		t.Fatalf("intersection list: %v", err)
	}
	rows := decodeAuditSlice(t, decodeAuditMap(t, out)["missing_doi"])
	if len(rows) != 1 {
		t.Fatalf("limited rows = %d, want 1", len(rows))
	}
	out, _, err = runItemsAudit(t, flags, "--missing-doi", "--scope", "collection:COLA AND tag:read")
	if err != nil {
		t.Fatalf("intersection list: %v", err)
	}
	got := auditRowKeys(t, decodeAuditSlice(t, decodeAuditMap(t, out)["missing_doi"]))
	if len(got) != 2 || !got["BOTH1"] || !got["BOTH2"] {
		t.Fatalf("missing_doi keys = %v, want {BOTH1, BOTH2}", got)
	}

	_, _, err = runItemsAudit(t, flags, "--scope", "library AND tag:read")
	if err == nil || ExitCode(err) != 2 || !strings.Contains(err.Error(), "cannot combine library") {
		t.Fatalf("library AND tag: err = %v (exit %d), want a usage error naming the library rule", err, ExitCode(err))
	}
}
