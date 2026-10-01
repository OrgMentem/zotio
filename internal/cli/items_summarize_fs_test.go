// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"zotio/internal/store"
)

const fsFocusTerms = "quantum annealing"

// fsSeedFocusStore seeds collection FC with three items. F1 has two PDFs: FA1
// opens with more than 8,000 characters of introduction and states the focus
// phrase only after it, and FA1B states it near the top. F1's annotations are
// one matching highlight on p.12 and one early, unrelated highlight on p.1. F2
// has an indexed PDF whose long introduction never mentions the terms. F3 has
// no synced PDF text at all.
//
// OUT, outside the collection, matches the terms more strongly than anything in
// FC, so any leak of unselected items shows up first.
func fsSeedFocusStore(t *testing.T) {
	t.Helper()
	isolateDemoEnv(t, "0")
	db, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := db.UpsertBatch("items", []json.RawMessage{
		json.RawMessage(`{"key":"F1","version":1,"data":{"key":"F1","itemType":"journalArticle","title":"Alpha Focus","abstractNote":"A","DOI":"10.1/f1","collections":["FC"]}}`),
		json.RawMessage(`{"key":"F2","version":1,"data":{"key":"F2","itemType":"journalArticle","title":"Beta Distractor","abstractNote":"B","DOI":"10.1/f2","collections":["FC"]}}`),
		json.RawMessage(`{"key":"F3","version":1,"data":{"key":"F3","itemType":"journalArticle","title":"Gamma Unindexed","abstractNote":"G","DOI":"10.1/f3","collections":["FC"]}}`),
		json.RawMessage(`{"key":"OUT","version":1,"data":{"key":"OUT","itemType":"journalArticle","title":"Outside","abstractNote":"O","DOI":"10.1/out"}}`),
		json.RawMessage(`{"key":"FA1","version":1,"data":{"key":"FA1","itemType":"attachment","parentItem":"F1","contentType":"application/pdf"}}`),
		json.RawMessage(`{"key":"FA1B","version":1,"data":{"key":"FA1B","itemType":"attachment","parentItem":"F1","contentType":"application/pdf"}}`),
		json.RawMessage(`{"key":"FA2","version":1,"data":{"key":"FA2","itemType":"attachment","parentItem":"F2","contentType":"application/pdf"}}`),
		json.RawMessage(`{"key":"AOUT","version":1,"data":{"key":"AOUT","itemType":"attachment","parentItem":"OUT","contentType":"application/pdf"}}`),
		json.RawMessage(`{"key":"ANN1","version":1,"data":{"key":"ANN1","itemType":"annotation","parentItem":"FA1","annotationType":"highlight","annotationText":"the quantum annealing schedule decides the outcome","annotationPageLabel":"12","dateAdded":"2026-01-02T00:00:00Z"}}`),
		json.RawMessage(`{"key":"ANN2","version":1,"data":{"key":"ANN2","itemType":"annotation","parentItem":"FA1","annotationType":"highlight","annotationText":"an early introduction highlight","annotationPageLabel":"1","dateAdded":"2026-01-01T00:00:00Z"}}`),
		json.RawMessage(`{"key":"ANNOUT","version":1,"data":{"key":"ANNOUT","itemType":"annotation","parentItem":"AOUT","annotationType":"highlight","annotationText":"quantum annealing outside the cohort","annotationPageLabel":"3","dateAdded":"2026-01-03T00:00:00Z"}}`),
	}); err != nil {
		t.Fatalf("seed items: %v", err)
	}
	intro := strings.Repeat("Introduction background context. ", 300)
	if utf8.RuneCountInString(intro) <= 8000 {
		t.Fatalf("fixture introduction is %d characters, want more than 8000", utf8.RuneCountInString(intro))
	}
	docs := map[string]string{
		"FA1":  intro + "Later we show that the quantum annealing schedule decides the outcome. " + strings.Repeat("Closing remarks. ", 20),
		"FA1B": "Summary: quantum annealing is revisited in this appendix. " + strings.Repeat("Appendix tables. ", 40),
		"FA2":  strings.Repeat("Irrelevant introduction about history. ", 300),
		"AOUT": "quantum annealing quantum annealing quantum annealing",
	}
	keys := []string{"FA1", "FA1B", "FA2", "AOUT"}
	payloads := make([]json.RawMessage, 0, len(keys))
	for _, key := range keys {
		raw, err := json.Marshal(map[string]string{"content": docs[key]})
		if err != nil {
			t.Fatalf("encode %s: %v", key, err)
		}
		payloads = append(payloads, raw)
	}
	if _, err := db.UpsertKeyed("fulltext", keys, payloads); err != nil {
		t.Fatalf("seed full text: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
}

func fsDecodeCohortBundle(t *testing.T, out string) map[string]summarizeBundle {
	t.Helper()
	var cb summarizeCollectionBundle
	if err := json.Unmarshal([]byte(out), &cb); err != nil {
		t.Fatalf("decode bundle %q: %v", out, err)
	}
	byKey := make(map[string]summarizeBundle, len(cb.Items))
	for _, item := range cb.Items {
		byKey[item.Key] = item
	}
	return byKey
}

func fsFulltextPassages(b summarizeBundle) []summarizeFocusPassage {
	var out []summarizeFocusPassage
	if b.Focus == nil {
		return out
	}
	for _, p := range b.Focus.Passages {
		if p.Source == "fulltext" {
			out = append(out, p)
		}
	}
	return out
}

// A phrase past the first 8,000 characters is invisible to the first-pages
// excerpt. --focus must select it within a 500-character budget, attribute it,
// and report the distractor and the unindexed item honestly.
func TestFsSummarizeFocusSelectsLatePassageWithinBudget(t *testing.T) {
	fsSeedFocusStore(t)

	plain, err := runSummarize(t, &rootFlags{asJSON: true}, "--scope", "collection:FC", "--max-chars", "500")
	if err != nil {
		t.Fatalf("summarize without --focus: %v", err)
	}
	if strings.Contains(plain, `"focus"`) {
		t.Fatalf("no-focus bundle grew a focus section:\n%s", plain)
	}
	if f1 := fsDecodeCohortBundle(t, plain)["F1"]; f1.Fulltext == "" || strings.Contains(f1.Fulltext, "quantum annealing schedule") {
		t.Fatalf("no-focus F1 excerpt = %q, want a first-pages excerpt without the late phrase", f1.Fulltext)
	}

	out, err := runSummarize(t, &rootFlags{asJSON: true}, "--scope", "collection:FC", "--focus", fsFocusTerms, "--max-chars", "500")
	if err != nil {
		t.Fatalf("summarize --focus: %v (exit %d)", err, ExitCode(err))
	}
	if strings.Contains(out, "OUT") || strings.Contains(out, "AOUT") {
		t.Fatalf("focus bundle leaked the item outside the scope:\n%s", out)
	}
	items := fsDecodeCohortBundle(t, out)
	if len(items) != 3 {
		t.Fatalf("bundle items = %d, want F1, F2 and F3", len(items))
	}

	f1 := items["F1"]
	if f1.Focus == nil || f1.Focus.Fulltext != focusFulltextMatched || f1.Focus.NoHit || f1.Focus.Terms != fsFocusTerms {
		t.Fatalf("F1 focus = %+v, want matched terms %q", f1.Focus, fsFocusTerms)
	}
	if f1.Fulltext != "" || len(f1.Annotations) != 0 {
		t.Fatalf("F1 kept the first-pages excerpt or page-ordered annotations in focus mode: %+v", f1)
	}
	budget := 0
	var late bool
	for _, p := range fsFulltextPassages(f1) {
		budget += utf8.RuneCountInString(p.Text)
		if p.ItemKey != "F1" || (p.AttachmentKey != "FA1" && p.AttachmentKey != "FA1B") {
			t.Fatalf("passage %+v is not attributed to F1 and one of its PDFs", p)
		}
		if p.AttachmentKey == "FA1" && strings.Contains(p.Text, "quantum annealing schedule") {
			late = true
		}
	}
	if !late {
		t.Fatalf("F1 passages = %+v, want FA1's phrase from after character 8000", fsFulltextPassages(f1))
	}
	if budget > 500 {
		t.Fatalf("F1 fulltext passages total %d characters, want at most --max-chars 500", budget)
	}
	var annotationKeys []string
	for _, p := range f1.Focus.Passages {
		if p.Source == "annotation" {
			annotationKeys = append(annotationKeys, p.AnnotationKey)
			if p.ItemKey != "F1" || p.AttachmentKey != "FA1" || p.Page != "12" {
				t.Fatalf("annotation passage %+v, want F1/FA1 p.12", p)
			}
		}
	}
	if strings.Join(annotationKeys, ",") != "ANN1" {
		t.Fatalf("annotation passages = %v, want only the matching ANN1", annotationKeys)
	}

	f2 := items["F2"]
	if f2.Focus == nil || f2.Focus.Fulltext != focusFulltextNoMatch || !f2.Focus.NoHit || len(f2.Focus.Passages) != 0 {
		t.Fatalf("F2 focus = %+v, want an explicit no-hit over searched text", f2.Focus)
	}
	if f2.Fulltext != "" {
		t.Fatalf("F2 carried its unrelated introduction in focus mode: %q", f2.Fulltext)
	}

	f3 := items["F3"]
	if f3.Focus == nil || f3.Focus.Fulltext != focusFulltextNotIndexed || !f3.Focus.NoHit {
		t.Fatalf("F3 focus = %+v, want not_indexed rather than a searched no-match", f3.Focus)
	}
	if !strings.Contains(strings.Join(f3.Gaps, ","), "no fulltext") {
		t.Fatalf("F3 gaps = %v, want the no fulltext gap", f3.Gaps)
	}
	if strings.Contains(strings.Join(f2.Gaps, ","), "no fulltext") {
		t.Fatalf("F2 gaps = %v; its text is indexed", f2.Gaps)
	}

	again, err := runSummarize(t, &rootFlags{asJSON: true}, "--scope", "collection:FC", "--focus", fsFocusTerms, "--max-chars", "500")
	if err != nil {
		t.Fatalf("second summarize --focus: %v", err)
	}
	if again != out {
		t.Fatalf("focus ranking is not deterministic:\n%s\n---\n%s", out, again)
	}
}

// The budget is shared by every PDF passage of an item: the passage that
// would overflow it is cut, later ones are dropped, and truncation is reported
// through the existing fields. The annotation cap reports the same way.
func TestFsSummarizeFocusBudgetsAndTruncation(t *testing.T) {
	fsSeedFocusStore(t)

	out, err := runSummarize(t, &rootFlags{asJSON: true}, "F1", "--focus", fsFocusTerms, "--max-chars", "40", "--max-annotations", "1")
	if err != nil {
		t.Fatalf("summarize F1 --focus: %v", err)
	}
	var b summarizeBundle
	if err := json.Unmarshal([]byte(out), &b); err != nil {
		t.Fatalf("decode bundle %q: %v", out, err)
	}
	total := 0
	for _, p := range fsFulltextPassages(b) {
		total += utf8.RuneCountInString(p.Text)
	}
	if total == 0 || total > 40 {
		t.Fatalf("fulltext passages total %d characters, want 1..40", total)
	}
	if !b.Truncated.Fulltext {
		t.Fatalf("truncated = %+v, want fulltext truncation reported", b.Truncated)
	}

	// Both F1 highlights match "introduction OR annealing"; one is kept.
	out, err = runSummarize(t, &rootFlags{asJSON: true}, "F1", "--focus", "introduction OR annealing", "--max-annotations", "1", "--no-fulltext")
	if err != nil {
		t.Fatalf("summarize F1 --focus --no-fulltext: %v", err)
	}
	b = summarizeBundle{}
	if err := json.Unmarshal([]byte(out), &b); err != nil {
		t.Fatalf("decode bundle %q: %v", out, err)
	}
	if b.Focus == nil || b.Focus.Fulltext != focusFulltextSkipped || len(fsFulltextPassages(b)) != 0 {
		t.Fatalf("focus = %+v, want PDF text skipped under --no-fulltext", b.Focus)
	}
	if !b.Truncated.Annotations || b.Truncated.AnnotationsKept != 1 || b.Truncated.AnnotationsTotal != 2 || len(b.Focus.Passages) != 1 {
		t.Fatalf("truncated = %+v passages = %+v, want 1 of 2 matching annotations", b.Truncated, b.Focus.Passages)
	}
}

func TestFsSummarizeFocusRejectsBlankTerms(t *testing.T) {
	fsSeedFocusStore(t)
	_, err := runSummarize(t, &rootFlags{asJSON: true}, "F1", "--focus", "  ")
	if err == nil || ExitCode(err) != 2 {
		t.Fatalf("blank --focus error = %v (exit %d), want usage error", err, ExitCode(err))
	}
}

// The Markdown brief carries the same evidence with its attachment key.
func TestFsSummarizeFocusMarkdownAttributesPassages(t *testing.T) {
	fsSeedFocusStore(t)
	out, err := runSummarize(t, &rootFlags{}, "F1", "--focus", fsFocusTerms)
	if err != nil {
		t.Fatalf("summarize F1 --focus (markdown): %v", err)
	}
	if !strings.Contains(out, "`FA1`") || !strings.Contains(out, "quantum annealing schedule") {
		t.Fatalf("markdown lacks the attributed late passage:\n%s", out)
	}
}
