package store

import (
	"context"
	"database/sql"
	"fmt"
)

// ClaimIncomingMessage durably reserves an authenticated inbound provider
// message. It returns true exactly once for a (backend, messageIDHash) pair;
// later calls return false until the row expires. Callers must use a one-way
// hash, never a raw provider message identifier.
//
// Expired rows are pruned in the same transaction before the insert. This
// keeps the table bounded without a background worker and makes expiry
// deterministic from the supplied timestamps.
func (s *Store) ClaimIncomingMessage(ctx context.Context, backend, messageIDHash string, receivedAtMs, expiresAtMs int64) (bool, error) {
	if backend == "" || messageIDHash == "" {
		return false, fmt.Errorf("store: incoming message backend and hash are required")
	}
	if expiresAtMs <= receivedAtMs {
		return false, fmt.Errorf("store: incoming message expiry must be after receipt")
	}

	claimed := false
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM incoming_messages WHERE expires_at_ms <= ?", receivedAtMs); err != nil {
			return fmt.Errorf("store: pruning incoming messages: %w", err)
		}
		result, err := tx.ExecContext(ctx, `
INSERT INTO incoming_messages (backend, message_id_hash, received_at_ms, expires_at_ms)
VALUES (?, ?, ?, ?)
ON CONFLICT(backend, message_id_hash) DO NOTHING`, backend, messageIDHash, receivedAtMs, expiresAtMs)
		if err != nil {
			return fmt.Errorf("store: claiming incoming message: %w", err)
		}
		n, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: checking incoming-message claim: %w", err)
		}
		claimed = n == 1
		return nil
	})
	return claimed, err
}
