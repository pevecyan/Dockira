// Package updates checks whether newer images exist (by digest, since tags like :latest move),
// runs updates with a rollback point, a health verification and an automatic rollback, and
// applies updates by itself in a maintenance window for stacks whose policy says so.
package updates

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"docker-manager/internal/docker"
	"docker-manager/internal/jobs"
	"docker-manager/internal/notify"
	"docker-manager/internal/registry"
	"docker-manager/internal/snapshots"
	"docker-manager/internal/stacks"
	"docker-manager/internal/store"
)

// States of a service's image.
const (
	UpToDate         = "up_to_date"
	UpdateAvailable  = "update_available"
	PulledNotApplied = "pulled_not_applied"
	LocalBuild       = "local_build"
	BaseUpdated      = "base_updated"
	PinnedDigest     = "pinned_digest"
	Unknown          = "unknown"
	NotChecked       = "not_checked"
)

type Engine interface {
	ImageInspect(ctx context.Context, ref string) (docker.Image, error)
	ImageTag(ctx context.Context, source, target string) error
	ImageRemove(ctx context.Context, ref string) error
	Inspect(ctx context.Context, id string) (docker.Details, error)
	Exec(ctx context.Context, id string, cmd []string, out, errOut io.Writer) (int, error)
}

type Registry interface {
	CheckAll(ctx context.Context, refs []registry.Ref) map[string]registry.Result
	Tags(ctx context.Context, ref registry.Ref) ([]string, error)
}

type StackSource interface {
	Stacks() []stacks.Stack
	Get(id string) (stacks.Stack, bool)
	RefreshContainers(ctx context.Context) error
}

type Composer interface {
	Cmd(ctx context.Context, dir string, files []string, args ...string) *exec.Cmd
}

type Runner interface {
	Submit(spec jobs.Spec) (jobs.Job, error)
	Wait(ctx context.Context, id int64) (jobs.Job, error)
	Active() []jobs.Job
}

type Publisher interface {
	Publish(name string, data any)
}

type Settings struct {
	CheckCron     string `json:"check_cron" doc:"When to look for updates (cron, local time)"`
	DefaultMode   string `json:"default_mode" enum:"manual,notify,auto" doc:"Policy of stacks that don't set one"`
	WindowCron    string `json:"window_cron" doc:"Start of the maintenance window for automatic updates (cron)"`
	WindowMinutes int    `json:"window_minutes" minimum:"10" maximum:"1440"`
	CooldownHours int    `json:"cooldown_hours" minimum:"0" maximum:"720" doc:"A new image must be this old before it's applied automatically"`
	VerifySeconds int    `json:"verify_seconds" minimum:"20" maximum:"900" doc:"How long an updated service has to become healthy"`
	KeepRollbacks int    `json:"keep_rollbacks" minimum:"1" maximum:"10" doc:"Rollback images kept per service"`
	Paused        bool   `json:"paused" doc:"No automatic updates until resumed"`
	MinFreeGiB    int    `json:"min_free_gib" minimum:"0" doc:"Automatic updates need this much free disk"`
}

func DefaultSettings() Settings {
	return Settings{CheckCron: "0 */6 * * *", DefaultMode: "notify", WindowCron: "0 4 * * *", WindowMinutes: 60,
		CooldownHours: 24, VerifySeconds: 90, KeepRollbacks: 2, MinFreeGiB: 2}
}

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

func (s Settings) Validate() error {
	if _, err := cronParser.Parse(s.CheckCron); err != nil {
		return fmt.Errorf("check schedule: %w", err)
	}
	if _, err := cronParser.Parse(s.WindowCron); err != nil {
		return fmt.Errorf("maintenance window: %w", err)
	}
	switch s.DefaultMode {
	case "manual", "notify", "auto":
	default:
		return fmt.Errorf("default mode %q", s.DefaultMode)
	}
	if s.WindowMinutes < 10 || s.VerifySeconds < 20 || s.KeepRollbacks < 1 || s.CooldownHours < 0 {
		return errors.New("window ≥ 10 min, verification ≥ 20 s, keep ≥ 1 rollback, cooldown ≥ 0")
	}
	return nil
}

type Manager struct {
	Store     *store.Store
	Engine    Engine
	Registry  Registry
	Stacks    StackSource
	Compose   Composer
	Jobs      Runner
	Hub       Publisher
	Log       *slog.Logger
	Notify    func(notify.Event)
	DataDir   string // backups of pre-update hooks
	DiskPath  string // free-space check before automatic updates
	SelfID    func() bool
	Snapshots *snapshots.Manager // the "volumes" hook

	mu        sync.Mutex
	checking  bool
	lastCheck time.Time
	nextCheck time.Time
	attempted map[string]time.Time // stack → window start of the last automatic attempt
}

const settingsKey = "updates"

func (m *Manager) Settings(ctx context.Context) Settings {
	s := DefaultSettings()
	_ = m.Store.GetSetting(ctx, settingsKey, &s)
	return s
}

func (m *Manager) SaveSettings(ctx context.Context, s Settings) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if err := m.Store.SetSetting(ctx, settingsKey, s); err != nil {
		return err
	}
	m.mu.Lock()
	m.nextCheck = time.Time{} // recompute
	m.mu.Unlock()
	m.publish(ctx)
	return nil
}

// imageRow is one image_status row.
type imageRow struct {
	Ref, LocalDigest, RemoteDigest, LocalImageID, Error, ErrorKind string
	CheckedAt, RemoteSince, HintsCheckedAt                         time.Time
	HintSame, HintMajor, NotifiedDigest                            string
}

func (m *Manager) rows(ctx context.Context) (map[string]imageRow, error) {
	rs, err := m.Store.DB.QueryContext(ctx, `SELECT ref, local_digest, remote_digest, local_image_id, error, error_kind, checked_at,
		remote_since, hint_same, hint_major, hints_checked_at, notified_digest FROM image_status`)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := map[string]imageRow{}
	for rs.Next() {
		var r imageRow
		var checked, since, hints string
		if err := rs.Scan(&r.Ref, &r.LocalDigest, &r.RemoteDigest, &r.LocalImageID, &r.Error, &r.ErrorKind, &checked, &since,
			&r.HintSame, &r.HintMajor, &hints, &r.NotifiedDigest); err != nil {
			return nil, err
		}
		r.CheckedAt, r.RemoteSince, r.HintsCheckedAt = parse(checked), parse(since), parse(hints)
		out[r.Ref] = r
	}
	return out, rs.Err()
}

func ts(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parse(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// target is one image ref to check and where it comes from.
type target struct {
	raw  string
	ref  registry.Ref
	base bool // base image of a built service
}

// refsOf lists what to check for a stack: service images, and base images of built services.
func refsOf(st stacks.Stack) []target {
	var out []target
	for _, sv := range st.Services {
		if !sv.InFile {
			continue
		}
		if sv.Build {
			for _, raw := range baseImages(sv) {
				if r, err := registry.ParseRef(raw); err == nil && !r.Pinned() {
					out = append(out, target{raw: raw, ref: r, base: true})
				}
			}
			continue
		}
		if r, err := registry.ParseRef(sv.Image); err == nil && !r.Pinned() {
			out = append(out, target{raw: sv.Image, ref: r})
		}
	}
	return out
}

// baseImages reads the FROM lines of a built service's Dockerfile.
func baseImages(sv stacks.Service) []string {
	p := sv.Dockerfile
	if !filepath.IsAbs(p) {
		p = filepath.Join(sv.BuildContext, p)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	refs, _ := registry.BaseImages(string(b), sv.BuildArgs)
	return refs
}

// Check asks the registries about every image (of one stack, or all) and stores the answers.
func (m *Manager) Check(ctx context.Context, stackID string, out io.Writer) error {
	m.mu.Lock()
	if m.checking {
		m.mu.Unlock()
		return errors.New("a check is already running")
	}
	m.checking = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.checking = false
		if stackID == "" {
			m.lastCheck = time.Now()
		}
		m.mu.Unlock()
	}()
	if out == nil {
		out = io.Discard
	}

	var targets []target
	seen := map[string]bool{}
	for _, st := range m.Stacks.Stacks() {
		if st.Orphaned || (stackID != "" && st.ID != stackID) {
			continue
		}
		for _, t := range refsOf(st) {
			if !seen[t.raw] {
				seen[t.raw] = true
				targets = append(targets, t)
			}
		}
	}
	if len(targets) == 0 {
		fmt.Fprintln(out, "no images to check")
		m.publish(ctx)
		return nil
	}
	// images that exist here without a registry digest were built or loaded locally (e.g.
	// docker-manager's own): no registry to ask
	local := map[string]docker.Image{}
	var refs []registry.Ref
	for _, t := range targets {
		img, err := m.Engine.ImageInspect(ctx, t.raw)
		if err == nil {
			local[t.raw] = img
		}
		if err == nil && len(img.RepoDigests) == 0 {
			continue
		}
		refs = append(refs, t.ref)
	}
	fmt.Fprintf(out, "▸ checking %d images\n", len(refs))
	results := m.Registry.CheckAll(ctx, refs)
	old, _ := m.rows(ctx)
	now := time.Now()
	var errs int
	for _, t := range targets {
		res, asked := results[t.ref.String()]
		row := old[t.raw]
		row.Ref = t.raw
		row.CheckedAt = now
		row.Error, row.ErrorKind = "", ""
		if !asked {
			row.RemoteDigest = ""
			fmt.Fprintf(out, "  %s: local image (not from a registry)\n", t.raw)
		} else if res.Err != nil {
			errs++
			row.Error, row.ErrorKind = res.Err.Error(), registry.Classify(res.Err)
			fmt.Fprintf(out, "  %s: %s (%s)\n", t.raw, row.ErrorKind, row.Error)
		} else {
			if res.Digest != row.RemoteDigest {
				row.RemoteSince = now
			}
			row.RemoteDigest = res.Digest
		}
		prevLocal := row.LocalDigest
		row.LocalDigest, row.LocalImageID = "", ""
		if img, ok := local[t.raw]; ok {
			row.LocalDigest = registry.MatchRepoDigest(t.ref, img.RepoDigests)
			row.LocalImageID = img.ID
		}
		// BuildKit doesn't keep base images as images: remember the digest the registry had
		// when we first looked (reset after each rebuild) and report when it moves on
		if t.base && row.LocalDigest == "" && asked && res.Err == nil {
			row.LocalDigest = prevLocal
			if row.LocalDigest == "" {
				row.LocalDigest = res.Digest
			}
		}
		if asked && res.Err == nil {
			state := "up to date"
			if row.LocalDigest == "" {
				state = "not pulled from this registry yet"
			} else if row.LocalDigest != row.RemoteDigest {
				state = "UPDATE AVAILABLE"
			}
			fmt.Fprintf(out, "  %s: %s\n", t.raw, state)
		}
		if asked && !t.base && now.Sub(row.HintsCheckedAt) > 24*time.Hour && versioned(t.ref.Tag) {
			if tags, err := m.Registry.Tags(ctx, t.ref); err == nil {
				row.HintSame, row.HintMajor = registry.NewerTags(t.ref.Tag, tags)
				row.HintsCheckedAt = now
			}
		}
		if _, err := m.Store.DB.ExecContext(ctx, `INSERT INTO image_status (ref, local_digest, remote_digest, local_image_id, error, error_kind,
			checked_at, remote_since, hint_same, hint_major, hints_checked_at, notified_digest) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (ref) DO UPDATE SET local_digest = excluded.local_digest, remote_digest = excluded.remote_digest,
			local_image_id = excluded.local_image_id, error = excluded.error, error_kind = excluded.error_kind, checked_at = excluded.checked_at,
			remote_since = excluded.remote_since, hint_same = excluded.hint_same, hint_major = excluded.hint_major,
			hints_checked_at = excluded.hints_checked_at`,
			row.Ref, row.LocalDigest, row.RemoteDigest, row.LocalImageID, row.Error, row.ErrorKind, ts(row.CheckedAt), ts(row.RemoteSince),
			row.HintSame, row.HintMajor, ts(row.HintsCheckedAt), row.NotifiedDigest); err != nil {
			return err
		}
	}
	m.notifyAvailable(ctx)
	if errs > 0 && errs == len(refs) && m.Notify != nil && stackID == "" {
		m.Notify(notify.Event{Type: "system.registry_errors", Severity: notify.Warning, Title: "Update check failed",
			Message: fmt.Sprintf("None of the %d images could be checked: %s", len(refs), results[refs[0].String()].Err), Time: now})
	}
	m.publish(ctx)
	return nil
}

var versionTag = regexp.MustCompile(`^v?\d+(\.\d+)*([-_.][a-z0-9.]+)?$`)

func versioned(tag string) bool { return versionTag.MatchString(tag) }

// ServiceUpdate is the update state of one service.
type ServiceUpdate struct {
	StackID      string      `json:"stack_id"`
	Service      string      `json:"service"`
	Image        string      `json:"image"`
	State        string      `json:"state" enum:"up_to_date,update_available,pulled_not_applied,local_build,base_updated,pinned_digest,unknown,not_checked"`
	LocalDigest  string      `json:"local_digest,omitempty"`
	RemoteDigest string      `json:"remote_digest,omitempty"`
	Error        string      `json:"error,omitempty"`
	ErrorKind    string      `json:"error_kind,omitempty"`
	CheckedAt    time.Time   `json:"checked_at,omitzero"`
	RemoteSince  time.Time   `json:"remote_since,omitzero" doc:"When the registry's current image was first seen"`
	HintSame     string      `json:"hint_same,omitempty" doc:"Newer tag in the same version series"`
	HintMajor    string      `json:"hint_major,omitempty" doc:"Newer major version"`
	Base         []BaseImage `json:"base,omitempty"`
	Build        bool        `json:"build"`
	Stateful     bool        `json:"stateful"`
	Mode         string      `json:"mode" enum:"manual,notify,auto"`
	Auto         AutoInfo    `json:"auto"`
}

type BaseImage struct {
	Image        string `json:"image"`
	State        string `json:"state"`
	LocalDigest  string `json:"local_digest,omitempty"`
	RemoteDigest string `json:"remote_digest,omitempty"`
	Error        string `json:"error,omitempty"`
}

// AutoInfo says whether and when the service would be updated automatically.
type AutoInfo struct {
	Eligible bool      `json:"eligible"`
	At       time.Time `json:"at,omitzero" doc:"Next maintenance window it would be applied in"`
	Blocked  string    `json:"blocked,omitempty" doc:"Why it won't be applied automatically"`
}

// Services returns the update state of every service (or one stack's).
func (m *Manager) Services(ctx context.Context, stackID string) ([]ServiceUpdate, error) {
	rows, err := m.rows(ctx)
	if err != nil {
		return nil, err
	}
	pols, _ := m.policies(ctx)
	set := m.Settings(ctx)
	bad := m.badDigests(ctx)
	out := []ServiceUpdate{}
	for _, st := range m.Stacks.Stacks() {
		if st.Orphaned || (stackID != "" && st.ID != stackID) {
			continue
		}
		for _, sv := range st.Services {
			if !sv.InFile {
				continue
			}
			u := serviceState(st, sv, rows)
			u.Mode = effective(pols, set, st.ID, sv.Name).Mode
			u.Auto = m.autoInfo(st, sv, u, effective(pols, set, st.ID, sv.Name), set, bad)
			out = append(out, u)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StackID != out[j].StackID {
			return out[i].StackID < out[j].StackID
		}
		return out[i].Service < out[j].Service
	})
	return out, nil
}

func serviceState(st stacks.Stack, sv stacks.Service, rows map[string]imageRow) ServiceUpdate {
	u := ServiceUpdate{StackID: st.ID, Service: sv.Name, Image: sv.Image, Build: sv.Build, Stateful: sv.Stateful}
	if sv.Build {
		u.State = LocalBuild
		for _, raw := range baseImages(sv) {
			b := BaseImage{Image: raw, State: NotChecked}
			if ref, err := registry.ParseRef(raw); err == nil && ref.Pinned() {
				b.State = PinnedDigest
			} else if r, ok := rows[raw]; ok {
				b.LocalDigest, b.RemoteDigest, b.Error = r.LocalDigest, r.RemoteDigest, r.Error
				switch {
				case r.Error != "":
					b.State = Unknown
				case r.LocalDigest == "":
					b.State = Unknown
					b.Error = "not pulled locally yet"
				case r.LocalDigest != r.RemoteDigest:
					b.State = BaseUpdated
					u.State = BaseUpdated
					if u.RemoteSince.IsZero() || r.RemoteSince.After(u.RemoteSince) {
						u.RemoteSince = r.RemoteSince
					}
				default:
					b.State = UpToDate
				}
				if r.CheckedAt.After(u.CheckedAt) {
					u.CheckedAt = r.CheckedAt
				}
			}
			u.Base = append(u.Base, b)
		}
		return u
	}
	if ref, err := registry.ParseRef(sv.Image); err == nil && ref.Pinned() {
		u.State = PinnedDigest
		return u
	}
	r, ok := rows[sv.Image]
	if !ok {
		u.State = NotChecked
		return u
	}
	u.LocalDigest, u.RemoteDigest, u.Error, u.ErrorKind = r.LocalDigest, r.RemoteDigest, r.Error, r.ErrorKind
	u.CheckedAt, u.RemoteSince, u.HintSame, u.HintMajor = r.CheckedAt, r.RemoteSince, r.HintSame, r.HintMajor
	switch {
	case r.Error != "" && r.ErrorKind == registry.ErrAuth && r.LocalImageID != "" && !strings.Contains(sv.Image, "/"):
		// an "official image" name Docker Hub doesn't know (401): built or loaded here
		u.State = LocalBuild
		u.Error = "a local image (built or loaded here, not on Docker Hub)"
	case r.Error != "":
		u.State = Unknown
	case r.RemoteDigest == "" && r.LocalImageID != "":
		u.State = LocalBuild
		u.Error = "a local image (built or loaded here, not pulled from a registry)"
	case r.LocalDigest == "" && len(sv.Containers) == 0:
		u.State = UpdateAvailable // never pulled: `up` would pull it
	case r.LocalDigest == "":
		u.State = Unknown
		u.Error = "the local image has no digest from this registry (built or loaded locally?)"
	case r.LocalDigest != r.RemoteDigest:
		u.State = UpdateAvailable
	default:
		u.State = UpToDate
		for _, c := range sv.Containers {
			if r.LocalImageID != "" && c.ImageID != r.LocalImageID {
				u.State = PulledNotApplied
			}
		}
	}
	return u
}

// Summary is what the dashboard card shows.
type Summary struct {
	Available int       `json:"available" doc:"Services with an update or a pulled image not applied yet"`
	Errors    int       `json:"errors"`
	Checked   int       `json:"checked"`
	Total     int       `json:"total"`
	LastCheck time.Time `json:"last_check,omitzero"`
	NextCheck time.Time `json:"next_check,omitzero"`
	Window    time.Time `json:"window,omitzero" doc:"Start of the next maintenance window"`
	Checking  bool      `json:"checking"`
	Paused    bool      `json:"paused"`
	AutoNext  int       `json:"auto_next" doc:"Services that would be updated in the next window"`
}

func (m *Manager) Summary(ctx context.Context) Summary {
	set := m.Settings(ctx)
	svcs, _ := m.Services(ctx, "")
	m.mu.Lock()
	sum := Summary{Checking: m.checking, LastCheck: m.lastCheck, NextCheck: m.nextCheck, Paused: set.Paused}
	m.mu.Unlock()
	for _, s := range svcs {
		sum.Total++
		if s.State != NotChecked {
			sum.Checked++
		}
		if s.CheckedAt.After(sum.LastCheck) {
			sum.LastCheck = s.CheckedAt
		}
		switch s.State {
		case UpdateAvailable, PulledNotApplied, BaseUpdated:
			sum.Available++
		case Unknown:
			sum.Errors++
		}
		if s.Auto.Eligible && s.Auto.Blocked == "" {
			sum.AutoNext++
		}
	}
	if sched, err := cronParser.Parse(set.WindowCron); err == nil {
		sum.Window = sched.Next(time.Now())
		if inWindow(sched, set.WindowMinutes, time.Now()) {
			sum.Window = time.Now()
		}
	}
	if sum.NextCheck.IsZero() {
		if sched, err := cronParser.Parse(set.CheckCron); err == nil {
			sum.NextCheck = sched.Next(time.Now())
		}
	}
	return sum
}

func (m *Manager) publish(ctx context.Context) {
	if m.Hub != nil {
		m.Hub.Publish("updates", m.Summary(ctx))
	}
}

// notifyAvailable sends one "update available" message per check for updates not announced yet,
// for stacks whose policy is notify or auto.
func (m *Manager) notifyAvailable(ctx context.Context) {
	if m.Notify == nil {
		return
	}
	svcs, err := m.Services(ctx, "")
	if err != nil {
		return
	}
	rows, _ := m.rows(ctx)
	var lines []string
	var refs []string
	for _, s := range svcs {
		if s.Mode == "manual" {
			continue
		}
		ref, digest := s.Image, s.RemoteDigest
		if s.State == BaseUpdated {
			for _, b := range s.Base {
				if b.State == BaseUpdated {
					ref, digest = b.Image, b.RemoteDigest
				}
			}
		} else if s.State != UpdateAvailable {
			continue
		}
		if rows[ref].NotifiedDigest == digest || digest == "" {
			continue
		}
		what := "new image " + ref
		if s.State == BaseUpdated {
			what = "base image " + ref + " changed (rebuild)"
		}
		if s.Mode == "auto" && s.Auto.Eligible {
			what += ", applies automatically"
		}
		lines = append(lines, fmt.Sprintf("• %s/%s: %s", s.StackID, s.Service, what))
		refs = append(refs, ref, digest)
	}
	if len(lines) == 0 {
		return
	}
	m.Notify(notify.Event{Type: "update.available", Severity: notify.Info, Title: fmt.Sprintf("%d update(s) available", len(lines)),
		Message: strings.Join(lines, "\n"), URL: "/updates", Time: time.Now()})
	for i := 0; i < len(refs); i += 2 {
		_, _ = m.Store.DB.ExecContext(ctx, `UPDATE image_status SET notified_digest = ? WHERE ref = ?`, refs[i+1], refs[i])
	}
}

func (m *Manager) badDigests(ctx context.Context) map[string]bool {
	out := map[string]bool{}
	rs, err := m.Store.DB.QueryContext(ctx, `SELECT ref, digest FROM bad_digests`)
	if err != nil {
		return out
	}
	defer rs.Close()
	for rs.Next() {
		var ref, d string
		if rs.Scan(&ref, &d) == nil {
			out[ref+"@"+d] = true
		}
	}
	return out
}

func (m *Manager) markBad(ctx context.Context, ref, digest, reason string) {
	if digest == "" {
		return
	}
	_, _ = m.Store.DB.ExecContext(ctx, `INSERT OR REPLACE INTO bad_digests (ref, digest, reason) VALUES (?, ?, ?)`, ref, digest, reason)
}
