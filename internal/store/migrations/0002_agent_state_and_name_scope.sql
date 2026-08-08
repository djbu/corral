-- 0002: agent-state columns + name uniqueness scoped to active sessions
-- (design doc §6.1, §6.2, Amendment A.8).
--
-- sessions.name loses its inline UNIQUE and gains a partial unique index
-- that only covers "active" sessions — a session is terminal when
-- desired_state='stopped' AND status IN ('exited','failed'); a graceful
-- shutdown in flight (desired_state='running', status='exited') still
-- reserves its name, since the supervisor may still be converging it.
--
-- SQLite cannot drop an inline UNIQUE constraint or add a NOT NULL column
-- with a computed default in place, so this follows the standard 12-step
-- table-rebuild recipe (disable FK enforcement, rebuild into a new table,
-- drop the old one, rename, rebuild indexes, re-enable FK enforcement).
-- Foreign-key enforcement for this migration is controlled by the caller
-- opening this migration on a dedicated connection with
-- _pragma=foreign_keys(OFF) (store.go, §6.2) — setting the PRAGMA here
-- would be a silent no-op, since PRAGMA foreign_keys cannot change state
-- inside an already-open transaction, and the migration runner always
-- wraps this file's contents in one.

CREATE TABLE sessions_new (
  id                    TEXT    PRIMARY KEY,
  name                  TEXT    NOT NULL,
  mode                  TEXT    NOT NULL,
  cwd                   TEXT    NOT NULL,
  claude_bin            TEXT    NOT NULL,
  model                 TEXT,
  argv_json             TEXT    NOT NULL,
  env_keys_json         TEXT    NOT NULL,
  settings_path         TEXT    NOT NULL,
  setting_sources       TEXT    NOT NULL,
  claude_session_id     TEXT,
  desired_state         TEXT    NOT NULL,
  status                TEXT    NOT NULL,
  pid                   INTEGER NOT NULL DEFAULT 0,
  pgid                  INTEGER NOT NULL DEFAULT 0,
  proc_start_ns         INTEGER NOT NULL DEFAULT 0,
  rows                  INTEGER NOT NULL,
  cols                  INTEGER NOT NULL,
  exit_code             INTEGER,
  exit_signal           TEXT,
  resume_count          INTEGER NOT NULL DEFAULT 0,
  created_at_ms         INTEGER NOT NULL,
  updated_at_ms         INTEGER NOT NULL,
  started_at_ms         INTEGER,
  last_attached_at_ms   INTEGER,
  ended_at_ms           INTEGER,
  -- New in 0002 (design doc §6.1, Amendment A.8):
  agent_state           TEXT    NOT NULL DEFAULT 'starting', -- rendered by `corral ls`
  agent_state_since_ms  INTEGER,                             -- NULL until first hook/lifecycle event sets it
  blocked_reason_json   TEXT,                                -- NULL unless agent_state='blocked'; shape owned by internal/state (later step)
  last_hook_at_ms       INTEGER,                              -- NULL until the first hook-relay delivery for this session
  hook_count            INTEGER NOT NULL DEFAULT 0,
  permission_mode       TEXT,                                -- NULL = unset; user/env-settable only, never repo-settable
  last_prompt_id        TEXT                                 -- NULL until the first UserPromptSubmit hook
) STRICT;

INSERT INTO sessions_new (
  id, name, mode, cwd, claude_bin, model, argv_json, env_keys_json,
  settings_path, setting_sources, claude_session_id, desired_state, status,
  pid, pgid, proc_start_ns, rows, cols, exit_code, exit_signal,
  resume_count, created_at_ms, updated_at_ms, started_at_ms,
  last_attached_at_ms, ended_at_ms,
  agent_state, agent_state_since_ms, blocked_reason_json, last_hook_at_ms,
  hook_count, permission_mode, last_prompt_id
)
SELECT
  id, name, mode, cwd, claude_bin, model, argv_json, env_keys_json,
  settings_path, setting_sources, claude_session_id, desired_state, status,
  pid, pgid, proc_start_ns, rows, cols, exit_code, exit_signal,
  resume_count, created_at_ms, updated_at_ms, started_at_ms,
  last_attached_at_ms, ended_at_ms,
  -- Backfill agent_state from the existing Status, mirroring
  -- internal/state's NoopEngine.statusToAgentState mapping exactly, so a
  -- freshly migrated M1 database renders the same `corral ls` state it did
  -- before the upgrade.
  CASE status
    WHEN 'starting'          THEN 'starting'
    WHEN 'running'            THEN 'running'
    WHEN 'stopping'           THEN 'running'
    WHEN 'exited'             THEN 'exited'
    WHEN 'failed'             THEN 'failed'
    ELSE                           'unknown'
  END,
  NULL, NULL, NULL, 0, NULL, NULL
FROM sessions;

DROP TABLE sessions;

ALTER TABLE sessions_new RENAME TO sessions;

CREATE INDEX sessions_desired_state ON sessions(desired_state, status);

CREATE INDEX sessions_agent_state ON sessions(agent_state);

-- Replaces the old inline UNIQUE(name): unique only among sessions that
-- are not fully terminal. desired_state='stopped' AND status IN
-- ('exited','failed') is the only terminal combination — a session whose
-- supervisor is still gracefully shutting it down (desired_state='running',
-- status='exited') keeps its name reserved, since the daemon may still act
-- on it before flipping desired_state to 'stopped'.
CREATE UNIQUE INDEX sessions_name_active ON sessions(name)
  WHERE NOT (desired_state = 'stopped' AND status IN ('exited', 'failed'));
