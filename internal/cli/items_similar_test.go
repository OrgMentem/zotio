// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"zotio/internal/store"
)

func TestBuildItemSimilarReportScoresExplainableSignals(t *testing.T) {
	db := seedItemsSimilarFixture(t, filepath.Join(t.TempDir(), "data.db"))
	defer db.Close()

	report, found, err := buildItemSimilarReport(localQueryStore{db}, "SRC", itemSimilarOptions{Limit: 10})
	if err != nil {
		t.Fatalf("buildItemSimilarReport: %v", err)
	}
	if !found {
		t.Fatal("source item not found")
	}
	if report.Source.Key != "SRC" || report.Source.Title != "Source Paper" {
		t.Fatalf("source = %+v, want SRC Source Paper", report.Source)
	}
	if len(report.Similar) != 1 {
		t.Fatalf("similar count = %d, want 1 zero-score candidates excluded: %+v", len(report.Similar), report.Similar)
	}

	got := report.Similar[0]
	if got.Key != "SIM" || got.Rank != 1 {
		t.Fatalf("top entry = %+v, want SIM rank 1", got)
	}
	wantScore := (1.0/3.0)*itemSimilarCollectionWeight +
		(1.0/3.0)*itemSimilarTagWeight +
		(1.0/2.0)*itemSimilarCreatorWeight +
		itemSimilarVenueWeight +
		(1.0/2.0)*itemSimilarTextWeight
	if math.Abs(got.Score-wantScore) > 0.000001 {
		t.Fatalf("score = %.8f, want %.8f", got.Score, wantScore)
	}
	if math.Abs(got.Signals.Collections.Score-(1.0/3.0)) > 0.000001 {
		t.Errorf("collections score = %.8f, want 1/3", got.Signals.Collections.Score)
	}
	if math.Abs(got.Signals.Tags.Score-(1.0/3.0)) > 0.000001 {
		t.Errorf("tags score = %.8f, want 1/3", got.Signals.Tags.Score)
	}
	if math.Abs(got.Signals.Creators.Score-(1.0/2.0)) > 0.000001 {
		t.Errorf("creators score = %.8f, want 1/2 from normalized Jane Smith", got.Signals.Creators.Score)
	}
	if got.Signals.Venue.Score != 1 {
		t.Errorf("venue score = %.8f, want binary match", got.Signals.Venue.Score)
	}
	if math.Abs(got.Signals.Text.Score-(1.0/2.0)) > 0.000001 {
		t.Errorf("text score = %.8f, want rare-word overlap 1/2", got.Signals.Text.Score)
	}

	joinedReasons := strings.Join(got.Reasons, " | ")
	for _, want := range []string{"1 shared collection (C1)", "1 shared tag (RL)", "same venue", "50% text overlap", "1 shared creator (Jane Smith)"} {
		if !strings.Contains(joinedReasons, want) {
			t.Errorf("reasons %q missing %q", joinedReasons, want)
		}
	}
}

func TestBuildItemSimilarReportLimitAndMinScore(t *testing.T) {
	db := seedItemsSimilarFixture(t, filepath.Join(t.TempDir(), "data.db"))
	defer db.Close()

	report, found, err := buildItemSimilarReport(localQueryStore{db}, "SRC", itemSimilarOptions{Limit: 1, MinScore: 0.40})
	if err != nil {
		t.Fatalf("buildItemSimilarReport: %v", err)
	}
	if !found {
		t.Fatal("source item not found")
	}
	if len(report.Similar) != 1 || report.Similar[0].Key != "SIM" {
		t.Fatalf("filtered report = %+v, want only SIM", report.Similar)
	}

	report, found, err = buildItemSimilarReport(localQueryStore{db}, "SRC", itemSimilarOptions{Limit: 10, MinScore: 0.60})
	if err != nil {
		t.Fatalf("buildItemSimilarReport high threshold: %v", err)
	}
	if !found {
		t.Fatal("source item not found at high threshold")
	}
	if len(report.Similar) != 0 {
		t.Fatalf("high min-score report = %+v, want empty", report.Similar)
	}
}

func TestItemsSimilarCommandJSONIsStable(t *testing.T) {
	isolateItemsSimilarStore(t)
	db := seedItemsSimilarFixture(t, defaultDBPath("zotio"))
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	first := runItemsSimilarCommand(t, &rootFlags{asJSON: true}, "similar", "SRC", "--limit", "1")
	second := runItemsSimilarCommand(t, &rootFlags{asJSON: true}, "similar", "SRC", "--limit", "1")
	if !bytes.Equal(first, second) {
		t.Fatalf("JSON output is not deterministic\nfirst: %s\nsecond: %s", first, second)
	}

	var report itemSimilarReport
	if err := json.Unmarshal(first, &report); err != nil {
		t.Fatalf("decode JSON %q: %v", string(first), err)
	}
	if report.Source.Key != "SRC" || len(report.Similar) != 1 {
		t.Fatalf("report = %+v, want source SRC and one hit", report)
	}
	entry := report.Similar[0]
	if entry.Signals.Collections.Reason == "" || entry.Signals.Text.Reason == "" || len(entry.Reasons) == 0 {
		t.Fatalf("JSON entry missing signal reasons: %+v", entry)
	}
}

func TestItemsSimilarCommandTextOutputExplainsWhy(t *testing.T) {
	isolateItemsSimilarStore(t)
	db := seedItemsSimilarFixture(t, defaultDBPath("zotio"))
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	out := string(runItemsSimilarCommand(t, &rootFlags{}, "similar", "SRC", "--limit", "1"))
	for _, want := range []string{"RANK", "SCORE", "SIM", "0.46", "1 shared collection (C1)", "50% text overlap"} {
		if !strings.Contains(out, want) {
			t.Fatalf("text output %q missing %q", out, want)
		}
	}
}

func TestItemsSimilarNoStoreRefusesLoudly(t *testing.T) {
	isolateItemsSimilarStore(t)
	flags := &rootFlags{}
	cmd := newItemsCmd(flags)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"similar", "SRC"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "run 'zotio sync' first") {
		t.Fatalf("err = %v, want loud sync precondition refusal", err)
	}
}

func TestItemSimilarTextSignalNotesMissingFulltext(t *testing.T) {
	source := itemSimilarRecord{itemSimilarSummary: itemSimilarSummary{Key: "SRC"}, Fulltext: itemSimilarFulltext{}}
	candidate := itemSimilarRecord{itemSimilarSummary: itemSimilarSummary{Key: "CAND"}, Collections: map[string]string{"c1": "C1"}}
	source.Collections = map[string]string{"c1": "C1"}
	entry := scoreItemSimilarCandidate(source, candidate)
	if entry.Signals.Text.Score != 0 || !strings.Contains(entry.Signals.Text.Reason, "source has no synced fulltext") {
		t.Fatalf("text signal = %+v, want explicit missing-source-fulltext note", entry.Signals.Text)
	}
	if !containsString(entry.Reasons, "source has no synced fulltext") {
		t.Fatalf("entry reasons = %v, want missing fulltext note", entry.Reasons)
	}
}

func TestNormalizeItemSimilarCreatorCanonicalizesFieldModes(t *testing.T) {
	fromName, _ := normalizeItemSimilarCreator("", "", "  Marie   Curie ")
	fromParts, _ := normalizeItemSimilarCreator("Marie", "Curie", "")
	reorderedName, _ := normalizeItemSimilarCreator("", "", "CURIE marie")
	if fromName != fromParts || fromName != reorderedName || fromName != "curie marie" {
		t.Fatalf("creator identities = name %q, parts %q, reordered %q; want %q", fromName, fromParts, reorderedName, "curie marie")
	}

	rec, err := itemSimilarRecordFromRaw(json.RawMessage(`{"key":"X","data":{"key":"X","itemType":"book","creators":[{"creatorType":"editor","name":"Marie Curie"},{"creatorType":"author","firstName":"Marie","lastName":"Curie"}]}}`))
	if err != nil {
		t.Fatalf("parse creators: %v", err)
	}
	if len(rec.Creators) != 1 {
		t.Fatalf("creator identities = %v, want creatorType-independent single identity", rec.Creators)
	}
}

func TestBuildItemSimilarFulltextCorpusCountsEmptyDocuments(t *testing.T) {
	db, err := store.OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "empty-fulltext.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	items := []json.RawMessage{
		json.RawMessage(`{"key":"SRC","data":{"key":"SRC","itemType":"journalArticle"}}`),
		json.RawMessage(`{"key":"OTHER","data":{"key":"OTHER","itemType":"journalArticle"}}`),
		json.RawMessage(`{"key":"EMPTY","data":{"key":"EMPTY","itemType":"journalArticle"}}`),
		json.RawMessage(`{"key":"ASRC","data":{"key":"ASRC","itemType":"attachment","parentItem":"SRC"}}`),
		json.RawMessage(`{"key":"AOTHER","data":{"key":"AOTHER","itemType":"attachment","parentItem":"OTHER"}}`),
		json.RawMessage(`{"key":"AEMPTY","data":{"key":"AEMPTY","itemType":"attachment","parentItem":"EMPTY"}}`),
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed items: %v", err)
	}
	if err := db.UpsertKeyed("fulltext", []string{"ASRC", "AOTHER", "AEMPTY"}, []json.RawMessage{
		json.RawMessage(`{"content":"alpha"}`),
		json.RawMessage(`{"content":"beta"}`),
		json.RawMessage(`{"content":"a ! 2"}`),
	}); err != nil {
		t.Fatalf("seed fulltext: %v", err)
	}

	corpus, err := buildItemSimilarFulltextCorpus(db, "SRC")
	if err != nil {
		t.Fatalf("build corpus: %v", err)
	}
	if corpus.DocumentCount != 3 {
		t.Fatalf("document count = %d, want 3 including empty-token document", corpus.DocumentCount)
	}
	if _, ok := corpus.Source.Rare["alpha"]; !ok {
		t.Fatalf("source rare terms = %v, want alpha rare with 1/3 document frequency", corpus.Source.Rare)
	}
	if got := itemSimilarTextReason(0, 0, itemSimilarFulltext{Present: true}, itemSimilarFulltext{Present: true, Usable: true}); got != "source fulltext has no usable terms" {
		t.Fatalf("empty source reason = %q", got)
	}
	if got := itemSimilarTextReason(0, 0, itemSimilarFulltext{Present: true, Usable: true}, itemSimilarFulltext{Present: true}); got != "candidate fulltext has no usable terms" {
		t.Fatalf("empty candidate reason = %q", got)
	}
}

func TestBuildItemSimilarReportRejectsTrashedSourceCoexistence(t *testing.T) {
	db := seedItemsSimilarFixture(t, filepath.Join(t.TempDir(), "trash-source.db"))
	defer db.Close()
	if _, _, err := db.UpsertBatch("items-trash", []json.RawMessage{
		json.RawMessage(`{"key":"SRC","data":{"key":"SRC","itemType":"journalArticle"}}`),
	}); err != nil {
		t.Fatalf("seed trash source: %v", err)
	}

	_, _, err := buildItemSimilarReport(localQueryStore{db}, "SRC", itemSimilarOptions{Limit: 10})
	if err == nil || !strings.Contains(err.Error(), "item is in trash") {
		t.Fatalf("error = %v, want loud trash error", err)
	}
}

func seedItemsSimilarFixture(t *testing.T, dbPath string) *store.Store {
	t.Helper()
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	items := []json.RawMessage{
		json.RawMessage(`{"key":"SRC","version":1,"data":{"key":"SRC","itemType":"journalArticle","title":"Source Paper","publicationTitle":"Journal of Tests","collections":["C1","C2"],"tags":[{"tag":"RL"},{"tag":"Methods"}],"creators":[{"creatorType":"author","firstName":"Jane","lastName":"Smith"},{"creatorType":"author","name":"AI Lab"}]}}`),
		json.RawMessage(`{"key":"SIM","version":1,"data":{"key":"SIM","itemType":"journalArticle","title":"Similar Paper","publicationTitle":" journal of tests ","collections":["C1","C3"],"tags":[{"tag":"rl"},{"tag":"Other"}],"creators":[{"creatorType":"author","firstName":"JANE","lastName":"SMITH"}]}}`),
		json.RawMessage(`{"key":"ZERO","version":1,"data":{"key":"ZERO","itemType":"journalArticle","title":"Different Paper","publicationTitle":"Other Venue","collections":["Z"],"tags":[{"tag":"Unrelated"}],"creators":[{"creatorType":"author","firstName":"Un","lastName":"Related"}]}}`),
		json.RawMessage(`{"key":"NOISE1","version":1,"data":{"key":"NOISE1","itemType":"journalArticle","title":"Noise One"}}`),
		json.RawMessage(`{"key":"NOISE2","version":1,"data":{"key":"NOISE2","itemType":"journalArticle","title":"Noise Two"}}`),
		json.RawMessage(`{"key":"NOISE3","version":1,"data":{"key":"NOISE3","itemType":"journalArticle","title":"Noise Three"}}`),
		json.RawMessage(`{"key":"ASRC","version":1,"data":{"key":"ASRC","itemType":"attachment","parentItem":"SRC","title":"PDF","contentType":"application/pdf"}}`),
		json.RawMessage(`{"key":"ASIM","version":1,"data":{"key":"ASIM","itemType":"attachment","parentItem":"SIM","title":"PDF","contentType":"application/pdf"}}`),
		json.RawMessage(`{"key":"AN1","version":1,"data":{"key":"AN1","itemType":"attachment","parentItem":"NOISE1","title":"PDF","contentType":"application/pdf"}}`),
		json.RawMessage(`{"key":"AN2","version":1,"data":{"key":"AN2","itemType":"attachment","parentItem":"NOISE2","title":"PDF","contentType":"application/pdf"}}`),
		json.RawMessage(`{"key":"AN3","version":1,"data":{"key":"AN3","itemType":"attachment","parentItem":"NOISE3","title":"PDF","contentType":"application/pdf"}}`),
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		db.Close()
		t.Fatalf("seed items: %v", err)
	}
	ids := []string{"ASRC", "ASIM", "AN1", "AN2", "AN3"}
	fulltexts := []json.RawMessage{
		json.RawMessage(`{"content":"bandit sourceonly of an"}`),
		json.RawMessage(`{"content":"bandit candidateonly to in"}`),
		json.RawMessage(`{"content":"noiseone common"}`),
		json.RawMessage(`{"content":"noisetwo common"}`),
		json.RawMessage(`{"content":"noisethree common"}`),
	}
	if err := db.UpsertKeyed("fulltext", ids, fulltexts); err != nil {
		db.Close()
		t.Fatalf("seed fulltext: %v", err)
	}
	return db
}

func isolateItemsSimilarStore(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTIO_DEMO", "0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	t.Setenv("ZOTERO_BASE_URL", "http://127.0.0.1:1/api/users/0")
	savedGroup := activeGroupID
	activeGroupID = ""
	t.Cleanup(func() { activeGroupID = savedGroup })
}

func runItemsSimilarCommand(t *testing.T, flags *rootFlags, args ...string) []byte {
	t.Helper()
	cmd := newItemsCmd(flags)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs(args)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("items command %v: %v; stdout=%s", args, err, out.String())
	}
	return out.Bytes()
}
