package main

// Minimal shell-command structure for bash and PowerShell: top-level segments, pipes and words.
// Anything we cannot read confidently (here-docs, here-strings, subshells spanning segments) is
// reported as opaque so callers leave the command alone.

import (
	"regexp"
	"strings"
)

// Segment is one top-level command and the separator after it ("", ";", "\n", "&&" or "||").
type Segment struct {
	Text string
	Sep  string
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

// segments splits at top-level `;`, `&&`, `||` and newlines, respecting quotes, braces and parens.
func segments(cmd string) []Segment {
	var out []Segment
	start, depth := 0, 0
	var q byte
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		if q != 0 {
			if c == '\\' && q == '"' && i+1 < len(cmd) {
				i++
				continue
			}
			if c == '`' && q == '"' {
				i++
				continue
			}
			if c == q {
				q = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			q = c
			continue
		case '\\':
			i++
			continue
		case '(', '{', '[':
			depth++
			continue
		case ')', '}', ']':
			depth = max(0, depth-1)
			continue
		}
		if depth > 0 {
			continue
		}
		if i+1 < len(cmd) && (cmd[i:i+2] == "&&" || cmd[i:i+2] == "||") {
			out = append(out, Segment{cmd[start:i], cmd[i : i+2]})
			start = i + 2
			i++
			continue
		}
		if c == ';' || c == '\n' {
			out = append(out, Segment{cmd[start:i], string(c)})
			start = i + 1
		}
	}
	return append(out, Segment{cmd[min(start, len(cmd)):], ""})
}

// pipes splits one segment at top-level single `|`.
func pipes(seg string) []string {
	var out []string
	start, depth := 0, 0
	var q byte
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		if q != 0 {
			if c == '\\' && q == '"' {
				i++
				continue
			}
			if c == q {
				q = 0
			}
			continue
		}
		switch {
		case c == '\'' || c == '"':
			q = c
		case c == '(' || c == '{':
			depth++
		case c == ')' || c == '}':
			depth = max(0, depth-1)
		case c == '|' && depth == 0 && (i+1 >= len(seg) || seg[i+1] != '|') && (i == 0 || seg[i-1] != '|'):
			out = append(out, seg[start:i])
			start = i + 1
		}
	}
	return append(out, seg[min(start, len(seg)):])
}

// words returns the words of a simple command with quotes removed, or nil if it uses expansion
// we can't evaluate.
func words(seg string) []string {
	w := []string{}
	var cur strings.Builder
	var q byte
	any := false
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		if q != 0 {
			if c == q {
				q = 0
				continue
			}
			if q == '"' && (c == '$' || c == '`') {
				return nil
			}
			cur.WriteByte(c)
			continue
		}
		switch c {
		case '\'', '"':
			q = c
			any = true
			continue
		case '$', '`', '(', ')', '{', '}', '<', '>', '*', '?':
			return nil
		}
		if isSpace(c) {
			if cur.Len() > 0 || any {
				w = append(w, cur.String())
			}
			cur.Reset()
			any = false
			continue
		}
		cur.WriteByte(c)
	}
	if q != 0 {
		return nil
	}
	if cur.Len() > 0 || any {
		w = append(w, cur.String())
	}
	return w
}

// outsideQuotes applies fn to the unquoted stretches of s only; quoted text belongs to another program.
func outsideQuotes(s string, fn func(string) string) string {
	var out strings.Builder
	bufStart := 0
	var q byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if q != 0 {
			out.WriteByte(c)
			if c == q {
				q = 0
				bufStart = i + 1
			}
			continue
		}
		if c == '\'' || c == '"' {
			out.WriteString(fn(s[bufStart:i]))
			out.WriteByte(c)
			q = c
		}
	}
	if q == 0 {
		out.WriteString(fn(s[bufStart:]))
	} else {
		out.WriteString(fn(""))
	}
	return out.String()
}

// unquoted is the unquoted text of s, for structural tests such as "is there a redirect".
func unquoted(s string) string {
	var b strings.Builder
	outsideQuotes(s, func(t string) string { b.WriteString(t); return t })
	return b.String()
}

var opaqueRe = regexp.MustCompile(`(?m)<<-?\s*['"]?\w+|@['"]\s*\r?\n|\$\(\s*$`)

// opaque is true when the command contains constructs whose structure we should not touch.
func opaque(cmd string) bool { return opaqueRe.MatchString(cmd) }

// quote makes s one literal word for the given shell.
func quote(s, shell string) string {
	if shell == "pwsh" {
		return "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func join(segs []Segment) string {
	var b strings.Builder
	for _, s := range segs {
		b.WriteString(s.Text)
		b.WriteString(s.Sep)
	}
	return b.String()
}

// rgValueFlags are the rg options whose value is the next word.
var rgValueFlags = map[string]bool{
	"-g": true, "--glob": true, "--iglob": true, "-t": true, "--type": true, "-T": true, "--type-not": true,
	"-e": true, "--regexp": true, "-f": true, "--file": true, "-m": true, "--max-count": true,
	"-A": true, "--after-context": true, "-B": true, "--before-context": true, "-C": true, "--context": true,
	"-M": true, "--max-columns": true, "-j": true, "--threads": true, "-E": true, "--encoding": true,
	"-d": true, "--max-depth": true, "--sort": true, "--sortr": true, "-r": true, "--replace": true,
	"--type-add": true, "--colors": true, "--pre": true, "--max-filesize": true, "--path-separator": true,
}

// rgShellGlobs rewrites the unquoted path globs of an rg command for PowerShell, which passes
// `scripts/*.ts` to rg as a literal file name (os error 123), into rg's own glob filter:
// `rg pat scripts/*.ts` becomes `rg -g '*.ts' pat scripts`. 97 Codex calls hit this in two weeks.
func rgShellGlobs(seg string) (string, bool) {
	toks := quotedWords(seg)
	if len(toks) < 2 || toks[0] != "rg" {
		return seg, false
	}
	patternGiven, positional, skip := false, 0, false
	var globs []string
	out := []string{"rg"}
	bases := map[string]bool{}
	for _, tok := range toks[1:] {
		if skip {
			skip = false
			out = append(out, tok)
			continue
		}
		if tok == "--" || strings.HasPrefix(tok, "-") {
			if rgValueFlags[tok] {
				skip = true
			}
			if tok == "-e" || tok == "--regexp" || tok == "-f" || tok == "--file" || strings.HasPrefix(tok, "--regexp=") || strings.HasPrefix(tok, "--file=") {
				patternGiven = true
			}
			out = append(out, tok)
			continue
		}
		positional++
		if (positional == 1 && !patternGiven) || !strings.ContainsAny(tok, "*?") || strings.ContainsAny(tok, `'"$`+"`") {
			out = append(out, tok)
			continue
		}
		segs := strings.FieldsFunc(tok, func(r rune) bool { return r == '/' || r == '\\' })
		cut := 0
		for cut < len(segs) && !strings.ContainsAny(segs[cut], "*?") {
			cut++
		}
		base := strings.Join(segs[:cut], "/")
		if base == "" {
			base = "."
		}
		if strings.HasPrefix(tok, "/") || strings.HasPrefix(tok, `\`) {
			base = "/" + base
		}
		globs = append(globs, "-g", "'"+strings.Join(segs[cut:], "/")+"'")
		if !bases[base] {
			bases[base] = true
			out = append(out, base)
		}
	}
	if len(globs) == 0 {
		return seg, false
	}
	lead := seg[:len(seg)-len(strings.TrimLeft(seg, " \t"))]
	trail := seg[len(strings.TrimRight(seg, " \t")):]
	return lead + strings.Join(append(append([]string{"rg"}, globs...), out[1:]...), " ") + trail, true
}

// quotedWords splits a simple command on unquoted whitespace and keeps each word's quotes.
func quotedWords(seg string) []string {
	var w []string
	var cur strings.Builder
	var q byte
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		switch {
		case q != 0:
			if c == q {
				q = 0
			}
		case c == '\'' || c == '"':
			q = c
		case isSpace(c):
			if cur.Len() > 0 {
				w = append(w, cur.String())
				cur.Reset()
			}
			continue
		}
		cur.WriteByte(c)
	}
	if cur.Len() > 0 {
		w = append(w, cur.String())
	}
	return w
}
