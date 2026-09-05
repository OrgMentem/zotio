// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"hash/fnv"
	"path/filepath"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// TestSchemaVersion_StampedOnFreshDB verifies that opening a brand-new
// database stamps the current schema version. This is the contract that
// makes StoreSchemaVersion upgrades safe: every freshly-created DB
// records the version it was built under.
func TestSchemaVersion_StampedOnFreshDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data.db")
	s, err := OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open fresh db: %v", err)
	}
	defer s.Close()

	v, err := s.SchemaVersion()
	if err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if v != StoreSchemaVersion {
		t.Fatalf("fresh db version = %d, want %d", v, StoreSchemaVersion)
	}
}

// TestSchemaVersion_StampExistingZeroDB verifies the stamp-and-continue
// rule for existing deployed databases. A DB that predates the gate has
// user_version = 0; opening it with this binary should run migrations and
// stamp the current version.
func TestSchemaVersion_StampExistingZeroDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data.db")

	// Pre-create the DB with user_version = 0 and no tables, simulating
	// a database created by a pre-gate version of the binary before any
	// migrations ran.
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.Exec(`PRAGMA user_version = 0`); err != nil {
		t.Fatalf("stamp zero: %v", err)
	}
	raw.Close()

	s, err := OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open pre-gate db: %v", err)
	}
	defer s.Close()

	v, err := s.SchemaVersion()
	if err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if v != StoreSchemaVersion {
		t.Fatalf("post-stamp version = %d, want %d", v, StoreSchemaVersion)
	}
}

// TestSchemaVersion_RefusesNewerDB verifies fail-fast when the on-disk
// schema is newer than the binary supports. Without this gate, a user
// who upgrades their library but not their binary would hit silent
// "no such column" errors instead of a clear version mismatch.
func TestSchemaVersion_RefusesNewerDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data.db")

	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.Exec(`PRAGMA user_version = 999`); err != nil {
		t.Fatalf("stamp future version: %v", err)
	}
	raw.Close()

	_, err = OpenWithContext(context.Background(), dbPath)
	if err == nil {
		t.Fatalf("expected open to fail on newer schema, got nil")
	}
}

// TestMigrate_ConcurrentFreshDB exercises the BEGIN IMMEDIATE migration
// transaction. Without it, N goroutines opening the same fresh DB in
// parallel race per CREATE TABLE statement and trip SQLITE_BUSY despite
// the busy_timeout. With it, they serialize on the RESERVED lock
// acquired at BEGIN time and every Open succeeds.
func TestMigrate_ConcurrentFreshDB(t *testing.T) {
	if testing.Short() {
		t.Skip("concurrent migration test can take up to migrationLockTimeout under contention")
	}
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "data.db")

	const n = 8
	errs := make(chan error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			s, err := OpenWithContext(context.Background(), dbPath)
			if err != nil {
				errs <- err
				return
			}
			s.Close()
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent Open failed: %v", err)
	}
}

// holdWriteLock takes an exclusive write lock on dbPath that a peer's
// BEGIN IMMEDIATE cannot acquire until the returned cleanup runs. Used
// to construct contention scenarios in the migration tests.
func holdWriteLock(t *testing.T, dbPath string) (cleanup func()) {
	t.Helper()
	holder, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("open holder: %v", err)
	}
	htx, err := holder.Begin()
	if err != nil {
		_ = holder.Close()
		t.Fatalf("begin holder tx: %v", err)
	}
	if _, err := htx.Exec(`CREATE TABLE IF NOT EXISTS holder_lock (id INTEGER)`); err != nil {
		_ = htx.Rollback()
		_ = holder.Close()
		t.Fatalf("seed holder write: %v", err)
	}
	return func() {
		_ = htx.Rollback()
		_ = holder.Close()
	}
}

// TestOpenWithContext_RespectsCancellation verifies that a caller that
// cancels its context during a stalled migration sees the cancellation
// surface as the returned error within a short window, instead of
// having to wait out the full migrationLockTimeout. SIGINT in a Cobra
// command's context must interrupt store.Open, not just block on it.
func TestOpenWithContext_RespectsCancellation(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "data.db")
	defer holdWriteLock(t, dbPath)()

	// Pre-cancel the context. The migration's BEGIN IMMEDIATE will BUSY
	// against the holder; the very first iteration of retryOnBusy then
	// hits the ctx.Done() arm of its select and propagates ctx.Canceled.
	// A blocked-then-cancel pattern using time.Sleep would prove the
	// same property but cost the sleep interval on every CI run.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	_, err := OpenWithContext(ctx, dbPath)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected OpenWithContext to fail under contention with cancelled ctx")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled in error chain, got: %v", err)
	}
	// Without ctx threading this would block until migrationLockTimeout
	// (default 30s). 5s is generous headroom over the actual return
	// time (microseconds for a pre-cancelled ctx) without flaking CI.
	if elapsed > 5*time.Second {
		t.Fatalf("OpenWithContext returned after %s; pre-cancelled ctx should short-circuit immediately", elapsed)
	}
}

// TestMigrate_RejectsNewerDBImmediately verifies that an old binary
// opening a newer-schema DB rejects fast even when a peer migrator is
// still holding the write lock. The schema-version check runs on the
// pinned connection BEFORE BEGIN IMMEDIATE so the rejection path
// doesn't have to wait out the migration lock.
func TestMigrate_RejectsNewerDBImmediately(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "data.db")

	// Pre-stamp the DB at a version this binary doesn't support.
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.Exec(`PRAGMA user_version = 999`); err != nil {
		t.Fatalf("stamp future version: %v", err)
	}
	raw.Close()

	defer holdWriteLock(t, dbPath)()

	start := time.Now()
	_, err = OpenWithContext(context.Background(), dbPath)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected Open to refuse a newer-schema DB")
	}
	// The fast-path goal: rejection must arrive well under
	// migrationLockTimeout. 5s leaves headroom over the WAL init race
	// (a few ms in practice) without being so tight CI flakes.
	if elapsed > 5*time.Second {
		t.Fatalf("Open rejected after %s; fast-path should reject in well under migrationLockTimeout (30s)", elapsed)
	}
}

// TestSchemaVersion_ReopenIsIdempotent verifies that opening an already
// correctly-stamped DB is a no-op — the second open reads the version
// and the migrations are all idempotent (CREATE TABLE IF NOT EXISTS).
func TestSchemaVersion_ReopenIsIdempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data.db")

	s1, err := OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	s1.Close()

	s2, err := OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer s2.Close()

	v, err := s2.SchemaVersion()
	if err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if v != StoreSchemaVersion {
		t.Fatalf("reopened version = %d, want %d", v, StoreSchemaVersion)
	}
}

// TestOpenReadOnly_RejectsWrites pins the contract: direct and CTE-wrapped
// writes against the main DB fail under mode=ro. Deliberately does not
// assert VACUUM INTO and ATTACH DATABASE — modernc.org/sqlite allows both
// under mode=ro, so those defenses live in the handleSQL keyword blocklist.
func TestOpenReadOnly_RejectsWrites(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data.db")

	rw, err := OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("seed open: %v", err)
	}
	if _, err := rw.DB().Exec(`INSERT INTO resources (id, resource_type, data) VALUES ('seed', 'thing', '{}')`); err != nil {
		t.Fatalf("seed insert: %v", err)
	}
	rw.Close()

	ro, err := OpenReadOnlyContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open read-only: %v", err)
	}
	defer ro.Close()

	writes := []struct {
		name string
		stmt string
	}{
		{"insert", `INSERT INTO resources (id, resource_type, data) VALUES ('x', 'y', '{}')`},
		{"update", `UPDATE resources SET resource_type = 'hijacked' WHERE id = 'seed'`},
		{"delete", `DELETE FROM resources WHERE id = 'seed'`},
		{"replace", `REPLACE INTO resources (id, resource_type, data) VALUES ('seed', 'evil', '{}')`},
		// CTE-wrapped INSERT is load-bearing: it justifies leaving WITH
		// out of the handleSQL blocklist so SELECT-form CTEs work.
		{"cte_insert", `WITH stale AS (SELECT id FROM resources) INSERT INTO resources (id, resource_type, data) SELECT id || '-evil', 'thing', '{}' FROM stale`},
	}
	for _, w := range writes {
		if _, err := ro.DB().Exec(w.stmt); err == nil {
			t.Errorf("%s succeeded under mode=ro; expected rejection. stmt=%q", w.name, w.stmt)
		}
	}

	var count int
	if err := ro.DB().QueryRow(`SELECT COUNT(*) FROM resources`).Scan(&count); err != nil {
		t.Fatalf("read-only SELECT failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("SELECT returned %d rows, want 1 (only the seed should remain)", count)
	}
	if err := ro.DB().QueryRow(`WITH r AS (SELECT id FROM resources WHERE id = 'seed') SELECT COUNT(*) FROM r`).Scan(&count); err != nil {
		t.Fatalf("read-only WITH...SELECT CTE failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("CTE SELECT returned %d rows, want 1", count)
	}
}

// TestMigrate_AddsColumnsOnUpgrade_SyncState verifies that opening a
// database created by an older binary succeeds and adds newly generated
// columns before CREATE INDEX runs against the pre-existing table. Regression
// coverage for parent_id upgrades and indexed generated columns.
func TestMigrate_AddsColumnsOnUpgrade_SyncState(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data.db")

	// Pre-create the DB with the older table shape: id, data, synced_at and
	// none of the newer generated columns. user_version stays 0 (pre-gate).
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE sync_state (
		id TEXT PRIMARY KEY,
		data JSON NOT NULL,
		synced_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		raw.Close()
		t.Fatalf("create old table: %v", err)
	}
	raw.Close()

	// Opening with the new binary must run CREATE INDEX statements without
	// erroring on missing generated columns.
	s, err := OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open upgraded db: %v", err)
	}
	defer s.Close()

	// The migration must have added every generated column.
	rows, err := s.DB().Query(`PRAGMA table_info(sync_state)`)
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	defer rows.Close()

	hasColumn := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan: %v", err)
		}
		hasColumn[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	for _, want := range []string{
		"last_cursor",
		"last_synced_at",
		"total_count",
	} {
		if !hasColumn[want] {
			t.Fatalf("%s column missing from sync_state after migrate", want)
		}
	}
}

func TestMigrate_ItemLifecycleCanonicalizesPreVersion4DB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data.db")
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE resources (
		id TEXT NOT NULL,
		resource_type TEXT NOT NULL,
		data JSON NOT NULL,
		synced_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (resource_type, id)
	)`); err != nil {
		raw.Close()
		t.Fatalf("create pre-version-4 resources: %v", err)
	}
	if _, err := raw.Exec(`CREATE VIRTUAL TABLE resources_fts USING fts5(
		id, resource_type, content, tokenize='porter unicode61'
	)`); err != nil {
		raw.Close()
		t.Fatalf("create pre-version-4 FTS: %v", err)
	}
	insert := func(resourceType, id, payload, content string) {
		t.Helper()
		if _, err := raw.Exec(
			`INSERT INTO resources (id, resource_type, data) VALUES (?, ?, ?)`,
			id, resourceType, payload,
		); err != nil {
			t.Fatalf("seed %s/%s: %v", resourceType, id, err)
		}
		if _, err := raw.Exec(
			`INSERT INTO resources_fts (rowid, id, resource_type, content) VALUES (?, ?, ?, ?)`,
			ftsRowID(resourceType, id), id, resourceType, content,
		); err != nil {
			t.Fatalf("seed FTS %s/%s: %v", resourceType, id, err)
		}
	}
	insert("items", "LIVE-WINS", `{"key":"LIVE-WINS","version":9,"data":{"title":"canonical live"}}`, "canonical live")
	insert("items-trash", "LIVE-WINS", `{"key":"LIVE-WINS","version":8,"data":{"title":"obsolete trash"}}`, "obsolete trash")
	insert("items", "TRASH-TIE", `{"key":"TRASH-TIE","data":{"version":3,"title":"obsolete live"}}`, "obsolete live")
	insert("items-trash", "TRASH-TIE", `{"key":"TRASH-TIE","version":3,"data":{"title":"canonical trash"}}`, "canonical trash")
	insert("items", "INVALID-VERSION", `{"key":"INVALID-VERSION","version":"not-a-number","data":{"version":99,"title":"invalid live"}}`, "invalid live")
	insert("items-trash", "INVALID-VERSION", `{"key":"INVALID-VERSION","data":{"title":"zero trash"}}`, "zero trash")
	insert("collections", "TRASH-TIE", `{"key":"TRASH-TIE","name":"unrelated collection"}`, "unrelated collection")
	if _, err := raw.Exec(`PRAGMA user_version = 3`); err != nil {
		raw.Close()
		t.Fatalf("stamp pre-version-4 schema: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	s, err := OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open and migrate pre-version-4 db: %v", err)
	}
	if version, err := s.SchemaVersion(); err != nil {
		s.Close()
		t.Fatalf("read migrated schema version: %v", err)
	} else if version != StoreSchemaVersion {
		s.Close()
		t.Fatalf("migrated schema version = %d, want %d", version, StoreSchemaVersion)
	}
	assertCanonicalItemState(t, s, "LIVE-WINS", "items", "items-trash")
	assertCanonicalItemState(t, s, "TRASH-TIE", "items-trash", "items")
	assertCanonicalItemState(t, s, "INVALID-VERSION", "items-trash", "items")
	var collectionCount int
	if err := s.DB().QueryRow(
		`SELECT count(*) FROM resources WHERE resource_type = 'collections' AND id = 'TRASH-TIE'`,
	).Scan(&collectionCount); err != nil {
		s.Close()
		t.Fatalf("count unrelated collection: %v", err)
	}
	if collectionCount != 1 {
		s.Close()
		t.Fatalf("unrelated collection count = %d, want 1", collectionCount)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close migrated store: %v", err)
	}

	// Reopening the now-current database must not change the canonical rows.
	reopened, err := OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("reopen migrated db: %v", err)
	}
	defer reopened.Close()
	assertCanonicalItemState(t, reopened, "LIVE-WINS", "items", "items-trash")
	assertCanonicalItemState(t, reopened, "TRASH-TIE", "items-trash", "items")
	assertCanonicalItemState(t, reopened, "INVALID-VERSION", "items-trash", "items")
}

func TestMigrateVersion5InvalidatesLegacyTagCacheForTypedRehydration(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data.db")
	s, err := OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("create current store: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close current store: %v", err)
	}

	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	const oldID = "Depression"
	const payload = `{"tag":"Depression","meta":{"type":1,"numItems":10}}`
	if _, err := raw.Exec(
		`INSERT INTO resources (id, resource_type, data) VALUES (?, 'tags', ?)`,
		oldID, payload,
	); err != nil {
		_ = raw.Close()
		t.Fatalf("seed legacy tag: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO resources_fts (id, resource_type, content) VALUES (?, 'tags', ?)`,
		oldID, payload,
	); err != nil {
		_ = raw.Close()
		t.Fatalf("seed legacy tag FTS: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO sync_state (resource_type, last_synced_at, total_count) VALUES ('tags', CURRENT_TIMESTAMP, 1)`,
	); err != nil {
		_ = raw.Close()
		t.Fatalf("seed legacy tag sync state: %v", err)
	}
	if _, err := raw.Exec(`PRAGMA user_version = 4`); err != nil {
		_ = raw.Close()
		t.Fatalf("stamp version 4: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	migrated, err := OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("migrate version 4 store: %v", err)
	}
	defer migrated.Close()
	for label, query := range map[string]string{
		"resources":  `SELECT count(*) FROM resources WHERE resource_type = 'tags'`,
		"FTS":        `SELECT count(*) FROM resources_fts WHERE resource_type = 'tags'`,
		"sync state": `SELECT count(*) FROM sync_state WHERE resource_type = 'tags'`,
	} {
		var count int
		if err := migrated.DB().QueryRow(query).Scan(&count); err != nil {
			t.Fatalf("count migrated %s: %v", label, err)
		}
		if count != 0 {
			t.Fatalf("migrated %s count = %d, want 0 before tag rehydration", label, count)
		}
	}

	if _, _, err := migrated.UpsertBatch("tags", []json.RawMessage{
		json.RawMessage(`{"tag":"Depression","meta":{"type":0,"numItems":5}}`),
		json.RawMessage(`{"tag":"Depression","meta":{"type":1,"numItems":10}}`),
	}); err != nil {
		t.Fatalf("rehydrate typed tags: %v", err)
	}
	if count, err := migrated.Count("tags"); err != nil || count != 2 {
		t.Fatalf("rehydrated tag count = %d, %v; want 2", count, err)
	}
}

func TestMigrateVersion6ReindexesFulltextBodyOnly(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data.db")
	s, err := OpenWithContext(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("create current store: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close current store: %v", err)
	}

	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw store: %v", err)
	}
	const attachmentKey = "ATTACH"
	const payload = `{"content":"only PDF prose is searchable","indexedPages":1}`
	if _, err := raw.Exec(
		`INSERT INTO resources (id, resource_type, data) VALUES (?, 'fulltext', ?)`,
		attachmentKey, payload,
	); err != nil {
		_ = raw.Close()
		t.Fatalf("seed fulltext resource: %v", err)
	}
	legacyRowID := ftsRowID("fulltext", attachmentKey) + 1
	if _, err := raw.Exec(
		`INSERT INTO resources_fts (rowid, id, resource_type, content) VALUES (?, ?, 'fulltext', ?)`,
		legacyRowID, attachmentKey, payload,
	); err != nil {
		_ = raw.Close()
		t.Fatalf("seed legacy fulltext index: %v", err)
	}
	if _, err := raw.Exec(`PRAGMA user_version = 5`); err != nil {
		_ = raw.Close()
		t.Fatalf("stamp version 5: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw store: %v", err)
	}

	migrated, err := OpenWithContext(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("migrate version 5 store: %v", err)
	}
	defer migrated.Close()
	var indexedRows int
	if err := migrated.DB().QueryRow(
		`SELECT count(*) FROM resources_fts WHERE id = ? AND resource_type = 'fulltext'`,
		attachmentKey,
	).Scan(&indexedRows); err != nil {
		t.Fatalf("count migrated fulltext rows: %v", err)
	}
	if indexedRows != 1 {
		t.Fatalf("migrated fulltext row count = %d, want 1", indexedRows)
	}
	var indexed string
	if err := migrated.DB().QueryRow(
		`SELECT content FROM resources_fts WHERE rowid = ?`,
		ftsRowID("fulltext", attachmentKey),
	).Scan(&indexed); err != nil {
		t.Fatalf("read migrated fulltext index: %v", err)
	}
	if indexed != "only PDF prose is searchable" {
		t.Fatalf("migrated fulltext index = %q", indexed)
	}
}

// legacyFNVFTSRowID is the rowid scheme a superseded binary used for
// resources_fts: FNV-1a over the same resource-qualified key the current
// sha256 scheme hashes, masked to 63 bits. It is reproduced here because it is
// the scheme found in a real 4906-item mirror, where it left a second indexed
// document for 4063 items. Test-only: no production path may write it.
func legacyFNVFTSRowID(resourceType, id string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(resourceType + "\x00" + id))
	return int64(h.Sum64() & 0x7FFFFFFFFFFFFFFF)
}

// ftsRowID is the address every write path uses to delete a resource's old
// index document before inserting the new one. Changing it therefore orphans
// every row already in the index — the rows become unaddressable, stay
// matchable, and every reader that joins the index returns the resource twice.
// That has happened twice already, which is what version 9 repairs.
//
// So the scheme is pinned. A change that breaks this test is not wrong, but it
// is a MIGRATION: bump StoreSchemaVersion and reap under the new scheme, the
// way version 9 does, or the next mirror silently double-counts.
func TestFTSRowIDSchemeIsPinned(t *testing.T) {
	for _, c := range []struct {
		resourceType string
		id           string
		want         int64
	}{
		{resourceType: "items", id: "FB2YZV5Z", want: 7435646800143279193},
		{resourceType: "items-trash", id: "FB2YZV5Z", want: 4609749548917306032},
		{resourceType: "fulltext", id: "FB2YZV5Z", want: 1065665020847398669},
		{resourceType: "collections", id: "ABCD1234", want: 5567516239958249985},
	} {
		if got := ftsRowID(c.resourceType, c.id); got != c.want {
			t.Errorf("ftsRowID(%q, %q) = %d, want %d — see the doc comment before changing this",
				c.resourceType, c.id, got, c.want)
		}
	}
	// The resource type is part of the key, so one id in two types addresses
	// two rows. Without that, a trashed copy would overwrite the live item's
	// document.
	if ftsRowID("items", "FB2YZV5Z") == ftsRowID("items-trash", "FB2YZV5Z") {
		t.Error("ftsRowID ignores the resource type, so two resources share one index row")
	}
}

// A row written under a superseded rowid scheme is never deleted again,
// because every write path deletes by the CURRENT rowid. The row stays in the
// index and keeps matching, so a reader that joins on (id, resource_type) sees
// the resource twice. Version 9 reaps those rows, and it must do it without
// losing the only document a resource has.
func TestMigrate_ReapsOrphanedFTSIndexRows(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data.db")
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE resources (
		id TEXT NOT NULL,
		resource_type TEXT NOT NULL,
		data JSON NOT NULL,
		parent_key TEXT,
		item_type TEXT,
		synced_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (resource_type, id)
	)`); err != nil {
		raw.Close()
		t.Fatalf("create pre-version-9 resources: %v", err)
	}
	if _, err := raw.Exec(`CREATE VIRTUAL TABLE resources_fts USING fts5(
		id, resource_type, content, tokenize='porter unicode61'
	)`); err != nil {
		raw.Close()
		t.Fatalf("create pre-version-9 FTS: %v", err)
	}
	seedResource := func(resourceType, id, payload string) {
		t.Helper()
		if _, err := raw.Exec(
			`INSERT INTO resources (id, resource_type, data) VALUES (?, ?, ?)`,
			id, resourceType, payload,
		); err != nil {
			t.Fatalf("seed %s/%s: %v", resourceType, id, err)
		}
	}
	seedIndex := func(rowid int64, resourceType, id, content string) {
		t.Helper()
		if _, err := raw.Exec(
			`INSERT INTO resources_fts (rowid, id, resource_type, content) VALUES (?, ?, ?, ?)`,
			rowid, id, resourceType, content,
		); err != nil {
			t.Fatalf("seed index %s/%s at %d: %v", resourceType, id, rowid, err)
		}
	}

	// DOUBLED: written by both binaries, so it holds two documents and is
	// returned twice by every reader.
	seedResource("items", "DOUBLED", `{"key":"DOUBLED","version":1,"data":{"key":"DOUBLED","itemType":"journalArticle","title":"Bushfire investigations in Australia"}}`)
	seedIndex(ftsRowID("items", "DOUBLED"), "items", "DOUBLED", "Bushfire investigations in Australia")
	seedIndex(legacyFNVFTSRowID("items", "DOUBLED"), "items", "DOUBLED", "Bushfire investigations in Australia stale")
	// STALEONLY: last written by the older binary, so the orphan is its ONLY
	// document. Reaping without reindexing would make it unsearchable.
	seedResource("items", "STALEONLY", `{"key":"STALEONLY","version":1,"data":{"key":"STALEONLY","itemType":"journalArticle","title":"Ember attack mechanisms"}}`)
	seedIndex(legacyFNVFTSRowID("items", "STALEONLY"), "items", "STALEONLY", "Ember attack mechanisms")
	// HEALTHY: one current document, and the migration must not touch it.
	seedResource("items", "HEALTHY", `{"key":"HEALTHY","version":1,"data":{"key":"HEALTHY","itemType":"journalArticle","title":"Prescribed burning policy"}}`)
	seedIndex(ftsRowID("items", "HEALTHY"), "items", "HEALTHY", "hand written document kept verbatim")
	// Residue from the pre-canonicalization items-top alias: no resource row
	// backs it, and purgeAliasResources could not address it by rowid.
	seedIndex(legacyFNVFTSRowID("items-top", "ALIASGONE"), "items-top", "ALIASGONE", "Bushfire investigations alias residue")

	if _, err := raw.Exec(`PRAGMA user_version = 8`); err != nil {
		raw.Close()
		t.Fatalf("stamp pre-version-9 schema: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	s, err := OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open and migrate pre-version-9 db: %v", err)
	}
	defer s.Close()

	orphans := map[string]int{}
	rows, err := s.DB().Query(`SELECT rowid, id, resource_type FROM resources_fts`)
	if err != nil {
		t.Fatalf("scan migrated index: %v", err)
	}
	documents := map[string]int{}
	for rows.Next() {
		var rowid int64
		var id, resourceType string
		if err := rows.Scan(&rowid, &id, &resourceType); err != nil {
			rows.Close()
			t.Fatalf("scan index row: %v", err)
		}
		documents[resourceType+"/"+id]++
		if rowid != ftsRowID(resourceType, id) {
			orphans[resourceType]++
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("scan migrated index: %v", err)
	}
	if len(orphans) != 0 {
		t.Errorf("orphaned index rows by resource type after migrating = %v, want none", orphans)
	}
	if got := documents["items/DOUBLED"]; got != 1 {
		t.Errorf("DOUBLED index documents = %d, want 1", got)
	}
	if got := documents["items/STALEONLY"]; got != 1 {
		t.Errorf("STALEONLY index documents = %d, want 1", got)
	}
	if got := documents["items-top/ALIASGONE"]; got != 0 {
		t.Errorf("alias residue documents = %d, want 0: no resource backs them", got)
	}

	// The reaped item is searchable exactly once, which is the user-visible
	// half: `search` and `items list --query` join this index.
	found, err := s.Search("Bushfire", 50)
	if err != nil {
		t.Fatalf("search migrated store: %v", err)
	}
	if len(found) != 1 {
		t.Errorf("search hits = %d, want 1 item once: %s", len(found), found)
	}
	// The item whose only document was stale keeps its recall, rebuilt from
	// the stored payload rather than from the stale text.
	if found, err := s.Search("Ember", 50); err != nil {
		t.Fatalf("search reindexed item: %v", err)
	} else if len(found) != 1 {
		t.Errorf("reindexed item hits = %d, want 1: reaping dropped its only document", len(found))
	}
	// A current document is left alone: no needless rewrite of the index.
	var healthy string
	if err := s.DB().QueryRow(
		`SELECT content FROM resources_fts WHERE rowid = ?`, ftsRowID("items", "HEALTHY"),
	).Scan(&healthy); err != nil {
		t.Fatalf("read healthy document: %v", err)
	}
	if healthy != "hand written document kept verbatim" {
		t.Errorf("healthy document = %q, want it untouched", healthy)
	}
}
