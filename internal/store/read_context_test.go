// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
)

// TestStoreReadContextsHonorCancelledContext pins the MCP/CLI fix: the
// context-bound Get/List/AnnotationsForItem/Count reads take the request
// context, so a disconnected MCP client or Ctrl+C aborts the SQLite read
// instead of running it to completion. A pre-cancelled context must fail fast
// with the context error rather than returning rows.
func TestStoreReadContextsHonorCancelledContext(t *testing.T) {
	s, err := OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "readctx.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Upsert("items", "RC1", json.RawMessage(`{"key":"RC1","version":1,"data":{"key":"RC1","itemType":"book","title":"readctx"}}`)); err != nil {
		t.Fatalf("seed item: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := s.GetContext(ctx, "items", "RC1"); !errors.Is(err, context.Canceled) {
		t.Errorf("GetContext err = %v, want context.Canceled", err)
	}
	if _, err := s.ListContext(ctx, "items", 0); !errors.Is(err, context.Canceled) {
		t.Errorf("ListContext err = %v, want context.Canceled", err)
	}
	if _, err := s.AnnotationsForItemContext(ctx, "RC1"); !errors.Is(err, context.Canceled) {
		t.Errorf("AnnotationsForItemContext err = %v, want context.Canceled", err)
	}
	if _, err := s.CountContext(ctx, "items"); !errors.Is(err, context.Canceled) {
		t.Errorf("CountContext err = %v, want context.Canceled", err)
	}
	if _, _, _, err := s.GetSyncStateContext(ctx, "items"); !errors.Is(err, context.Canceled) {
		t.Errorf("GetSyncStateContext err = %v, want context.Canceled", err)
	}
}

// TestKeysWithCollectionsListsReferencingRows proves the reconciliation
// helper: rows whose data.collections names a reaped key are returned by id,
// unrelated rows are excluded, and an empty key set short-circuits to empty.
func TestKeysWithCollectionsListsReferencingRows(t *testing.T) {
	s, err := OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "keyswith.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	seed := map[string]string{
		"K1": `{"key":"K1","version":1,"data":{"key":"K1","itemType":"book","title":"t","collections":["C1","C2"]}}`,
		"K2": `{"key":"K2","version":1,"data":{"key":"K2","itemType":"book","title":"t","collections":["C2"]}}`,
		"K3": `{"key":"K3","version":1,"data":{"key":"K3","itemType":"book","title":"t","collections":[]}}`,
	}
	for key, body := range seed {
		if err := s.Upsert("items", key, json.RawMessage(body)); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}
	got, err := s.KeysWithCollections(context.Background(), "items", []string{"C1"})
	if err != nil {
		t.Fatalf("KeysWithCollections: %v", err)
	}
	if len(got) != 1 || got[0] != "K1" {
		t.Fatalf("KeysWithCollections(C1) = %v, want [K1]", got)
	}
	got, err = s.KeysWithCollections(context.Background(), "items", []string{"C2"})
	if err != nil {
		t.Fatalf("KeysWithCollections: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("KeysWithCollections(C2) = %v, want 2 rows", got)
	}
	seen := map[string]bool{}
	for _, key := range got {
		seen[key] = true
	}
	for _, want := range []string{"K1", "K2"} {
		if !seen[want] {
			t.Fatalf("KeysWithCollections(C2) = %v, want the exact set {K1 K2}", got)
		}
	}
	got, err = s.KeysWithCollections(context.Background(), "items", []string{"MISSING"})
	if err != nil {
		t.Fatalf("KeysWithCollections: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("KeysWithCollections(MISSING) = %v, want empty", got)
	}
	got, err = s.KeysWithCollections(context.Background(), "items", nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("KeysWithCollections(nil) = %v, %v; want empty nil error", got, err)
	}
}
