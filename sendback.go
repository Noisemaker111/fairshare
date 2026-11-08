package main

// Send-backs: calls answered at once with the reason instead of being run. Each one costs the agent
// a turn, so a rule belongs here only when running the call wastes far more than a turn.
//
// unsafe-rm (Claude): Claude Code's built-in safety check stops a recursive delete of a system, home
// or workspace folder, or of a target that becomes `rm -rf /` when its variables are empty, and only
// a person may approve it, even with permissions bypassed. The chat then sits idle until Jon
// answers: `rm -rf "$S/$p"` waited 200 minutes on 2026-10-28 and was declined. That prompt skips
// PermissionRequest hooks, so the only place to catch it is before the call.
//
// hand-wait (Claude): a foreground command that only polls for another job to finish (handwait.go).

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	keywordLead = regexp.MustCompile(`^\s*(?:(?:do|then|else|\{|\()\s+)+`)
	rmRecursive = regexp.MustCompile(`^-[a-zA-Z]*[rR][a-zA-Z]*$|^--recursive$`)
	psRecurse   = regexp.MustCompile(`(?i)^-r(e(c(u(r(s(e)?)?)?)?)?)?$`)
	psPathFlag  = regexp.MustCompile(`(?i)^-(Path|LiteralPath|PSPath|LP)$`)
	psValueFlag = regexp.MustCompile(`(?i)^-(Filter|Include|Exclude|Credential|Stream)$`)
	psRemove    = set("remove-item", "rm", "ri", "del", "rmdir", "rd", "erase")
	varRef      = regexp.MustCompile(`^\$(?:\{[^}]*\}|\([^)]*\)|(?:env:)?[A-Za-z_]\w*|[0-9@*#?$!-])`)
	rootLike    = regexp.MustCompile(`(?i)^(/+|~|[a-z]:|/[a-z])$`) // a literal target: the root, home, a drive (C: or Git Bash's /c)
	emptyRoot   = regexp.MustCompile(`(?i)^(~|[a-z]:)?$`)             // what is left of a target with its variables empty, after trailing / \ *
)

// rawWords splits a simple command into words with quotes removed; variable references are kept
// as written ($x, ${x}, $(cmd), `cmd`), so a caller can see what is not literal.
func rawWords(seg string) []string {
	var w []string
	var cur strings.Builder
	var q byte
	any := false
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		switch {
		case q == '\'':
			if c == '\'' {
				q = 0
			} else {
				cur.WriteByte(c)
			}
		case q == '"':
			if c == '"' {
				q = 0
			} else {
				cur.WriteByte(c)
			}
		case c == '\'' || c == '"':
			q = c
			any = true
		case isSpace(c):
			if cur.Len() > 0 || any {
				w = append(w, cur.String())
			}
			cur.Reset()
			any = false
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 || any {
		w = append(w, cur.String())
	}
	return w
}

// withoutVars is a word with every variable and command substitution taken out: what it becomes
// when they are all empty.
func withoutVars(word string) (string, bool) {
	var b strings.Builder
	had := false
	for i := 0; i < len(word); i++ {
		if word[i] == '`' {
			had = true
			if j := strings.IndexByte(word[i+1:], '`'); j >= 0 {
				i += j + 1
				continue
			}
			break
		}
		if word[i] == '$' {
			if m := varRef.FindString(word[i:]); m != "" {
				had = true
				i += len(m) - 1
				continue
			}
		}
		b.WriteByte(word[i])
	}
	return b.String(), had
}

// rmTargets lists what a segment deletes recursively.
func rmTargets(seg, shell string) []string {
	w := rawWords(keywordLead.ReplaceAllString(pipes(seg)[0], ""))
	if len(w) > 0 && w[0] == "sudo" {
		w = w[1:]
	}
	if len(w) < 2 {
		return nil
	}
	verb := strings.ToLower(w[0])
	var targets []string
	recursive := false
	if shell == "bash" && verb == "rm" {
		for _, a := range w[1:] {
			switch {
			case rmRecursive.MatchString(a):
				recursive = true
			case strings.HasPrefix(a, "-"):
			default:
				targets = append(targets, a)
			}
		}
	} else if shell == "pwsh" && psRemove[verb] {
		for i := 1; i < len(w); i++ {
			a := w[i]
			switch {
			case psRecurse.MatchString(a):
				recursive = true
			case psPathFlag.MatchString(a) && i+1 < len(w):
				i++
				targets = append(targets, strings.Split(w[i], ",")...)
			case psValueFlag.MatchString(a):
				i++
			case strings.HasPrefix(a, "-"):
			default:
				targets = append(targets, strings.Split(a, ",")...)
			}
		}
	}
	if !recursive {
		return nil
	}
	return targets
}

// protectedDirs are folders whose removal Claude Code always asks a person about.
func protectedDirs(cwd string) []string {
	dirs := []string{homeDir(), os.Getenv("SystemRoot"), os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("ProgramData"), filepath.Dir(homeDir())}
	if cwd != "" {
		dirs = append(dirs, cwd)
		if root := gitRoot(cwd); root != "" {
			dirs = append(dirs, root)
		}
	}
	return dirs
}

func samePath(a, b string) bool {
	return a != "" && b != "" && strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// unsafeRm finds a recursive delete Claude Code's safety check would hold for a person.
func unsafeRm(c Call, cmd string) (target, why string, ok bool) {
	for _, s := range segments(cmd) {
		for _, t := range rmTargets(s.Text, c.Shell) {
			bare := strings.TrimRight(t, `/\*`)
			if empty, had := withoutVars(t); had {
				// rm -rf "" deletes nothing; "/", "~/" or "C:\" with anything after them is the whole drive or home
				if e := strings.TrimRight(empty, `/\*`); emptyRoot.MatchString(e) && (e != "" || strings.ContainsAny(empty, `/\`)) {
					return t, fmt.Sprintf("with its variables empty it is `rm -rf %s`", or(empty, `""`)), true
				}
				continue
			}
			if bare == "" || rootLike.MatchString(bare) {
				return t, "it is a whole drive or the home folder", true
			}
			abs := resolvePath(c.Cwd, bare)
			for _, d := range protectedDirs(c.Cwd) {
				if samePath(abs, d) {
					return t, "it is a home, system or workspace folder itself", true
				}
			}
		}
	}
	return "", "", false
}

func sendBack(c Call, cmd string) (Decision, bool) {
	if c.Agent != "claude" {
		return Decision{}, false
	}
	if target, why, ok := unsafeRm(c, cmd); ok {
		return Decision{Kind: "deny", Rules: []string{"unsafe-rm"}, Reason: fmt.Sprintf(
			"Not run: Claude Code's safety check would hold this delete for Jon's approval, and the chat would wait "+
				"until he answers (one waited 200 minutes). The target %s is unsafe: %s. Write each path out in full, "+
				"or start it with a fixed folder (`rm -rf \"/c/work/$name\"`); to empty a folder, delete what is inside it by name.",
			target, why)}, true
	}
	if !truthy(c.Input["run_in_background"]) {
		if what := handWait(cmd, c.Shell); what != "" {
			return Decision{Kind: "deny", Rules: []string{"hand-wait"}, Reason: fmt.Sprintf(handWaitReason, what)}, true
		}
	}
	return Decision{}, false
}
