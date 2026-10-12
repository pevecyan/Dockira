// Package dotenv reads .env files the way compose does (KEY=VALUE, optional quotes, comments,
// `export` prefix), keeping line numbers so the editor can point at entries.
package dotenv

import (
	"bufio"
	"regexp"
	"strings"
)

type Entry struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	Line  int    `json:"line"`
}

var keyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`)

// Parse returns the entries; malformed lines are reported by line number.
func Parse(content string) (entries []Entry, bad []int) {
	sc := bufio.NewScanner(strings.NewReader(content))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	n := 0
	for sc.Scan() {
		n++
		l := strings.TrimSpace(sc.Text())
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		l = strings.TrimPrefix(l, "export ")
		k, v, ok := strings.Cut(l, "=")
		k = strings.TrimSpace(k)
		if !ok || !keyRe.MatchString(k) {
			bad = append(bad, n)
			continue
		}
		entries = append(entries, Entry{Key: k, Value: unquote(strings.TrimSpace(v)), Line: n})
	}
	return entries, bad
}

func unquote(v string) string {
	if len(v) >= 2 {
		switch q := v[0]; {
		case (q == '"' || q == '\'') && v[len(v)-1] == q:
			inner := v[1 : len(v)-1]
			if q == '"' {
				inner = strings.NewReplacer(`\n`, "\n", `\"`, `"`, `\\`, `\`).Replace(inner)
			}
			return inner
		}
	}
	if i := strings.Index(v, " #"); i >= 0 { // inline comment after an unquoted value
		v = strings.TrimSpace(v[:i])
	}
	return v
}

// Values returns all values (for scrubbing secrets out of job output).
func Values(content string) []string {
	entries, _ := Parse(content)
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Value)
	}
	return out
}

// Referenced returns the variable names a compose file interpolates (${VAR}, ${VAR:-x}, $VAR).
func Referenced(compose string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range refRe.FindAllStringSubmatch(compose, -1) {
		name := m[1]
		if name == "" {
			name = m[2]
		}
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// $$ is an escaped dollar; ${NAME…} or $NAME.
var refRe = regexp.MustCompile(`(?:^|[^$])\$(?:\{([A-Za-z_][A-Za-z0-9_]*)[^}]*\}|([A-Za-z_][A-Za-z0-9_]*))`)
