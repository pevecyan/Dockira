package registry

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

// testRegistry is an in-memory registry; wrap lets a test put middleware in front of it.
func testRegistry(t *testing.T, wrap func(http.Handler) http.Handler) string {
	t.Helper()
	var h http.Handler = ggcrregistry.New(ggcrregistry.Logger(log.New(io.Discard, "", 0)))
	if wrap != nil {
		h = wrap(h)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func pushImage(t *testing.T, ref string) v1.Hash {
	t.Helper()
	img, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(mustName(t, ref), img); err != nil {
		t.Fatal(err)
	}
	d, _ := img.Digest()
	return d
}

func pushIndex(t *testing.T, ref string) v1.Hash {
	t.Helper()
	idx, err := random.Index(256, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.WriteIndex(mustName(t, ref), idx); err != nil {
		t.Fatal(err)
	}
	d, _ := idx.Digest()
	return d
}

func mustName(t *testing.T, s string) name.Reference {
	t.Helper()
	r, err := name.ParseReference(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func mustParse(t *testing.T, s string) Ref {
	t.Helper()
	r, err := ParseRef(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// counter records registry requests and how many were in flight at once.
type counter struct {
	next              http.RoundTripper
	delay             time.Duration
	mu                sync.Mutex
	reqs              []string // "METHOD /path"
	inflight, maxSeen int
}

func (c *counter) RoundTrip(r *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.reqs = append(c.reqs, r.Method+" "+r.URL.Path)
	c.inflight++
	c.maxSeen = max(c.maxSeen, c.inflight)
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.inflight--; c.mu.Unlock() }()
	time.Sleep(c.delay)
	return c.next.RoundTrip(r)
}

func (c *counter) count(method, pathPart string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, r := range c.reqs {
		if strings.HasPrefix(r, method+" ") && strings.Contains(r, pathPart) {
			n++
		}
	}
	return n
}

// testClient never reads the developer's ~/.docker: credentials come from an empty dir.
func testClient(t *testing.T, o Options) *Client {
	if o.DockerConfigDir == "" {
		o.DockerConfigDir = t.TempDir()
	}
	o.Insecure = true
	return New(o)
}

func TestDigestTagsAndRepush(t *testing.T) {
	host := testRegistry(t, nil)
	ctx := context.Background()
	c := testClient(t, Options{})

	want := pushImage(t, host+"/team/app:1")
	app := mustParse(t, host+"/team/app:1")
	if got, err := c.Digest(ctx, app); err != nil || got != want.String() {
		t.Fatalf("image digest = %q, %v; want %s", got, err, want)
	}

	// A multi-arch image resolves to the index digest, as in docker's RepoDigests.
	wantIdx := pushIndex(t, host+"/team/multi:1")
	if got, err := c.Digest(ctx, mustParse(t, host+"/team/multi:1")); err != nil || got != wantIdx.String() {
		t.Fatalf("index digest = %q, %v; want %s", got, err, wantIdx)
	}

	// Re-pushing the tag is exactly what the update check has to notice.
	repushed := pushImage(t, host+"/team/app:1")
	if got, err := c.Digest(ctx, app); err != nil || got != repushed.String() || got == want.String() {
		t.Fatalf("after re-push: %q, %v; want %s", got, err, repushed)
	}

	// A pinned ref without tag is looked up by its digest.
	if got, err := c.Digest(ctx, mustParse(t, host+"/team/app@"+repushed.String())); err != nil || got != repushed.String() {
		t.Fatalf("by digest: %q, %v", got, err)
	}

	pushImage(t, host+"/team/app:2")
	tags, err := c.Tags(ctx, app)
	slices.Sort(tags)
	if err != nil || !slices.Equal(tags, []string{"1", "2"}) {
		t.Fatalf("tags = %v, %v", tags, err)
	}

	for _, ref := range []string{host + "/team/app:nope", host + "/team/missing:1"} {
		_, err := c.Digest(ctx, mustParse(t, ref))
		if Classify(err) != ErrNotFound {
			t.Errorf("%s: %v classified %q", ref, err, Classify(err))
		}
	}
	if _, err := c.Tags(ctx, mustParse(t, host+"/team/missing")); Classify(err) != ErrNotFound {
		t.Errorf("tags of missing repo: %v classified %q", err, Classify(err))
	}
}

// headerStripper hides Docker-Content-Digest on HEAD, like some registries and proxies do.
type headerStripper struct{ http.ResponseWriter }

func (w headerStripper) WriteHeader(code int) {
	w.Header().Del("Docker-Content-Digest")
	w.ResponseWriter.WriteHeader(code)
}

func TestDigestFallsBackToGet(t *testing.T) {
	host := testRegistry(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodHead {
				w = headerStripper{w}
			}
			h.ServeHTTP(w, r)
		})
	})
	want := pushIndex(t, host+"/team/multi:1")
	cnt := &counter{next: http.DefaultTransport}
	c := testClient(t, Options{Transport: cnt})
	got, err := c.Digest(context.Background(), mustParse(t, host+"/team/multi:1"))
	if err != nil || got != want.String() {
		t.Fatalf("got %q, %v; want %s", got, err, want)
	}
	if cnt.count("HEAD", "/manifests/") != 1 || cnt.count("GET", "/manifests/") != 1 {
		t.Errorf("expected one HEAD then one GET: %v", cnt.reqs)
	}
}

func TestCredentialsFromConfigDir(t *testing.T) {
	host := testRegistry(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if u, p, ok := r.BasicAuth(); ok && u == "bot" && p == "s3cret" {
				h.ServeHTTP(w, r)
				return
			}
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			w.WriteHeader(http.StatusUnauthorized)
		})
	})
	want, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(mustName(t, host+"/private/app:1"), want,
		remote.WithAuth(&authn.Basic{Username: "bot", Password: "s3cret"})); err != nil {
		t.Fatal(err)
	}
	wantDigest, _ := want.Digest()

	dir := t.TempDir()
	auth := base64.StdEncoding.EncodeToString([]byte("bot:s3cret"))
	cfg := fmt.Sprintf(`{"auths": {%q: {"auth": %q}}}`, host, auth)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	ref := mustParse(t, host+"/private/app:1")

	got, err := testClient(t, Options{DockerConfigDir: dir}).Digest(context.Background(), ref)
	if err != nil || got != wantDigest.String() {
		t.Fatalf("with credentials: %q, %v", got, err)
	}
	_, err = testClient(t, Options{}).Digest(context.Background(), ref)
	if Classify(err) != ErrAuth {
		t.Fatalf("without credentials: %v classified %q", err, Classify(err))
	}
}

func TestCheckAllDedupes(t *testing.T) {
	host := testRegistry(t, nil)
	a := pushImage(t, host+"/team/a:1")
	b := pushIndex(t, host+"/team/b:latest")
	cnt := &counter{next: http.DefaultTransport}
	c := testClient(t, Options{Transport: cnt})

	refs := []Ref{
		mustParse(t, host+"/team/a:1"), mustParse(t, host+"/team/b"), mustParse(t, host+"/team/a:1"),
		mustParse(t, host+"/team/b:latest"), mustParse(t, host+"/team/gone:1"),
	}
	res := c.CheckAll(context.Background(), refs)
	if len(res) != 3 {
		t.Fatalf("want 3 results, got %v", res)
	}
	ra, rb, rg := res[refs[0].String()], res[refs[1].String()], res[refs[4].String()]
	if ra.Err != nil || ra.Digest != a.String() || rb.Err != nil || rb.Digest != b.String() {
		t.Errorf("results: %+v %+v", ra, rb)
	}
	if Classify(rg.Err) != ErrNotFound || rg.Ref != refs[4] || rg.CheckedAt.IsZero() {
		t.Errorf("missing image: %+v", rg)
	}
	if n := cnt.count("HEAD", "/manifests/"); n != 3 {
		t.Errorf("want 3 manifest HEADs, got %d: %v", n, cnt.reqs)
	}
}

func TestCheckAllConcurrencyLimit(t *testing.T) {
	host := testRegistry(t, nil)
	var refs []Ref
	for i := range 8 {
		r := fmt.Sprintf("%s/team/app%d:1", host, i)
		pushImage(t, r)
		refs = append(refs, mustParse(t, r))
	}
	cnt := &counter{next: http.DefaultTransport, delay: 20 * time.Millisecond}
	c := testClient(t, Options{Transport: cnt, Concurrency: 2})
	for k, r := range c.CheckAll(context.Background(), refs) {
		if r.Err != nil {
			t.Errorf("%s: %v", k, r.Err)
		}
	}
	if cnt.maxSeen != 2 {
		t.Errorf("max requests in flight = %d, want 2", cnt.maxSeen)
	}
}

func TestCheckAllPerRefErrors(t *testing.T) {
	good := testRegistry(t, nil)
	want := pushImage(t, good+"/team/app:1")

	// A registry that accepts the connection and never answers.
	release := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(hang.Close)
	t.Cleanup(func() { close(release) })

	// A port nothing listens on.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := l.Addr().String()
	l.Close()

	c := testClient(t, Options{Timeout: 300 * time.Millisecond})
	refs := []Ref{
		mustParse(t, strings.TrimPrefix(hang.URL, "http://")+"/team/app:1"),
		mustParse(t, closed+"/team/app:1"),
		mustParse(t, good+"/team/app:1"),
	}
	start := time.Now()
	res := c.CheckAll(context.Background(), refs)
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("CheckAll took %v despite a 300ms timeout", d)
	}
	if r := res[refs[2].String()]; r.Err != nil || r.Digest != want.String() {
		t.Errorf("good registry: %+v", r)
	}
	for _, ref := range refs[:2] {
		if r := res[ref.String()]; Classify(r.Err) != ErrNetwork {
			t.Errorf("%s: %v classified %q", ref, r.Err, Classify(r.Err))
		}
	}
}

func TestClassify(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{nil, ""},
		{&transport.Error{StatusCode: 401}, ErrAuth},
		{&transport.Error{StatusCode: 403}, ErrAuth},
		{&transport.Error{StatusCode: 404}, ErrNotFound},
		{&transport.Error{StatusCode: 429}, ErrRateLimited},
		{&transport.Error{StatusCode: 503}, ErrNetwork},
		{&transport.Error{StatusCode: 400}, ErrOther},
		{&transport.Error{StatusCode: 400, Errors: []transport.Diagnostic{{Code: transport.TooManyRequestsErrorCode}}}, ErrRateLimited},
		{&transport.Error{StatusCode: 401, Errors: []transport.Diagnostic{{Code: transport.NameUnknownErrorCode}}}, ErrNotFound},
		{fmt.Errorf("check: %w", &transport.Error{StatusCode: 403, Errors: []transport.Diagnostic{{Code: transport.DeniedErrorCode}}}), ErrAuth},
		{&url.Error{Op: "Get", URL: "https://x", Err: errors.New("dial tcp: connection refused")}, ErrNetwork},
		{fmt.Errorf("wrapped: %w", context.DeadlineExceeded), ErrNetwork},
		{&net.DNSError{Err: "no such host", Name: "x"}, ErrNetwork},
		{errors.New("boom"), ErrOther},
	} {
		if got := Classify(c.err); got != c.want {
			t.Errorf("Classify(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}
