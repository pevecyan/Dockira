package registry

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/docker/cli/cli/config"
	"github.com/docker/cli/cli/config/types"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

// Options configures a Client; zero values get the defaults noted per field.
type Options struct {
	DockerConfigDir string            // dir with config.json for registry credentials ("" = default keychain lookup via DOCKER_CONFIG / ~/.docker)
	Timeout         time.Duration     // per request, default 15s
	Concurrency     int               // parallel registry requests in CheckAll, default 4
	Insecure        bool              // allow plain-http registries (tests, local registries)
	Transport       http.RoundTripper // optional (tests)
	UserAgent       string
}

// Client looks up image digests and tags; it is safe for concurrent use.
type Client struct {
	opts     Options
	keychain authn.Keychain
}

// New applies defaults. Credentials are read per lookup, never cached.
func New(o Options) *Client {
	if o.Timeout <= 0 {
		o.Timeout = 15 * time.Second
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 4
	}
	var kc authn.Keychain = authn.DefaultKeychain
	if o.DockerConfigDir != "" {
		kc = dirKeychain{o.DockerConfigDir}
	}
	return &Client{opts: o, keychain: kc}
}

// Retry only what a few seconds can fix, and quickly enough to stay inside Timeout. 429 is left
// out on purpose: a rate limit lasts far longer than any backoff, and retrying spends more of it.
var (
	retryBackoff = remote.Backoff{Duration: 300 * time.Millisecond, Factor: 3, Jitter: 0.1, Steps: 3}
	retryStatus  = []int{http.StatusRequestTimeout, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout}
)

func (c *Client) remoteOptions(ctx context.Context) []remote.Option {
	opts := []remote.Option{
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(c.keychain),
		remote.WithRetryBackoff(retryBackoff),
		remote.WithRetryStatusCodes(retryStatus...),
	}
	if c.opts.Transport != nil {
		opts = append(opts, remote.WithTransport(c.opts.Transport))
	}
	if c.opts.UserAgent != "" {
		opts = append(opts, remote.WithUserAgent(c.opts.UserAgent))
	}
	return opts
}

func (c *Client) nameOptions() []name.Option {
	if c.opts.Insecure {
		return []name.Option{name.Insecure}
	}
	return nil
}

// Digest returns the manifest digest a pull of ref's tag would get right now: a HEAD request
// (Docker Hub HEADs don't count toward the pull rate limit), falling back to GET when the registry
// doesn't return Docker-Content-Digest. For a multi-arch image this is the index digest, i.e. the
// same value docker stores in RepoDigests after `docker pull`.
func (c *Client) Digest(ctx context.Context, ref Ref) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	defer cancel()
	r, err := ref.reference(c.nameOptions()...)
	if err != nil {
		return "", err
	}
	opts := c.remoteOptions(ctx)
	d, err := remote.Head(r, opts...)
	if err == nil {
		return d.Digest.String(), nil
	}
	if !headUnusable(err) {
		return "", err
	}
	desc, err := remote.Get(r, opts...)
	if err != nil {
		return "", err
	}
	return desc.Digest.String(), nil
}

// headUnusable reports whether the registry answered the HEAD but not usefully (no digest header,
// or HEAD not allowed), so a GET is worth its rate-limit cost. Auth, not-found and network errors
// would fail the same way on GET.
func headUnusable(err error) bool {
	var te *transport.Error
	if errors.As(err, &te) {
		return te.StatusCode == http.StatusMethodNotAllowed
	}
	var ue *url.Error
	return !errors.As(err, &ue) && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled)
}

// Tags lists the repository's tags (all pages), within one Timeout.
func (c *Client) Tags(ctx context.Context, ref Ref) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	defer cancel()
	repo, err := name.NewRepository(ref.Repo(), c.nameOptions()...)
	if err != nil {
		return nil, err
	}
	return remote.List(repo, c.remoteOptions(ctx)...)
}

// Result is one CheckAll lookup: Digest on success, Err otherwise (see Classify).
type Result struct {
	Ref       Ref
	Digest    string
	Err       error
	CheckedAt time.Time
}

// CheckAll looks up every distinct ref once (dedupe by String()), at most Concurrency at a time,
// each with Timeout; errors are per ref and never abort the rest. Map key = Ref.String().
func (c *Client) CheckAll(ctx context.Context, refs []Ref) map[string]Result {
	out := make(map[string]Result, len(refs))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, c.opts.Concurrency)
	seen := make(map[string]bool, len(refs))
	for _, ref := range refs {
		key := ref.String()
		if seen[key] {
			continue
		}
		seen[key] = true
		wg.Go(func() {
			var res Result
			select {
			case sem <- struct{}{}:
				d, err := c.Digest(ctx, ref)
				<-sem
				res = Result{Ref: ref, Digest: d, Err: err}
			case <-ctx.Done():
				res = Result{Ref: ref, Err: ctx.Err()}
			}
			res.CheckedAt = time.Now()
			mu.Lock()
			out[key] = res
			mu.Unlock()
		})
	}
	wg.Wait()
	return out
}

// dirKeychain reads credentials from DockerConfigDir/config.json, the way authn.DefaultKeychain
// does for ~/.docker, so the server can use a mounted config without touching its environment.
// The file is read on every lookup, so `docker login` on the host takes effect without a restart.
type dirKeychain struct{ dir string }

func (k dirKeychain) Resolve(target authn.Resource) (authn.Authenticator, error) {
	cf, err := config.Load(k.dir)
	if err != nil {
		return nil, err
	}
	for _, key := range []string{target.String(), target.RegistryStr()} {
		if key == name.DefaultRegistry {
			key = authn.DefaultAuthKey // Docker Hub's historical key in config.json
		}
		ac, err := cf.GetAuthConfig(key)
		if err != nil {
			return nil, err
		}
		ac.ServerAddress = "" // set by GetAuthConfig, not part of the credentials
		if ac != (types.AuthConfig{}) {
			return authn.FromConfig(authn.AuthConfig{
				Username:      ac.Username,
				Password:      ac.Password,
				Auth:          ac.Auth,
				IdentityToken: ac.IdentityToken,
				RegistryToken: ac.RegistryToken,
			}), nil
		}
	}
	return authn.Anonymous, nil
}
