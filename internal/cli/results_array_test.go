// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Pins the read-envelope invariant end to end, at the command level: .results
// is always a JSON array, for a single-item read (items get), a list read
// (items list), and items find — which previously had no envelope at all.
// See dev/field-report-2026-08-08.md finding 10 and
// dev/field-report-2026-08-08-library-hygiene.md finding 8.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/spf13/cobra"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"zotio/internal/store"
)

// resultsArrayEnvelope decodes the shared {meta, results} shape with results
// typed as a slice — json.Unmarshal fails loudly (not silently) if a command
// ever regresses .results back to a bare object.
type resultsArrayEnvelope struct {
	Results []map[string]any `json:"results"`
	Meta    struct {
		Source string `json:"source"`
	} `json:"meta"`
}

func decodeResultsArrayEnvelope(t *testing.T, out []byte) resultsArrayEnvelope {
	t.Helper()
	var env resultsArrayEnvelope
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("decode envelope %s: %v (results must unmarshal into a JSON array)", out, err)
	}
	return env
}

// TestItemsGetResultsIsOneElementArray covers the single-item-read case named
// in report #1 finding 10: `items get` used to emit .results as a bare
// object, breaking jq written for the list shape.
func TestItemsGetResultsIsOneElementArray(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/0/items/ABCD1234" {
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"key":"ABCD1234","version":7,"data":{"title":"A Paper"}}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

	cmd := newItemsGetCmd(&rootFlags{asJSON: true, dataSource: "live", noCache: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"ABCD1234"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("items get: %v", err)
	}

	env := decodeResultsArrayEnvelope(t, out.Bytes())
	if len(env.Results) != 1 {
		t.Fatalf("results length = %d, want 1 (single-item read): %s", len(env.Results), out.String())
	}
	// jq-style traversal: results[0].key must reach the item, uniformly with
	// how a list read is indexed.
	if got := env.Results[0]["key"]; got != "ABCD1234" {
		t.Fatalf("results[0].key = %v, want ABCD1234", got)
	}
}

// TestItemsListResultsIsArray covers the list-read case that report #1
// finding 10 contrasted `items get` against: `items list` already returned
// .results as an array, and must keep doing so.
func TestItemsListResultsIsArray(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/0/items" {
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`[{"key":"A"},{"key":"B"}]`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

	cmd := newItemsListCmd(&rootFlags{asJSON: true, dataSource: "live", noCache: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("items list: %v", err)
	}

	env := decodeResultsArrayEnvelope(t, out.Bytes())
	if len(env.Results) != 2 {
		t.Fatalf("results length = %d, want 2 (list read): %s", len(env.Results), out.String())
	}
	if got := env.Results[0]["key"]; got != "A" {
		t.Fatalf("results[0].key = %v, want A", got)
	}
}

// TestItemsFindGainsResultsWrapper covers report #2 finding 8: `items find`
// used to print a bare top-level array with no meta/results wrapper at all
// (jq '.results[]' failed outright; only jq '.[]' worked). It must now emit
// the same {meta, results} envelope every other read command uses, with
// .results as an array.
func TestItemsFindGainsResultsWrapper(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	t.Setenv("ZOTERO_BASE_URL", "http://127.0.0.1:1/api/users/0") // unused; items find is local-only

	dbPath, err := defaultDBPath("zotio")
	if err != nil {
		t.Fatalf("defaultDBPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	seed := []json.RawMessage{
		json.RawMessage(`{"key":"DOIKEY1","version":3,"data":{"key":"DOIKEY1","itemType":"journalArticle","title":"Findable Paper","DOI":"10.1145/3290605.3300709"}}`),
	}
	if _, _, err := db.UpsertBatch("items", seed); err != nil {
		t.Fatalf("seed items: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	cmd := newItemsFindCmd(&rootFlags{asJSON: true, noCache: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--doi", "10.1145/3290605.3300709"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("items find: %v", err)
	}

	// Must decode as an object with a meta/results wrapper, not a bare array.
	var bareArray []json.RawMessage
	if json.Unmarshal(out.Bytes(), &bareArray) == nil {
		t.Fatalf("items find --json emitted a bare top-level array, want a {meta, results} envelope: %s", out.String())
	}

	env := decodeResultsArrayEnvelope(t, out.Bytes())
	if env.Meta.Source == "" {
		t.Fatalf("meta.source missing from envelope: %s", out.String())
	}
	if len(env.Results) != 1 {
		t.Fatalf("results length = %d, want 1: %s", len(env.Results), out.String())
	}
	if got := env.Results[0]["key"]; got != "DOIKEY1" {
		t.Fatalf("results[0].key = %v, want DOIKEY1", got)
	}
}

func annotationEnvelopeFixture() string {
	return `{"key":"ANN1","version":1,"data":{"key":"ANN1","itemType":"annotation","parentItem":"PARENT1","dateAdded":"2026-08-08T10:00:00Z","annotationColor":"#ffd400","annotationType":"highlight","annotationText":"A highlighted passage","annotationComment":"A note","annotationPageLabel":"1"}}`
}

func TestAnnotationsSearchResultsIsArray(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/0/items" {
			t.Fatalf("unexpected request path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte("[" + annotationEnvelopeFixture() + "]"))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

	flags := &rootFlags{asJSON: true, dataSource: "live", noCache: true}
	cmd := newAnnotationsSearchCmd(flags)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"passage"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("annotations search: %v", err)
	}
	env := decodeResultsArrayEnvelope(t, out.Bytes())
	if len(env.Results) != 1 || env.Results[0]["key"] != "ANN1" {
		t.Fatalf("results[0] = %#v, want annotation ANN1", env.Results)
	}
}

func TestAnnotationsTimelineResultsIsArray(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/0/items" {
			t.Fatalf("unexpected request path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte("[" + annotationEnvelopeFixture() + "]"))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

	flags := &rootFlags{asJSON: true, dataSource: "live", noCache: true}
	cmd := newAnnotationsTimelineCmd(flags)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("annotations timeline: %v", err)
	}
	env := decodeResultsArrayEnvelope(t, out.Bytes())
	if len(env.Results) != 1 || env.Results[0]["key"] != "ANN1" {
		t.Fatalf("results[0] = %#v, want annotation ANN1", env.Results)
	}
}

func TestCapabilitiesResultsIsArray(t *testing.T) {
	root := &cobra.Command{Use: "zotio"}
	root.AddCommand(&cobra.Command{
		Use:         "demo",
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE:        func(cmd *cobra.Command, args []string) error { return nil },
	})
	cmd := newCapabilitiesCmd(root, &rootFlags{})
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("capabilities: %v", err)
	}
	env := decodeResultsArrayEnvelope(t, out.Bytes())
	if len(env.Results) != 1 || env.Results[0]["path"] != "demo" {
		t.Fatalf("results[0] = %#v, want capability demo", env.Results)
	}
}

func TestProfileListResultsIsArray(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	if err := saveProfileStore(&profileStore{Profiles: map[string]Profile{
		"night": {Name: "night", Description: "Nightly defaults", Values: map[string]string{"data-source": "local"}},
	}}); err != nil {
		t.Fatalf("seed profile store: %v", err)
	}
	cmd := newProfileListCmd(&rootFlags{asJSON: true})
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("profile list: %v", err)
	}
	env := decodeResultsArrayEnvelope(t, out.Bytes())
	if len(env.Results) != 1 || env.Results[0]["name"] != "night" {
		t.Fatalf("results[0] = %#v, want profile night", env.Results)
	}
}
