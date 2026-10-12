package registry

import (
	"regexp"
	"strings"
)

// BaseImages returns the images a Dockerfile builds FROM, for update checks of `build:` services.
// It skips "scratch" and FROMs of earlier stages (multi-stage builds), drops --platform and other
// flags, and substitutes $ARG / ${ARG} (also ${ARG:-default}, ${ARG:+alt}) from the ARGs declared
// before the first FROM, with buildArgs taking precedence. As in docker, ARGs declared inside a
// stage are not visible to later FROM lines. Refs still containing "$" after substitution are
// returned in unresolved. Both lists keep file order and have no duplicates.
func BaseImages(dockerfile string, buildArgs map[string]string) (refs []string, unresolved []string) {
	args := map[string]string{} // global ARGs with a value
	lookup := func(k string) (string, bool) {
		if v, ok := buildArgs[k]; ok {
			return v, true
		}
		v, ok := args[k]
		return v, ok
	}
	stages := map[string]bool{}
	seen := map[string]bool{}
	inStage := false
	for _, ins := range instructions(dockerfile) {
		kw, rest, _ := strings.Cut(ins, " ")
		switch strings.ToUpper(kw) {
		case "ARG":
			if inStage {
				continue
			}
			for _, a := range shellWords(rest) {
				k, v, ok := strings.Cut(a, "=")
				if ok {
					args[k] = expand(v, lookup) // a default may use an earlier ARG
				}
			}
		case "FROM":
			inStage = true
			f := strings.Fields(rest)
			for len(f) > 0 && strings.HasPrefix(f[0], "--") {
				f = f[1:]
			}
			if len(f) == 0 {
				continue
			}
			img := expand(f[0], lookup)
			switch {
			case strings.EqualFold(img, "scratch"), stages[strings.ToLower(img)], seen[img]:
			case strings.Contains(img, "$"):
				seen[img] = true
				unresolved = append(unresolved, img)
			default:
				seen[img] = true
				refs = append(refs, img)
			}
			if len(f) >= 3 && strings.EqualFold(f[1], "AS") {
				stages[strings.ToLower(f[2])] = true
			}
		}
	}
	return refs, unresolved
}

var heredocRE = regexp.MustCompile(`<<-?\s*["']?([A-Za-z_][A-Za-z0-9_]*)["']?`)

// instructions splits a Dockerfile into logical instructions: comments dropped (also between
// continuation lines, as docker does), "\" continuations joined, heredoc bodies skipped (a Python
// heredoc's "from x import y" is not a FROM).
func instructions(df string) []string {
	var out, heredocs []string
	var cur strings.Builder
	for line := range strings.SplitSeq(strings.ReplaceAll(df, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(line)
		if len(heredocs) > 0 {
			if t == heredocs[0] {
				heredocs = heredocs[1:]
			}
			continue
		}
		if strings.HasPrefix(t, "#") || (t == "" && cur.Len() == 0) {
			continue
		}
		if s, ok := strings.CutSuffix(t, `\`); ok {
			cur.WriteString(s + " ")
			continue
		}
		cur.WriteString(t)
		ins := strings.Join(strings.Fields(cur.String()), " ")
		cur.Reset()
		out = append(out, ins)
		for _, m := range heredocRE.FindAllStringSubmatch(ins, -1) {
			heredocs = append(heredocs, m[1])
		}
	}
	if cur.Len() > 0 {
		out = append(out, strings.Join(strings.Fields(cur.String()), " "))
	}
	return out
}

// shellWords splits on unquoted whitespace and removes the quotes: ARG V="1.2 x" W=3.
func shellWords(s string) []string {
	var words []string
	var w strings.Builder
	var quote rune
	inWord := false
	for _, r := range s {
		switch {
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
			w.WriteRune(r)
		case r == '"' || r == '\'':
			quote, inWord = r, true
		case r == ' ' || r == '\t':
			if inWord {
				words = append(words, w.String())
				w.Reset()
				inWord = false
			}
		default:
			w.WriteRune(r)
			inWord = true
		}
	}
	if inWord {
		words = append(words, w.String())
	}
	return words
}

var varRE = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?:(:?[-+])([^}]*))?\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// expand substitutes variables like docker does in FROM lines. An unset variable without a
// default is left as written, so the caller can tell the ref is unresolved.
func expand(s string, lookup func(string) (string, bool)) string {
	return varRE.ReplaceAllStringFunc(s, func(m string) string {
		p := varRE.FindStringSubmatch(m)
		k, op, word := p[1], p[2], p[3]
		if k == "" {
			k = p[4]
		}
		v, set := lookup(k)
		switch op {
		case "":
			if !set {
				return m
			}
			return v
		case ":-":
			if !set || v == "" {
				return expand(word, lookup)
			}
		case "-":
			if !set {
				return expand(word, lookup)
			}
		case ":+":
			if set && v != "" {
				return expand(word, lookup)
			}
			return ""
		case "+":
			if set {
				return expand(word, lookup)
			}
			return ""
		}
		return v
	})
}
