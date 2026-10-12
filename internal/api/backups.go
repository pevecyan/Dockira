package api

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"docker-manager/internal/jobs"
	"docker-manager/internal/snapshots"
)

type Dump struct {
	File    string    `json:"file"`
	Service string    `json:"service"`
	Size    int64     `json:"size"`
	Created time.Time `json:"created"`
}

type Backups struct {
	Snapshots []snapshots.Snapshot `json:"snapshots"`
	Dumps     []Dump               `json:"dumps" doc:"pg_dump files made before updates"`
	Volumes   int                  `json:"volumes" doc:"Named volumes the stack's containers use"`
	Binds     int                  `json:"binds" doc:"Bind-mounted folders inside the stack folder"`
}

type BackupsOutput struct{ Body Backups }

type SnapshotInput struct {
	ID   string `path:"id"`
	Body snapshots.Options
}

type SnapshotIDInput struct {
	ID      string `path:"id"`
	SID     string `path:"sid"`
	Confirm string `cookie:"dm_confirm"`
}

func (s *Server) registerBackups() {
	tags := []string{"backups"}
	huma.Register(s.api, huma.Operation{OperationID: "list-backups", Method: http.MethodGet, Path: "/api/stacks/{id}/backups", Summary: "Volume snapshots and database dumps of a stack", Tags: tags}, s.listBackups)
	huma.Register(s.api, huma.Operation{OperationID: "take-snapshot", Method: http.MethodPost, Path: "/api/stacks/{id}/snapshots", Summary: "Snapshot the stack's volumes now (a job)", Tags: tags}, s.takeSnapshot)
	huma.Register(s.api, huma.Operation{OperationID: "restore-snapshot", Method: http.MethodPost, Path: "/api/stacks/{id}/snapshots/{sid}/restore", Summary: "Put a snapshot back (a job; needs a password confirmation)", Tags: tags}, s.restoreSnapshot)
	huma.Register(s.api, huma.Operation{OperationID: "delete-snapshot", Method: http.MethodDelete, Path: "/api/stacks/{id}/snapshots/{sid}", Summary: "Delete a snapshot (needs a password confirmation)", Tags: tags}, s.deleteSnapshot)
	s.mux.HandleFunc("GET /api/stacks/{id}/backups/download", s.downloadBackup)
}

func (s *Server) backupDir(stackID string) string {
	return filepath.Join(s.Config.DataDir, "backups", strings.NewReplacer("/", "_", "~", "").Replace(stackID))
}

var dumpRe = regexp.MustCompile(`^(.+)-(\d{8}-\d{6})\.sql\.gz$`)

func (s *Server) dumps(stackID string) []Dump {
	out := []Dump{}
	des, _ := os.ReadDir(s.backupDir(stackID))
	for _, de := range des {
		m := dumpRe.FindStringSubmatch(de.Name())
		if m == nil || de.IsDir() {
			continue
		}
		fi, err := de.Info()
		if err != nil {
			continue
		}
		t, _ := time.ParseInLocation("20060102-150405", m[2], time.Local)
		out = append(out, Dump{File: de.Name(), Service: m[1], Size: fi.Size(), Created: t})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

func (s *Server) listBackups(ctx context.Context, in *StackIDInput) (*BackupsOutput, error) {
	st, err := s.stack(in.ID)
	if err != nil {
		return nil, err
	}
	snaps, err := s.Snapshots.List(st.ID)
	if err != nil {
		return nil, huma.Error500InternalServerError("snapshots", err)
	}
	out := Backups{Snapshots: snaps, Dumps: s.dumps(st.ID)}
	seen := map[string]bool{}
	for _, sv := range st.Services {
		for _, c := range sv.Containers {
			d, err := s.Docker.Inspect(ctx, c.ID)
			if err != nil {
				continue
			}
			for _, m := range d.Mounts {
				switch {
				case m.Type == "volume" && !seen["v"+m.Name]:
					seen["v"+m.Name] = true
					out.Volumes++
				case m.Type == "bind" && strings.HasPrefix(m.Source, st.Dir+"/") && !seen["b"+m.Source]:
					seen["b"+m.Source] = true
					out.Binds++
				}
			}
		}
	}
	return &BackupsOutput{Body: out}, nil
}

func (s *Server) snapshotJob(stackID string, spec jobs.Spec) (*JobOutput, error) {
	if s.Alerts != nil {
		s.Alerts.Expect(stackID, spec.Timeout) // containers stop on purpose
	}
	j, err := s.Jobs.Submit(spec)
	if err != nil {
		return nil, huma.Error500InternalServerError("job", err)
	}
	return &JobOutput{Body: j}, nil
}

func (s *Server) takeSnapshot(ctx context.Context, in *SnapshotInput) (*JobOutput, error) {
	st, err := s.stack(in.ID)
	if err != nil {
		return nil, err
	}
	if st.Self {
		return nil, huma.Error409Conflict("Dockira's own data is backed up from the host (copy its data folder)")
	}
	o := in.Body
	o.Trigger = "manual"
	s.audit(ctx, "snapshot.take", st.ID, o, nil)
	return s.snapshotJob(st.ID, jobs.Spec{StackID: st.ID, Kind: "snapshot", Title: "Snapshot " + st.ID, Timeout: 2 * time.Hour,
		Run: func(ctx context.Context, out *jobs.Output) error {
			_, err := s.Snapshots.Take(ctx, st, o, out)
			return err
		}})
}

func (s *Server) restoreSnapshot(ctx context.Context, in *SnapshotIDInput) (*JobOutput, error) {
	st, err := s.stack(in.ID)
	if err != nil {
		return nil, err
	}
	if !s.confirmed(in.Confirm) {
		return nil, needConfirm()
	}
	if _, err := s.Snapshots.Get(st.ID, in.SID); err != nil {
		return nil, huma.Error404NotFound("no such snapshot")
	}
	s.audit(ctx, "snapshot.restore", st.ID, map[string]string{"snapshot": in.SID}, nil)
	return s.snapshotJob(st.ID, jobs.Spec{StackID: st.ID, Kind: "restore", Title: fmt.Sprintf("Restore %s from %s", st.ID, in.SID), Timeout: 2 * time.Hour,
		Run: func(ctx context.Context, out *jobs.Output) error {
			err := s.Snapshots.Restore(ctx, st, in.SID, out)
			s.afterChange(st)
			return err
		}})
}

func (s *Server) deleteSnapshot(ctx context.Context, in *SnapshotIDInput) (*struct{}, error) {
	st, err := s.stack(in.ID)
	if err != nil {
		return nil, err
	}
	if !s.confirmed(in.Confirm) {
		return nil, needConfirm()
	}
	err = s.Snapshots.Delete(st.ID, in.SID)
	s.audit(ctx, "snapshot.delete", st.ID, map[string]string{"snapshot": in.SID}, err)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, huma.Error404NotFound("no such snapshot")
	}
	if err != nil {
		return nil, huma.Error500InternalServerError("delete", err)
	}
	return nil, nil
}

// downloadBackup sends one archive of a snapshot (?snapshot=&file=) or a dump (?dump=).
func (s *Server) downloadBackup(w http.ResponseWriter, r *http.Request) {
	st, ok := s.Stacks.Get(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "no such stack")
		return
	}
	if s.Auth != nil && !s.Auth.RequireConfirm(w, r) {
		return
	}
	q := r.URL.Query()
	var f *os.File
	var err error
	name := ""
	if d := q.Get("dump"); d != "" {
		for _, x := range s.dumps(st.ID) {
			if x.File == d {
				name = d
				f, err = os.Open(filepath.Join(s.backupDir(st.ID), d))
			}
		}
	} else {
		name = q.Get("file")
		f, err = s.Snapshots.File(st.ID, q.Get("snapshot"), name)
	}
	if f == nil || err != nil {
		writeErr(w, http.StatusNotFound, "no such backup")
		return
	}
	defer f.Close()
	fi, _ := f.Stat()
	s.audit(r.Context(), "backup.download", st.ID+"/"+name, nil, nil)
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, strings.NewReplacer("/", "_").Replace(st.ID)+"-"+name))
	http.ServeContent(w, r, "", fi.ModTime(), f)
}
