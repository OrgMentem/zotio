// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package store provides local SQLite persistence for zotio.
// Uses modernc.org/sqlite (pure Go, no CGO) for zero-dependency cross-compilation.
// FTS5 full-text search indexes are created for searchable content.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// IsUUID returns true if the input looks like a UUID.
func IsUUID(s string) bool {
	return uuidPattern.MatchString(s)
}

// StoreSchemaVersion is the on-disk schema version this binary understands.
// It is stamped into SQLite's PRAGMA user_version on fresh databases and
// checked on every open. Bump this whenever a migration changes table
// shape — adding columns, dropping indexes, changing FTS5 tokenizers —
// so an older binary refuses to open a newer database rather than silently
// producing wrong results against a schema it cannot read.
const StoreSchemaVersion = 4

type Store struct {
	db *sql.DB
	// writeMu serializes all DB writes. Read paths bypass the lock and run
	// concurrently against WAL. Resource-level concurrency in sync.go.tmpl
	// is 1 (one goroutine per resource via len(resources)-sized work channel)
	// — read-then-write sequences (e.g., GetSyncCursor → SaveSyncState) are
	// race-free by construction within a resource.
	writeMu sync.Mutex
	path    string
}

// OpenReadOnly opens an existing SQLite store at dbPath in read-only mode.
// mode=ro rejects direct and CTE-wrapped writes (INSERT, UPDATE, DELETE,
// REPLACE, "WITH x AS (...) INSERT ...") at the driver level. Skips
// MkdirAll and migrate; the file is expected to exist.
//
// The file: URI prefix is load-bearing: modernc.org/sqlite only honors
// SQLite's URI query parameters (mode, cache, etc.) when the DSN starts
// with "file:". Without the prefix, "?mode=ro" is silently dropped and
// the connection opens read-write. Underscore-prefixed driver pragmas
// (_journal_mode, _busy_timeout, etc.) work either way; they're parsed
// out of the DSN by the driver before sqlite3_open_v2.
func OpenReadOnly(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_journal_mode=WAL&_busy_timeout=10000&_foreign_keys=ON&_temp_store=MEMORY&_mmap_size=268435456")
	if err != nil {
		return nil, fmt.Errorf("opening database (read-only): %w", err)
	}
	db.SetMaxOpenConns(2)
	return &Store{db: db, path: dbPath}, nil
}

// Open opens or creates the SQLite store at dbPath using a background
// context. Thin wrapper over OpenWithContext for callers without a
// cancellable context.
func Open(dbPath string) (*Store, error) {
	return OpenWithContext(context.Background(), dbPath)
}

// OpenWithContext opens or creates the SQLite store at dbPath. The
// context is honored by the migration path: cancellation interrupts the
// retry-on-SQLITE_BUSY loop and propagates ctx.Err() back to the caller
// instead of waiting out the full migrationLockTimeout.
func OpenWithContext(ctx context.Context, dbPath string) (*Store, error) {
	dbDir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dbDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating db directory: %w", err)
	}
	if err := os.Chmod(dbDir, 0o700); err != nil {
		return nil, fmt.Errorf("securing db directory: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_synchronous=NORMAL&_busy_timeout=5000&_foreign_keys=ON&_temp_store=MEMORY&_mmap_size=268435456")
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	// WAL mode + 2 connections allows one read cursor open while a second
	// query executes (e.g., analytics commands calling helpers during row
	// iteration). Writes are still serialized by SQLite's WAL lock.
	db.SetMaxOpenConns(2)

	s := &Store{db: db, path: dbPath}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("running migrations: %w", err)
	}
	if err := os.Chmod(dbPath, 0o600); err != nil {
		db.Close()
		return nil, fmt.Errorf("securing database file: %w", err)
	}

	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// Path returns the on-disk path of the backing SQLite file.
func (s *Store) Path() string {
	return s.path
}

// DB exposes the underlying *sql.DB for callers that need to run ad-hoc
// queries (e.g., doctor's cache inspection, share snapshot import).
// Callers must not call Close on the returned handle.
func (s *Store) DB() *sql.DB {
	return s.db
}

// SchemaVersion reads PRAGMA user_version, which is stamped by migrate().
// A zero value means the database predates the schema-version gate — not
// a bug, but the caller may want to warn.
func (s *Store) SchemaVersion() (int, error) {
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return 0, fmt.Errorf("read user_version: %w", err)
	}
	return v, nil
}

// ensureColumn adds a column to an existing table if it isn't already
// present. It is the upgrade-path safety valve for schema additions:
// CREATE TABLE IF NOT EXISTS is a no-op when the table already exists, so
// columns added by newer binaries (e.g. parent_id from the dependent-
// resources work) never land on databases created by older binaries —
// which then trip "no such column" when a follow-on CREATE INDEX runs.
//
// Skips silently if the table doesn't yet exist (fresh install — the
// CREATE TABLE migration will create it with the column already declared)
// or if the column already exists. Runs on the pinned migration
// connection so it sees the writes performed by the in-flight BEGIN
// IMMEDIATE transaction; using s.db here would route through the pool
// and BUSY against the holding writer under concurrent migrators.
func (s *Store) ensureColumn(ctx context.Context, conn *sql.Conn, table, column, decl string) error {
	var name string
	err := conn.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table,
	).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking table %s: %w", table, err)
	}

	rows, err := conn.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info("%s")`, table))
	if err != nil {
		return fmt.Errorf("table_info %s: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var n, typ string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &n, &typ, &notnull, &dflt, &pk); err != nil {
			return fmt.Errorf("scan table_info %s: %w", table, err)
		}
		if n == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating table_info %s: %w", table, err)
	}

	if _, err := conn.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE "%s" ADD COLUMN "%s" %s`, table, column, decl)); err != nil {
		// A concurrent Open() may have added the column between our
		// PRAGMA check and this ALTER. SQLite returns SQLITE_ERROR with
		// "duplicate column name", which busy_timeout does not retry.
		// The DB is now in the desired state regardless of who won.
		if strings.Contains(err.Error(), "duplicate column name") {
			return nil
		}
		return fmt.Errorf("add column %s.%s: %w", table, column, err)
	}
	return nil
}

// backfillColumns adds columns that newer binaries declare but that
// pre-existing databases (created before those columns were added) lack.
// Must run before the migrations slice so that subsequent CREATE INDEX
// statements referencing the column can succeed against the upgraded
// table. Idempotent: safe to call on fresh DBs (table-not-found short-
// circuit) and on already-current DBs (column-exists short-circuit).
//
// Table names are emitted bare (no safeName) — ensureColumn double-quotes
// them at SQL emit time and uses parameter binding for the sqlite_master
// lookup, so the values flow as Go string literals first and SQL
// identifiers second. Wrapping with safeName here would embed literal
// double-quote characters into the Go string and break compilation for
// any spec whose dependent-resource snake_cased name is a SQL reserved
// word.
func (s *Store) backfillColumns(ctx context.Context, conn *sql.Conn) error {
	for _, c := range []struct{ table, column, decl string }{
		{table: "sync_state", column: "last_cursor", decl: "TEXT"},
		{table: "sync_state", column: "last_synced_at", decl: "DATETIME"},
		{table: "sync_state", column: "total_count", decl: "INTEGER DEFAULT 0"},
		// Zotero incremental sync is keyed on an
		// integer library version (Last-Modified-Version), not a timestamp.
		{table: "sync_state", column: "library_version", decl: "INTEGER DEFAULT 0"},
		// indexed columns for dependent-resource sync
		// (annotations / attachments) so parent/type queries don't scan JSON.
		{table: "resources", column: "parent_key", decl: "TEXT"},
		{table: "resources", column: "item_type", decl: "TEXT"},
		{table: "resources", column: "annotation_color", decl: "TEXT"},
		{table: "resources", column: "item_date", decl: "TEXT"},
	} {
		if err := s.ensureColumn(ctx, conn, c.table, c.column, c.decl); err != nil {
			return err
		}
	}
	return nil
}

// backfillIndexedColumnValues populates the dependent-resource columns for rows
// inserted before those columns existed. backfillColumns only ADDS the columns
// (leaving NULL for existing rows), which silently breaks every query that
// filters on them — e.g. `items audit` missing-pdf/missing-doi return 0 because
// `item_type IN (...)` never matches a NULL. The `item_type IS NULL` guard makes
// this a one-time, idempotent no-op on already-populated stores (insert writes
// ” rather than NULL for type-less rows like collections). The json paths
// mirror extractIndexedColumnsFromObj.
func (s *Store) backfillIndexedColumnValues(ctx context.Context, conn *sql.Conn) error {
	_, err := conn.ExecContext(ctx, `
UPDATE resources SET
	item_type = COALESCE(json_extract(data, '$.data.itemType'), json_extract(data, '$.itemType'), ''),
	parent_key = COALESCE(json_extract(data, '$.data.parentItem'), json_extract(data, '$.parentItem'), ''),
	annotation_color = COALESCE(json_extract(data, '$.data.annotationColor'), json_extract(data, '$.annotationColor'), ''),
	item_date = COALESCE(json_extract(data, '$.data.dateModified'), json_extract(data, '$.dateModified'), json_extract(data, '$.data.date'), json_extract(data, '$.date'), '')
WHERE item_type IS NULL`)
	return err
}

// resourcesTablePKComposite reports whether the on-disk resources table uses the
// composite (resource_type, id) primary key. Older binaries created the table
// with an id-only PK, which both risks cross-resource-type id collisions (a tag
// named "annotation" vs the "annotation" itemType) and breaks the
// ON CONFLICT(resource_type, id) upsert. Returns (composite, exists).
func resourcesTablePKComposite(ctx context.Context, conn *sql.Conn) (composite bool, exists bool, err error) {
	rows, err := conn.QueryContext(ctx, `PRAGMA table_info(resources)`)
	if err != nil {
		return false, false, err
	}
	defer rows.Close()
	pkCols := 0
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, false, err
		}
		exists = true
		if pk > 0 {
			pkCols++
		}
	}
	if err := rows.Err(); err != nil {
		return false, false, err
	}
	return pkCols >= 2, exists, nil
}

// rebuildResourcesPKIfNeeded upgrades a legacy id-only-PK resources table to the
// composite (resource_type, id) PK in place, preserving rows. No-op when the
// table is fresh or already composite. Runs inside the migration transaction
// after backfillColumns guarantees all columns exist; the migrations slice that
// follows recreates the (table-scoped) indexes dropped with the old table.
func (s *Store) rebuildResourcesPKIfNeeded(ctx context.Context, conn *sql.Conn) error {
	composite, exists, err := resourcesTablePKComposite(ctx, conn)
	if err != nil {
		return err
	}
	if !exists || composite {
		return nil
	}
	for _, stmt := range []string{
		`CREATE TABLE resources_pkfix (
			id TEXT NOT NULL,
			resource_type TEXT NOT NULL,
			data JSON NOT NULL,
			parent_key TEXT,
			item_type TEXT,
			annotation_color TEXT,
			item_date TEXT,
			synced_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (resource_type, id)
		)`,
		`INSERT OR IGNORE INTO resources_pkfix (id, resource_type, data, parent_key, item_type, annotation_color, item_date, synced_at, updated_at)
			SELECT id, resource_type, data, parent_key, item_type, annotation_color, item_date, synced_at, updated_at FROM resources`,
		`DROP TABLE resources`,
		`ALTER TABLE resources_pkfix RENAME TO resources`,
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("rebuilding resources primary key: %w", err)
		}
	}
	return nil
}

func (s *Store) migrate(ctx context.Context) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquiring migration connection: %w", err)
	}
	defer conn.Close()

	// Read user_version before the migration lock so an old binary
	// opening a newer-schema DB rejects immediately. WAL readers don't
	// normally block on writers, but the fresh-DB WAL-init race can BUSY
	// a SELECT — share the lock's deadline so total budget stays bounded.
	deadline := time.Now().Add(migrationLockTimeout)
	var current int
	if err := retryOnBusy(ctx, deadline, "reading schema version", func() error {
		return conn.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&current)
	}); err != nil {
		return err
	}
	if current > StoreSchemaVersion {
		return fmt.Errorf("database schema version %d is newer than supported version %d; upgrade the CLI binary or open an older database", current, StoreSchemaVersion)
	}

	migrations := []string{
		`CREATE TABLE IF NOT EXISTS resources (
			id TEXT NOT NULL,
			resource_type TEXT NOT NULL,
			data JSON NOT NULL,
			parent_key TEXT,
			item_type TEXT,
			annotation_color TEXT,
			item_date TEXT,
			synced_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (resource_type, id)
		)`, // parent_key/item_type/annotation_color/item_date.
		`CREATE INDEX IF NOT EXISTS idx_resources_type ON resources(resource_type)`,
		`CREATE INDEX IF NOT EXISTS idx_resources_synced ON resources(synced_at)`,
		// index the dependent-resource lookup columns.
		`CREATE INDEX IF NOT EXISTS idx_resources_parent_key ON resources(parent_key)`,
		`CREATE INDEX IF NOT EXISTS idx_resources_item_type ON resources(item_type)`,
		`CREATE TABLE IF NOT EXISTS sync_state (
			resource_type TEXT PRIMARY KEY,
			last_cursor TEXT,
			last_synced_at DATETIME,
			total_count INTEGER DEFAULT 0,
			library_version INTEGER DEFAULT 0
		)`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS resources_fts USING fts5(
			id, resource_type, content, tokenize='porter unicode61'
		)`,
	}

	// Run every migration — including the column backfill and the
	// schema-version stamp — inside a single BEGIN IMMEDIATE transaction
	// pinned to one connection. IMMEDIATE acquires SQLite's RESERVED lock
	// at BEGIN time so concurrent migrators serialize on it instead of
	// racing per-statement and tripping SQLITE_BUSY despite busy_timeout.
	// modernc.org/sqlite's busy_timeout does not always cover write-write
	// contention at BEGIN/COMMIT time, so we retry both explicitly on
	// SQLITE_BUSY for up to migrationLockTimeout.
	return withMigrationLock(ctx, conn, deadline, func() error {
		// Re-read user_version inside the lock. This is load-bearing,
		// not paranoid: between the pre-lock read above and our
		// successful BEGIN IMMEDIATE, a newer-binary peer may have
		// committed a higher version stamp. Without this re-read, an
		// older binary (smaller StoreSchemaVersion) would proceed to
		// stamp its own lower version at the end of the closure,
		// silently downgrading user_version on a schema that's already
		// at the newer level. Future maintainers: leave this read in.
		var current int
		if err := conn.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&current); err != nil {
			return fmt.Errorf("reading schema version: %w", err)
		}
		if current > StoreSchemaVersion {
			return fmt.Errorf("database schema version %d is newer than supported version %d; upgrade the CLI binary or open an older database", current, StoreSchemaVersion)
		}

		if err := s.backfillColumns(ctx, conn); err != nil {
			return fmt.Errorf("backfilling columns: %w", err)
		}
		// upgrade a legacy id-only-PK resources table to the
		// composite (resource_type, id) PK so ON CONFLICT(resource_type, id) works
		// and cross-type ids cannot collide. No-op on fresh/already-composite DBs.
		if err := s.rebuildResourcesPKIfNeeded(ctx, conn); err != nil {
			return err
		}
		for _, m := range migrations {
			if _, err := conn.ExecContext(ctx, m); err != nil {
				return fmt.Errorf("migration failed: %w", err)
			}
		}
		if current < 4 {
			if err := reconcileExistingItemLifecycle(ctx, conn); err != nil {
				return fmt.Errorf("reconciling item lifecycle: %w", err)
			}
		}
		// populate the indexed columns for rows that
		// predate them; without this, item_type/parent_key stay NULL and break
		// the audit/query commands that filter on them.
		if err := s.backfillIndexedColumnValues(ctx, conn); err != nil {
			return fmt.Errorf("backfilling indexed column values: %w", err)
		}
		// pre-canonicalization archive
		// runs wrote items-top/collections-top as independent resource types.
		// New syncs fold those aliases into items/collections, so purge the
		// frozen alias resource/sync-state rows idempotently on open instead of
		// surfacing stale counts in status/doctor.
		if err := s.purgeAliasResources(ctx, conn); err != nil {
			return fmt.Errorf("purging alias resources: %w", err)
		}
		// run novel-feature auxiliary-table
		// migrations after the generated migrations and before the version
		// stamp, as migrateExtras documents. Currently a no-op (empty slice);
		// wiring it makes the extension seam actually execute on every open.
		if err := s.migrateExtras(ctx, conn); err != nil {
			return err
		}
		// Stamp the current schema version only after all schema and data
		// migrations succeed. On an already-current DB this is a no-op write;
		// older and pre-gate databases are upgraded transactionally above.
		if _, err := conn.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, StoreSchemaVersion)); err != nil {
			return fmt.Errorf("stamp user_version: %w", err)
		}
		return nil
	})
}

func (s *Store) purgeAliasResources(ctx context.Context, conn *sql.Conn) error {
	aliases := []string{"items-top", "collections-top"}
	hasSyncStateResourceType, err := tableHasColumn(ctx, conn, "sync_state", "resource_type")
	if err != nil {
		return err
	}
	for _, resourceType := range aliases {
		rows, err := conn.QueryContext(ctx, `SELECT id FROM resources WHERE resource_type = ?`, resourceType)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for _, id := range ids {
			if _, err := conn.ExecContext(ctx, `DELETE FROM resources_fts WHERE rowid = ?`, ftsRowID(resourceType, id)); err != nil {
				fmt.Fprintf(os.Stderr, "warning: FTS alias cleanup failed for %s/%s: %v\n", resourceType, id, err)
			}
		}
		if _, err := conn.ExecContext(ctx, `DELETE FROM resources WHERE resource_type = ?`, resourceType); err != nil {
			return err
		}
		if hasSyncStateResourceType {
			if _, err := conn.ExecContext(ctx, `DELETE FROM sync_state WHERE resource_type = ?`, resourceType); err != nil {
				return err
			}
		}
	}
	return nil
}

// reconcileExistingItemLifecycle is the version-4 data migration. It runs on
// the pinned connection while migrate holds BEGIN IMMEDIATE, so each losing
// resource and its deterministic FTS row disappear in the same migration
// transaction. Reading all conflicts before deleting also keeps cursor
// lifetimes separate from writes for SQLite driver portability.
func reconcileExistingItemLifecycle(ctx context.Context, conn *sql.Conn) error {
	rows, err := conn.QueryContext(ctx, `
SELECT live.id, live.data, trash.data
FROM resources AS live
JOIN resources AS trash ON trash.id = live.id
WHERE live.resource_type = 'items'
  AND trash.resource_type = 'items-trash'`)
	if err != nil {
		return err
	}
	type conflict struct {
		id        string
		liveData  string
		trashData string
	}
	var conflicts []conflict
	for rows.Next() {
		var item conflict
		if err := rows.Scan(&item.id, &item.liveData, &item.trashData); err != nil {
			rows.Close()
			return err
		}
		conflicts = append(conflicts, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	for _, item := range conflicts {
		var live, trash map[string]any
		if err := json.Unmarshal([]byte(item.liveData), &live); err != nil {
			return fmt.Errorf("reconcile lifecycle: unmarshal live item %s: %w", item.id, err)
		}
		if err := json.Unmarshal([]byte(item.trashData), &trash); err != nil {
			return fmt.Errorf("reconcile lifecycle: unmarshal trash item %s: %w", item.id, err)
		}
		loserType := "items"
		if zoteroObjectVersion(live) > zoteroObjectVersion(trash) {
			loserType = "items-trash"
		}
		if _, err := conn.ExecContext(ctx,
			`DELETE FROM resources WHERE resource_type = ? AND id = ?`,
			loserType, item.id,
		); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx,
			`DELETE FROM resources_fts WHERE rowid = ?`,
			ftsRowID(loserType, item.id),
		); err != nil {
			return err
		}
	}
	return nil
}

func tableHasColumn(ctx context.Context, conn *sql.Conn, table, column string) (bool, error) {
	rows, err := conn.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info("%s")`, table))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

const (
	migrationLockTimeout    = 30 * time.Second
	migrationLockBackoffMin = 5 * time.Millisecond
	migrationLockBackoffMax = 100 * time.Millisecond
)

// withMigrationLock runs fn inside a BEGIN IMMEDIATE / COMMIT pair on
// conn, retrying both BEGIN and COMMIT on SQLITE_BUSY against the
// caller-provided deadline. Sharing the deadline with the pre-lock
// version read keeps total Open() latency bounded by a single budget.
// The real upper bound is deadline + one trailing backoff interval
// (≤100ms) + the driver's busy_timeout for the in-flight Exec, since
// the deadline is checked after each failed attempt rather than as a
// hard wall-clock cutoff. fn must use conn (not s.db) so its writes
// participate in the held transaction.
func withMigrationLock(ctx context.Context, conn *sql.Conn, deadline time.Time, fn func() error) error {
	if err := execWithBusyRetry(ctx, conn, "BEGIN IMMEDIATE", "begin migration transaction", deadline); err != nil {
		return err
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		// ROLLBACK uses context.Background() so caller-context cancellation
		// can't strand the connection in an open transaction. A failed
		// rollback is rare on local SQLite (broken file handle, fatal
		// driver error) but worth surfacing — silent swallow leaves a
		// pinned connection returned to the pool with state that will
		// confuse later queries.
		if _, rerr := conn.ExecContext(context.Background(), "ROLLBACK"); rerr != nil {
			fmt.Fprintf(os.Stderr, "warning: store migration rollback failed: %v\n", rerr)
		}
	}()

	if err := fn(); err != nil {
		return err
	}

	if err := execWithBusyRetry(ctx, conn, "COMMIT", "commit migration transaction", deadline); err != nil {
		return err
	}
	committed = true
	return nil
}

// execWithBusyRetry runs stmt on conn and retries on SQLITE_BUSY until
// deadline. It covers BEGIN IMMEDIATE and COMMIT contention;
// modernc.org/sqlite's busy_timeout does not reliably cover either when
// multiple connections race for the WAL write lock.
func execWithBusyRetry(ctx context.Context, conn *sql.Conn, stmt, label string, deadline time.Time) error {
	return retryOnBusy(ctx, deadline, label, func() error {
		_, err := conn.ExecContext(ctx, stmt)
		return err
	})
}

// retryOnBusy runs op and retries it on SQLITE_BUSY/LOCKED until
// deadline. The same retry shape covers Exec, Query, and any other
// SQLite call that can race the WAL writer lock — including the
// pre-lock user_version read, where the WAL initialization race on a
// fresh DB can BUSY a SELECT that should otherwise succeed under WAL
// reader/writer concurrency.
func retryOnBusy(ctx context.Context, deadline time.Time, label string, op func() error) error {
	backoff := migrationLockBackoffMin
	for {
		err := op()
		if err == nil {
			return nil
		}
		if !isSQLiteBusy(err) {
			return fmt.Errorf("%s: %w", label, err)
		}
		if time.Now().After(deadline) {
			// The label carries the operation context (e.g. "begin
			// migration transaction", "reading schema version") — we
			// don't hardcode "waiting for write lock" because pre-lock
			// reads also flow through this helper.
			return fmt.Errorf("%s: timed out after %s under SQLite contention: %w", label, migrationLockTimeout, err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: %w", label, ctx.Err())
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, migrationLockBackoffMax)
	}
}

// isSQLiteBusy reports whether err is a retryable SQLite lock condition.
// Covers both the file-level WAL writer race (SQLITE_BUSY / "database is
// locked") and the table-level shared-cache contention (SQLITE_LOCKED /
// "database table is locked"). The match is on the error string because
// modernc.org/sqlite does not export an error type the generated code
// can switch on without dragging the driver package into every store
// consumer.
func isSQLiteBusy(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "SQLITE_BUSY") ||
		strings.Contains(msg, "SQLITE_LOCKED") ||
		strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked")
}

func (s *Store) upsertGenericResourceTx(tx *sql.Tx, resourceType, id string, data json.RawMessage, obj map[string]any) error {
	// populate indexed dependent-resource columns from the
	// payload so annotation/attachment queries avoid scanning JSON. Non-item
	// rows (collections, tags) leave these empty, which is harmless.
	// reuse the caller's already-unmarshaled obj
	// (batch path) instead of parsing the same payload a second time. The single
	// Upsert path passes nil and falls back to the raw-bytes extractor.
	var parentKey, itemType, color, itemDate string
	if obj != nil {
		parentKey, itemType, color, itemDate = extractIndexedColumnsFromObj(obj)
	} else {
		parentKey, itemType, color, itemDate = extractIndexedColumns(data)
	}
	_, err := tx.Exec(
		`INSERT INTO resources (id, resource_type, data, parent_key, item_type, annotation_color, item_date, synced_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(resource_type, id) DO UPDATE SET data = excluded.data, parent_key = excluded.parent_key, item_type = excluded.item_type, annotation_color = excluded.annotation_color, item_date = excluded.item_date, synced_at = excluded.synced_at, updated_at = excluded.updated_at`,
		id, resourceType, string(data), parentKey, itemType, color, itemDate, time.Now(), time.Now(),
	)
	if err != nil {
		return err
	}

	ftsRowid := ftsRowID(resourceType, id)
	// Use explicit rowid for FTS5 compatibility with modernc.org/sqlite.
	// Standard DELETE WHERE column=? may not work on FTS5 virtual tables.
	if _, err = tx.Exec(`DELETE FROM resources_fts WHERE rowid = ?`, ftsRowid); err != nil {
		fmt.Fprintf(os.Stderr, "warning: FTS index cleanup failed: %v\n", err)
	}

	searchDocument := buildSearchDocument(resourceType, data)
	if obj != nil {
		// reuse the already
		// unmarshaled batch object when constructing the FTS document instead of
		// parsing every synced item a second time while the SQLite write lock is held.
		searchDocument = buildSearchDocumentFromObj(resourceType, data, obj)
	}
	if _, err = tx.Exec(
		`INSERT INTO resources_fts (rowid, id, resource_type, content)
		 VALUES (?, ?, ?, ?)`,
		// index a curated Zotero-aware document for items
		// instead of the raw JSON blob (raw JSON retained for other types).
		ftsRowid, id, resourceType, searchDocument,
	); err != nil {
		// FTS insert failure is non-fatal
		fmt.Fprintf(os.Stderr, "warning: FTS index update failed: %v\n", err)
	}

	return nil
}

// reconcileItemLifecycleTx enforces the single canonical item state after an
// items or items-trash row has been written. Zotero versions are compared with
// the top-level field taking precedence over the nested data.version fallback;
// absent and non-numeric versions compare as zero. Trash wins equal versions so
// a late live page cannot resurrect a deletion.
func reconcileItemLifecycleTx(tx *sql.Tx, resourceType, id string, incoming map[string]any) error {
	if resourceType != "items" && resourceType != "items-trash" {
		return nil
	}
	oppositeType := "items"
	if resourceType == "items" {
		oppositeType = "items-trash"
	}

	var oppositeData string
	err := tx.QueryRow(
		`SELECT data FROM resources WHERE resource_type = ? AND id = ?`,
		oppositeType, id,
	).Scan(&oppositeData)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading opposite item state %s/%s: %w", oppositeType, id, err)
	}

	var opposite map[string]any
	if err := json.Unmarshal([]byte(oppositeData), &opposite); err != nil {
		return fmt.Errorf("reconcile lifecycle: unmarshal opposite item state %s/%s: %w", oppositeType, id, err)
	}
	incomingVersion := zoteroObjectVersion(incoming)
	oppositeVersion := zoteroObjectVersion(opposite)
	loserType := resourceType
	if incomingVersion > oppositeVersion ||
		(incomingVersion == oppositeVersion && resourceType == "items-trash") {
		loserType = oppositeType
	}
	if _, err := tx.Exec(
		`DELETE FROM resources WHERE resource_type = ? AND id = ?`,
		loserType, id,
	); err != nil {
		return fmt.Errorf("deleting losing item state %s/%s: %w", loserType, id, err)
	}
	if _, err := tx.Exec(
		`DELETE FROM resources_fts WHERE rowid = ?`,
		ftsRowID(loserType, id),
	); err != nil {
		return fmt.Errorf("deleting losing item FTS row %s/%s: %w", loserType, id, err)
	}
	return nil
}

func zoteroObjectVersion(obj map[string]any) float64 {
	if obj == nil {
		return 0
	}
	if version, ok := obj["version"]; ok {
		value, _ := version.(float64)
		return value
	}
	if data, ok := obj["data"].(map[string]any); ok {
		value, _ := data["version"].(float64)
		return value
	}
	return 0
}

// extractIndexedColumns pulls the indexed dependent-resource columns out of a
// stored item payload. Zotero item objects nest the real fields under a "data"
// sub-object ({key, version, data:{itemType, parentItem, ...}}); this descends
// into "data" when present and falls back to the top level otherwise. Missing
// fields yield empty strings.
func extractIndexedColumns(data json.RawMessage) (parentKey, itemType, color, itemDate string) {
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return "", "", "", ""
	}
	return extractIndexedColumnsFromObj(obj)
}

// extractIndexedColumnsFromObj is the map-based core of extractIndexedColumns,
// letting the batch upsert path reuse the object it already unmarshaled instead
// of parsing the same payload twice.
func extractIndexedColumnsFromObj(obj map[string]any) (parentKey, itemType, color, itemDate string) {
	fields := obj
	if inner, ok := obj["data"].(map[string]any); ok {
		fields = inner
	}
	asStr := func(v any) string {
		s, _ := v.(string)
		return s
	}
	parentKey = asStr(LookupFieldValue(fields, "parent_item"))
	itemType = asStr(LookupFieldValue(fields, "item_type"))
	color = asStr(LookupFieldValue(fields, "annotation_color"))
	itemDate = asStr(LookupFieldValue(fields, "date_modified"))
	if itemDate == "" {
		itemDate = asStr(LookupFieldValue(fields, "date"))
	}
	return parentKey, itemType, color, itemDate
}

func buildSearchDocumentFromObj(resourceType string, data json.RawMessage, obj map[string]any) string {
	if resourceType != "items" {
		return string(data)
	}
	fields := obj
	if inner, ok := obj["data"].(map[string]any); ok {
		fields = inner
	}

	var parts []string
	add := func(v any) {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			parts = append(parts, s)
		}
	}

	add(obj["key"])
	for _, f := range itemSearchFields {
		add(fields[f])
	}
	if creators, ok := fields["creators"].([]any); ok {
		for _, c := range creators {
			if cm, ok := c.(map[string]any); ok {
				add(cm["firstName"])
				add(cm["lastName"])
				add(cm["name"])
			}
		}
	}
	if tags, ok := fields["tags"].([]any); ok {
		for _, t := range tags {
			if tm, ok := t.(map[string]any); ok {
				add(tm["tag"])
			}
		}
	}

	doc := strings.Join(parts, " ")
	if strings.TrimSpace(doc) == "" {
		return string(data)
	}
	return doc
}

func (s *Store) Upsert(resourceType, id string, data json.RawMessage) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var obj map[string]any
	if resourceType == "items" || resourceType == "items-trash" {
		if err := json.Unmarshal(data, &obj); err != nil {
			return fmt.Errorf("upsert %s/%s: unmarshal item payload: %w", resourceType, id, err)
		}
	}
	if err := s.upsertGenericResourceTx(tx, resourceType, id, data, obj); err != nil {
		return err
	}
	if err := reconcileItemLifecycleTx(tx, resourceType, id, obj); err != nil {
		return err
	}

	return tx.Commit()
}

// UpsertKeyed inserts or replaces multiple records whose primary keys come from
// the caller (not the payload) in a single transaction. It fits resources whose
// id is carried by the request path rather than the body — e.g. per-item
// fulltext, where the response is {content, indexedChars, ...} with no id field.
// Uses a single batched transaction instead of one writeMu-serialized Upsert
// per item, avoiding many tiny transactions and lock contention.
func (s *Store) UpsertKeyed(resourceType string, ids []string, data []json.RawMessage) error {
	if len(ids) != len(data) {
		return fmt.Errorf("UpsertKeyed: ids/data length mismatch (%d vs %d)", len(ids), len(data))
	}
	if len(ids) == 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("starting keyed batch transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for i, id := range ids {
		if err := s.upsertGenericResourceTx(tx, resourceType, id, data[i], nil); err != nil {
			return fmt.Errorf("upserting %s/%s: %w", resourceType, id, err)
		}
	}
	return tx.Commit()
}

func (s *Store) Get(resourceType, id string) (json.RawMessage, error) {
	var data string
	err := s.db.QueryRow(
		`SELECT data FROM resources WHERE resource_type = ? AND id = ?`,
		resourceType, id,
	).Scan(&data)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return json.RawMessage(data), nil
}

func (s *Store) List(resourceType string, limit int) ([]json.RawMessage, error) {
	query := `SELECT data FROM resources WHERE resource_type = ? ORDER BY updated_at DESC`
	args := []any{resourceType}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.Query(
		// limit <= 0 means "all rows" for local list reads.
		query,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []json.RawMessage
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		results = append(results, json.RawMessage(data))
	}
	return results, rows.Err()
}

// Search runs an FTS search over all resource types. A limit of 0 applies the
// interactive default of 50; a negative limit means no limit (SQLite LIMIT -1),
// letting callers such as resolveScope enumerate the full match cohort.
func (s *Store) Search(query string, limit int) ([]json.RawMessage, error) {
	if limit == 0 {
		limit = 50
	}
	rows, err := s.queryWithBusyRetry(
		`SELECT r.data FROM resources r
		 JOIN resources_fts f ON r.id = f.id AND r.resource_type = f.resource_type
		 WHERE resources_fts MATCH ?
		 ORDER BY rank
		 LIMIT ?`,
		// ftsMatchQuery preserves documented boolean operators and phrases.
		ftsMatchQuery(query), limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	results := make([]json.RawMessage, 0)
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		results = append(results, json.RawMessage(data))
	}
	return results, rows.Err()
}

// ItemsByType returns the stored payloads of all items with the given
// itemType (e.g. "annotation", "attachment"). limit <= 0 means no limit.
// backs local-first annotation listing.
func (s *Store) ItemsByType(itemType string, limit int) ([]json.RawMessage, error) {
	query := `SELECT data FROM resources WHERE item_type = ?`
	args := []any{itemType}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []json.RawMessage
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		results = append(results, json.RawMessage(data))
	}
	return results, rows.Err()
}

// AnnotationsForItem returns every annotation whose attachment parent's own
// parent is topItemKey. Zotero nests annotations under attachments under the
// top-level item, so this joins annotation -> attachment -> top item.
// backs local-first annotation export/timeline.
func (s *Store) AnnotationsForItem(topItemKey string) ([]json.RawMessage, error) {
	rows, err := s.db.Query(
		`SELECT a.data FROM resources a
		 JOIN resources att ON a.parent_key = att.id
		 WHERE a.item_type = 'annotation' AND att.parent_key = ?`,
		topItemKey,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []json.RawMessage
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		results = append(results, json.RawMessage(data))
	}
	return results, rows.Err()
}

// AnnotationsForItems returns annotations grouped by their top-level item key
// for every key in topItemKeys. It batches the lookup to stay below SQLite's
// variable limit while still avoiding a per-item AnnotationsForItem query. Keys
// with no annotations are simply absent from the returned map.
func (s *Store) AnnotationsForItems(topItemKeys []string) (map[string][]json.RawMessage, error) {
	out := make(map[string][]json.RawMessage, len(topItemKeys))
	if len(topItemKeys) == 0 {
		return out, nil
	}
	const batchSize = 500
	for start := 0; start < len(topItemKeys); start += batchSize {
		end := start + batchSize
		if end > len(topItemKeys) {
			end = len(topItemKeys)
		}
		batch := topItemKeys[start:end]
		placeholders := make([]string, len(batch))
		args := make([]any, len(batch))
		for i, k := range batch {
			placeholders[i] = "?"
			args[i] = k
		}
		// #nosec G202 -- placeholder count is dynamic but values are safe questions marks
		rows, err := s.db.Query(
			`SELECT att.parent_key, a.data FROM resources a
			 JOIN resources att ON a.parent_key = att.id
			 WHERE a.item_type = 'annotation' AND att.parent_key IN (`+strings.Join(placeholders, ",")+`)`,
			args...,
		)
		if err != nil {
			return nil, err
		}

		for rows.Next() {
			var top, data string
			if err := rows.Scan(&top, &data); err != nil {
				_ = rows.Close()
				return nil, err
			}
			out[top] = append(out[top], json.RawMessage(data))
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Fulltext returns the stored full-text payload for an attachment key, if any.
// backs local-first PDF full-text reads.
func (s *Store) Fulltext(attachmentKey string) (json.RawMessage, bool, error) {
	var data string
	err := s.db.QueryRow(
		`SELECT data FROM resources WHERE resource_type = 'fulltext' AND id = ?`,
		attachmentKey,
	).Scan(&data)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return json.RawMessage(data), true, nil
}

// ftsRowID derives a deterministic rowid from a resource-qualified ID for use
// with FTS5. modernc.org/sqlite's FTS5 implementation may not support DELETE
// WHERE column=? on virtual tables, so we use explicit rowids and DELETE WHERE
// rowid=? instead.
func ftsRowID(resourceType, id string) int64 {
	// include resourceType in the
	// key and use a SHA-256-derived 63-bit value instead of the old small
	// multiplier hash, reducing accidental FTS overwrite risk.
	sum := sha256.Sum256([]byte(resourceType + "\x00" + id))
	return int64(binary.BigEndian.Uint64(sum[:8]) & 0x7FFFFFFFFFFFFFFF)
}

// LookupFieldValue resolves a field value from a JSON object map, trying
// the snake_case key first and the camelCase rendering second. Exported so
// the sync command's extractID and the upsert path resolve fields the same
// way — a divergence here produces silent drops on heterogeneous payloads.
func LookupFieldValue(obj map[string]any, snakeKey string) any {
	if v, ok := obj[snakeKey]; ok {
		return sqliteFieldValue(v)
	}
	parts := strings.Split(snakeKey, "_")
	for i := 1; i < len(parts); i++ {
		if parts[i] == "" {
			continue
		}
		parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
	}
	if v, ok := obj[strings.Join(parts, "")]; ok {
		return sqliteFieldValue(v)
	}
	return nil
}

func sqliteFieldValue(v any) any {
	switch v.(type) {
	case nil, string, bool, int, int64, float64, []byte:
		return v
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(data)
	}
}

// lookupFieldValue is kept as an unexported alias for in-package callers so
// the existing UpsertBatch code reads naturally without prefixing every call
// with the package name.
func lookupFieldValue(obj map[string]any, snakeKey string) any {
	return LookupFieldValue(obj, snakeKey)
}

// ResourceIDFieldOverrides maps resources whose primary key is a domain-name
// field rather than a generic id/name/key field. prepareResourceItem consults
// this first so the domain field wins over the generic fallback list; sync
// shares it so both ingest paths key rows identically.
var ResourceIDFieldOverrides = map[string]string{
	// Zotero tags and global schema lists are keyed by domain-name fields.
	"tags":                  "tag",
	"schema":                "itemType",
	"schema-creator-fields": "field",
	"schema-item-fields":    "field",
}

// genericIDFieldFallbacks is the runtime safety net for resources that did
// NOT receive a templated IDField. API-specific names belong in spec
// annotations (x-resource-id), not this list.
var genericIDFieldFallbacks = []string{"id", "ID", "name", "uuid", "slug", "key", "code", "uid"}

type preparedResourceItem struct {
	id   string
	data json.RawMessage
	obj  map[string]any
}

func prepareResourceItem(resourceType string, item json.RawMessage) (preparedResourceItem, bool, bool) {
	var obj map[string]any
	if err := json.Unmarshal(item, &obj); err != nil {
		return preparedResourceItem{}, false, false
	}

	// Templated IDField wins; generic fallback list runs second when the
	// override is empty OR the override field is absent on this particular item
	// (response shape mismatches happen even when the spec declares x-resource-id).
	var id string
	if override, ok := ResourceIDFieldOverrides[resourceType]; ok && override != "" {
		if v := lookupFieldValue(obj, override); v != nil {
			s := fmt.Sprintf("%v", v)
			if s != "" && s != "<nil>" {
				id = s
			}
		}
	}
	if id == "" {
		for _, key := range genericIDFieldFallbacks {
			if v := lookupFieldValue(obj, key); v != nil {
				s := fmt.Sprintf("%v", v)
				if s != "" && s != "<nil>" {
					id = s
					break
				}
			}
		}
	}
	if id == "" {
		return preparedResourceItem{}, false, true
	}
	return preparedResourceItem{id: id, data: item, obj: obj}, true, true
}

// UpsertBatch inserts or replaces multiple records in a single transaction
// and returns (stored, extractFailures, err). stored counts rows actually
// landed; extractFailures counts items that survived JSON unmarshal but had
// no extractable primary key (templated IDField AND generic fallback both
// missed). callers (sync.go.tmpl) compare these against len(items) to emit
// the per-item primary_key_unresolved warning and the F4b
// stored_count_zero_after_extraction probe.
//
// For resource types that have a domain-specific typed table, the per-item
// generic insert is followed by a dispatch to the matching upsert<Pascal>Tx
// inside the same transaction. Without that dispatch, paginated syncs would
// only populate the generic resources table — typed tables (and indexed
// columns like parent_id added by dependent-resource sync) would stay empty.
func (s *Store) UpsertBatch(resourceType string, items []json.RawMessage) (int, int, error) {
	// parse JSON and extract
	// primary keys before taking writeMu so concurrent sync workers are only
	// serialized for the SQLite transaction, not CPU-heavy unmarshalling.
	prepared := make([]preparedResourceItem, 0, len(items))
	var skippedCount, extractFailures int
	for _, item := range items {
		row, ok, parsed := prepareResourceItem(resourceType, item)
		if !ok {
			skippedCount++
			if parsed {
				extractFailures++
			}
			continue
		}
		prepared = append(prepared, row)
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, extractFailures, fmt.Errorf("starting batch transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var stored int
	for _, item := range prepared {
		if err := s.upsertGenericResourceTx(tx, resourceType, item.id, item.data, item.obj); err != nil {
			return 0, extractFailures, fmt.Errorf("upserting %s/%s: %w", resourceType, item.id, err)
		}
		if err := reconcileItemLifecycleTx(tx, resourceType, item.id, item.obj); err != nil {
			return 0, extractFailures, fmt.Errorf("reconciling %s/%s: %w", resourceType, item.id, err)
		}

		switch resourceType {
		}
		stored++
	}

	// Warn when most items in a batch lack an extractable ID — this likely
	// means the API uses a primary key field we don't recognize yet.
	if skippedCount > 0 && len(items) > 0 && skippedCount*2 > len(items) {
		fmt.Fprintf(os.Stderr, "warning: %d/%d %s items skipped (no extractable ID field found)\n", skippedCount, len(items), resourceType)
	}

	if err := tx.Commit(); err != nil {
		return 0, extractFailures, err
	}
	return stored, extractFailures, nil
}

func (s *Store) SaveSyncState(resourceType, cursor string, count int) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO sync_state (resource_type, last_cursor, last_synced_at, total_count)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(resource_type) DO UPDATE SET last_cursor = excluded.last_cursor,
		 last_synced_at = excluded.last_synced_at, total_count = excluded.total_count`,
		resourceType, cursor, time.Now(), count,
	)
	return err
}

func (s *Store) GetSyncState(resourceType string) (cursor string, lastSynced time.Time, count int, err error) {
	err = s.db.QueryRow(
		`SELECT last_cursor, last_synced_at, total_count FROM sync_state WHERE resource_type = ?`,
		resourceType,
	).Scan(&cursor, &lastSynced, &count)
	if err == sql.ErrNoRows {
		return "", time.Time{}, 0, nil
	}
	return
}

// SaveLibraryVersion records the Zotero Last-Modified-Version checkpoint for a
// resource so the next incremental sync can pass it as the integer `since`
// parameter.
func (s *Store) SaveLibraryVersion(resourceType string, version int) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO sync_state (resource_type, library_version)
		 VALUES (?, ?)
		 ON CONFLICT(resource_type) DO UPDATE SET library_version = excluded.library_version`,
		resourceType, version,
	)
	return err
}

// GetLibraryVersion returns the stored Zotero library version checkpoint for a
// resource, or 0 when none has been recorded.
func (s *Store) GetLibraryVersion(resourceType string) (int, error) {
	var v sql.NullInt64
	err := s.db.QueryRow(
		`SELECT library_version FROM sync_state WHERE resource_type = ?`,
		resourceType,
	).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return int(v.Int64), nil
}

// SaveSyncCursor stores the pagination cursor for a resource type.
func (s *Store) SaveSyncCursor(resourceType, cursor string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO sync_state (resource_type, last_cursor, last_synced_at, total_count)
		 VALUES (?, ?, CURRENT_TIMESTAMP, 0)
		 ON CONFLICT(resource_type) DO UPDATE SET last_cursor = ?, last_synced_at = CURRENT_TIMESTAMP`,
		resourceType, cursor, cursor,
	)
	return err
}

// GetSyncCursor returns the last pagination cursor for a resource type.
func (s *Store) GetSyncCursor(resourceType string) string {
	var cursor sql.NullString
	_ = s.db.QueryRow("SELECT last_cursor FROM sync_state WHERE resource_type = ?", resourceType).Scan(&cursor)
	if cursor.Valid {
		return cursor.String
	}
	return ""
}

// ListIDs returns all IDs for a resource type from the generic resources table.
// Used by dependent sync to iterate parents.
func (s *Store) ListIDs(resourceType string) ([]string, error) {
	if resourceType == "" {
		return nil, fmt.Errorf("resource type is required")
	}
	rows, err := s.db.Query(
		// resourceType is data,
		// not a SQL identifier; bind it instead of interpolating it as a table name.
		"SELECT id FROM resources WHERE resource_type = ? ORDER BY id",
		resourceType,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			continue
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// GetLastSyncedAt returns the last sync timestamp for a resource type.
func (s *Store) GetLastSyncedAt(resourceType string) string {
	var ts sql.NullString
	_ = s.db.QueryRow("SELECT last_synced_at FROM sync_state WHERE resource_type = ?", resourceType).Scan(&ts)
	if ts.Valid {
		return ts.String
	}
	return ""
}

// ClearSyncCursors resets all sync state for a full resync.
func (s *Store) ClearSyncCursors() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.Exec("DELETE FROM sync_state")
	return err
}

// ExecWrite runs a write statement under the store's write serialization lock,
// so auxiliary writers (e.g. creators-audit ORCID evidence) share the same
// in-process serialization as sync's batch writers instead of racing them via
// a raw DB().ExecContext that bypasses writeMu.
func (s *Store) ExecWrite(ctx context.Context, query string, args ...any) (sql.Result, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.db.ExecContext(ctx, query, args...)
}

// Query executes a raw SQL query and returns the rows.
// Used by workflow commands that need custom queries against the local store.
func (s *Store) Query(query string, args ...any) (*sql.Rows, error) {
	return s.queryWithBusyRetry(query, args...)
}

func (s *Store) queryWithBusyRetry(query string, args ...any) (*sql.Rows, error) {
	var rows *sql.Rows
	deadline := time.Now().Add(migrationLockTimeout)
	err := retryOnBusy(context.Background(), deadline, "querying local store", func() error {
		var err error
		rows, err = s.db.Query(query, args...)
		return err
	})
	return rows, err
}

func (s *Store) Count(resourceType string) (int, error) {
	var count int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM resources WHERE resource_type = ?`,
		resourceType,
	).Scan(&count)
	return count, err
}

func (s *Store) Status() (map[string]int, error) {
	rows, err := s.db.Query(
		`SELECT resource_type, COUNT(*) FROM resources GROUP BY resource_type ORDER BY resource_type`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	status := make(map[string]int)
	for rows.Next() {
		var rt string
		var count int
		if err := rows.Scan(&rt, &count); err != nil {
			return nil, err
		}
		status[rt] = count
	}
	return status, rows.Err()
}

// ResolveByName resolves a human-readable name to a UUID from synced data.
// If the input is already a UUID, it is returned as-is.
// matchFields are JSON field names to search against (e.g., "name", "key", "email").
func (s *Store) ResolveByName(resourceType string, input string, matchFields ...string) (string, error) {
	if IsUUID(input) {
		return input, nil
	}

	var matches []string
	for _, field := range matchFields {
		// the json_extract path is built with
		// Sprintf, so a field name containing a quote could break out of the
		// '$.<field>' path and inject SQL. Callers pass literals today; reject
		// anything that isn't a plain dotted identifier as defense in depth.
		if !isSafeJSONFieldName(field) {
			continue
		}
		// #nosec G201 -- field name is pre-validated with isSafeJSONFieldName
		query := fmt.Sprintf(
			`SELECT id FROM resources WHERE resource_type = ? AND LOWER(json_extract(data, '$.%s')) = LOWER(?)`,
			field,
		)
		rows, err := s.db.Query(query, resourceType, input)
		if err != nil {
			continue
		}
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil {
				// Deduplicate
				found := false
				for _, m := range matches {
					if m == id {
						found = true
						break
					}
				}
				if !found {
					matches = append(matches, id)
				}
			}
		}
		rows.Close()
	}

	switch len(matches) {
	case 0:
		return "", fmt.Errorf("%s %q not found in local store. Run 'sync' first, or use the UUID directly", resourceType, input)
	case 1:
		return matches[0], nil
	default:
		var hint string
		if len(matches) > 5 {
			hint = strings.Join(matches[:5], ", ") + "..."
		} else {
			hint = strings.Join(matches, ", ")
		}
		return "", fmt.Errorf("ambiguous: %q matches %d %s entries (%s). Use the exact UUID instead", input, len(matches), resourceType, hint)
	}
}

// isSafeJSONFieldName reports whether s is a plain, optionally dotted
// identifier safe to interpolate into a json_extract path string.
// guards ResolveByName's Sprintf-built query.
func isSafeJSONFieldName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}
