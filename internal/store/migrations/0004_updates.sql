-- Update checks: what the registry has for each image ref (and base image of built services).
CREATE TABLE image_status (
  ref              TEXT PRIMARY KEY,  -- as written in the compose file / Dockerfile
  local_digest     TEXT NOT NULL DEFAULT '',
  remote_digest    TEXT NOT NULL DEFAULT '',
  local_image_id   TEXT NOT NULL DEFAULT '',
  error            TEXT NOT NULL DEFAULT '',
  error_kind       TEXT NOT NULL DEFAULT '',
  checked_at       TEXT NOT NULL DEFAULT '',
  remote_since     TEXT NOT NULL DEFAULT '', -- when the current remote digest was first seen (cooldown)
  hint_same        TEXT NOT NULL DEFAULT '', -- newer tag in the same series
  hint_major       TEXT NOT NULL DEFAULT '', -- newest newer major
  hints_checked_at TEXT NOT NULL DEFAULT '',
  notified_digest  TEXT NOT NULL DEFAULT '' -- last remote digest an "update available" message went out for
);

-- Images tagged before an update so it can be undone (dm-rollback/<project>-<service>:<time>).
CREATE TABLE rollback_points (
  id         INTEGER PRIMARY KEY,
  stack_id   TEXT NOT NULL,
  service    TEXT NOT NULL,
  ref        TEXT NOT NULL,   -- the image name the service uses
  image_id   TEXT NOT NULL,
  digest     TEXT NOT NULL DEFAULT '',
  tag        TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE INDEX rollback_points_svc ON rollback_points (stack_id, service, id);

-- Every update (manual or automatic) and how it went.
CREATE TABLE update_runs (
  id         INTEGER PRIMARY KEY,
  stack_id   TEXT NOT NULL,
  trigger    TEXT NOT NULL,
  job_id     INTEGER NOT NULL DEFAULT 0,
  services   TEXT NOT NULL DEFAULT '[]',
  changes    TEXT NOT NULL DEFAULT '[]',   -- [{service, ref, from, to}]
  result     TEXT NOT NULL DEFAULT 'running', -- running, updated, unchanged, failed, rolled_back
  message    TEXT NOT NULL DEFAULT '',
  started_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
  ended_at   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX update_runs_stack ON update_runs (stack_id, id);

-- Digests whose automatic update failed: not tried again until a newer one appears.
CREATE TABLE bad_digests (
  ref        TEXT NOT NULL,
  digest     TEXT NOT NULL,
  reason     TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
  PRIMARY KEY (ref, digest)
);

-- Auto-update policy per stack (service = '') with per-service overrides.
CREATE TABLE update_policies (
  stack_id      TEXT NOT NULL,
  service       TEXT NOT NULL DEFAULT '',
  mode          TEXT NOT NULL DEFAULT '',  -- '' (inherit), manual, notify, auto
  auto_rollback INTEGER NOT NULL DEFAULT 1,
  rebuild       INTEGER NOT NULL DEFAULT 0, -- auto: rebuild build: services when their base image changes
  include_db    INTEGER NOT NULL DEFAULT 0, -- auto: also stateful services
  hooks         TEXT NOT NULL DEFAULT '[]',
  PRIMARY KEY (stack_id, service)
);
