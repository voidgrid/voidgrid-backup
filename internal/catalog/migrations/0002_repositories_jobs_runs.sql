CREATE TABLE repositories (
  id             TEXT PRIMARY KEY,
  name           TEXT NOT NULL UNIQUE,
  -- repocfg.Config as JSON, secrets included. The catalog file is 0600.
  config         TEXT NOT NULL,
  password       TEXT NOT NULL,
  created_at     TEXT NOT NULL,
  initialized_at TEXT
) STRICT;

CREATE TABLE jobs (
  id            TEXT PRIMARY KEY,
  name          TEXT NOT NULL UNIQUE,
  agent_id      TEXT NOT NULL REFERENCES agents(id),
  repository_id TEXT NOT NULL REFERENCES repositories(id),
  paths         TEXT NOT NULL,              -- JSON array
  excludes      TEXT NOT NULL DEFAULT '[]', -- JSON array of Kopia ignore rules
  schedule      TEXT NOT NULL DEFAULT '',   -- cron expression; '' = manual only
  keep_latest   INTEGER NOT NULL,
  keep_hourly   INTEGER NOT NULL,
  keep_daily    INTEGER NOT NULL,
  keep_weekly   INTEGER NOT NULL,
  keep_monthly  INTEGER NOT NULL,
  keep_annual   INTEGER NOT NULL,
  enabled       INTEGER NOT NULL DEFAULT 1,
  created_at    TEXT NOT NULL
) STRICT;

CREATE TABLE runs (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  job_id      TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  kind        TEXT NOT NULL,          -- 'backup' | 'restore'
  trigger     TEXT NOT NULL,          -- 'schedule' | 'manual'
  status      TEXT NOT NULL,          -- 'running' | 'success' | 'partial' | 'failed'
  started_at  TEXT NOT NULL,
  finished_at TEXT,
  bytes       INTEGER NOT NULL DEFAULT 0,
  files       INTEGER NOT NULL DEFAULT 0,
  summary     TEXT NOT NULL DEFAULT '',
  error       TEXT NOT NULL DEFAULT ''
) STRICT;

CREATE INDEX runs_job_started ON runs (job_id, started_at DESC);
