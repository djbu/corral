-- 0004_tasks.sql
-- Task DAGs (design doc m4.md §3.2): tasks, their dependency edges, and
-- per-dag cost/budget rollups. Greenfield tables — no existing data to
-- migrate, so this is plain CREATE TABLE, not the 12-step rebuild recipe
-- 0002 needed.
--
-- Schema is verbatim from m4.md §3.2. Sharp edges (documented there,
-- repeated here for anyone reading just this file):
--   - Migration connection opens with foreign_keys(OFF) (migrate.go /
--     store.go precedent); the long-lived conn is foreign_keys(ON), so the
--     FKs below are enforced at runtime, not during migration.
--   - tasks.status is a string enum mirroring the session.Status
--     convention: no CHECK constraint, validated in Go (internal/store's
--     TaskStatus type) matching every other status-bearing table.
--   - cost_usd is stored twice on purpose (tasks.cost_usd AND
--     dag_budgets.cost_usd) and updated together in one transaction by
--     AddCost (§7): denormalized so `corral ls --cost` never re-sums
--     events on every render.

CREATE TABLE tasks (
  id            TEXT PRIMARY KEY,          -- uuid
  dag_id        TEXT NOT NULL,             -- groups a run; a single `corral run` w/o deps is a 1-node dag
  name          TEXT NOT NULL,             -- human label; session names derive as <name>-a<attempt>
  prompt        TEXT NOT NULL,             -- corral OWNS the prompt (drives spawn + re-send, §9)
  repo          TEXT NOT NULL,             -- absolute repo path
  cwd           TEXT NOT NULL,             -- worktree path once created, else repo
  worktree      TEXT,                      -- worktree path if --worktree, else NULL (runs in repo)
  branch        TEXT,                      -- branch-per-task name if worktree, else NULL
  model         TEXT,                      -- per-task model declaration (not routing, §14)
  permission_mode TEXT,                    -- resolved user/env-only; NULL => daemon default
  status        TEXT NOT NULL,             -- pending|ready|running|succeeded|failed|cancelled|blocked
  attempts      INTEGER NOT NULL DEFAULT 0,
  max_attempts  INTEGER NOT NULL DEFAULT 1,
  session_id    TEXT,                      -- current attempt's session (FK sessions.id, ON DELETE SET NULL)
  cost_usd      REAL NOT NULL DEFAULT 0,   -- rolled up across this task's attempts
  budget_usd    REAL,                      -- per-task cap; NULL => unbounded (dag budget still applies)
  created_ms    INTEGER NOT NULL,
  updated_ms    INTEGER NOT NULL,
  FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE SET NULL
) STRICT;
CREATE INDEX tasks_dag ON tasks(dag_id);

CREATE TABLE task_deps (
  task_id   TEXT NOT NULL,                 -- the dependent
  depends_on TEXT NOT NULL,                -- must reach succeeded first
  PRIMARY KEY (task_id, depends_on),
  FOREIGN KEY (task_id)   REFERENCES tasks(id) ON DELETE CASCADE,
  FOREIGN KEY (depends_on) REFERENCES tasks(id) ON DELETE CASCADE
) STRICT;

CREATE TABLE dag_budgets (
  dag_id     TEXT PRIMARY KEY,
  budget_usd REAL,                          -- NULL => unbounded
  cost_usd   REAL NOT NULL DEFAULT 0        -- rolled up across all tasks in the dag
) STRICT;
