package registry

import (
	"strings"
	"testing"
)

var (
	dig  = "sha256:" + strings.Repeat("a", 64)
	dig2 = "sha256:" + strings.Repeat("b", 64)
)

func TestParseRef(t *testing.T) {
	for _, c := range []struct {
		in                  string
		ref                 Ref
		str, familiar, repo string
	}{
		{"postgres:16", Ref{"index.docker.io", "library/postgres", "16", ""},
			"index.docker.io/library/postgres:16", "postgres:16", "index.docker.io/library/postgres"},
		{"postgres", Ref{"index.docker.io", "library/postgres", "latest", ""},
			"index.docker.io/library/postgres:latest", "postgres:latest", "index.docker.io/library/postgres"},
		{"library/postgres", Ref{"index.docker.io", "library/postgres", "latest", ""},
			"index.docker.io/library/postgres:latest", "postgres:latest", "index.docker.io/library/postgres"},
		{"docker.io/library/postgres:16", Ref{"index.docker.io", "library/postgres", "16", ""},
			"index.docker.io/library/postgres:16", "postgres:16", "index.docker.io/library/postgres"},
		{"louislam/uptime-kuma:1", Ref{"index.docker.io", "louislam/uptime-kuma", "1", ""},
			"index.docker.io/louislam/uptime-kuma:1", "louislam/uptime-kuma:1", "index.docker.io/louislam/uptime-kuma"},
		{"ghcr.io/acme/notes-mcp:latest", Ref{"ghcr.io", "acme/notes-mcp", "latest", ""},
			"ghcr.io/acme/notes-mcp:latest", "ghcr.io/acme/notes-mcp:latest", "ghcr.io/acme/notes-mcp"},
		{"registry.example.com/acme/gateway:main-stable", Ref{"registry.example.com", "acme/gateway", "main-stable", ""},
			"registry.example.com/acme/gateway:main-stable", "registry.example.com/acme/gateway:main-stable", "registry.example.com/acme/gateway"},
		{"localhost:5000/app", Ref{"localhost:5000", "app", "latest", ""},
			"localhost:5000/app:latest", "localhost:5000/app:latest", "localhost:5000/app"},
		{"localhost:5000/team/app:1.2", Ref{"localhost:5000", "team/app", "1.2", ""},
			"localhost:5000/team/app:1.2", "localhost:5000/team/app:1.2", "localhost:5000/team/app"},
		{"img@" + dig, Ref{"index.docker.io", "library/img", "", dig},
			"index.docker.io/library/img@" + dig, "img@" + dig, "index.docker.io/library/img"},
		{"img:tag@" + dig, Ref{"index.docker.io", "library/img", "tag", dig},
			"index.docker.io/library/img:tag@" + dig, "img:tag@" + dig, "index.docker.io/library/img"},
	} {
		r, err := ParseRef(c.in)
		if err != nil {
			t.Errorf("%s: %v", c.in, err)
			continue
		}
		if r != c.ref || r.String() != c.str || r.Familiar() != c.familiar || r.Repo() != c.repo {
			t.Errorf("%s: got %+v %q %q %q", c.in, r, r.String(), r.Familiar(), r.Repo())
		}
		if r.Pinned() != (c.ref.Digest != "") {
			t.Errorf("%s: Pinned() = %v", c.in, r.Pinned())
		}
		if again, err := ParseRef(r.String()); err != nil || again != r {
			t.Errorf("%s: String() does not round-trip: %+v %v", c.in, again, err)
		}
	}
}

func TestParseRefInvalid(t *testing.T) {
	for _, in := range []string{
		"", "  ", "Postgres:16", "postgres:", "postgres:bad tag", "img@sha256:abc",
		"img@" + dig + "@" + dig, "img@", "a:b:c", "ghcr.io/", "-bad/img",
	} {
		if r, err := ParseRef(in); err == nil {
			t.Errorf("%q: expected error, got %+v", in, r)
		}
	}
}

func TestMatchRepoDigest(t *testing.T) {
	mustRef := func(s string) Ref {
		r, err := ParseRef(s)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	for _, c := range []struct {
		ref     string
		digests []string
		want    string
	}{
		{"postgres:16", []string{"postgres@" + dig}, dig},
		{"docker.io/library/postgres:17", []string{"mirror.local/postgres@" + dig2, "postgres@" + dig}, dig},
		{"louislam/uptime-kuma:1", []string{"louislam/uptime-kuma@" + dig}, dig},
		{"ghcr.io/acme/notes-mcp:latest", []string{"acme/notes-mcp@" + dig2, "ghcr.io/acme/notes-mcp@" + dig}, dig},
		{"registry.example.com/acme/gateway:main-stable", []string{"registry.example.com/acme/gateway@" + dig}, dig},
		{"localhost:5000/app", []string{"localhost:5000/app@" + dig}, dig},
		{"postgres:16", []string{"ghcr.io/postgres@" + dig, "other@" + dig2, "garbage"}, ""},
		{"postgres:16", nil, ""},
	} {
		if got := MatchRepoDigest(mustRef(c.ref), c.digests); got != c.want {
			t.Errorf("%s %v: got %q, want %q", c.ref, c.digests, got, c.want)
		}
	}
}
