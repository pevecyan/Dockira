package api

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"docker-manager/internal/docker"
	"docker-manager/internal/jobs"
	"docker-manager/internal/registry"
	"docker-manager/internal/store"
)

// SystemEngine is what the Host page needs from Docker besides Engine.
type SystemEngine interface {
	Images(ctx context.Context, reference string) ([]docker.Image, error)
	RemoveImageID(ctx context.Context, id string) error
	PruneBuildCache(ctx context.Context) (int64, error)
}

type HostSample struct {
	Time     time.Time `json:"t"`
	CPU      float64   `json:"cpu"`
	MemUsed  uint64    `json:"mem"`
	MemTotal uint64    `json:"mem_total"`
	Load     float64   `json:"load"`
}

// hostHistory keeps 30 minutes of 10-second samples, so charts have a past when a page opens.
type hostHistory struct {
	mu      sync.Mutex
	samples []HostSample
}

func (h *hostHistory) add(s HostSample) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.samples = append(h.samples, s)
	if len(h.samples) > 180 {
		h.samples = append([]HostSample(nil), h.samples[len(h.samples)-180:]...)
	}
}

func (h *hostHistory) list() []HostSample {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]HostSample{}, h.samples...)
}

// sampleHost records host figures every 10 seconds (independent of the UI's live stream).
func (s *Server) sampleHost(ctx context.Context) {
	if s.Stats == nil {
		return
	}
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		if h, _ := s.Stats.Host(); h.MemTotal > 0 {
			s.history.add(HostSample{Time: h.Time, CPU: h.CPUPct, MemUsed: h.MemUsed, MemTotal: h.MemTotal, Load: h.Load[0]})
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

type PruneImage struct {
	ID      string    `json:"id"`
	Tags    []string  `json:"tags"`
	Size    int64     `json:"size"`
	Created time.Time `json:"created"`
}

type PrunePreview struct {
	Dangling        []PruneImage `json:"dangling" doc:"Untagged images no container uses"`
	Unused          []PruneImage `json:"unused" doc:"Tagged images no container and no compose file uses (rollback images and Dockira's own are kept)"`
	DanglingSize    int64        `json:"dangling_size"`
	UnusedSize      int64        `json:"unused_size"`
	BuildCacheSize  int64        `json:"build_cache_size"`
	BuildCacheFree  int64        `json:"build_cache_reclaimable"`
	RollbackImages  int          `json:"rollback_images"`
	RollbackSize    int64        `json:"rollback_size"`
	ProtectedImages int          `json:"protected_images" doc:"Unused images kept because a compose file names them"`
}

type PruneInput struct {
	Confirm string `cookie:"dm_confirm"`
	Body    struct {
		What string `json:"what" enum:"dangling,unused,build_cache"`
	}
}

type AuditInput struct {
	Before int64  `query:"before"`
	Limit  int    `query:"limit" default:"100" maximum:"500"`
	Filter string `query:"q"`
}

type AuditOutput struct {
	Body struct {
		Entries []store.AuditEntry `json:"entries"`
	}
}

type LoginsOutput struct {
	Body struct {
		Events []store.AuthEvent `json:"events"`
	}
}

type ServerSettings struct {
	Version        string   `json:"version"`
	Listen         string   `json:"listen"`
	AppsRoot       string   `json:"apps_root"`
	DataDir        string   `json:"data_dir"`
	ScanDepth      int      `json:"scan_depth"`
	PublicURL      string   `json:"public_url"`
	TrustedProxies []string `json:"trusted_proxies"`
	IPAllowlist    []string `json:"ip_allowlist"`
	ReadOnly       bool     `json:"read_only"`
	ExecEnabled    bool     `json:"exec_enabled"`
	SchemaVersion  int      `json:"schema_version"`
	SelfStack      string   `json:"self_stack,omitempty"`
}

type ServerSettingsOutput struct{ Body ServerSettings }

type HistoryOutput struct {
	Body struct {
		Samples []HostSample `json:"samples"`
	}
}

func (s *Server) registerSystem() {
	tags := []string{"system"}
	huma.Register(s.api, huma.Operation{OperationID: "host-history", Method: http.MethodGet, Path: "/api/host/history", Summary: "The last 30 minutes of host CPU and memory", Tags: tags}, s.hostHistoryOut)
	huma.Register(s.api, huma.Operation{OperationID: "prune-preview", Method: http.MethodGet, Path: "/api/system/prune", Summary: "What a cleanup would remove", Tags: tags}, s.prunePreview)
	huma.Register(s.api, huma.Operation{OperationID: "prune", Method: http.MethodPost, Path: "/api/system/prune", Summary: "Remove unused images or build cache (a job)", Tags: tags}, s.prune)
	huma.Register(s.api, huma.Operation{OperationID: "audit", Method: http.MethodGet, Path: "/api/audit", Summary: "Audit log, newest first", Tags: tags}, s.auditLog)
	huma.Register(s.api, huma.Operation{OperationID: "logins", Method: http.MethodGet, Path: "/api/logins", Summary: "Recent logins and failed attempts", Tags: tags}, s.logins)
	huma.Register(s.api, huma.Operation{OperationID: "server-settings", Method: http.MethodGet, Path: "/api/settings/server", Summary: "The server's configuration (no secrets)", Tags: tags}, s.serverSettings)
}

func (s *Server) hostHistoryOut(ctx context.Context, _ *struct{}) (*HistoryOutput, error) {
	out := &HistoryOutput{}
	out.Body.Samples = s.history.list()
	return out, nil
}

func (s *Server) sysEngine() (SystemEngine, error) {
	e, ok := s.Docker.(SystemEngine)
	if !ok {
		return nil, huma.Error501NotImplemented("not available")
	}
	return e, nil
}

// pruneCandidates sorts images into dangling and unused, keeping anything a container or a
// compose file uses, rollback images and docker-manager's own.
func (s *Server) pruneCandidates(ctx context.Context) (PrunePreview, error) {
	var p PrunePreview
	e, err := s.sysEngine()
	if err != nil {
		return p, err
	}
	imgs, err := e.Images(ctx, "")
	if err != nil {
		return p, huma.Error502BadGateway("images", err)
	}
	used := map[string]bool{}
	for _, c := range s.Stacks.Containers() {
		used[c.ImageID] = true
	}
	for _, c := range s.Stacks.Standalone() {
		used[c.ImageID] = true
	}
	wanted := map[string]bool{} // repositories compose files name
	for _, st := range s.Stacks.Stacks() {
		for _, sv := range st.Services {
			if r, err := registry.ParseRef(sv.Image); err == nil {
				wanted[r.Repo()] = true
			}
		}
	}
	for _, img := range imgs {
		if used[img.ID] {
			continue
		}
		pi := PruneImage{ID: img.ID, Tags: img.RepoTags, Size: img.Size, Created: img.Created}
		if pi.Tags == nil {
			pi.Tags = []string{}
		}
		var real []string
		for _, t := range img.RepoTags {
			if t != "<none>:<none>" {
				real = append(real, t)
			}
		}
		switch {
		case len(real) == 0:
			p.Dangling = append(p.Dangling, pi)
			p.DanglingSize += pi.Size
		case anyPrefix(real, "dm-rollback/"):
			p.RollbackImages++
			p.RollbackSize += pi.Size
		case anyPrefix(real, "docker-manager:"):
		default:
			keep := false
			for _, t := range real {
				if r, err := registry.ParseRef(t); err == nil && wanted[r.Repo()] {
					keep = true
				}
			}
			if keep {
				p.ProtectedImages++
				continue
			}
			p.Unused = append(p.Unused, pi)
			p.UnusedSize += pi.Size
		}
	}
	sort.Slice(p.Unused, func(i, j int) bool { return p.Unused[i].Size > p.Unused[j].Size })
	if du, err := s.Docker.DiskUsage(ctx); err == nil {
		p.BuildCacheSize, p.BuildCacheFree = du.BuildCacheSize, du.BuildCacheReclaim
	}
	if p.Dangling == nil {
		p.Dangling = []PruneImage{}
	}
	if p.Unused == nil {
		p.Unused = []PruneImage{}
	}
	return p, nil
}

func anyPrefix(list []string, prefix string) bool {
	for _, s := range list {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

type PrunePreviewOutput struct{ Body PrunePreview }

func (s *Server) prunePreview(ctx context.Context, _ *struct{}) (*PrunePreviewOutput, error) {
	p, err := s.pruneCandidates(ctx)
	if err != nil {
		return nil, err
	}
	return &PrunePreviewOutput{Body: p}, nil
}

func (s *Server) prune(ctx context.Context, in *PruneInput) (*JobOutput, error) {
	if in.Body.What == "unused" && !s.confirmed(in.Confirm) {
		return nil, needConfirm()
	}
	e, err := s.sysEngine()
	if err != nil {
		return nil, err
	}
	titles := map[string]string{"dangling": "Remove dangling images", "unused": "Remove unused images", "build_cache": "Clear build cache"}
	spec := jobs.Spec{Kind: "prune", Title: titles[in.Body.What], Timeout: 30 * time.Minute, Run: func(ctx context.Context, out *jobs.Output) error {
		if in.Body.What == "build_cache" {
			freed, err := e.PruneBuildCache(ctx)
			if err != nil {
				return err
			}
			out.Printf("✓ build cache cleared: %s freed", humanBytes(freed))
			return nil
		}
		p, err := s.pruneCandidates(ctx)
		if err != nil {
			return err
		}
		list := p.Dangling
		if in.Body.What == "unused" {
			list = p.Unused
		}
		var freed int64
		var failed int
		for _, img := range list {
			name := strings.TrimPrefix(img.ID, "sha256:")[:12]
			if len(img.Tags) > 0 && img.Tags[0] != "<none>:<none>" {
				name = strings.Join(img.Tags, ", ")
			}
			if err := e.RemoveImageID(ctx, img.ID); err != nil {
				failed++
				out.Printf("  ! %s: %v", name, err)
				continue
			}
			freed += img.Size
			out.Printf("  removed %s (%s)", name, humanBytes(img.Size))
		}
		out.Printf("✓ %d images removed, about %s freed", len(list)-failed, humanBytes(freed))
		return nil
	}}
	j, err := s.Jobs.Submit(spec)
	s.audit(ctx, "system.prune", in.Body.What, map[string]any{"job": j.ID}, err)
	if err != nil {
		return nil, huma.Error500InternalServerError("prune", err)
	}
	return &JobOutput{Body: j}, nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

func (s *Server) auditLog(ctx context.Context, in *AuditInput) (*AuditOutput, error) {
	entries, err := s.Store.AuditLog(ctx, in.Before, in.Limit)
	if err != nil {
		return nil, huma.Error500InternalServerError("audit", err)
	}
	if q := strings.ToLower(in.Filter); q != "" {
		kept := entries[:0]
		for _, e := range entries {
			if strings.Contains(strings.ToLower(e.Action+" "+e.Target+" "+e.Details+" "+e.IP+" "+e.Result), q) {
				kept = append(kept, e)
			}
		}
		entries = kept
	}
	out := &AuditOutput{}
	out.Body.Entries = entries
	return out, nil
}

func (s *Server) logins(ctx context.Context, _ *struct{}) (*LoginsOutput, error) {
	ev, err := s.Store.AuthEvents(ctx, 100)
	if err != nil {
		return nil, huma.Error500InternalServerError("logins", err)
	}
	out := &LoginsOutput{}
	out.Body.Events = ev
	return out, nil
}

func (s *Server) serverSettings(ctx context.Context, _ *struct{}) (*ServerSettingsOutput, error) {
	c := s.Config
	v, _ := s.Store.Version(ctx)
	out := ServerSettings{Version: s.Version, Listen: c.Listen, AppsRoot: c.AppsRoot, DataDir: c.DataDir, ScanDepth: c.ScanDepth,
		PublicURL: c.PublicURL, TrustedProxies: c.TrustedProxies, IPAllowlist: c.IPAllowlist, ReadOnly: c.ReadOnly,
		ExecEnabled: c.ExecEnabled, SchemaVersion: v}
	if out.TrustedProxies == nil {
		out.TrustedProxies = []string{}
	}
	if out.IPAllowlist == nil {
		out.IPAllowlist = []string{}
	}
	for _, st := range s.Stacks.Stacks() {
		if st.Self {
			out.SelfStack = st.ID
		}
	}
	return &ServerSettingsOutput{Body: out}, nil
}
