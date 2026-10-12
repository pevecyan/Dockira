package api

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"docker-manager/internal/docker"
	"docker-manager/internal/stacks"
)

type StackList struct {
	Stacks      []stacks.Stack     `json:"stacks"`
	Standalone  []docker.Container `json:"standalone" doc:"Containers not started by compose"`
	ScannedAt   time.Time          `json:"scanned_at"`
	DockerError string             `json:"docker_error,omitempty"`
}

type StackListOutput struct{ Body StackList }

type StackIDInput struct {
	ID string `path:"id" doc:"Stack ID (its folder relative to apps_root), URL-encoded"`
}

type StackDetail struct {
	stacks.Stack
	Details []docker.Details `json:"details" doc:"Inspect data of the stack's containers"`
}

type StackDetailOutput struct{ Body StackDetail }

func (s *Server) registerStacks() {
	huma.Register(s.api, huma.Operation{OperationID: "list-stacks", Method: http.MethodGet, Path: "/api/stacks",
		Summary: "All stacks with their services and containers", Tags: []string{"stacks"}}, s.listStacks)
	huma.Register(s.api, huma.Operation{OperationID: "rescan-stacks", Method: http.MethodPost, Path: "/api/stacks/rescan",
		Summary: "Scan apps_root and Docker again", Tags: []string{"stacks"}}, s.rescan)
	huma.Register(s.api, huma.Operation{OperationID: "get-stack", Method: http.MethodGet, Path: "/api/stacks/{id}",
		Summary: "One stack with container details", Tags: []string{"stacks"}}, s.getStack)
}

func (s *Server) stackList() StackList {
	l := StackList{Stacks: s.Stacks.Stacks(), Standalone: s.Stacks.Standalone(), ScannedAt: s.Stacks.Scanned()}
	if err := s.Stacks.DockerError(); err != nil {
		l.DockerError = err.Error()
	}
	if l.Stacks == nil {
		l.Stacks = []stacks.Stack{}
	}
	if l.Standalone == nil {
		l.Standalone = []docker.Container{}
	}
	return l
}

func (s *Server) listStacks(ctx context.Context, _ *struct{}) (*StackListOutput, error) {
	return &StackListOutput{Body: s.stackList()}, nil
}

func (s *Server) rescan(ctx context.Context, _ *struct{}) (*StackListOutput, error) {
	if err := s.Stacks.Scan(ctx); err != nil {
		s.Log.Warn("rescan", "err", err)
	}
	l := s.stackList()
	s.Hub.Publish("stacks", l)
	return &StackListOutput{Body: l}, nil
}

func (s *Server) stack(id string) (stacks.Stack, error) {
	st, ok := s.Stacks.Get(id)
	if !ok {
		return st, huma.Error404NotFound("no stack " + id)
	}
	return st, nil
}

func (s *Server) getStack(ctx context.Context, in *StackIDInput) (*StackDetailOutput, error) {
	st, err := s.stack(in.ID)
	if err != nil {
		return nil, err
	}
	out := StackDetail{Stack: st, Details: []docker.Details{}}
	for _, sv := range st.Services {
		for _, c := range sv.Containers {
			if d, err := s.Docker.Inspect(ctx, c.ID); err == nil {
				out.Details = append(out.Details, d)
			}
		}
	}
	sort.Slice(out.Details, func(i, j int) bool { return out.Details[i].Name < out.Details[j].Name })
	return &StackDetailOutput{Body: out}, nil
}
