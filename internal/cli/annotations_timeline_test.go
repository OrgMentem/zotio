// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

package cli

import "testing"

func sortFilteredAnnotations(annotations []annotationSummary) []annotationSummary {
	return sortAnnotationsByInstantDesc(annotations)
}

func TestAnnotationsTimelineSortByParsedInstant(t *testing.T) {
	// Chronological order, not lexical: RFC3339 strings that differ only by
	// timezone offset must sort by instant. The strings below are crafted so
	// lexical descending differs from chronological descending.
	//   B: 2024-01-02T10:00:00+02:00  (instant 2024-01-02T08:00:00Z)
	//   A: 2024-01-02T09:00:00Z       (instant 2024-01-02T09:00:00Z) -> newest
	// Lexical ascending: A < B (because '+' < 'Z' at pos 19), so lexical
	// descending would put B before A, while chronological descending puts A before B.
	filtered := []annotationSummary{
		{Key: "A", DateAdded: "2024-01-02T09:00:00Z"},
		{Key: "B", DateAdded: "2024-01-02T10:00:00+02:00"},
	}
	sorted := sortFilteredAnnotations(filtered)
	if len(sorted) != 2 || sorted[0].Key != "A" || sorted[1].Key != "B" {
		t.Fatalf("chronological sort = %v, want A then B", keysOf(sorted))
	}
}

func TestAnnotationsTimelineInvalidTimestampLast(t *testing.T) {
	filtered := []annotationSummary{
		{Key: "BAD", DateAdded: "not-a-date"},
		{Key: "A", DateAdded: "2024-01-02T09:00:00Z"},
		{Key: "B", DateAdded: "bad-date-2"},
	}
	sorted := sortFilteredAnnotations(filtered)
	if len(sorted) != 3 || sorted[0].Key != "A" {
		t.Fatalf("valid entries should be first, got %v", keysOf(sorted))
	}
	if sorted[1].Key != "B" || sorted[2].Key != "BAD" {
		t.Fatalf("invalid entries should be last sorted by key, got %v", keysOf(sorted))
	}
}

func TestAnnotationsTimelineStableTieBreak(t *testing.T) {
	filtered := []annotationSummary{
		{Key: "Z", DateAdded: "2024-01-02T09:00:00Z"},
		{Key: "A", DateAdded: "2024-01-02T09:00:00Z"},
		{Key: "M", DateAdded: "2024-01-02T09:00:00Z"},
	}
	sorted := sortFilteredAnnotations(filtered)
	if sorted[0].Key != "A" || sorted[1].Key != "M" || sorted[2].Key != "Z" {
		t.Fatalf("tie-break by key = %v, want A,M,Z", keysOf(sorted))
	}
}

func keysOf(anns []annotationSummary) []string {
	out := make([]string, len(anns))
	for i, a := range anns {
		out[i] = a.Key
	}
	return out
}
