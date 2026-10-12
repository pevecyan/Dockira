// Package registry asks container registries what a tag points to right now, so the update
// check can compare that with the digest of the local image. It also has the helpers around
// it: image reference normalisation, newer-tag hints and Dockerfile FROM parsing.
package registry

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
)

// DockerHub is the registry name go-containerregistry (and Docker) uses for unqualified refs.
const DockerHub = name.DefaultRegistry // "index.docker.io"

// pathComponentRE is the distribution spec's grammar for one repository path component.
var pathComponentRE = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*$`)

// Ref is a normalized image reference.
type Ref struct {
	Registry   string // "index.docker.io", "ghcr.io", "registry.example.com"
	Repository string // "library/postgres", "acme/notes-mcp"
	Tag        string // "16", "latest" (default "latest" when no tag and no digest)
	Digest     string // "sha256:…" when pinned with @
}

// ParseRef normalizes an image reference the way docker does: "postgres:16" becomes
// index.docker.io/library/postgres:16 and a ref with neither tag nor digest gets ":latest".
// "img:tag@sha256:…" keeps both; the digest is what docker pulls.
func ParseRef(s string) (Ref, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Ref{}, errors.New("empty image reference")
	}
	base, dig, pinned := strings.Cut(s, "@")
	// A ':' after the last '/' is a tag; one before it is a registry port (localhost:5000/app).
	tag := ""
	if i := strings.LastIndex(base, ":"); i > strings.LastIndex(base, "/") {
		base, tag = base[:i], base[i+1:]
		if tag == "" {
			return Ref{}, fmt.Errorf("%q: empty tag", s)
		}
	}
	repo, err := name.NewRepository(base)
	if err != nil {
		return Ref{}, fmt.Errorf("%q: %w", s, err)
	}
	// go-containerregistry only checks the character set; docker also rejects "-x", "a..b".
	for c := range strings.SplitSeq(repo.RepositoryStr(), "/") {
		if !pathComponentRE.MatchString(c) {
			return Ref{}, fmt.Errorf("%q: invalid repository path component %q", s, c)
		}
	}
	if tag != "" {
		if _, err := name.NewTag(repo.Name() + ":" + tag); err != nil {
			return Ref{}, fmt.Errorf("%q: %w", s, err)
		}
	}
	if pinned {
		if _, err := name.NewDigest(repo.Name() + "@" + dig); err != nil {
			return Ref{}, fmt.Errorf("%q: %w", s, err)
		}
	} else if tag == "" {
		tag = "latest"
	}
	return Ref{Registry: repo.RegistryStr(), Repository: repo.RepositoryStr(), Tag: tag, Digest: dig}, nil
}

// Repo is registry/repository without tag or digest.
func (r Ref) Repo() string { return r.Registry + "/" + r.Repository }

// Pinned reports whether the ref names a digest, so a pull can never change what it gets.
func (r Ref) Pinned() bool { return r.Digest != "" }

// String is the canonical full form, e.g. index.docker.io/library/postgres:16. It round-trips
// through ParseRef and is the identity used to dedupe registry lookups.
func (r Ref) String() string { return r.Repo() + r.suffix() }

// Familiar is the short form docker shows: "postgres:16", "ghcr.io/acme/notes-mcp:latest".
func (r Ref) Familiar() string {
	if r.Registry != DockerHub {
		return r.String()
	}
	repo := r.Repository
	if rest, ok := strings.CutPrefix(repo, "library/"); ok && !strings.Contains(rest, "/") {
		repo = rest
	}
	return repo + r.suffix()
}

func (r Ref) suffix() string {
	s := ""
	if r.Tag != "" {
		s += ":" + r.Tag
	}
	if r.Digest != "" {
		s += "@" + r.Digest
	}
	return s
}

// reference is what a pull of r would ask the registry for: the tag when there is one (the
// question is what the tag points to now), otherwise the digest.
func (r Ref) reference(opts ...name.Option) (name.Reference, error) {
	if r.Tag != "" {
		return name.NewTag(r.Repo()+":"+r.Tag, opts...)
	}
	return name.NewDigest(r.Repo()+"@"+r.Digest, opts...)
}

// MatchRepoDigest picks the digest from an image's RepoDigests (as `docker image inspect` returns them,
// e.g. "postgres@sha256:…", "ghcr.io/acme/notes-mcp@sha256:…") that belongs to ref's repository.
// Returns "" when none matches.
//
// An image carries one entry per repository it was pulled from, and the one for ref's repository is
// the digest the registry served for the pulled tag.
func MatchRepoDigest(ref Ref, repoDigests []string) string {
	want := ref.Repo()
	for _, rd := range repoDigests {
		p, err := ParseRef(rd)
		if err == nil && p.Digest != "" && p.Repo() == want {
			return p.Digest
		}
	}
	return ""
}
