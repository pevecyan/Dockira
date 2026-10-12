package api

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"docker-manager/internal/compose"
	"docker-manager/internal/files"
	"docker-manager/internal/jobs"
)

type NewAppBody struct {
	Path    string `json:"path" doc:"Folder relative to apps_root, e.g. myapp or tools/myapp" minLength:"1" maxLength:"200"`
	Compose string `json:"compose" maxLength:"1048576"`
	Env     string `json:"env,omitempty" maxLength:"1048576" doc:"Content of .env (optional)"`
	Start   bool   `json:"start,omitempty" doc:"docker compose up -d right away (pulls the images)"`
}

type NewAppInput struct {
	Body NewAppBody
}

type NewAppResult struct {
	Stack string    `json:"stack" doc:"The new stack's ID"`
	Job   *jobs.Job `json:"job,omitempty"`
}

type NewAppOutput struct{ Body NewAppResult }

type FoldersOutput struct {
	Body struct {
		Root    string   `json:"root"`
		Depth   int      `json:"depth"`
		Folders []string `json:"folders" doc:"Folders that group apps (not apps themselves)"`
	}
}

func (s *Server) registerApps() {
	tags := []string{"stacks"}
	huma.Register(s.api, huma.Operation{OperationID: "validate-new-app", Method: http.MethodPost, Path: "/api/apps/validate", Summary: "Check a new app before creating it", Tags: tags}, s.validateNewApp)
	huma.Register(s.api, huma.Operation{OperationID: "create-app", Method: http.MethodPost, Path: "/api/apps", Summary: "Create a new app (folder + compose file), optionally start it", Tags: tags}, s.createApp)
	huma.Register(s.api, huma.Operation{OperationID: "app-folders", Method: http.MethodGet, Path: "/api/apps/folders", Summary: "Existing group folders under apps_root", Tags: tags}, s.appFolders)
}

func (s *Server) newApp(b NewAppBody) files.NewApp {
	return files.NewApp{Root: s.Config.AppsRoot, Depth: s.Config.ScanDepth, Path: b.Path, Compose: b.Compose, Env: b.Env}
}

func (s *Server) validateNewApp(ctx context.Context, in *NewAppInput) (*ValidateOutput, error) {
	out := &ValidateOutput{}
	err := s.Files.ValidateNew(ctx, s.newApp(in.Body))
	var ve *files.ValidationError
	switch {
	case err == nil:
		out.Body.OK = true
	case errors.As(err, &ve):
		out.Body.Error = ve
	default:
		return nil, fileErr(err)
	}
	return out, nil
}

func (s *Server) createApp(ctx context.Context, in *NewAppInput) (*NewAppOutput, error) {
	dir, err := s.Files.Create(ctx, s.newApp(in.Body), clientIP(ctx))
	s.audit(ctx, "app.create", in.Body.Path, map[string]any{"start": in.Body.Start}, err)
	if err != nil {
		var ve *files.ValidationError
		if errors.As(err, &ve) {
			return nil, &FileError{Status: http.StatusUnprocessableEntity, Title: "Invalid", Detail: ve.Message, Line: ve.Line, Column: ve.Column}
		}
		return nil, fileErr(err)
	}
	_ = s.Stacks.Scan(ctx)
	s.publishStacks()
	var st, found = s.Stacks.Stacks(), false
	out := &NewAppOutput{}
	for _, x := range st {
		if x.Dir == dir {
			out.Body.Stack, found = x.ID, true
			s.addFeed(FeedItem{Time: time.Now(), Kind: "system", Level: "info", Stack: x.ID, Action: "create", Message: "new app created"})
			if in.Body.Start {
				spec, err := s.ComposeJob(x, "up", nil, "user")
				if err != nil {
					return nil, err
				}
				spec.Timeout = 30 * time.Minute // first start pulls the images
				j, err := s.Jobs.Submit(spec)
				if err == nil {
					out.Body.Job = &j
				}
			}
		}
	}
	if !found {
		return nil, huma.Error500InternalServerError("the app was written to " + dir + " but not found by the scan")
	}
	return out, nil
}

// appFolders lists folders that can hold apps: not apps themselves, not inside one, not hidden.
func (s *Server) appFolders(ctx context.Context, _ *struct{}) (*FoldersOutput, error) {
	out := &FoldersOutput{}
	out.Body.Root, out.Body.Depth, out.Body.Folders = s.Config.AppsRoot, s.Config.ScanDepth, []string{}
	root, err := filepath.EvalSymlinks(s.Config.AppsRoot)
	if err != nil {
		return out, nil
	}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() || p == root {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules" || compose.FindFiles(p) != nil {
			return fs.SkipDir
		}
		if strings.Count(rel, "/")+1 >= s.Config.ScanDepth { // an app inside would be too deep
			return fs.SkipDir
		}
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			out.Body.Folders = append(out.Body.Folders, rel)
		}
		return nil
	})
	sort.Strings(out.Body.Folders)
	return out, nil
}
