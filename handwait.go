package main

// hand-wait (Claude): a foreground command that only waits for something else to finish, polling a
// file, a log or a job until it changes. Claude Code refuses a bare `sleep N`, so agents build the
// same wait by hand: `end=$((SECONDS+570)); while ...; do ...; timeout 30 tail -f /dev/null; done`,
// a `node -e` script that rereads a log on setInterval, or `fairshare await` on a task's output.
// Each one runs to its timeout while the job is still going and is run again: in the 48 hours to
// 2026-11-08 these were the costliest repeated commands on the PC, about 150 minutes. The tool's own
// background option plus its completion notice does the same wait for free.

import (
	"math"
	"regexp"
	"strconv"
	"strings"
)

var (
	// fairshare's own wait, which the early-read rewrite puts in front of a read; typed by hand it is a poll.
	fairshareAwait = regexp.MustCompile(`(?im)(?:^|[;&|(]\s*)['"]?[^\s;&|'"]*fairshare(?:\.exe)?['"]?\s+await\b`)
	// A script that rereads a file on a timer: node/bun/deno -e with setInterval or setTimeout, or
	// python -c with a while loop and time.sleep.
	scriptEval  = regexp.MustCompile(`(?i)\b(?:node|bun|deno)(?:\.exe)?\s+(?:-\S+\s+)*?(?:-e|--eval|-p)\s`)
	scriptTimer = regexp.MustCompile(`\b(?:setInterval|setTimeout)\s*\(`)
	scriptFile  = regexp.MustCompile(`\b(?:readFileSync|existsSync|statSync|readFile|watchFile|accessSync)\b`)
	scriptWrite = regexp.MustCompile(`\b(?:writeFileSync|writeFile|appendFileSync)\b`) // an edit script, whatever its text says
	pyEval      = regexp.MustCompile(`(?i)\bpython[\w.]*\s+(?:-\S+\s+)*?-c\s`)
	pyPoll      = regexp.MustCompile(`\bwhile\b[\s\S]*\btime\.sleep\s*\(`)
	// A log watched for a fixed time: `timeout 590 tail -f x.log`. One that stops at a line
	// (`| grep -m1 ready`) is a readiness wait and stays (waitKind).
	timedTail = regexp.MustCompile(`\btimeout\s+\d+[smh]?\s+tail\s+(?:[^|;&\s]+\s+)*?-[fF]\b`)
	// A loop starts at a command position.
	loopStart = regexp.MustCompile(`(?i)(?:^|[;&|\n{}(]\s*|\b(?:then|do|else)\s+)(while|until|for)\b`)
	// What makes a bash loop wait: a pause, or a clock it checks. `timeout 30 tail -f /dev/null` and
	// `read -t 15` are sleeps under another name.
	bashPause = regexp.MustCompile(`\bsleep\b|\btimeout\s+\S+\s+tail\s+-f\s+/dev/null|\bread\s+-t\b|\bSECONDS\b|\bdate\s+\+%s`)
	// A PowerShell loop waits when it sleeps whole seconds; a few milliseconds between UI steps is work.
	pwshPause = regexp.MustCompile(`(?i)\b(?:Start-Sleep|sleep)\s+(?:-s(?:econds)?\s+)?\d|\b(?:Start-Sleep|sleep)\s+-m\w*\s+\d{4,}`)
	loopBreak = regexp.MustCompile(`(?i)\bbreak\b`)
	// A loop that probes a server waits for something that never sends a completion notice: it stays.
	netProbe = regexp.MustCompile(`(?i)\b(?:curl|wget|Invoke-WebRequest|iwr|Invoke-RestMethod|irm|Test-NetConnection)\b`)
	// A deadline the command sets itself: SECONDS+N, $(date +%s)+N, (Get-Date).AddSeconds(N) or
	// AddMinutes(N), or a counter checked with -lt N.
	deadlineSecs = regexp.MustCompile(`(?i)\bSECONDS\s*\+\s*(\d+)|date\s+\+%s\s*\)\s*\+\s*(\d+)|\.AddSeconds\(\s*(\d+)|\.AddMinutes\(\s*(\d+)|-lt\s+(\d+)\b`)
)

// handWait says which hand-made wait cmd is, or "" for none.
func handWait(cmd, shell string) string {
	if opaque(cmd) { // a here-doc's body is a script or a file being written, not shell
		return ""
	}
	if fairshareAwait.MatchString(cmd) {
		return "`fairshare await` typed by hand"
	}
	if scriptEval.MatchString(cmd) && scriptTimer.MatchString(cmd) && scriptFile.MatchString(cmd) && !scriptWrite.MatchString(cmd) {
		return "a script that rereads a file on a timer"
	}
	if pyEval.MatchString(cmd) && pyPoll.MatchString(cmd) {
		return "a script that rereads a file on a timer"
	}
	u := unquoted(cmd)
	if timedTail.MatchString(u) && !grepStops.MatchString(u) && !strings.Contains(u, "/dev/null") {
		return "a log watched for a fixed time"
	}
	pause := bashPause
	if shell == "pwsh" {
		pause = pwshPause
	}
	for _, m := range loopStart.FindAllStringSubmatchIndex(u, -1) {
		kw := strings.ToLower(u[m[2]:m[3]])
		body := loopBody(u, m[2], shell)
		if !pause.MatchString(body) || netProbe.MatchString(body) {
			continue
		}
		// A `for` over a fixed list that pauses between items (sampling, retries) is work; one that
		// breaks out when something appears is a poll with a cap.
		if kw == "for" && !loopBreak.MatchString(body) {
			continue
		}
		// A poll that gives up within a few minutes (a dev server's log saying "ready", a program
		// closing) never reaches the tool's timeout, so it is never run again.
		if min(cappedSeconds(body), deadline(u)) <= limits.ShortPollS {
			continue
		}
		return "a loop that pauses until something changes"
	}
	return ""
}

var (
	forRange   = regexp.MustCompile(`^for\s+\w+\s+in\s+(?:\$\(\s*seq\s+(?:(\d+)\s+)?(\d+)\s*\)|\{(\d+)\.\.(\d+)\}|((?:\d+\s+)*\d+))\s*;?\s*do\b`)
	sleepSecs  = regexp.MustCompile(`\bsleep\s+(\d+(?:\.\d+)?)s?\b|\btimeout\s+(\d+)\s+tail\s+-f\s+/dev/null`)
	sleepWords = regexp.MustCompile(`\bsleep\b|\btimeout\s+\S+\s+tail\s+-f\s+/dev/null|\bread\s+-t\b`)
)

// cappedSeconds is the longest a bash `for i in $(seq 1 N)` / `{1..N}` / `1 2 3` loop with one fixed
// pause can wait, or a large number when that can't be read.
func cappedSeconds(body string) float64 {
	m := forRange.FindStringSubmatch(body)
	pauses := sleepSecs.FindAllStringSubmatch(body, -1)
	if m == nil || len(pauses) != 1 || len(sleepWords.FindAllString(body, -1)) != 1 {
		return math.MaxFloat64
	}
	n := 0
	switch {
	case m[5] != "":
		n = len(strings.Fields(m[5]))
	case m[4] != "":
		n = atoi(m[4]) - atoi(m[3]) + 1
	case m[1] != "":
		n = atoi(m[2]) - atoi(m[1]) + 1
	default:
		n = atoi(m[2])
	}
	secs, _ := strconv.ParseFloat(pauses[0][1]+pauses[0][2], 64)
	return float64(max(0, n)) * secs
}

// deadline is the longest deadline the command sets itself, in seconds, or a large number for none.
func deadline(u string) float64 {
	best := -1.0
	for _, m := range deadlineSecs.FindAllStringSubmatch(u, -1) {
		s := float64(atoi(m[1] + m[2] + m[3] + m[5]))
		if m[4] != "" {
			s = float64(atoi(m[4]) * 60)
		}
		best = max(best, s)
	}
	if best < 0 {
		return math.MaxFloat64
	}
	return best
}

// loopBody is the loop whose keyword starts at u[at]: in bash up to its matching `done`; in PowerShell
// its condition and block, or for the `until`/`while` of `do { } until (...)`, the block before it.
func loopBody(u string, at int, shell string) string {
	s := u[at:]
	if shell == "pwsh" {
		if before := strings.TrimRight(u[:at], " \t\r\n"); strings.HasSuffix(before, "}") {
			open := matching(before, len(before)-1, -1)
			return before[open:] + " " + s[:balancedEnd(s)]
		}
		return s[:balancedEnd(s)]
	}
	depth := 0
	for i := 0; i < len(s); i++ {
		for _, w := range []string{"done", "do"} {
			if !strings.HasPrefix(s[i:], w) || (i > 0 && !strings.ContainsRune(" \t\r\n;&|(){}", rune(s[i-1]))) {
				continue
			}
			if j := i + len(w); j < len(s) && !strings.ContainsRune(" \t\r\n;&|)", rune(s[j])) {
				continue
			}
			if w == "do" {
				depth++
			} else if depth--; depth == 0 {
				return s[:i+len(w)]
			}
			i += len(w) - 1
			break
		}
	}
	return s
}

// balancedEnd is where a PowerShell loop statement at the start of s ends: past its `(...)` condition
// and its `{...}` block, whichever it has.
func balancedEnd(s string) int {
	i := strings.IndexAny(s, "({")
	if i < 0 {
		return len(s)
	}
	end := matching(s, i, 1)
	if s[i] == '(' {
		if j := strings.IndexAny(s[end+1:], "{;\n"); j >= 0 && s[end+1+j] == '{' {
			end = matching(s, end+1+j, 1)
		}
	}
	return min(len(s), end+1)
}

// matching finds the bracket that closes (dir 1) or opens (dir -1) the one at s[i], or the far end of s.
func matching(s string, i, dir int) int {
	depth := 0
	for ; i >= 0 && i < len(s); i += dir {
		switch s[i] {
		case '(', '{':
			depth += dir
		case ')', '}':
			depth -= dir
		}
		if depth == 0 {
			return i
		}
	}
	if dir > 0 {
		return len(s) - 1
	}
	return 0
}

const handWaitReason = "Not run: this is a hand-made wait (%s). Waits like this run to their timeout while the job is still going and get run again; " +
	"they were the costliest repeated commands on this PC (about 150 minutes in two days). Run the job itself with the tool's own background " +
	"option (run_in_background: true) and wait for its completion notice: Claude Code tells you when it ends, and reading its output then costs " +
	"nothing. A job already in the background sends that notice by itself, so keep working or end the turn until it arrives (in T3 Code, " +
	"t3_background_wait waits for it); reading a running task's output file already waits up to 5 minutes. To wait on something that is not " +
	"your own job, such as a CI run or a file another chat writes, give the wait itself run_in_background: true."
