package main

// Every rule for an agent's tool call, as one decision: pass it, rewrite it, or send it back.
// A rewrite never costs the agent a turn; a send-back costs one, so it is only for calls that
// would otherwise waste far more (a prompt nobody is there to answer). Thresholds come from the
// 2026-07..10 transcript audit (186k calls) and agent-guard's per-rule review.

import (
	"encoding/base64"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

var limits = struct {
	DumpLines, DumpBytes, Window int // a whole-file print above DumpLines or DumpBytes shows Window lines
	BoundedWaitS                 int // a wait that ends by itself within this many seconds stays in front
	ClaudeMaxMs                  int // Claude's longest foreground timeout
	CodexYieldMs                 int // minimum wait for an empty write_stdin / cell wait
	RgColumns                    int
	ScanSeconds                  int // a drive or home-folder find is cut off after this
}{800, 60000, 200, 590, 600000, 30000, 400, 45}

// Call is one tool call as the hook sees it.
type Call struct {
	Agent   string // claude, codex, opencode
	Tool    string
	Shell   string // bash or pwsh
	Cwd     string
	Session string
	Command *string
	Input   map[string]any
}

// Decision is what to do with a call.
type Decision struct {
	Kind    string // pass, rewrite, input, deny
	Rules   []string
	Command string
	Patch   map[string]any
	Input   map[string]any
	Reason  string
}

var pass = Decision{Kind: "pass"}

var (
	readersBash = set("cat", "head", "tail", "less", "more", "ls", "stat", "wc", "type")
	readersPwsh = set("get-content", "gc", "type", "cat", "get-childitem", "gci", "ls", "dir", "get-item", "gi", "select-string")
	valueFlags  = regexp.MustCompile(`(?i)^-(Pattern|Filter|Include|Exclude|Encoding|Delimiter|n|c|m)$`)
	// longRun is a test, build, check or install run: long enough to need a summary and a limit.
	longRun    = regexp.MustCompile(`^(?:(?:npm|pnpm|yarn|bun)\s+(?:i|install|ci)\b|(?:bunx?|npx|pnpm|npm|yarn|cargo|go|pytest|python\s+-m\s+pytest|uv|vitest|jest|tsc|tsgo|turbo|dotnet|gradle|mvn|make)\b[^|\n\r]*?\b(test|tests|check|check:\w+|check-types|typecheck|build|lint|install|ci|vitest|jest|tsc|tsgo|e2e|verify)\b)`)
	singleTest = regexp.MustCompile(`\.(test|spec)\.\w+|\s(test|tests|src)/\S+\.\w+`)
	// server is a command that runs until stopped (a dev server, a watcher): never a long run,
	// whatever its flags say (`bunx convex dev --typecheck disable` held a runner for 98 minutes).
	server = regexp.MustCompile(`^\S+(?:\s+\S+)?\s+(?:run\s+)?(?:dev|serve|start|preview|watch)\b|(?:^|\s)--watch\b`)
)

func set(xs ...string) map[string]bool {
	m := map[string]bool{}
	for _, x := range xs {
		m[x] = true
	}
	return m
}

func isLongRun(s string) bool {
	return longRun.MatchString(s) && !server.MatchString(s) && !discoveryRun.MatchString(s)
}

func homeDir() string {
	for _, k := range []string{"USERPROFILE", "HOME"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return "~"
}

var msysDrive = regexp.MustCompile(`(?i)^/([a-z])/`)

// resolvePath turns an agent's path argument into an absolute path: ~ is the home folder and
// /c/... is C:/... (Git Bash).
func resolvePath(cwd, p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		p = homeDir() + p[1:]
	}
	p = msysDrive.ReplaceAllString(p, "$1:/")
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	switch {
	case filepath.IsAbs(p):
		return filepath.Clean(p)
	case strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`):
		return filepath.Clean(filepath.VolumeName(cwd) + p)
	}
	return filepath.Join(cwd, p)
}

func posix(p string) string { return strings.ReplaceAll(p, `\`, "/") }

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func rel(cwd, p string) string {
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	r, err := filepath.Rel(cwd, p)
	if err != nil {
		return posix(p)
	}
	return posix(r)
}

func thousands(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// nearby lists the nearest real names for a missing path: same folder, sharing the most characters.
func nearby(abs string) []string {
	entries, err := os.ReadDir(filepath.Dir(abs))
	if err != nil {
		return nil
	}
	// NTFS lists names in upper-case order; keep it, so ties come out as File Explorer shows them.
	slices.SortFunc(entries, func(a, b os.DirEntry) int {
		return strings.Compare(strings.ToUpper(a.Name()), strings.ToUpper(b.Name()))
	})
	base := strings.ToLower(filepath.Base(abs))
	type scored struct {
		name  string
		score int
	}
	var all []scored
	for _, e := range entries {
		n, s := strings.ToLower(e.Name()), 0
		for _, part := range nonAlnum.Split(base, -1) {
			if part != "" && strings.Contains(n, part) {
				s += len(part)
			}
		}
		if s > 1 {
			all = append(all, scored{e.Name(), s})
		}
	}
	slices.SortStableFunc(all, func(a, b scored) int { return b.score - a.score })
	var out []string
	for i := 0; i < len(all) && i < 4; i++ {
		out = append(out, all[i].name)
	}
	return out
}

// gitRoot is the nearest folder at or above dir holding .git, or "" outside a project.
func gitRoot(dir string) string {
	if dir == "" {
		dir, _ = os.Getwd()
	}
	for d := dir; ; d = filepath.Dir(d) {
		if exists(filepath.Join(d, ".git")) {
			return d
		}
		if filepath.Dir(d) == d {
			return ""
		}
	}
}

var (
	foundMu sync.Mutex
	found   = map[string][]string{}
	skipDir = regexp.MustCompile(`^(node_modules|\.git|\.claude|\.worktrees|dist|build|target|\.next|\.cache|\.venv)$`)
)

// findByName lists files named name (any case) under the project root, skipping dependency and
// VCS folders. Bounded: 80 ms, 30,000 entries, 6 hits. Outside a project there is no bounded tree.
func findByName(cwd, name string) []string {
	root := gitRoot(cwd)
	if root == "" {
		return nil
	}
	key := root + "\x00" + strings.ToLower(name)
	foundMu.Lock()
	defer foundMu.Unlock()
	if hits, ok := found[key]; ok {
		return hits
	}
	want := strings.ToLower(name)
	var hits []string
	stack := []string{root}
	stop := time.Now().Add(80 * time.Millisecond)
	seen := 0
	for len(stack) > 0 && time.Now().Before(stop) && seen < 30000 && len(hits) < 6 {
		d := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		ents, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range ents {
			seen++
			if e.IsDir() {
				if !skipDir.MatchString(e.Name()) {
					stack = append(stack, filepath.Join(d, e.Name()))
				}
			} else if strings.ToLower(e.Name()) == want {
				hits = append(hits, filepath.Join(d, e.Name()))
			}
		}
	}
	found[key] = hits
	return hits
}

var (
	taskFileRe = regexp.MustCompile(`([A-Za-z]:[\\/][^\s"']*?[\\/]tasks[\\/]([\w-]+)\.output|/[^\s"']*?/tasks/([\w-]+)\.output)`)
	exitMarker = regexp.MustCompile(`exited with code|\[exit(ed)?\b`)
)

// taskFinished reports whether a Claude background task's output file ends with its exit
// marker. A file that can't be read counts as finished, so nothing waits on it.
func taskFinished(file string) bool {
	f, err := os.Open(file)
	if err != nil {
		return true
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return true
	}
	n := min(400, st.Size())
	buf := make([]byte, n)
	f.ReadAt(buf, st.Size()-n)
	return exitMarker.Match(buf)
}

// runningTaskFile finds a Claude background task's output file in text that has no exit marker yet.
func runningTaskFile(text string) string {
	m := taskFileRe.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	file := resolvePath("", m[1])
	if _, err := os.Stat(file); err != nil || taskFinished(file) {
		return ""
	}
	return file
}

// looksLikeCmd spots cmd.exe syntax reaching a hook that assumed PowerShell (Codex code mode can run either).
var looksLikeCmdRe = regexp.MustCompile(`(?i)(^|[\s&])cd\s+/d\s|%\w+%|\s2?>\s*nul\b|(^|&\s*)(findstr|type\s+\S+\s*&|dir\s+/[a-z])|\bset\s+\w+=|&\s*(echo|if|for)\b`)

var (
	tailFollow    = regexp.MustCompile(`\btail\s+(?:-n\s*\+?\d+\s+)?-[fF]\b`)
	timeoutTail   = regexp.MustCompile(`\btimeout\s+(\d+)\s+tail\b`)
	grepStops     = regexp.MustCompile(`\|\s*(grep|rg)\s+[^|]*-(m\s*\d+|q|-max-count|-quiet)\b`)
	watchFlag     = regexp.MustCompile(`(^|\s)--watch\b`)
	ghWatch       = regexp.MustCompile(`\bgh\s+(run\s+watch|pr\s+checks)\b`)
	sleepLoop     = regexp.MustCompile(`(?i)(^|[;&|\n{]\s*|\bthen\s+|\bdo\s+)(while|until|for)\b[\s\S]*?\b(sleep|Start-Sleep)\b`)
	forEachSleep  = regexp.MustCompile(`(?i)ForEach-Object[\s\S]*Start-Sleep`)
	waitsThatEnd  = regexp.MustCompile(`(?i)\bWait-Process\b|\bgh\s+(run\s+watch|pr\s+checks\b[^|;&]*--watch)`)
	alreadyWaits  = regexp.MustCompile(`(?i)\bWait-Process\b|\btail\s+--pid\b|\b(while|until)\b`)
	pwshNested    = regexp.MustCompile(`(?i)(^|[\s&;(|])((?:pwsh|powershell)(?:\.exe)?)`)
	pwshNestedArg = regexp.MustCompile(`(?i)^\s+-(?:Command|c|File|NoLogo|NonInteractive|ExecutionPolicy)\b`)
	npxRe         = regexp.MustCompile(`(^|[\s;&|(])npx(\s+)`)
	npxYes        = regexp.MustCompile(`^(-y|--yes)\b`)
	sshRe         = regexp.MustCompile(`(^|[\s;&|(])ssh(\s+)`)
	gitMerge      = regexp.MustCompile(`(?i)^\s*git\s+merge\s+`)
	gitMergeHead  = regexp.MustCompile(`(?i)^(\s*git\s+merge)`)
	mergeAnswered = regexp.MustCompile(`(?i)--no-edit|--abort|--continue|--quit|--no-commit|--squash|\s-m\s|--message`)
	driveFind     = regexp.MustCompile(`(?i)^find\s+(/|/c/?|~/?|/c/Users/[^/\s]+/?|[A-Za-z]:[\\/]?(Users[\\/][^\\/\s]+[\\/]?)?)(\s|$)`)
	maxDepth      = regexp.MustCompile(`-maxdepth\s+(\d+)`)
	shellState    = regexp.MustCompile("[$`>]|Out-File|Tee-Object|\\btee\\b")
	shellVars     = regexp.MustCompile("[$`]")
	windowFlags   = regexp.MustCompile(`(?i)^-(TotalCount|Head|Tail|First|Last|Wait|ReadCount)$`)
	pathFlags     = regexp.MustCompile(`(?i)^-(Path|LiteralPath|Raw)$`)
	encodingFlag  = regexp.MustCompile(`(?i)^-(Encoding)$`)
	pipeHead      = regexp.MustCompile(`^(\s*)head(?:\s+-n\s*|\s+-)?(\d+)?\s*$`)
	pipeTail      = regexp.MustCompile(`^(\s*)tail(?:\s+-n\s*|\s+-)?(\d+)?\s*$`)
	devNull2      = regexp.MustCompile(`(^|\s)2>\s*/dev/null\b`)
	devNullAll    = regexp.MustCompile(`(^|\s)&>\s*/dev/null\b`)
	devNull       = regexp.MustCompile(`(^|\s)>\s*/dev/null\b`)
	whichRe       = regexp.MustCompile(`^(\s*)which\s+([\w.-]+)\s*$`)
	winPath       = regexp.MustCompile(`(^|[\s=])([A-Za-z]:\\[^\s'";|&<>()]*)`)
	python3       = regexp.MustCompile(`(^|[\s;&|(])python3(\s)`)
	rgStart       = regexp.MustCompile(`^\s*rg\s`)
	rgUncapped    = regexp.MustCompile(`--(files|max-columns|count|files-with-matches|json)\b|\s-[a-zA-Z]*[lc][a-zA-Z]*\s`)
	personView    = regexp.MustCompile(`(?i)^\s*(head|tail|Select-Object|sort|uniq|wc|Measure-Object)\b`)
	rgHead        = regexp.MustCompile(`^(\s*)rg(\s)`)
	searchVerb    = regexp.MustCompile(`^(rg|grep)\s`)
	leadSpace     = regexp.MustCompile(`^\s*`)
	safeLabel     = regexp.MustCompile(`^[\w-]+$`)
)

// waitKind says what kind of wait a command is, judged on its unquoted text only. "forever" waits
// never end by themselves (a bare tail -f, --watch); other waits ("ends") usually do: 93% of
// waits finished inside their own timeout.
func waitKind(cmd string) string {
	u := unquoted(cmd)
	if tailFollow.MatchString(u) {
		if grepStops.MatchString(u) {
			return "ends"
		}
		if m := timeoutTail.FindStringSubmatch(u); m != nil {
			if n, _ := strconv.Atoi(m[1]); n <= limits.BoundedWaitS {
				return "ends"
			}
		}
		return "forever"
	}
	if watchFlag.MatchString(u) && !ghWatch.MatchString(u) {
		return "forever"
	}
	if sleepLoop.MatchString(u) && !forEachSleep.MatchString(u) {
		return "ends"
	}
	if waitsThatEnd.MatchString(u) {
		return "ends"
	}
	return ""
}

// truthy is JavaScript truthiness for JSON values, as the agents send them.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0 && !math.IsNaN(x)
	case string:
		return x != ""
	}
	return true
}

func num(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}

// replaceMatches rewrites every match of re in s with fn(submatches).
func replaceMatches(re *regexp.Regexp, s string, fn func(m []int) (string, bool)) string {
	var b strings.Builder
	last := 0
	for _, m := range re.FindAllStringSubmatchIndex(s, -1) {
		if r, ok := fn(m); ok {
			b.WriteString(s[last:m[0]])
			b.WriteString(r)
			last = m[1]
		}
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

func group(s string, m []int, i int) string {
	if m[2*i] < 0 {
		return ""
	}
	return s[m[2*i]:m[2*i+1]]
}

// untilSeparator is s up to its first |, ; or &.
func untilSeparator(s string) string {
	if i := strings.IndexAny(s, "|;&"); i >= 0 {
		return s[:i]
	}
	return s
}

// noPrompt makes commands that would wait for an answer fail fast or not ask: nothing in an
// agent's shell can answer a prompt.
func noPrompt(u string) string {
	u = replaceMatches(pwshNested, u, func(m []int) (string, bool) {
		rest := u[m[5]:]
		if !pwshNestedArg.MatchString(rest) || strings.Contains(strings.ToLower(untilSeparator(rest)), "-noprofile") {
			return "", false
		}
		return group(u, m, 1) + group(u, m, 2) + " -NoProfile", true
	})
	u = replaceMatches(npxRe, u, func(m []int) (string, bool) {
		ws := group(u, m, 2)
		if !npxYes.MatchString(u[m[1]:]) {
			return group(u, m, 1) + "npx -y ", true
		}
		if len(ws) == 1 {
			return "", false
		}
		// like the backtracking original: `npx  -y` matches with one space less
		return group(u, m, 1) + "npx -y " + ws[len(ws)-1:], true
	})
	u = replaceMatches(sshRe, u, func(m []int) (string, bool) {
		if strings.Contains(untilSeparator(u[m[1]:]), "BatchMode") {
			return "", false
		}
		return group(u, m, 1) + "ssh -o BatchMode=yes ", true
	})
	return u
}

func replaceFirst(re *regexp.Regexp, s, repl string) string {
	m := re.FindStringSubmatchIndex(s)
	if m == nil {
		return s
	}
	return s[:m[0]] + string(re.ExpandString(nil, repl, s, m)) + s[m[1]:]
}

// replaceToken replaces from with to where it stands as its own word.
func replaceToken(s, from, to string) string {
	var b strings.Builder
	last := 0
	for i := 0; ; {
		j := strings.Index(s[i:], from)
		if j < 0 {
			break
		}
		at := i + j
		end := at + len(from)
		before := at == 0 || strings.ContainsRune(" \t\n\r\f\v'\"", rune(s[at-1]))
		after := end == len(s) || strings.ContainsRune(" \t\n\r\f\v'\",", rune(s[end]))
		if before && after {
			b.WriteString(s[last:at])
			b.WriteString(to)
			last = end
			i = end
		} else {
			i = at + 1
		}
		if i >= len(s) {
			break
		}
	}
	b.WriteString(s[last:])
	return b.String()
}

func countLines(b []byte) int {
	n := 0
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	if len(b) > 0 && b[len(b)-1] != '\n' {
		n++
	}
	return n
}

type ruleSet struct{ list []string }

func (r *ruleSet) add(name string) {
	if !slices.Contains(r.list, name) {
		r.list = append(r.list, name)
	}
}

// runnerCommand is how a rewritten command calls this program back.
var runnerCommand = func(shell string) string {
	exe, err := os.Executable()
	if err != nil {
		exe = "fairshare"
	}
	q := quote(posix(exe), shell)
	if shell == "pwsh" {
		return "& " + q
	}
	return q
}

func decide(c Call) Decision {
	input := c.Input
	if input == nil {
		input = map[string]any{}
	}

	// Codex waits: one long wait instead of a loop of 1-second checks. It returns as soon as the command exits.
	if c.Agent == "codex" && (c.Tool == "write_stdin" || c.Tool == "wait") {
		empty := c.Tool == "wait" || !truthy(input["chars"])
		if empty && !truthy(input["terminate"]) && num(input["yield_time_ms"]) < float64(limits.CodexYieldMs) {
			in := maps(input)
			in["yield_time_ms"] = limits.CodexYieldMs
			return Decision{Kind: "input", Rules: []string{"wait-longer"}, Input: in}
		}
		return pass
	}

	if c.Command == nil || strings.TrimSpace(*c.Command) == "" {
		return pass
	}
	cmd := *c.Command
	if c.Shell == "pwsh" && looksLikeCmdRe.MatchString(cmd) {
		return pass
	}
	if d, ok := sendBack(c, cmd); ok {
		return d
	}
	patch := map[string]any{}
	rules := &ruleSet{}
	timeoutNow := 120000.0
	if truthy(input["timeout"]) {
		timeoutNow = num(input["timeout"])
	}

	// Reading a background task's output before it finished: wait for it in the same call, then
	// read it. A refusal would cost a turn and the agent reads again after the notification anyway.
	if c.Agent == "claude" {
		if file := runningTaskFile(cmd); file != "" && !alreadyWaits.MatchString(unquoted(cmd)) {
			pre := runnerCommand(c.Shell) + " await " + quote(posix(file), c.Shell) + " 300 20"
			return Decision{Kind: "rewrite", Rules: []string{"early-read"}, Command: pre + "; " + cmd,
				Patch: map[string]any{"timeout": int(max(timeoutNow, 330000))}}
		}
	}

	if opaque(cmd) {
		return pass
	}
	segs := segments(cmd)

	// Claude waits: one that ends by itself stays in front with room to finish (no extra turn); only
	// a wait that can never end moves to the background, where it costs one notification.
	if c.Agent == "claude" && !truthy(input["run_in_background"]) {
		switch waitKind(cmd) {
		case "forever":
			patch["run_in_background"] = true
			rules.add("background-wait")
		case "ends":
			if timeoutNow < float64(limits.ClaudeMaxMs) {
				patch["timeout"] = limits.ClaudeMaxMs
				rules.add("wait-timeout")
			}
		}
	}

	// A path that doesn't exist: one file with that exact parent/name in the project -> read it in
	// this call; otherwise run unchanged with the nearest names printed first.
	type fix struct {
		seg            int
		from, to, hint string
	}
	var fixes []fix
	readers := readersBash
	if c.Shell == "pwsh" {
		readers = readersPwsh
	}
	var reads []int
	for i, s := range segs {
		if strings.TrimSpace(s.Text) != "" {
			reads = append(reads, i)
		}
	}
	allRead := len(reads) > 0
	for _, i := range reads {
		w := words(strings.TrimSpace(pipes(segs[i].Text)[0]))
		if len(w) == 0 || !readers[strings.ToLower(w[0])] {
			allRead = false
			break
		}
	}
	if allRead {
		for _, si := range reads {
			w := words(strings.TrimSpace(pipes(segs[si].Text)[0]))
			selectString := strings.EqualFold(w[0], "select-string")
			positional := 0
			for i := 1; i < len(w); i++ {
				a := strings.TrimRight(w[i], ",")
				if valueFlags.MatchString(w[i]) {
					i++
					continue
				}
				if strings.HasPrefix(a, "-") {
					continue
				}
				if selectString {
					positional++
					if positional == 1 {
						continue
					}
				}
				if !strings.ContainsAny(a, `\/.`) || isDigits(a) {
					continue
				}
				abs := resolvePath(c.Cwd, a)
				if exists(abs) {
					continue
				}
				want := strings.ToLower(strings.TrimPrefix(posix(a), "./"))
				hits := findByName(c.Cwd, filepath.Base(abs))
				var exact []string
				for _, h := range hits {
					p := strings.ToLower(posix(h))
					if p == want || strings.HasSuffix(p, "/"+want) {
						exact = append(exact, h)
					}
				}
				if len(exact) == 1 {
					fixes = append(fixes, fix{seg: si, from: a, to: rel(c.Cwd, exact[0])})
					continue
				}
				var near []string
				if len(hits) > 0 {
					for _, h := range hits[:min(5, len(hits))] {
						near = append(near, rel(c.Cwd, h))
					}
				} else {
					near = nearby(abs)
				}
				if len(near) > 0 {
					fixes = append(fixes, fix{seg: si, hint: fmt.Sprintf("[fairshare: %s does not exist. Closest: %s]", a, strings.Join(near, ", "))})
				}
			}
		}
	}

	out := slices.Clone(segs)
	say := func(text string) string {
		if c.Shell == "pwsh" {
			return "Write-Output " + quote(text, c.Shell)
		}
		return "echo " + quote(text, c.Shell)
	}
	for _, f := range fixes {
		o := &out[f.seg]
		lead := leadSpace.FindString(o.Text)
		rest := o.Text[len(lead):]
		// The note goes first so the segment's exit code stays the command's own.
		if f.to != "" {
			o.Text = lead + say(fmt.Sprintf("[fairshare: %s does not exist; reading %s]", f.from, f.to)) + "; " + replaceToken(rest, f.from, f.to)
			rules.add("path-fixed")
		} else {
			o.Text = lead + say(f.hint) + "; " + rest
			rules.add("path-hint")
		}
	}

	for si := range out {
		s := &out[si]
		lead := leadSpace.FindString(s.Text)
		body := s.Text[len(lead):]
		if body == "" {
			continue
		}

		quiet := outsideQuotes(body, noPrompt)
		merged := quiet
		if m := gitMerge.FindStringIndex(quiet); m != nil && !mergeAnswered.MatchString(untilSeparator(quiet[m[1]:])) {
			merged = replaceFirst(gitMergeHead, quiet, "$1 --no-edit")
		}
		if merged != body {
			body = merged
			rules.add("no-prompt")
		}

		// A find over a whole drive or home folder: cut it off instead of letting it run to the time limit.
		if c.Shell == "bash" && driveFind.MatchString(body) {
			if m := maxDepth.FindStringSubmatch(body); m == nil || atoi(m[1]) > 3 {
				body = fmt.Sprintf("timeout %d %s", limits.ScanSeconds, body)
				rules.add("drive-scan")
			}
		}

		// Long test / build / install runs: full log to a file, summary to the agent, a hang stopped
		// early. Only unbounded runs (a command already piped into tail chose its own limit).
		// Variables and redirects belong to the agent's shell, which the runner's fresh shell would not see.
		harness := 0 // the agent's own time limit on this call, when it is known
		switch c.Agent {
		case "claude":
			harness = limits.ClaudeMaxMs
			if truthy(input["timeout"]) {
				harness = int(num(input["timeout"]))
			}
		case "opencode":
			harness = int(num(input["timeout"]))
		}
		if !truthy(input["run_in_background"]) && isLongRun(body) && len(pipes(body)) == 1 && !shellState.MatchString(unquoted(body)) && !shellVars.MatchString(body) {
			deadline := 0
			if harness > 0 {
				deadline = max(30000, harness-15000)
			}
			sh := "bash"
			if c.Shell == "pwsh" {
				sh = "pwsh"
			}
			body = fmt.Sprintf("%s exec %s %s %d", runnerCommand(c.Shell), sh, base64.StdEncoding.EncodeToString([]byte(body)), deadline)
			if safeLabel.MatchString(c.Session) {
				body += " " + c.Agent + ":" + clip(c.Session, 8)
			}
			rules.add("long-run")
			s.Text = lead + body
			continue
		}

		// A whole suite or check that stays in the agent's shell (background, redirected, piped):
		// it still waits for a heavy-job slot, with its output and exit code untouched (slot.go).
		// A leading VAR=value belongs to the command, so that one runs as it is.
		// In front, it waits at most half the agent's time limit, then says it did not start.
		if heavyRun(body) && !leadAssign.MatchString(body) && !ownSlot(body, c.Cwd) {
			wait := ""
			if !truthy(input["run_in_background"]) && harness > 0 {
				wait = fmt.Sprintf("--wait %d ", harness/2)
			}
			body = runnerCommand(c.Shell) + " slot " + wait + body
			rules.add("heavy-slot")
			s.Text = lead + body
			continue
		}

		// Whole-file prints of big files: show a window plus how to read the rest.
		if len(pipes(body)) == 1 {
			if text, ok := fileWindow(c, body); ok {
				s.Text = lead + text
				rules.add("file-window")
				continue
			}
		}

		if c.Shell == "pwsh" {
			// Dialect: bash habits sent to PowerShell.
			parts := pipes(body)
			for i, p := range parts {
				if i > 0 {
					p = replaceSub(pipeHead, p, func(g []string) string { return g[1] + "Select-Object -First " + or(g[2], "10") + " " })
					p = replaceSub(pipeTail, p, func(g []string) string { return g[1] + "Select-Object -Last " + or(g[2], "10") + " " })
				}
				p = outsideQuotes(p, func(u string) string {
					u = devNull2.ReplaceAllString(u, "${1}2>$$null")
					u = devNullAll.ReplaceAllString(u, "${1}*>$$null")
					return devNull.ReplaceAllString(u, "${1}>$$null")
				})
				p = replaceSub(whichRe, p, func(g []string) string {
					return g[1] + "(Get-Command " + g[2] + " -ErrorAction SilentlyContinue).Source "
				})
				parts[i] = p
			}
			if joined := strings.Join(parts, "|"); joined != body {
				body = joined
				rules.add("pwsh-dialect")
			}
			parts = pipes(body)
			globbed := false
			for i, p := range parts {
				if q, ok := rgShellGlobs(p); ok {
					parts[i], globbed = q, true
				}
			}
			if globbed {
				body = strings.Join(parts, "|")
				rules.add("rg-glob")
			}
		} else {
			// Unquoted Windows paths lose their backslashes in bash: C:\Users\x becomes C:Usersx.
			fixed := outsideQuotes(body, func(u string) string {
				return replaceMatches(winPath, u, func(m []int) (string, bool) {
					return group(u, m, 1) + strings.ReplaceAll(group(u, m, 2), `\`, "/"), true
				})
			})
			if fixed != body {
				body = fixed
				rules.add("bash-winpath")
			}
		}
		if py := outsideQuotes(body, func(u string) string { return python3.ReplaceAllString(u, "${1}python${2}") }); py != body {
			body = py
			rules.add("python3")
		}

		// Search output: cap pathological lines (minified bundles, lockfiles), but only where a
		// person-sized view is the end of the line. A program downstream must see whole lines.
		stages := pipes(body)
		capped := make([]string, len(stages))
		for i, p := range stages {
			capped[i] = p
			if !rgStart.MatchString(p) || rgUncapped.MatchString(p) {
				continue
			}
			if i+1 < len(stages) && stages[i+1] != "" && !personView.MatchString(stages[i+1]) {
				continue
			}
			capped[i] = outsideQuotes(p, func(u string) string {
				return replaceFirst(rgHead, u, fmt.Sprintf("${1}rg --max-columns=%d --max-columns-preview${2}", limits.RgColumns))
			})
		}
		if j := strings.Join(capped, "|"); j != body {
			body = j
			rules.add("rg-columns")
		}
		s.Text = lead + body
	}

	// Codex and OpenCode report a batch as failed when its last step is a search that found
	// nothing. Clear only that search's own exit 1 (never an earlier step's failure).
	if c.Agent != "claude" {
		var real []int
		for i, s := range out {
			if strings.TrimSpace(s.Text) != "" {
				real = append(real, i)
			}
		}
		if len(real) > 1 {
			li := real[len(real)-1]
			last := &out[li]
			lw := strings.TrimSpace(last.Text)
			chained := false
			for _, s := range out[:li] {
				if s.Sep == "&&" || s.Sep == "||" {
					chained = true
				}
			}
			if !chained && searchVerb.MatchString(lw) && len(pipes(lw)) == 1 {
				if c.Shell == "pwsh" {
					last.Text = strings.TrimRightFunc(last.Text, isSpaceRune) + "; if ($LASTEXITCODE -eq 1) { $global:LASTEXITCODE = 0 }"
				} else {
					lead := leadSpace.FindString(last.Text)
					last.Text = lead + "{ " + strings.TrimRightFunc(last.Text[len(lead):], isSpaceRune) + " || [ $? -eq 1 ]; }"
				}
				rules.add("search-exit")
			}
		}
	}

	// Claude kills a command at its 2-minute default and the agent re-runs it with a longer limit.
	// Check scripts, installs, builds and whole suites run past that often (p90 146 s).
	if c.Agent == "claude" && !truthy(input["timeout"]) && patch["run_in_background"] == nil && patch["timeout"] == nil {
		for _, s := range segs {
			if isLongRun(strings.TrimSpace(s.Text)) && !singleTest.MatchString(s.Text) {
				patch["timeout"] = limits.ClaudeMaxMs
				rules.add("long-timeout")
				break
			}
		}
	}

	if len(rules.list) == 0 {
		return pass
	}
	d := Decision{Kind: "rewrite", Rules: rules.list, Command: join(out)}
	if len(patch) > 0 {
		d.Patch = patch
	}
	return d
}

// fileWindow turns a whole-file print of a big file into its first lines plus directions.
func fileWindow(c Call, body string) (string, bool) {
	w := words(strings.TrimSpace(body))
	if len(w) == 0 {
		return "", false
	}
	verb := strings.ToLower(w[0])
	pwshRead := c.Shell == "pwsh" && (verb == "get-content" || verb == "gc" || verb == "type" || verb == "cat")
	bashRead := c.Shell == "bash" && verb == "cat"
	if !pwshRead && !bashRead || slices.ContainsFunc(w, windowFlags.MatchString) {
		return "", false
	}
	var files []string
	for i := 1; i < len(w); i++ {
		a := w[i]
		if pathFlags.MatchString(a) {
			continue
		}
		if encodingFlag.MatchString(a) {
			i++
			continue
		}
		if strings.HasPrefix(a, "-") {
			return "", false
		}
		files = append(files, a)
	}
	if len(files) == 0 {
		return "", false
	}
	var pieces []string
	windowed := false
	for _, f := range files {
		abs := resolvePath(c.Cwd, f)
		st, err := os.Stat(abs)
		if err != nil || !st.Mode().IsRegular() {
			return "", false
		}
		size := int(st.Size())
		lines := 0
		if size > limits.DumpBytes || size > limits.DumpLines*20 {
			if b, err := os.ReadFile(abs); err == nil {
				lines = countLines(b)
			}
		}
		q := quote(f, c.Shell)
		if lines > limits.DumpLines || size > limits.DumpBytes {
			windowed = true
			n := limits.Window
			how := fmt.Sprintf("sed -n '%d,%dp' %s", n+1, 2*n, q)
			if c.Shell == "pwsh" {
				how = fmt.Sprintf("Get-Content -LiteralPath %s | Select-Object -Skip %d -First %d", q, n, n)
			}
			note := fmt.Sprintf("[fairshare: %s has %s lines (%s KB); showed lines 1-%d. Read a range: %s. Find something: rg -n '<pattern>' %s]",
				f, thousands(lines), thousands(int(math.Floor(float64(size)/1024+0.5))), n, how, q)
			if c.Shell == "pwsh" {
				pieces = append(pieces, fmt.Sprintf("Get-Content -LiteralPath %s -TotalCount %d; Write-Output %s", q, n, quote(note, c.Shell)))
			} else {
				pieces = append(pieces, fmt.Sprintf("head -n %d %s; echo %s", n, q, quote(note, c.Shell)))
			}
		} else if c.Shell == "pwsh" {
			pieces = append(pieces, "Get-Content -LiteralPath "+q)
		} else {
			pieces = append(pieces, "cat "+q)
		}
	}
	if !windowed {
		return "", false
	}
	return strings.Join(pieces, "; "), true
}

func replaceSub(re *regexp.Regexp, s string, fn func(g []string) string) string {
	m := re.FindStringSubmatch(s)
	if m == nil {
		return s
	}
	loc := re.FindStringIndex(s)
	return s[:loc[0]] + fn(m) + s[loc[1]:]
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

func isSpaceRune(r rune) bool { return r < 128 && isSpace(byte(r)) }

func maps(m map[string]any) map[string]any {
	out := make(map[string]any, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	return out
}
