package notify

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSeverityJSON(t *testing.T) {
	b, err := json.Marshal(struct{ S Severity }{Warning})
	if err != nil || string(b) != `{"S":"warning"}` {
		t.Fatalf("%s %v", b, err)
	}
	for in, want := range map[string]Severity{`"info"`: Info, `"ERROR"`: Error, `"warn"`: Warning, `2`: Error, `0`: Info} {
		var s Severity
		if err := json.Unmarshal([]byte(in), &s); err != nil || s != want {
			t.Errorf("%s: got %v, %v", in, s, err)
		}
	}
	for _, in := range []string{`"loud"`, `7`, `-1`, `true`} {
		var s Severity
		if err := json.Unmarshal([]byte(in), &s); err == nil {
			t.Errorf("%s accepted", in)
		}
	}
	if _, err := json.Marshal(Severity(9)); err == nil {
		t.Error("invalid severity marshalled")
	}
	if Severity(9).String() != "severity(9)" {
		t.Error(Severity(9).String())
	}
}

func TestEventJSONAndSecurity(t *testing.T) {
	e := Event{Type: "auth.lockout", Severity: Warning, Title: "t", Time: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	b, _ := json.Marshal(e)
	if !strings.Contains(string(b), `"severity":"warning"`) || strings.Contains(string(b), "stack") {
		t.Errorf("json: %s", b)
	}
	if !e.Security() || (Event{Type: "update.failed"}).Security() {
		t.Error("Security()")
	}
}

func TestEventTypesComplete(t *testing.T) {
	seen := map[string]bool{}
	for _, et := range EventTypes {
		if seen[et.Type] || et.Description == "" {
			t.Errorf("duplicate or undescribed %q", et.Type)
		}
		seen[et.Type] = true
	}
	for _, typ := range []string{"update.available", "update.applied", "update.failed", "update.rolled_back",
		"autoupdate.skipped", "container.unhealthy", "container.restart_loop", "container.died", "job.failed",
		"stack.file_changed_externally", "auth.login_new_ip", "auth.lockout", "system.disk_low",
		"system.registry_errors", "test"} {
		if !seen[typ] {
			t.Errorf("missing %s", typ)
		}
	}
}
