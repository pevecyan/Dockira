-- Every saved version of a stack's compose/.env files (edits in the UI, restores, and changes
-- made elsewhere that were noticed), so any version can be compared and restored.
CREATE TABLE file_revisions (
  id         INTEGER PRIMARY KEY,
  stack_id   TEXT NOT NULL,
  file_name  TEXT NOT NULL,
  sha256     TEXT NOT NULL,
  content    TEXT NOT NULL,
  reason     TEXT NOT NULL, -- baseline, edit, restore, external
  ip         TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE INDEX file_revisions_file ON file_revisions (stack_id, file_name, id);
