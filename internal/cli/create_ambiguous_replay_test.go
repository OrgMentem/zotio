// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"errors"
	"strings"
	"testing"

	"zotio/internal/client"
)

// A lost create response must refuse automatic retry, not report a plain
// retryable failure: the request may already be committed in Zotero.
func TestIsCommittedCreateConflictRefusesLostWriteResponse(t *testing.T) {
	ambiguous := &client.AmbiguousWriteError{Method: "POST", Path: "/items", Attempts: 1, Err: errors.New("connection reset")}
	if !isCommittedCreateConflict(ambiguous) {
		t.Fatal("isCommittedCreateConflict(lost POST /items response) = false, want true")
	}
	refusal := ambiguousCreateRefusal("import file", "the 1 record(s) starting at record 1", ambiguous)
	for _, want := range []string{"refusing automatic retry", "may already be committed", "duplicates"} {
		if !strings.Contains(refusal.Error(), want) {
			t.Fatalf("refusal = %q, want %q", refusal.Error(), want)
		}
	}
}

// A server can commit a create before answering 500. The write token protects
// a replay, but the caller must still report the unknown outcome as a conflict.
func TestIsCommittedCreateConflictRefusesServerError(t *testing.T) {
	final := &client.APIError{Method: "POST", Path: "/items", StatusCode: 500, Body: "internal error"}
	ambiguous := &client.AmbiguousWriteError{Method: "POST", Path: "/items", Attempts: 4, Err: final}
	if !isCommittedCreateConflict(ambiguous) {
		t.Fatal("isCommittedCreateConflict(retried HTTP 500) = false, want true")
	}
}

// A replayed write token answered 412 proves the batch already committed, so
// rerunning the same input must reconcile or refuse instead of duplicating.
func TestCreateBatchWriteTokenIsStableForSamePayload(t *testing.T) {
	payload := []map[string]any{{"itemType": "journalArticle", "title": "Same"}}
	first := createBatchWriteToken("zotio.import.file", "0", payload)
	second := createBatchWriteToken("zotio.import.file", "0", payload)
	if first == "" || first != second {
		t.Fatalf("tokens = %q, %q; want a stable non-empty token", first, second)
	}
	if other := createBatchWriteToken("zotio.import.file", "50", payload); other == first {
		t.Fatal("batch position 50 reused the batch 0 token; identical payloads in different windows must not share a token")
	}
}
