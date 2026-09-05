// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"zotio/internal/store"
)

func TestBuildItemSimilarReportScoresExplainableSignals(t *testing.T) {
	db := seedItemsSimilarFixture(t, filepath.Join(t.TempDir(), "data.db"))
	defer db.Close()

	report, found, err := buildItemSimilarReport(context.Background(), localQueryStore{db}, "SRC", itemSimilarOptions{Limit: 10})
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

	report, found, err := buildItemSimilarReport(context.Background(), localQueryStore{db}, "SRC", itemSimilarOptions{Limit: 1, MinScore: 0.40})
	if err != nil {
		t.Fatalf("buildItemSimilarReport: %v", err)
	}
	if !found {
		t.Fatal("source item not found")
	}
	if len(report.Similar) != 1 || report.Similar[0].Key != "SIM" {
		t.Fatalf("filtered report = %+v, want only SIM", report.Similar)
	}

	report, found, err = buildItemSimilarReport(context.Background(), localQueryStore{db}, "SRC", itemSimilarOptions{Limit: 10, MinScore: 0.60})
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
	db := seedItemsSimilarFixture(t, helpersTestDefaultDBPath(t, "zotio"))
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
	db := seedItemsSimilarFixture(t, helpersTestDefaultDBPath(t, "zotio"))
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
	if _, err := db.UpsertKeyed("fulltext", []string{"ASRC", "AOTHER", "AEMPTY"}, []json.RawMessage{
		json.RawMessage(`{"content":"alpha"}`),
		json.RawMessage(`{"content":"beta"}`),
		json.RawMessage(`{"content":"a ! 2"}`),
	}); err != nil {
		t.Fatalf("seed fulltext: %v", err)
	}

	corpus, err := buildItemSimilarFulltextCorpus(context.Background(), db, "SRC")
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

func TestBuildItemSimilarReportRejectsNewerTrashedSource(t *testing.T) {
	db := seedItemsSimilarFixture(t, filepath.Join(t.TempDir(), "trash-source.db"))
	defer db.Close()
	if _, _, err := db.UpsertBatch("items-trash", []json.RawMessage{
		json.RawMessage(`{"key":"SRC","version":2,"data":{"key":"SRC","version":2,"itemType":"journalArticle"}}`),
	}); err != nil {
		t.Fatalf("seed trash source: %v", err)
	}

	_, _, err := buildItemSimilarReport(context.Background(), localQueryStore{db}, "SRC", itemSimilarOptions{Limit: 10})
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
	if _, err := db.UpsertKeyed("fulltext", ids, fulltexts); err != nil {
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
	savedGroup := activeGroupIDLocked()
	setActiveGroupID("")
	t.Cleanup(func() { setActiveGroupID(savedGroup) })
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

// tabwriterColumnGap matches the run of spaces newTabWriter puts between two
// cells. Its padding is 2, so two or more spaces is always a column break and
// never content, as long as a fixture keeps single spaces inside a cell.
var tabwriterColumnGap = regexp.MustCompile("  +")

// assertOneRowPerRecord fails unless the table prints exactly one line per
// record, each with the same number of columns as its header, and nothing
// record-shaped after them. Those are the two shapes a control byte in a cell
// destroys: a newline inside a cell adds a line the record does not own and
// splits its cells across two lines, and a tab inside a cell opens a column,
// which pushes every later value one header to the right. header is the first
// header word; trailing prose such as the gaps summary is not record-shaped,
// so it is ignored. printItemRelatedReport, printCollectionGapsReport and
// printReadingList assert with this too.
func assertOneRowPerRecord(t *testing.T, where, body, header string, records int) {
	t.Helper()
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	at := -1
	for i, line := range lines {
		if strings.HasPrefix(line, header) {
			at = i
			break
		}
	}
	if at < 0 {
		t.Fatalf("%s: no header row starting with %q:\n%q", where, header, body)
	}
	columns := func(line string) int {
		return len(tabwriterColumnGap.Split(strings.TrimRight(line, " "), -1))
	}
	want := columns(lines[at])
	for i := range records {
		row := at + 1 + i
		if row >= len(lines) {
			t.Fatalf("%s: record %d has no row; the table is %d lines:\n%q", where, i+1, len(lines), body)
		}
		if got := columns(lines[row]); got != want {
			t.Fatalf("%s: row %d has %d columns, want %d, so a cell opened a column of its own:\n%q", where, i+1, got, want, body)
		}
	}
	for i, line := range lines[min(at+1+records, len(lines)):] {
		if columns(line) == want {
			t.Fatalf("%s: line %d after the %d records is record-shaped, so a cell forged a row:\n%q", where, i+1, records, body)
		}
	}
}

// A stored title, a stored item key and every reason quoting a collection, a
// tag, a creator or a venue are all publisher- or user-supplied text, so the
// similar table must render each of them as inert data.
func TestPrintItemSimilarReportRendersHostileLibraryTextAsInertData(t *testing.T) {
	cmd := &cobra.Command{}
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)

	report := itemSimilarReport{
		Source: itemSimilarSummary{Key: "SRC"},
		Similar: []itemSimilarEntry{
			{Rank: 1, Key: "SIM", Title: "Similar Paper", Score: 0.46, Reasons: []string{"1 shared collection (C1)", "same venue (Journal of Tests)"}},
			{Rank: 2, Key: "EVIL\x1b[32m", Title: hostileLibraryText, Score: 0.31, Reasons: []string{"1 shared tag (" + hostileLibraryText + ")", "same venue (V\tW)"}},
		},
	}
	if err := printItemSimilarReport(cmd, report); err != nil {
		t.Fatalf("printItemSimilarReport: %v", err)
	}
	body := out.String()
	assertNoTerminalInjection(t, "items similar", body)
	assertNoTerminalInjection(t, "items similar stderr", errOut.String())
	assertAdvisoryRowShape(t, "items similar", body, []string{"0.46", "0.31"})
	assertOneRowPerRecord(t, "items similar", body, "RANK", 2)
	// Each reason is folded on its own, so the cap bounds one quoted library
	// value at a time and the signals behind a hostile one still print. Folding
	// the joined cell instead would leave the column unable to answer why.
	if !strings.Contains(body, "1 shared tag (") {
		t.Fatalf("the WHY column dropped the tag signal:\n%q", body)
	}
	if !strings.Contains(body, "same venue (V W)") {
		t.Fatalf("the WHY column dropped the venue signal behind the hostile one, or left its tab:\n%q", body)
	}
}

// The empty-result line names the source key, which is stored text too.
func TestPrintItemSimilarReportSanitizesTheSourceKeyOnTheEmptyLine(t *testing.T) {
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	if err := printItemSimilarReport(cmd, itemSimilarReport{Source: itemSimilarSummary{Key: hostileLibraryText}}); err != nil {
		t.Fatalf("printItemSimilarReport: %v", err)
	}
	assertNoTerminalInjection(t, "items similar empty", out.String())
	if lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n"); len(lines) != 1 {
		t.Fatalf("the empty answer is %d lines, so the key forged one:\n%q", len(lines), out.String())
	}
}

// Folding is display-only. --json carries the stored bytes, because a consumer
// diffing a title against its own record needs them, and an escape sequence
// inside a JSON string is inert. The same store drives both paths here, so the
// human table cannot be safe by having lost the data.
func TestItemsSimilarKeepsHostileLibraryTextByteIdenticalInJSON(t *testing.T) {
	isolateItemsSimilarStore(t)
	db, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	items := make([]json.RawMessage, 0, 3)
	for _, item := range []struct {
		key, title string
	}{
		{"SRC", "Source Paper"},
		{"EVIL", hostileLibraryText},
		{"SIM", "Similar Paper"},
	} {
		raw, err := json.Marshal(map[string]any{
			"key":     item.key,
			"version": 1,
			"data": map[string]any{
				"key":         item.key,
				"itemType":    "journalArticle",
				"title":       item.title,
				"collections": []string{"C1"},
				"tags":        []map[string]string{{"tag": hostileLibraryText}},
			},
		})
		if err != nil {
			t.Fatalf("marshal %s: %v", item.key, err)
		}
		items = append(items, raw)
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		db.Close()
		t.Fatalf("seed items: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	raw := runItemsSimilarCommand(t, &rootFlags{asJSON: true}, "similar", "SRC", "--limit", "5")
	var report itemSimilarReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatalf("decode JSON %q: %v", raw, err)
	}
	var hostile *itemSimilarEntry
	for i := range report.Similar {
		if report.Similar[i].Key == "EVIL" {
			hostile = &report.Similar[i]
		}
	}
	if hostile == nil {
		t.Fatalf("EVIL is not in the JSON report: %+v", report.Similar)
	}
	if hostile.Title != hostileLibraryText {
		t.Fatalf("JSON title = %q, want the stored bytes %q", hostile.Title, hostileLibraryText)
	}
	if joined := strings.Join(hostile.Reasons, "\n"); !strings.Contains(joined, hostileLibraryText) {
		t.Fatalf("JSON reasons dropped or folded the stored tag: %q", joined)
	}
	if !bytes.Contains(raw, []byte(`\u001b`)) {
		t.Fatalf("JSON does not carry the escape as its own \\u001b, so the value was rewritten:\n%s", raw)
	}
	if bytes.Contains(raw, []byte("\uFFFD")) {
		t.Fatalf("JSON carries a replacement rune, so display folding leaked into the data path:\n%s", raw)
	}

	body := string(runItemsSimilarCommand(t, &rootFlags{}, "similar", "SRC", "--limit", "5"))
	assertNoTerminalInjection(t, "items similar end to end", body)
	assertOneRowPerRecord(t, "items similar end to end", body, "RANK", len(report.Similar))
}
