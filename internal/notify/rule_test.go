package notify

import (
	"testing"
	"time"
)

func TestMatchPattern(t *testing.T) {
	cases := []struct {
		p, typ string
		want   bool
	}{
		{"*", "update.available", true},
		{"update.*", "update.available", true},
		{"update.*", "autoupdate.skipped", false},
		{"update.*", "update", false},
		{"*.failed", "job.failed", true},
		{"job.failed", "job.failed", true},
		{"job.failed", "job.failedx", false},
		{"[", "x", false},
	}
	for _, c := range cases {
		if got := MatchPattern(c.p, c.typ); got != c.want {
			t.Errorf("MatchPattern(%q, %q) = %v", c.p, c.typ, got)
		}
	}
}

func TestRuleMatches(t *testing.T) {
	upd := Event{Type: "update.applied", Severity: Info, Stack: "media/jellyfin"}
	died := Event{Type: "container.died", Severity: Error, Stack: "web"}
	global := Event{Type: "system.disk_low", Severity: Warning}

	all := Rule{}
	if !all.Matches(upd) || !all.Matches(died) || !all.Matches(global) {
		t.Error("empty rule should match everything")
	}
	warn := Rule{MinSeverity: Warning}
	if warn.Matches(upd) || !warn.Matches(died) || !warn.Matches(global) {
		t.Error("severity")
	}
	updates := Rule{Events: []string{"update.*"}}
	if !updates.Matches(upd) || updates.Matches(died) {
		t.Error("events pattern")
	}
	multi := Rule{Events: []string{"container.died", "system.*"}}
	if multi.Matches(upd) || !multi.Matches(died) || !multi.Matches(global) {
		t.Error("multiple patterns")
	}
	stacks := Rule{Stacks: []string{"media/jellyfin"}}
	if !stacks.Matches(upd) || stacks.Matches(died) || !stacks.Matches(global) {
		t.Error("stacks (global events always match)")
	}
}

func TestRuleValidate(t *testing.T) {
	if err := (Rule{Events: []string{"update.*"}, QuietHours: &QuietHours{"22:00", "07:00"}}).Validate(); err != nil {
		t.Error(err)
	}
	for _, r := range []Rule{
		{Events: []string{"["}},
		{Events: []string{""}},
		{MinSeverity: 5},
		{QuietHours: &QuietHours{"25:00", "07:00"}},
		{QuietHours: &QuietHours{"22:00", ""}},
	} {
		if r.Validate() == nil {
			t.Errorf("accepted %+v", r)
		}
	}
}

func TestQuietHours(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 10, 11, h, m, 0, 0, time.Local) }
	wrap := &QuietHours{Start: "22:00", End: "07:00"}
	for _, c := range []struct {
		h, m int
		want bool
	}{{21, 59, false}, {22, 0, true}, {23, 30, true}, {0, 0, true}, {6, 59, true}, {7, 0, false}, {12, 0, false}} {
		if got := wrap.Active(at(c.h, c.m)); got != c.want {
			t.Errorf("22–07 at %02d:%02d = %v", c.h, c.m, got)
		}
	}
	day := &QuietHours{Start: "09:00", End: "17:30"}
	if !day.Active(at(9, 0)) || !day.Active(at(17, 29)) || day.Active(at(17, 30)) || day.Active(at(8, 0)) {
		t.Error("09–17:30")
	}
	var none *QuietHours
	if none.Active(at(23, 0)) || (&QuietHours{"10:00", "10:00"}).Active(at(10, 0)) || (&QuietHours{"x", "y"}).Active(at(0, 0)) {
		t.Error("nil, empty and malformed windows must be inactive")
	}
	// The time's own location counts, not UTC.
	tz := time.FixedZone("UTC+5", 5*3600)
	if !wrap.Active(time.Date(2026, 1, 1, 18, 0, 0, 0, time.UTC).In(tz)) { // 23:00 local
		t.Error("location ignored")
	}
}
