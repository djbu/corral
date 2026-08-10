package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// DatabaseInfo is the validation metadata reported for a backup or restore
// source. Integrity is only true when SQLite's full integrity_check returns ok.
type DatabaseInfo struct {
	SchemaVersion int
	Integrity     bool
}

// KnownSessionIDs returns every durable session id. GC treats every returned
// id as referenced regardless of terminal status; only filesystem directories
// absent from this set can be candidates.
func (s *Store) KnownSessionIDs(ctx context.Context) (map[string]struct{}, error) {
	return knownSessionIDs(ctx, s.db)
}

// KnownSessionIDsFromDatabase is the read-only counterpart used by gc
// dry-run, which must not open the normal migrating/WAL store.
func KnownSessionIDsFromDatabase(ctx context.Context, path string) (map[string]struct{}, error) {
	db, err := openReadOnly(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	return knownSessionIDs(ctx, db)
}

func knownSessionIDs(ctx context.Context, db dbtx) (map[string]struct{}, error) {
	rows, err := db.QueryContext(ctx, "SELECT id FROM sessions")
	if err != nil {
		return nil, fmt.Errorf("store: list session ids for gc: %w", err)
	}
	defer rows.Close()
	ids := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan session id for gc: %w", err)
		}
		ids[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list session ids for gc: %w", err)
	}
	return ids, nil
}

// MaintenanceResult reports WAL and freelist maintenance. VACUUM runs only
// when requested and the reclaimable freelist meets minReclaimBytes.
type MaintenanceResult struct {
	ReclaimableBytes int64
	Vacuumed         bool
}

func (s *Store) Maintain(ctx context.Context, vacuum bool, minReclaimBytes int64) (MaintenanceResult, error) {
	if err := s.WalCheckpointTruncate(ctx); err != nil {
		return MaintenanceResult{}, err
	}
	var pageSize, freePages int64
	if err := s.db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		return MaintenanceResult{}, fmt.Errorf("store: reading page_size: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&freePages); err != nil {
		return MaintenanceResult{}, fmt.Errorf("store: reading freelist_count: %w", err)
	}
	result := MaintenanceResult{ReclaimableBytes: pageSize * freePages}
	if vacuum && result.ReclaimableBytes >= minReclaimBytes {
		if _, err := s.db.ExecContext(ctx, "VACUUM"); err != nil {
			return result, fmt.Errorf("store: vacuum: %w", err)
		}
		if err := s.WalCheckpointTruncate(ctx); err != nil {
			return result, err
		}
		result.Vacuumed = true
	}
	return result, nil
}

// ValidateDatabase opens path read-only and verifies both SQLite integrity and
// schema compatibility without applying migrations or creating WAL sidecars.
func ValidateDatabase(ctx context.Context, path string) (DatabaseInfo, error) {
	db, err := openReadOnly(path)
	if err != nil {
		return DatabaseInfo{}, err
	}
	defer db.Close()

	var info DatabaseInfo
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&info.SchemaVersion); err != nil {
		return DatabaseInfo{}, fmt.Errorf("store: reading schema from %s: %w", path, err)
	}
	latest, err := LatestSchemaVersion()
	if err != nil {
		return DatabaseInfo{}, err
	}
	if info.SchemaVersion > latest {
		return DatabaseInfo{}, fmt.Errorf("%w: db user_version=%d, newest known migration=%d", ErrSchemaTooNew, info.SchemaVersion, latest)
	}
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		return DatabaseInfo{}, fmt.Errorf("store: integrity_check %s: %w", path, err)
	}
	info.Integrity = integrity == "ok"
	if !info.Integrity {
		return DatabaseInfo{}, fmt.Errorf("store: integrity_check failed for %s: %s", path, integrity)
	}
	return info, nil
}

// BackupDatabase creates a consistent standalone snapshot of source at dest.
// dest must not exist. SQLite writes into a private temporary file; fsync and
// hard-link publication make a failed/racing backup invisible at dest.
func BackupDatabase(ctx context.Context, source, dest string) (DatabaseInfo, error) {
	if !filepath.IsAbs(source) || !filepath.IsAbs(dest) {
		return DatabaseInfo{}, fmt.Errorf("store: backup paths must be absolute")
	}
	if _, err := os.Lstat(dest); err == nil {
		return DatabaseInfo{}, fmt.Errorf("store: backup destination already exists: %s", dest)
	} else if !os.IsNotExist(err) {
		return DatabaseInfo{}, fmt.Errorf("store: stat backup destination %s: %w", dest, err)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return DatabaseInfo{}, fmt.Errorf("store: create backup directory: %w", err)
	}
	db, err := openReadOnly(source)
	if err != nil {
		return DatabaseInfo{}, err
	}
	defer db.Close()

	tmpFile, err := os.CreateTemp(filepath.Dir(dest), ".corral-backup-*.db")
	if err != nil {
		return DatabaseInfo{}, fmt.Errorf("store: create backup temporary: %w", err)
	}
	tmp := tmpFile.Name()
	if err := tmpFile.Close(); err != nil {
		return DatabaseInfo{}, err
	}
	if err := os.Remove(tmp); err != nil {
		return DatabaseInfo{}, err
	}
	defer os.Remove(tmp)

	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", tmp); err != nil {
		return DatabaseInfo{}, fmt.Errorf("store: snapshot %s: %w", source, err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return DatabaseInfo{}, fmt.Errorf("store: chmod backup: %w", err)
	}
	info, err := ValidateDatabase(ctx, tmp)
	if err != nil {
		return DatabaseInfo{}, err
	}
	if err := syncFile(tmp); err != nil {
		return DatabaseInfo{}, err
	}
	if err := os.Link(tmp, dest); err != nil {
		if errors.Is(err, os.ErrExist) {
			return DatabaseInfo{}, fmt.Errorf("store: backup destination appeared concurrently: %s", dest)
		}
		return DatabaseInfo{}, fmt.Errorf("store: publish backup %s: %w", dest, err)
	}
	if err := syncDir(filepath.Dir(dest)); err != nil {
		return DatabaseInfo{}, err
	}
	return info, nil
}

// RestoreDatabase validates backup, materializes a clean standalone copy next
// to target, and atomically replaces target. The caller must prove the daemon
// is stopped and create a rollback backup before calling. A non-empty WAL
// fails closed; applying it to a different main DB would corrupt it. SHM is
// coordination-only and is safely removed immediately before replacement.
func RestoreDatabase(ctx context.Context, backup, target string) (DatabaseInfo, error) {
	info, err := ValidateDatabase(ctx, backup)
	if err != nil {
		return DatabaseInfo{}, err
	}
	wal := target + "-wal"
	st, err := os.Lstat(wal)
	if err == nil && st.Size() != 0 {
		return DatabaseInfo{}, fmt.Errorf("store: refusing restore with non-empty SQLite WAL: %s", wal)
	}
	if err != nil && !os.IsNotExist(err) {
		return DatabaseInfo{}, fmt.Errorf("store: stat %s: %w", wal, err)
	}

	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return DatabaseInfo{}, err
	}
	tmpFile, err := os.CreateTemp(dir, ".corral-restore-*.db")
	if err != nil {
		return DatabaseInfo{}, fmt.Errorf("store: create restore temporary: %w", err)
	}
	tmp := tmpFile.Name()
	if err := tmpFile.Close(); err != nil {
		return DatabaseInfo{}, err
	}
	if err := os.Remove(tmp); err != nil {
		return DatabaseInfo{}, err
	}
	defer os.Remove(tmp)

	db, err := openReadOnly(backup)
	if err != nil {
		return DatabaseInfo{}, err
	}
	_, vacuumErr := db.ExecContext(ctx, "VACUUM INTO ?", tmp)
	closeErr := db.Close()
	if vacuumErr != nil {
		return DatabaseInfo{}, fmt.Errorf("store: materialize restore: %w", vacuumErr)
	}
	if closeErr != nil {
		return DatabaseInfo{}, fmt.Errorf("store: close restore source: %w", closeErr)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return DatabaseInfo{}, err
	}
	if _, err := ValidateDatabase(ctx, tmp); err != nil {
		return DatabaseInfo{}, err
	}
	if err := syncFile(tmp); err != nil {
		return DatabaseInfo{}, err
	}
	for _, sidecar := range []string{target + "-wal", target + "-shm"} {
		if err := os.Remove(sidecar); err != nil && !os.IsNotExist(err) {
			return DatabaseInfo{}, fmt.Errorf("store: remove stale sidecar %s: %w", sidecar, err)
		}
	}
	if err := os.Rename(tmp, target); err != nil {
		return DatabaseInfo{}, fmt.Errorf("store: replace %s: %w", target, err)
	}
	if err := syncDir(dir); err != nil {
		return DatabaseInfo{}, err
	}
	return info, nil
}

func openReadOnly(path string) (*sql.DB, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("store: stat database %s: %w", path, err)
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("store: database is not a regular file: %s", path)
	}
	u := &url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("store: open database %s read-only: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: open database %s read-only: %w", path, err)
	}
	return db, nil
}

func syncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("store: open %s for fsync: %w", path, err)
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("store: fsync %s: %w", path, err)
	}
	return nil
}

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("store: open directory %s for fsync: %w", path, err)
	}
	defer f.Close()
	if err := f.Sync(); err != nil && !strings.Contains(err.Error(), "invalid argument") {
		return fmt.Errorf("store: fsync directory %s: %w", path, err)
	}
	return nil
}
