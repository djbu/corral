-- M9 records the normalized version of the operator-selected Claude binary
-- observed when a session is admitted. Existing historical rows stay unknown.
ALTER TABLE sessions ADD COLUMN claude_version TEXT;
