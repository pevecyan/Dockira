package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"docker-manager/internal/dotenv"
	"docker-manager/internal/jobs"
	"docker-manager/internal/stacks"
)

// StackActions are the compose verbs the UI offers. down_volumes also removes named volumes
// (data!), so it needs a password confirmation.
var StackActions = []string{"up", "start", "stop", "restart", "down", "down_volumes", "pull", "recreate", "update", "rebuild"}

type ActionBody struct {
	Action   string   `json:"action" enum:"up,start,stop,restart,down,down_volumes,pull,recreate,update,rebuild"`
	Services []string `json:"services,omitempty" doc:"Limit to these services (default: the whole stack)"`
}

type ActionInput struct {
	ID      string `path:"id"`
	Confirm string `cookie:"dm_confirm"`
	Body    ActionBody
}

type JobOutput struct{ Body jobs.Job }

type ContainerActionInput struct {
	ID   string `path:"id"`
	Body struct {
		Action string `json:"action" enum:"start,stop,restart,pause,unpause"`
	}
}

type JobListInput struct {
	Stack  string `query:"stack"`
	Before int64  `query:"before"`
	Limit  int    `query:"limit" default:"50" minimum:"1" maximum:"500"`
}

type JobList struct {
	Jobs   []jobs.Job `json:"jobs"`
	Active []jobs.Job `json:"active"`
}

type JobListOutput struct{ Body JobList }

type JobIDInput struct {
	ID int64 `path:"id"`
}

type JobDetail struct {
	Job    jobs.Job `json:"job"`
	Output string   `json:"output"`
}

type JobDetailOutput struct{ Body JobDetail }

// ProblemError is an error body with extra flags the UI acts on (confirm: ask for the password).
type ProblemError struct {
	Status  int    `json:"status"`
	Title   string `json:"title"`
	Detail  string `json:"detail"`
	Confirm bool   `json:"confirm,omitempty"`
}

func (e *ProblemError) Error() string  { return e.Detail }
func (e *ProblemError) GetStatus() int { return e.Status }

func needConfirm() error {
	return &ProblemError{Status: http.StatusForbidden, Title: "Forbidden", Detail: "confirm your password to continue", Confirm: true}
}

type ctxKey int

const clientIPKey ctxKey = 0

func clientIP(ctx context.Context) string {
	ip, _ := ctx.Value(clientIPKey).(string)
	return ip
}

// audit records a change with the caller's address.
func (s *Server) audit(ctx context.Context, action, target string, details any, err error) {
	if s.Store == nil {
		return
	}
	b, _ := json.Marshal(details)
	result := "ok"
	if err != nil {
		result = "error: " + err.Error()
	}
	s.Store.Audit(context.WithoutCancel(ctx), clientIP(ctx), action, target, string(b), result)
}

func (s *Server) registerActions() {
	huma.Register(s.api, huma.Operation{OperationID: "stack-action", Method: http.MethodPost, Path: "/api/stacks/{id}/actions",
		Summary: "Run a compose action on a stack (as a job)", Tags: []string{"stacks"}}, s.stackAction)
	huma.Register(s.api, huma.Operation{OperationID: "container-action", Method: http.MethodPost, Path: "/api/containers/{id}/actions",
		Summary: "Start, stop, restart, pause or unpause one container", Tags: []string{"containers"}}, s.containerAction)
	huma.Register(s.api, huma.Operation{OperationID: "list-jobs", Method: http.MethodGet, Path: "/api/jobs",
		Summary: "Recent jobs", Tags: []string{"jobs"}}, s.listJobs)
	huma.Register(s.api, huma.Operation{OperationID: "get-job", Method: http.MethodGet, Path: "/api/jobs/{id}",
		Summary: "One job with its output", Tags: []string{"jobs"}}, s.getJob)
	huma.Register(s.api, huma.Operation{OperationID: "cancel-job", Method: http.MethodPost, Path: "/api/jobs/{id}/cancel",
		Summary: "Cancel a queued or running job", Tags: []string{"jobs"}}, s.cancelJob)
	s.mux.HandleFunc("GET /api/jobs/{id}/stream", s.streamJob)
}

func (s *Server) stackAction(ctx context.Context, in *ActionInput) (*JobOutput, error) {
	st, err := s.stack(in.ID)
	if err != nil {
		return nil, err
	}
	if in.Body.Action == "down_volumes" && !s.confirmed(in.Confirm) {
		return nil, needConfirm()
	}
	spec, err := s.ComposeJob(st, in.Body.Action, in.Body.Services, "user")
	if err != nil {
		return nil, err
	}
	if s.Alerts != nil {
		s.Alerts.Expect(st.ID, spec.Timeout)
	}
	j, err := s.Jobs.Submit(spec)
	s.audit(ctx, "stack."+in.Body.Action, st.ID, map[string]any{"services": in.Body.Services, "job": j.ID}, err)
	if err != nil {
		return nil, huma.Error500InternalServerError("submit job", err)
	}
	return &JobOutput{Body: j}, nil
}

func (s *Server) confirmed(cookie string) bool {
	return s.Auth == nil || s.Auth.ConfirmedValue(cookie)
}

// ComposeJob builds the job for a compose action on a stack.
func (s *Server) ComposeJob(st stacks.Stack, action string, services []string, trigger string) (jobs.Spec, error) {
	if !slices.Contains(StackActions, action) {
		return jobs.Spec{}, huma.Error400BadRequest("unknown action " + action)
	}
	known := map[string]bool{}
	for _, sv := range st.Services {
		known[sv.Name] = true
	}
	for _, sv := range services {
		if !known[sv] {
			return jobs.Spec{}, huma.Error400BadRequest(fmt.Sprintf("stack %s has no service %q", st.ID, sv))
		}
	}
	if st.Self {
		switch action {
		case "stop", "down", "down_volumes":
			return jobs.Spec{}, huma.Error409Conflict("this is Dockira's own stack: stopping it would cut off the UI")
		}
	}
	if len(services) > 0 && (action == "down" || action == "down_volumes") {
		return jobs.Spec{}, huma.Error400BadRequest("down works on the whole stack")
	}
	if st.Orphaned && !slices.Contains([]string{"start", "stop", "restart", "down"}, action) {
		return jobs.Spec{}, huma.Error409Conflict("the stack's compose files are missing: only start, stop, restart and down work")
	}
	if !st.Orphaned && st.Status == "invalid" && action != "down" && action != "stop" {
		return jobs.Spec{}, huma.Error409Conflict("the compose file is invalid: " + st.Error)
	}

	label := st.ID
	if len(services) > 0 {
		label += " (" + strings.Join(services, ", ") + ")"
	}
	titles := map[string]string{"up": "Start", "start": "Start", "stop": "Stop", "restart": "Restart", "down": "Take down",
		"down_volumes": "Take down with volumes", "pull": "Pull", "recreate": "Recreate", "update": "Update", "rebuild": "Rebuild"}
	spec := jobs.Spec{StackID: st.ID, Kind: action, Title: titles[action] + " " + label, Services: services, Trigger: trigger,
		Secrets: s.stackSecrets(st), Timeout: 10 * time.Minute}
	if action == "pull" || action == "update" || action == "rebuild" {
		spec.Timeout = 45 * time.Minute
	}

	cmd := func(args ...string) [][]string { return [][]string{args} }
	var steps [][]string
	switch action {
	case "up":
		steps = cmd(append([]string{"up", "-d"}, services...)...)
	case "start", "stop", "restart":
		steps = cmd(append([]string{action}, services...)...)
	case "down":
		steps = cmd("down")
	case "down_volumes":
		steps = cmd("down", "--volumes")
	case "pull":
		steps = cmd(append([]string{"pull"}, services...)...)
	case "recreate":
		steps = cmd(append([]string{"up", "-d", "--force-recreate"}, services...)...)
	case "update":
		steps = [][]string{append([]string{"pull"}, services...), append([]string{"up", "-d"}, services...)}
	case "rebuild":
		steps = [][]string{append([]string{"build", "--pull"}, services...), append([]string{"up", "-d"}, services...)}
	}
	if action == "update" && st.Self {
		// pull here, then let a helper replace our container (no rollback: we'd be gone by then)
		return s.selfSpec(st, spec, steps...)
	}
	if action == "update" && s.Updates != nil {
		return s.Updates.UpdateSpec(st, services, trigger, spec)
	}
	if action == "rebuild" && s.Updates != nil {
		if len(services) == 0 {
			for _, sv := range st.Services {
				if sv.Build && sv.InFile {
					services = append(services, sv.Name)
				}
			}
		}
		spec, err := s.Updates.UpdateSpec(st, services, trigger, spec)
		spec.Kind = "rebuild"
		return spec, err
	}
	if st.Self && (action == "up" || action == "restart" || action == "recreate") {
		return s.selfSpec(st, spec, steps[len(steps)-1])
	}
	spec.Run = func(ctx context.Context, out *jobs.Output) error {
		for _, args := range steps {
			out.Printf("▸ docker compose %s", strings.Join(args, " "))
			c := s.Compose.Cmd(ctx, st.Dir, st.Files, args...)
			if st.Orphaned {
				c = s.Compose.ProjectCmd(ctx, st.Name, args...)
			}
			if err := out.Exec(c); err != nil {
				return err
			}
		}
		s.afterChange(st)
		return nil
	}
	return spec, nil
}

// selfSpec runs compose commands on docker-manager's own stack. The last one runs from a
// short-lived helper container (the same image, which has the compose CLI): recreating our own
// container would otherwise kill the process running compose halfway. Earlier ones (a pull)
// don't touch the container and run here.
func (s *Server) selfSpec(st stacks.Stack, spec jobs.Spec, steps ...[]string) (jobs.Spec, error) {
	pre, args := steps[:len(steps)-1], steps[len(steps)-1]
	spec.Run = func(ctx context.Context, out *jobs.Output) error {
		for _, a := range pre {
			out.Printf("▸ docker compose %s", strings.Join(a, " "))
			if err := out.Exec(s.Compose.Cmd(ctx, st.Dir, st.Files, a...)); err != nil {
				return err
			}
		}
		self := ""
		for _, sv := range st.Services {
			for _, c := range sv.Containers {
				if c.ID != "" && st.Self {
					self = c.ImageID
				}
			}
		}
		if self == "" {
			return fmt.Errorf("can't find Dockira's own image")
		}
		cmd := append([]string{"run", "--rm", "-d", "--name", "docker-manager-helper-" + strconv.FormatInt(time.Now().Unix(), 10),
			"-v", "/var/run/docker.sock:/var/run/docker.sock", "-v", st.Dir + ":" + st.Dir, "-w", st.Dir,
			"--entrypoint", "docker", self}, "compose", "--project-directory", st.Dir)
		for _, f := range st.Files {
			cmd = append(cmd, "-f", f)
		}
		cmd = append(cmd, args...)
		out.Printf("▸ Dockira's own stack: running `docker compose %s` from a helper container", strings.Join(args, " "))
		out.Printf("  the UI reconnects when the new container is up")
		return out.Exec(exec.CommandContext(ctx, s.Compose.Bin, cmd...))
	}
	return spec, nil
}

// afterChange refreshes the stack right away instead of waiting for Docker events to settle.
func (s *Server) afterChange(st stacks.Stack) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = s.Stacks.RefreshContainers(ctx)
	s.publishStacks()
}

// stackSecrets are the stack's .env values, scrubbed from job output.
func (s *Server) stackSecrets(st stacks.Stack) []string {
	if !st.EnvFile {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(st.Dir, ".env"))
	if err != nil {
		return nil
	}
	return dotenv.Values(string(b))
}

func (s *Server) containerAction(ctx context.Context, in *ContainerActionInput) (*struct{}, error) {
	var found bool
	var self bool
	var name string
	for _, st := range s.Stacks.Stacks() {
		for _, sv := range st.Services {
			for _, c := range sv.Containers {
				if c.ID == in.ID || c.Name == in.ID {
					found, self, name = true, st.Self, c.Name
				}
			}
		}
	}
	for _, c := range s.Stacks.Standalone() {
		if c.ID == in.ID || c.Name == in.ID {
			found, name = true, c.Name
		}
	}
	if !found {
		return nil, huma.Error404NotFound("no container " + in.ID)
	}
	if self && (in.Body.Action == "stop" || in.Body.Action == "pause") {
		return nil, huma.Error409Conflict("this is Dockira's own container")
	}
	if s.Alerts != nil {
		for _, st := range s.Stacks.Stacks() {
			for _, sv := range st.Services {
				for _, c := range sv.Containers {
					if c.ID == in.ID || c.Name == in.ID {
						s.Alerts.Expect(st.ID, time.Minute)
					}
				}
			}
		}
	}
	err := s.Docker.ContainerAction(ctx, in.ID, in.Body.Action)
	s.audit(ctx, "container."+in.Body.Action, name, nil, err)
	if err != nil {
		return nil, huma.Error502BadGateway(in.Body.Action+" "+name, err)
	}
	go func() {
		_ = s.Stacks.RefreshContainers(context.Background())
		s.publishStacks()
	}()
	return nil, nil
}

func (s *Server) listJobs(ctx context.Context, in *JobListInput) (*JobListOutput, error) {
	list, err := s.Jobs.List(ctx, in.Stack, in.Before, in.Limit)
	if err != nil {
		return nil, huma.Error500InternalServerError("jobs", err)
	}
	active := s.Jobs.Active()
	if active == nil {
		active = []jobs.Job{}
	}
	return &JobListOutput{Body: JobList{Jobs: list, Active: active}}, nil
}

func (s *Server) getJob(ctx context.Context, in *JobIDInput) (*JobDetailOutput, error) {
	j, out, err := s.Jobs.Get(ctx, in.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, huma.Error404NotFound("no job " + strconv.FormatInt(in.ID, 10))
	}
	if err != nil {
		return nil, huma.Error500InternalServerError("job", err)
	}
	return &JobDetailOutput{Body: JobDetail{Job: j, Output: out}}, nil
}

func (s *Server) cancelJob(ctx context.Context, in *JobIDInput) (*struct{}, error) {
	if !s.Jobs.Cancel(in.ID) {
		return nil, huma.Error409Conflict("the job is not running")
	}
	s.audit(ctx, "job.cancel", strconv.FormatInt(in.ID, 10), nil, nil)
	return nil, nil
}

// streamJob sends a job's output so far, then new lines, then the final state.
func (s *Server) streamJob(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad job id")
		return
	}
	lines, ch, unsub, live := s.Jobs.Subscribe(id)
	defer unsub()
	if !live {
		j, out, err := s.Jobs.Get(r.Context(), id)
		if err != nil {
			writeErr(w, http.StatusNotFound, "no job %d", id)
			return
		}
		send, ok := sse(w)
		if !ok {
			return
		}
		send("lines", strings.Split(strings.TrimRight(out, "\n"), "\n"))
		send("done", j)
		return
	}
	send, ok := sse(w)
	if !ok {
		return
	}
	if len(lines) > 0 && send("lines", lines) != nil {
		return
	}
	batch := []string{}
	tick := time.NewTicker(150 * time.Millisecond) // batch lines: compose pull prints hundreds per second
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case l, ok := <-ch:
			if !ok {
				if len(batch) > 0 {
					send("lines", batch)
				}
				j, _, _ := s.Jobs.Get(r.Context(), id)
				send("done", j)
				return
			}
			batch = append(batch, l)
		case <-tick.C:
			if len(batch) > 0 {
				if send("lines", batch) != nil {
					return
				}
				batch = batch[:0]
			}
		}
	}
}
