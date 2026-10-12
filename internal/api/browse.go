package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path/filepath"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"docker-manager/internal/files"
)

const maxUpload = 50 << 20

type TreeInput struct {
	ID   string `path:"id"`
	Path string `query:"path" doc:"Folder relative to the stack folder (empty: the stack folder)"`
}

type TreeOutput struct{ Body files.Listing }

type PathInput struct {
	ID      string `path:"id"`
	Path    string `query:"path" required:"true"`
	Confirm string `cookie:"dm_confirm"`
}

type AnyFile struct {
	Path    string    `json:"path"`
	Content string    `json:"content"`
	SHA256  string    `json:"sha256"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time,omitzero"`
}

type AnyFileOutput struct{ Body AnyFile }

type SaveAnyInput struct {
	ID      string `path:"id"`
	Confirm string `cookie:"dm_confirm"`
	Body    struct {
		Path       string `json:"path" minLength:"1"`
		Content    string `json:"content" maxLength:"1048576"`
		BaseSHA256 string `json:"base_sha256" doc:"Hash the editor started from (empty for a new file)"`
		Apply      bool   `json:"apply,omitempty"`
	}
}

type FolderInput struct {
	ID   string `path:"id"`
	Body struct {
		Path string `json:"path" minLength:"1"`
	}
}

func (s *Server) registerBrowse() {
	tags := []string{"files"}
	huma.Register(s.api, huma.Operation{OperationID: "tree", Method: http.MethodGet, Path: "/api/stacks/{id}/tree", Summary: "List a folder of the stack", Tags: tags}, s.tree)
	huma.Register(s.api, huma.Operation{OperationID: "read-any", Method: http.MethodGet, Path: "/api/stacks/{id}/file", Summary: "Read a text file of the stack folder (needs a password confirmation unless it's a compose file)", Tags: tags}, s.readAny)
	huma.Register(s.api, huma.Operation{OperationID: "save-any", Method: http.MethodPut, Path: "/api/stacks/{id}/file", Summary: "Save a text file of the stack folder", Tags: tags}, s.saveAny)
	huma.Register(s.api, huma.Operation{OperationID: "delete-any", Method: http.MethodDelete, Path: "/api/stacks/{id}/file", Summary: "Delete a file (kept in History) or an empty folder", Tags: tags}, s.deleteAny)
	huma.Register(s.api, huma.Operation{OperationID: "create-folder", Method: http.MethodPost, Path: "/api/stacks/{id}/folder", Summary: "Create a folder", Tags: tags}, s.createFolder)
	s.mux.HandleFunc("GET /api/stacks/{id}/download", s.download)
	s.mux.HandleFunc("PUT /api/stacks/{id}/upload", s.upload)
}

func browseErr(err error) error {
	switch {
	case errors.Is(err, files.ErrBinary):
		return huma.Error415UnsupportedMediaType(err.Error())
	case errors.Is(err, files.ErrIsDir):
		return huma.Error400BadRequest(err.Error())
	case errors.Is(err, fs.ErrNotExist):
		return huma.Error404NotFound("no such file or folder")
	case errors.Is(err, fs.ErrPermission):
		return huma.Error403Forbidden("permission denied")
	}
	return fileErr(err)
}

func (s *Server) tree(ctx context.Context, in *TreeInput) (*TreeOutput, error) {
	st, err := s.stack(in.ID)
	if err != nil {
		return nil, err
	}
	l, err := files.Tree(st, in.Path)
	if err != nil {
		return nil, browseErr(err)
	}
	return &TreeOutput{Body: l}, nil
}

func (s *Server) readAny(ctx context.Context, in *PathInput) (*AnyFileOutput, error) {
	st, err := s.stack(in.ID)
	if err != nil {
		return nil, err
	}
	if files.Sensitive(in.Path) && !s.confirmed(in.Confirm) {
		return nil, needConfirm()
	}
	e, err := files.Stat(st, in.Path)
	if err != nil {
		return nil, browseErr(err)
	}
	content, sum, err := s.Files.Read(ctx, st, in.Path)
	if err != nil {
		return nil, browseErr(err)
	}
	return &AnyFileOutput{Body: AnyFile{Path: e.Path, Content: content, SHA256: sum, Size: e.Size, ModTime: e.ModTime}}, nil
}

func (s *Server) saveAny(ctx context.Context, in *SaveAnyInput) (*SaveOutput, error) {
	st, err := s.stack(in.ID)
	if err != nil {
		return nil, err
	}
	if files.Sensitive(in.Body.Path) && !s.confirmed(in.Confirm) {
		return nil, needConfirm()
	}
	return s.save(ctx, st, in.Body.Path, in.Body.Content, in.Body.BaseSHA256, "edit", in.Body.Apply)
}

func (s *Server) deleteAny(ctx context.Context, in *PathInput) (*struct{}, error) {
	st, err := s.stack(in.ID)
	if err != nil {
		return nil, err
	}
	if !s.confirmed(in.Confirm) {
		return nil, needConfirm()
	}
	err = s.Files.Delete(ctx, st, in.Path, clientIP(ctx))
	s.audit(ctx, "file.delete", st.ID+"/"+in.Path, nil, err)
	if err != nil {
		if errors.Is(err, files.ErrNotAllowed) || errors.Is(err, fs.ErrNotExist) {
			return nil, browseErr(err)
		}
		return nil, huma.Error409Conflict(err.Error())
	}
	s.addFeed(FeedItem{Time: time.Now(), Kind: "system", Level: "info", Stack: st.ID, Action: "file", Message: in.Path + " deleted (kept in History)"})
	return nil, nil
}

func (s *Server) createFolder(ctx context.Context, in *FolderInput) (*struct{}, error) {
	st, err := s.stack(in.ID)
	if err != nil {
		return nil, err
	}
	err = files.Mkdir(st, in.Body.Path)
	s.audit(ctx, "file.mkdir", st.ID+"/"+in.Body.Path, nil, err)
	if err != nil {
		return nil, browseErr(err)
	}
	return nil, nil
}

// download streams a file of the stack folder.
func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	st, ok := s.Stacks.Get(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "no such stack")
		return
	}
	rel := r.URL.Query().Get("path")
	if files.Sensitive(rel) && s.Auth != nil && !s.Auth.RequireConfirm(w, r) {
		return
	}
	f, fi, err := files.Open(st, rel)
	if err != nil {
		writeErr(w, http.StatusNotFound, "%v", err)
		return
	}
	defer f.Close()
	s.audit(r.Context(), "file.download", st.ID+"/"+rel, nil, nil)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, filepath.Base(rel)))
	http.ServeContent(w, r, "", fi.ModTime(), f)
}

// upload writes the request body as a file of the stack folder (?path=folder/name).
func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	st, ok := s.Stacks.Get(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "no such stack")
		return
	}
	if s.Auth != nil && !s.Auth.RequireConfirm(w, r) {
		return
	}
	rel := r.URL.Query().Get("path")
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxUpload))
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, "uploads are limited to %d MiB", maxUpload>>20)
		return
	}
	err = s.Files.Upload(r.Context(), st, rel, body, clientIP(r.Context()))
	s.audit(r.Context(), "file.upload", st.ID+"/"+rel, map[string]int{"bytes": len(body)}, err)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "%v", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
