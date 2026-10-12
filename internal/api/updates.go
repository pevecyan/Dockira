package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"docker-manager/internal/jobs"
	"docker-manager/internal/updates"
)

type UpdatesOverview struct {
	Summary  updates.Summary         `json:"summary"`
	Services []updates.ServiceUpdate `json:"services"`
	Runs     []updates.Run           `json:"runs"`
}

type UpdatesOutput struct{ Body UpdatesOverview }

type CheckInput struct {
	Body struct {
		Stack string `json:"stack,omitempty" doc:"Only this stack (default: all)"`
	}
}

type UpdateSettingsOutput struct{ Body updates.Settings }

type UpdateSettingsInput struct {
	Body updates.Settings
}

type StackUpdates struct {
	Services  []updates.ServiceUpdate `json:"services"`
	Runs      []updates.Run           `json:"runs"`
	Rollbacks []updates.RollbackPoint `json:"rollbacks"`
	Policy    updates.StackPolicy     `json:"policy"`
}

type StackUpdatesOutput struct{ Body StackUpdates }

type PolicyInput struct {
	ID   string `path:"id"`
	Body updates.StackPolicy
}

type RollbackInput struct {
	ID    string `path:"id"`
	Point int64  `path:"point"`
}

func (s *Server) registerUpdates() {
	tags := []string{"updates"}
	huma.Register(s.api, huma.Operation{OperationID: "get-updates", Method: http.MethodGet, Path: "/api/updates", Summary: "Update state of every service", Tags: tags}, s.getUpdates)
	huma.Register(s.api, huma.Operation{OperationID: "check-updates", Method: http.MethodPost, Path: "/api/updates/check", Summary: "Look for updates now (a job)", Tags: tags}, s.checkUpdates)
	huma.Register(s.api, huma.Operation{OperationID: "get-update-settings", Method: http.MethodGet, Path: "/api/updates/settings", Summary: "Check schedule, maintenance window, defaults", Tags: tags}, s.getUpdateSettings)
	huma.Register(s.api, huma.Operation{OperationID: "put-update-settings", Method: http.MethodPut, Path: "/api/updates/settings", Summary: "Change update settings", Tags: tags}, s.putUpdateSettings)
	huma.Register(s.api, huma.Operation{OperationID: "get-stack-updates", Method: http.MethodGet, Path: "/api/stacks/{id}/updates", Summary: "A stack's update state, history, rollback points and policy", Tags: tags}, s.getStackUpdates)
	huma.Register(s.api, huma.Operation{OperationID: "put-stack-policy", Method: http.MethodPut, Path: "/api/stacks/{id}/policy", Summary: "Set a stack's update policy", Tags: tags}, s.putPolicy)
	huma.Register(s.api, huma.Operation{OperationID: "rollback", Method: http.MethodPost, Path: "/api/stacks/{id}/rollbacks/{point}", Summary: "Go back to a rollback point (a job)", Tags: tags}, s.rollback)
}

func (s *Server) getUpdates(ctx context.Context, _ *struct{}) (*UpdatesOutput, error) {
	svcs, err := s.Updates.Services(ctx, "")
	if err != nil {
		return nil, huma.Error500InternalServerError("updates", err)
	}
	runs, _ := s.Updates.Runs(ctx, "", 30)
	return &UpdatesOutput{Body: UpdatesOverview{Summary: s.Updates.Summary(ctx), Services: svcs, Runs: runs}}, nil
}

func (s *Server) checkUpdates(ctx context.Context, in *CheckInput) (*JobOutput, error) {
	if in.Body.Stack != "" {
		if _, err := s.stack(in.Body.Stack); err != nil {
			return nil, err
		}
	}
	j, err := s.Jobs.Submit(s.Updates.CheckSpec(in.Body.Stack, "user"))
	if err != nil {
		return nil, huma.Error500InternalServerError("check", err)
	}
	return &JobOutput{Body: j}, nil
}

func (s *Server) getUpdateSettings(ctx context.Context, _ *struct{}) (*UpdateSettingsOutput, error) {
	return &UpdateSettingsOutput{Body: s.Updates.Settings(ctx)}, nil
}

func (s *Server) putUpdateSettings(ctx context.Context, in *UpdateSettingsInput) (*UpdateSettingsOutput, error) {
	err := s.Updates.SaveSettings(ctx, in.Body)
	s.audit(ctx, "updates.settings", "", in.Body, err)
	if err != nil {
		return nil, huma.Error422UnprocessableEntity(err.Error())
	}
	return &UpdateSettingsOutput{Body: s.Updates.Settings(ctx)}, nil
}

func (s *Server) getStackUpdates(ctx context.Context, in *StackIDInput) (*StackUpdatesOutput, error) {
	if _, err := s.stack(in.ID); err != nil {
		return nil, err
	}
	svcs, err := s.Updates.Services(ctx, in.ID)
	if err != nil {
		return nil, huma.Error500InternalServerError("updates", err)
	}
	runs, _ := s.Updates.Runs(ctx, in.ID, 30)
	pts, _ := s.Updates.RollbackPoints(ctx, in.ID)
	pol, _ := s.Updates.StackPolicy(ctx, in.ID)
	return &StackUpdatesOutput{Body: StackUpdates{Services: svcs, Runs: runs, Rollbacks: pts, Policy: pol}}, nil
}

func (s *Server) putPolicy(ctx context.Context, in *PolicyInput) (*StackUpdatesOutput, error) {
	st, err := s.stack(in.ID)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, sv := range st.Services {
		known[sv.Name] = true
	}
	for _, h := range in.Body.Stack.Hooks {
		if h.Type != "volumes" && !known[h.Service] {
			return nil, huma.Error422UnprocessableEntity("hook: no service " + h.Service)
		}
	}
	err = s.Updates.SaveStackPolicy(ctx, st.ID, in.Body)
	s.audit(ctx, "updates.policy", st.ID, in.Body, err)
	if err != nil {
		return nil, huma.Error422UnprocessableEntity(err.Error())
	}
	return s.getStackUpdates(ctx, &StackIDInput{ID: st.ID})
}

func (s *Server) rollback(ctx context.Context, in *RollbackInput) (*JobOutput, error) {
	st, err := s.stack(in.ID)
	if err != nil {
		return nil, err
	}
	spec, err := s.Updates.RollbackSpec(ctx, st, in.Point)
	if err != nil {
		return nil, huma.Error404NotFound("no rollback point")
	}
	var j jobs.Job
	j, err = s.Jobs.Submit(spec)
	s.audit(ctx, "stack.rollback", st.ID, map[string]any{"point": in.Point, "job": j.ID}, err)
	if err != nil {
		return nil, huma.Error500InternalServerError("rollback", err)
	}
	return &JobOutput{Body: j}, nil
}
