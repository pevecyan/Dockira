-- Notification channels (config JSON encrypted with secret_key), rules routing events to them,
-- and a log of deliveries.
CREATE TABLE notification_channels (
  id         INTEGER PRIMARY KEY,
  type       TEXT NOT NULL,
  name       TEXT NOT NULL,
  enabled    INTEGER NOT NULL DEFAULT 1,
  config_enc TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE notification_rules (
  id         INTEGER PRIMARY KEY,
  channel_id INTEGER NOT NULL REFERENCES notification_channels (id) ON DELETE CASCADE,
  rule       TEXT NOT NULL -- notify.Rule as JSON
);

CREATE TABLE notify_log (
  id         INTEGER PRIMARY KEY,
  ts         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
  channel_id INTEGER NOT NULL,
  event_type TEXT NOT NULL,
  title      TEXT NOT NULL DEFAULT '',
  stack_id   TEXT NOT NULL DEFAULT '',
  attempt    INTEGER NOT NULL DEFAULT 1,
  ok         INTEGER NOT NULL,
  error      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX notify_log_ts ON notify_log (id);
