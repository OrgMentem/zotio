// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
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
