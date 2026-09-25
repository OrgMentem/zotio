// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

package cli

import (
	"strings"
	"testing"
	"time"
)

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

// The invalid arm must fail closed: if it ever returns (zero, false, nil), a
// typo'd --since is silently treated as "no filter" and the timeline widens
// to all annotations. These cases pin the usage error and the date-only arm.
func TestParseAnnotationSinceValidation(t *testing.T) {
	if _, ok, err := parseAnnotationSince("2026-13-45"); err == nil || !strings.Contains(err.Error(), "invalid --since value") {
		t.Fatalf("invalid since err = %v, want it to mention \"invalid --since value\"", err)
	} else if ok {
		t.Fatal("invalid since ok = true, want false so the caller filters nothing silently")
	}

	got, ok, err := parseAnnotationSince("2026-01-02")
	if err != nil || !ok {
		t.Fatalf("date-only since = (%v, %v, %v), want (midnight UTC, true, nil)", got, ok, err)
	}
	if want := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("date-only since = %v, want %v", got, want)
	}

	if _, ok, err := parseAnnotationSince(""); err != nil || ok {
		t.Fatalf("empty since = (ok=%v, err=%v), want (false, nil)", ok, err)
	}
}
