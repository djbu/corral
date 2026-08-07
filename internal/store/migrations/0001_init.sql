PRAGMA foreign_keys = ON;

CREATE TABLE sessions (
  id                  TEXT    PRIMARY KEY,        -- uuidv4; also passed as claude --session-id
  name                TEXT    NOT NULL UNIQUE,    -- human handle used by CLI
  mode                TEXT    NOT NULL,           -- 'interactive' (M1); 'headless' later
  cwd                 TEXT    NOT NULL,
  claude_bin          TEXT    NOT NULL,           -- resolved absolute path actually executed
  model               TEXT,                       -- NULL = claude default
  argv_json           TEXT    NOT NULL,           -- exact argv used, JSON array
  env_keys_json       TEXT    NOT NULL,           -- child env KEY NAMES only, JSON array (never values)
  settings_path       TEXT    NOT NULL,           -- pinned --settings file
  setting_sources     TEXT    NOT NULL,           -- value passed to --setting-sources
  claude_session_id   TEXT,                       -- resume key; == id unless claude forked it
  desired_state       TEXT    NOT NULL,           -- 'running' | 'stopped'   (durable intent)
  status              TEXT    NOT NULL,           -- 'starting'|'running'|'stopping'|'exited'|'failed'
  pid                 INTEGER NOT NULL DEFAULT 0,
  pgid                INTEGER NOT NULL DEFAULT 0,
  proc_start_ns       INTEGER NOT NULL DEFAULT 0, -- guards against PID reuse
  rows                INTEGER NOT NULL,
  cols                INTEGER NOT NULL,
  exit_code           INTEGER,
  exit_signal         TEXT,
  resume_count        INTEGER NOT NULL DEFAULT 0,
  created_at_ms       INTEGER NOT NULL,
  updated_at_ms       INTEGER NOT NULL,
  started_at_ms       INTEGER,
  last_attached_at_ms INTEGER,
  ended_at_ms         INTEGER
) STRICT;

CREATE INDEX sessions_desired_state ON sessions(desired_state, status);

CREATE TABLE events (
  seq        INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT    REFERENCES sessions(id) ON DELETE CASCADE,  -- NULL = daemon-scoped
  ts_ms      INTEGER NOT NULL,
  kind       TEXT    NOT NULL,
  data_json  TEXT    NOT NULL DEFAULT '{}'
) STRICT;

CREATE INDEX events_session_seq ON events(session_id, seq);

CREATE TABLE meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
) STRICT;
