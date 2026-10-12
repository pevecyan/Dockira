package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validKey = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=" // 32 bytes

func TestLoadFileAndEnv(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(p, []byte("apps_root: /srv/apps\nscan_depth: 2\ntrusted_proxies: [10.0.0.1]\n"), 0o600)
	t.Setenv("DM_SCAN_DEPTH", "4")
	t.Setenv("DM_READ_ONLY", "true")
	t.Setenv("DM_IP_ALLOWLIST", "192.168.1.0/24, 10.0.0.0/8")

	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.AppsRoot != "/srv/apps" || c.ScanDepth != 4 || !c.ReadOnly || c.Listen != ":8080" {
		t.Errorf("unexpected config: %+v", c)
	}
	if len(c.TrustedProxies) != 1 || len(c.IPAllowlist) != 2 || c.IPAllowlist[1] != "10.0.0.0/8" {
		t.Errorf("lists: %v %v", c.TrustedProxies, c.IPAllowlist)
	}
}

func TestLoadMissingFileGivesDefaults(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil || c.AppsRoot != "/root/apps" || c.ScanDepth != 3 {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestValidate(t *testing.T) {
	ok := Default()
	ok.PasswordHash = "$2a$10$x"
	ok.SecretKey = validKey
	ok.PublicURL = "https://docker.example.com"
	ok.TrustedProxies = []string{"192.168.1.10", "172.16.0.0/12", "::1"}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid config refused: %v", err)
	}

	for name, mut := range map[string]func(*Config){
		"password_hash": func(c *Config) { c.PasswordHash = "" },
		"secret_key":    func(c *Config) { c.SecretKey = "" },
		"at least 32":   func(c *Config) { c.SecretKey = "c2hvcnQ=" },
		"public_url":    func(c *Config) { c.PublicURL = "docker.example.com" },
		"not an IP":     func(c *Config) { c.TrustedProxies = []string{"proxy"} },
		"scan_depth":    func(c *Config) { c.ScanDepth = 0 },
	} {
		c := ok
		mut(&c)
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: got %v", name, err)
		}
	}
}
