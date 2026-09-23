// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"zotio/internal/store"
)

// schemaServer serves the global Zotero schema endpoints from the given sets.
// itemTypes/itemFields/creatorFields are slices of bare names; the handler wraps
// each into the {key: name} object shape the real API returns.
func schemaServer(t *testing.T, itemTypes, itemFields, creatorFields []string) *httptest.Server {
	t.Helper()
	write := func(w http.ResponseWriter, key string, names []string) {
		rows := make([]map[string]string, 0, len(names))
		for _, n := range names {
			rows = append(rows, map[string]string{key: n, "localized": n})
		}
		b, _ := json.Marshal(rows)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/itemTypes":
			write(w, "itemType", itemTypes)
		case "/itemFields":
			write(w, "field", itemFields)
		case "/creatorFields":
			write(w, "field", creatorFields)
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
		}
	}))
}

func runSchemaDrift(t *testing.T, baseURL, baselinePath string, asJSON bool, extra ...string) (string, error) {
	t.Helper()
	t.Setenv("ZOTERO_BASE_URL", baseURL)
	cmd := newSchemaDriftCmd(&rootFlags{asJSON: asJSON})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	args := append([]string{"--baseline", baselinePath}, extra...)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestSchemaDriftRejectsRelativeHomeWithoutBaselineWrite(t *testing.T) {
	srv := schemaServer(t, []string{"book"}, []string{"title"}, []string{"firstName"})
	defer srv.Close()
	for _, home := range []string{"rel", ""} {
		t.Run("HOME="+home, func(t *testing.T) {
			cwd := t.TempDir()
			t.Chdir(cwd)
			t.Setenv("HOME", home)
			t.Setenv("ZOTERO_BASE_URL", srv.URL)

			cmd := newSchemaDriftCmd(&rootFlags{})
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true
			err := cmd.Execute()
			if err == nil {
				t.Fatal("schema drift succeeded without an absolute home")
			}
			if home == "rel" && !strings.Contains(err.Error(), `home directory "rel" is not absolute`) {
				t.Fatalf("schema drift error = %v, want invalid home error", err)
			}
			for _, parent := range []string{cwd, filepath.Join(cwd, "rel")} {
				baseline := filepath.Join(parent, ".local", "share", "zotio", "schema-baseline.json")
				if _, statErr := os.Stat(baseline); !os.IsNotExist(statErr) {
					t.Fatalf("baseline created under working directory at %s: %v", baseline, statErr)
				}
			}
		})
	}
}

func TestSchemaDriftDefaultBaselineStaysUnderHome(t *testing.T) {
	srv := schemaServer(t, []string{"book"}, []string{"title"}, []string{"firstName"})
	defer srv.Close()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ZOTERO_BASE_URL", srv.URL)
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "other-data"))

	cmd := newSchemaDriftCmd(&rootFlags{})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	if err := cmd.Execute(); err != nil {
		t.Fatalf("schema drift: %v", err)
	}
	baseline := filepath.Join(home, ".local", "share", "zotio", "schema-baseline.json")
	if _, err := os.Stat(baseline); err != nil {
		t.Fatalf("default baseline %s: %v", baseline, err)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("XDG_DATA_HOME"), "zotio", "schema-baseline.json")); !os.IsNotExist(err) {
		t.Fatalf("baseline moved to XDG data directory: %v", err)
	}
}

func TestSchemaDriftCapturesBaselineThenReportsNoDrift(t *testing.T) {
	srv := schemaServer(t,
		[]string{"book", "journalArticle"},
		[]string{"title", "date"},
		[]string{"firstName", "lastName"},
	)
	defer srv.Close()
	baseline := filepath.Join(t.TempDir(), "baseline.json")

	// First run captures the baseline.
	out, err := runSchemaDrift(t, srv.URL, baseline, true)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(out), &first); err != nil {
		t.Fatalf("decoding first output %q: %v", out, err)
	}
	if first["baseline_captured"] != true {
		t.Errorf("first run baseline_captured = %v, want true", first["baseline_captured"])
	}
	if first["drift"] != false {
		t.Errorf("first run drift = %v, want false", first["drift"])
	}

	// Second run against the same schema reports no drift.
	out, err = runSchemaDrift(t, srv.URL, baseline, true)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	var second map[string]any
	if err := json.Unmarshal([]byte(out), &second); err != nil {
		t.Fatalf("decoding second output %q: %v", out, err)
	}
	if second["baseline_captured"] != false {
		t.Errorf("second run baseline_captured = %v, want false", second["baseline_captured"])
	}
	if second["drift"] != false {
		t.Errorf("second run drift = %v, want false", second["drift"])
	}
}

func TestSchemaDriftReportsAddedAndRemoved(t *testing.T) {
	baseline := filepath.Join(t.TempDir(), "baseline.json")

	// Capture a baseline on the "old" schema.
	oldSrv := schemaServer(t,
		[]string{"book", "journalArticle"},
		[]string{"title", "date"},
		[]string{"firstName", "lastName"},
	)
	if _, err := runSchemaDrift(t, oldSrv.URL, baseline, true); err != nil {
		oldSrv.Close()
		t.Fatalf("capture baseline: %v", err)
	}
	oldSrv.Close()

	// "Upgrade": add item type preprint + field citationKey, drop item type book.
	newSrv := schemaServer(t,
		[]string{"journalArticle", "preprint"},
		[]string{"title", "date", "citationKey"},
		[]string{"firstName", "lastName"},
	)
	defer newSrv.Close()

	out, err := runSchemaDrift(t, newSrv.URL, baseline, true)
	if err != nil {
		t.Fatalf("drift run: %v", err)
	}
	var res struct {
		Drift  bool `json:"drift"`
		Deltas []struct {
			Section string   `json:"section"`
			Added   []string `json:"added"`
			Removed []string `json:"removed"`
		} `json:"deltas"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("decoding drift output %q: %v", out, err)
	}
	if !res.Drift {
		t.Fatalf("expected drift, got none: %s", out)
	}
	sections := map[string]struct {
		added   []string
		removed []string
	}{}
	for _, d := range res.Deltas {
		sections[d.Section] = struct {
			added   []string
			removed []string
		}{d.Added, d.Removed}
	}
	it, ok := sections["item-types"]
	if !ok || !contains(it.added, "preprint") || !contains(it.removed, "book") {
		t.Errorf("item-types delta = %+v, want +preprint -book", it)
	}
	f, ok := sections["item-fields"]
	if !ok || !contains(f.added, "citationKey") || len(f.removed) != 0 {
		t.Errorf("item-fields delta = %+v, want +citationKey only", f)
	}
	if _, ok := sections["creator-fields"]; ok {
		t.Errorf("creator-fields unchanged should produce no delta, got %+v", sections["creator-fields"])
	}
}

func TestSchemaDriftHumanOutput(t *testing.T) {
	baseline := filepath.Join(t.TempDir(), "baseline.json")
	oldSrv := schemaServer(t, []string{"book"}, []string{"title"}, []string{"firstName"})
	if _, err := runSchemaDrift(t, oldSrv.URL, baseline, false); err != nil {
		oldSrv.Close()
		t.Fatalf("capture: %v", err)
	}
	oldSrv.Close()

	newSrv := schemaServer(t, []string{"book", "preprint"}, []string{"title"}, []string{"firstName"})
	defer newSrv.Close()
	out, err := runSchemaDrift(t, newSrv.URL, baseline, false)
	if err != nil {
		t.Fatalf("drift: %v", err)
	}
	if !strings.Contains(out, "Schema drift detected") || !strings.Contains(out, "+ item-types: preprint") {
		t.Errorf("human output missing drift line:\n%s", out)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// writeSchemaRows writes a Zotero schema array response ([{key:name,localized:name}]).
func writeSchemaRows(w http.ResponseWriter, key string, names []string) {
	rows := make([]map[string]string, 0, len(names))
	for _, n := range names {
		rows = append(rows, map[string]string{key: n, "localized": n})
	}
	b, _ := json.Marshal(rows)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

// versionedSchemaServer sets Zotero-Schema-Version on every response and counts
// hits per path so tests can assert which endpoints the fast path skips.
func versionedSchemaServer(version string, itemTypes []string, hits map[string]int, mu *sync.Mutex) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.Path]++
		mu.Unlock()
		w.Header().Set("Zotero-Schema-Version", version)
		switch r.URL.Path {
		case "/itemTypes":
			writeSchemaRows(w, "itemType", itemTypes)
		case "/itemFields":
			writeSchemaRows(w, "field", []string{"title"})
		case "/creatorFields":
			writeSchemaRows(w, "field", []string{"firstName"})
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
}

func TestSchemaDriftFastPathSkipsFetchWhenVersionUnchanged(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]int{}
	srv := versionedSchemaServer("100", []string{"book"}, hits, &mu)
	defer srv.Close()
	baseline := filepath.Join(t.TempDir(), "baseline.json")

	// Capture baseline (fetches all three global lists).
	if _, err := runSchemaDrift(t, srv.URL, baseline, true); err != nil {
		t.Fatalf("capture: %v", err)
	}
	mu.Lock()
	hits["/itemTypes"], hits["/itemFields"], hits["/creatorFields"] = 0, 0, 0
	mu.Unlock()

	// Second run, same Zotero-Schema-Version: must short-circuit after /itemTypes.
	out, err := runSchemaDrift(t, srv.URL, baseline, true)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits["/itemTypes"] != 1 {
		t.Errorf("/itemTypes hits = %d, want 1", hits["/itemTypes"])
	}
	if hits["/itemFields"] != 0 || hits["/creatorFields"] != 0 {
		t.Errorf("fast path must skip itemFields/creatorFields, hits = %v", hits)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if res["drift"] != false {
		t.Errorf("drift = %v, want false", res["drift"])
	}
	if res["schema_version"] != "100" {
		t.Errorf("schema_version = %v, want 100", res["schema_version"])
	}
}

func TestSchemaDriftRefetchesWhenVersionChanges(t *testing.T) {
	var mu sync.Mutex
	baseline := filepath.Join(t.TempDir(), "baseline.json")

	s1 := versionedSchemaServer("100", []string{"book"}, map[string]int{}, &mu)
	if _, err := runSchemaDrift(t, s1.URL, baseline, true); err != nil {
		s1.Close()
		t.Fatalf("capture: %v", err)
	}
	s1.Close()

	hits := map[string]int{}
	s2 := versionedSchemaServer("101", []string{"book", "preprint"}, hits, &mu)
	defer s2.Close()
	out, err := runSchemaDrift(t, s2.URL, baseline, true)
	if err != nil {
		t.Fatalf("drift run: %v", err)
	}
	mu.Lock()
	if hits["/itemFields"] == 0 {
		t.Errorf("version change must trigger a full refetch, hits = %v", hits)
	}
	mu.Unlock()
	var res struct {
		Drift         bool   `json:"drift"`
		SchemaVersion string `json:"schema_version"`
		Deltas        []struct {
			Section string   `json:"section"`
			Added   []string `json:"added"`
		} `json:"deltas"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if !res.Drift || res.SchemaVersion != "101" {
		t.Fatalf("want drift with version 101, got %+v", res)
	}
	got := false
	for _, d := range res.Deltas {
		if d.Section == "item-types" && contains(d.Added, "preprint") {
			got = true
		}
	}
	if !got {
		t.Errorf("expected +item-types preprint, got %s", out)
	}
}

// TestSchemaCommandStripsLibraryPrefix proves the generated schema commands hit the
// global /itemTypes path, not the library-prefixed /users/0/itemTypes (which 404s on
// the live local API). The server only serves the global path, so the command can
// succeed only when newSchemaClient has stripped the prefix.
func TestSchemaCommandStripsLibraryPrefix(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate any store access
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/itemTypes":
			writeSchemaRows(w, "itemType", []string{"book", "journalArticle"})
		case "/users/0/itemTypes":
			http.Error(w, "No endpoint found", http.StatusNotFound)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	cmd := newSchemaItemTypesCmd(&rootFlags{asJSON: true})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("schema item-types should succeed against the global path, got: %v", err)
	}
	if !strings.Contains(out.String(), "journalArticle") {
		t.Errorf("expected item types in output, got %s", out.String())
	}
}

// deepSchemaServer serves global schema endpoints including per-type deep
// endpoints. It versions every response when version != "".
func deepSchemaServer(t *testing.T, version string, itemTypes []string, hits map[string]int, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	typeFields := map[string][]string{"book": {"title", "ISBN"}}
	typeCreators := map[string][]string{"book": {"author"}}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mu != nil {
			mu.Lock()
			hits[r.URL.Path]++
			mu.Unlock()
		}
		if version != "" {
			w.Header().Set("Zotero-Schema-Version", version)
		}
		switch r.URL.Path {
		case "/itemTypes":
			writeSchemaRows(w, "itemType", itemTypes)
		case "/itemFields":
			writeSchemaRows(w, "field", []string{"title"})
		case "/creatorFields":
			writeSchemaRows(w, "field", []string{"firstName"})
		case "/itemTypeFields":
			it := r.URL.Query().Get("itemType")
			writeSchemaRows(w, "field", typeFields[it])
		case "/itemTypeCreatorTypes":
			it := r.URL.Query().Get("itemType")
			writeSchemaRows(w, "creatorType", typeCreators[it])
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
}

func seedDeepSchemaCache(t *testing.T, dbPath, version string, fields, creatorTypes map[string][]string) {
	t.Helper()
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open deep schema cache: %v", err)
	}
	defer db.Close()

	seed := func(resource string, values map[string][]string, key string) {
		ids := make([]string, 0, len(values))
		rows := make([]json.RawMessage, 0, len(values))
		for itemType, names := range values {
			payload := make([]map[string]string, 0, len(names))
			for _, name := range names {
				payload = append(payload, map[string]string{key: name})
			}
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatalf("marshal %s/%s: %v", resource, itemType, err)
			}
			ids = append(ids, itemType)
			rows = append(rows, raw)
		}
		if _, err := db.UpsertKeyed(resource, ids, rows); err != nil {
			t.Fatalf("seed %s: %v", resource, err)
		}
		if err := db.SaveZoteroSchemaVersion(resource, version); err != nil {
			t.Fatalf("seed %s version: %v", resource, err)
		}
	}
	seed("schema-item-type-fields", fields, "field")
	seed("schema-item-type-creator-types", creatorTypes, "creatorType")
}

func TestSchemaDriftShallowThenDeepUsesCachedPerTypeSchema(t *testing.T) {
	var mu sync.Mutex
	srv1 := versionedSchemaServer("100", []string{"book"}, map[string]int{}, &mu)
	baseline := filepath.Join(t.TempDir(), "baseline.json")
	if _, err := runSchemaDrift(t, srv1.URL, baseline, true); err != nil {
		srv1.Close()
		t.Fatalf("shallow capture: %v", err)
	}
	srv1.Close()
	baseLoaded, _, err := loadSchemaBaseline(baseline)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if baseLoaded.TypeFields != nil || baseLoaded.TypeCreators != nil {
		t.Fatalf("expected shallow baseline, got deep maps: %#v", baseLoaded)
	}

	dbPath := filepath.Join(t.TempDir(), "schema.db")
	seedDeepSchemaCache(t, dbPath, "100",
		map[string][]string{"book": {"title", "ISBN"}},
		map[string][]string{"book": {"author"}},
	)
	hits := map[string]int{}
	srv2 := deepSchemaServer(t, "100", []string{"book"}, hits, &mu)
	defer srv2.Close()
	out, err := runSchemaDrift(t, srv2.URL, baseline, true, "--deep", "--db", dbPath)
	if err != nil {
		t.Fatalf("deep drift: %v", err)
	}
	mu.Lock()
	gotDeepRequests := hits["/itemTypeFields"] + hits["/itemTypeCreatorTypes"]
	mu.Unlock()
	if gotDeepRequests != 0 {
		t.Fatalf("deep drift fetched per-type endpoints instead of using the cache: hits=%v output=%s", hits, out)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if res["drift"] != true {
		t.Errorf("drift = %v, want true (deep baseline newly surfaced), output=%s", res["drift"], out)
	}
	if !strings.Contains(out, "type-fields:book") && !strings.Contains(out, "type-creators:book") {
		t.Errorf("expected deep deltas for book, got %s", out)
	}
}

func TestSchemaDriftDeepPerTypeFieldChangeIsReportedFromCache(t *testing.T) {
	var mu sync.Mutex
	srv1 := versionedSchemaServer("100", []string{"book"}, map[string]int{}, &mu)
	baseline := filepath.Join(t.TempDir(), "baseline.json")
	if _, err := runSchemaDrift(t, srv1.URL, baseline, true); err != nil {
		srv1.Close()
		t.Fatalf("shallow capture: %v", err)
	}
	srv1.Close()

	dbPath := filepath.Join(t.TempDir(), "schema.db")
	seedDeepSchemaCache(t, dbPath, "100",
		map[string][]string{"book": {"title", "ISBN", "DOI"}},
		map[string][]string{"book": {"author"}},
	)
	hits := map[string]int{}
	srv2 := deepSchemaServer(t, "100", []string{"book"}, hits, &mu)
	defer srv2.Close()
	out, err := runSchemaDrift(t, srv2.URL, baseline, true, "--deep", "--db", dbPath)
	if err != nil {
		t.Fatalf("deep drift: %v", err)
	}
	mu.Lock()
	gotDeepRequests := hits["/itemTypeFields"] + hits["/itemTypeCreatorTypes"]
	mu.Unlock()
	if gotDeepRequests != 0 {
		t.Errorf("deep drift fetched per-type endpoints instead of using the cache: hits=%v", hits)
	}
	var res struct {
		Drift  bool `json:"drift"`
		Deltas []struct {
			Section string   `json:"section"`
			Added   []string `json:"added"`
		} `json:"deltas"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if !res.Drift {
		t.Fatalf("expected drift for changed per-type field, got none: %s", out)
	}
	found := false
	for _, d := range res.Deltas {
		if d.Section == "type-fields:book" && contains(d.Added, "DOI") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected type-fields:book +DOI, got %s", out)
	}
}

func TestSchemaDriftDeepFastPathUsesSelectedCacheContents(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]int{}
	srv := deepSchemaServer(t, "100", []string{"book"}, hits, &mu)
	defer srv.Close()

	baseline := filepath.Join(t.TempDir(), "baseline.json")
	dbA := filepath.Join(t.TempDir(), "schema-a.db")
	seedDeepSchemaCache(t, dbA, "100",
		map[string][]string{"book": {"title", "ISBN"}},
		map[string][]string{"book": {"author"}},
	)
	if _, err := runSchemaDrift(t, srv.URL, baseline, true, "--deep", "--db", dbA); err != nil {
		t.Fatalf("capture deep baseline from cache A: %v", err)
	}

	dbB := filepath.Join(t.TempDir(), "schema-b.db")
	seedDeepSchemaCache(t, dbB, "100",
		map[string][]string{"book": {"title", "DOI"}},
		map[string][]string{"book": {"editor"}},
	)
	out, err := runSchemaDrift(t, srv.URL, baseline, true, "--deep", "--db", dbB)
	if err != nil {
		t.Fatalf("compare selected cache B: %v", err)
	}

	var res struct {
		Drift  bool `json:"drift"`
		Deltas []struct {
			Section string   `json:"section"`
			Added   []string `json:"added"`
			Removed []string `json:"removed"`
		} `json:"deltas"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if !res.Drift {
		t.Fatalf("selected cache B differs from baseline cache A but reported no drift: %s", out)
	}
	var fieldsFromB, creatorsFromB bool
	for _, delta := range res.Deltas {
		switch delta.Section {
		case "type-fields:book":
			fieldsFromB = contains(delta.Added, "DOI") && contains(delta.Removed, "ISBN")
		case "type-creators:book":
			creatorsFromB = contains(delta.Added, "editor") && contains(delta.Removed, "author")
		}
	}
	if !fieldsFromB || !creatorsFromB {
		t.Fatalf("deltas = %+v, want cache B values (+DOI, +editor) against cache A (-ISBN, -author)", res.Deltas)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits["/itemTypeFields"] != 0 || hits["/itemTypeCreatorTypes"] != 0 {
		t.Fatalf("explicit valid caches should supply both deep snapshots, hits=%v", hits)
	}
}

func TestSchemaDriftShallowFastPathStillFires(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]int{}
	srv := versionedSchemaServer("100", []string{"book"}, hits, &mu)
	defer srv.Close()
	baseline := filepath.Join(t.TempDir(), "baseline.json")
	if _, err := runSchemaDrift(t, srv.URL, baseline, true); err != nil {
		t.Fatalf("capture: %v", err)
	}
	mu.Lock()
	hits["/itemTypes"], hits["/itemFields"], hits["/creatorFields"] = 0, 0, 0
	mu.Unlock()
	out, err := runSchemaDrift(t, srv.URL, baseline, true)
	if err != nil {
		t.Fatalf("second shallow run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits["/itemTypes"] != 1 {
		t.Errorf("/itemTypes hits = %d, want 1", hits["/itemTypes"])
	}
	if hits["/itemFields"] != 0 || hits["/creatorFields"] != 0 {
		t.Errorf("shallow fast path must still skip remaining fetches, hits=%v", hits)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if res["drift"] != false {
		t.Errorf("drift = %v, want false", res["drift"])
	}
}

func TestSchemaDriftDeepWithoutDBFetchesPerTypeSchemaLive(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]int{}
	srv := deepSchemaServer(t, "100", []string{"book"}, hits, &mu)
	defer srv.Close()

	baseline := filepath.Join(t.TempDir(), "baseline.json")
	if _, err := runSchemaDrift(t, srv.URL, baseline, true, "--deep"); err != nil {
		t.Fatalf("deep live capture: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if hits["/itemTypeFields"] != 1 || hits["/itemTypeCreatorTypes"] != 1 {
		t.Fatalf("deep live endpoint hits = %v, want one request per endpoint", hits)
	}
}

func TestSchemaDriftDeepFastPathRejectsPartialExplicitCache(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]int{}
	srv := deepSchemaServer(t, "100", []string{"book", "journalArticle"}, hits, &mu)
	defer srv.Close()

	baseline := filepath.Join(t.TempDir(), "baseline.json")
	if _, err := runSchemaDrift(t, srv.URL, baseline, true, "--deep"); err != nil {
		t.Fatalf("deep baseline capture: %v", err)
	}

	dbPath := filepath.Join(t.TempDir(), "schema.db")
	seedDeepSchemaCache(t, dbPath, "100",
		map[string][]string{"book": {"title", "ISBN"}},
		map[string][]string{"book": {"author"}},
	)
	mu.Lock()
	hits["/itemTypeFields"] = 0
	hits["/itemTypeCreatorTypes"] = 0
	mu.Unlock()

	out, err := runSchemaDrift(t, srv.URL, baseline, true, "--deep", "--db", dbPath)
	if err == nil || ExitCode(err) != 9 {
		t.Fatalf("partial cache error = %v (exit %d), want precondition exit 9; output=%s", err, ExitCode(err), out)
	}
	var env preconditionUnmetEnvelope
	if decodeErr := json.Unmarshal([]byte(out), &env); decodeErr != nil {
		t.Fatalf("decode precondition envelope %q: %v", out, decodeErr)
	}
	if env.Kind != "precondition_unmet" || env.Precondition != preconditionSyncedStore {
		t.Fatalf("envelope = %+v, want precondition_unmet/%s", env, preconditionSyncedStore)
	}
	if !strings.Contains(env.Detail, "journalArticle") {
		t.Fatalf("precondition detail = %q, want the missing item type", env.Detail)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits["/itemTypeFields"] != 0 || hits["/itemTypeCreatorTypes"] != 0 {
		t.Fatalf("partial explicit cache silently fell back instead of refusing: hits=%v", hits)
	}
}

func TestSchemaDriftDeepWithNoLiveVersionFallsBackFromCacheToLive(t *testing.T) {
	var mu sync.Mutex
	baselineServer := deepSchemaServer(t, "100", []string{"book"}, map[string]int{}, &mu)
	baseline := filepath.Join(t.TempDir(), "baseline.json")
	if _, err := runSchemaDrift(t, baselineServer.URL, baseline, true, "--deep"); err != nil {
		baselineServer.Close()
		t.Fatalf("deep baseline capture: %v", err)
	}
	baselineServer.Close()

	dbPath := filepath.Join(t.TempDir(), "schema.db")
	seedDeepSchemaCache(t, dbPath, "100",
		map[string][]string{"book": {"title", "ISBN"}},
		map[string][]string{"book": {"author"}},
	)
	hits := map[string]int{}
	liveServer := deepSchemaServer(t, "", []string{"book"}, hits, &mu)
	defer liveServer.Close()

	if _, err := runSchemaDrift(t, liveServer.URL, baseline, true, "--deep", "--db", dbPath); err != nil {
		t.Fatalf("deep drift without live version: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits["/itemTypeFields"] != 1 || hits["/itemTypeCreatorTypes"] != 1 {
		t.Fatalf("unverifiable cache did not use live per-type endpoints: hits=%v", hits)
	}
}

func TestSchemaDriftDeepMissingCheckpointTableFallsBackToLive(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]int{}
	srv := deepSchemaServer(t, "100", []string{"book"}, hits, &mu)
	defer srv.Close()

	baseline := filepath.Join(t.TempDir(), "baseline.json")
	if _, err := runSchemaDrift(t, srv.URL, baseline, true, "--deep"); err != nil {
		t.Fatalf("deep baseline capture: %v", err)
	}

	dbPath := filepath.Join(t.TempDir(), "old.db")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("create old store: %v", err)
	}
	if _, err := db.DB().Exec(`DROP TABLE schema_sync_versions`); err != nil {
		db.Close()
		t.Fatalf("drop optional checkpoint table: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close old store: %v", err)
	}
	mu.Lock()
	hits["/itemTypeFields"] = 0
	hits["/itemTypeCreatorTypes"] = 0
	mu.Unlock()

	if _, err := runSchemaDrift(t, srv.URL, baseline, true, "--deep", "--db", dbPath); err != nil {
		t.Fatalf("deep drift with pre-feature store: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits["/itemTypeFields"] != 1 || hits["/itemTypeCreatorTypes"] != 1 {
		t.Fatalf("missing checkpoint table did not fall back live: hits=%v", hits)
	}
}
