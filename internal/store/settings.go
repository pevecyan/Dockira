package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// GetSetting decodes a setting into v; a missing key leaves v untouched (its defaults).
func (s *Store) GetSetting(ctx context.Context, key string, v any) error {
	var raw string
	err := s.DB.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(raw), v)
}

func (s *Store) SetSetting(ctx context.Context, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx, `INSERT INTO settings (key, value, updated_at) VALUES (?, ?, datetime('now'))
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, key, string(b))
	return err
}
