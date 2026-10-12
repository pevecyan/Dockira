// Package config loads the server config (/etc/docker-manager/config.yaml). Settings that are
// edited in the UI (notifications, update policies, …) live in SQLite instead.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const DefaultPath = "/etc/docker-manager/config.yaml"

type Config struct {
	Listen         string   `yaml:"listen"`          // HTTP address (TLS is the reverse proxy's job)
	AppsRoot       string   `yaml:"apps_root"`       // one folder per compose stack, searched scan_depth levels deep
	ScanDepth      int      `yaml:"scan_depth"`      //
	DataDir        string   `yaml:"data_dir"`        // SQLite database and backups
	PasswordHash   string   `yaml:"password_hash"`   // bcrypt (`docker-manager password`)
	SecretKey      string   `yaml:"secret_key"`      // base64, ≥ 32 bytes (`docker-manager gen-key`): signs sessions, encrypts channel secrets
	PublicURL      string   `yaml:"public_url"`      // e.g. https://docker.example.com: Origin check and links in notifications
	TrustedProxies []string `yaml:"trusted_proxies"` // CIDRs whose X-Forwarded-* headers are believed
	IPAllowlist    []string `yaml:"ip_allowlist"`    // optional CIDRs; empty = everyone
	ReadOnly       bool     `yaml:"read_only"`       // refuse every change (dev against the real server)
	ExecEnabled    bool     `yaml:"exec_enabled"`    // container shell in the UI
	Dev            bool     `yaml:"dev"`             // API docs at /api/docs, Vite origin allowed
}

func Default() Config {
	return Config{
		Listen:    ":8080",
		AppsRoot:  "/root/apps",
		ScanDepth: 3,
		DataDir:   "/data",
	}
}

// Load reads the config file and applies DM_<KEY> environment overrides (e.g. DM_APPS_ROOT,
// DM_READ_ONLY=true, DM_TRUSTED_PROXIES=a,b). A missing file yields the defaults.
func Load(path string) (Config, error) {
	c := Default()
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return c, err
	default:
		if err := yaml.Unmarshal(b, &c); err != nil {
			return c, fmt.Errorf("%s: %w", path, err)
		}
	}
	if err := applyEnv(&c); err != nil {
		return c, err
	}
	return c, nil
}

func applyEnv(c *Config) error {
	v := reflect.ValueOf(c).Elem()
	t := v.Type()
	for i := range t.NumField() {
		key := "DM_" + strings.ToUpper(t.Field(i).Tag.Get("yaml"))
		s, ok := os.LookupEnv(key)
		if !ok {
			continue
		}
		f := v.Field(i)
		switch f.Kind() {
		case reflect.String:
			f.SetString(s)
		case reflect.Bool:
			b, err := strconv.ParseBool(s)
			if err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			f.SetBool(b)
		case reflect.Int:
			n, err := strconv.Atoi(s)
			if err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			f.SetInt(int64(n))
		case reflect.Slice:
			var list []string
			for _, p := range strings.Split(s, ",") {
				if p = strings.TrimSpace(p); p != "" {
					list = append(list, p)
				}
			}
			f.Set(reflect.ValueOf(list))
		}
	}
	return nil
}

// Validate refuses configs the server must not run with: no password, no key, bad CIDRs.
func (c Config) Validate() error {
	var errs []error
	if c.PasswordHash == "" {
		errs = append(errs, errors.New("password_hash is not set: run `docker-manager password`"))
	}
	if _, err := c.Key(); err != nil {
		errs = append(errs, err)
	}
	if c.AppsRoot == "" {
		errs = append(errs, errors.New("apps_root is empty"))
	}
	if c.ScanDepth < 1 {
		errs = append(errs, errors.New("scan_depth must be at least 1"))
	}
	if c.PublicURL != "" {
		if u, err := url.Parse(c.PublicURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("public_url %q is not an http(s) URL", c.PublicURL))
		}
	}
	for _, list := range [][]string{c.TrustedProxies, c.IPAllowlist} {
		if _, err := ParseCIDRs(list); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Key decodes secret_key.
func (c Config) Key() ([]byte, error) {
	if c.SecretKey == "" {
		return nil, errors.New("secret_key is not set: run `docker-manager gen-key`")
	}
	k, err := base64.StdEncoding.DecodeString(c.SecretKey)
	if err != nil {
		return nil, fmt.Errorf("secret_key is not base64: %w", err)
	}
	if len(k) < 32 {
		return nil, fmt.Errorf("secret_key has %d bytes, needs at least 32", len(k))
	}
	return k, nil
}

// ParseCIDRs accepts CIDRs and bare IPs (as /32 or /128).
func ParseCIDRs(list []string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, s := range list {
		if !strings.Contains(s, "/") {
			ip := net.ParseIP(s)
			if ip == nil {
				return nil, fmt.Errorf("%q is not an IP or CIDR", s)
			}
			bits := 128
			if ip.To4() != nil {
				ip, bits = ip.To4(), 32
			}
			out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			return nil, fmt.Errorf("%q is not an IP or CIDR", s)
		}
		out = append(out, n)
	}
	return out, nil
}
