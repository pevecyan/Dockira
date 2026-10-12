// Package docker is a thin wrapper over the Engine SDK that turns its types into the small ones
// the rest of the app uses (and fakes in tests). Everything that changes a stack goes through
// the compose CLI instead (internal/compose); this package reads state, streams logs and events,
// and does the few single-container operations compose has no verb for.
package docker

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"
)

// Compose labels set on every container compose creates.
const (
	LabelProject     = "com.docker.compose.project"
	LabelService     = "com.docker.compose.service"
	LabelWorkingDir  = "com.docker.compose.project.working_dir"
	LabelConfigFiles = "com.docker.compose.project.config_files"
	LabelNumber      = "com.docker.compose.container-number"
	LabelOneoff      = "com.docker.compose.oneoff"
)

type Port struct {
	IP      string `json:"ip,omitempty"`
	Private uint16 `json:"private"`
	Public  uint16 `json:"public,omitempty"`
	Type    string `json:"type"`
}

type Container struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Image      string            `json:"image"`    // as configured (ref or ID)
	ImageID    string            `json:"image_id"` // sha256:… of the image the container runs
	State      string            `json:"state"`    // running, exited, restarting, paused, created, dead, removing
	Status     string            `json:"status"`   // "Up 28 minutes (healthy)"
	Health     string            `json:"health,omitempty"`
	ExitCode   int               `json:"exit_code"`
	Created    time.Time         `json:"created"`
	Ports      []Port            `json:"ports,omitempty"`
	Labels     map[string]string `json:"-"`
	Project    string            `json:"project,omitempty"`
	Service    string            `json:"service,omitempty"`
	WorkingDir string            `json:"working_dir,omitempty"`
	Oneoff     bool              `json:"oneoff,omitempty"` // `compose run` containers
}

// Details adds what only an inspect returns.
type Details struct {
	Container
	StartedAt    time.Time `json:"started_at"`
	FinishedAt   time.Time `json:"finished_at"`
	RestartCount int       `json:"restart_count"`
	Mounts       []Mount   `json:"mounts,omitempty"`
	Command      string    `json:"command"`
	HealthLog    []string  `json:"health_log,omitempty"`
}

type Mount struct {
	Type        string `json:"type"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Name        string `json:"name,omitempty"`
	RW          bool   `json:"rw"`
}

type Image struct {
	ID          string    `json:"id"`
	RepoTags    []string  `json:"repo_tags"`
	RepoDigests []string  `json:"repo_digests"`
	Created     time.Time `json:"created"`
	Size        int64     `json:"size"`
}

type Event struct {
	Type   string            `json:"type"`   // container, image, network
	Action string            `json:"action"` // start, die, health_status: healthy, …
	ID     string            `json:"id"`
	Attrs  map[string]string `json:"attrs,omitempty"`
	Time   time.Time         `json:"time"`
}

type System struct {
	Name          string `json:"name" doc:"The Docker host's hostname"`
	Version       string `json:"version"`
	APIVersion    string `json:"api_version"`
	OS            string `json:"os"`
	Kernel        string `json:"kernel"`
	CPUs          int    `json:"cpus"`
	Memory        int64  `json:"memory"`
	Containers    int    `json:"containers"`
	Running       int    `json:"running"`
	Images        int    `json:"images"`
	StorageDriver string `json:"storage_driver"`
	DockerRootDir string `json:"docker_root_dir"`
}

type DiskUsage struct {
	ImagesSize        int64 `json:"images_size"`
	ImagesReclaimable int64 `json:"images_reclaimable"`
	ImagesCount       int64 `json:"images_count"`
	ContainersSize    int64 `json:"containers_size"`
	VolumesSize       int64 `json:"volumes_size"`
	VolumesCount      int64 `json:"volumes_count"`
	BuildCacheSize    int64 `json:"build_cache_size"`
	BuildCacheReclaim int64 `json:"build_cache_reclaimable"`
}

type Client struct {
	c *client.Client
}

// New connects to DOCKER_HOST (default: the local socket) with API version negotiation.
func New() (*Client, error) {
	c, err := client.New(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	return &Client{c: c}, nil
}

func (d *Client) Close() error { return d.c.Close() }

// SDK exposes the underlying client for the few streaming calls (exec) that need it.
func (d *Client) SDK() *client.Client { return d.c }

func IsNotFound(err error) bool { return errdefs.IsNotFound(err) }

func (d *Client) Ping(ctx context.Context) error {
	_, err := d.c.Ping(ctx, client.PingOptions{})
	return err
}

// Containers lists all containers, stopped ones included.
func (d *Client) Containers(ctx context.Context) ([]Container, error) {
	res, err := d.c.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		return nil, err
	}
	out := make([]Container, 0, len(res.Items))
	for _, s := range res.Items {
		out = append(out, fromSummary(s))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

var exitRe = regexp.MustCompile(`^Exited \((-?\d+)\)`)

func fromSummary(s container.Summary) Container {
	c := Container{
		ID:      s.ID,
		Image:   s.Image,
		ImageID: s.ImageID,
		State:   string(s.State),
		Status:  s.Status,
		Created: time.Unix(s.Created, 0),
		Labels:  s.Labels,
	}
	if len(s.Names) > 0 {
		c.Name = strings.TrimPrefix(s.Names[0], "/")
	}
	if s.Health != nil && s.Health.Status != "none" {
		c.Health = string(s.Health.Status)
	}
	if m := exitRe.FindStringSubmatch(s.Status); m != nil {
		c.ExitCode, _ = strconv.Atoi(m[1])
	}
	c.applyLabels()
	seen := map[Port]bool{}
	for _, p := range s.Ports {
		port := Port{Private: p.PrivatePort, Public: p.PublicPort, Type: p.Type}
		if p.IP.IsValid() && !p.IP.IsUnspecified() {
			port.IP = p.IP.String()
		}
		if !seen[port] { // IPv4 and IPv6 bindings of the same port show once
			seen[port] = true
			c.Ports = append(c.Ports, port)
		}
	}
	sort.Slice(c.Ports, func(i, j int) bool { return c.Ports[i].Private < c.Ports[j].Private })
	return c
}

func (c *Container) applyLabels() {
	c.Project = c.Labels[LabelProject]
	c.Service = c.Labels[LabelService]
	c.WorkingDir = c.Labels[LabelWorkingDir]
	c.Oneoff = c.Labels[LabelOneoff] == "True"
}

// Inspect returns one container with start time, restarts, mounts and health log.
func (d *Client) Inspect(ctx context.Context, id string) (Details, error) {
	res, err := d.c.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return Details{}, err
	}
	r := res.Container
	det := Details{Container: Container{ID: r.ID, Name: strings.TrimPrefix(r.Name, "/"), ImageID: r.Image}}
	if r.Config != nil {
		det.Image = r.Config.Image
		det.Labels = r.Config.Labels
		det.Command = strings.Join(append(append([]string{}, r.Config.Entrypoint...), r.Config.Cmd...), " ")
	}
	det.applyLabels()
	det.Created, _ = time.Parse(time.RFC3339Nano, r.Created)
	if st := r.State; st != nil {
		det.State = string(st.Status)
		det.ExitCode = st.ExitCode
		det.StartedAt, _ = time.Parse(time.RFC3339Nano, st.StartedAt)
		det.FinishedAt, _ = time.Parse(time.RFC3339Nano, st.FinishedAt)
		if st.Health != nil {
			det.Health = string(st.Health.Status)
			for _, l := range st.Health.Log {
				det.HealthLog = append(det.HealthLog, fmt.Sprintf("%s exit %d: %s", l.End.Format(time.TimeOnly), l.ExitCode, strings.TrimSpace(l.Output)))
			}
		}
	}
	det.RestartCount = r.RestartCount
	for _, m := range r.Mounts {
		det.Mounts = append(det.Mounts, Mount{Type: string(m.Type), Source: m.Source, Destination: m.Destination, Name: m.Name, RW: m.RW})
	}
	return det, nil
}

// ImageInspect returns an image by ref or ID; IsNotFound(err) when it isn't there.
func (d *Client) ImageInspect(ctx context.Context, ref string) (Image, error) {
	res, err := d.c.ImageInspect(ctx, ref)
	if err != nil {
		return Image{}, err
	}
	img := Image{ID: res.ID, RepoTags: res.RepoTags, RepoDigests: res.RepoDigests, Size: res.Size}
	img.Created, _ = time.Parse(time.RFC3339Nano, res.Created)
	return img, nil
}

// ImageTag adds target as a name for source (rollback points).
func (d *Client) ImageTag(ctx context.Context, source, target string) error {
	_, err := d.c.ImageTag(ctx, client.ImageTagOptions{Source: source, Target: target})
	return err
}

// ImageRemove removes a tag (not the image while other tags or containers use it).
func (d *Client) ImageRemove(ctx context.Context, ref string) error {
	_, err := d.c.ImageRemove(ctx, ref, client.ImageRemoveOptions{})
	return err
}

// Images lists images whose reference matches the filter ("" = all).
func (d *Client) Images(ctx context.Context, reference string) ([]Image, error) {
	f := make(client.Filters)
	if reference != "" {
		f.Add("reference", reference)
	}
	res, err := d.c.ImageList(ctx, client.ImageListOptions{Filters: f})
	if err != nil {
		return nil, err
	}
	out := make([]Image, 0, len(res.Items))
	for _, s := range res.Items {
		out = append(out, Image{ID: s.ID, RepoTags: s.RepoTags, RepoDigests: s.RepoDigests, Created: time.Unix(s.Created, 0), Size: s.Size})
	}
	return out, nil
}

func (d *Client) System(ctx context.Context) (System, error) {
	info, err := d.c.Info(ctx, client.InfoOptions{})
	if err != nil {
		return System{}, err
	}
	i := info.Info
	s := System{Name: i.Name, OS: i.OperatingSystem, Kernel: i.KernelVersion, CPUs: i.NCPU, Memory: i.MemTotal, Containers: i.Containers,
		Running: i.ContainersRunning, Images: i.Images, StorageDriver: i.Driver, DockerRootDir: i.DockerRootDir, Version: i.ServerVersion}
	if v, err := d.c.ServerVersion(ctx, client.ServerVersionOptions{}); err == nil {
		s.Version, s.APIVersion = v.Version, v.APIVersion
	}
	return s, nil
}

func (d *Client) DiskUsage(ctx context.Context) (DiskUsage, error) {
	res, err := d.c.DiskUsage(ctx, client.DiskUsageOptions{Containers: true, Images: true, Volumes: true, BuildCache: true})
	if err != nil {
		return DiskUsage{}, err
	}
	return DiskUsage{
		ImagesSize: res.Images.TotalSize, ImagesReclaimable: res.Images.Reclaimable, ImagesCount: res.Images.TotalCount,
		ContainersSize: res.Containers.TotalSize,
		VolumesSize:    res.Volumes.TotalSize, VolumesCount: res.Volumes.TotalCount,
		BuildCacheSize: res.BuildCache.TotalSize, BuildCacheReclaim: res.BuildCache.Reclaimable,
	}, nil
}

// ContainerAction runs start/stop/restart/pause/unpause on one container.
func (d *Client) ContainerAction(ctx context.Context, id, action string) error {
	var err error
	switch action {
	case "start":
		_, err = d.c.ContainerStart(ctx, id, client.ContainerStartOptions{})
	case "stop":
		_, err = d.c.ContainerStop(ctx, id, client.ContainerStopOptions{})
	case "restart":
		_, err = d.c.ContainerRestart(ctx, id, client.ContainerRestartOptions{})
	case "pause":
		_, err = d.c.ContainerPause(ctx, id, client.ContainerPauseOptions{})
	case "unpause":
		_, err = d.c.ContainerUnpause(ctx, id, client.ContainerUnpauseOptions{})
	default:
		return fmt.Errorf("unknown container action %q", action)
	}
	return err
}

type LogOptions struct {
	Tail       string // "200", "all"
	Since      string // RFC 3339 or unix seconds
	Until      string
	Follow     bool
	Timestamps bool
}

// Logs returns the raw (multiplexed unless the container has a TTY) log stream; see Demux.
func (d *Client) Logs(ctx context.Context, id string, o LogOptions) (io.ReadCloser, error) {
	return d.c.ContainerLogs(ctx, id, client.ContainerLogsOptions{
		ShowStdout: true, ShowStderr: true, Tail: o.Tail, Since: o.Since, Until: o.Until, Follow: o.Follow, Timestamps: o.Timestamps,
	})
}

// Events streams container and image events until ctx ends or the connection drops (the error
// channel then yields the reason; callers reconnect).
func (d *Client) Events(ctx context.Context) (<-chan Event, <-chan error) {
	f := make(client.Filters).Add("type", "container", "image")
	res := d.c.Events(ctx, client.EventsListOptions{Filters: f})
	out := make(chan Event, 64)
	errc := make(chan error, 1)
	go func() {
		defer close(out)
		for {
			select {
			case m, ok := <-res.Messages:
				if !ok {
					errc <- io.EOF
					return
				}
				out <- fromEvent(m)
			case err := <-res.Err:
				if err == nil {
					err = io.EOF
				}
				errc <- err
				return
			case <-ctx.Done():
				errc <- ctx.Err()
				return
			}
		}
	}()
	return out, errc
}

func fromEvent(m events.Message) Event {
	t := time.Unix(0, m.TimeNano)
	if m.TimeNano == 0 {
		t = time.Unix(m.Time, 0)
	}
	return Event{Type: string(m.Type), Action: string(m.Action), ID: m.Actor.ID, Attrs: m.Actor.Attributes, Time: t}
}

// IsTTY reports whether a container was created with a TTY (its logs are not multiplexed).
func (d *Client) IsTTY(ctx context.Context, id string) (bool, error) {
	res, err := d.c.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return false, err
	}
	return res.Container.Config != nil && res.Container.Config.Tty, nil
}

// Exec runs a command in a running container (no TTY) and copies its stdout to out; stderr
// goes to errOut. It returns the exit code.
func (d *Client) Exec(ctx context.Context, id string, cmd []string, out, errOut io.Writer) (int, error) {
	cr, err := d.c.ExecCreate(ctx, id, client.ExecCreateOptions{Cmd: cmd, AttachStdout: true, AttachStderr: true})
	if err != nil {
		return -1, err
	}
	at, err := d.c.ExecAttach(ctx, cr.ID, client.ExecAttachOptions{})
	if err != nil {
		return -1, err
	}
	defer at.Close()
	if _, err := stdcopy.StdCopy(out, errOut, at.Reader); err != nil && ctx.Err() == nil {
		return -1, err
	}
	ins, err := d.c.ExecInspect(ctx, cr.ID, client.ExecInspectOptions{})
	if err != nil {
		return -1, err
	}
	return ins.ExitCode, nil
}

// RemoveImageID deletes an image by ID (with untagged parents); it fails while a container
// uses the image.
func (d *Client) RemoveImageID(ctx context.Context, id string) error {
	_, err := d.c.ImageRemove(ctx, id, client.ImageRemoveOptions{PruneChildren: true})
	return err
}

// PruneBuildCache drops unused build cache and returns the bytes freed.
func (d *Client) PruneBuildCache(ctx context.Context) (int64, error) {
	res, err := d.c.BuildCachePrune(ctx, client.BuildCachePruneOptions{All: true})
	if err != nil {
		return 0, err
	}
	return int64(res.Report.SpaceReclaimed), nil
}

// ExecSession is an interactive TTY in a container (the exec shell).
type ExecSession struct {
	ID     string
	Conn   io.WriteCloser // stdin
	Output io.Reader      // stdout+stderr (a TTY merges them)
	close  func()
}

func (s *ExecSession) Close() {
	if s.close != nil {
		s.close()
	}
}

// ExecTTY starts cmd with a TTY of the given size.
func (d *Client) ExecTTY(ctx context.Context, id string, cmd []string, cols, rows uint) (*ExecSession, error) {
	size := client.ConsoleSize{Height: rows, Width: cols}
	cr, err := d.c.ExecCreate(ctx, id, client.ExecCreateOptions{Cmd: cmd, TTY: true, AttachStdin: true, AttachStdout: true, AttachStderr: true,
		ConsoleSize: size, Env: []string{"TERM=xterm-256color"}})
	if err != nil {
		return nil, err
	}
	at, err := d.c.ExecAttach(ctx, cr.ID, client.ExecAttachOptions{TTY: true, ConsoleSize: size})
	if err != nil {
		return nil, err
	}
	return &ExecSession{ID: cr.ID, Conn: at.Conn, Output: at.Reader, close: at.Close}, nil
}

func (d *Client) ExecResize(ctx context.Context, execID string, cols, rows uint) error {
	_, err := d.c.ExecResize(ctx, execID, client.ExecResizeOptions{Height: rows, Width: cols})
	return err
}
