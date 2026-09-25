// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func withWriteTxTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "writetx.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func execTxInsert(t *testing.T, ctx context.Context, tx *sql.Tx, key string) {
	t.Helper()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO resources(resource_type, id, data) VALUES('writetx', ?, ?)`,
		key, `{"key":"`+key+`"}`); err != nil {
		t.Fatalf("tx insert %s: %v", key, err)
	}
}

// TestWithWriteTxCommit pins the commit path: two rows written inside fn are
// both visible after a nil return.
func TestWithWriteTxCommit(t *testing.T) {
	s := withWriteTxTestStore(t)
	err := s.WithWriteTx(context.Background(), func(tx *sql.Tx) error {
		execTxInsert(t, context.Background(), tx, "COMMIT1")
		execTxInsert(t, context.Background(), tx, "COMMIT2")
		return nil
	})
	if err != nil {
		t.Fatalf("WithWriteTx: %v", err)
	}
	for _, key := range []string{"COMMIT1", "COMMIT2"} {
		if _, err := s.Get("writetx", key); err != nil {
			t.Fatalf("row %s missing after commit: %v", key, err)
		}
	}
}

// TestWithWriteTxFnErrorRollsBack pins the rollback path: a row written
// before fn fails must not survive, and the fn error propagates.
func TestWithWriteTxFnErrorRollsBack(t *testing.T) {
	s := withWriteTxTestStore(t)
	fnErr := errors.New("boom")
	err := s.WithWriteTx(context.Background(), func(tx *sql.Tx) error {
		execTxInsert(t, context.Background(), tx, "ROLLBACK1")
		return fnErr
	})
	if !errors.Is(err, fnErr) {
		t.Fatalf("WithWriteTx err = %v, want fn error", err)
	}
	if got, err := s.Get("writetx", "ROLLBACK1"); got != nil || !errors.Is(err, ErrNotFound) {
		t.Fatalf("row survived fn-error rollback: got=%s err=%v", got, err)
	}
}

// TestWithWriteTxCancelledContextRollsBack pins the documented guarantee: a
// pre-cancelled context never commits, rows are absent, and the error is the
// context error.
func TestWithWriteTxCancelledContextRollsBack(t *testing.T) {
	s := withWriteTxTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		t.Error("fn ran despite cancelled context")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WithWriteTx err = %v, want context.Canceled", err)
	}
	if n, err := s.Count("writetx"); err != nil || n != 0 {
		t.Fatalf("count after cancelled tx = %d, err = %v; want 0", n, err)
	}
}

// TestWithWriteTxIsNotReentrant pins the documented footgun: an inner
// WithWriteTx inside fn cannot acquire the held write lock, so once the
// deadline passes it fails fast with the context error instead of deadlocking.
func TestWithWriteTxIsNotReentrant(t *testing.T) {
	s := withWriteTxTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	// The callback runs off the test goroutine, so it records instead of
	// calling t.*: t.FailNow outside the test goroutine is not allowed.
	type reentrantOutcome struct {
		enteredOuter bool
		insertErr    error
		innerErr     error
		outerErr     error
	}
	done := make(chan reentrantOutcome, 1)
	go func() {
		var oc reentrantOutcome
		oc.outerErr = s.WithWriteTx(ctx, func(tx *sql.Tx) error {
			oc.enteredOuter = true
			_, err := tx.ExecContext(context.Background(),
				`INSERT INTO resources(resource_type, id, data) VALUES('writetx', ?, ?)`,
				"OUTER1", `{"key":"OUTER1"}`)
			oc.insertErr = err
			if err != nil {
				return err
			}
			oc.innerErr = s.WithWriteTx(ctx, func(_ *sql.Tx) error { return nil })
			return oc.innerErr
		})
		done <- oc
	}()
	// Bound completion with a timer independent of ctx: the old elapsed
	// check ran only after the synchronous call returned, so a lock
	// regression that ignores ctx hung until the suite timeout.
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case oc := <-done:
		// A pre-expired context can return before the callback and still
		// leave the row absent, passing the rollback check vacuously.
		if !oc.enteredOuter {
			t.Fatal("outer WithWriteTx returned without running fn; the rollback check below would pass vacuously")
		}
		if oc.insertErr != nil {
			t.Fatalf("outer tx insert OUTER1: %v", oc.insertErr)
		}
		if !errors.Is(oc.innerErr, context.DeadlineExceeded) {
			t.Fatalf("inner WithWriteTx err = %v, want context.DeadlineExceeded", oc.innerErr)
		}
		if !errors.Is(oc.outerErr, context.DeadlineExceeded) {
			t.Fatalf("outer WithWriteTx err = %v, want context.DeadlineExceeded", oc.outerErr)
		}
	case <-timer.C:
		t.Fatal("re-entrant write did not finish within 5s; must fail fast on ctx deadline")
	}
	if got, err := s.Get("writetx", "OUTER1"); got != nil || !errors.Is(err, ErrNotFound) {
		t.Fatalf("outer row survived rollback: got=%s err=%v", got, err)
	}
}
