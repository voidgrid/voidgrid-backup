CREATE TABLE agents (
  id               TEXT PRIMARY KEY,
  name             TEXT NOT NULL UNIQUE,
  address          TEXT NOT NULL,
  hostname         TEXT NOT NULL DEFAULT '',
  version          TEXT NOT NULL DEFAULT '',
  cert_fingerprint TEXT NOT NULL,
  enrolled_at      TEXT NOT NULL,
  last_seen        TEXT,
  last_error       TEXT NOT NULL DEFAULT ''
) STRICT;
