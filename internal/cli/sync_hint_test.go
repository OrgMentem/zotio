// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zotio/internal/store"

	"github.com/spf13/cobra"
)

func newSyncHintTestStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func newSyncHintTestCmd() (*cobra.Command, *bytes.Buffer) {
	var stderr bytes.Buffer
	cmd := &cobra.Command{Use: "zotio"}
	cmd.SetErr(&stderr)
	return cmd, &stderr
}

func TestHintIfStale_BackdatedSyncStateWritesHintToStderr(t *testing.T) {
	db := newSyncHintTestStore(t)
	if _, err := db.DB().Exec(
		`INSERT INTO sync_state(resource_type, last_synced_at, total_count) VALUES (?, ?, ?)`,
		"issues", time.Now().Add(-2*time.Hour), 1,
	); err != nil {
		t.Fatalf("seed sync_state: %v", err)
	}
	cmd, stderr := newSyncHintTestCmd()

	if !hintIfStale(cmd, db, "", 30*time.Minute) {
		t.Fatalf("hintIfStale returned false for stale sync_state")
	}
	got := stderr.String()
	if !strings.Contains(got, "older than --max-age=30m0s") || !strings.Contains(got, "Run 'zotio sync'") {
		t.Fatalf("stderr = %q, want stale sync hint", got)
	}
}

func TestHintIfStale_MaxAgeZeroDisablesHint(t *testing.T) {
	db := newSyncHintTestStore(t)
	if _, err := db.DB().Exec(
		`INSERT INTO sync_state(resource_type, last_synced_at, total_count) VALUES (?, ?, ?)`,
		"issues", time.Now().Add(-2*time.Hour), 1,
	); err != nil {
		t.Fatalf("seed sync_state: %v", err)
	}
	cmd, stderr := newSyncHintTestCmd()

	if hintIfStale(cmd, db, "", 0) {
		t.Fatalf("hintIfStale returned true when maxAge is zero")
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want no hint", stderr.String())
	}
}

func TestHintIfStale_AllResourcesIgnoresNullTimestampRows(t *testing.T) {
	db := newSyncHintTestStore(t)
	now := time.Now()
	for _, row := range []struct {
		resource string
		syncedAt any
	}{
		{"users", nil},
		{"issues", now.Add(-2 * time.Hour)},
	} {
		if _, err := db.DB().Exec(
			`INSERT INTO sync_state(resource_type, last_synced_at, total_count) VALUES (?, ?, ?)`,
			row.resource, row.syncedAt, 1,
		); err != nil {
			t.Fatalf("seed %s sync_state: %v", row.resource, err)
		}
	}

	cmd, stderr := newSyncHintTestCmd()
	if !hintIfStale(cmd, db, "", 30*time.Minute) {
		t.Fatalf("hintIfStale returned false for oldest valid all-resource timestamp")
	}
	if got := stderr.String(); !strings.Contains(got, "older than --max-age=30m0s") {
		t.Fatalf("stderr = %q, want stale hint from valid timestamp", got)
	}
}

func TestHintIfStale_ResourceFilterUsesRequestedResource(t *testing.T) {
	db := newSyncHintTestStore(t)
	now := time.Now()
	for _, row := range []struct {
		resource string
		syncedAt time.Time
	}{
		{"users", now.Add(-5 * time.Minute)},
		{"issues", now.Add(-2 * time.Hour)},
	} {
		if _, err := db.DB().Exec(
			`INSERT INTO sync_state(resource_type, last_synced_at, total_count) VALUES (?, ?, ?)`,
			row.resource, row.syncedAt, 1,
		); err != nil {
			t.Fatalf("seed %s sync_state: %v", row.resource, err)
		}
	}

	cmd, stderr := newSyncHintTestCmd()
	if hintIfStale(cmd, db, "users", 30*time.Minute) {
		t.Fatalf("hintIfStale returned true for fresh users resource")
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want no hint for fresh users resource", stderr.String())
	}

	if !hintIfStale(cmd, db, "issues", 30*time.Minute) {
		t.Fatalf("hintIfStale returned false for stale issues resource")
	}
	if got := stderr.String(); !strings.Contains(got, "older than --max-age=30m0s") {
		t.Fatalf("stderr = %q, want stale issues hint", got)
	}
}
