// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

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
