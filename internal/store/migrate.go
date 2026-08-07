package store

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
)

// ErrSchemaTooNew is returned when the database's PRAGMA user_version is
// higher than the newest migration file this binary knows about — i.e. the
// DB was written by a newer corral. Downgrades are unsupported (§6.1).
var ErrSchemaTooNew = errors.New("store: database schema is newer than this binary understands")

var migrationFileRE = regexp.MustCompile(`^(\d{4})_[^/]+\.sql$`)

// migrationFile is one parsed "NNNN_slug.sql" migration file.
type migrationFile struct {
	version int
	name    string
}

// listMigrations reads every "NNNN_slug.sql" file at the root of fsys,
// sorted by version ascending. Names that don't match the convention are
// skipped rather than erroring, so a stray non-migration file doesn't break
// the runner.
func listMigrations(fsys fs.FS) ([]migrationFile, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("store: reading migrations dir: %w", err)
	}

	var files []migrationFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := migrationFileRE.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		v, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, fmt.Errorf("store: migration file %s: %w", e.Name(), err)
		}
		files = append(files, migrationFile{version: v, name: e.Name()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].version < files[j].version })
	return files, nil
}

// runMigrations applies every migration in fsys with version > the
// database's current user_version, one transaction per file: exec the
// file's SQL, then set PRAGMA user_version to that file's version. Any
// error rolls back that file's transaction and returns; the DB is left at
// the last good version (§6.1).
func runMigrations(db *sql.DB, fsys fs.FS) error {
	files, err := listMigrations(fsys)
	if err != nil {
		return err
	}

	current, err := userVersion(db)
	if err != nil {
		return err
	}

	var highest int
	for _, f := range files {
		if f.version > highest {
			highest = f.version
		}
	}
	if current > highest {
		return fmt.Errorf("%w: db user_version=%d, newest known migration=%d", ErrSchemaTooNew, current, highest)
	}

	for _, f := range files {
		if f.version <= current {
			continue
		}
		sqlText, err := fs.ReadFile(fsys, f.name)
		if err != nil {
			return fmt.Errorf("store: reading migration %s: %w", f.name, err)
		}
		if err := applyMigration(db, f, sqlText); err != nil {
			return err
		}
	}
	return nil
}

func userVersion(db *sql.DB) (int, error) {
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return 0, fmt.Errorf("store: reading user_version: %w", err)
	}
	return v, nil
}

// applyMigration execs sqlText and bumps user_version in one transaction.
// PRAGMA user_version cannot take a bound parameter, so the version is
// formatted directly into the statement; it comes from the migration
// filename, never from user input.
func applyMigration(db *sql.DB, f migrationFile, sqlText []byte) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin tx for migration %s: %w", f.name, err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(string(sqlText)); err != nil {
		return fmt.Errorf("store: applying migration %s: %w", f.name, err)
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", f.version)); err != nil {
		return fmt.Errorf("store: setting user_version=%d for migration %s: %w", f.version, f.name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: committing migration %s: %w", f.name, err)
	}
	return nil
}
