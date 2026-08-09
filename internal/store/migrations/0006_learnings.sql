-- 0006_learnings.sql — M6 verified learning-loop projections.
-- The append-only events table remains the source of truth; these rows make
-- candidate state, provenance, and measurement windows cheap to query.

CREATE TABLE learnings (
  id                 TEXT PRIMARY KEY,
  repo               TEXT NOT NULL,
  kind               TEXT NOT NULL,
  fingerprint        TEXT NOT NULL,
  status             TEXT NOT NULL,
  content_json       TEXT NOT NULL,
  evidence_count     INTEGER NOT NULL,
  baseline_json      TEXT NOT NULL,
  verification_json  TEXT,
  created_ms         INTEGER NOT NULL,
  updated_ms         INTEGER NOT NULL,
  verified_ms        INTEGER,
  proposed_ms        INTEGER,
  adopted_ms         INTEGER,
  rejected_ms        INTEGER,
  expires_ms         INTEGER NOT NULL,
  UNIQUE(repo, kind, fingerprint)
) STRICT;
CREATE INDEX learnings_repo_status ON learnings(repo, status, updated_ms);

CREATE TABLE learning_evidence (
  learning_id TEXT NOT NULL,
  event_seq   INTEGER NOT NULL,
  role        TEXT NOT NULL,
  PRIMARY KEY (learning_id, event_seq, role),
  FOREIGN KEY (learning_id) REFERENCES learnings(id) ON DELETE CASCADE,
  FOREIGN KEY (event_seq) REFERENCES events(seq) ON DELETE RESTRICT
) STRICT;

CREATE TABLE learning_measurements (
  id              TEXT PRIMARY KEY,
  learning_id     TEXT NOT NULL,
  phase           TEXT NOT NULL,
  window_start_ms INTEGER NOT NULL,
  window_end_ms   INTEGER NOT NULL,
  metrics_json    TEXT NOT NULL,
  verdict         TEXT NOT NULL,
  created_ms      INTEGER NOT NULL,
	UNIQUE(learning_id, phase, window_start_ms, window_end_ms),
  FOREIGN KEY (learning_id) REFERENCES learnings(id) ON DELETE CASCADE
) STRICT;
CREATE INDEX learning_measurements_learning ON learning_measurements(learning_id, created_ms);
