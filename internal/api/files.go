package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"docker-manager/internal/dotenv"
	"docker-manager/internal/files"
	"docker-manager/internal/jobs"
	"docker-manager/internal/stacks"
)

type FileListOutput struct {
	Body struct {
		Files []files.File `json:"files"`
	}
}

type FileInput struct {
	ID      string `path:"id"`
	Name    string `path:"name" doc:"File name: one of the stack's compose files or .env"`
	Confirm string `cookie:"dm_confirm"`
}

type FileContent struct {
	Name    string `json:"name"`
	Content string `json:"content"`
	SHA256  string `json:"sha256" doc:"Send back as base_sha256 when saving"`
}

type FileOutput struct{ Body FileContent }

type ValidateInput struct {
	ID   string `path:"id"`
	Name string `path:"name"`
	Body struct {
		Content string `json:"content" maxLength:"1048576"`
	}
}

type ValidateOutput struct {
	Body struct {
		OK    bool                   `json:"ok"`
		Error *files.ValidationError `json:"error,omitempty"`
	}
}

type SaveInput struct {
	ID      string `path:"id"`
	Name    string `path:"name"`
	Confirm string `cookie:"dm_confirm"`
	Body    struct {
		Content    string `json:"content" maxLength:"1048576"`
		BaseSHA256 string `json:"base_sha256" doc:"Hash of the version the editor started from (empty for a new file)"`
		Apply      bool   `json:"apply,omitempty" doc:"Run docker compose up -d afterwards"`
	}
}

type SaveResult struct {
	Revision files.Revision `json:"revision"`
	SHA256   string         `json:"sha256"`
	Job      *jobs.Job      `json:"job,omitempty"`
}

type SaveOutput struct{ Body SaveResult }

// FileError carries the line of a validation error, or the current hash on a conflict.
type FileError struct {
	Status  int    `json:"status"`
	Title   string `json:"title"`
	Detail  string `json:"detail"`
	Line    int    `json:"line,omitempty"`
	Column  int    `json:"column,omitempty"`
	Current string `json:"current_sha256,omitempty"`
	Confirm bool   `json:"confirm,omitempty"`
}

func (e *FileError) Error() string  { return e.Detail }
func (e *FileError) GetStatus() int { return e.Status }

type EnvKey struct {
	Key        string `json:"key"`
	Line       int    `json:"line"`
	Referenced bool   `json:"referenced" doc:"Used by a compose file"`
	Empty      bool   `json:"empty"`
}

type EnvOutput struct {
	Body struct {
		Exists  bool     `json:"exists"`
		Keys    []EnvKey `json:"keys"`
		Missing []string `json:"missing" doc:"Variables the compose files use that .env doesn't set (they may come from the environment or have defaults)"`
	}
}

type RevisionsInput struct {
	ID    string `path:"id"`
	File  string `query:"file"`
	Limit int    `query:"limit" default:"100" maximum:"500"`
}

type RevisionsOutput struct {
	Body struct {
		Revisions []files.Revision `json:"revisions"`
	}
}

type RevisionInput struct {
	ID      string `path:"id"`
	Rev     int64  `path:"rev"`
	Confirm string `cookie:"dm_confirm"`
}

type RevisionOutput struct{ Body files.Revision }

type RestoreInput struct {
	ID      string `path:"id"`
	Rev     int64  `path:"rev"`
	Confirm string `cookie:"dm_confirm"`
	Body    struct {
		Apply bool `json:"apply,omitempty"`
	}
}

func (s *Server) registerFiles() {
	tags := []string{"files"}
	huma.Register(s.api, huma.Operation{OperationID: "list-files", Method: http.MethodGet, Path: "/api/stacks/{id}/files", Summary: "Editable files of a stack", Tags: tags}, s.listFiles)
	huma.Register(s.api, huma.Operation{OperationID: "get-file", Method: http.MethodGet, Path: "/api/stacks/{id}/files/{name}", Summary: "Read a file (.env needs a password confirmation)", Tags: tags}, s.getFile)
	huma.Register(s.api, huma.Operation{OperationID: "validate-file", Method: http.MethodPost, Path: "/api/stacks/{id}/files/{name}/validate", Summary: "Check content with docker compose config", Tags: tags}, s.validateFile)
	huma.Register(s.api, huma.Operation{OperationID: "save-file", Method: http.MethodPut, Path: "/api/stacks/{id}/files/{name}", Summary: "Validate and save a file", Tags: tags}, s.saveFile)
	huma.Register(s.api, huma.Operation{OperationID: "get-env", Method: http.MethodGet, Path: "/api/stacks/{id}/env", Summary: ".env keys (no values) and variables the compose files use", Tags: tags}, s.getEnv)
	huma.Register(s.api, huma.Operation{OperationID: "list-revisions", Method: http.MethodGet, Path: "/api/stacks/{id}/revisions", Summary: "Saved versions of a stack's files", Tags: tags}, s.listRevisions)
	huma.Register(s.api, huma.Operation{OperationID: "get-revision", Method: http.MethodGet, Path: "/api/stacks/{id}/revisions/{rev}", Summary: "One saved version with content", Tags: tags}, s.getRevision)
	huma.Register(s.api, huma.Operation{OperationID: "restore-revision", Method: http.MethodPost, Path: "/api/stacks/{id}/revisions/{rev}/restore", Summary: "Write a saved version back", Tags: tags}, s.restoreRevision)
}

func fileErr(err error) error {
	var ve *files.ValidationError
	switch {
	case errors.As(err, &ve):
		return &FileError{Status: http.StatusUnprocessableEntity, Title: "Invalid", Detail: ve.Message, Line: ve.Line, Column: ve.Column}
	case errors.Is(err, files.ErrNotAllowed):
		return huma.Error404NotFound(err.Error())
	case errors.Is(err, files.ErrTooLarge):
		return huma.Error413RequestEntityTooLarge(err.Error())
	case errors.Is(err, sql.ErrNoRows):
		return huma.Error404NotFound("no such revision")
	}
	return huma.Error500InternalServerError(err.Error())
}

func (s *Server) listFiles(ctx context.Context, in *StackIDInput) (*FileListOutput, error) {
	st, err := s.stack(in.ID)
	if err != nil {
		return nil, err
	}
	out := &FileListOutput{}
	out.Body.Files = files.List(st)
	if out.Body.Files == nil {
		out.Body.Files = []files.File{}
	}
	return out, nil
}

func (s *Server) getFile(ctx context.Context, in *FileInput) (*FileOutput, error) {
	st, err := s.stack(in.ID)
	if err != nil {
		return nil, err
	}
	if files.Sensitive(in.Name) && !s.confirmed(in.Confirm) {
		return nil, needConfirm()
	}
	content, sum, err := s.Files.Read(ctx, st, in.Name)
	if err != nil {
		return nil, fileErr(err)
	}
	if in.Name == files.EnvName {
		s.audit(ctx, "env.reveal", st.ID, nil, nil)
	}
	return &FileOutput{Body: FileContent{Name: in.Name, Content: content, SHA256: sum}}, nil
}

func (s *Server) validateFile(ctx context.Context, in *ValidateInput) (*ValidateOutput, error) {
	st, err := s.stack(in.ID)
	if err != nil {
		return nil, err
	}
	out := &ValidateOutput{}
	err = s.Files.Validate(ctx, st, in.Name, in.Body.Content)
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

func (s *Server) saveFile(ctx context.Context, in *SaveInput) (*SaveOutput, error) {
	st, err := s.stack(in.ID)
	if err != nil {
		return nil, err
	}
	if in.Name == files.EnvName && !s.confirmed(in.Confirm) {
		return nil, needConfirm()
	}
	return s.save(ctx, st, in.Name, in.Body.Content, in.Body.BaseSHA256, "edit", in.Body.Apply)
}

func (s *Server) save(ctx context.Context, st stacks.Stack, name, content, base, reason string, apply bool) (*SaveOutput, error) {
	rev, err := s.Files.Save(ctx, st, name, content, base, reason, clientIP(ctx))
	if errors.Is(err, files.ErrConflict) {
		_, cur, _ := s.Files.Read(ctx, st, name)
		return nil, &FileError{Status: http.StatusConflict, Title: "Conflict", Detail: "the file changed since you opened it (edited elsewhere?): reload to see the new version", Current: cur}
	}
	s.audit(ctx, "file."+reason, st.ID+"/"+name, map[string]any{"revision": rev.ID, "apply": apply}, err)
	if err != nil {
		return nil, fileErr(err)
	}
	s.Stacks.Invalidate(st.Dir)
	_ = s.Stacks.Scan(ctx)
	s.publishStacks()
	s.addFeed(FeedItem{Time: time.Now(), Kind: "system", Level: "info", Stack: st.ID, Action: "file", Message: name + " saved (" + reason + ")"})
	out := &SaveOutput{Body: SaveResult{Revision: rev, SHA256: rev.SHA256}}
	if apply {
		fresh, ok := s.Stacks.Get(st.ID)
		if !ok {
			fresh = st
		}
		spec, err := s.ComposeJob(fresh, "up", nil, "user")
		if err != nil {
			return nil, err
		}
		j, err := s.Jobs.Submit(spec)
		if err == nil {
			out.Body.Job = &j
		}
	}
	return out, nil
}

func (s *Server) getEnv(ctx context.Context, in *StackIDInput) (*EnvOutput, error) {
	st, err := s.stack(in.ID)
	if err != nil {
		return nil, err
	}
	out := &EnvOutput{}
	out.Body.Keys, out.Body.Missing = []EnvKey{}, []string{}
	referenced := map[string]bool{}
	var order []string
	for _, f := range st.Files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, k := range dotenv.Referenced(string(b)) {
			if !referenced[k] {
				referenced[k] = true
				order = append(order, k)
			}
		}
	}
	set := map[string]bool{}
	if b, err := os.ReadFile(filepath.Join(st.Dir, files.EnvName)); err == nil {
		out.Body.Exists = true
		entries, _ := dotenv.Parse(string(b))
		for _, e := range entries {
			set[e.Key] = true
			out.Body.Keys = append(out.Body.Keys, EnvKey{Key: e.Key, Line: e.Line, Referenced: referenced[e.Key], Empty: e.Value == ""})
		}
	}
	for _, k := range order {
		if !set[k] {
			out.Body.Missing = append(out.Body.Missing, k)
		}
	}
	sort.Strings(out.Body.Missing)
	return out, nil
}

func (s *Server) listRevisions(ctx context.Context, in *RevisionsInput) (*RevisionsOutput, error) {
	if _, err := s.stack(in.ID); err != nil {
		return nil, err
	}
	revs, err := s.Files.Revisions(ctx, in.ID, in.File, in.Limit)
	if err != nil {
		return nil, fileErr(err)
	}
	out := &RevisionsOutput{}
	out.Body.Revisions = revs
	return out, nil
}

func (s *Server) getRevision(ctx context.Context, in *RevisionInput) (*RevisionOutput, error) {
	r, err := s.Files.Revision(ctx, in.ID, in.Rev)
	if err != nil {
		return nil, fileErr(err)
	}
	if files.Sensitive(r.File) && !s.confirmed(in.Confirm) {
		return nil, needConfirm()
	}
	return &RevisionOutput{Body: r}, nil
}

func (s *Server) restoreRevision(ctx context.Context, in *RestoreInput) (*SaveOutput, error) {
	st, err := s.stack(in.ID)
	if err != nil {
		return nil, err
	}
	r, err := s.Files.Revision(ctx, in.ID, in.Rev)
	if err != nil {
		return nil, fileErr(err)
	}
	if files.Sensitive(r.File) && !s.confirmed(in.Confirm) {
		return nil, needConfirm()
	}
	_, cur, err := s.Files.Read(ctx, st, r.File)
	if err != nil {
		return nil, fileErr(err)
	}
	return s.save(ctx, st, r.File, r.Content, cur, "restore", in.Body.Apply)
}

// addFeed adds a line to the Events feed and sends it to the UIs.
func (s *Server) addFeed(it FeedItem) {
	s.feed.add(it)
	s.Hub.Publish("feed", it)
}
