// Package alerts stores notification channels and rules, sends events through them (package
// notify does the sending), and turns what happens on the host into events: unhealthy or dying
// containers, restart loops, failed jobs, a filling disk.
package alerts

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"docker-manager/internal/docker"
	"docker-manager/internal/hoststats"
	"docker-manager/internal/jobs"
	"docker-manager/internal/notify"
	"docker-manager/internal/secret"
	"docker-manager/internal/store"
)

type Manager struct {
	db        *sql.DB
	box       *secret.Box
	log       *slog.Logger
	publicURL string
	d         *notify.Dispatcher

	mu       sync.Mutex
	expected map[string]time.Time   // stack → until: changes come from the user, not a crash
	starts   map[string][]time.Time // container → recent starts (restart loops)
	diskAt   time.Time
	looped   map[string]time.Time
}

func New(db *sql.DB, box *secret.Box, log *slog.Logger, publicURL string) *Manager {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	m := &Manager{db: db, box: box, log: log, publicURL: strings.TrimRight(publicURL, "/"),
		expected: map[string]time.Time{}, starts: map[string][]time.Time{}, looped: map[string]time.Time{}}
	m.d = notify.NewDispatcher(m, notify.DispatcherOptions{OnDelivery: m.logDelivery, Logger: log})
	return m
}

// Send dispatches an event (asynchronously). Relative URLs become links into the UI.
func (m *Manager) Send(e notify.Event) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	if strings.HasPrefix(e.URL, "/") {
		if m.publicURL == "" {
			e.URL = ""
		} else {
			e.URL = m.publicURL + e.URL
		}
	}
	m.d.Dispatch(e)
}

func (m *Manager) Wait() { m.d.Wait() }

// --- storage -------------------------------------------------------------------------------

func (m *Manager) seal(cfg json.RawMessage) (string, error) {
	b, err := m.box.Seal(cfg)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

func (m *Manager) open(s string) (json.RawMessage, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	return m.box.Open(b)
}

// Channels returns all channels with decrypted configs (notify.Source).
func (m *Manager) Channels(ctx context.Context) ([]notify.Channel, error) {
	rows, err := m.db.QueryContext(ctx, `SELECT id, type, name, enabled, config_enc FROM notification_channels ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []notify.Channel
	for rows.Next() {
		var ch notify.Channel
		var enc string
		if err := rows.Scan(&ch.ID, &ch.Type, &ch.Name, &ch.Enabled, &enc); err != nil {
			return nil, err
		}
		if ch.Config, err = m.open(enc); err != nil {
			m.log.Warn("notification channel config unreadable (secret_key changed?)", "channel", ch.Name)
			ch.Enabled, ch.Config = false, json.RawMessage(`{}`)
		}
		out = append(out, ch)
	}
	return out, rows.Err()
}

func (m *Manager) Channel(ctx context.Context, id int64) (notify.Channel, error) {
	all, err := m.Channels(ctx)
	if err != nil {
		return notify.Channel{}, err
	}
	for _, ch := range all {
		if ch.ID == id {
			return ch, nil
		}
	}
	return notify.Channel{}, sql.ErrNoRows
}

// ChannelView is a channel as the UI sees it: secrets blanked, with the names of those that are set.
type ChannelView struct {
	ID         int64           `json:"id"`
	Type       string          `json:"type" enum:"ntfy,gotify,telegram,webhook"`
	Name       string          `json:"name"`
	Enabled    bool            `json:"enabled"`
	Config     json.RawMessage `json:"config"`
	SecretsSet []string        `json:"secrets_set" doc:"Secret fields that have a value (never sent back)"`
	LastOK     *time.Time      `json:"last_ok,omitempty"`
	LastError  string          `json:"last_error,omitempty"`
	LastAt     *time.Time      `json:"last_at,omitempty"`
}

func (m *Manager) Views(ctx context.Context) ([]ChannelView, error) {
	chs, err := m.Channels(ctx)
	if err != nil {
		return nil, err
	}
	out := []ChannelView{}
	for _, ch := range chs {
		cfg, set := notify.Redact(ch)
		if set == nil {
			set = []string{}
		}
		v := ChannelView{ID: ch.ID, Type: ch.Type, Name: ch.Name, Enabled: ch.Enabled, Config: cfg, SecretsSet: set}
		var ts, errText string
		var ok bool
		if m.db.QueryRowContext(ctx, `SELECT ts, ok, error FROM notify_log WHERE channel_id = ? ORDER BY id DESC LIMIT 1`, ch.ID).Scan(&ts, &ok, &errText) == nil {
			t := store.ParseTime(ts)
			v.LastAt = &t
			if !ok {
				v.LastError = errText
			}
		}
		if m.db.QueryRowContext(ctx, `SELECT ts FROM notify_log WHERE channel_id = ? AND ok = 1 ORDER BY id DESC LIMIT 1`, ch.ID).Scan(&ts) == nil {
			t := store.ParseTime(ts)
			v.LastOK = &t
		}
		out = append(out, v)
	}
	return out, nil
}

// SaveChannel creates (ID 0) or updates a channel. On update, empty secret fields keep their
// stored value (the UI never sees secrets, so it can't send them back).
func (m *Manager) SaveChannel(ctx context.Context, ch notify.Channel) (notify.Channel, error) {
	if ch.ID != 0 {
		old, err := m.Channel(ctx, ch.ID)
		if err != nil {
			return ch, err
		}
		if old.Type != ch.Type {
			return ch, errors.New("the type of a channel can't change: create a new one")
		}
		if ch.Config, err = notify.MergeSecrets(ch.Type, old.Config, ch.Config); err != nil {
			return ch, err
		}
	}
	if err := notify.Validate(ch); err != nil {
		return ch, err
	}
	enc, err := m.seal(ch.Config)
	if err != nil {
		return ch, err
	}
	if ch.ID == 0 {
		res, err := m.db.ExecContext(ctx, `INSERT INTO notification_channels (type, name, enabled, config_enc) VALUES (?, ?, ?, ?)`, ch.Type, ch.Name, ch.Enabled, enc)
		if err != nil {
			return ch, err
		}
		ch.ID, _ = res.LastInsertId()
		return ch, nil
	}
	_, err = m.db.ExecContext(ctx, `UPDATE notification_channels SET name = ?, enabled = ?, config_enc = ? WHERE id = ?`, ch.Name, ch.Enabled, enc, ch.ID)
	return ch, err
}

func (m *Manager) DeleteChannel(ctx context.Context, id int64) error {
	_, err := m.db.ExecContext(ctx, `DELETE FROM notification_rules WHERE channel_id = ?`, id)
	if err == nil {
		_, err = m.db.ExecContext(ctx, `DELETE FROM notification_channels WHERE id = ?`, id)
	}
	return err
}

// Test sends a test message through one channel right away.
func (m *Manager) Test(ctx context.Context, id int64) error {
	ch, err := m.Channel(ctx, id)
	if err != nil {
		return err
	}
	return m.d.SendTest(ctx, ch)
}

// Rules returns all rules (notify.Source).
func (m *Manager) Rules(ctx context.Context) ([]notify.Rule, error) {
	rows, err := m.db.QueryContext(ctx, `SELECT id, channel_id, rule FROM notification_rules ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []notify.Rule{}
	for rows.Next() {
		var r notify.Rule
		var raw string
		var id, ch int64
		if err := rows.Scan(&id, &ch, &raw); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(raw), &r)
		r.ID, r.ChannelID = id, ch
		if r.Events == nil {
			r.Events = []string{}
		}
		if r.Stacks == nil {
			r.Stacks = []string{}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (m *Manager) SaveRule(ctx context.Context, r notify.Rule) (notify.Rule, error) {
	if err := r.Validate(); err != nil {
		return r, err
	}
	if _, err := m.Channel(ctx, r.ChannelID); err != nil {
		return r, fmt.Errorf("no channel %d", r.ChannelID)
	}
	b, _ := json.Marshal(r)
	if r.ID == 0 {
		res, err := m.db.ExecContext(ctx, `INSERT INTO notification_rules (channel_id, rule) VALUES (?, ?)`, r.ChannelID, string(b))
		if err != nil {
			return r, err
		}
		r.ID, _ = res.LastInsertId()
		return r, nil
	}
	res, err := m.db.ExecContext(ctx, `UPDATE notification_rules SET channel_id = ?, rule = ? WHERE id = ?`, r.ChannelID, string(b), r.ID)
	if n, _ := res.RowsAffected(); err == nil && n == 0 {
		return r, sql.ErrNoRows
	}
	return r, err
}

func (m *Manager) DeleteRule(ctx context.Context, id int64) error {
	_, err := m.db.ExecContext(ctx, `DELETE FROM notification_rules WHERE id = ?`, id)
	return err
}

type LogEntry struct {
	Time      time.Time `json:"time"`
	ChannelID int64     `json:"channel_id"`
	Event     string    `json:"event"`
	Title     string    `json:"title"`
	Stack     string    `json:"stack,omitempty"`
	Attempt   int       `json:"attempt"`
	OK        bool      `json:"ok"`
	Error     string    `json:"error,omitempty"`
}

func (m *Manager) Log(ctx context.Context, limit int) ([]LogEntry, error) {
	rows, err := m.db.QueryContext(ctx, `SELECT ts, channel_id, event_type, title, stack_id, attempt, ok, error FROM notify_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LogEntry{}
	for rows.Next() {
		var e LogEntry
		var ts string
		if err := rows.Scan(&ts, &e.ChannelID, &e.Event, &e.Title, &e.Stack, &e.Attempt, &e.OK, &e.Error); err != nil {
			return nil, err
		}
		e.Time = store.ParseTime(ts)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (m *Manager) logDelivery(d notify.Delivery) {
	_, _ = m.db.Exec(`INSERT INTO notify_log (channel_id, event_type, title, stack_id, attempt, ok, error) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		d.ChannelID, d.Event.Type, d.Event.Title, d.Event.Stack, d.Attempt, d.OK, d.Err)
	_, _ = m.db.Exec(`DELETE FROM notify_log WHERE ts < strftime('%Y-%m-%dT%H:%M:%fZ', 'now', '-30 days')`)
}

// --- turning host events into notifications -------------------------------------------------

// Expect marks changes to a stack as wanted for a while (a job or action runs on it): its
// containers stopping then is no crash.
func (m *Manager) Expect(stackID string, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if until := time.Now().Add(d); until.After(m.expected[stackID]) {
		m.expected[stackID] = until
	}
}

func (m *Manager) isExpected(stackID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return time.Now().Before(m.expected[stackID])
}

// DockerEvent looks at container events: health turning unhealthy, unexpected non-zero exits,
// restart loops (3 starts in 5 minutes).
func (m *Manager) DockerEvent(e docker.Event, stackID string) {
	if e.Type != "container" {
		return
	}
	name := e.Attrs["name"]
	if e.Attrs[docker.LabelOneoff] == "True" {
		return
	}
	link := ""
	if stackID != "" {
		link = "/stacks/" + stackID
	}
	switch {
	case strings.HasPrefix(e.Action, "health_status") && strings.HasSuffix(e.Action, "unhealthy"):
		m.Send(notify.Event{Type: "container.unhealthy", Severity: notify.Error, Title: name + " is unhealthy",
			Message: "The healthcheck of " + name + " fails.", Stack: stackID, URL: link, Time: e.Time, Fields: map[string]string{"dedupe": name}})
	case e.Action == "die":
		code := e.Attrs["exitCode"]
		if code == "0" || code == "" || m.isExpected(stackID) {
			return
		}
		m.Send(notify.Event{Type: "container.died", Severity: notify.Error, Title: name + " exited with code " + code,
			Message: name + " stopped unexpectedly (exit code " + code + ").", Stack: stackID, URL: link, Time: e.Time, Fields: map[string]string{"dedupe": name}})
	case e.Action == "start":
		m.mu.Lock()
		now := time.Now()
		var recent []time.Time
		for _, t := range m.starts[e.ID] {
			if now.Sub(t) < 5*time.Minute {
				recent = append(recent, t)
			}
		}
		recent = append(recent, now)
		m.starts[e.ID] = recent
		loop := len(recent) > 3 && now.Sub(m.looped[e.ID]) > 30*time.Minute
		if loop {
			m.looped[e.ID] = now
		}
		m.mu.Unlock()
		if loop && !m.isExpected(stackID) {
			m.Send(notify.Event{Type: "container.restart_loop", Severity: notify.Error, Title: name + " keeps restarting",
				Message: fmt.Sprintf("%s started %d times in 5 minutes.", name, len(recent)), Stack: stackID, URL: link, Fields: map[string]string{"dedupe": name}})
		}
	}
}

// JobFinished reports failed jobs (updates report themselves with more detail).
func (m *Manager) JobFinished(j jobs.Job) {
	if j.StackID != "" { // containers settle for a moment after the job, then crashes count again
		m.mu.Lock()
		m.expected[j.StackID] = time.Now().Add(30 * time.Second)
		m.mu.Unlock()
	}
	if j.State != jobs.Failed {
		return
	}
	switch j.Kind {
	case "update", "rebuild", "rollback":
		return
	}
	link := "/jobs?id=" + fmt.Sprint(j.ID)
	m.Send(notify.Event{Type: "job.failed", Severity: notify.Warning, Title: j.Title + " failed", Message: j.Error, Stack: j.StackID, URL: link,
		Fields: map[string]string{"dedupe": j.Kind}})
}

// Host warns when the disk under apps_root has less than 10 % free (at most every 6 hours).
func (m *Manager) Host(h hoststats.Host) {
	if h.DiskTotal == 0 || float64(h.DiskFree)/float64(h.DiskTotal) >= 0.10 {
		return
	}
	m.mu.Lock()
	due := time.Since(m.diskAt) > 6*time.Hour
	if due {
		m.diskAt = time.Now()
	}
	m.mu.Unlock()
	if due {
		m.Send(notify.Event{Type: "system.disk_low", Severity: notify.Warning, Title: "Disk almost full",
			Message: fmt.Sprintf("%s: %.1f GiB free of %.0f GiB.", h.DiskPath, float64(h.DiskFree)/(1<<30), float64(h.DiskTotal)/(1<<30)), URL: "/host"})
	}
}
