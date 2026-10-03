-- The server's own record of each job's snapshots, so pages don't have to open
-- the remote repository. Filled in after each backup and reconciled from the
-- repository on demand. Keyed per job: two jobs can share a path.
CREATE TABLE snapshots (
  job_id      TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  snapshot_id TEXT NOT NULL,
  path        TEXT NOT NULL,
  started_at  TEXT NOT NULL,
  ended_at    TEXT NOT NULL,
  bytes       INTEGER NOT NULL,
  files       INTEGER NOT NULL,
  errors      INTEGER NOT NULL DEFAULT 0,
  incomplete  TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (job_id, snapshot_id)
) STRICT;

CREATE INDEX snapshots_job_started ON snapshots (job_id, started_at DESC);
