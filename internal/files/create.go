package files

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"docker-manager/internal/compose"
	"docker-manager/internal/dotenv"
)

// NewApp is a new stack: a folder under apps_root with a compose file and an optional .env.
type NewApp struct {
	Root    string // apps_root
	Depth   int    // scan_depth: deeper folders wouldn't be found
	Path    string // folder relative to Root, e.g. "myapp" or "tools/myapp"
	Compose string
	Env     string
}

var segmentRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// target checks the folder for a new app and returns its absolute path and whether it exists.
func (n NewApp) target() (string, bool, error) {
	rel := strings.Trim(filepath.ToSlash(strings.TrimSpace(n.Path)), "/")
	if rel == "" {
		return "", false, errors.New("name the app's folder")
	}
	segs := strings.Split(rel, "/")
	depth := n.Depth
	if depth < 1 {
		depth = 3
	}
	if len(segs) > depth {
		return "", false, fmt.Errorf("at most %d folder levels below the apps folder (scan_depth)", depth)
	}
	for _, s := range segs {
		if !segmentRe.MatchString(s) {
			return "", false, fmt.Errorf("%q: use letters, digits, '.', '-' and '_' (not starting with '.' or '-')", s)
		}
	}
	root, err := filepath.EvalSymlinks(n.Root)
	if err != nil {
		return "", false, fmt.Errorf("apps folder: %w", err)
	}
	// a folder inside another app's folder would be that app's data, not a new app
	cur := root
	for _, s := range segs[:len(segs)-1] {
		cur = filepath.Join(cur, s)
		if compose.FindFiles(cur) != nil {
			rel, _ := filepath.Rel(root, cur)
			return "", false, fmt.Errorf("%s is an app already: a folder inside it wouldn't be found", filepath.ToSlash(rel))
		}
	}
	dir := filepath.Join(root, filepath.FromSlash(rel))
	fi, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return dir, false, nil
	case err != nil:
		return "", false, err
	case !fi.IsDir():
		return "", false, fmt.Errorf("%s exists and is not a folder", rel)
	case compose.FindFiles(dir) != nil:
		return "", false, fmt.Errorf("%s has a compose file already: it is an app", rel)
	}
	return dir, true, nil
}

// ValidateNew checks a new app's compose file (with its .env) the way `up` would read it, in the
// real folder when it exists (relative paths resolve there) or in a temporary hidden one.
func (m *Manager) ValidateNew(ctx context.Context, n NewApp) error {
	dir, exists, err := n.target()
	if err != nil {
		return &ValidationError{Message: err.Error()}
	}
	if strings.TrimSpace(n.Compose) == "" {
		return &ValidationError{Message: "the compose file is empty"}
	}
	if len(n.Compose) > MaxSize || len(n.Env) > MaxSize {
		return ErrTooLarge
	}
	work := dir
	if !exists {
		b := make([]byte, 6)
		_, _ = rand.Read(b)
		root, err := filepath.EvalSymlinks(n.Root)
		if err != nil {
			return err
		}
		// directly in the apps folder: creating the app's parent folders here would leave them behind
		work = filepath.Join(root, ".dm-new-"+hex.EncodeToString(b))
		if err := os.MkdirAll(work, 0o700); err != nil {
			return err
		}
		defer os.RemoveAll(work)
	}
	cf, err := tempFile(work, "compose.yaml", n.Compose)
	if err != nil {
		return err
	}
	defer os.Remove(cf)
	if n.Env != "" {
		if _, bad := dotenv.Parse(n.Env); len(bad) > 0 {
			return &ValidationError{Message: fmt.Sprintf(".env line %d is not KEY=value", bad[0])}
		}
		ef, err := tempFile(work, ".env", n.Env)
		if err != nil {
			return err
		}
		defer os.Remove(ef)
		err = m.Validator.ValidateEnv(ctx, work, []string{cf}, ef)
		if err != nil {
			return parseError(strings.ReplaceAll(err.Error(), filepath.Base(cf), "compose.yaml"))
		}
		return nil
	}
	if err := m.Validator.Validate(ctx, work, []string{cf}); err != nil {
		return parseError(strings.ReplaceAll(err.Error(), filepath.Base(cf), "compose.yaml"))
	}
	return nil
}

// Create validates and writes a new app (compose.yaml, and .env when given). It never touches
// an existing compose file; a folder it created is removed again when writing fails.
func (m *Manager) Create(ctx context.Context, n NewApp, ip string) (string, error) {
	if err := m.ValidateNew(ctx, n); err != nil {
		return "", err
	}
	dir, exists, err := n.target()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	fail := func(err error) (string, error) {
		if !exists {
			os.RemoveAll(dir)
		}
		return "", err
	}
	if n.Env != "" {
		if _, err := os.Stat(filepath.Join(dir, EnvName)); err == nil {
			return fail(errors.New("the folder has a .env already: leave the .env field empty to keep it"))
		}
		if err := writeAtomic(filepath.Join(dir, EnvName), []byte(n.Env)); err != nil {
			return fail(err)
		}
	}
	if err := writeAtomic(filepath.Join(dir, "compose.yaml"), []byte(n.Compose)); err != nil {
		return fail(err)
	}
	id := strings.Trim(filepath.ToSlash(strings.TrimSpace(n.Path)), "/")
	m.record(ctx, id, "compose.yaml", n.Compose, Hash(n.Compose), "edit", ip)
	if n.Env != "" {
		m.record(ctx, id, EnvName, n.Env, Hash(n.Env), "edit", ip)
	}
	return dir, nil
}
