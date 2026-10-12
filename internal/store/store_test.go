package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestOpenMigratesOnceAndReopens(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	s, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.Version(ctx)
	if err != nil || v < 1 {
		t.Fatalf("version %d, %v", v, err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES ('k', '"v"')`); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// reopening must not re-run migrations (CREATE TABLE would fail) and must keep the data
	s, err = Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var val string
	if err := s.DB.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'k'`).Scan(&val); err != nil || val != `"v"` {
		t.Fatalf("%q %v", val, err)
	}
	var mode string
	s.DB.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode)
	if mode != "wal" {
		t.Errorf("journal_mode %q", mode)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "docker-manager.db")); len(m) != 1 {
		t.Errorf("database file missing in %s", dir)
	}
}

func TestCleanup(t *testing.T) {
	ctx := context.Background()
	s, _ := OpenFile(ctx, ":memory:")
	defer s.Close()
	s.DB.Exec(`INSERT INTO jobs (kind, title, state, created_at) VALUES ('up', 'old', 'succeeded', '2020-01-01T00:00:00Z'), ('up', 'new', 'succeeded', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))`)
	s.DB.Exec(`INSERT INTO audit (ts, action) VALUES ('2020-01-01T00:00:00.000Z', 'old'), (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), 'new')`)
	if err := s.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	var jobs, audit int
	s.DB.QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&jobs)
	s.DB.QueryRow(`SELECT COUNT(*) FROM audit`).Scan(&audit)
	if jobs != 1 || audit != 1 {
		t.Errorf("jobs %d audit %d", jobs, audit)
	}
}
