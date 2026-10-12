// Package hoststats reads host CPU/RAM/disk figures from /proc and per-container CPU/RAM from
// the cgroup v2 tree. The server runs in a container, so both are normally the host's trees
// mounted read-only at /host/proc and /host/cgroup; reading files directly avoids a Docker API
// stats stream per container, which is slow (each call blocks for a sampling interval).
package hoststats

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Host struct {
	Time         time.Time  `json:"time"`
	Cores        int        `json:"cores"`
	CPUPct       float64    `json:"cpu_pct"` // all cores, 0–100, since the previous Host() call (0 on the first)
	Load         [3]float64 `json:"load"`
	MemTotal     uint64     `json:"mem_total"` // bytes
	MemUsed      uint64     `json:"mem_used"`  // total - available
	MemAvailable uint64     `json:"mem_available"`
	SwapTotal    uint64     `json:"swap_total"`
	SwapUsed     uint64     `json:"swap_used"`
	UptimeSec    float64    `json:"uptime_sec"`
	DiskPath     string     `json:"disk_path"`
	DiskTotal    uint64     `json:"disk_total"`
	DiskUsed     uint64     `json:"disk_used"`
	DiskFree     uint64     `json:"disk_free"` // available to unprivileged users (Bavail)
}

type Container struct {
	ID       string  `json:"id"`
	CPUPct   float64 `json:"cpu_pct"`   // share of ALL host cores, 0–100 (like the host figure), delta-based
	MemBytes uint64  `json:"mem_bytes"` // memory.current minus inactive_file (what docker stats shows as usage)
	MemLimit uint64  `json:"mem_limit"` // memory.max, 0 = unlimited
	PIDs     uint64  `json:"pids"`
}

type Options struct {
	ProcDir   string           // default: "/host/proc" if it exists, else "/proc"
	CgroupDir string           // default: "/host/cgroup" if it exists, else "/sys/fs/cgroup"
	DiskPath  string           // path whose filesystem is reported (apps_root); empty = no disk figures
	Now       func() time.Time // tests
}

// searchEvery bounds how often the fallback tree walk runs while some id stays unresolved
// (e.g. a container that has just stopped and is still being asked for).
const searchEvery = 60 * time.Second

// searchDepth is deep enough for e.g. kubepods-ish or nested-slice layouts without walking
// the whole tree.
const searchDepth = 4

type cpuTimes struct{ total, idle uint64 }

type usage struct {
	usec uint64
	at   time.Time
}

// Sampler keeps the previous samples that CPU percentages are computed against. It is safe
// for concurrent use, but concurrent callers share the deltas (each call resets the baseline).
type Sampler struct {
	o Options

	mu       sync.Mutex
	prevCPU  cpuTimes
	havePrev bool
	cores    int
	paths    map[string]string // container id → cgroup dir
	prev     map[string]usage  // container id → last usage_usec
	prevRoot usage             // system-container mode: last usage_usec of the cgroup root
	dirs     []string          // fallback search result
	searchAt time.Time
}

func New(o Options) *Sampler {
	if o.ProcDir == "" {
		o.ProcDir = firstDir("/host/proc", "/proc")
	}
	if o.CgroupDir == "" {
		o.CgroupDir = firstDir("/host/cgroup", "/sys/fs/cgroup")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Sampler{o: o, paths: map[string]string{}, prev: map[string]usage{}}
}

func firstDir(preferred, fallback string) string {
	if fi, err := os.Stat(preferred); err == nil && fi.IsDir() {
		return preferred
	}
	return fallback
}

// Host samples the host. Files that cannot be read leave their fields zero and are reported in
// the (joined) error, so callers can still show what was read.
func (s *Sampler) Host() (Host, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := Host{Time: s.o.Now(), DiskPath: s.o.DiskPath}
	var errs []error

	if cpu, cores, err := s.readStat(); err != nil {
		errs = append(errs, err)
	} else {
		if s.havePrev && cpu.total > s.prevCPU.total {
			dt := cpu.total - s.prevCPU.total
			di := min(cpu.idle-min(cpu.idle, s.prevCPU.idle), dt)
			h.CPUPct = float64(dt-di) / float64(dt) * 100
		}
		s.prevCPU, s.havePrev = cpu, true
		s.cores = cores
	}
	h.Cores = s.coresLocked()

	if m, err := s.readMeminfo(); err != nil {
		errs = append(errs, err)
	} else {
		h.MemTotal, h.MemAvailable = m["MemTotal"], m["MemAvailable"]
		h.MemUsed = h.MemTotal - min(h.MemAvailable, h.MemTotal)
		h.SwapTotal = m["SwapTotal"]
		h.SwapUsed = h.SwapTotal - min(m["SwapFree"], h.SwapTotal)
	}

	if f, err := s.procFields("loadavg"); err != nil {
		errs = append(errs, err)
	} else {
		for i := 0; i < 3 && i < len(f); i++ {
			h.Load[i], _ = strconv.ParseFloat(f[i], 64)
		}
	}

	if f, err := s.procFields("uptime"); err != nil {
		errs = append(errs, err)
	} else if len(f) > 0 {
		h.UptimeSec, _ = strconv.ParseFloat(f[0], 64)
	}

	// Inside a system container (LXC on Proxmox, …) /proc/meminfo and /proc/stat are served
	// by lxcfs for the *reading* process's cgroup, i.e. they describe docker-manager's own
	// container. The cgroup root we see then belongs to the system container and has the real
	// figures: use them for used memory and CPU.
	if cur, err := readUint(filepath.Join(s.o.CgroupDir, "memory.current")); err == nil && h.MemTotal > 0 {
		st := readKV(filepath.Join(s.o.CgroupDir, "memory.stat"))
		used := cur - min(st["file"], cur)
		h.MemUsed = min(used, h.MemTotal)
		h.MemAvailable = h.MemTotal - h.MemUsed
		if sw, err := readUint(filepath.Join(s.o.CgroupDir, "memory.swap.current")); err == nil {
			h.SwapUsed = min(sw, h.SwapTotal)
		}
		if cs := readKV(filepath.Join(s.o.CgroupDir, "cpu.stat")); cs["usage_usec"] > 0 {
			now := s.o.Now()
			h.CPUPct = 0
			if !s.prevRoot.at.IsZero() && cs["usage_usec"] >= s.prevRoot.usec && now.After(s.prevRoot.at) && h.Cores > 0 {
				wall := float64(now.Sub(s.prevRoot.at).Microseconds())
				h.CPUPct = min(100, float64(cs["usage_usec"]-s.prevRoot.usec)/(wall*float64(h.Cores))*100)
			}
			s.prevRoot = usage{usec: cs["usage_usec"], at: now}
		}
	}

	if s.o.DiskPath != "" {
		var st syscall.Statfs_t
		if err := syscall.Statfs(s.o.DiskPath, &st); err != nil {
			errs = append(errs, fmt.Errorf("statfs %s: %w", s.o.DiskPath, err))
		} else {
			bs := uint64(st.Bsize)
			h.DiskTotal = st.Blocks * bs
			h.DiskFree = st.Bavail * bs
			// Used counts the root-reserved blocks as free, matching df.
			h.DiskUsed = (st.Blocks - st.Bfree) * bs
		}
	}
	return h, errors.Join(errs...)
}

// readStat parses the aggregate "cpu" line and counts the "cpuN" lines of /proc/stat.
func (s *Sampler) readStat() (cpuTimes, int, error) {
	b, err := os.ReadFile(filepath.Join(s.o.ProcDir, "stat"))
	if err != nil {
		return cpuTimes{}, 0, err
	}
	var c cpuTimes
	found, cores := false, 0
	for line := range bytes.Lines(b) {
		f := strings.Fields(string(line))
		if len(f) == 0 || !strings.HasPrefix(f[0], "cpu") {
			continue
		}
		if f[0] != "cpu" {
			cores++
			continue
		}
		// user nice system idle iowait irq softirq steal; guest/guest_nice are already
		// counted in user/nice, so adding them would count that time twice.
		for i := 1; i <= 8 && i < len(f); i++ {
			v, _ := strconv.ParseUint(f[i], 10, 64)
			c.total += v
			if i == 4 || i == 5 {
				c.idle += v
			}
		}
		found = true
	}
	if !found {
		return c, cores, fmt.Errorf("%s/stat: no cpu line", s.o.ProcDir)
	}
	return c, cores, nil
}

func (s *Sampler) coresLocked() int {
	if s.cores > 0 {
		return s.cores
	}
	if _, cores, err := s.readStat(); err == nil && cores > 0 {
		s.cores = cores
		return cores
	}
	return runtime.NumCPU()
}

// readMeminfo returns /proc/meminfo values in bytes.
func (s *Sampler) readMeminfo() (map[string]uint64, error) {
	f, err := os.Open(filepath.Join(s.o.ProcDir, "meminfo"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	m := map[string]uint64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		vals := strings.Fields(rest)
		if len(vals) == 0 {
			continue
		}
		v, err := strconv.ParseUint(vals[0], 10, 64)
		if err != nil {
			continue
		}
		if len(vals) > 1 && vals[1] == "kB" {
			v *= 1024
		}
		m[k] = v
	}
	return m, sc.Err()
}

func (s *Sampler) procFields(name string) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(s.o.ProcDir, name))
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(b)), nil
}

// Containers samples the given full container IDs; missing cgroups are left out of the map.
// CPU% comes from cpu.stat usage_usec deltas between calls (0 on an id's first call). IDs not
// asked for any more are forgotten.
func (s *Sampler) Containers(ids []string) map[string]Container {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.o.Now()
	cores := s.coresLocked()
	want := make(map[string]bool, len(ids))
	out := make(map[string]Container, len(ids))
	for _, id := range ids {
		want[id] = true
		dir := s.cgroupDir(id, now)
		if dir == "" {
			continue
		}
		c := Container{ID: id}
		cs := readKV(filepath.Join(dir, "cpu.stat"))
		if u, ok := cs["usage_usec"]; ok {
			if p, ok := s.prev[id]; ok && now.After(p.at) && u >= p.usec {
				wall := float64(now.Sub(p.at).Microseconds())
				c.CPUPct = float64(u-p.usec) / (wall * float64(cores)) * 100
			}
			s.prev[id] = usage{u, now}
		}
		cur, _ := readUint(filepath.Join(dir, "memory.current"))
		inactive := readKV(filepath.Join(dir, "memory.stat"))["inactive_file"]
		c.MemBytes = cur - min(inactive, cur)
		c.MemLimit, _ = readUint(filepath.Join(dir, "memory.max")) // "max" fails to parse → 0
		c.PIDs, _ = readUint(filepath.Join(dir, "pids.current"))
		out[id] = c
	}
	for id := range s.prev {
		if !want[id] {
			delete(s.prev, id)
		}
	}
	for id := range s.paths {
		if !want[id] {
			delete(s.paths, id)
		}
	}
	return out
}

// cgroupDir finds a container's cgroup directory, trying the layouts of the systemd and
// cgroupfs cgroup drivers before falling back to a (rate-limited) search of the tree.
func (s *Sampler) cgroupDir(id string, now time.Time) string {
	if d, ok := s.paths[id]; ok {
		if isDir(d) {
			return d
		}
		delete(s.paths, id) // container gone (or restarted under a new scope)
	}
	if id == "" {
		return ""
	}
	for _, rel := range []string{
		"system.slice/docker-" + id + ".scope",
		"docker/" + id,
		"docker.slice/docker-" + id + ".scope",
	} {
		if d := filepath.Join(s.o.CgroupDir, rel); isDir(d) {
			s.paths[id] = d
			return d
		}
	}
	if s.searchAt.IsZero() || now.Sub(s.searchAt) >= searchEvery {
		s.dirs = s.search()
		s.searchAt = now
	}
	for _, d := range s.dirs {
		if strings.Contains(filepath.Base(d), id) && isDir(d) {
			s.paths[id] = d
			return d
		}
	}
	return ""
}

// search lists every directory up to searchDepth levels below the cgroup root.
func (s *Sampler) search() []string {
	root := s.o.CgroupDir
	var dirs []string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() || p == root {
			return nil
		}
		dirs = append(dirs, p)
		if strings.Count(p[len(root):], string(filepath.Separator)) >= searchDepth {
			return fs.SkipDir
		}
		return nil
	})
	return dirs
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func readUint(p string) (uint64, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}

// readKV parses "key value" lines (cpu.stat, memory.stat); a missing file yields an empty map.
func readKV(p string) map[string]uint64 {
	m := map[string]uint64{}
	b, err := os.ReadFile(p)
	if err != nil {
		return m
	}
	for line := range bytes.Lines(b) {
		k, v, ok := strings.Cut(strings.TrimSpace(string(line)), " ")
		if !ok {
			continue
		}
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			m[k] = n
		}
	}
	return m
}

// CgroupV2 reports whether container stats via cgroup files can work here: the unified
// hierarchy's root has cgroup.controllers (cgroup v1 and hybrid roots do not).
func (s *Sampler) CgroupV2() bool {
	_, err := os.Stat(filepath.Join(s.o.CgroupDir, "cgroup.controllers"))
	return err == nil
}
