// Package stacks finds the compose stacks under apps_root and joins them with the containers
// Docker reports. A stack is a folder with a compose file; its containers are found by the
// labels compose puts on them (working dir first, project name as a fallback). Containers whose
// folder is gone show up as orphaned stacks, containers without compose labels as standalone.
package stacks

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"docker-manager/internal/compose"
	"docker-manager/internal/docker"
)

type Composer interface {
	Config(ctx context.Context, dir string, files []string) (*compose.Project, error)
}

type Lister interface {
	Containers(ctx context.Context) ([]docker.Container, error)
}

type Stack struct {
	ID       string    `json:"id" doc:"Folder relative to apps_root (or ~project for orphans outside it)"`
	Name     string    `json:"name" doc:"Compose project name"`
	Dir      string    `json:"dir"`
	Files    []string  `json:"files" doc:"Compose files, main file first"`
	EnvFile  bool      `json:"env_file" doc:"The folder has a .env file"`
	Status   string    `json:"status" enum:"running,partial,stopped,down,unhealthy,starting,restarting,invalid,orphaned"`
	Error    string    `json:"error,omitempty"`
	Orphaned bool      `json:"orphaned" doc:"Containers without a compose folder under apps_root"`
	External bool      `json:"external" doc:"The compose project lives outside apps_root"`
	Self     bool      `json:"self" doc:"This is the manager's own stack"`
	Running  int       `json:"running"`
	Total    int       `json:"total"`
	Services []Service `json:"services"`
}

type Service struct {
	Name        string             `json:"name"`
	Image       string             `json:"image" doc:"Configured image, or the name compose gives a built image"`
	Build       bool               `json:"build"`
	Healthcheck bool               `json:"healthcheck"`
	Stateful    bool               `json:"stateful" doc:"Looks like a database (auto-update opt-out by default)"`
	InFile      bool               `json:"in_file" doc:"Defined in the compose file (false: only a leftover container)"`
	Status      string             `json:"status"`
	Containers  []docker.Container `json:"containers"`

	BuildContext string            `json:"build_context,omitempty"`
	Dockerfile   string            `json:"dockerfile,omitempty"`
	BuildArgs    map[string]string `json:"-"`
}

type Manager struct {
	Root    string
	Depth   int
	Compose Composer
	Docker  Lister
	Log     *slog.Logger
	SelfID  string // this process's container ID prefix (its hostname inside a container)

	mu         sync.RWMutex
	disk       []diskStack
	configs    map[string]configEntry // by dir
	containers []docker.Container
	stacks     []Stack
	byID       map[string]int
	standalone []docker.Container
	scanned    time.Time
	dockerErr  error
}

type diskStack struct {
	id, dir string
	files   []string
	envFile bool
}

type configEntry struct {
	fingerprint string
	project     *compose.Project
	err         error
}

// Scan walks apps_root again, reloads changed compose projects and lists containers.
func (m *Manager) Scan(ctx context.Context) error {
	disk, err := m.walk()
	if err != nil {
		return fmt.Errorf("scan %s: %w", m.Root, err)
	}
	configs := m.loadConfigs(ctx, disk)
	containers, cerr := m.Docker.Containers(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.disk, m.configs, m.scanned = disk, configs, time.Now()
	m.dockerErr = cerr
	if cerr == nil {
		m.containers = containers
	}
	m.build()
	return cerr
}

// RefreshContainers re-reads containers only (after Docker events).
func (m *Manager) RefreshContainers(ctx context.Context) error {
	containers, err := m.Docker.Containers(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dockerErr = err
	if err != nil {
		return err
	}
	m.containers = containers
	m.build()
	return nil
}

// Invalidate forgets a stack's cached project (after its files were edited); the next Scan
// reloads it even if the modification time didn't change.
func (m *Manager) Invalidate(dir string) {
	m.mu.Lock()
	delete(m.configs, dir)
	m.mu.Unlock()
}

func (m *Manager) Stacks() []Stack {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]Stack(nil), m.stacks...)
}

func (m *Manager) Get(id string) (Stack, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	i, ok := m.byID[id]
	if !ok {
		return Stack{}, false
	}
	return m.stacks[i], true
}

func (m *Manager) Standalone() []docker.Container {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]docker.Container(nil), m.standalone...)
}

func (m *Manager) Containers() []docker.Container {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]docker.Container(nil), m.containers...)
}

// DockerError is the last error listing containers (nil when Docker answered).
func (m *Manager) DockerError() error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.dockerErr
}

func (m *Manager) Scanned() time.Time {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.scanned
}

// walk finds stack folders: up to Depth levels below Root, not descending into a folder once it
// is a stack (its subfolders are its data), skipping dot folders, node_modules and .dmignore entries.
func (m *Manager) walk() ([]diskStack, error) {
	root := filepath.Clean(m.Root)
	if st, err := os.Stat(root); err != nil {
		return nil, err
	} else if !st.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", root)
	}
	ignore := readIgnore(filepath.Join(root, ".dmignore"))
	var out []diskStack
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root {
				return err
			}
			return fs.SkipDir // unreadable folder: skip it, keep scanning
		}
		if !d.IsDir() || p == root {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		name := d.Name()
		if strings.HasPrefix(name, ".") || name == "node_modules" || ignore[rel] || ignore[name] {
			return fs.SkipDir
		}
		if files := compose.FindFiles(p); files != nil {
			out = append(out, diskStack{id: rel, dir: p, files: files, envFile: isFile(filepath.Join(p, ".env"))})
			return fs.SkipDir
		}
		if strings.Count(rel, "/")+1 >= m.depth() {
			return fs.SkipDir
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out, err
}

func (m *Manager) depth() int {
	if m.Depth < 1 {
		return 3
	}
	return m.Depth
}

func readIgnore(file string) map[string]bool {
	out := map[string]bool{}
	f, err := os.Open(file)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := strings.Trim(strings.TrimSpace(sc.Text()), "/")
		if l != "" && !strings.HasPrefix(l, "#") {
			out[l] = true
		}
	}
	return out
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// loadConfigs runs `compose config` for stacks whose files changed (4 at a time).
func (m *Manager) loadConfigs(ctx context.Context, disk []diskStack) map[string]configEntry {
	m.mu.RLock()
	old := m.configs
	m.mu.RUnlock()
	out := make(map[string]configEntry, len(disk))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, s := range disk {
		fp := fingerprint(s)
		if e, ok := old[s.dir]; ok && e.fingerprint == fp {
			out[s.dir] = e
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			p, err := m.Compose.Config(ctx, s.dir, s.files)
			if err != nil && m.Log != nil {
				m.Log.Warn("compose config failed", "stack", s.id, "err", err)
			}
			mu.Lock()
			out[s.dir] = configEntry{fingerprint: fp, project: p, err: err}
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

func fingerprint(s diskStack) string {
	var b strings.Builder
	for _, f := range append(append([]string{}, s.files...), filepath.Join(s.dir, ".env")) {
		if st, err := os.Stat(f); err == nil {
			fmt.Fprintf(&b, "%s|%d|%d;", f, st.ModTime().UnixNano(), st.Size())
		}
	}
	return b.String()
}

// build joins disk stacks, compose projects and containers. Caller holds m.mu.
func (m *Manager) build() {
	byDir := map[string][]docker.Container{}
	byProject := map[string][]docker.Container{}
	var standalone []docker.Container
	for _, c := range m.containers {
		switch {
		case c.Project == "":
			standalone = append(standalone, c)
		case c.WorkingDir != "":
			byDir[filepath.Clean(c.WorkingDir)] = append(byDir[filepath.Clean(c.WorkingDir)], c)
		default:
			byProject[c.Project] = append(byProject[c.Project], c)
		}
	}

	var stacks []Stack
	for _, d := range m.disk {
		cfg := m.configs[d.dir]
		name := defaultProjectName(d.dir)
		if cfg.project != nil && cfg.project.Name != "" {
			name = cfg.project.Name
		}
		cs := byDir[d.dir]
		delete(byDir, d.dir)
		cs = append(cs, byProject[name]...) // containers created without a working_dir label
		delete(byProject, name)
		s := Stack{ID: d.id, Name: name, Dir: d.dir, Files: d.files, EnvFile: d.envFile}
		if cfg.err != nil {
			s.Error = cfg.err.Error()
		}
		s.Services = services(cfg.project, cs)
		finish(&s, m.SelfID, cfg.err != nil)
		if s.Files == nil {
			s.Files = []string{}
		}
		stacks = append(stacks, s)
	}

	// containers whose compose folder is not (or no longer) a stack under apps_root
	root := filepath.Clean(m.Root)
	for dir, cs := range byDir {
		id := "~" + cs[0].Project
		inside := false
		if rel, err := filepath.Rel(root, dir); err == nil && !strings.HasPrefix(rel, "..") {
			id, inside = filepath.ToSlash(rel), true
		}
		cs = append(cs, byProject[cs[0].Project]...)
		byProject[cs[0].Project] = nil // merged here
		s := Stack{ID: id, Name: cs[0].Project, Dir: dir, Orphaned: true, Files: []string{}}
		switch {
		case inside:
			s.Error = "no compose file found in " + dir + " (moved or deleted?)"
		case compose.FindFiles(dir) != nil:
			s.External = true
			s.Error = "compose project outside apps_root: " + dir
		default:
			s.Error = "no compose file found in " + dir
		}
		s.Services = services(nil, cs)
		finish(&s, m.SelfID, false)
		stacks = append(stacks, s)
	}
	for project, cs := range byProject {
		if len(cs) == 0 {
			continue
		}
		s := Stack{ID: "~" + project, Name: project, Orphaned: true, Files: []string{}, Error: "containers have no compose working directory label"}
		s.Services = services(nil, cs)
		finish(&s, m.SelfID, false)
		stacks = append(stacks, s)
	}
	sort.Slice(stacks, func(i, j int) bool { return stacks[i].ID < stacks[j].ID })
	m.stacks = stacks
	m.byID = make(map[string]int, len(stacks))
	for i, s := range stacks {
		m.byID[s.ID] = i
	}
	m.standalone = standalone
}

func services(p *compose.Project, cs []docker.Container) []Service {
	bySvc := map[string][]docker.Container{}
	for _, c := range cs {
		if !c.Oneoff {
			bySvc[c.Service] = append(bySvc[c.Service], c)
		}
	}
	var out []Service
	if p != nil {
		for name, sv := range p.Services {
			s := Service{Name: name, Image: p.ImageName(name), Build: sv.Build != nil, InFile: true, Containers: []docker.Container{},
				Healthcheck: sv.Healthcheck != nil && !sv.Healthcheck.Disable && len(sv.Healthcheck.Test) > 0 && sv.Healthcheck.Test[0] != "NONE",
			}
			if c := bySvc[name]; c != nil {
				s.Containers = c
			}
			if b := sv.Build; b != nil {
				s.BuildContext, s.Dockerfile, s.BuildArgs = b.Context, b.Dockerfile, b.BuildArgs()
				if s.Dockerfile == "" {
					s.Dockerfile = "Dockerfile"
				}
			}
			delete(bySvc, name)
			out = append(out, s)
		}
	}
	for name, cs := range bySvc {
		out = append(out, Service{Name: name, Image: cs[0].Image, Containers: cs})
	}
	for i := range out {
		s := &out[i]
		sort.Slice(s.Containers, func(a, b int) bool { return containerNumber(s.Containers[a]) < containerNumber(s.Containers[b]) })
		if !s.Healthcheck {
			for _, c := range s.Containers {
				if c.Health != "" {
					s.Healthcheck = true // HEALTHCHECK from the image itself
				}
			}
		}
		s.Stateful = IsStateful(s.Image)
		s.Status = Aggregate(s.Containers)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if out == nil {
		out = []Service{}
	}
	return out
}

func containerNumber(c docker.Container) int {
	n, _ := strconv.Atoi(c.Labels[docker.LabelNumber])
	return n
}

func finish(s *Stack, selfID string, invalid bool) {
	var all []docker.Container
	for _, sv := range s.Services {
		all = append(all, sv.Containers...)
	}
	s.Total = len(all)
	for _, c := range all {
		if c.State == "running" {
			s.Running++
		}
		if selfID != "" && strings.HasPrefix(c.ID, selfID) {
			s.Self = true
		}
	}
	s.Status = Aggregate(all)
	switch {
	case invalid && s.Total == 0:
		s.Status = "invalid"
	case s.Orphaned && s.Total == 0:
		s.Status = "orphaned"
	}
}

// Aggregate summarises containers into one status (compose `run` containers excluded by caller).
func Aggregate(cs []docker.Container) string {
	if len(cs) == 0 {
		return "down"
	}
	running, restarting, unhealthy, starting := 0, 0, 0, 0
	for _, c := range cs {
		switch c.State {
		case "running":
			running++
		case "restarting":
			restarting++
		}
		if c.State != "running" {
			continue // a stopped container still reports its last health result
		}
		switch c.Health {
		case "unhealthy":
			unhealthy++
		case "starting":
			starting++
		}
	}
	switch {
	case restarting > 0:
		return "restarting"
	case unhealthy > 0:
		return "unhealthy"
	case running == len(cs) && starting > 0:
		return "starting"
	case running == len(cs):
		return "running"
	case running > 0:
		return "partial"
	default:
		return "stopped"
	}
}

// defaultProjectName is compose's name for a project without `name:`: the folder name,
// lowercased, keeping [a-z0-9_-].
func defaultProjectName(dir string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(filepath.Base(dir)) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	return strings.TrimLeft(b.String(), "_-")
}

var statefulImages = map[string]bool{
	"postgres": true, "postgis": true, "timescaledb": true, "mysql": true, "mariadb": true, "mongo": true, "mongodb": true,
	"redis": true, "valkey": true, "keydb": true, "dragonfly": true, "elasticsearch": true, "opensearch": true, "influxdb": true,
	"clickhouse-server": true, "couchdb": true, "cassandra": true, "rabbitmq": true, "etcd": true, "minio": true,
}

// IsStateful guesses whether an image holds data whose format an upgrade may change (databases).
func IsStateful(image string) bool {
	ref := image
	if i := strings.LastIndex(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		ref = ref[:i]
	}
	return statefulImages[path.Base(ref)]
}
