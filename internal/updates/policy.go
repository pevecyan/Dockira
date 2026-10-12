package updates

import (
	"context"
	"encoding/json"
	"fmt"
)

// Hook runs before an update of the stack (manual or automatic); a failing hook stops the update.
type Hook struct {
	Type    string `json:"type" enum:"pg_dump,command,volumes" doc:"pg_dump: dump all databases of a postgres service to data_dir/backups; command: run a command in a service's container; volumes: snapshot the stack's volumes"`
	Service string `json:"service,omitempty" doc:"Service whose container runs the hook (pg_dump, command)"`
	Command string `json:"command,omitempty" doc:"For type command: run with sh -c"`
	Keep    int    `json:"keep,omitempty" doc:"pg_dump: dumps kept per service (default 5); volumes: snapshots kept (default 3)"`
	Stop    bool   `json:"stop,omitempty" doc:"volumes: stop the stack while copying (consistent databases)"`
	Binds   bool   `json:"binds,omitempty" doc:"volumes: also bind-mounted folders inside the stack folder"`
}

// Policy is a stack's update policy (Service "") or a service's override. Empty Mode inherits.
type Policy struct {
	StackID      string `json:"stack_id"`
	Service      string `json:"service"`
	Mode         string `json:"mode" enum:",manual,notify,auto" doc:"Empty: inherit (service → stack → default)"`
	AutoRollback bool   `json:"auto_rollback" doc:"Roll back right away when the updated service doesn't become healthy"`
	Rebuild      bool   `json:"rebuild" doc:"Automatic: rebuild built services when their base image changes"`
	IncludeDB    bool   `json:"include_db" doc:"Automatic: also update services that look like databases"`
	Hooks        []Hook `json:"hooks"`
}

func (p Policy) Validate() error {
	switch p.Mode {
	case "", "manual", "notify", "auto":
	default:
		return fmt.Errorf("mode %q", p.Mode)
	}
	for _, h := range p.Hooks {
		switch {
		case h.Type != "pg_dump" && h.Type != "command" && h.Type != "volumes":
			return fmt.Errorf("hook type %q", h.Type)
		case h.Service == "" && h.Type != "volumes":
			return fmt.Errorf("%s hook: pick the service it runs in", h.Type)
		case h.Type == "command" && h.Command == "":
			return fmt.Errorf("command hook: the command is empty")
		}
	}
	return nil
}

type policyKey struct{ stack, service string }

func (m *Manager) policies(ctx context.Context) (map[policyKey]Policy, error) {
	rs, err := m.Store.DB.QueryContext(ctx, `SELECT stack_id, service, mode, auto_rollback, rebuild, include_db, hooks FROM update_policies`)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := map[policyKey]Policy{}
	for rs.Next() {
		var p Policy
		var hooks string
		if err := rs.Scan(&p.StackID, &p.Service, &p.Mode, &p.AutoRollback, &p.Rebuild, &p.IncludeDB, &hooks); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(hooks), &p.Hooks)
		if p.Hooks == nil {
			p.Hooks = []Hook{}
		}
		out[policyKey{p.StackID, p.Service}] = p
	}
	return out, rs.Err()
}

// effective merges defaults ← stack ← service. Hooks and flags come from the stack row.
func effective(pols map[policyKey]Policy, set Settings, stackID, service string) Policy {
	p := Policy{StackID: stackID, Service: service, Mode: set.DefaultMode, AutoRollback: true, Hooks: []Hook{}}
	if sp, ok := pols[policyKey{stackID, ""}]; ok {
		if sp.Mode != "" {
			p.Mode = sp.Mode
		}
		p.AutoRollback, p.Rebuild, p.IncludeDB, p.Hooks = sp.AutoRollback, sp.Rebuild, sp.IncludeDB, sp.Hooks
	}
	if service != "" {
		if sv, ok := pols[policyKey{stackID, service}]; ok && sv.Mode != "" {
			p.Mode = sv.Mode
		}
	}
	return p
}

// StackPolicy is the stack's own row plus per-service mode overrides.
type StackPolicy struct {
	Stack    Policy            `json:"stack"`
	Services map[string]string `json:"services" doc:"Service → mode override (empty: inherit)"`
	Default  string            `json:"default" doc:"The global default mode"`
}

func (m *Manager) StackPolicy(ctx context.Context, stackID string) (StackPolicy, error) {
	pols, err := m.policies(ctx)
	if err != nil {
		return StackPolicy{}, err
	}
	out := StackPolicy{Services: map[string]string{}, Default: m.Settings(ctx).DefaultMode}
	if p, ok := pols[policyKey{stackID, ""}]; ok {
		out.Stack = p
	} else {
		out.Stack = Policy{StackID: stackID, AutoRollback: true, Hooks: []Hook{}}
	}
	for k, p := range pols {
		if k.stack == stackID && k.service != "" && p.Mode != "" {
			out.Services[k.service] = p.Mode
		}
	}
	return out, nil
}

func (m *Manager) SaveStackPolicy(ctx context.Context, stackID string, sp StackPolicy) error {
	sp.Stack.StackID, sp.Stack.Service = stackID, ""
	if err := sp.Stack.Validate(); err != nil {
		return err
	}
	for svc, mode := range sp.Services {
		if err := (Policy{Mode: mode}).Validate(); err != nil {
			return fmt.Errorf("%s: %w", svc, err)
		}
	}
	tx, err := m.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if sp.Stack.Hooks == nil {
		sp.Stack.Hooks = []Hook{}
	}
	hooks, _ := json.Marshal(sp.Stack.Hooks)
	if _, err := tx.ExecContext(ctx, `DELETE FROM update_policies WHERE stack_id = ?`, stackID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO update_policies (stack_id, service, mode, auto_rollback, rebuild, include_db, hooks) VALUES (?, '', ?, ?, ?, ?, ?)`,
		stackID, sp.Stack.Mode, sp.Stack.AutoRollback, sp.Stack.Rebuild, sp.Stack.IncludeDB, string(hooks)); err != nil {
		return err
	}
	for svc, mode := range sp.Services {
		if mode == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO update_policies (stack_id, service, mode) VALUES (?, ?, ?)`, stackID, svc, mode); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	m.publish(ctx)
	return nil
}
