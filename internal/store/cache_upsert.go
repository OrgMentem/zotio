// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package store

import (
	"context"
	"encoding/json"
	"fmt"
)

// UpsertReadCacheContext caches read-plane rows without resurrecting permanent
// deletions. Marker reads and row writes share one transaction and write lock.
// Authoritative write-through must continue to use the unconditional upsert APIs.
func (s *Store) UpsertReadCacheContext(ctx context.Context, resourceType string, items []json.RawMessage) error {
	if ctx == nil {
		ctx = context.Background()
	}
	prepared := make([]preparedResourceItem, 0, len(items))
	for _, raw := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		if row, ok, _ := prepareResourceItem(resourceType, raw); ok {
			prepared = append(prepared, row)
		}
	}
	release, err := s.acquireWrite(ctx)
	if err != nil {
		return err
	}
	defer release()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("starting read-cache transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, item := range prepared {
		if resourceType == "items" || resourceType == "items-trash" {
			var deleted bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pending_writes WHERE resource_type = ? AND id = ? AND deleted = 1)`, resourceType, item.id).Scan(&deleted); err != nil {
				return fmt.Errorf("reading deletion marker for %s/%s: %w", resourceType, item.id, err)
			}
			if deleted {
				continue
			}
		}
		if s.upsertBatchHook != nil {
			s.upsertBatchHook()
		}
		written, err := s.upsertGenericResourceTx(ctx, tx, resourceType, item.id, item.data, item.obj)
		if err != nil {
			return fmt.Errorf("caching %s/%s: %w", resourceType, item.id, err)
		}
		if err := reconcileItemLifecycleTx(ctx, tx, resourceType, item.id, written, item.obj); err != nil {
			return fmt.Errorf("reconciling cached %s/%s: %w", resourceType, item.id, err)
		}
	}
	return tx.Commit()
}
