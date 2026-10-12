package updates

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"docker-manager/internal/docker"
	"docker-manager/internal/jobs"
	"docker-manager/internal/notify"
	"docker-manager/internal/snapshots"
	"docker-manager/internal/stacks"
	"docker-manager/internal/store"
)

type Change struct {
	Service string `json:"service"`
	Image   string `json:"image"`
	From    string `json:"from" doc:"Image ID before"`
	To      string `json:"to" doc:"Image ID after"`
}

type Run struct {
	ID        int64     `json:"id"`
	StackID   string    `json:"stack_id"`
	Trigger   string    `json:"trigger"`
	JobID     int64     `json:"job_id"`
	Services  []string  `json:"services"`
	Changes   []Change  `json:"changes"`
	Result    string    `json:"result" enum:"running,updated,unchanged,failed,rolled_back"`
	Message   string    `json:"message,omitempty"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitzero"`
}

type RollbackPoint struct {
	ID        int64     `json:"id"`
	StackID   string    `json:"stack_id"`
	Service   string    `json:"service"`
	Ref       string    `json:"ref"`
	ImageID   string    `json:"image_id"`
	Digest    string    `json:"digest,omitempty"`
	Tag       string    `json:"tag"`
	CreatedAt time.Time `json:"created_at"`
	Current   bool      `json:"current" doc:"The service runs this image right now"`
}

// UpdateSpec turns an "update" action into the full flow (api.Updater).
func (m *Manager) UpdateSpec(st stacks.Stack, services []string, trigger string, base jobs.Spec) (jobs.Spec, error) {
	pols, _ := m.policies(context.Background())
	set := m.Settings(context.Background())
	pol := effective(pols, set, st.ID, "")
	targets := pickServices(st, services)
	if len(targets) == 0 {
		return base, errors.New("nothing to update: no services with an image or a build")
	}
	base.Kind = "update"
	base.Run = func(ctx context.Context, out *jobs.Output) error {
		return m.update(ctx, st, targets, trigger, pol, set, out)
	}
	return base, nil
}

func pickServices(st stacks.Stack, only []string) []stacks.Service {
	var out []stacks.Service
	for _, sv := range st.Services {
		if !sv.InFile || (len(only) > 0 && !contains(only, sv.Name)) {
			continue
		}
		out = append(out, sv)
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func names(svcs []stacks.Service) []string {
	out := make([]string, len(svcs))
	for i, s := range svcs {
		out[i] = s.Name
	}
	return out
}

func (m *Manager) update(ctx context.Context, st stacks.Stack, targets []stacks.Service, trigger string, pol Policy, set Settings, out *jobs.Output) error {
	svcNames := names(targets)
	runID := m.startRun(ctx, st.ID, trigger, svcNames, out.JobID())
	finish := func(result, msg string, changes []Change) {
		m.endRun(ctx, runID, result, msg, changes)
	}

	// 1. rollback points: tag what runs now
	before := map[string]string{}
	points := map[string]RollbackPoint{}
	for _, sv := range targets {
		if p, ok, err := m.createPoint(ctx, st, sv); err != nil {
			out.Printf("  ! no rollback point for %s: %v", sv.Name, err)
		} else if ok {
			points[sv.Name] = p
			before[sv.Name] = p.ImageID
			out.Printf("▸ rollback point for %s: %s (%s)", sv.Name, p.Tag, short(p.ImageID))
		}
	}

	// 2. hooks (backups) — a failure stops the update before anything changed
	if err := m.runHooks(ctx, st, pol.Hooks, out); err != nil {
		finish("failed", "pre-update hook: "+err.Error(), nil)
		m.notifyResult(st, trigger, "update.failed", notify.Error, "Update of "+st.ID+" stopped", "A pre-update hook failed: "+err.Error())
		return fmt.Errorf("pre-update hook: %w", err)
	}

	// 3. new images
	var pull, build []string
	for _, sv := range targets {
		if sv.Build {
			build = append(build, sv.Name)
		} else {
			pull = append(pull, sv.Name)
		}
	}
	if len(pull) > 0 {
		if err := m.compose(ctx, st, out, append([]string{"pull"}, pull...)...); err != nil {
			finish("failed", "pull: "+err.Error(), nil)
			m.notifyResult(st, trigger, "update.failed", notify.Error, "Update of "+st.ID+" failed", "Pulling images failed: "+err.Error())
			return err
		}
	}
	if len(build) > 0 {
		if err := m.compose(ctx, st, out, append([]string{"build", "--pull"}, build...)...); err != nil {
			finish("failed", "build: "+err.Error(), nil)
			m.notifyResult(st, trigger, "update.failed", notify.Error, "Update of "+st.ID+" failed", "Building failed: "+err.Error())
			return err
		}
		// the build used the registry's current base images: that's the new baseline
		for _, sv := range targets {
			if sv.Build {
				for _, raw := range baseImages(sv) {
					_, _ = m.Store.DB.ExecContext(ctx, `UPDATE image_status SET local_digest = remote_digest WHERE ref = ? AND local_image_id = ''`, raw)
				}
			}
		}
	}

	// 4. apply
	if err := m.compose(ctx, st, out, append([]string{"up", "-d"}, svcNames...)...); err != nil {
		finish("failed", "up: "+err.Error(), nil)
		m.recheck(st.ID)
		m.notifyResult(st, trigger, "update.failed", notify.Error, "Update of "+st.ID+" failed", "docker compose up failed: "+err.Error())
		return err
	}

	// 5. what changed?
	_ = m.Stacks.RefreshContainers(ctx)
	fresh, _ := m.Stacks.Get(st.ID)
	var changes []Change
	for _, sv := range fresh.Services {
		if !contains(svcNames, sv.Name) || len(sv.Containers) == 0 {
			continue
		}
		if now := sv.Containers[0].ImageID; before[sv.Name] != now {
			changes = append(changes, Change{Service: sv.Name, Image: sv.Image, From: before[sv.Name], To: now})
		}
	}
	if len(changes) == 0 {
		out.Printf("▸ already up to date: no service got a new image")
		finish("unchanged", "", nil)
		m.recheck(st.ID)
		m.pruneUnused(ctx, st, points)
		return nil
	}
	for _, c := range changes {
		out.Printf("▸ %s: %s → %s", c.Service, short(c.From), short(c.To))
	}

	// 6. verify
	var changed []string
	for _, c := range changes {
		changed = append(changed, c.Service)
	}
	out.Printf("▸ waiting up to %d s for %s to become healthy", set.VerifySeconds, strings.Join(changed, ", "))
	verr := m.verify(ctx, st.ID, changed, time.Duration(set.VerifySeconds)*time.Second, out)
	if verr == nil {
		out.Printf("✓ healthy")
		finish("updated", "", changes)
		m.recheck(st.ID)
		m.prune(ctx, st.ID, changed, set.KeepRollbacks)
		m.notifyResult(st, trigger, "update.applied", notify.Info, "Updated "+st.ID, describeChanges(changes))
		return nil
	}
	out.Printf("✕ %v", verr)
	if !pol.AutoRollback || len(points) == 0 {
		finish("failed", verr.Error(), changes)
		m.recheck(st.ID)
		m.notifyResult(st, trigger, "update.failed", notify.Error, "Update of "+st.ID+" is unhealthy", verr.Error()+". Roll back from the stack's Updates tab.")
		return verr
	}

	// 7. roll back
	out.Printf("▸ rolling back")
	var rolled []string
	for _, svc := range changed {
		p, ok := points[svc]
		if !ok {
			continue
		}
		if err := m.restore(ctx, st, p, out); err != nil {
			out.Printf("  ! rollback of %s failed: %v", svc, err)
			continue
		}
		rolled = append(rolled, svc)
	}
	if trigger != "user" {
		for _, sv := range targets {
			if contains(changed, sv.Name) && !sv.Build {
				if r, err := m.rows(ctx); err == nil {
					m.markBad(ctx, sv.Image, r[sv.Image].RemoteDigest, verr.Error())
				}
			}
		}
	}
	rerr := m.verify(ctx, st.ID, rolled, time.Duration(set.VerifySeconds)*time.Second, out)
	msg := verr.Error() + "; rolled back " + strings.Join(rolled, ", ")
	if rerr != nil {
		msg += " (still unhealthy after the rollback: " + rerr.Error() + ")"
	}
	finish("rolled_back", msg, changes)
	m.recheck(st.ID)
	m.notifyResult(st, trigger, "update.rolled_back", notify.Error, "Update of "+st.ID+" rolled back", msg)
	return fmt.Errorf("rolled back: %v", verr)
}

func describeChanges(cs []Change) string {
	var l []string
	for _, c := range cs {
		l = append(l, fmt.Sprintf("• %s: %s (%s → %s)", c.Service, c.Image, short(c.From), short(c.To)))
	}
	return strings.Join(l, "\n")
}

func short(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	if id == "" {
		return "none"
	}
	return id
}

func (m *Manager) notifyResult(st stacks.Stack, trigger, typ string, sev notify.Severity, title, msg string) {
	if m.Notify != nil {
		m.Notify(notify.Event{Type: typ, Severity: sev, Title: title, Message: msg, Stack: st.ID, URL: "/stacks/" + st.ID + "?tab=updates",
			Time: time.Now(), Fields: map[string]string{"trigger": trigger}})
	}
}

func (m *Manager) compose(ctx context.Context, st stacks.Stack, out *jobs.Output, args ...string) error {
	out.Printf("▸ docker compose %s", strings.Join(args, " "))
	return out.Exec(m.Compose.Cmd(ctx, st.Dir, st.Files, args...))
}

// recheck refreshes a stack's image states in the background after an update.
func (m *Manager) recheck(stackID string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = m.Check(ctx, stackID, nil)
	}()
}

// verify waits until every container of the services is healthy (with a healthcheck) or has
// been running without restarting for 20 s.
func (m *Manager) verify(ctx context.Context, stackID string, services []string, limit time.Duration, out *jobs.Output) error {
	if len(services) == 0 {
		return nil
	}
	const stable = 20 * time.Second
	deadline := time.Now().Add(limit)
	restarts := map[string]int{}
	var last string
	for {
		_ = m.Stacks.RefreshContainers(ctx)
		st, _ := m.Stacks.Get(stackID)
		ok, problem := true, ""
		for _, sv := range st.Services {
			if !contains(services, sv.Name) {
				continue
			}
			if len(sv.Containers) == 0 {
				ok, problem = false, sv.Name+": no container"
				continue
			}
			for _, c := range sv.Containers {
				d, err := m.Engine.Inspect(ctx, c.ID)
				if err != nil {
					ok, problem = false, c.Name+": "+err.Error()
					continue
				}
				if first, seen := restarts[c.ID]; !seen {
					restarts[c.ID] = d.RestartCount
				} else if d.RestartCount-first >= 2 {
					return fmt.Errorf("%s keeps restarting (%d restarts)", c.Name, d.RestartCount-first)
				}
				switch {
				case d.State == "exited" || d.State == "dead":
					return fmt.Errorf("%s exited with code %d", c.Name, d.ExitCode)
				case d.State != "running":
					ok, problem = false, c.Name+" is "+d.State
				case d.Health == "unhealthy":
					ok, problem = false, c.Name+" is unhealthy"
				case d.Health == "starting":
					ok, problem = false, c.Name+" health is still starting"
				case d.Health == "" && time.Since(d.StartedAt) < stable:
					ok, problem = false, c.Name+" running for less than 20 s"
				}
			}
		}
		if ok {
			return nil
		}
		if problem != last && out != nil {
			out.Printf("  … %s", problem)
			last = problem
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("not healthy after %s: %s", limit.Round(time.Second), problem)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// createPoint tags the image a service runs now as dm-rollback/<project>-<service>:<time>.
func (m *Manager) createPoint(ctx context.Context, st stacks.Stack, sv stacks.Service) (RollbackPoint, bool, error) {
	if len(sv.Containers) == 0 {
		return RollbackPoint{}, false, nil
	}
	id := sv.Containers[0].ImageID
	if id == "" {
		return RollbackPoint{}, false, nil
	}
	tag := fmt.Sprintf("dm-rollback/%s-%s:%s", repoPart(st.Name), repoPart(sv.Name), time.Now().UTC().Format("20060102-150405"))
	if err := m.Engine.ImageTag(ctx, id, tag); err != nil {
		return RollbackPoint{}, false, err
	}
	digest := ""
	if img, err := m.Engine.ImageInspect(ctx, id); err == nil && len(img.RepoDigests) > 0 {
		digest = img.RepoDigests[0]
		if i := strings.LastIndex(digest, "@"); i >= 0 {
			digest = digest[i+1:]
		}
	}
	p := RollbackPoint{StackID: st.ID, Service: sv.Name, Ref: sv.Image, ImageID: id, Digest: digest, Tag: tag, CreatedAt: time.Now()}
	res, err := m.Store.DB.ExecContext(ctx, `INSERT INTO rollback_points (stack_id, service, ref, image_id, digest, tag) VALUES (?, ?, ?, ?, ?, ?)`,
		p.StackID, p.Service, p.Ref, p.ImageID, p.Digest, p.Tag)
	if err != nil {
		return p, false, err
	}
	p.ID, _ = res.LastInsertId()
	return p, true, nil
}

// repoPart makes a name usable in an image repository (lowercase [a-z0-9-]).
func repoPart(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// restore puts a rollback image back under the service's image name and recreates the service
// from it without pulling or building.
func (m *Manager) restore(ctx context.Context, st stacks.Stack, p RollbackPoint, out *jobs.Output) error {
	out.Printf("▸ docker tag %s %s", p.Tag, p.Ref)
	if err := m.Engine.ImageTag(ctx, p.Tag, p.Ref); err != nil {
		return err
	}
	return m.compose(ctx, st, out, "up", "-d", "--no-deps", "--no-build", "--pull", "never", p.Service)
}

// pruneUnused drops rollback points of an update that changed nothing.
func (m *Manager) pruneUnused(ctx context.Context, st stacks.Stack, points map[string]RollbackPoint) {
	for _, p := range points {
		_ = m.Engine.ImageRemove(ctx, p.Tag)
		_, _ = m.Store.DB.ExecContext(ctx, `DELETE FROM rollback_points WHERE id = ?`, p.ID)
	}
}

// prune keeps the newest keep rollback points per service (old images use a lot of disk).
func (m *Manager) prune(ctx context.Context, stackID string, services []string, keep int) {
	for _, svc := range services {
		rows, err := m.Store.DB.QueryContext(ctx, `SELECT id, tag FROM rollback_points WHERE stack_id = ? AND service = ? ORDER BY id DESC LIMIT -1 OFFSET ?`,
			stackID, svc, keep)
		if err != nil {
			continue
		}
		type old struct {
			id  int64
			tag string
		}
		var olds []old
		for rows.Next() {
			var o old
			if rows.Scan(&o.id, &o.tag) == nil {
				olds = append(olds, o)
			}
		}
		rows.Close()
		for _, o := range olds {
			if err := m.Engine.ImageRemove(ctx, o.tag); err != nil && !docker.IsNotFound(err) {
				m.Log.Warn("remove rollback image", "tag", o.tag, "err", err)
			}
			_, _ = m.Store.DB.ExecContext(ctx, `DELETE FROM rollback_points WHERE id = ?`, o.id)
		}
	}
}

// RollbackPoints lists a stack's rollback points, newest first.
func (m *Manager) RollbackPoints(ctx context.Context, stackID string) ([]RollbackPoint, error) {
	rows, err := m.Store.DB.QueryContext(ctx, `SELECT id, stack_id, service, ref, image_id, digest, tag, created_at FROM rollback_points WHERE stack_id = ? ORDER BY id DESC`, stackID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	st, _ := m.Stacks.Get(stackID)
	running := map[string]string{}
	for _, sv := range st.Services {
		if len(sv.Containers) > 0 {
			running[sv.Name] = sv.Containers[0].ImageID
		}
	}
	out := []RollbackPoint{}
	for rows.Next() {
		var p RollbackPoint
		var created string
		if err := rows.Scan(&p.ID, &p.StackID, &p.Service, &p.Ref, &p.ImageID, &p.Digest, &p.Tag, &created); err != nil {
			return nil, err
		}
		p.CreatedAt = store.ParseTime(created)
		p.Current = running[p.Service] == p.ImageID
		out = append(out, p)
	}
	return out, rows.Err()
}

// RollbackSpec is the job that goes back to a rollback point.
func (m *Manager) RollbackSpec(ctx context.Context, st stacks.Stack, pointID int64) (jobs.Spec, error) {
	var p RollbackPoint
	err := m.Store.DB.QueryRowContext(ctx, `SELECT id, stack_id, service, ref, image_id, tag FROM rollback_points WHERE id = ? AND stack_id = ?`, pointID, st.ID).
		Scan(&p.ID, &p.StackID, &p.Service, &p.Ref, &p.ImageID, &p.Tag)
	if err != nil {
		return jobs.Spec{}, err
	}
	set := m.Settings(ctx)
	return jobs.Spec{StackID: st.ID, Kind: "rollback", Title: fmt.Sprintf("Roll back %s/%s", st.ID, p.Service), Services: []string{p.Service},
		Timeout: 15 * time.Minute, Run: func(ctx context.Context, out *jobs.Output) error {
			runID := m.startRun(ctx, st.ID, "rollback", []string{p.Service}, out.JobID())
			_ = m.Stacks.RefreshContainers(ctx)
			cur, _ := m.Stacks.Get(st.ID)
			from := ""
			for _, sv := range cur.Services {
				if sv.Name == p.Service && len(sv.Containers) > 0 {
					from = sv.Containers[0].ImageID
				}
			}
			changes := []Change{{Service: p.Service, Image: p.Ref, From: from, To: p.ImageID}}
			if err := m.restore(ctx, st, p, out); err != nil {
				m.endRun(ctx, runID, "failed", err.Error(), changes)
				return err
			}
			if err := m.verify(ctx, st.ID, []string{p.Service}, time.Duration(set.VerifySeconds)*time.Second, out); err != nil {
				m.endRun(ctx, runID, "failed", err.Error(), changes)
				return err
			}
			out.Printf("✓ %s runs %s again", p.Service, short(p.ImageID))
			m.endRun(ctx, runID, "rolled_back", "manual rollback to "+p.Tag, changes)
			m.recheck(st.ID)
			return nil
		}}, nil
}

func (m *Manager) startRun(ctx context.Context, stackID, trigger string, services []string, jobID int64) int64 {
	svc, _ := json.Marshal(services)
	res, err := m.Store.DB.ExecContext(ctx, `INSERT INTO update_runs (stack_id, trigger, services, job_id) VALUES (?, ?, ?, ?)`, stackID, trigger, string(svc), jobID)
	if err != nil {
		return 0
	}
	id, _ := res.LastInsertId()
	m.publish(ctx)
	return id
}

func (m *Manager) endRun(ctx context.Context, id int64, result, msg string, changes []Change) {
	if changes == nil {
		changes = []Change{}
	}
	ch, _ := json.Marshal(changes)
	_, _ = m.Store.DB.ExecContext(context.WithoutCancel(ctx), `UPDATE update_runs SET result = ?, message = ?, changes = ?, ended_at = ? WHERE id = ?`,
		result, msg, string(ch), ts(time.Now()), id)
	m.publish(ctx)
}

// Runs lists update runs (of one stack or all), newest first.
func (m *Manager) Runs(ctx context.Context, stackID string, limit int) ([]Run, error) {
	q := `SELECT id, stack_id, trigger, job_id, services, changes, result, message, started_at, ended_at FROM update_runs`
	args := []any{}
	if stackID != "" {
		q += ` WHERE stack_id = ?`
		args = append(args, stackID)
	}
	rows, err := m.Store.DB.QueryContext(ctx, q+` ORDER BY id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Run{}
	for rows.Next() {
		var r Run
		var svc, ch, started, ended string
		if err := rows.Scan(&r.ID, &r.StackID, &r.Trigger, &r.JobID, &svc, &ch, &r.Result, &r.Message, &started, &ended); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(svc), &r.Services)
		_ = json.Unmarshal([]byte(ch), &r.Changes)
		if r.Services == nil {
			r.Services = []string{}
		}
		if r.Changes == nil {
			r.Changes = []Change{}
		}
		r.StartedAt, r.EndedAt = store.ParseTime(started), parse(ended)
		out = append(out, r)
	}
	return out, rows.Err()
}

// runHooks runs pre-update hooks in the stack's containers.
func (m *Manager) runHooks(ctx context.Context, st stacks.Stack, hooks []Hook, out *jobs.Output) error {
	for _, h := range hooks {
		if h.Type == "volumes" {
			if m.Snapshots == nil {
				return errors.New("volume snapshots are not available")
			}
			out.Printf("▸ snapshot of the stack's volumes before the update")
			if _, err := m.Snapshots.Take(ctx, st, snapshots.Options{Stop: h.Stop, Binds: h.Binds, Keep: h.Keep, Trigger: "update"}, out); err != nil && !errors.Is(err, snapshots.ErrNothing) {
				return fmt.Errorf("volume snapshot: %w", err)
			}
			continue
		}
		var cid string
		for _, sv := range st.Services {
			if sv.Name == h.Service {
				for _, c := range sv.Containers {
					if c.State == "running" {
						cid = c.ID
					}
				}
			}
		}
		if cid == "" {
			return fmt.Errorf("%s hook: %s has no running container", h.Type, h.Service)
		}
		switch h.Type {
		case "pg_dump":
			if err := m.pgDump(ctx, st, h, cid, out); err != nil {
				return err
			}
		case "command":
			out.Printf("▸ hook in %s: %s", h.Service, h.Command)
			code, err := m.Engine.Exec(ctx, cid, []string{"sh", "-c", h.Command}, out, out)
			if err != nil {
				return err
			}
			if code != 0 {
				return fmt.Errorf("command exited with %d", code)
			}
		}
	}
	return nil
}

// pgDump writes pg_dumpall of a postgres service to data_dir/backups/<stack>/<service>-<time>.sql.gz.
func (m *Manager) pgDump(ctx context.Context, st stacks.Stack, h Hook, cid string, out *jobs.Output) error {
	dir := filepath.Join(m.DataDir, "backups", strings.NewReplacer("/", "_", "~", "").Replace(st.ID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	file := filepath.Join(dir, fmt.Sprintf("%s-%s.sql.gz", h.Service, time.Now().Format("20060102-150405")))
	out.Printf("▸ pg_dumpall in %s → %s", h.Service, file)
	f, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(f)
	var stderr bytes.Buffer
	code, err := m.Engine.Exec(ctx, cid, []string{"sh", "-c", `pg_dumpall -U "${POSTGRES_USER:-postgres}"`}, zw, &stderr)
	cerr := zw.Close()
	ferr := f.Close()
	if err == nil && code != 0 {
		err = fmt.Errorf("pg_dumpall exited with %d: %s", code, strings.TrimSpace(stderr.String()))
	}
	if err == nil {
		err = errors.Join(cerr, ferr)
	}
	if err != nil {
		os.Remove(file)
		return err
	}
	if fi, err := os.Stat(file); err == nil {
		out.Printf("  %d KiB written", fi.Size()>>10)
	}
	keep := h.Keep
	if keep <= 0 {
		keep = 5
	}
	old, _ := filepath.Glob(filepath.Join(dir, h.Service+"-*.sql.gz"))
	sort.Strings(old)
	for len(old) > keep {
		os.Remove(old[0])
		old = old[1:]
	}
	return nil
}
