// Package snapshots saves and restores a stack's data: its named Docker volumes (archived by a
// short-lived helper container) and, if asked, bind-mounted folders inside the stack's folder
// (archived directly). Each snapshot is a folder of .tar.gz files with a manifest under
// data_dir/backups/<stack>/snapshots/<time>/.
package snapshots

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"docker-manager/internal/compose"
	"docker-manager/internal/docker"
	"docker-manager/internal/jobs"
	"docker-manager/internal/stacks"
)

type Inspector interface {
	Inspect(ctx context.Context, id string) (docker.Details, error)
}

type Composer interface {
	Cmd(ctx context.Context, dir string, files []string, args ...string) *exec.Cmd
}

type Manager struct {
	DataDir string
	Docker  string // docker CLI ("docker")
	Image   string // helper image with sh and tar (docker-manager's own image, or busybox)
	Engine  Inspector
	Compose Composer
}

// Options of a snapshot.
type Options struct {
	Stop    bool   `json:"stop" doc:"Stop the stack while its data is copied (consistent databases, a short downtime)"`
	Binds   bool   `json:"binds" doc:"Also save bind-mounted folders inside the stack's folder (like ./data)"`
	Keep    int    `json:"keep,omitempty" doc:"Snapshots kept for the stack (default 3)"`
	Trigger string `json:"-"`
}

// Item is one archive of a snapshot.
type Item struct {
	Kind   string `json:"kind" enum:"volume,bind"`
	Name   string `json:"name" doc:"Volume name, or the bind folder relative to the stack folder"`
	File   string `json:"file"`
	Size   int64  `json:"size"`
	Target string `json:"target" doc:"Where the container sees it"`
}

type Snapshot struct {
	ID      string    `json:"id"`
	StackID string    `json:"stack_id"`
	Created time.Time `json:"created"`
	Trigger string    `json:"trigger" doc:"manual, update, before restore"`
	Stopped bool      `json:"stopped" doc:"The stack was stopped while it was taken"`
	Items   []Item    `json:"items"`
	Size    int64     `json:"size"`
}

var ErrNothing = errors.New("this stack has no volumes or bind-mounted folders to save")

func (m *Manager) stackDir(stackID string) string {
	return filepath.Join(m.DataDir, "backups", strings.NewReplacer("/", "_", "~", "").Replace(stackID), "snapshots")
}

type source struct {
	kind, name, path, target string
}

// sources finds what a snapshot of the stack contains, from its containers' mounts.
func (m *Manager) sources(ctx context.Context, st stacks.Stack, binds bool) ([]source, error) {
	seen := map[string]bool{}
	var out []source
	dir, _ := filepath.EvalSymlinks(st.Dir)
	for _, sv := range st.Services {
		for _, c := range sv.Containers {
			d, err := m.Engine.Inspect(ctx, c.ID)
			if err != nil {
				return nil, err
			}
			for _, mt := range d.Mounts {
				switch {
				case mt.Type == "volume" && mt.Name != "" && !seen["v:"+mt.Name]:
					seen["v:"+mt.Name] = true
					out = append(out, source{kind: "volume", name: mt.Name, target: mt.Destination})
				case mt.Type == "bind" && binds && dir != "":
					src, err := filepath.EvalSymlinks(mt.Source)
					if err != nil || !safeBind(dir, src) || seen["b:"+src] {
						continue
					}
					seen["b:"+src] = true
					rel, _ := filepath.Rel(dir, src)
					out = append(out, source{kind: "bind", name: filepath.ToSlash(rel), path: src, target: mt.Destination})
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].kind+out[i].name < out[j].kind+out[j].name })
	if len(out) == 0 {
		return nil, ErrNothing
	}
	return out, nil
}

// safeBind: strictly inside the stack folder, and not a folder holding a compose file (restoring
// it would replace the stack's own files).
func safeBind(stackDir, src string) bool {
	if !strings.HasPrefix(src, stackDir+string(filepath.Separator)) {
		return false
	}
	if fi, err := os.Stat(src); err == nil && fi.IsDir() && compose.FindFiles(src) != nil {
		return false
	}
	return true
}

var nameRe = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

// Take makes a snapshot of the stack. With Stop, running services are stopped first and started
// again afterwards, even when copying fails.
func (m *Manager) Take(ctx context.Context, st stacks.Stack, o Options, out *jobs.Output) (Snapshot, error) {
	srcs, err := m.sources(ctx, st, o.Binds)
	if err != nil {
		return Snapshot{}, err
	}
	snap := Snapshot{ID: time.Now().UTC().Format("20060102-150405"), StackID: st.ID, Created: time.Now(), Trigger: o.Trigger, Items: []Item{}}
	if snap.Trigger == "" {
		snap.Trigger = "manual"
	}
	dir := filepath.Join(m.stackDir(st.ID), snap.ID)
	if _, err := os.Stat(dir); err == nil {
		dir += "-" + fmt.Sprint(time.Now().UnixNano()%1000)
		snap.ID = filepath.Base(dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return snap, err
	}
	if o.Stop {
		running := runningServices(st)
		if len(running) > 0 {
			snap.Stopped = true
			if err := m.compose(ctx, st, out, append([]string{"stop"}, running...)...); err != nil {
				os.RemoveAll(dir)
				return snap, err
			}
			defer func() {
				if err := m.compose(context.WithoutCancel(ctx), st, out, append([]string{"start"}, running...)...); err != nil {
					out.Printf("  ! starting %s again failed: %v", strings.Join(running, ", "), err)
				}
			}()
		}
	}
	for i, s := range srcs {
		file := fmt.Sprintf("%02d-%s-%s.tar.gz", i+1, s.kind, strings.Trim(nameRe.ReplaceAllString(s.name, "_"), "_"))
		p := filepath.Join(dir, file)
		out.Printf("▸ saving %s %s → %s", s.kind, s.name, file)
		if s.kind == "volume" {
			err = m.saveVolume(ctx, s.name, p, out)
		} else {
			err = archiveDir(s.path, p)
		}
		if err != nil {
			os.RemoveAll(dir)
			return snap, fmt.Errorf("%s %s: %w", s.kind, s.name, err)
		}
		fi, _ := os.Stat(p)
		snap.Items = append(snap.Items, Item{Kind: s.kind, Name: s.name, File: file, Size: fi.Size(), Target: s.target})
		snap.Size += fi.Size()
		out.Printf("  %s", human(fi.Size()))
	}
	b, _ := json.MarshalIndent(snap, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), b, 0o600); err != nil {
		return snap, err
	}
	keep := o.Keep
	if keep <= 0 {
		keep = 3
	}
	m.prune(st.ID, keep, snap.ID)
	out.Printf("✓ snapshot %s: %d items, %s", snap.ID, len(snap.Items), human(snap.Size))
	return snap, nil
}

func runningServices(st stacks.Stack) []string {
	var out []string
	for _, sv := range st.Services {
		for _, c := range sv.Containers {
			if c.State == "running" && sv.InFile {
				out = append(out, sv.Name)
				break
			}
		}
	}
	return out
}

func (m *Manager) compose(ctx context.Context, st stacks.Stack, out *jobs.Output, args ...string) error {
	out.Printf("▸ docker compose %s", strings.Join(args, " "))
	return out.Exec(m.Compose.Cmd(ctx, st.Dir, st.Files, args...))
}

// helper runs the helper image with a volume mounted at /v; the process group is killed on cancel.
func (m *Manager) helper(ctx context.Context, volume string, ro bool, script string) *exec.Cmd {
	mount := volume + ":/v"
	if ro {
		mount += ":ro"
	}
	bin := m.Docker
	if bin == "" {
		bin = "docker"
	}
	cmd := exec.CommandContext(ctx, bin, "run", "--rm", "-i", "--network", "none", "--entrypoint", "sh", "-v", mount, m.Image, "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 10 * time.Second
	return cmd
}

func (m *Manager) saveVolume(ctx context.Context, volume, file string, out *jobs.Output) error {
	f, err := os.OpenFile(file, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	cmd := m.helper(ctx, volume, true, "tar czf - -C /v .")
	cmd.Stdout, cmd.Stderr = f, out
	err = cmd.Run()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// List returns the stack's snapshots, newest first.
func (m *Manager) List(stackID string) ([]Snapshot, error) {
	des, err := os.ReadDir(m.stackDir(stackID))
	if errors.Is(err, fs.ErrNotExist) {
		return []Snapshot{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Snapshot{}
	for _, de := range des {
		if s, err := m.Get(stackID, de.Name()); err == nil {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

var idRe = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}(-[0-9]+)?$`)

func (m *Manager) Get(stackID, id string) (Snapshot, error) {
	if !idRe.MatchString(id) {
		return Snapshot{}, fs.ErrNotExist
	}
	b, err := os.ReadFile(filepath.Join(m.stackDir(stackID), id, "manifest.json"))
	if err != nil {
		return Snapshot{}, err
	}
	var s Snapshot
	if err := json.Unmarshal(b, &s); err != nil || s.StackID != stackID {
		return Snapshot{}, fs.ErrNotExist
	}
	return s, nil
}

// File opens one archive of a snapshot (download).
func (m *Manager) File(stackID, id, file string) (*os.File, error) {
	s, err := m.Get(stackID, id)
	if err != nil {
		return nil, err
	}
	for _, it := range s.Items {
		if it.File == file {
			return os.Open(filepath.Join(m.stackDir(stackID), id, file))
		}
	}
	return nil, fs.ErrNotExist
}

func (m *Manager) Delete(stackID, id string) error {
	if _, err := m.Get(stackID, id); err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(m.stackDir(stackID), id))
}

func (m *Manager) prune(stackID string, keep int, except string) {
	all, err := m.List(stackID)
	if err != nil {
		return
	}
	n := 0
	for _, s := range all {
		if s.ID == except || s.Trigger == "before restore" { // safety copies are removed by hand
			continue
		}
		n++
		if n >= keep {
			_ = os.RemoveAll(filepath.Join(m.stackDir(stackID), s.ID))
		}
	}
}

// Restore puts a snapshot back: the stack is stopped, its current data saved as a "before
// restore" snapshot, every archive restored (volumes emptied first), and the stack started again.
func (m *Manager) Restore(ctx context.Context, st stacks.Stack, id string, out *jobs.Output) error {
	snap, err := m.Get(st.ID, id)
	if err != nil {
		return err
	}
	dir, _ := filepath.EvalSymlinks(st.Dir)
	for _, it := range snap.Items { // check everything before changing anything
		if it.Kind == "bind" {
			p := filepath.Join(dir, filepath.FromSlash(it.Name))
			if real, err := filepath.EvalSymlinks(p); err == nil {
				p = real
			}
			if !safeBind(dir, p) {
				return fmt.Errorf("%s is not a folder inside the stack folder any more", it.Name)
			}
		}
	}
	running := runningServices(st)
	if len(running) > 0 {
		if err := m.compose(ctx, st, out, "stop"); err != nil {
			return err
		}
	}
	out.Printf("▸ saving the current data first (a \"before restore\" snapshot)")
	if _, err := m.Take(ctx, st, Options{Binds: hasBinds(snap), Keep: 1000, Trigger: "before restore"}, out); err != nil && !errors.Is(err, ErrNothing) {
		return fmt.Errorf("safety snapshot failed, nothing was restored: %w", err)
	}
	for _, it := range snap.Items {
		p := filepath.Join(m.stackDir(st.ID), id, it.File)
		out.Printf("▸ restoring %s %s", it.Kind, it.Name)
		if it.Kind == "volume" {
			err = m.restoreVolume(ctx, it.Name, p, out)
		} else {
			err = restoreDir(p, filepath.Join(dir, filepath.FromSlash(it.Name)))
		}
		if err != nil {
			return fmt.Errorf("%s %s: %w (the \"before restore\" snapshot has the data from before)", it.Kind, it.Name, err)
		}
	}
	if len(running) > 0 {
		if err := m.compose(ctx, st, out, append([]string{"start"}, running...)...); err != nil {
			return err
		}
	}
	out.Printf("✓ restored snapshot %s", id)
	return nil
}

func hasBinds(s Snapshot) bool {
	for _, it := range s.Items {
		if it.Kind == "bind" {
			return true
		}
	}
	return false
}

func (m *Manager) restoreVolume(ctx context.Context, volume, file string, out *jobs.Output) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	// the volume is created if it was removed; its content is replaced
	cmd := m.helper(ctx, volume, false, "find /v -mindepth 1 -delete && tar xzf - -C /v")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = f, out, out
	return cmd.Run()
}

// archiveDir writes a folder (or a single file) as .tar.gz, keeping modes, owners and symlinks.
func archiveDir(src, file string) error {
	f, err := os.OpenFile(file, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(f)
	tw := tar.NewWriter(zw)
	base := src
	single := false
	if fi, err := os.Stat(src); err == nil && !fi.IsDir() {
		base, single = filepath.Dir(src), true
	}
	err = filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(base, p)
		if !single && rel == "." {
			return nil
		}
		link := ""
		if fi.Mode()&fs.ModeSymlink != 0 {
			link, _ = os.Readlink(p)
		}
		h, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return err
		}
		h.Name = filepath.ToSlash(rel)
		if fi.IsDir() {
			h.Name += "/"
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			r, err := os.Open(p)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, r)
			r.Close()
			return err
		}
		return nil
	})
	for _, c := range []io.Closer{tw, zw, f} {
		if cerr := c.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		os.Remove(file)
	}
	return err
}

// restoreDir replaces a bind folder's content (or a single file) with an archive's.
func restoreDir(file, dst string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	base := dst
	if fi, err := os.Stat(dst); err == nil && !fi.IsDir() {
		base = filepath.Dir(dst) // a single file: the archive holds just its name
	} else {
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		des, _ := os.ReadDir(dst)
		for _, de := range des {
			if err := os.RemoveAll(filepath.Join(dst, de.Name())); err != nil {
				return err
			}
		}
	}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		p := filepath.Join(base, filepath.FromSlash(h.Name))
		if p != base && !strings.HasPrefix(p, base+string(filepath.Separator)) {
			return fmt.Errorf("archive entry %q leaves the folder", h.Name)
		}
		mode := fs.FileMode(h.Mode).Perm()
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, mode|0o700); err != nil {
				return err
			}
		case tar.TypeSymlink:
			_ = os.Remove(p)
			if err := os.Symlink(h.Linkname, p); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			w, err := os.OpenFile(p, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			_, err = io.Copy(w, tr)
			w.Close()
			if err != nil {
				return err
			}
		default:
			continue
		}
		_ = os.Lchown(p, h.Uid, h.Gid) // works as root (in the container); harmless otherwise
		if h.Typeflag != tar.TypeSymlink {
			_ = os.Chmod(p, mode)
			_ = os.Chtimes(p, h.ModTime, h.ModTime)
		}
	}
}

func human(n int64) string {
	const u = 1024
	if n < u {
		return fmt.Sprintf("%d B", n)
	}
	d, e := int64(u), 0
	for v := n / u; v >= u; v /= u {
		d *= u
		e++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(d), "KMGTP"[e])
}
