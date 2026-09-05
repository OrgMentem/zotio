// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// exclusion so attachments sharing a generic name ("PDF", "Snapshot") are never
// reported as bibliographic duplicates.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"zotio/internal/store"
)

func TestQueryDuplicateTitlesExcludesAttachments(t *testing.T) {
	db, err := store.OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	items := []json.RawMessage{
		// Two real articles sharing a title — a genuine duplicate group.
		json.RawMessage(`{"key":"P1","version":1,"data":{"key":"P1","itemType":"journalArticle","title":"Shared Title"}}`),
		json.RawMessage(`{"key":"P2","version":1,"data":{"key":"P2","itemType":"journalArticle","title":"Shared Title"}}`),
		// Two attachments named "PDF" — must NOT be flagged as duplicates.
		json.RawMessage(`{"key":"A1","version":1,"data":{"key":"A1","itemType":"attachment","parentItem":"P1","title":"PDF","contentType":"application/pdf"}}`),
		json.RawMessage(`{"key":"A2","version":1,"data":{"key":"A2","itemType":"attachment","parentItem":"P2","title":"PDF","contentType":"application/pdf"}}`),
	}
	qs := localQueryStore{db}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rows, err := queryDuplicateTitles(qs)
	if err != nil {
		t.Fatalf("queryDuplicateTitles: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("duplicate-title groups = %d, want 1 (only the article pair): %v", len(rows), rows)
	}
	if got := sqlStringValue(rows[0]["value"]); got != "Shared Title" {
		t.Errorf("duplicate group value = %q, want \"Shared Title\"", got)
	}
}

// Fold-equal titles: one paper imported twice, once from a reference list with
// a trailing full stop. The exact pass cannot see the pair (two LOWER(TRIM(...))
// keys), which is the defect; the near pass must report it AND must leave the
// exact groups alone, because those are what resolve merges and PRISMA counts.
func TestQueryNearDuplicateTitlesReportsFoldEqualPairSeparately(t *testing.T) {
	db := seedNearDuplicateStore(t,
		json.RawMessage(`{"key":"P1","version":1,"data":{"key":"P1","itemType":"journalArticle","title":"Attention Is All You Need"}}`),
		json.RawMessage(`{"key":"P2","version":1,"data":{"key":"P2","itemType":"journalArticle","title":"Attention is all you need."}}`),
		json.RawMessage(`{"key":"E1","version":1,"data":{"key":"E1","itemType":"journalArticle","title":"Deep Residual Learning"}}`),
		json.RawMessage(`{"key":"E2","version":1,"data":{"key":"E2","itemType":"journalArticle","title":"Deep Residual Learning"}}`),
		// Attachments are excluded from both passes: "PDF" against "PDF." must
		// not become an advisory row either.
		json.RawMessage(`{"key":"A1","version":1,"data":{"key":"A1","itemType":"attachment","parentItem":"P1","title":"PDF"}}`),
		json.RawMessage(`{"key":"A2","version":1,"data":{"key":"A2","itemType":"attachment","parentItem":"P2","title":"PDF."}}`),
	)

	exact, err := queryDuplicateTitles(db)
	if err != nil {
		t.Fatalf("queryDuplicateTitles: %v", err)
	}
	if len(exact) != 1 || sqlStringValue(exact[0]["value"]) != "Deep Residual Learning" {
		t.Fatalf("exact title groups = %v, want only the Deep Residual Learning pair", exact)
	}

	near, total, err := queryNearDuplicateTitles(context.Background(), db)
	if err != nil {
		t.Fatalf("queryNearDuplicateTitles: %v", err)
	}
	if len(near) != 1 || total != 1 {
		t.Fatalf("near groups = %+v (total %d), want exactly the Attention pair", near, total)
	}
	group := near[0]
	if group.Group != "title_near" || !group.RequiresReview {
		t.Errorf("group kind = %q, requires_review = %v; want title_near requiring review", group.Group, group.RequiresReview)
	}
	if group.Score != 1 {
		t.Errorf("score = %v, want 1 (equal under normalizeExactTitle)", group.Score)
	}
	if strings.Join(group.Keys, ",") != "P1,P2" || group.Count != 2 {
		t.Errorf("keys = %v (count %d), want P1,P2", group.Keys, group.Count)
	}
	if strings.Join(group.Titles, "|") != "Attention Is All You Need|Attention is all you need." {
		t.Errorf("titles = %v, want both stored spellings", group.Titles)
	}
	if group.ItemType != "journalArticle" {
		t.Errorf("item_type = %q, want journalArticle", group.ItemType)
	}
}

// A pair that is close but NOT equal after folding must score strictly below
// 1.00: the rows are printed under a heading saying the titles differ, so 1.00
// there reads as an exact hit the exact pass somehow missed.
func TestQueryNearDuplicateTitlesScoresDifferentTitlesBelowOne(t *testing.T) {
	db := seedNearDuplicateStore(t,
		json.RawMessage(`{"key":"P1","version":1,"data":{"key":"P1","itemType":"journalArticle","title":"Attention Is All You Need"}}`),
		json.RawMessage(`{"key":"P2","version":1,"data":{"key":"P2","itemType":"journalArticle","title":"Attention Is All You Need: Transformer Networks"}}`),
	)

	near, _, err := queryNearDuplicateTitles(context.Background(), db)
	if err != nil {
		t.Fatalf("queryNearDuplicateTitles: %v", err)
	}
	if len(near) != 1 {
		t.Fatalf("near groups = %+v, want the subtitle pair", near)
	}
	if near[0].Score >= 1 || near[0].Score < nearTitleMinScore {
		t.Errorf("score = %v, want in [%v, 1)", near[0].Score, nearTitleMinScore)
	}
	if strings.Join(near[0].Keys, ",") != "P1,P2" {
		t.Errorf("keys = %v, want P1,P2", near[0].Keys)
	}
}

// Two different works can share one distinctive word, so blocking pairs them
// and the score has to be the thing that keeps them out of a list a human
// reads: 2*1/(2+3) = 0.40 sits under the junk floor.
func TestQueryNearDuplicateTitlesLeavesIncidentalWordOverlapAlone(t *testing.T) {
	db := seedNearDuplicateStore(t,
		json.RawMessage(`{"key":"P1","version":1,"data":{"key":"P1","itemType":"journalArticle","title":"Attention Is All You Need"}}`),
		json.RawMessage(`{"key":"P2","version":1,"data":{"key":"P2","itemType":"journalArticle","title":"Attention Deficit in Rats"}}`),
	)

	near, total, err := queryNearDuplicateTitles(context.Background(), db)
	if err != nil {
		t.Fatalf("queryNearDuplicateTitles: %v", err)
	}
	if len(near) != 0 || total != 0 {
		t.Fatalf("near groups = %+v (total %d), want none", near, total)
	}
}

// A subtitle of words that appear nowhere else must not hide the pair. Blocking
// picks the rarest words, and a word carried by exactly one title can bucket
// nothing, so spending a slot on one costs the pair its only chance of being
// compared.
func TestQueryNearDuplicateTitlesBlocksOnSharedWordsNotUniqueOnes(t *testing.T) {
	db := seedNearDuplicateStore(t,
		json.RawMessage(`{"key":"P1","version":1,"data":{"key":"P1","itemType":"journalArticle","title":"Attention Is All You Need"}}`),
		json.RawMessage(`{"key":"P2","version":1,"data":{"key":"P2","itemType":"journalArticle","title":"Attention Is All You Need: Zqxwvu Plontar Grimbold"}}`),
	)

	near, _, err := queryNearDuplicateTitles(context.Background(), db)
	if err != nil {
		t.Fatalf("queryNearDuplicateTitles: %v", err)
	}
	if len(near) != 1 || strings.Join(near[0].Keys, ",") != "P1,P2" {
		t.Fatalf("near groups = %+v, want the subtitled pair", near)
	}
}

// The rank cap keeps the STRONGEST rows and reports how many there were, so a
// reader who sees 25 of 40 knows their pair may be on row 26. Rows are offered
// in whatever order the blocks came out of a map, so retention must not depend
// on arrival.
func TestNearTitleGroupCollectorKeepsTheStrongestAndCountsAll(t *testing.T) {
	collector := &nearTitleGroupCollector{}
	const offered = nearDuplicateTitleGroupLimit + 15
	for i := range offered {
		// Interleaved so the weak rows arrive first half the time.
		score := 0.6
		if i%2 == 0 {
			score = 0.9
		}
		collector.offer(newNearDuplicateTitleGroup(score, "journalArticle",
			[]string{fmt.Sprintf("Title %02d", i)}, []string{fmt.Sprintf("K%02d", i)}))
	}

	ranked := collector.ranked()
	if collector.total != offered {
		t.Errorf("total = %d, want %d", collector.total, offered)
	}
	if len(ranked) != nearDuplicateTitleGroupLimit {
		t.Fatalf("kept = %d, want the rank cap %d", len(ranked), nearDuplicateTitleGroupLimit)
	}
	strong := 0
	for _, group := range ranked {
		if group.Score == 0.9 {
			strong++
		}
	}
	if strong != (offered+1)/2 {
		t.Errorf("kept %d strong rows of %d, want every one of them ahead of the weak rows", strong, (offered+1)/2)
	}
	for i := 1; i < len(ranked); i++ {
		if ranked[i-1].Score < ranked[i].Score {
			t.Fatalf("ranked scores out of order at %d: %+v", i, ranked)
		}
	}
}

// The report's JSON: advisory rows travel in their own top-level key, never in
// .groups and never as an autofixable finding pointing at a resolver that
// cannot act on them.
func TestItemsDuplicatesJSONKeepsNearGroupsOutOfGroupsAndFindings(t *testing.T) {
	seedDuplicateResolveStore(t, []json.RawMessage{
		json.RawMessage(`{"key":"P1","version":1,"data":{"key":"P1","itemType":"journalArticle","title":"Attention Is All You Need"}}`),
		json.RawMessage(`{"key":"P2","version":1,"data":{"key":"P2","itemType":"journalArticle","title":"Attention is all you need."}}`),
		json.RawMessage(`{"key":"E1","version":1,"data":{"key":"E1","itemType":"journalArticle","title":"Deep Residual Learning"}}`),
		json.RawMessage(`{"key":"E2","version":1,"data":{"key":"E2","itemType":"journalArticle","title":"Deep Residual Learning"}}`),
	})

	out, _ := runItemsDuplicatesReport(t, &rootFlags{asJSON: true}, "--by", "title")
	payload := decodeDuplicatesReport(t, out)
	if len(payload.Groups) != 1 || sqlStringValue(payload.Groups[0]["value"]) != "Deep Residual Learning" {
		t.Fatalf("groups = %v, want only the exact pair", payload.Groups)
	}
	if len(payload.NearGroups) != 1 || payload.NearGroups[0].Group != "title_near" {
		t.Fatalf("near_title_groups = %+v, want one title_near row", payload.NearGroups)
	}
	if got := payload.Meta["title_lookup"]; got != "near_matches" {
		t.Errorf("meta.title_lookup = %v, want near_matches", got)
	}
	for _, finding := range payload.Findings {
		if finding.ItemKey == "P1" || finding.ItemKey == "P2" {
			t.Errorf("finding %+v names a near-match key; resolve cannot merge it", finding)
		}
	}
	if len(payload.Findings) != 2 {
		t.Errorf("findings = %d, want 2 (one per exact-group key)", len(payload.Findings))
	}
}

// meta.title_lookup names the state on every run, in the same words and under
// the same key `items find` uses, so a caller branches on data instead of on a
// key whose absence used to mean "clean", "not run" and "broken" at once.
func TestItemsDuplicatesReportsNearLookupState(t *testing.T) {
	for _, tt := range []struct {
		name      string
		args      []string
		items     []json.RawMessage
		wantState string
		wantNear  int
	}{
		{
			name:      "doi only never runs the pass",
			args:      []string{"--by", "doi"},
			items:     nearDuplicatePairFixture(),
			wantState: "not_requested",
		},
		{
			name:      "title with a near pair",
			args:      []string{"--by", "title"},
			items:     nearDuplicatePairFixture(),
			wantState: "near_matches",
			wantNear:  1,
		},
		{
			name: "title with nothing close",
			args: []string{"--by", "title"},
			items: []json.RawMessage{
				json.RawMessage(`{"key":"P1","version":1,"data":{"key":"P1","itemType":"journalArticle","title":"The Study of Networks"}}`),
			},
			wantState: "no_near_matches",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			seedDuplicateResolveStore(t, tt.items)
			out, _ := runItemsDuplicatesReport(t, &rootFlags{asJSON: true}, tt.args...)
			payload := decodeDuplicatesReport(t, out)
			if got := payload.Meta["title_lookup"]; got != tt.wantState {
				t.Errorf("meta.title_lookup = %v, want %v", got, tt.wantState)
			}
			if len(payload.NearGroups) != tt.wantNear {
				t.Errorf("near_title_groups = %d, want %d", len(payload.NearGroups), tt.wantNear)
			}
		})
	}
}

// Without --json the rows have nowhere to go: the stdout payload is the bare
// exact-group array every existing caller parses, and --plain/--csv have no
// column for a score. Name the count and the flag that carries them instead of
// dropping them on both streams. --quiet is exempt: there the exit code is the
// whole answer.
func TestItemsDuplicatesNotesNearGroupsWhenTheFormatCannotCarryThem(t *testing.T) {
	for _, tt := range []struct {
		name     string
		flags    *rootFlags
		wantNote bool
	}{
		{name: "piped default", flags: &rootFlags{}, wantNote: true},
		{name: "plain", flags: &rootFlags{plain: true}, wantNote: true},
		{name: "csv", flags: &rootFlags{csv: true}, wantNote: true},
		{name: "quiet", flags: &rootFlags{quiet: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			seedDuplicateResolveStore(t, nearDuplicatePairFixture())
			out, errOut := runItemsDuplicatesReport(t, tt.flags, "--by", "title")
			if strings.Contains(out, "title_near") {
				t.Errorf("stdout carries advisory rows in a format with no column for them: %s", out)
			}
			hasNote := strings.Contains(errOut, "1 near-duplicate title group") && strings.Contains(errOut, "--json")
			if hasNote != tt.wantNote {
				t.Errorf("stderr note = %v, want %v; stderr=%q", hasNote, tt.wantNote, errOut)
			}
		})
	}
}

// The human block leads with what the reader acts on and ends with the next
// command, including the fact that the resolver will not touch these rows.
func TestPrintNearDuplicateTitleGroupsLeadsWithKeysAndEndsWithNextCommand(t *testing.T) {
	cmd := newItemsDuplicatesCmd(&rootFlags{})
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	groups := []nearDuplicateTitleGroup{
		newNearDuplicateTitleGroup(1, "journalArticle", []string{"Attention Is All You Need", "Attention is all you need."}, []string{"P1", "P2"}),
	}

	if err := printNearDuplicateTitleGroups(cmd, groups, 3); err != nil {
		t.Fatalf("printNearDuplicateTitleGroups: %v", err)
	}
	body := out.String()
	if !strings.Contains(body, "confirm before merging") || !strings.Contains(body, "1.00") {
		t.Errorf("block = %q, want the review heading and the score", body)
	}
	if strings.Index(body, "P1, P2") > strings.Index(body, "1.00") {
		t.Errorf("block = %q, want the keys before the advisory score", body)
	}
	if !strings.Contains(body, "(1 of 3 shown)") {
		t.Errorf("block = %q, want the truncation line", body)
	}
	if !strings.Contains(errOut.String(), "zotio items get P1") || !strings.Contains(errOut.String(), "will not merge them") {
		t.Errorf("next-step line = %q, want the compare command and the resolver caveat", errOut.String())
	}
}

// A perfect score under a heading calling the titles different was the exact
// contradiction the `items find` near-title work removed, so the heading must
// state what 1.00 really means: equal in the store apart from folding.
func TestPrintNearDuplicateTitleGroupsExplainsAPerfectScore(t *testing.T) {
	cmd := newItemsDuplicatesCmd(&rootFlags{})
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)

	if err := printNearDuplicateTitleGroups(cmd, []nearDuplicateTitleGroup{
		newNearDuplicateTitleGroup(1, "journalArticle", []string{"Attention Is All You Need", "Attention is all you need."}, []string{"P1", "P2"}),
	}, 1); err != nil {
		t.Fatalf("printNearDuplicateTitleGroups: %v", err)
	}
	if body := out.String(); strings.Contains(body, "different titles") {
		t.Errorf("block = %q, want no claim that a 1.00 pair names different titles", body)
	}
	if body := out.String(); !strings.Contains(body, "equal apart from case, punctuation or Unicode form") {
		t.Errorf("block = %q, want the perfect score explained", body)
	}

	out.Reset()
	errOut.Reset()
	if err := printNearDuplicateTitleGroups(cmd, []nearDuplicateTitleGroup{
		newNearDuplicateTitleGroup(0.8, "journalArticle", []string{"A Survey of Reinforcement Learning", "A Survey of Reinforcement Methods"}, []string{"Q1", "Q2"}),
	}, 1); err != nil {
		t.Fatalf("printNearDuplicateTitleGroups: %v", err)
	}
	if body := out.String(); strings.Contains(body, "equal apart from") {
		t.Errorf("block = %q, want the fold-equality note withheld when no row scored 1.00", body)
	}
}

// The human branch replaces a bare `[]` with a sentence, because `[]` cannot be
// told apart from a broken command — the silence `matched: none` removed from
// `items find`. That branch needs a real terminal, so what is pinned here is
// the half a test writer can reach and the half that would break callers: a
// piped run still emits the exact-group array, sentence or not.
func TestItemsDuplicatesKeepsTheExactArrayForMachineReaders(t *testing.T) {
	seedDuplicateResolveStore(t, nearDuplicatePairFixture())

	out, _ := runItemsDuplicatesReport(t, &rootFlags{}, "--by", "title")
	if !strings.Contains(out, "[]") {
		t.Errorf("piped stdout = %q, want the exact-group array machine callers parse", out)
	}
	if strings.Contains(out, "no exact duplicate groups") {
		t.Errorf("piped stdout = %q, want the human sentence withheld from a machine format", out)
	}
}

// `items duplicates resolve --title` merges what it is given, so an advisory
// row must be unable to reach it. The exact pair in the same fixture proves the
// resolver is otherwise working, not that the fixture was empty.
func TestItemsDuplicatesResolveCannotMergeNearTitleGroup(t *testing.T) {
	seedDuplicateResolveStore(t, []json.RawMessage{
		json.RawMessage(`{"key":"N1","version":10,"data":{"key":"N1","itemType":"journalArticle","title":"Attention Is All You Need"}}`),
		json.RawMessage(`{"key":"N2","version":11,"data":{"key":"N2","itemType":"journalArticle","title":"Attention is all you need."}}`),
		json.RawMessage(`{"key":"X1","version":12,"data":{"key":"X1","itemType":"journalArticle","title":"Deep Residual Learning"}}`),
		json.RawMessage(`{"key":"X2","version":13,"data":{"key":"X2","itemType":"journalArticle","title":"Deep Residual Learning"}}`),
	})
	srv := newDuplicateResolveTestServer(t, map[string]int{"N1": 10, "N2": 11, "X1": 12, "X2": 13}, map[string]map[string]any{
		"N1": {"key": "N1", "itemType": "journalArticle", "title": "Attention Is All You Need"},
		"N2": {"key": "N2", "itemType": "journalArticle", "title": "Attention is all you need."},
		"X1": {"key": "X1", "itemType": "journalArticle", "title": "Deep Residual Learning"},
		"X2": {"key": "X2", "itemType": "journalArticle", "title": "Deep Residual Learning"},
	})

	env := runItemsDuplicatesResolveTestCmd(t, srv, &rootFlags{asJSON: true, maxChanges: -1}, "resolve", "--title")
	if len(env.Plan.Operations) != 1 {
		t.Fatalf("planned ops = %+v, want only the exact pair", env.Plan.Operations)
	}
	if env.Plan.Operations[0].Key != "X2" {
		t.Errorf("planned key = %q, want X2 (the exact duplicate)", env.Plan.Operations[0].Key)
	}
	for _, op := range env.Plan.Operations {
		if op.Key == "N1" || op.Key == "N2" {
			t.Errorf("op %+v would merge a near-match pair", op)
		}
	}
}

// PRISMA duplicate-removal counts are published numbers. They must not move
// because the detector learned to see near matches, so the counters are pinned
// byte for byte: this literal is the output of the same fixture before the near
// pass existed.
func TestLibraryPrismaDuplicateCountsIgnoreNearTitles(t *testing.T) {
	db := seedPrismaStore(t,
		json.RawMessage(`{"key":"N1","version":1,"data":{"key":"N1","itemType":"journalArticle","title":"Attention Is All You Need","libraryCatalog":"PubMed"}}`),
		json.RawMessage(`{"key":"N2","version":1,"data":{"key":"N2","itemType":"journalArticle","title":"Attention is all you need.","libraryCatalog":"Scopus"}}`),
		json.RawMessage(`{"key":"X1","version":1,"data":{"key":"X1","itemType":"journalArticle","title":"Deep Residual Learning","libraryCatalog":"PubMed"}}`),
		json.RawMessage(`{"key":"X2","version":1,"data":{"key":"X2","itemType":"journalArticle","title":"Deep Residual Learning","libraryCatalog":"Scopus"}}`),
	)

	report := prismaReportForTest(t, db, "library", "all")
	counters, err := json.Marshal(map[string]any{
		"identified":                  report.Identified.Total,
		"duplicate_clusters":          report.DuplicateClusters,
		"duplicate_records_removed":   report.DuplicateRecordsRemoved,
		"records_after_deduplication": report.RecordsAfterDeduplication,
	})
	if err != nil {
		t.Fatalf("marshal counters: %v", err)
	}
	const want = `{"duplicate_clusters":1,"duplicate_records_removed":1,"identified":4,"records_after_deduplication":3}`
	if string(counters) != want {
		t.Errorf("PRISMA duplicate counters = %s, want %s", counters, want)
	}
}

// `library health` grades a library and gates CI for some users, so an advisory
// judgement call must not become a finding there either.
func TestLibraryHealthDuplicateCandidatesIgnoreNearTitles(t *testing.T) {
	db := seedNearDuplicateStore(t,
		json.RawMessage(`{"key":"N1","version":1,"data":{"key":"N1","itemType":"journalArticle","title":"Attention Is All You Need"}}`),
		json.RawMessage(`{"key":"N2","version":1,"data":{"key":"N2","itemType":"journalArticle","title":"Attention is all you need."}}`),
	)

	findings, _, err := runDuplicateCandidates(db, &healthContext{src: FindingSource{Kind: "local"}, flags: &rootFlags{}})
	if err != nil {
		t.Fatalf("runDuplicateCandidates: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("health findings = %+v, want none for a near-match pair", findings)
	}
}

func seedNearDuplicateStore(t *testing.T, items ...json.RawMessage) localQueryStore {
	t.Helper()
	db, err := store.OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed items: %v", err)
	}
	return localQueryStore{Store: db}
}

func nearDuplicatePairFixture() []json.RawMessage {
	return []json.RawMessage{
		json.RawMessage(`{"key":"P1","version":1,"data":{"key":"P1","itemType":"journalArticle","title":"Attention Is All You Need"}}`),
		json.RawMessage(`{"key":"P2","version":1,"data":{"key":"P2","itemType":"journalArticle","title":"Attention is all you need."}}`),
	}
}

func runItemsDuplicatesReport(t *testing.T, flags *rootFlags, args ...string) (string, string) {
	t.Helper()
	cmd := newItemsDuplicatesCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("items duplicates %v: %v; stderr=%s", args, err, errOut.String())
	}
	return out.String(), errOut.String()
}

type duplicatesReportPayload struct {
	Groups     []map[string]any          `json:"groups"`
	NearGroups []nearDuplicateTitleGroup `json:"near_title_groups"`
	Findings   []Finding                 `json:"findings"`
	Meta       map[string]any            `json:"meta"`
}

func decodeDuplicatesReport(t *testing.T, out string) duplicatesReportPayload {
	t.Helper()
	var payload duplicatesReportPayload
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("decode report %q: %v", out, err)
	}
	return payload
}
