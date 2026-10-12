-- Jobs: every compose run and update, with its output (capped) for the Jobs page.
CREATE TABLE jobs (
  id         INTEGER PRIMARY KEY,
  stack_id   TEXT NOT NULL DEFAULT '',
  kind       TEXT NOT NULL,
  title      TEXT NOT NULL,
  services   TEXT NOT NULL DEFAULT '[]',
  trigger    TEXT NOT NULL DEFAULT 'user', -- user, auto, schedule
  state      TEXT NOT NULL,                -- queued, running, succeeded, failed, cancelled
  exit_code  INTEGER NOT NULL DEFAULT 0,
  error      TEXT NOT NULL DEFAULT '',
  output     TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  started_at TEXT NOT NULL DEFAULT '',
  ended_at   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX jobs_stack ON jobs (stack_id, id);
