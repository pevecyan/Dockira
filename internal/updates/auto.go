package updates

import (
	"context"
	"fmt"
	"strings"
	"syscall"
	"time"

	"github.com/robfig/cron/v3"

	"docker-manager/internal/jobs"
	"docker-manager/internal/notify"
	"docker-manager/internal/stacks"
)

// inWindow: the latest window start (at most dur ago) is not in the future.
func inWindow(sched cron.Schedule, minutes int, now time.Time) bool {
	dur := time.Duration(minutes) * time.Minute
	start := sched.Next(now.Add(-dur))
	return !start.After(now)
}

func windowStart(sched cron.Schedule, minutes int, now time.Time) time.Time {
	return sched.Next(now.Add(-time.Duration(minutes) * time.Minute))
}

// autoInfo decides whether a service would be updated automatically, and if not, why.
func (m *Manager) autoInfo(st stacks.Stack, sv stacks.Service, u ServiceUpdate, pol Policy, set Settings, bad map[string]bool) AutoInfo {
	block := func(why string) AutoInfo { return AutoInfo{Blocked: why} }
	if pol.Mode != "auto" {
		return AutoInfo{}
	}
	switch u.State {
	case UpdateAvailable, PulledNotApplied:
	case BaseUpdated:
		if !pol.Rebuild {
			return block("rebuilds are not automatic for this stack")
		}
	default:
		return AutoInfo{}
	}
	switch {
	case set.Paused:
		return block("automatic updates are paused")
	case st.Self:
		return block("Dockira updates itself only when you ask")
	case sv.Stateful && !pol.IncludeDB:
		return block("looks like a database: enable “include databases” to update it automatically")
	case st.Status != "running":
		return block("the stack is " + st.Status + ": only healthy stacks are updated automatically")
	case u.State == UpdateAvailable && bad[u.Image+"@"+u.RemoteDigest]:
		return block("this image failed before and was rolled back; waiting for a newer one")
	}
	if u.State != PulledNotApplied && set.CooldownHours > 0 && !u.RemoteSince.IsZero() {
		if ready := u.RemoteSince.Add(time.Duration(set.CooldownHours) * time.Hour); time.Now().Before(ready) {
			return AutoInfo{At: m.windowAfter(set, ready),
				Blocked: fmt.Sprintf("cooldown: the new image is %s old, waits until %s", time.Since(u.RemoteSince).Round(time.Minute), ready.Format("Jan 2 15:04"))}
		}
	}
	return AutoInfo{Eligible: true, At: m.windowAfter(set, time.Now())}
}

func (m *Manager) windowAfter(set Settings, t time.Time) time.Time {
	sched, err := cronParser.Parse(set.WindowCron)
	if err != nil {
		return time.Time{}
	}
	if inWindow(sched, set.WindowMinutes, t) {
		return t
	}
	return sched.Next(t)
}

// Run checks for updates on schedule and applies automatic updates in the maintenance window.
func (m *Manager) Run(ctx context.Context) {
	m.attempted = map[string]time.Time{}
	// first check soon after start when the last one is old
	time.AfterFunc(time.Minute, func() {
		if ctx.Err() == nil && time.Since(m.Summary(ctx).LastCheck) > 6*time.Hour {
			m.scheduledCheck(ctx)
		}
	})
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			m.tick(ctx, now)
		}
	}
}

func (m *Manager) tick(ctx context.Context, now time.Time) {
	set := m.Settings(ctx)
	if sched, err := cronParser.Parse(set.CheckCron); err == nil {
		m.mu.Lock()
		if m.nextCheck.IsZero() {
			m.nextCheck = sched.Next(now)
		}
		due := !now.Before(m.nextCheck)
		if due {
			m.nextCheck = sched.Next(now)
		}
		m.mu.Unlock()
		if due {
			m.scheduledCheck(ctx)
		}
	}
	if set.Paused {
		return
	}
	sched, err := cronParser.Parse(set.WindowCron)
	if err != nil || !inWindow(sched, set.WindowMinutes, now) {
		return
	}
	m.autoUpdate(ctx, set, windowStart(sched, set.WindowMinutes, now), now.Add(time.Duration(set.WindowMinutes)*time.Minute))
}

func (m *Manager) scheduledCheck(ctx context.Context) {
	if m.Jobs == nil {
		_ = m.Check(ctx, "", nil)
		return
	}
	j, err := m.Jobs.Submit(m.CheckSpec("", "schedule"))
	if err == nil {
		_, _ = m.Jobs.Wait(ctx, j.ID)
	}
}

// CheckSpec is the job that checks for updates.
func (m *Manager) CheckSpec(stackID, trigger string) jobs.Spec {
	title := "Check for updates"
	if stackID != "" {
		title += " (" + stackID + ")"
	}
	return jobs.Spec{Kind: "check", Title: title, Trigger: trigger, Timeout: 15 * time.Minute,
		Run: func(ctx context.Context, out *jobs.Output) error { return m.Check(ctx, stackID, out) }}
}

// autoUpdate applies eligible updates, one stack at a time, once per stack per window.
func (m *Manager) autoUpdate(ctx context.Context, set Settings, start, end time.Time) {
	svcs, err := m.Services(ctx, "")
	if err != nil {
		return
	}
	byStack := map[string][]string{}
	var order []string
	for _, s := range svcs {
		if !s.Auto.Eligible || s.Auto.Blocked != "" {
			continue
		}
		if _, ok := byStack[s.StackID]; !ok {
			order = append(order, s.StackID)
		}
		byStack[s.StackID] = append(byStack[s.StackID], s.Service)
	}
	for _, id := range order {
		if ctx.Err() != nil || time.Now().After(end) {
			return
		}
		m.mu.Lock()
		done := !m.attempted[id].Before(start)
		if !done {
			m.attempted[id] = start
		}
		m.mu.Unlock()
		if done {
			continue
		}
		st, ok := m.Stacks.Get(id)
		if !ok || m.busy(id) {
			continue
		}
		if why := m.lowDisk(set); why != "" {
			m.skip(st, why)
			return
		}
		spec, err := m.UpdateSpec(st, byStack[id], "auto", jobs.Spec{StackID: st.ID, Kind: "update", Trigger: "auto",
			Title: "Automatic update of " + st.ID + " (" + strings.Join(byStack[id], ", ") + ")", Services: byStack[id], Timeout: 45 * time.Minute})
		if err != nil {
			m.skip(st, err.Error())
			continue
		}
		j, err := m.Jobs.Submit(spec)
		if err != nil {
			continue
		}
		_, _ = m.Jobs.Wait(ctx, j.ID) // one stack at a time
	}
}

func (m *Manager) busy(stackID string) bool {
	for _, j := range m.Jobs.Active() {
		if j.StackID == stackID {
			return true
		}
	}
	return false
}

func (m *Manager) lowDisk(set Settings) string {
	if set.MinFreeGiB <= 0 || m.DiskPath == "" {
		return ""
	}
	var fs syscall.Statfs_t
	if syscall.Statfs(m.DiskPath, &fs) != nil {
		return ""
	}
	free := fs.Bavail * uint64(fs.Bsize)
	if free < uint64(set.MinFreeGiB)<<30 {
		return fmt.Sprintf("only %.1f GiB free disk (need %d GiB)", float64(free)/(1<<30), set.MinFreeGiB)
	}
	return ""
}

func (m *Manager) skip(st stacks.Stack, why string) {
	m.Log.Warn("automatic update skipped", "stack", st.ID, "reason", why)
	if m.Notify != nil {
		m.Notify(notify.Event{Type: "autoupdate.skipped", Severity: notify.Warning, Title: "Automatic update of " + st.ID + " skipped",
			Message: why, Stack: st.ID, URL: "/stacks/" + st.ID + "?tab=updates", Time: time.Now()})
	}
}
