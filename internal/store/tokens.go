package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/danielbecerra/corral/internal/apitoken"
)

// TokenRow is the persisted view of a row in the api_tokens table (m5.md
// §3.2). It deliberately has no field for token_hash — ListTokens must
// never leak it, and giving the type no place to put it makes that
// impossible to get wrong at a call site, not just a matter of discipline
// in the SELECT. Nullable DB columns map to "" (string) or 0 (int64),
// matching the Task/session.Session convention elsewhere in this package.
type TokenRow struct {
	ID         string
	Label      string
	Scope      string // "admin" | "session"
	SessionID  string // "" when NULL
	CreatedMs  int64
	LastUsedMs int64 // 0 when NULL
	RevokedMs  int64 // 0 when NULL (active)
}

// nullableStrPtr converts a *string to a driver value: nil (and NULL for
// an empty-string pointee) map to SQL NULL, matching nullableStr/
// nullableFloatPtr/nullableIntPtr's convention that "no value" is never
// written as the zero value.
func nullableStrPtr(p *string) any {
	if p == nil || *p == "" {
		return nil
	}
	return *p
}

// CreateToken inserts a new api_tokens row. id and hash are minted by the
// caller (apitoken.Mint plus a uuid, step 27's handler) — unlike
// CreateTask, this never re-fetches and returns the row, because the
// plaintext (the only thing worth showing the operator) is never stored
// and CreateToken has no way to hand it back anyway; the caller already
// has it. created_ms is stamped from the Store's clock; last_used_ms and
// revoked_ms start NULL.
func (s *Store) CreateToken(ctx context.Context, id, hash, label, scope string, sessionID *string) error {
	now := s.clk.Now().UnixMilli()

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO api_tokens (
			id, token_hash, label, scope, session_id,
			created_ms, last_used_ms, revoked_ms
		) VALUES (?, ?, ?, ?, ?, ?, NULL, NULL)
	`,
		id, hash, label, scope, nullableStrPtr(sessionID), now,
	)
	if err != nil {
		return fmt.Errorf("store: creating token %s: %w", id, err)
	}
	return nil
}

// VerifyToken hashes plaintext and looks it up by token_hash, returning
// the row iff a matching token exists and is active (revoked_ms IS
// NULL). No match and a revoked match are both reported as (_, false,
// nil) — not an error — matching m5.md §3.3: an unauthenticated request
// isn't a store failure. The bearer-auth middleware (step 34) layers its
// own constant-time dummy-hash compare on top of this so a 401's timing
// can't distinguish "no such token" from "revoked" from "malformed"; this
// method only does the indexed lookup.
func (s *Store) VerifyToken(ctx context.Context, plaintext string) (TokenRow, bool, error) {
	hash := apitoken.HashForLookup(plaintext)

	var (
		row        TokenRow
		sessionID  sql.NullString
		lastUsedMs sql.NullInt64
		revokedMs  sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT id, label, scope, session_id, created_ms, last_used_ms, revoked_ms
		FROM api_tokens WHERE token_hash = ?
	`, hash).Scan(
		&row.ID, &row.Label, &row.Scope, &sessionID, &row.CreatedMs, &lastUsedMs, &revokedMs,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return TokenRow{}, false, nil
	}
	if err != nil {
		return TokenRow{}, false, fmt.Errorf("store: verifying token: %w", err)
	}
	if revokedMs.Valid {
		return TokenRow{}, false, nil
	}

	row.SessionID = sessionID.String
	row.LastUsedMs = lastUsedMs.Int64
	return row, true, nil
}

// ListTokens returns every token, ordered by created_ms ascending (i.e.
// creation order), for `token list`. It selects token_hash from nowhere —
// see TokenRow's doc comment — shrinking the blast radius of any future
// read-path bug even though token_hash was never the plaintext to begin
// with.
func (s *Store) ListTokens(ctx context.Context) ([]TokenRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, label, scope, session_id, created_ms, last_used_ms, revoked_ms
		FROM api_tokens ORDER BY created_ms ASC, id ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("store: listing tokens: %w", err)
	}
	defer rows.Close()

	var out []TokenRow
	for rows.Next() {
		var (
			row        TokenRow
			sessionID  sql.NullString
			lastUsedMs sql.NullInt64
			revokedMs  sql.NullInt64
		)
		if err := rows.Scan(
			&row.ID, &row.Label, &row.Scope, &sessionID, &row.CreatedMs, &lastUsedMs, &revokedMs,
		); err != nil {
			return nil, fmt.Errorf("store: scanning token: %w", err)
		}
		row.SessionID = sessionID.String
		row.LastUsedMs = lastUsedMs.Int64
		row.RevokedMs = revokedMs.Int64
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing tokens: %w", err)
	}
	return out, nil
}

// RevokeToken sets revoked_ms on id. It is idempotent: revoking an
// already-revoked (or nonexistent) id is a no-op, not an error — a
// caller retrying a revoke (e.g. after a timeout on the first attempt)
// should never see a spurious failure for having already succeeded.
func (s *Store) RevokeToken(ctx context.Context, id string) error {
	now := s.clk.Now().UnixMilli()
	_, err := s.db.ExecContext(ctx, `
		UPDATE api_tokens SET revoked_ms = ? WHERE id = ? AND revoked_ms IS NULL
	`, now, id)
	if err != nil {
		return fmt.Errorf("store: revoking token %s: %w", id, err)
	}
	return nil
}

// TouchToken best-effort updates last_used_ms on id. It has no error
// return by design: m5.md §3.2 requires last_used_ms writes to never be
// on the critical path of auth — a caller (the bearer-auth middleware)
// invokes this fire-and-forget after a request is already authorized, and
// a failed touch must never fail, delay, or retry the request it's
// reporting on. Errors are swallowed here rather than logged because this
// method has no logger to log to; a caller that wants observability on
// touch failures should wrap the call, not this method.
func (s *Store) TouchToken(ctx context.Context, id string) {
	now := s.clk.Now().UnixMilli()
	_, _ = s.db.ExecContext(ctx, `UPDATE api_tokens SET last_used_ms = ? WHERE id = ?`, now, id)
}
