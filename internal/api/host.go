package api

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"docker-manager/internal/docker"
	"docker-manager/internal/hoststats"
)

type HostInfo struct {
	Host           hoststats.Host                 `json:"host"`
	System         docker.System                  `json:"system"`
	Disk           *docker.DiskUsage              `json:"disk,omitempty" doc:"docker system df (cached for a minute)"`
	Containers     map[string]hoststats.Container `json:"containers" doc:"CPU and memory of running containers, by ID"`
	ComposeVersion string                         `json:"compose_version"`
	DockerError    string                         `json:"docker_error,omitempty"`
}

type HostOutput struct{ Body HostInfo }

type FeedOutput struct {
	Body struct {
		Items []FeedItem `json:"items"`
	}
}

func (s *Server) registerHost() {
	huma.Register(s.api, huma.Operation{OperationID: "get-host", Method: http.MethodGet, Path: "/api/host",
		Summary: "Host and Docker engine figures", Tags: []string{"system"}}, s.host)
	huma.Register(s.api, huma.Operation{OperationID: "get-feed", Method: http.MethodGet, Path: "/api/feed",
		Summary: "Recent Docker events and job messages", Tags: []string{"system"}}, s.getFeed)
}

// cached keeps a slow value (docker system df takes seconds on a busy host) for ttl.
type cached[T any] struct {
	mu  sync.Mutex
	at  time.Time
	val T
	err error
}

func (c *cached[T]) get(ttl time.Duration, load func() (T, error)) (T, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.at) > ttl {
		c.val, c.err = load()
		c.at = time.Now()
	}
	return c.val, c.err
}

func (s *Server) host(ctx context.Context, _ *struct{}) (*HostOutput, error) {
	out := HostInfo{}
	out.Host, _ = s.Stats.Host()
	sys, err := s.system.get(30*time.Second, func() (docker.System, error) { return s.Docker.System(ctx) })
	if err != nil {
		out.DockerError = err.Error()
	}
	out.System = sys
	if du, err := s.diskUsage.get(time.Minute, func() (docker.DiskUsage, error) { return s.Docker.DiskUsage(ctx) }); err == nil {
		out.Disk = &du
	}
	var ids []string
	for _, c := range s.Stacks.Containers() {
		if c.State == "running" {
			ids = append(ids, c.ID)
		}
	}
	out.Containers = s.Stats.Containers(ids)
	out.ComposeVersion, _ = s.composeVersion.get(time.Hour, func() (string, error) { return s.Compose.Version(ctx) })
	return &HostOutput{Body: out}, nil
}

func (s *Server) getFeed(ctx context.Context, _ *struct{}) (*FeedOutput, error) {
	out := &FeedOutput{}
	out.Body.Items = s.feed.list()
	return out, nil
}
