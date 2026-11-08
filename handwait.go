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
	fairshareAwait = regexp.MustCompile(`(?i)\bfairshare(?:\.exe)?['"]?\s+await\b`)
	// A script that rereads a file on a timer: node/bun/deno -e with setInterval or setTimeout, or
	// python -c with a while loop and time.sleep.
	scriptEval  = regexp.MustCompile(`(?i)\b(?:node|bun|deno)(?:\.exe)?\s+(?:-\S+\s+)*?(?:-e|--eval|-p)\s`)
	scriptTimer = regexp.MustCompile(`\b(?:setInterval|setTimeout)\s*\(`)
	scriptFile  = regexp.MustCompile(`\b(?:readFileSync|existsSync|statSync|readFile|watchFile|accessSync)\b`)
	pyEval      = regexp.MustCompile(`(?i)\bpython[\w.]*\s+(?:-\S+\s+)*?-c\s`)
	pyPoll      = regexp.MustCompile(`\bwhile\b[\s\S]*\btime\.sleep\s*\(`)
	// A log watched for a fixed time: `timeout 590 tail -f x.log`. One that stops at a line
	// (`| grep -m1 ready`) is a readiness wait and stays (waitKind).
	timedTail = regexp.MustCompile(`\btimeout\s+\d+[smh]?\s+tail\s+(?:[^|;&\s]+\s+)*?-[fF]\b`)
	// A loop starts at a command position.
	loopStart = regexp.MustCompile(`(?i)(?:^|[;&|\n{}(]\s*|\b(?:then|do|else)\s+)(while|until|for)\b`)
	// What makes a loop wait: a pause, or a clock it checks. `timeout 30 tail -f /dev/null` and
	// `read -t 15` are sleeps under another name.
	loopPause = regexp.MustCompile(`(?i)\b(?:sleep|Start-Sleep)\b|\btimeout\s+\S+\s+tail\s+-f\s+/dev/null|\bread\s+-t\b|\bping\s+-n\s+\d|\bSECONDS\b|\bdate\s+\+%s|\bGet-Date\b`)
	loopBreak = regexp.MustCompile(`(?i)\bbreak\b`)
	// A loop that probes a server waits for something that never sends a completion notice: it stays.
	netProbe = regexp.MustCompile(`(?i)\b(?:curl|wget|Invoke-WebRequest|iwr|Invoke-RestMethod|irm|Test-NetConnection)\b`)
)

// handWait says which hand-made wait cmd is, or "" for none.
func handWait(cmd, shell string) string {
	if fairshareAwait.MatchString(cmd) {
		return "`fairshare await` typed by hand"
	}
	if opaque(cmd) { // a here-doc's body is a script or a file being written, not shell
		return ""
	}
	if scriptEval.MatchString(cmd) && scriptTimer.MatchString(cmd) && scriptFile.MatchString(cmd) {
		return "a script that rereads a file on a timer"
	}
	if pyEval.MatchString(cmd) && pyPoll.MatchString(cmd) {
		return "a script that rereads a file on a timer"
	}
	u := unquoted(cmd)
	if timedTail.MatchString(u) && !grepStops.MatchString(u) && !strings.Contains(u, "/dev/null") {
		return "a log watched for a fixed time"
	}
	for _, m := range loopStart.FindAllStringSubmatchIndex(u, -1) {
		kw := strings.ToLower(u[m[2]:m[3]])
		body := loopBody(u[m[2]:], shell)
		if shell == "pwsh" && strings.HasSuffix(strings.TrimRight(u[:m[2]], " \t"), "}") {
			body = u // the `until (...)` of `do { ... } until (...)`: the pause is in the block before it
		}
		if !loopPause.MatchString(body) || netProbe.MatchString(body) {
			continue
		}
		// A `for` over a fixed list that pauses between items (sampling, retries) is work; one that
		// breaks out when something appears is a poll with a cap.
		if kw == "for" && !loopBreak.MatchString(body) {
			continue
		}
		// A capped poll that gives up within a few minutes (a dev server's log saying "ready") never
		// reaches the tool's timeout, so it is never run again.
		if kw == "for" && cappedSeconds(body) <= limits.ShortPollS {
			continue
		}
		return "a loop that pauses until something changes"
	}
	return ""
}

var (
	seqRange   = regexp.MustCompile(`^for\s+\w+\s+in\s+(?:\$\(\s*seq\s+(?:(\d+)\s+)?(\d+)\s*\)|\{(\d+)\.\.(\d+)\})\s*;?\s*do\b`)
	sleepSecs  = regexp.MustCompile(`(?i)\b(?:sleep|Start-Sleep)\s+(?:-s\s+)?(\d+(?:\.\d+)?)s?\b|\btimeout\s+(\d+)\s+tail\s+-f\s+/dev/null`)
	sleepWords = regexp.MustCompile(`(?i)\b(?:sleep|Start-Sleep)\b|\btimeout\s+\S+\s+tail\s+-f\s+/dev/null|\bread\s+-t\b`)
)

// cappedSeconds is the longest a `for i in $(seq 1 N)` / `{1..N}` loop with one fixed pause can wait,
// or a large number when that can't be read.
func cappedSeconds(body string) float64 {
	m := seqRange.FindStringSubmatch(body)
	pauses := sleepSecs.FindAllStringSubmatch(body, -1)
	if m == nil || len(pauses) != 1 || len(sleepWords.FindAllString(body, -1)) != 1 {
		return math.MaxFloat64
	}
	from, to := 1, 0
	if m[4] != "" {
		from, to = atoi(m[3]), atoi(m[4])
	} else {
		if m[1] != "" {
			from = atoi(m[1])
		}
		to = atoi(m[2])
	}
	secs, _ := strconv.ParseFloat(pauses[0][1]+pauses[0][2], 64)
	return float64(max(0, to-from+1)) * secs
}

// loopBody is a loop from its keyword to its matching `done` (bash), or to the end of the command
// (PowerShell, whose braces segments() already keeps together).
func loopBody(s, shell string) string {
	if shell == "pwsh" {
		return s
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

const handWaitReason = "Not run: this is a hand-made wait (%s). Waits like this run to their timeout while the job is still going and get run again; " +
	"they were the costliest repeated commands on this PC (about 150 minutes in two days). Run the job itself with the tool's own background " +
	"option (run_in_background: true) and wait for its completion notice: Claude Code tells you when it ends, and reading its output then costs " +
	"nothing. A job already in the background sends that notice by itself, so keep working or end the turn until it arrives (in T3 Code, " +
	"t3_background_wait waits for it); reading a running task's output file already waits up to 5 minutes. To wait on something that is not " +
	"your own job, such as a CI run or a file another chat writes, give the wait itself run_in_background: true."
