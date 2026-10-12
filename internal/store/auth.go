package store

import (
	"context"
	"time"
)

type AuthEvent struct {
	Time time.Time `json:"time"`
	IP   string    `json:"ip"`
	UA   string    `json:"ua"`
	Kind string    `json:"kind"`
	OK   bool      `json:"ok"`
}

// Record stores a login event (auth.Recorder). Errors are not the caller's problem: a full
// disk must not block logins.
func (s *Store) Record(ctx context.Context, kind, ip, ua string, ok bool) {
	if len(ua) > 300 {
		ua = ua[:300]
	}
	_, _ = s.DB.ExecContext(ctx, `INSERT INTO auth_events (ip, ua, kind, ok) VALUES (?, ?, ?, ?)`, ip, ua, kind, ok)
}

// KnownIP reports a previous successful login from ip.
func (s *Store) KnownIP(ctx context.Context, ip string) bool {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_events WHERE ip = ? AND kind = 'login' AND ok = 1`, ip).Scan(&n)
	return err == nil && n > 0
}

// AuthEvents returns the latest login events, newest first.
func (s *Store) AuthEvents(ctx context.Context, limit int) ([]AuthEvent, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT ts, ip, ua, kind, ok FROM auth_events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuthEvent{}
	for rows.Next() {
		var e AuthEvent
		var ts string
		if err := rows.Scan(&ts, &e.IP, &e.UA, &e.Kind, &e.OK); err != nil {
			return nil, err
		}
		e.Time = ParseTime(ts)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ParseTime reads the timestamps SQLite writes (strftime with milliseconds, or datetime()).
func ParseTime(s string) time.Time {
	for _, layout := range []string{"2006-01-02T15:04:05.999Z", time.RFC3339Nano, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// Audit appends to the audit log.
func (s *Store) Audit(ctx context.Context, ip, action, target, details, result string) {
	if details == "" {
		details = "{}"
	}
	_, _ = s.DB.ExecContext(ctx, `INSERT INTO audit (ip, action, target, details, result) VALUES (?, ?, ?, ?, ?)`, ip, action, target, details, result)
}

type AuditEntry struct {
	ID      int64     `json:"id"`
	Time    time.Time `json:"time"`
	IP      string    `json:"ip"`
	Action  string    `json:"action"`
	Target  string    `json:"target"`
	Details string    `json:"details"`
	Result  string    `json:"result"`
}

// AuditLog returns entries older than before (0 = newest), newest first.
func (s *Store) AuditLog(ctx context.Context, before int64, limit int) ([]AuditEntry, error) {
	q := `SELECT id, ts, ip, action, target, details, result FROM audit`
	args := []any{}
	if before > 0 {
		q += ` WHERE id < ?`
		args = append(args, before)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		var ts string
		if err := rows.Scan(&e.ID, &ts, &e.IP, &e.Action, &e.Target, &e.Details, &e.Result); err != nil {
			return nil, err
		}
		e.Time = ParseTime(ts)
		out = append(out, e)
	}
	return out, rows.Err()
}
