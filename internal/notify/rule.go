package notify

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"time"
)

// QuietHours is a daily window in local time, e.g. 22:00–07:00. The window may wrap midnight;
// Start is inclusive, End exclusive, and Start == End means no quiet hours.
type QuietHours struct {
	Start string `json:"start"` // "HH:MM"
	End   string `json:"end"`
}

// Active reports whether t (in its own location, normally time.Local) falls in the window.
// A nil or malformed window is never active: losing a notification is worse than a late ping.
func (q *QuietHours) Active(t time.Time) bool {
	if q == nil {
		return false
	}
	start, err1 := clockMinutes(q.Start)
	end, err2 := clockMinutes(q.End)
	if err1 != nil || err2 != nil || start == end {
		return false
	}
	m := t.Hour()*60 + t.Minute()
	if start < end {
		return m >= start && m < end
	}
	return m >= start || m < end
}

func (q *QuietHours) validate() error {
	if _, err := clockMinutes(q.Start); err != nil {
		return fmt.Errorf("quiet_hours.start: %w", err)
	}
	if _, err := clockMinutes(q.End); err != nil {
		return fmt.Errorf("quiet_hours.end: %w", err)
	}
	return nil
}

func clockMinutes(s string) (int, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a time of day (HH:MM)", s)
	}
	return t.Hour()*60 + t.Minute(), nil
}

// Rule routes events to one channel.
type Rule struct {
	ID            int64       `json:"id,omitempty"`
	ChannelID     int64       `json:"channel_id"`
	Events        []string    `json:"events,omitempty"` // patterns: "*", "update.*", exact types; empty = all
	Stacks        []string    `json:"stacks,omitempty"` // empty = all stacks; global events (no stack) always match
	MinSeverity   Severity    `json:"min_severity"`
	DedupeMinutes int         `json:"dedupe_minutes,omitempty"` // 0 = DefaultDedupe, negative = never suppress repeats
	QuietHours    *QuietHours `json:"quiet_hours,omitempty"`
}

const DefaultDedupe = 60 * time.Minute

// Matches reports whether the event is selected by the rule's events, stacks and severity.
// Quiet hours and dedupe are applied by the Dispatcher, not here.
func (r Rule) Matches(e Event) bool {
	if e.Severity < r.MinSeverity {
		return false
	}
	if len(r.Events) > 0 && !slices.ContainsFunc(r.Events, func(p string) bool { return MatchPattern(p, e.Type) }) {
		return false
	}
	if len(r.Stacks) > 0 && e.Stack != "" && !slices.Contains(r.Stacks, e.Stack) {
		return false
	}
	return true
}

// Validate checks patterns, severity and quiet hours.
func (r Rule) Validate() error {
	var errs []error
	for _, p := range r.Events {
		if _, err := path.Match(p, ""); err != nil || p == "" {
			errs = append(errs, fmt.Errorf("event pattern %q is malformed", p))
		}
	}
	if r.MinSeverity < Info || r.MinSeverity > Error {
		errs = append(errs, errors.New("min_severity is invalid"))
	}
	if r.QuietHours != nil {
		errs = append(errs, r.QuietHours.validate())
	}
	return errors.Join(errs...)
}

// dedupeWindow is the rule's suppression window; 0 means repeats are not suppressed.
func (r Rule) dedupeWindow() time.Duration {
	switch {
	case r.DedupeMinutes == 0:
		return DefaultDedupe
	case r.DedupeMinutes < 0:
		return 0
	}
	return time.Duration(r.DedupeMinutes) * time.Minute
}

// MatchPattern matches an event type against "*", "update.*", "*.failed" or an exact type
// (shell-style globbing, as path.Match). A malformed pattern matches nothing.
func MatchPattern(pattern, typ string) bool {
	ok, err := path.Match(pattern, typ)
	return ok && err == nil
}
