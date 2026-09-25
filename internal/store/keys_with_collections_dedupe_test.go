// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
)

// An item naming collection keys that fall into two 500-key batches must be
// returned once, not once per batch. Without the dedupe, the sync member
// repair fetched such an item twice per sync.
func TestKeysWithCollectionsDedupesAcrossBatches(t *testing.T) {
	s, err := OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "dedupe.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	const total = 501
	keys := make([]string, 0, total)
	for i := range total {
		keys = append(keys, fmt.Sprintf("C%07d", i))
	}
	body := fmt.Sprintf(
		`{"key":"K1","version":1,"data":{"key":"K1","itemType":"book","title":"t","collections":[%q,%q]}}`,
		keys[0], keys[total-1],
	)
	if err := s.Upsert("items", "K1", json.RawMessage(body)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := s.KeysWithCollections(context.Background(), "items", keys)
	if err != nil {
		t.Fatalf("KeysWithCollections: %v", err)
	}
	if len(got) != 1 || got[0] != "K1" {
		t.Fatalf("KeysWithCollections(501 keys) = %v, want [K1] exactly once", got)
	}
}
