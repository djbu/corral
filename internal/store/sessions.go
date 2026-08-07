package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	sqlite "modernc.org/sqlite"

	"github.com/danielbecerra/corral/internal/session"
)

// ErrNotFound is returned by Get* methods when no row matches.
var ErrNotFound = errors.New("store: not found")

// ErrDuplicateName is returned by CreateSession when a session with the
// same name already exists (sessions.name is UNIQUE).
var ErrDuplicateName = errors.New("store: session name already exists")

// sqliteConstraintUnique is SQLITE_CONSTRAINT_UNIQUE (sqlite3.h), the
// extended result code for a UNIQUE constraint violation. It's a stable
// part of SQLite's C ABI, not something modernc.org/sqlite's public API
// re-exports as a named constant.
const sqliteConstraintUnique = 2067

func isUniqueConstraintError(err error) bool {
	var serr *sqlite.Error
	if errors.As(err, &serr) {
		return serr.Code() == sqliteConstraintUnique
	}
	// Fallback in case the error was wrapped by something that doesn't
	// preserve the concrete type through errors.As.
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// CreateSessionParams is everything CreateSession needs to insert a new
// row. Timestamps are never passed in — CreateSession stamps
// created_at_ms/updated_at_ms itself from the Store's Clock, and leaves
// exit_code/exit_signal/started_at_ms/last_attached_at_ms/ended_at_ms NULL,
// since none of them apply to a session that doesn't exist yet.
type CreateSessionParams struct {
	ID              string
	Name            string
	Mode            session.Mode
	Cwd             string
	ClaudeBin       string
	Model           string // "" persists as NULL
	Argv            []string
	EnvKeys         []string
	SettingsPath    string
	SettingSources  string
	ClaudeSessionID string // "" persists as NULL
	DesiredState    session.DesiredState
	Status          session.Status
	PID             int
	PGID            int
	ProcStartNs     int64
	Rows            int
	Cols            int
}

// CreateSession inserts a new session row and returns the persisted
// record. It returns ErrDuplicateName (wrapped) if p.Name is already in
// use.
func (s *Store) CreateSession(ctx context.Context, p CreateSessionParams) (*session.Session, error) {
	argvJSON, err := marshalStrings(p.Argv)
	if err != nil {
		return nil, fmt.Errorf("store: marshaling argv: %w", err)
	}
	envKeysJSON, err := marshalStrings(p.EnvKeys)
	if err != nil {
		return nil, fmt.Errorf("store: marshaling env keys: %w", err)
	}

	now := s.clk.Now().UnixMilli()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO sessions (
			id, name, mode, cwd, claude_bin, model, argv_json, env_keys_json,
			settings_path, setting_sources, claude_session_id,
			desired_state, status, pid, pgid, proc_start_ns, rows, cols,
			resume_count, created_at_ms, updated_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?)
	`,
		p.ID, p.Name, string(p.Mode), p.Cwd, p.ClaudeBin, nullableStr(p.Model),
		argvJSON, envKeysJSON, p.SettingsPath, p.SettingSources,
		nullableStr(p.ClaudeSessionID), string(p.DesiredState), string(p.Status),
		p.PID, p.PGID, p.ProcStartNs, p.Rows, p.Cols, now, now,
	)
	if err != nil {
		if isUniqueConstraintError(err) {
			return nil, fmt.Errorf("%w: %s", ErrDuplicateName, p.Name)
		}
		return nil, fmt.Errorf("store: creating session %s: %w", p.Name, err)
	}

	return s.GetSession(ctx, p.ID)
}

// GetSession returns the session with the given id, or ErrNotFound.
func (s *Store) GetSession(ctx context.Context, id string) (*session.Session, error) {
	row := s.db.QueryRowContext(ctx, sessionSelectColumns+" FROM sessions WHERE id = ?", id)
	return scanSession(row)
}

// GetSessionByName returns the session with the given name, or
// ErrNotFound. Names are unique, so this always returns at most one row.
func (s *Store) GetSessionByName(ctx context.Context, name string) (*session.Session, error) {
	row := s.db.QueryRowContext(ctx, sessionSelectColumns+" FROM sessions WHERE name = ?", name)
	return scanSession(row)
}

// ListSessions returns every session, ordered by created_at_ms ascending
// (i.e. creation order) for deterministic output.
func (s *Store) ListSessions(ctx context.Context) ([]*session.Session, error) {
	rows, err := s.db.QueryContext(ctx, sessionSelectColumns+" FROM sessions ORDER BY created_at_ms ASC, id ASC")
	if err != nil {
		return nil, fmt.Errorf("store: listing sessions: %w", err)
	}
	defer rows.Close()

	var out []*session.Session
	for rows.Next() {
		sess, err := scanSessionRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing sessions: %w", err)
	}
	return out, nil
}

// UpdateSession loads the session with id, applies mutate to it, and
// writes every mutable column back — all inside one transaction, so the
// read-modify-write is atomic even though SetMaxOpenConns(1) means there's
// only one underlying connection to contend for. updated_at_ms is always
// bumped to the Store's current clock time, whether or not mutate touches
// anything else. mutate must not change ID or CreatedAtMs; if it does,
// those changes are discarded (id is the WHERE key and CreatedAtMs is
// immutable by design).
func (s *Store) UpdateSession(ctx context.Context, id string, mutate func(*session.Session)) (*session.Session, error) {
	var updated *session.Session
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, sessionSelectColumns+" FROM sessions WHERE id = ?", id)
		sess, err := scanSession(row)
		if err != nil {
			return err
		}

		originalID, originalCreated := sess.ID, sess.CreatedAtMs
		mutate(sess)
		sess.ID = originalID
		sess.CreatedAtMs = originalCreated
		sess.UpdatedAtMs = s.clk.Now().UnixMilli()

		argvJSON, err := marshalStrings(sess.Argv)
		if err != nil {
			return fmt.Errorf("store: marshaling argv: %w", err)
		}
		envKeysJSON, err := marshalStrings(sess.EnvKeys)
		if err != nil {
			return fmt.Errorf("store: marshaling env keys: %w", err)
		}

		_, err = tx.ExecContext(ctx, `
			UPDATE sessions SET
				name = ?, mode = ?, cwd = ?, claude_bin = ?, model = ?,
				argv_json = ?, env_keys_json = ?, settings_path = ?,
				setting_sources = ?, claude_session_id = ?, desired_state = ?,
				status = ?, pid = ?, pgid = ?, proc_start_ns = ?, rows = ?,
				cols = ?, exit_code = ?, exit_signal = ?, resume_count = ?,
				updated_at_ms = ?, started_at_ms = ?, last_attached_at_ms = ?,
				ended_at_ms = ?
			WHERE id = ?
		`,
			sess.Name, string(sess.Mode), sess.Cwd, sess.ClaudeBin, nullableStr(sess.Model),
			argvJSON, envKeysJSON, sess.SettingsPath, sess.SettingSources,
			nullableStr(sess.ClaudeSessionID), string(sess.DesiredState), string(sess.Status),
			sess.PID, sess.PGID, sess.ProcStartNs, sess.Rows, sess.Cols,
			nullableIntPtr(sess.ExitCode), nullableStr(sess.ExitSignal), sess.ResumeCount,
			sess.UpdatedAtMs, sess.StartedAtMs, sess.LastAttachedAtMs, sess.EndedAtMs,
			sess.ID,
		)
		if err != nil {
			if isUniqueConstraintError(err) {
				return fmt.Errorf("%w: %s", ErrDuplicateName, sess.Name)
			}
			return fmt.Errorf("store: updating session %s: %w", sess.ID, err)
		}
		updated = sess
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

const sessionSelectColumns = `SELECT
	id, name, mode, cwd, claude_bin, model, argv_json, env_keys_json,
	settings_path, setting_sources, claude_session_id, desired_state,
	status, pid, pgid, proc_start_ns, rows, cols, exit_code, exit_signal,
	resume_count, created_at_ms, updated_at_ms, started_at_ms,
	last_attached_at_ms, ended_at_ms`

// rowScanner is implemented by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanSession(row rowScanner) (*session.Session, error) {
	return scanSessionRow(row)
}

func scanSessionRow(row rowScanner) (*session.Session, error) {
	var (
		sess                                     session.Session
		mode, desiredState, status               string
		model, claudeSessionID, exitSignal       sql.NullString
		argvJSON, envKeysJSON                    string
		exitCode                                 sql.NullInt64
		startedAtMs, lastAttachedAtMs, endedAtMs sql.NullInt64
	)

	err := row.Scan(
		&sess.ID, &sess.Name, &mode, &sess.Cwd, &sess.ClaudeBin, &model,
		&argvJSON, &envKeysJSON, &sess.SettingsPath, &sess.SettingSources,
		&claudeSessionID, &desiredState, &status, &sess.PID, &sess.PGID,
		&sess.ProcStartNs, &sess.Rows, &sess.Cols, &exitCode, &exitSignal,
		&sess.ResumeCount, &sess.CreatedAtMs, &sess.UpdatedAtMs,
		&startedAtMs, &lastAttachedAtMs, &endedAtMs,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: scanning session: %w", err)
	}

	sess.Mode = session.Mode(mode)
	sess.DesiredState = session.DesiredState(desiredState)
	sess.Status = session.Status(status)
	sess.Model = model.String
	sess.ClaudeSessionID = claudeSessionID.String
	sess.ExitSignal = exitSignal.String
	if exitCode.Valid {
		v := int(exitCode.Int64)
		sess.ExitCode = &v
	}
	if startedAtMs.Valid {
		v := startedAtMs.Int64
		sess.StartedAtMs = &v
	}
	if lastAttachedAtMs.Valid {
		v := lastAttachedAtMs.Int64
		sess.LastAttachedAtMs = &v
	}
	if endedAtMs.Valid {
		v := endedAtMs.Int64
		sess.EndedAtMs = &v
	}

	if err := json.Unmarshal([]byte(argvJSON), &sess.Argv); err != nil {
		return nil, fmt.Errorf("store: unmarshaling argv_json for session %s: %w", sess.ID, err)
	}
	if err := json.Unmarshal([]byte(envKeysJSON), &sess.EnvKeys); err != nil {
		return nil, fmt.Errorf("store: unmarshaling env_keys_json for session %s: %w", sess.ID, err)
	}

	return &sess, nil
}

func marshalStrings(v []string) (string, error) {
	if v == nil {
		v = []string{}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func nullableStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableIntPtr(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}
