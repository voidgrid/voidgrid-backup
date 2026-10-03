-- Agents that contacted the server's registration listener with a valid
-- token and are waiting for (or have received) an operator's decision.
CREATE TABLE registrations (
  id          TEXT PRIMARY KEY,
  fingerprint TEXT NOT NULL UNIQUE,
  agent_cert  BLOB NOT NULL,
  hostname    TEXT NOT NULL DEFAULT '',
  address     TEXT NOT NULL,
  version     TEXT NOT NULL DEFAULT '',
  created_at  TEXT NOT NULL,
  last_poll   TEXT,
  status      TEXT NOT NULL DEFAULT 'pending',
  agent_id    TEXT NOT NULL DEFAULT '',
  issued_cert BLOB
) STRICT;
