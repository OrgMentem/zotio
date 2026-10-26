// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package store

import (
	"context"
	"encoding/json"
	"testing"
)

func TestSyncProgressDoesNotStampCompletion(t *testing.T) {
	s := mirroredWriteTestOpen(t)
	ctx := context.Background()
	if err := s.SaveSyncResumeStateContext(ctx, "items", "100", "test-scope", 100); err != nil {
		t.Fatal(err)
	}
	cursor, stamp, _, err := s.GetSyncState("items")
	if err != nil || cursor != "100" || !stamp.IsZero() {
		t.Fatalf("first progress: cursor=%q stamp=%v err=%v", cursor, stamp, err)
	}
	if err := s.SaveSyncState("items", "", 100); err != nil {
		t.Fatal(err)
	}
	_, completed, _, err := s.GetSyncState("items")
	if err != nil || completed.IsZero() {
		t.Fatalf("completion: stamp=%v err=%v", completed, err)
	}
	if err := s.SaveSyncResumeStateContext(ctx, "items", "200", "test-scope", 100); err != nil {
		t.Fatal(err)
	}
	_, later, _, err := s.GetSyncState("items")
	if err != nil || !later.Equal(completed) {
		t.Fatalf("later progress: stamp=%v want=%v err=%v", later, completed, err)
	}
}

func TestFulltextReadinessRequiresCompletionNotRows(t *testing.T) {
	s := mirroredWriteTestOpen(t)
	ctx := context.Background()
	if _, err := s.UpsertKeyed("fulltext", []string{"ABCD2345"}, []json.RawMessage{json.RawMessage(`{"content":"partial text"}`)}); err != nil {
		t.Fatal(err)
	}
	if ready, err := s.FulltextSearchReadyContext(ctx); err != nil || ready {
		t.Fatalf("partial ready=%v err=%v", ready, err)
	}
	if err := s.SaveSyncState("fulltext", "", 0); err != nil {
		t.Fatal(err)
	}
	if ready, err := s.FulltextSearchReadyContext(ctx); err != nil || !ready {
		t.Fatalf("complete ready=%v err=%v", ready, err)
	}
}

func TestFulltextLegacyCheckpointRemainsReadyAfterUpgrade(t *testing.T) {
	s := mirroredWriteTestOpen(t)
	ctx := context.Background()
	if _, err := s.UpsertKeyed("fulltext", []string{"ABCD2345"}, []json.RawMessage{json.RawMessage(`{"content":"legacy text"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveLibraryVersion("fulltext", "legacy-plane", 7); err != nil {
		t.Fatal(err)
	}
	if _, stamp, _, err := s.GetSyncState("fulltext"); err != nil || !stamp.IsZero() {
		t.Fatalf("legacy stamp=%v err=%v", stamp, err)
	}
	if ready, err := s.FulltextSearchReadyContext(ctx); err != nil || !ready {
		t.Fatalf("legacy completed index ready=%v err=%v", ready, err)
	}
}
