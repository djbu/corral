// Package store is corral's only SQLite access point (design doc §6).
// Everything else — supervisor, api, cmd/ — goes through the typed methods
// here; no other package imports database/sql for corral's own state. The
// events table is append-only by construction: this package's events.go
// exports only AppendEvent and ListEvents, never an update or delete.
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"os"

	_ "modernc.org/sqlite"

	"github.com/danielbecerra/corral/internal/clock"
)

//go:embed migrations/*.sql
var embeddedMigrations embed.FS

// migrationsFS returns the embedded migration files rooted at
// "migrations/", i.e. with names like "0001_init.sql" rather than
// "migrations/0001_init.sql".
func migrationsFS() fs.FS {
	sub, err := fs.Sub(embeddedMigrations, "migrations")
	if err != nil {
		// Only reachable if the go:embed directive above and this path
		// disagree, which would be a build-time bug, not a runtime one.
		panic(fmt.Sprintf("store: embedded migrations: %v", err))
	}
	return sub
}

// Store is a single-connection handle to corral's SQLite database. All
// *_at_ms timestamps it writes come from the injected Clock — corral never
// calls time.Now() directly for a persisted value.
type Store struct {
	db  *sql.DB
	clk clock.Clock
	// pub is step 32's SSE fan-out seam (see events.go's EventPublisher). Set
	// once at daemon startup via SetEventPublisher, before any goroutine can
	// call AppendEvent, so a plain field needs no lock/atomic to be safe for
	// concurrent readers thereafter.
	pub EventPublisher
}

// dbtx is the subset of *sql.DB / *sql.Tx that CRUD methods need. Every
// method in sessions.go/events.go is written against this interface
// (unexported) rather than against *Store directly, so the exact same code
// path works standalone (against s.db) or composed inside a transaction
// (against a *sql.Tx). This matters because SetMaxOpenConns(1) means the DB
// has exactly one connection: a method that reached for s.db while already
// inside a transaction on that connection would block forever waiting for
// a connection nothing will ever release.
type dbtx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Open opens (creating if necessary) the SQLite database at path, applies
// any pending migrations, and tightens file permissions on the DB and its
// -wal/-shm siblings to 0600 (§6.3, §10.3). clk stamps every *_at_ms column
// this Store writes.
//
// Migrations run on a separate, short-lived connection opened with
// _pragma=foreign_keys(OFF) before the long-lived connection (foreign_keys
// ON) is opened for normal operation (§6.2). This matters because SQLite's
// 12-step table-rebuild recipe — used by migration 0002 and any future
// migration that needs to change a column set SQLite can't ALTER in place —
// renames the table being rebuilt out of the way and then DROPs it; with
// foreign_keys ON, DROP TABLE on a table other tables reference performs an
// implicit DELETE FROM first, and ON DELETE CASCADE on events.session_id
// would fire for every row, silently wiping the events table. Running
// migrations with foreign_keys OFF avoids that entirely, independent of
// what any individual migration file does or forgets to do — PRAGMA
// foreign_keys is a no-op once a transaction has started, so a migration
// file cannot toggle it itself.
func Open(path string, clk clock.Clock) (*Store, error) {
	migDSN := fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(OFF)",
		path,
	)
	migDB, err := sql.Open("sqlite", migDSN)
	if err != nil {
		return nil, fmt.Errorf("store: opening %s for migration: %w", path, err)
	}
	migDB.SetMaxOpenConns(1)

	if err := runMigrations(migDB, migrationsFS()); err != nil {
		migDB.Close()
		return nil, err
	}
	if err := migDB.Close(); err != nil {
		return nil, fmt.Errorf("store: closing migration connection for %s: %w", path, err)
	}

	dsn := fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)",
		path,
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: opening %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)

	if err := chmodDBFiles(path); err != nil {
		db.Close()
		return nil, err
	}

	return &Store{db: db, clk: clk}, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// SchemaVersion returns the database's current PRAGMA user_version, for
// GET /v1/version's schema_version field.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return 0, fmt.Errorf("store: reading user_version: %w", err)
	}
	return v, nil
}

// WalCheckpointTruncate runs PRAGMA wal_checkpoint(TRUNCATE), folding the
// WAL back into the main database file and truncating it to zero bytes.
// Shutdown (design doc §3.6 step 5) calls this immediately before Close so
// a daemon restart never has to replay a WAL left over from a clean exit.
func (s *Store) WalCheckpointTruncate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return fmt.Errorf("store: wal_checkpoint(TRUNCATE): %w", err)
	}
	return nil
}

// withTx runs fn inside a transaction, committing if fn returns nil and
// rolling back otherwise (including on panic, via the deferred Rollback —
// harmless no-op after a successful Commit).
func (s *Store) withTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin tx: %w", err)
	}
	defer tx.Rollback()

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit tx: %w", err)
	}
	return nil
}

// chmodDBFiles tightens path and its -wal/-shm siblings to 0600. The -wal
// and -shm files only exist once WAL mode has actually been engaged by a
// write (which migrations guarantee happens before this is called), but
// tolerate ENOENT defensively in case a future caller reorders this.
func chmodDBFiles(path string) error {
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(p, 0o600); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("store: chmod %s: %w", p, err)
		}
	}
	return nil
}
