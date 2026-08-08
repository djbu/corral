-- 0003_activity.sql
-- last_activity_ms: unix-ms of the last HUMAN/EXTERNAL input to this session
-- (PTY WriteInput, client attach, inbound hook event). It is deliberately
-- NOT bumped by agent PTY output: a session streaming output with no human
-- present is exactly what a future idle reaper must be able to reap. Seeded
-- for existing rows to started_at_ms (fallback created_at_ms).
ALTER TABLE sessions ADD COLUMN last_activity_ms INTEGER NOT NULL DEFAULT 0;
UPDATE sessions SET last_activity_ms = COALESCE(started_at_ms, created_at_ms, 0);
