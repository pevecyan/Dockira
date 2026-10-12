// Package notify sends events (updates, unhealthy containers, security alerts, …) to the
// channels configured under Settings → Notifications: ntfy, Gotify, Telegram and webhooks.
// Rules decide which events go where; the Dispatcher applies them, suppresses repeats and
// retries failed deliveries.
package notify

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type Severity int

const (
	Info Severity = iota
	Warning
	Error
)

func (s Severity) String() string {
	switch s {
	case Info:
		return "info"
	case Warning:
		return "warning"
	case Error:
		return "error"
	}
	return fmt.Sprintf("severity(%d)", int(s))
}

// ParseSeverity accepts "info", "warning" (or "warn") and "error", in any case.
func ParseSeverity(s string) (Severity, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "info":
		return Info, nil
	case "warning", "warn":
		return Warning, nil
	case "error":
		return Error, nil
	}
	return 0, fmt.Errorf("unknown severity %q", s)
}

// MarshalText makes Severity a string in JSON ("warning", not 1).
func (s Severity) MarshalText() ([]byte, error) {
	if s < Info || s > Error {
		return nil, fmt.Errorf("invalid severity %d", int(s))
	}
	return []byte(s.String()), nil
}

func (s *Severity) UnmarshalText(b []byte) error {
	v, err := ParseSeverity(string(b))
	if err != nil {
		return err
	}
	*s = v
	return nil
}

// UnmarshalJSON accepts the string form and, for leniency, the number (0–2).
func (s *Severity) UnmarshalJSON(b []byte) error {
	var n int
	if err := json.Unmarshal(b, &n); err == nil {
		if n < int(Info) || n > int(Error) {
			return fmt.Errorf("invalid severity %d", n)
		}
		*s = Severity(n)
		return nil
	}
	var str string
	if err := json.Unmarshal(b, &str); err != nil {
		return fmt.Errorf("severity must be a string: %w", err)
	}
	return s.UnmarshalText([]byte(str))
}

type Event struct {
	Type     string            `json:"type"` // e.g. "update.available", "container.unhealthy", "auth.lockout"
	Severity Severity          `json:"severity"`
	Title    string            `json:"title"`
	Message  string            `json:"message"`
	Stack    string            `json:"stack,omitempty"` // stack id (relative path), "" for global
	URL      string            `json:"url,omitempty"`   // link into the UI
	Time     time.Time         `json:"time"`
	Fields   map[string]string `json:"fields,omitempty"` // extra details; "dedupe" refines the dedupe key
}

// Security events get through quiet hours: a lockout at 3 a.m. is exactly when you want to know.
func (e Event) Security() bool { return strings.HasPrefix(e.Type, "auth.") }

// EventType describes an event the app can emit, for the rule editor in the UI.
type EventType struct {
	Type        string   `json:"type"`
	Severity    Severity `json:"severity"`
	Description string   `json:"description"`
}

// TypeTest is the event sent by the "Send test" button.
const TypeTest = "test"

// EventTypes lists every event type the app emits, with its default severity.
var EventTypes = []EventType{
	{"update.available", Info, "New image versions found (one summary per check run)"},
	{"update.applied", Info, "An update was applied (manually or automatically)"},
	{"update.failed", Error, "An update failed"},
	{"update.rolled_back", Error, "An update was rolled back"},
	{"autoupdate.skipped", Warning, "An automatic update was skipped (pre-condition or hook failed)"},
	{"container.unhealthy", Error, "A container's health check is failing"},
	{"container.restart_loop", Error, "A container restarted more than 3 times in 5 minutes"},
	{"container.died", Error, "A container exited with a non-zero code without a user action"},
	{"job.failed", Warning, "A background job failed"},
	{"stack.file_changed_externally", Info, "A compose or .env file was changed outside the app"},
	{"auth.login_new_ip", Warning, "Successful login from an IP address not seen before"},
	{"auth.lockout", Warning, "Logins paused after too many failed attempts"},
	{"system.disk_low", Warning, "Less than 10% disk space free"},
	{"system.registry_errors", Warning, "Repeated errors talking to an image registry"},
	{TypeTest, Info, "Test message from the \"Send test\" button"},
}
