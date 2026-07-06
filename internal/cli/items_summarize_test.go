// Copyright 2026 OrgMentem. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean nbiv): unit coverage for the pure bundle assembly + bounding.

package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBuildItemBundle(t *testing.T) {
	raw := json.RawMessage(`{"key":"K1","data":{"key":"K1","itemType":"journalArticle","title":"Attention Is All You Need","creators":[{"lastName":"Vaswani","firstName":"Ashish","creatorType":"author"}],"date":"2017-06-12","publicationTitle":"NeurIPS","DOI":"10.1/x","url":"http://x","abstractNote":"We propose the Transformer."}}`)
	ann := []json.RawMessage{
		json.RawMessage(`{"key":"A1","data":{"itemType":"annotation","parentItem":"K1","annotationType":"highlight","annotationText":"self-attention","annotationComment":"key","annotationPageLabel":"3"}}`),
	}
	b := buildItemBundle(raw, ann, "Full body text here.", summarizeOpts{maxChars: 8000, maxAnnotations: 40})

	if b.Key != "K1" {
		t.Errorf("key = %q", b.Key)
	}
	for _, want := range []string{"Vaswani", "(2017)", "Attention", "NeurIPS"} {
		if !strings.Contains(b.Citation, want) {
			t.Errorf("citation %q missing %q", b.Citation, want)
		}
	}
	if b.Abstract != "We propose the Transformer." {
		t.Errorf("abstract = %q", b.Abstract)
	}
	if len(b.Annotations) != 1 || b.Annotations[0].Text != "self-attention" || b.Annotations[0].Page != "3" {
		t.Errorf("annotations = %+v", b.Annotations)
	}
	if b.Fulltext != "Full body text here." || b.Truncated.Fulltext {
		t.Errorf("fulltext = %q truncated = %v", b.Fulltext, b.Truncated.Fulltext)
	}
	if len(b.Gaps) != 0 {
		t.Errorf("expected no gaps, got %v", b.Gaps)
	}
	if b.Prompt == "" {
		t.Errorf("missing synthesis prompt")
	}
}

func TestBuildItemBundleBounding(t *testing.T) {
	raw := json.RawMessage(`{"key":"K2","data":{"key":"K2","itemType":"book","title":"B"}}`)
	ann := []json.RawMessage{
		json.RawMessage(`{"data":{"itemType":"annotation","annotationText":"one","annotationPageLabel":"1"}}`),
		json.RawMessage(`{"data":{"itemType":"annotation","annotationText":"two","annotationPageLabel":"2"}}`),
		json.RawMessage(`{"data":{"itemType":"annotation","annotationText":"three","annotationPageLabel":"3"}}`),
	}
	b := buildItemBundle(raw, ann, strings.Repeat("x", 100), summarizeOpts{maxChars: 10, maxAnnotations: 2})

	if !b.Truncated.Annotations || b.Truncated.AnnotationsKept != 2 || b.Truncated.AnnotationsTotal != 3 {
		t.Errorf("annotation truncation = %+v", b.Truncated)
	}
	if len(b.Annotations) != 2 {
		t.Errorf("kept %d annotations, want 2", len(b.Annotations))
	}
	if len(b.Fulltext) != 10 || !b.Truncated.Fulltext {
		t.Errorf("fulltext len = %d truncated = %v, want 10/true", len(b.Fulltext), b.Truncated.Fulltext)
	}
	if !strings.Contains(strings.Join(b.Gaps, ","), "no abstract") {
		t.Errorf("gaps = %v, want 'no abstract'", b.Gaps)
	}
}

func TestBuildItemBundleNoFulltextGap(t *testing.T) {
	raw := json.RawMessage(`{"key":"K3","data":{"key":"K3","itemType":"journalArticle","title":"T","abstractNote":"a"}}`)
	b := buildItemBundle(raw, nil, "", summarizeOpts{maxChars: 8000, maxAnnotations: 40, noFulltext: true})
	g := strings.Join(b.Gaps, ",")
	if !strings.Contains(g, "no DOI") {
		t.Errorf("want 'no DOI' gap for article without DOI, got %v", b.Gaps)
	}
	if strings.Contains(g, "no fulltext") {
		t.Errorf("must not claim 'no fulltext' when --no-fulltext, got %v", b.Gaps)
	}
}

func TestSummarizeCitation(t *testing.T) {
	c := summarizeCitation(vaultMeta{Key: "K", Title: "T", Year: "2020", Authors: []string{"A", "B", "C", "D"}}, "Venue")
	for _, want := range []string{"A et al.", "(2020)", "T", "Venue"} {
		if !strings.Contains(c, want) {
			t.Errorf("citation %q missing %q", c, want)
		}
	}
	if got := summarizeCitation(vaultMeta{Key: "K9"}, ""); got != "K9" {
		t.Errorf("empty metadata should fall back to key, got %q", got)
	}
}

func TestExtractVenue(t *testing.T) {
	if v := extractVenue(json.RawMessage(`{"data":{"publicationTitle":"J","publisher":"P"}}`)); v != "J" {
		t.Errorf("publicationTitle should win, got %q", v)
	}
	if v := extractVenue(json.RawMessage(`{"data":{"publisher":"P"}}`)); v != "P" {
		t.Errorf("publisher fallback = %q, want P", v)
	}
	if v := extractVenue(json.RawMessage(`{"data":{}}`)); v != "" {
		t.Errorf("no venue should be empty, got %q", v)
	}
}

func TestTruncateRunes(t *testing.T) {
	out, cut := truncateRunes(strings.Repeat("é", 10), 5) // 20 bytes -> cap 5
	if !cut {
		t.Errorf("expected truncation")
	}
	if !utf8.ValidString(out) {
		t.Errorf("truncation produced invalid UTF-8: %q", out)
	}
	if len(out) > 5 {
		t.Errorf("len = %d, want <= 5", len(out))
	}
	if out2, cut2 := truncateRunes("abc", 10); cut2 || out2 != "abc" {
		t.Errorf("short input should be untouched, got %q cut=%v", out2, cut2)
	}
}
