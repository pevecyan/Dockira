package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"docker-manager/internal/alerts"
	"docker-manager/internal/notify"
)

type NotificationsOverview struct {
	Channels   []alerts.ChannelView `json:"channels"`
	Rules      []notify.Rule        `json:"rules"`
	EventTypes []notify.EventType   `json:"event_types"`
	Log        []alerts.LogEntry    `json:"log"`
}

type NotificationsOutput struct{ Body NotificationsOverview }

type ChannelBody struct {
	Type    string          `json:"type" enum:"ntfy,gotify,telegram,webhook"`
	Name    string          `json:"name" minLength:"1" maxLength:"80"`
	Enabled bool            `json:"enabled"`
	Config  json.RawMessage `json:"config" doc:"Type-specific settings; empty secret fields keep their stored value"`
}

type ChannelInput struct {
	Confirm string `cookie:"dm_confirm"`
	Body    ChannelBody
}

type ChannelUpdateInput struct {
	CID     int64  `path:"cid"`
	Confirm string `cookie:"dm_confirm"`
	Body    ChannelBody
}

type ChannelIDInput struct {
	CID int64 `path:"cid"`
}

type ChannelOutput struct{ Body alerts.ChannelView }

type TestOutput struct {
	Body struct {
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	}
}

type RuleInput struct {
	Body notify.Rule
}

type RuleUpdateInput struct {
	RID  int64 `path:"rid"`
	Body notify.Rule
}

type RuleIDInput struct {
	RID int64 `path:"rid"`
}

type RuleOutput struct{ Body notify.Rule }

func (s *Server) registerNotifications() {
	tags := []string{"notifications"}
	huma.Register(s.api, huma.Operation{OperationID: "get-notifications", Method: http.MethodGet, Path: "/api/notifications", Summary: "Channels, rules, event types and recent deliveries", Tags: tags}, s.getNotifications)
	huma.Register(s.api, huma.Operation{OperationID: "create-channel", Method: http.MethodPost, Path: "/api/notifications/channels", Summary: "Add a channel (needs a password confirmation)", Tags: tags}, s.createChannel)
	huma.Register(s.api, huma.Operation{OperationID: "update-channel", Method: http.MethodPut, Path: "/api/notifications/channels/{cid}", Summary: "Change a channel (needs a password confirmation)", Tags: tags}, s.updateChannel)
	huma.Register(s.api, huma.Operation{OperationID: "delete-channel", Method: http.MethodDelete, Path: "/api/notifications/channels/{cid}", Summary: "Remove a channel and its rules", Tags: tags}, s.deleteChannel)
	huma.Register(s.api, huma.Operation{OperationID: "test-channel", Method: http.MethodPost, Path: "/api/notifications/channels/{cid}/test", Summary: "Send a test message", Tags: tags}, s.testChannel)
	huma.Register(s.api, huma.Operation{OperationID: "create-rule", Method: http.MethodPost, Path: "/api/notifications/rules", Summary: "Add a rule", Tags: tags}, s.createRule)
	huma.Register(s.api, huma.Operation{OperationID: "update-rule", Method: http.MethodPut, Path: "/api/notifications/rules/{rid}", Summary: "Change a rule", Tags: tags}, s.updateRule)
	huma.Register(s.api, huma.Operation{OperationID: "delete-rule", Method: http.MethodDelete, Path: "/api/notifications/rules/{rid}", Summary: "Remove a rule", Tags: tags}, s.deleteRule)
}

func (s *Server) getNotifications(ctx context.Context, _ *struct{}) (*NotificationsOutput, error) {
	views, err := s.Alerts.Views(ctx)
	if err != nil {
		return nil, huma.Error500InternalServerError("channels", err)
	}
	rules, _ := s.Alerts.Rules(ctx)
	log, _ := s.Alerts.Log(ctx, 100)
	return &NotificationsOutput{Body: NotificationsOverview{Channels: views, Rules: rules, EventTypes: notify.EventTypes, Log: log}}, nil
}

func (s *Server) channelView(ctx context.Context, id int64) (*ChannelOutput, error) {
	views, err := s.Alerts.Views(ctx)
	if err != nil {
		return nil, huma.Error500InternalServerError("channels", err)
	}
	for _, v := range views {
		if v.ID == id {
			return &ChannelOutput{Body: v}, nil
		}
	}
	return nil, huma.Error404NotFound("no channel")
}

func (s *Server) createChannel(ctx context.Context, in *ChannelInput) (*ChannelOutput, error) {
	if !s.confirmed(in.Confirm) {
		return nil, needConfirm()
	}
	ch, err := s.Alerts.SaveChannel(ctx, notify.Channel{Type: in.Body.Type, Name: in.Body.Name, Enabled: in.Body.Enabled, Config: in.Body.Config})
	s.audit(ctx, "notifications.channel.create", in.Body.Name, map[string]string{"type": in.Body.Type}, err)
	if err != nil {
		return nil, huma.Error422UnprocessableEntity(err.Error())
	}
	return s.channelView(ctx, ch.ID)
}

func (s *Server) updateChannel(ctx context.Context, in *ChannelUpdateInput) (*ChannelOutput, error) {
	if !s.confirmed(in.Confirm) {
		return nil, needConfirm()
	}
	_, err := s.Alerts.SaveChannel(ctx, notify.Channel{ID: in.CID, Type: in.Body.Type, Name: in.Body.Name, Enabled: in.Body.Enabled, Config: in.Body.Config})
	s.audit(ctx, "notifications.channel.update", in.Body.Name, map[string]any{"id": in.CID}, err)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, huma.Error404NotFound("no channel")
	}
	if err != nil {
		return nil, huma.Error422UnprocessableEntity(err.Error())
	}
	return s.channelView(ctx, in.CID)
}

func (s *Server) deleteChannel(ctx context.Context, in *ChannelIDInput) (*struct{}, error) {
	err := s.Alerts.DeleteChannel(ctx, in.CID)
	s.audit(ctx, "notifications.channel.delete", "", map[string]any{"id": in.CID}, err)
	if err != nil {
		return nil, huma.Error500InternalServerError("delete", err)
	}
	return nil, nil
}

func (s *Server) testChannel(ctx context.Context, in *ChannelIDInput) (*TestOutput, error) {
	out := &TestOutput{}
	err := s.Alerts.Test(ctx, in.CID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, huma.Error404NotFound("no channel")
	}
	out.Body.OK = err == nil
	if err != nil {
		out.Body.Error = err.Error()
	}
	return out, nil
}

func (s *Server) createRule(ctx context.Context, in *RuleInput) (*RuleOutput, error) {
	in.Body.ID = 0
	r, err := s.Alerts.SaveRule(ctx, in.Body)
	s.audit(ctx, "notifications.rule.create", "", in.Body, err)
	if err != nil {
		return nil, huma.Error422UnprocessableEntity(err.Error())
	}
	return &RuleOutput{Body: r}, nil
}

func (s *Server) updateRule(ctx context.Context, in *RuleUpdateInput) (*RuleOutput, error) {
	in.Body.ID = in.RID
	r, err := s.Alerts.SaveRule(ctx, in.Body)
	s.audit(ctx, "notifications.rule.update", "", in.Body, err)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, huma.Error404NotFound("no rule")
	}
	if err != nil {
		return nil, huma.Error422UnprocessableEntity(err.Error())
	}
	return &RuleOutput{Body: r}, nil
}

func (s *Server) deleteRule(ctx context.Context, in *RuleIDInput) (*struct{}, error) {
	err := s.Alerts.DeleteRule(ctx, in.RID)
	s.audit(ctx, "notifications.rule.delete", "", map[string]any{"id": in.RID}, err)
	if err != nil {
		return nil, huma.Error500InternalServerError("delete", err)
	}
	return nil, nil
}
