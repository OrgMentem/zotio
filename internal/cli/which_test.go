// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"strings"
	"testing"
)

// Fixture index used across which-ranking tests. Covers a typical mix
// of single-word commands, multi-word commands, and grouped entries so
// the ranker is exercised against shapes a generated CLI actually
// produces.
var whichTestIndex = []whichEntry{
	{Command: "search", Description: "Full-text search across synced resources", Group: "Local state"},
	{Command: "stale", Description: "Find tickets that have not moved in a while", Group: "Local state"},
	{Command: "bottleneck", Description: "Identify pipeline bottlenecks", Group: "Local state"},
	{Command: "send", Description: "Send a message", Group: "Write operations"},
	{Command: "sync", Description: "Sync resources to local SQLite", Group: "Local state"},
}

// Happy path: a query that matches a command by keyword returns that
// command first. This is the load-bearing promise of `which`.
func TestRankWhich_ExactTokenMatchWins(t *testing.T) {
	got := rankWhich(whichTestIndex, "search", 3)
	if len(got) == 0 {
		t.Fatalf("expected at least one match, got zero")
	}
	if got[0].Entry.Command != "search" {
		t.Errorf("top match: want search, got %s", got[0].Entry.Command)
	}
}

// Happy path: a query matching the description wins when the command
// itself does not contain the query tokens.
func TestRankWhich_DescriptionMatch(t *testing.T) {
	got := rankWhich(whichTestIndex, "bottlenecks", 3)
	if len(got) == 0 || got[0].Entry.Command != "bottleneck" {
		t.Errorf("expected bottleneck command as top match for bottlenecks query, got %+v", got)
	}
}

// Happy path: a multi-word query resolves to the best single match by
// summing per-token scores.
func TestRankWhich_MultiTokenQuery(t *testing.T) {
	got := rankWhich(whichTestIndex, "send a message", 3)
	if len(got) == 0 || got[0].Entry.Command != "send" {
		t.Errorf("expected send as top match for 'send a message', got %+v", got)
	}
}

// Edge case: empty query should surface the full index (listing mode)
// rather than treating as no-match. Agents use this for broad discovery.
func TestRankWhich_EmptyQueryListsIndex(t *testing.T) {
	got := rankWhich(whichTestIndex, "", 3)
	if len(got) != len(whichTestIndex) {
		t.Errorf("empty query should return all %d entries, got %d", len(whichTestIndex), len(got))
	}
	for i, m := range got {
		if m.Score != 0 {
			t.Errorf("empty query entry %d: score should be 0, got %d", i, m.Score)
		}
	}
}

// Edge case: the limit flag caps the result set so agents can ask for
// a single top answer when they want a deterministic branch.
func TestRankWhich_LimitCapsResults(t *testing.T) {
	got := rankWhich(whichTestIndex, "local", 1)
	if len(got) > 1 {
		t.Errorf("limit=1 should return at most 1 match, got %d", len(got))
	}
}

// No-match path: a query that hits nothing in the index returns an
// empty slice so the caller can exit with the no-match code (2) rather
// than printing a misleading best-effort result.
func TestRankWhich_NoMatchReturnsEmpty(t *testing.T) {
	got := rankWhich(whichTestIndex, "nonexistentxyz", 3)
	if len(got) != 0 {
		t.Errorf("nonsense query should return zero matches, got %d (%+v)", len(got), got)
	}
}

// Sanity: whichIndex compiles and is well-formed. Generated CLIs with
// zero NovelFeatures ship an empty index, and that is still a valid
// state (which returns the "no curated index" error at runtime).
func TestWhichIndex_ExistsAndIsWellFormed(t *testing.T) {
	for i, e := range whichIndex {
		if e.Command == "" {
			t.Errorf("whichIndex[%d] has empty Command - template rendered bad data", i)
		}
		if strings.TrimSpace(e.Description) == "" {
			t.Errorf("whichIndex[%d] (%s) has empty Description - template rendered bad data", i, e.Command)
		}
	}
}

// Regression: report #1 finding 12 - the two most obvious phrasings for
// filing an item into a collection must resolve to the actual command,
// not to an unrelated curated entry that merely shares a keyword (e.g.
// "attachments add" for "add").
func TestRankWhich_ReportPhrasesResolveToCollectionMembership(t *testing.T) {
	index := buildWhichIndex(RootCmd())
	wantAny := map[string]bool{"items move": true, "items add-to-collection": true}
	for _, query := range []string{
		"add an item to a collection",
		"file a paper into a collection",
	} {
		got := rankWhich(index, query, 3)
		found := false
		for _, m := range got {
			if wantAny[m.Entry.Command] {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("query %q: want 'items move' or 'items add-to-collection' in top 3, got %+v", query, got)
		}
	}
}

// Regression: a command with no curated whichIndex entry must still be
// reachable by its own name - buildWhichIndex now walks the whole Cobra
// tree instead of scoring only the curated highlights.
func TestBuildWhichIndex_UncuratedCommandReachableByOwnName(t *testing.T) {
	index := buildWhichIndex(RootCmd())
	curated := make(map[string]bool, len(whichIndex))
	for _, e := range whichIndex {
		curated[e.Command] = true
	}
	var uncurated string
	for _, e := range index {
		if !curated[e.Command] {
			uncurated = e.Command
			break
		}
	}
	if uncurated == "" {
		t.Fatal("expected at least one command in the full tree with no curated highlight entry")
	}
	got := rankWhich(index, uncurated, 3)
	if len(got) == 0 || got[0].Entry.Command != uncurated {
		t.Errorf("query %q (uncurated command's own name): want it to rank first, got %+v", uncurated, got)
	}
}

// Regression: queries that already resolved to a sensible curated answer
// before the full-tree index and intent aliases were added must keep
// resolving to that same answer - the fix must not push good matches out.
func TestRankWhich_PriorGoodMatchesStillWin(t *testing.T) {
	index := buildWhichIndex(RootCmd())
	cases := map[string]string{
		"retracted papers":  "items retract-check",
		"check manuscript":  "items bibcheck",
		"citekey conflicts": "library health",
	}
	for query, want := range cases {
		got := rankWhich(index, query, 3)
		if len(got) == 0 || got[0].Entry.Command != want {
			t.Errorf("query %q: want top match %q, got %+v", query, want, got)
		}
	}
}
