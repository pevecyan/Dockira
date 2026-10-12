// Package store is the SQLite database in data_dir: settings edited in the UI, job and update
// history, file revisions, audit log. Schema changes are numbered files in migrations/.
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

type Store struct {
	DB *sql.DB
}

// Open opens (creating if needed) data_dir/docker-manager.db and applies pending migrations.
func Open(ctx context.Context, dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	return OpenFile(ctx, filepath.Join(dataDir, "docker-manager.db"))
}

// OpenFile opens a database file (":memory:" works for tests).
func OpenFile(ctx context.Context, file string) (*Store, error) {
	dsn := "file:" + file + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if file == ":memory:" {
		db.SetMaxOpenConns(1) // every connection would get its own empty database
	}
	s := &Store{DB: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.DB.Close() }

// migrate applies migrations/NNNN_name.sql files newer than the recorded version, each in its
// own transaction.
func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.DB.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL DEFAULT (datetime('now')))`); err != nil {
		return fmt.Errorf("migrations table: %w", err)
	}
	var current int
	if err := s.DB.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return err
	}
	files, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".sql")
		num, _, _ := strings.Cut(name, "_")
		v, err := strconv.Atoi(num)
		if err != nil {
			return fmt.Errorf("migration %s: name must start with a number", f)
		}
		if v <= current {
			continue
		}
		body, err := migrations.ReadFile(f)
		if err != nil {
			return err
		}
		if err := s.apply(ctx, v, name, string(body)); err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
	}
	return nil
}

func (s *Store) apply(ctx context.Context, version int, name, body string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, body); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, name) VALUES (?, ?)`, version, name); err != nil {
		return err
	}
	return tx.Commit()
}

// Version reports the applied schema version.
func (s *Store) Version(ctx context.Context) (int, error) {
	var v int
	err := s.DB.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v)
	return v, err
}

// Cleanup drops old history: jobs after 90 days, audit, login and update records after a year.
func (s *Store) Cleanup(ctx context.Context) error {
	for _, q := range []string{
		`DELETE FROM jobs WHERE created_at < strftime('%Y-%m-%dT%H:%M:%fZ', 'now', '-90 days')`,
		`DELETE FROM audit WHERE ts < strftime('%Y-%m-%dT%H:%M:%fZ', 'now', '-365 days')`,
		`DELETE FROM auth_events WHERE ts < strftime('%Y-%m-%dT%H:%M:%fZ', 'now', '-365 days')`,
		`DELETE FROM update_runs WHERE started_at < strftime('%Y-%m-%dT%H:%M:%fZ', 'now', '-365 days')`,
		`DELETE FROM notify_log WHERE ts < strftime('%Y-%m-%dT%H:%M:%fZ', 'now', '-30 days')`,
	} {
		if _, err := s.DB.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	_, err := s.DB.ExecContext(ctx, `PRAGMA optimize`)
	return err
}
