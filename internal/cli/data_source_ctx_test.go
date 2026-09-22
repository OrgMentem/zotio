// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zotio/internal/client"
	"zotio/internal/config"
	"zotio/internal/store"
)

// TestResolveLocalItemList_CancelledContextAbortsContendedQuery proves cancellation
// propagates from the CLI local-read planner through QueryItemsContext. Before
// context threading, resolveLocalItemList called QueryItems (Background) and a
// canceled request could sit in the busy-retry window for migrationLockTimeout.
func TestResolveLocalItemList_CancelledContextAbortsContendedQuery(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "cancel_local_items.db")
	s, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := s.Upsert("items", "A", json.RawMessage(`{"key":"A","version":1,"data":{"key":"A","itemType":"journalArticle","title":"Alpha"}}`)); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if _, err := s.DB().Exec(`PRAGMA journal_mode=DELETE`); err != nil {
		t.Fatalf("journal_mode delete: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close seeded store: %v", err)
	}

	readDB, err := store.OpenReadOnlyContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open read-only store: %v", err)
	}
	t.Cleanup(func() { _ = readDB.Close() })
	if _, err := readDB.DB().Exec(`PRAGMA busy_timeout=0`); err != nil {
		t.Fatalf("busy_timeout: %v", err)
	}

	holder, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatalf("open holder: %v", err)
	}
	holder.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = holder.Close() })
	if _, err := holder.Exec(`BEGIN EXCLUSIVE`); err != nil {
		t.Fatalf("begin exclusive: %v", err)
	}
	t.Cleanup(func() { _, _ = holder.Exec(`ROLLBACK`) })

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, handled, err := resolveLocalItemList(ctx, readDB, "/items", map[string]string{})
	elapsed := time.Since(start)
	if !handled {
		t.Fatalf("resolveLocalItemList not handled for /items")
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("cancellation not honored promptly: elapsed %v", elapsed)
	}
	if err == nil {
		t.Fatalf("expected cancellation error, got nil")
	}
	if !errors.Is(err, context.Canceled) && !strings.Contains(strings.ToLower(err.Error()), "canceled") {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

// dataSourceBacklogHangRoundTripper never answers: the request hangs until the
// caller's context fires, so the resulting error carries the context's own
// timeout/cancellation semantics instead of a transport failure.
type dataSourceBacklogHangRoundTripper struct{}

func (dataSourceBacklogHangRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	<-req.Context().Done()
	return nil, req.Context().Err()
}

func dataSourceBacklogHangClient() *client.Client {
	c := client.New(&config.Config{BaseURL: "http://127.0.0.1:1/api/users/0"}, time.Second, 0)
	c.NoCache = true
	c.HTTPClient = &http.Client{Transport: dataSourceBacklogHangRoundTripper{}}
	return c
}

// TestDataSourceAutoFallsBackOnTimeout holds a live read open until the
// request context times out: auto mode must serve the seeded mirror instead
// of failing with the deadline error.
func TestDataSourceAutoFallsBackOnTimeout(t *testing.T) {
	seedLocalQueryPlannerDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	flags := &rootFlags{asJSON: true, dataSource: "auto", noCache: true, timeout: 5 * time.Second}
	data, prov, err := resolveRead(ctx, dataSourceBacklogHangClient(), flags, "items", false, "/items", nil, nil)
	if err != nil {
		t.Fatalf("auto read on timeout: %v", err)
	}
	if prov.Source != "local" || prov.Reason != "api_unreachable" {
		t.Fatalf("provenance = %+v, want a local api_unreachable fallback", prov)
	}
	got := itemKeysFromRawList(t, data)
	for _, want := range []string{"A", "B", "C"} {
		found := false
		for _, key := range got {
			if key == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("fallback keys = %v, want the seeded [A B C]", got)
		}
	}
	if len(got) != 3 {
		t.Fatalf("fallback keys = %v, want exactly the seeded [A B C]", got)
	}
}

// TestDataSourceAutoPreservesCancellation cancels the request context before
// the live read: auto mode must propagate the cancellation instead of
// serving local rows the user interrupted away from.
func TestDataSourceAutoPreservesCancellation(t *testing.T) {
	seedLocalQueryPlannerDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	flags := &rootFlags{asJSON: true, dataSource: "auto", noCache: true, timeout: 5 * time.Second}
	_, _, err := resolveRead(ctx, dataSourceBacklogHangClient(), flags, "items", false, "/items", nil, nil)
	if err == nil {
		t.Fatal("canceled auto read fell back to local data; want the cancellation to propagate")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("auto read error = %v, want context.Canceled", err)
	}
}
