-- Settings edited in the UI (key → JSON value). The config file stays the source for
-- everything in config.yaml.
CREATE TABLE settings (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL,
  updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

-- Who did what: every change, login and password confirmation.
CREATE TABLE audit (
  id      INTEGER PRIMARY KEY,
  ts      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
  ip      TEXT NOT NULL DEFAULT '',
  action  TEXT NOT NULL,
  target  TEXT NOT NULL DEFAULT '',
  details TEXT NOT NULL DEFAULT '{}',
  result  TEXT NOT NULL DEFAULT 'ok'
);
CREATE INDEX audit_ts ON audit (ts);

-- Login attempts (for new-IP alerts and the Settings page).
CREATE TABLE auth_events (
  id   INTEGER PRIMARY KEY,
  ts   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
  ip   TEXT NOT NULL,
  ua   TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL, -- login, login_failed, lockout, logout, confirm
  ok   INTEGER NOT NULL
);
CREATE INDEX auth_events_ip ON auth_events (ip, ok);
