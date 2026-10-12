// Package compose runs the `docker compose` CLI. Everything that changes a stack goes through
// it, so the result is exactly what `docker compose` in a terminal would do; reading a project
// uses `compose config --format json`, i.e. compose's own interpolation, profiles and overrides.
package compose

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// FileNames are the compose files a stack folder is recognised by, in compose's own order.
var FileNames = []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}

// OverrideNames are picked up next to the main file (compose does this only without -f).
var OverrideNames = map[string][]string{
	"compose.yaml":        {"compose.override.yaml", "compose.override.yml"},
	"compose.yml":         {"compose.override.yml", "compose.override.yaml"},
	"docker-compose.yaml": {"docker-compose.override.yaml", "docker-compose.override.yml"},
	"docker-compose.yml":  {"docker-compose.override.yml", "docker-compose.override.yaml"},
}

// FindFiles returns the compose files of a folder (main file first), or nil.
func FindFiles(dir string) []string {
	for _, name := range FileNames {
		if isFile(filepath.Join(dir, name)) {
			files := []string{filepath.Join(dir, name)}
			for _, o := range OverrideNames[name] {
				if isFile(filepath.Join(dir, o)) {
					files = append(files, filepath.Join(dir, o))
					break
				}
			}
			return files
		}
	}
	return nil
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

type Project struct {
	Name     string             `json:"name"`
	Services map[string]Service `json:"services"`
}

type Service struct {
	Image         string            `json:"image,omitempty"`
	Build         *Build            `json:"build,omitempty"`
	Ports         []PortConfig      `json:"ports,omitempty"`
	Healthcheck   *Healthcheck      `json:"healthcheck,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
	ContainerName string            `json:"container_name,omitempty"`
	Profiles      []string          `json:"profiles,omitempty"`
	Restart       string            `json:"restart,omitempty"`
	Volumes       []VolumeConfig    `json:"volumes,omitempty"`
}

type Build struct {
	Context    string             `json:"context"`
	Dockerfile string             `json:"dockerfile,omitempty"`
	Args       map[string]*string `json:"args,omitempty"`
}

type PortConfig struct {
	Target    int    `json:"target"`
	Published string `json:"published,omitempty"`
	Protocol  string `json:"protocol,omitempty"`
	HostIP    string `json:"host_ip,omitempty"`
}

type Healthcheck struct {
	Test    []string `json:"test,omitempty"`
	Disable bool     `json:"disable,omitempty"`
}

type VolumeConfig struct {
	Type   string `json:"type"`
	Source string `json:"source,omitempty"`
	Target string `json:"target"`
}

// ImageName is the image a service runs: its image:, or for build-only services the name
// compose gives the built image (<project>-<service>).
func (p *Project) ImageName(service string) string {
	s := p.Services[service]
	if s.Image != "" {
		return s.Image
	}
	return p.Name + "-" + service
}

// BuildArgs flattens the service's build args (nil values come from the environment and are
// unknown here).
func (b *Build) BuildArgs() map[string]string {
	out := map[string]string{}
	for k, v := range b.Args {
		if v != nil {
			out[k] = *v
		}
	}
	return out
}

type CLI struct {
	Bin string   // "docker"
	Env []string // extra environment (DOCKER_HOST, …)
}

// New runs plain `docker` with machine-friendly output (no colours, no cursor tricks).
func New() *CLI {
	return &CLI{Bin: "docker", Env: []string{"COMPOSE_ANSI=never", "COMPOSE_PROGRESS=plain"}}
}

// Args builds the global part of a compose command line for a stack.
func Args(dir string, files []string, rest ...string) []string {
	args := []string{"compose", "--project-directory", dir}
	for _, f := range files {
		args = append(args, "-f", f)
	}
	return append(args, rest...)
}

// Cmd prepares a compose command for a stack in its own process group, so cancelling a job
// can kill compose and everything it started.
func (c *CLI) Cmd(ctx context.Context, dir string, files []string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, c.Bin, Args(dir, files, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), c.Env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// ProjectCmd works on a project by name, without files (orphaned stacks: compose finds the
// containers by label for stop/start/restart/down).
func (c *CLI) ProjectCmd(ctx context.Context, project string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, c.Bin, append([]string{"compose", "-p", project}, args...)...)
	cmd.Env = append(os.Environ(), c.Env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// Config loads a project the way compose sees it.
func (c *CLI) Config(ctx context.Context, dir string, files []string) (*Project, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := c.run(ctx, dir, files, "config", "--format", "json")
	if err != nil {
		return nil, err
	}
	var p Project
	if err := json.Unmarshal(out, &p); err != nil {
		return nil, fmt.Errorf("compose config output: %w", err)
	}
	return &p, nil
}

// Validate checks files the way `up` would read them, without starting anything.
func (c *CLI) Validate(ctx context.Context, dir string, files []string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err := c.run(ctx, dir, files, "config", "--quiet")
	return err
}

// ValidateEnv validates with another env file in place of the folder's .env (editing .env).
func (c *CLI) ValidateEnv(ctx context.Context, dir string, files []string, envFile string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	args := []string{"compose", "--project-directory", dir, "--env-file", envFile}
	for _, f := range files {
		args = append(args, "-f", f)
	}
	cmd := exec.CommandContext(ctx, c.Bin, append(args, "config", "--quiet")...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), c.Env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := CleanError(stderr.String()); msg != "" {
			return errors.New(msg)
		}
		return err
	}
	return nil
}

// Version returns the compose plugin version ("v5.6.0").
func (c *CLI) Version(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Bin, "compose", "version", "--short")
	cmd.Env = append(os.Environ(), c.Env...)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func (c *CLI) run(ctx context.Context, dir string, files []string, args ...string) ([]byte, error) {
	cmd := c.Cmd(ctx, dir, files, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := CleanError(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			msg = "timed out: " + msg
		}
		return nil, errors.New(msg)
	}
	return stdout.Bytes(), nil
}

// CleanError turns compose's stderr into a one-paragraph message for the UI.
func CleanError(stderr string) string {
	var lines []string
	for _, l := range strings.Split(stderr, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "WARN[") && strings.Contains(l, "obsolete") {
			continue
		}
		lines = append(lines, l)
	}
	return strings.Join(lines, "\n")
}
