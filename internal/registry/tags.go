package registry

import (
	"regexp"
	"strconv"
	"strings"
)

// version is a tag that looks like a version: optional "v", 1–4 numeric parts, optional
// "-suffix" ("-alpine", "-bookworm", "-rc.1").
type version struct {
	raw    string
	prefix string
	nums   []int
	suffix string
	pre    bool
}

var (
	versionRE    = regexp.MustCompile(`^(v?)(\d+(?:\.\d+){0,3})(-[0-9A-Za-z][0-9A-Za-z._-]*)?$`)
	suffixWordRE = regexp.MustCompile(`[A-Za-z]+`)
	preWords     = map[string]bool{"alpha": true, "beta": true, "rc": true, "dev": true, "pre": true, "preview": true, "snapshot": true, "nightly": true, "canary": true, "unstable": true}
)

func parseVersion(tag string) (version, bool) {
	m := versionRE.FindStringSubmatch(tag)
	if m == nil {
		return version{}, false
	}
	v := version{raw: tag, prefix: m[1], suffix: m[3]}
	for p := range strings.SplitSeq(m[2], ".") {
		n, err := strconv.Atoi(p)
		if err != nil {
			return version{}, false
		}
		v.nums = append(v.nums, n)
	}
	// Whole words only, so "-devel" or "-alpine" aren't mistaken for "dev" or "alpha".
	for _, w := range suffixWordRE.FindAllString(v.suffix, -1) {
		if preWords[strings.ToLower(w)] {
			v.pre = true
		}
	}
	return v, true
}

// compare orders by numeric parts; missing parts count as 0, so "16" == "16.0".
func compare(a, b version) int {
	for i := range max(len(a.nums), len(b.nums)) {
		x, y := at(a.nums, i), at(b.nums, i)
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

func at(s []int, i int) int {
	if i < len(s) {
		return s[i]
	}
	return 0
}

// NewerTags gives version hints for a pinned version tag; they are never applied automatically.
// sameSeries is the newest tag in current's major series that is newer than current ("16" →
// "16.4"), newestMajor the newest tag with a higher major ("16" → "17"); "" when none.
//
// Only tags of the same shape count: same "v" prefix and same suffix, so the "16-alpine" series
// stays "-alpine". Non-version tags (latest, main-stable, sha-…) and pre-releases are ignored.
// Among candidates, ones with as many parts as current win ("16.4" → "16.5" rather than
// "16.5.1"), because that is the granularity the user chose to pin at.
func NewerTags(current string, tags []string) (sameSeries, newestMajor string) {
	cur, ok := parseVersion(current)
	if !ok {
		return "", ""
	}
	suffix := cur.suffix
	if cur.pre {
		suffix = "" // pinned to a pre-release: point at the releases
	}
	var same, major []version
	for _, t := range tags {
		v, ok := parseVersion(t)
		if !ok || v.pre || v.prefix != cur.prefix || v.suffix != suffix {
			continue
		}
		// Date tags (20240101) parse as huge majors; they are a different scheme, not a new major.
		if cur.nums[0] < 1000 && v.nums[0] >= 1000 {
			continue
		}
		if c := compare(v, cur); c < 0 || (c == 0 && !cur.pre) {
			continue
		}
		switch {
		case v.nums[0] == cur.nums[0]:
			same = append(same, v)
		case v.nums[0] > cur.nums[0]:
			major = append(major, v)
		}
	}
	return newest(same, len(cur.nums)), newest(major, len(cur.nums))
}

// newest picks the highest version, preferring ones with `parts` numeric parts and, on equal
// versions ("17" vs "17.0"), the shorter tag.
func newest(vs []version, parts int) string {
	var exact []version
	for _, v := range vs {
		if len(v.nums) == parts {
			exact = append(exact, v)
		}
	}
	if len(exact) > 0 {
		vs = exact
	}
	var best *version
	for i := range vs {
		v := &vs[i]
		if best == nil {
			best = v
			continue
		}
		if c := compare(*v, *best); c > 0 || (c == 0 && len(v.nums) < len(best.nums)) {
			best = v
		}
	}
	if best == nil {
		return ""
	}
	return best.raw
}
