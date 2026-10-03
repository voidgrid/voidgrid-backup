ALTER TABLE agents ADD COLUMN warnings TEXT NOT NULL DEFAULT ''; -- newline-separated, from the last successful check
