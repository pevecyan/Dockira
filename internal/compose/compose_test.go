package compose

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFindFiles(t *testing.T) {
	d := t.TempDir()
	if FindFiles(d) != nil {
		t.Fatal("empty dir has no compose files")
	}
	write(t, filepath.Join(d, "docker-compose.yml"), "services: {}")
	write(t, filepath.Join(d, "compose.yaml"), "services: {}")
	write(t, filepath.Join(d, "compose.override.yml"), "services: {}")
	got := FindFiles(d)
	if len(got) != 2 || filepath.Base(got[0]) != "compose.yaml" || filepath.Base(got[1]) != "compose.override.yml" {
		t.Errorf("%v", got)
	}
}

func TestImageNameAndArgs(t *testing.T) {
	v := "3.20"
	p := &Project{Name: "shop", Services: map[string]Service{
		"web": {Image: "nginx:1"},
		"app": {Build: &Build{Context: ".", Args: map[string]*string{"V": &v, "FROM_ENV": nil}}},
	}}
	if p.ImageName("web") != "nginx:1" || p.ImageName("app") != "shop-app" {
		t.Errorf("%s %s", p.ImageName("web"), p.ImageName("app"))
	}
	if a := p.Services["app"].Build.BuildArgs(); len(a) != 1 || a["V"] != "3.20" {
		t.Errorf("%v", a)
	}
	if got := strings.Join(Args("/a", []string{"/a/compose.yaml"}, "up", "-d"), " "); got != "compose --project-directory /a -f /a/compose.yaml up -d" {
		t.Error(got)
	}
}

// The CLI tests need docker compose on PATH; they never start containers.
func TestConfigAndValidateWithRealCLI(t *testing.T) {
	if exec.Command("docker", "compose", "version").Run() != nil {
		t.Skip("docker compose not available")
	}
	ctx := context.Background()
	d := t.TempDir()
	write(t, filepath.Join(d, "compose.yaml"), "name: demo\nservices:\n  web:\n    image: nginx:${TAG:-1}\n    ports: ['8080:80']\n  app:\n    build: .\n")
	write(t, filepath.Join(d, ".env"), "TAG=1.27\n")
	c := New()
	p, err := c.Config(ctx, d, FindFiles(d))
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "demo" || p.Services["web"].Image != "nginx:1.27" || p.Services["app"].Build == nil || p.Services["web"].Ports[0].Target != 80 {
		t.Errorf("%+v", p)
	}
	if err := c.Validate(ctx, d, FindFiles(d)); err != nil {
		t.Errorf("valid file: %v", err)
	}
	write(t, filepath.Join(d, "compose.yaml"), "services:\n  web:\n    image: [broken\n")
	if err := c.Validate(ctx, d, FindFiles(d)); err == nil {
		t.Error("broken file validated")
	}
}
