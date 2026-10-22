-- A person's note that they have seen a failed or partial restore, check or
-- import. The Status page stops counting an acknowledged run.
ALTER TABLE runs ADD COLUMN acked_at TEXT;
ALTER TABLE runs ADD COLUMN acked_by TEXT NOT NULL DEFAULT '';
