package registry

import "testing"

func TestNewerTags(t *testing.T) {
	postgres := []string{
		"latest", "alpine", "16", "16.3", "16.4", "16.4-alpine", "16-alpine", "16.3-alpine", "16-bookworm",
		"16.4-bookworm", "17", "17.0", "17.2", "17.2-alpine", "17-alpine", "18beta1", "18-rc1", "18.0-rc1",
		"20240101", "sha-1a2b3c", "main-stable", "15", "15.8",
	}
	for _, c := range []struct {
		current     string
		tags        []string
		same, major string
	}{
		{"16", postgres, "16.4", "17"},
		{"16.3", postgres, "16.4", "17.2"},
		{"16.4", postgres, "", "17.2"},
		{"16-alpine", postgres, "16.4-alpine", "17-alpine"},
		{"16.3-alpine", postgres, "16.4-alpine", "17.2-alpine"},
		{"16-bookworm", postgres, "16.4-bookworm", ""},
		{"15", postgres, "15.8", "17"},
		{"17.2", postgres, "", ""},
		{"latest", postgres, "", ""},
		{"main-stable", postgres, "", ""},
		// v prefix stays v; plain-number tags are another series.
		{"v1.2.3", []string{"v1.2.4", "v1.3.0", "1.9.9", "v2.0.0", "v2.1.0", "v2.2.0-rc.1", "v3.0.0-beta"}, "v1.3.0", "v2.1.0"},
		// Same precision wins: pinned at minor, hint at minor.
		{"1.2", []string{"1.2.9", "1.3", "1.3.1", "1.4.0", "2.0", "2.0.1"}, "1.3", "2.0"},
		// No candidate of the same precision: fall back to any.
		{"1.2", []string{"1.2.9", "2.0.1"}, "1.2.9", "2.0.1"},
		// Pre-releases are ignored, "-devel" is not one.
		{"1.0", []string{"1.1-rc1", "1.1-alpha", "2.0-beta.2", "1.1.0-dev", "1.0.1-preview"}, "", ""},
		{"12.1-devel", []string{"12.2-devel", "12.2-runtime", "13.0-devel"}, "12.2-devel", "13.0-devel"},
		// A pinned pre-release points at the release.
		{"2.0.0-rc.1", []string{"2.0.0-rc.2", "2.0.0", "2.0.1"}, "2.0.1", ""},
		// Year-based schemes still work among themselves.
		{"2024.1", []string{"2024.2", "2025.1", "1.0"}, "2024.2", "2025.1"},
	} {
		same, major := NewerTags(c.current, c.tags)
		if same != c.same || major != c.major {
			t.Errorf("NewerTags(%q) = %q, %q; want %q, %q", c.current, same, major, c.same, c.major)
		}
	}
}
