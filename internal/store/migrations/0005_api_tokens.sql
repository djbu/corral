-- 0005_api_tokens.sql
-- API bearer tokens (design doc m5.md §3.2): one row per minted token.
-- Greenfield table — no existing data to migrate, so this is plain
-- CREATE TABLE, not the 12-step rebuild recipe 0002 needed.
--
-- Schema is verbatim from m5.md §3.2. Sharp edges (documented there,
-- repeated here for anyone reading just this file):
--   - token_hash is the SHA-256 hex digest of the plaintext (internal/
--     apitoken) — the plaintext itself is never persisted, logged, or
--     returned by any read endpoint; it exists only once, at mint time.
--   - scope/session_id exist from this migration even though enforcement
--     doesn't land until step 34, so per-session tokens cost no future
--     migration to enable. Every token minted before step 34 is
--     scope='admin', session_id=NULL.
--   - last_used_ms is observability only; its write is best-effort and
--     must never be on the critical path of auth.
--   - revoked_ms NULL means active; non-NULL means revoked and fails
--     verify. Revocation is a soft delete — rows are never removed.
CREATE TABLE api_tokens (
    id           TEXT PRIMARY KEY,          -- uuid, safe to show/log (not the secret)
    token_hash   TEXT NOT NULL UNIQUE,      -- SHA-256 hex of the plaintext; the only stored form
    label        TEXT NOT NULL,             -- human label ("phone", "laptop"), for `token list`
    scope        TEXT NOT NULL DEFAULT 'admin', -- 'admin' | 'session' (step 34); admin = full
    session_id   TEXT,                      -- NULL for admin tokens; the subtree root for scope='session'
    created_ms   INTEGER NOT NULL,
    last_used_ms INTEGER,                   -- updated best-effort on verify (observability only)
    revoked_ms   INTEGER                    -- NULL = active; non-NULL = revoked, fails verify
) STRICT;
CREATE UNIQUE INDEX api_tokens_hash ON api_tokens(token_hash);
CREATE INDEX api_tokens_session ON api_tokens(session_id);
