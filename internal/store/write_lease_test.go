// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func openWriteLeaseStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "write_lease.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestWithWriteLease_ReentrantForContextWrites proves a context-taking write
// inside a lease reuses the held lock instead of deadlocking on it: writeMu is
// a plain sync.Mutex, so acquireWrite has to recognize the lease.
func TestWithWriteLease_ReentrantForContextWrites(t *testing.T) {
	s := openWriteLeaseStore(t)
	done := make(chan error, 1)
	go func() {
		done <- s.WithWriteLease(context.Background(), func(ctx context.Context) error {
			if !s.holdsWriteLease(ctx) {
				t.Error("lease context does not carry the lease")
			}
			if err := s.UpsertContext(ctx, "items", "AAA", json.RawMessage(`{"key":"AAA","version":1,"data":{"key":"AAA","itemType":"book"}}`)); err != nil {
				return err
			}
			// Nested leases are no-ops, not a second acquisition.
			return s.WithWriteLease(ctx, func(ctx context.Context) error {
				_, err := s.UpsertKeyedContext(ctx, "schema-item-type-fields", []string{"book"}, []json.RawMessage{json.RawMessage(`[{"field":"title"}]`)})
				return err
			})
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("leased writes: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("leased write deadlocked on writeMu")
	}
	if _, err := s.Get("items", "AAA"); err != nil {
		t.Fatalf("leased upsert did not land: %v", err)
	}
	if _, err := s.Get("schema-item-type-fields", "book"); err != nil {
		t.Fatalf("nested leased keyed upsert did not land: %v", err)
	}
}

// TestWithWriteLease_ExcludesOtherWriters proves the lease is what sync's
// change tracking needs: no other in-process writer can land a write while a
// lease-held read/write/read sequence is in flight.
func TestWithWriteLease_ExcludesOtherWriters(t *testing.T) {
	s := openWriteLeaseStore(t)
	inside := make(chan struct{})
	otherLanded := make(chan struct{})
	go func() {
		<-inside
		if err := s.Upsert("items", "BBB", json.RawMessage(`{"key":"BBB","version":1,"data":{"key":"BBB","itemType":"book"}}`)); err != nil {
			t.Errorf("competing write: %v", err)
		}
		close(otherLanded)
	}()

	var sawOther bool
	err := s.WithWriteLease(context.Background(), func(ctx context.Context) error {
		close(inside)
		select {
		case <-otherLanded:
			sawOther = true
		case <-time.After(time.Second):
		}
		return nil
	})
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	<-otherLanded
	if sawOther {
		t.Fatal("a competing writer landed while the write lease was held")
	}
}

// TestWithWriteLease_CancelledContextDoesNotRunFn keeps the cancellation
// contract of acquireWrite: a cancelled caller fails fast rather than waiting
// out the holding writer, and its body never runs.
func TestWithWriteLease_CancelledContextDoesNotRunFn(t *testing.T) {
	s := openWriteLeaseStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ran := false
	err := s.WithWriteLease(ctx, func(context.Context) error {
		ran = true
		return nil
	})
	if err == nil {
		t.Fatal("cancelled lease returned no error")
	}
	if ran {
		t.Fatal("cancelled lease ran its body")
	}
}
