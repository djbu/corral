package store

import (
	"database/sql"
	"errors"
	"testing"
	"testing/fstest"

	_ "modernc.org/sqlite"
)

func openMemDB(t *testing.T) *sql.DB {
	t.Helper()
	// A unique memory DB per test (not the bare ":memory:" alias, which
	// database/sql's connection pooling can otherwise reopen as a
	// different empty database on a second connection).
	db, err := sql.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func TestRunMigrations_AppliesInOrder(t *testing.T) {
	db := openMemDB(t)
	fsys := fstest.MapFS{
		"0001_init.sql":  &fstest.MapFile{Data: []byte(`CREATE TABLE a (x INTEGER);`)},
		"0002_add_b.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE b (y INTEGER);`)},
	}

	if err := runMigrations(db, fsys); err != nil {
		t.Fatalf("runMigrations: %v", err)
	}

	v, err := userVersion(db)
	if err != nil {
		t.Fatalf("userVersion: %v", err)
	}
	if v != 2 {
		t.Fatalf("user_version = %d, want 2", v)
	}
	assertTableExists(t, db, "a")
	assertTableExists(t, db, "b")
}

func TestRunMigrations_OnlyAppliesPending(t *testing.T) {
	db := openMemDB(t)
	fsys := fstest.MapFS{
		"0001_init.sql":  &fstest.MapFile{Data: []byte(`CREATE TABLE a (x INTEGER);`)},
		"0002_add_b.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE b (y INTEGER);`)},
	}
	if err := runMigrations(db, fsys); err != nil {
		t.Fatalf("first runMigrations: %v", err)
	}

	// Second call against the same DB and same migration set must be a
	// no-op: no error, no re-execution (which would fail on "table already
	// exists" if it tried).
	if err := runMigrations(db, fsys); err != nil {
		t.Fatalf("second (idempotent) runMigrations: %v", err)
	}
	v, err := userVersion(db)
	if err != nil {
		t.Fatalf("userVersion: %v", err)
	}
	if v != 2 {
		t.Fatalf("user_version after idempotent re-run = %d, want 2", v)
	}
}

func TestRunMigrations_FailureRollsBackAndStopsAtLastGoodVersion(t *testing.T) {
	db := openMemDB(t)
	fsys := fstest.MapFS{
		"0001_init.sql":      &fstest.MapFile{Data: []byte(`CREATE TABLE a (x INTEGER);`)},
		"0002_bad.sql":       &fstest.MapFile{Data: []byte(`CREATE TABLE a (x INTEGER);`)}, // "a" already exists -> fails
		"0003_never_run.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE c (z INTEGER);`)},
	}

	err := runMigrations(db, fsys)
	if err == nil {
		t.Fatal("runMigrations: want error from failing migration 0002, got nil")
	}

	v, verr := userVersion(db)
	if verr != nil {
		t.Fatalf("userVersion: %v", verr)
	}
	if v != 1 {
		t.Fatalf("user_version = %d, want 1 (stopped at last good version)", v)
	}
	assertTableExists(t, db, "a")
	assertTableNotExists(t, db, "c") // 0003 must never have run.
}

func TestRunMigrations_ErrSchemaTooNew(t *testing.T) {
	db := openMemDB(t)
	if _, err := db.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatalf("PRAGMA user_version = 99: %v", err)
	}

	fsys := fstest.MapFS{
		"0001_init.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE a (x INTEGER);`)},
	}
	err := runMigrations(db, fsys)
	if !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("runMigrations: err = %v, want ErrSchemaTooNew", err)
	}
}

func TestListMigrations_SkipsNonMatchingFiles(t *testing.T) {
	fsys := fstest.MapFS{
		"0001_init.sql":    &fstest.MapFile{Data: []byte(`CREATE TABLE a (x INTEGER);`)},
		"0010_ten.sql":     &fstest.MapFile{Data: []byte(`CREATE TABLE j (x INTEGER);`)},
		"0002_two.sql":     &fstest.MapFile{Data: []byte(`CREATE TABLE b (x INTEGER);`)},
		"README.md":        &fstest.MapFile{Data: []byte(`not a migration`)},
		"not_a_number.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE d (x INTEGER);`)},
	}
	files, err := listMigrations(fsys)
	if err != nil {
		t.Fatalf("listMigrations: %v", err)
	}
	if len(files) != 3 {
		t.Fatalf("len(files) = %d, want 3 (README.md and not_a_number.sql skipped): %+v", len(files), files)
	}
	wantOrder := []int{1, 2, 10}
	for i, f := range files {
		if f.version != wantOrder[i] {
			t.Fatalf("files[%d].version = %d, want %d (ascending order): %+v", i, f.version, wantOrder[i], files)
		}
	}
}

func assertTableExists(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", name).Scan(&n); err != nil {
		t.Fatalf("checking table %s exists: %v", name, err)
	}
	if n != 1 {
		t.Fatalf("table %s does not exist", name)
	}
}

func assertTableNotExists(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", name).Scan(&n); err != nil {
		t.Fatalf("checking table %s does not exist: %v", name, err)
	}
	if n != 0 {
		t.Fatalf("table %s exists, want absent (its migration should have been rolled back)", name)
	}
}
