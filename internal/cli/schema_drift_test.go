// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// PATCH: Cover schema-drift baseline capture, no-drift, and added/removed deltas
// against a real client pointed at an httptest Zotero schema surface.

package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
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
