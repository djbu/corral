-- M8 keeps execution state and operator review state independent. Existing
-- tasks retain their status verbatim; base_commit is populated only for
-- worktrees created by an M8-aware orchestrator.
ALTER TABLE tasks ADD COLUMN base_commit TEXT;

CREATE TABLE task_reviews (
  task_id        TEXT PRIMARY KEY,
  status         TEXT NOT NULL, -- pending_review|released|discarded
  strategy       TEXT,
  target_ref     TEXT,
  target_before  TEXT,
  result_commit  TEXT,
  recovery_ref   TEXT,
  created_ms     INTEGER NOT NULL,
  updated_ms     INTEGER NOT NULL,
  FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE CASCADE
) STRICT;

CREATE INDEX task_reviews_status ON task_reviews(status, updated_ms);
