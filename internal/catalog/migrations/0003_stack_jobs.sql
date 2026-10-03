ALTER TABLE jobs ADD COLUMN kind TEXT NOT NULL DEFAULT 'paths'; -- 'paths' | 'stack'
ALTER TABLE jobs ADD COLUMN stack TEXT NOT NULL DEFAULT '';     -- JSON StackConfig for kind 'stack'
