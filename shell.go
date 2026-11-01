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

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v' }

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
