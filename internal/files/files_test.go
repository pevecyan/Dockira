package files

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"docker-manager/internal/secret"
	"docker-manager/internal/stacks"
	"docker-manager/internal/store"
)

type fakeValidator struct{ lastFiles []string }

// "broken" anywhere in a compose file or a REQUIRED= line missing from .env fails validation.
func (f *fakeValidator) Validate(_ context.Context, dir string, files []string) error {
	f.lastFiles = files
	for _, p := range files {
		b, _ := os.ReadFile(p)
		if strings.Contains(string(b), "broken") {
			return errors.New("yaml: line 3: did not find expected key in " + filepath.Base(p))
		}
	}
	return nil
}

func (f *fakeValidator) ValidateEnv(_ context.Context, dir string, files []string, env string) error {
	b, _ := os.ReadFile(env)
	if !strings.Contains(string(b), "REQUIRED=") {
		return errors.New("required variable REQUIRED is missing a value")
	}
	return nil
}

func setup(t *testing.T) (*Manager, stacks.Stack, string) {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services:\n  web:\n    image: nginx\n"), 0o640)
	os.WriteFile(filepath.Join(dir, "compose.override.yaml"), []byte("services: {}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, ".env"), []byte("REQUIRED=1\nTOKEN=s3cret-value\n"), 0o600)
	st, err := store.OpenFile(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	box, _ := secret.NewBox([]byte(strings.Repeat("k", 32)), "file revisions")
	stack := stacks.Stack{ID: "app", Dir: dir, Files: []string{filepath.Join(dir, "compose.yaml"), filepath.Join(dir, "compose.override.yaml")}, EnvFile: true}
	return &Manager{DB: st.DB, Validator: &fakeValidator{}, Box: box}, stack, dir
}

func TestListAndPaths(t *testing.T) {
	m, st, dir := setup(t)
	fs := List(st)
	if len(fs) != 3 || fs[0].Kind != "compose" || fs[1].Kind != "override" || fs[2].Name != ".env" || !fs[2].Exists || fs[0].SHA256 == "" {
		t.Fatalf("%+v", fs)
	}
	ctx := context.Background()
	// any file of the folder can be read now; ".." can't climb out of it
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM alpine\n"), 0o644)
	if c, _, err := m.Read(ctx, st, "Dockerfile"); err != nil || c != "FROM alpine\n" {
		t.Errorf("Dockerfile: %q %v", c, err)
	}
	if p, rel, err := resolve(st, "../../etc/passwd"); err != nil || rel != "etc/passwd" || !strings.HasPrefix(p, dir) {
		t.Errorf("climbing out: %s %s %v", p, rel, err)
	}
	// a symlink pointing outside the folder is refused
	outside := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(outside, []byte("x"), 0o600)
	os.Remove(filepath.Join(dir, ".env"))
	os.Symlink(outside, filepath.Join(dir, ".env"))
	if _, _, err := m.Read(ctx, st, ".env"); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Errorf("symlink escape: %v", err)
	}
}

func TestSaveConflictAndRevisions(t *testing.T) {
	m, st, dir := setup(t)
	ctx := context.Background()
	content, sum, err := m.Read(ctx, st, "compose.yaml")
	if err != nil || !strings.Contains(content, "nginx") {
		t.Fatal(content, err)
	}
	// invalid content is refused with the line, nothing written
	_, err = m.Save(ctx, st, "compose.yaml", "services:\n  web:\n    broken\n", sum, "edit", "1.2.3.4")
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Line != 3 || !strings.Contains(ve.Message, "compose.yaml") {
		t.Fatalf("validation: %#v", err)
	}
	if v := m.Validator.(*fakeValidator); filepath.Base(v.lastFiles[0]) == "compose.yaml" || filepath.Base(v.lastFiles[1]) != "compose.override.yaml" {
		t.Errorf("validated %v: the temp file should replace only the edited file", v.lastFiles)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".dm-*")); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}

	rev, err := m.Save(ctx, st, "compose.yaml", "services:\n  web:\n    image: nginx:1.27\n", sum, "edit", "1.2.3.4")
	if err != nil || rev.ID == 0 {
		t.Fatal(rev, err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "compose.yaml"))
	if string(b) != "services:\n  web:\n    image: nginx:1.27\n" {
		t.Errorf("written %q", b)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "compose.yaml")); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode %v not kept", fi.Mode())
	}
	// saving again with the old hash: conflict
	if _, err := m.Save(ctx, st, "compose.yaml", "x", sum, "edit", ""); !errors.Is(err, ErrConflict) {
		t.Errorf("stale base: %v", err)
	}
	// a change made elsewhere is recorded on the next read
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services: {} # by hand\n"), 0o640)
	m.Read(ctx, st, "compose.yaml")
	revs, _ := m.Revisions(ctx, "app", "compose.yaml", 10)
	reasons := []string{}
	for _, r := range revs {
		reasons = append(reasons, r.Reason)
	}
	if strings.Join(reasons, ",") != "external,edit,baseline" {
		t.Errorf("revisions %v", reasons)
	}
	old, err := m.Revision(ctx, "app", revs[2].ID)
	if err != nil || !strings.Contains(old.Content, "image: nginx\n") {
		t.Errorf("baseline content %q %v", old.Content, err)
	}
	if _, err := m.Revision(ctx, "other-stack", revs[2].ID); err == nil {
		t.Error("revision readable through another stack")
	}
}

func TestEnvEncryptedAndValidated(t *testing.T) {
	m, st, _ := setup(t)
	ctx := context.Background()
	_, sum, _ := m.Read(ctx, st, ".env")
	if _, err := m.Save(ctx, st, ".env", "TOKEN=x\n", sum, "edit", ""); err == nil || !strings.Contains(err.Error(), "REQUIRED") {
		t.Errorf("env validation: %v", err)
	}
	if _, err := m.Save(ctx, st, ".env", "not a line\n", sum, "edit", ""); err == nil {
		t.Error("malformed .env accepted")
	}
	var stored string
	m.DB.QueryRow(`SELECT content FROM file_revisions WHERE file_name = '.env'`).Scan(&stored)
	if !strings.HasPrefix(stored, encPrefix) || strings.Contains(stored, "s3cret") {
		t.Errorf(".env revision stored as %q", stored)
	}
	revs, _ := m.Revisions(ctx, "app", ".env", 5)
	r, err := m.Revision(ctx, "app", revs[0].ID)
	if err != nil || !strings.Contains(r.Content, "s3cret-value") {
		t.Errorf("decrypt: %q %v", r.Content, err)
	}
}

func TestNewEnvFile(t *testing.T) {
	m, st, dir := setup(t)
	os.Remove(filepath.Join(dir, ".env"))
	ctx := context.Background()
	c, sum, err := m.Read(ctx, st, ".env")
	if err != nil || c != "" || sum != "" {
		t.Fatal(c, sum, err)
	}
	if _, err := m.Save(ctx, st, ".env", "REQUIRED=yes\n", "", "edit", ""); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, ".env")); fi.Mode().Perm() != 0o600 {
		t.Errorf("new .env mode %v", fi.Mode())
	}
}

func TestTreeUploadDeleteAndProtection(t *testing.T) {
	m, st, dir := setup(t)
	ctx := context.Background()
	os.MkdirAll(filepath.Join(dir, "config", "certs"), 0o755)
	os.WriteFile(filepath.Join(dir, "config", "app.yaml"), []byte("key: value\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "config", "logo.png"), []byte{0x89, 'P', 'N', 'G', 0, 0, 1}, 0o644)
	os.Symlink("/etc", filepath.Join(dir, "etc-link"))
	os.Symlink("config", filepath.Join(dir, "cfg"))

	l, err := Tree(st, "")
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, e := range l.Entries {
		names = append(names, e.Name+":"+e.Type)
	}
	if strings.Join(names, ",") != "cfg:dir,config:dir,.env:file,compose.override.yaml:file,compose.yaml:file,etc-link:symlink" {
		t.Errorf("tree %v", names)
	}
	for _, e := range l.Entries {
		if e.Name == "etc-link" && (!e.Outside || e.Readable) {
			t.Errorf("outside link: %+v", e)
		}
	}
	if _, _, err := m.Read(ctx, st, "etc-link/hostname"); err == nil {
		t.Error("read through a symlink leaving the folder")
	}
	if c, _, err := m.Read(ctx, st, "cfg/app.yaml"); err != nil || c != "key: value\n" {
		t.Errorf("inside link: %q %v", c, err)
	}
	if _, _, err := m.Read(ctx, st, "config/logo.png"); !errors.Is(err, ErrBinary) {
		t.Errorf("binary: %v", err)
	}

	// syntax checks for YAML and JSON
	_, sum, _ := m.Read(ctx, st, "config/app.yaml")
	if _, err := m.Save(ctx, st, "config/app.yaml", "key: [broken\n", sum, "edit", ""); err == nil {
		t.Error("broken YAML saved")
	}
	if _, err := m.Save(ctx, st, "config/new.json", `{"a": 1,}`, "", "edit", ""); err == nil {
		t.Error("broken JSON saved")
	}
	if _, err := m.Save(ctx, st, "config/new/nested.txt", "hello", "", "edit", ""); err != nil {
		t.Errorf("new file in a new folder: %v", err)
	}
	var stored string
	m.DB.QueryRow(`SELECT content FROM file_revisions WHERE file_name = 'config/new/nested.txt'`).Scan(&stored)
	if !strings.HasPrefix(stored, encPrefix) {
		t.Errorf("non-compose revision stored in plain text: %q", stored)
	}

	if err := m.Upload(ctx, st, "config/certs/key.pem", []byte("-----BEGIN-----\n"), ""); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(ctx, st, "config/certs/key.pem", "1.2.3.4"); err != nil {
		t.Fatal(err)
	}
	revs, _ := m.Revisions(ctx, "app", "config/certs/key.pem", 5)
	if len(revs) != 2 || revs[0].Reason != "delete" {
		t.Errorf("revisions %+v", revs)
	}
	if err := m.Delete(ctx, st, "config", ""); err == nil {
		t.Error("deleted a folder that isn't empty")
	}
	if err := m.Delete(ctx, st, "compose.yaml", ""); err == nil {
		t.Error("deleted the compose file")
	}

	// docker-manager's own folder: config.yaml and data/ are out of reach
	self := st
	self.Self = true
	os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("secret_key: x"), 0o600)
	if _, _, err := m.Read(ctx, self, "config.yaml"); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("own config readable: %v", err)
	}
	if _, err := Tree(self, "data"); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("own data listable: %v", err)
	}
}

func TestNewApp(t *testing.T) {
	m, _, _ := setup(t)
	ctx := context.Background()
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "existing-app"), 0o755)
	os.WriteFile(filepath.Join(root, "existing-app", "compose.yaml"), []byte("services: {}"), 0o644)
	os.MkdirAll(filepath.Join(root, "copied"), 0o755)
	os.WriteFile(filepath.Join(root, "copied", "app.conf"), []byte("x"), 0o644)
	good := "services:\n  web:\n    image: nginx\n"

	for path, want := range map[string]string{
		"":                   "name the app",
		".hidden":            "letters, digits",
		"a/b/c/d":            "at most 3",
		"existing-app":       "compose file already",
		"existing-app/inner": "is an app already",
		"has space":          "letters, digits",
		"../escape":          "letters, digits",
	} {
		if _, err := m.Create(ctx, NewApp{Root: root, Depth: 3, Path: path, Compose: good}, ""); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v", path, err)
		}
	}
	// invalid compose: nothing is left behind
	if _, err := m.Create(ctx, NewApp{Root: root, Depth: 3, Path: "tools/bad", Compose: "services:\n  x:\n    broken\n"}, ""); err == nil {
		t.Error("broken compose created")
	}
	if _, err := os.Stat(filepath.Join(root, "tools")); !os.IsNotExist(err) {
		t.Errorf("leftover folder: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(root, ".dm-new-*")); len(left) != 0 {
		t.Errorf("temp folders left: %v", left)
	}

	dir, err := m.Create(ctx, NewApp{Root: root, Depth: 3, Path: "tools/myapp", Compose: good, Env: "REQUIRED=1\nTOKEN=abc\n"}, "1.2.3.4")
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "compose.yaml")); string(b) != good {
		t.Errorf("compose %q", b)
	}
	if fi, _ := os.Stat(filepath.Join(dir, ".env")); fi.Mode().Perm() != 0o600 {
		t.Errorf(".env mode %v", fi.Mode())
	}
	if revs, _ := m.Revisions(ctx, "tools/myapp", "", 5); len(revs) != 2 {
		t.Errorf("revisions %+v", revs)
	}
	// adopting a folder that has files but no compose file keeps its files
	if _, err := m.Create(ctx, NewApp{Root: root, Depth: 3, Path: "copied", Compose: good}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "copied", "app.conf")); err != nil {
		t.Errorf("adopted folder lost its files: %v", err)
	}
}
