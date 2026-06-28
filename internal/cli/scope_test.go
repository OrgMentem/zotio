// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean roadmap-phase2): tests for the shared scope resolver — grammar
// parsing and local-store resolution, incl. the saved-search live precondition.

package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"zotero-pp-cli/internal/store"
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
