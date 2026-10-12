package hoststats

import (
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const id1 = "1981798c5c23009a9903e120a1afdc69e09d3eede5181b33eaf81fd4f5ac3ea0"

// clock is a fake Options.Now that tests move forward by hand.
type clock struct{ t time.Time }

func (c *clock) now() time.Time           { return c.t }
func (c *clock) add(d time.Duration)      { c.t = c.t.Add(d) }
func newClock() *clock                    { return &clock{time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)} }
func near(a, b float64) bool              { return math.Abs(a-b) < 1e-9 }
func write(t *testing.T, p, s string)     { t.Helper(); mkdir(t, filepath.Dir(p)); writeFile(t, p, s) }
func mkdir(t *testing.T, p string)        { t.Helper(); must(t, os.MkdirAll(p, 0o755)) }
func writeFile(t *testing.T, p, s string) { t.Helper(); must(t, os.WriteFile(p, []byte(s), 0o644)) }

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func fakeProc(t *testing.T, stat string) string {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "stat"), stat)
	write(t, filepath.Join(dir, "meminfo"), `MemTotal:       16000000 kB
MemFree:         2000000 kB
MemAvailable:   10000000 kB
SwapTotal:       4000000 kB
SwapFree:        3000000 kB
`)
	write(t, filepath.Join(dir, "loadavg"), "0.52 1.25 2.00 2/1234 5678\n")
	write(t, filepath.Join(dir, "uptime"), "12345.67 98765.43\n")
	return dir
}

const stat1 = `cpu  100 0 100 700 100 0 0 0 50 0
cpu0 25 0 25 175 25 0 0 0 0 0
cpu1 25 0 25 175 25 0 0 0 0 0
cpu2 25 0 25 175 25 0 0 0 0 0
cpu3 25 0 25 175 25 0 0 0 0 0
intr 1 2 3
`

// stat2 adds 300 busy and 100 idle+iowait jiffies (guest grows too, but must not count).
const stat2 = `cpu  300 0 200 750 150 0 0 0 250 0
cpu0 1 0 1 1 1 0 0 0 0 0
cpu1 1 0 1 1 1 0 0 0 0 0
cpu2 1 0 1 1 1 0 0 0 0 0
cpu3 1 0 1 1 1 0 0 0 0 0
`

func TestHost(t *testing.T) {
	proc := fakeProc(t, stat1)
	clk := newClock()
	s := New(Options{ProcDir: proc, CgroupDir: t.TempDir(), DiskPath: t.TempDir(), Now: clk.now})

	h, err := s.Host()
	must(t, err)
	if h.CPUPct != 0 || h.Cores != 4 || !h.Time.Equal(clk.t) {
		t.Errorf("first sample: %+v", h)
	}
	if h.MemTotal != 16000000*1024 || h.MemAvailable != 10000000*1024 || h.MemUsed != 6000000*1024 {
		t.Errorf("mem: %+v", h)
	}
	if h.SwapTotal != 4000000*1024 || h.SwapUsed != 1000000*1024 {
		t.Errorf("swap: %+v", h)
	}
	if h.Load != [3]float64{0.52, 1.25, 2.00} || h.UptimeSec != 12345.67 {
		t.Errorf("load/uptime: %+v", h)
	}
	if h.DiskTotal == 0 || h.DiskUsed+h.DiskFree > h.DiskTotal || h.DiskPath == "" {
		t.Errorf("disk: %+v", h)
	}

	writeFile(t, filepath.Join(proc, "stat"), stat2)
	clk.add(time.Second)
	h, err = s.Host()
	must(t, err)
	if !near(h.CPUPct, 75) { // 300 busy / 400 total
		t.Errorf("cpu pct = %v, want 75", h.CPUPct)
	}
}

func TestHostMissingFiles(t *testing.T) {
	proc := fakeProc(t, stat1)
	os.Remove(filepath.Join(proc, "stat"))
	os.Remove(filepath.Join(proc, "loadavg"))
	s := New(Options{ProcDir: proc, CgroupDir: t.TempDir(), DiskPath: "/does/not/exist"})
	h, err := s.Host()
	if err == nil {
		t.Fatal("want an error for the missing files")
	}
	if h.MemTotal == 0 || h.UptimeSec == 0 {
		t.Errorf("partial result lost: %+v", h)
	}
	if h.Cores != runtime.NumCPU() {
		t.Errorf("cores fallback = %d", h.Cores)
	}

	// Nothing there at all: still no panic.
	h, err = New(Options{ProcDir: t.TempDir(), CgroupDir: t.TempDir()}).Host()
	if err == nil || h.MemTotal != 0 || h.Cores == 0 {
		t.Errorf("empty proc: %+v %v", h, err)
	}
}

func fakeCgroup(t *testing.T, dir, usage, current, max string) {
	write(t, filepath.Join(dir, "cpu.stat"), "usage_usec "+usage+"\nuser_usec 1\nsystem_usec 2\n")
	write(t, filepath.Join(dir, "memory.current"), current+"\n")
	write(t, filepath.Join(dir, "memory.max"), max+"\n")
	write(t, filepath.Join(dir, "memory.stat"), "anon 100\nfile 300\nactive_file 100\ninactive_file 200\n")
	write(t, filepath.Join(dir, "pids.current"), "7\n")
}

func TestContainers(t *testing.T) {
	proc := fakeProc(t, stat1) // 4 cores
	cg := t.TempDir()
	write(t, filepath.Join(cg, "cgroup.controllers"), "cpu memory pids\n")
	dir := filepath.Join(cg, "system.slice", "docker-"+id1+".scope")
	fakeCgroup(t, dir, "5000000", "1000", "max")
	clk := newClock()
	s := New(Options{ProcDir: proc, CgroupDir: cg, Now: clk.now})
	if !s.CgroupV2() {
		t.Error("CgroupV2 = false")
	}

	m := s.Containers([]string{id1, "gone"})
	if len(m) != 1 {
		t.Fatalf("got %v", m)
	}
	c := m[id1]
	if c.ID != id1 || c.CPUPct != 0 || c.MemBytes != 800 || c.MemLimit != 0 || c.PIDs != 7 {
		t.Errorf("first sample: %+v", c)
	}

	write(t, filepath.Join(dir, "cpu.stat"), "usage_usec 6000000\n")
	write(t, filepath.Join(dir, "memory.max"), "536870912\n")
	clk.add(time.Second)
	c = s.Containers([]string{id1})[id1]
	if !near(c.CPUPct, 25) { // 1 s of CPU in 1 s on 4 cores
		t.Errorf("cpu pct = %v, want 25", c.CPUPct)
	}
	if c.MemLimit != 536870912 {
		t.Errorf("limit = %d", c.MemLimit)
	}

	// inactive_file larger than current must not wrap around.
	write(t, filepath.Join(dir, "memory.current"), "100\n")
	if c := s.Containers([]string{id1})[id1]; c.MemBytes != 0 {
		t.Errorf("mem = %d, want 0", c.MemBytes)
	}
}

func TestContainersMissingFiles(t *testing.T) {
	cg := t.TempDir()
	dir := filepath.Join(cg, "docker", id1)
	mkdir(t, dir)
	write(t, filepath.Join(dir, "memory.current"), "4096\n")
	s := New(Options{ProcDir: fakeProc(t, stat1), CgroupDir: cg})
	c, ok := s.Containers([]string{id1})[id1]
	if !ok || c.MemBytes != 4096 || c.CPUPct != 0 || c.PIDs != 0 {
		t.Errorf("partial: %+v %v", c, ok)
	}
	if s.CgroupV2() {
		t.Error("CgroupV2 without cgroup.controllers")
	}
}

func TestLookupOrder(t *testing.T) {
	cg := t.TempDir()
	// All three layouts exist; the systemd one wins, then cgroupfs, then docker.slice.
	layouts := []string{
		filepath.Join(cg, "system.slice", "docker-"+id1+".scope"),
		filepath.Join(cg, "docker", id1),
		filepath.Join(cg, "docker.slice", "docker-"+id1+".scope"),
	}
	for _, d := range layouts {
		fakeCgroup(t, d, "0", "1000", "max")
	}
	clk := newClock()
	s := New(Options{ProcDir: fakeProc(t, stat1), CgroupDir: cg, Now: clk.now})
	for i := range layouts {
		if _, ok := s.Containers([]string{id1})[id1]; !ok {
			t.Errorf("layout %d: not found", i)
		}
		if got := s.paths[id1]; got != layouts[i] {
			t.Errorf("layout %d: path %q", i, got)
		}
		must(t, os.RemoveAll(layouts[i]))
	}
}

func TestFallbackSearch(t *testing.T) {
	cg := t.TempDir()
	deep := filepath.Join(cg, "a.slice", "b.slice", "cri-docker-"+id1+".scope")
	fakeCgroup(t, deep, "0", "1000", "max")
	tooDeep := filepath.Join(cg, "1", "2", "3", "4", "x-"+strings.Repeat("f", 64))
	fakeCgroup(t, tooDeep, "0", "1000", "max")

	clk := newClock()
	s := New(Options{ProcDir: fakeProc(t, stat1), CgroupDir: cg, Now: clk.now})
	if _, ok := s.Containers([]string{id1})[id1]; !ok || s.paths[id1] != deep {
		t.Fatalf("fallback not found: %q", s.paths[id1])
	}
	if _, ok := s.Containers([]string{strings.Repeat("f", 64)})[strings.Repeat("f", 64)]; ok {
		t.Error("search went deeper than 4 levels")
	}

	// A new container appearing outside the known layouts is only found after the rate limit.
	id2 := strings.Repeat("ab", 32)
	late := filepath.Join(cg, "c.slice", "docker-"+id2+".scope")
	fakeCgroup(t, late, "0", "1000", "max")
	clk.add(30 * time.Second)
	if _, ok := s.Containers([]string{id2})[id2]; ok {
		t.Error("search re-ran before 60 s")
	}
	clk.add(30 * time.Second)
	if _, ok := s.Containers([]string{id2})[id2]; !ok {
		t.Error("search did not re-run after 60 s")
	}
}

func TestForget(t *testing.T) {
	cg := t.TempDir()
	id2 := strings.Repeat("ab", 32)
	fakeCgroup(t, filepath.Join(cg, "docker", id1), "1000000", "0", "max")
	fakeCgroup(t, filepath.Join(cg, "docker", id2), "1000000", "0", "max")
	clk := newClock()
	s := New(Options{ProcDir: fakeProc(t, stat1), CgroupDir: cg, Now: clk.now})

	s.Containers([]string{id1, id2})
	s.Containers([]string{id1})
	if _, ok := s.prev[id2]; ok {
		t.Error("id2 usage not forgotten")
	}
	if _, ok := s.paths[id2]; ok {
		t.Error("id2 path not forgotten")
	}
	// id2 coming back starts over: no delta against the stale sample.
	write(t, filepath.Join(cg, "docker", id2, "cpu.stat"), "usage_usec 9000000\n")
	clk.add(time.Second)
	if c := s.Containers([]string{id1, id2})[id2]; c.CPUPct != 0 {
		t.Errorf("stale delta used: %v", c.CPUPct)
	}
	if len(s.prev) != 2 {
		t.Errorf("prev = %v", s.prev)
	}
}

// TestRealHost reads this machine's /proc and cgroup tree and only checks the values are sane.
func TestRealHost(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	s := New(Options{ProcDir: "/proc", CgroupDir: "/sys/fs/cgroup", DiskPath: "/"})
	if _, err := s.Host(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	h, err := s.Host()
	if err != nil {
		t.Fatal(err)
	}
	if h.Cores < 1 || h.CPUPct < 0 || h.CPUPct > 100 || h.MemTotal == 0 || h.MemUsed > h.MemTotal ||
		h.SwapUsed > h.SwapTotal || h.UptimeSec <= 0 || h.DiskTotal == 0 || h.DiskUsed > h.DiskTotal {
		t.Errorf("insane: %+v", h)
	}
	t.Logf("%+v cgroup2=%v", h, s.CgroupV2())

	// Any containers running here (systemd driver layout) must resolve and look sane.
	scopes, _ := filepath.Glob("/sys/fs/cgroup/system.slice/docker-*.scope")
	var ids []string
	for _, p := range scopes {
		ids = append(ids, strings.TrimSuffix(strings.TrimPrefix(filepath.Base(p), "docker-"), ".scope"))
	}
	for id, c := range s.Containers(ids) {
		if c.PIDs == 0 || c.CPUPct != 0 {
			t.Errorf("container %s: %+v", id, c)
		}
	}
}

// Inside an LXC, lxcfs answers /proc for the reader's cgroup; the cgroup root we see has the
// system container's real memory and CPU.
func TestSystemContainerMode(t *testing.T) {
	proc := fakeProc(t, stat1)
	cg := t.TempDir()
	write(t, filepath.Join(cg, "memory.current"), "1702121472\n")
	write(t, filepath.Join(cg, "memory.stat"), "anon 730943488\nfile 887701504\ninactive_file 722145280\n")
	write(t, filepath.Join(cg, "memory.swap.current"), "1048576\n")
	write(t, filepath.Join(cg, "cpu.stat"), "usage_usec 1000000\n")
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s := New(Options{ProcDir: proc, CgroupDir: cg, Now: func() time.Time { return now }})
	h, _ := s.Host()
	if want := uint64(1702121472 - 887701504); h.MemUsed != want || h.MemAvailable != h.MemTotal-want || h.SwapUsed != 1048576 {
		t.Errorf("memory %d / %d, swap %d", h.MemUsed, h.MemAvailable, h.SwapUsed)
	}
	// 2 s of CPU over 1 s on 4 cores = 50 %
	now = now.Add(time.Second)
	write(t, filepath.Join(cg, "cpu.stat"), "usage_usec 3000000\n")
	h, _ = s.Host()
	if h.CPUPct < 49.9 || h.CPUPct > 50.1 {
		t.Errorf("cpu %.2f", h.CPUPct)
	}
}
