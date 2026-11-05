package main

// `fairshare exec <bash|pwsh> <base64 command> <deadlineMs> [label]` runs a long test, build or
// install command for an agent. The full output goes to a log file; the agent gets a summary:
// everything when it is short, the last 20 lines on success, and on failure each error line with
// the two lines after it plus the last 30. It stops the command early, with the summary so far,
// when the agent's harness would kill it first (deadlineMs), when it reaches 9m45s (nothing an
// agent runs may take 10 minutes), or when it has printed nothing and used almost no CPU for 3
// minutes (it is waiting on something that will not come). Every run is one line in the event log.

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Noisemaker111/fairshare/internal/windows"
)

const (
	summaryShort = 80
	errorLines   = 40
	failTail     = 30
	okTail       = 20
)

var (
	runCeiling = 585 * time.Second
	hungAfter  = 3 * time.Minute
	hungCPU    = 0.02 // share of one core below which a silent command counts as waiting
)

var (
	ansi     = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)
	errLine  = regexp.MustCompile(`(?i)\b(error|errors|fail|failed|failure|failing|panic|exception|traceback|assert(ion)?)\b|✗|×|\(fail\)`)
	okLine   = regexp.MustCompile(`(?i)\b0 (fail|errors?)\b|\bno errors\b`)
	duration = regexp.MustCompile(`\d+(\.\d+)?m?s\b`)
	slugRe   = regexp.MustCompile(`[^\w]+`)
)

func bashPath() string {
	for _, p := range []string{os.Getenv("FAIRSHARE_BASH"), `C:\Program Files\Git\bin\bash.exe`, `C:\Program Files\Git\usr\bin\bash.exe`} {
		if p != "" && exists(p) {
			return p
		}
	}
	return "bash"
}

func logsDir() string { return filepath.Join(os.TempDir(), "fairshare", "logs") }

// pruneLogs deletes this program's own run logs older than a week, now and then.
func pruneLogs() {
	if rand.IntN(50) != 0 {
		return
	}
	entries, _ := os.ReadDir(logsDir())
	for _, e := range entries {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 7*24*time.Hour {
			os.Remove(filepath.Join(logsDir(), e.Name()))
		}
	}
}

// output collects a command's merged stdout and stderr, and notes when it last grew.
type output struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	file *os.File
	last time.Time
}

func (o *output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.buf.Write(p)
	if o.file != nil {
		o.file.Write(p)
	}
	o.last = time.Now()
	return len(p), nil
}

func (o *output) lastWrite() time.Time {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.last
}

func selfCPU() float64 {
	var c, e, k, u windows.Filetime
	if windows.GetProcessTimes(windows.CurrentProcess(), &c, &e, &k, &u) != nil {
		return 0
	}
	return float64(ftTicks(k)+ftTicks(u)) / 1e7
}

// summarize is what the agent sees of a run's output.
func runSummary(text string, exit int, stopNote, logPath string) string {
	text = ansi.ReplaceAllString(text, "")
	lines := regexp.MustCompile(`\r?\n`).Split(text, -1)
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	where := ""
	if logPath != "" {
		where = " · full log: " + logPath
	}
	var out []string
	if stopNote != "" {
		out = append(out, fmt.Sprintf("[fairshare] %s · %s lines so far%s", stopNote, thousands(len(lines)), where))
	}
	switch {
	case len(lines) <= summaryShort:
		out = append(out, lines...)
	case exit == 0:
		out = append(out, fmt.Sprintf("[fairshare] exit 0 · %s lines%s", thousands(len(lines)), where))
		out = append(out, lines[len(lines)-okTail:]...)
	default:
		keep, seen := map[int]bool{}, map[string]bool{}
		limit := len(lines) - failTail
		for i := 0; i < limit && len(keep) < errorLines; i++ {
			l := lines[i]
			if !errLine.MatchString(l) || okLine.MatchString(l) {
				continue
			}
			key := duration.ReplaceAllString(strings.TrimSpace(l), "")
			if seen[key] {
				continue
			}
			seen[key] = true
			for j := i; j <= i+2 && j < limit && len(keep) < errorLines; j++ {
				keep[j] = true
			}
		}
		if stopNote == "" {
			out = append(out, fmt.Sprintf("[fairshare] exit %d · %s lines%s", exit, thousands(len(lines)), where))
		}
		if len(keep) > 0 {
			idx := make([]int, 0, len(keep))
			for i := range keep {
				idx = append(idx, i)
			}
			slices.Sort(idx)
			out = append(out, "--- error lines with context ---")
			for _, i := range idx {
				out = append(out, fmt.Sprintf("%d: %s", i+1, lines[i]))
			}
		}
		out = append(out, fmt.Sprintf("--- last %d lines ---", failTail))
		out = append(out, lines[len(lines)-failTail:]...)
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, "\n") + "\n"
}

func cmdExec(args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: fairshare exec <bash|pwsh> <base64 command> [deadlineMs] [label]")
		return 2
	}
	shell := args[0]
	raw, err := base64.StdEncoding.DecodeString(args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "fairshare exec: bad command:", err)
		return 2
	}
	cmdText := string(raw)
	deadline := time.Duration(0)
	if len(args) > 2 {
		ms, _ := strconv.Atoi(args[2])
		deadline = time.Duration(ms) * time.Millisecond
	}
	label := ""
	if len(args) > 3 {
		label = args[3]
	}
	signal.Ignore(os.Interrupt) // the command gets Ctrl+C itself; we wait for it and pass on its code
	pruneLogs()
	if d, err := time.ParseDuration(os.Getenv("FAIRSHARE_HUNG_AFTER")); err == nil && d > 0 {
		hungAfter = d // for checks of the hang stop itself
	}

	// Everything the command starts is in a job Windows kills when this process ends, or in a
	// side job (see jobset.go) Windows kills the same way.
	job, jobErr := killOnExitJob()
	jobs := &jobSet{main: job, newSide: func() (windows.Handle, func(), error) {
		j, err := killOnCloseJob()
		return j, func() { windows.CloseHandle(j) }, err
	}}

	var exe string
	var argv []string
	if shell == "pwsh" {
		// -NoProfile: the agent's shell already passed its environment (PATH included) down to us.
		exe, argv = "pwsh", []string{"-NoProfile", "-NonInteractive", "-Command", cmdText}
	} else {
		exe, argv = bashPath(), []string{"-c", cmdText}
	}

	out := &output{}
	logPath := ""
	if os.MkdirAll(logsDir(), 0o755) == nil {
		slug := strings.Trim(clip(slugRe.ReplaceAllString(cmdText, "-"), 40), "-")
		logPath = filepath.Join(logsDir(), time.Now().UTC().Format("2006-01-02T15-04-05.000Z")+"-"+slug+".log")
		if f, err := os.Create(logPath); err == nil {
			out.file = f
		} else {
			logPath = ""
		}
	}

	limit := runCeiling
	if deadline > 0 && deadline < limit {
		limit = deadline
	}
	// A whole suite or check waits for a heavy-job slot (slot.go), for at most half its time.
	var slot *heavySlot
	if heavyRun(cmdText) {
		cwd, _ := os.Getwd()
		s, err := acquireSlot(cmdText, cwd, label, limit/2, func(m string) { out.Write([]byte(m + "\n")) })
		if err != nil {
			os.Stdout.WriteString(fmt.Sprintf("[fairshare] not started: %s within %s; the PC already runs %d whole suites or checks (%s). Run it with run_in_background, where it waits its turn, or narrow it to one test file or package.\n",
				errBusy, (limit / 2).Round(time.Second), slotCount(), describeHolders(slotHolders())))
			logEvent("run", map[string]any{"label": label, "shell": shell, "cmd": clip(cmdText, 300), "exit": 124, "stopped": "no-slot", "waited_ms": (limit / 2).Milliseconds()})
			return 124
		}
		defer s.Close()
		limit -= s.Waited
		slot = s
	}

	pr, pw, err := os.Pipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fairshare exec:", err)
		return 127
	}
	cmd := exec.Command(exe, argv...)
	if slot != nil {
		cmd.Env = append(os.Environ(), heldEnv+"=1")
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, pw, pw // no stdin: an agent command has no one to answer it
	start := time.Now()
	cpu0 := selfCPU()
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "fairshare exec could not start %s: %v\n", exe, err)
		return 127
	}
	pw.Close()
	out.last = start
	readDone := make(chan struct{})
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, err := pr.Read(buf)
			if n > 0 {
				out.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
		close(readDone)
	}()
	exited := make(chan int, 1)
	go func() {
		err := cmd.Wait()
		code := 0
		if err != nil {
			code = 1
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			}
		}
		exited <- code
	}()

	// Every 5 s: what the command's threads are doing, its CPU (GetProcessTimes per process; one
	// that exited keeps what it had at the last look) and its I/O, network included.
	seen, seenTotal, self := map[uint32]float64{}, 0.0, windows.GetCurrentProcessId()
	var pulledMu sync.Mutex
	pulled, sided, tried, outside, refusal := 0, 0, map[uint32]bool{}, []uint32(nil), ""
	sweep := func() {
		if jobErr == nil {
			pulledMu.Lock()
			defer pulledMu.Unlock()
			p, sd, out, why := pullIntoJobs(jobs, self, tried)
			pulled, sided, outside = pulled+p, sided+sd, out
			if why != nil {
				refusal = why.Error()
			}
		}
	}
	// Processes that start outside the job are pulled in soon after they appear.
	go func() {
		for _, d := range []time.Duration{100, 300, 600, 1000, 2000, 3500} {
			time.Sleep(d * time.Millisecond)
			sweep()
		}
	}()
	look := func() (state string, cpu, io float64) {
		if jobErr != nil {
			return "unknown", 0, 0
		}
		sweep()
		pulledMu.Lock()
		pids := slices.DeleteFunc(jobs.pids(), func(p uint32) bool { return p == self })
		pids = append(pids, outside...)
		ioMB := jobs.stats().AllIOMB
		pulledMu.Unlock()
		set := map[uint32]bool{}
		for _, p := range pids {
			set[p] = true
		}
		for pid, c := range pidsCPU(pids) {
			if d := c - seen[pid]; d > 0 {
				seenTotal += d
			}
			seen[pid] = c
		}
		st, ok := threadStates(set)
		switch {
		case !ok:
			state = "unknown"
		case st.Running > 0:
			state = "running"
		case st.Ready > 0:
			state = "starved" // wants a CPU the PC is giving to others
		case st.Suspended > 0:
			state = "frozen"
		default:
			state = "waiting"
		}
		return state, seenTotal, ioMB
	}
	const every = 5 * time.Second
	ticks := map[string]int{} // seconds = ticks * 5: where the run's time went
	var waitingSince time.Time
	_, lastCPU, lastIO := look()
	tick := time.NewTicker(every)
	defer tick.Stop()
	timer := time.NewTimer(limit)
	code, stopNote, stopped := 0, "", ""
wait:
	for {
		select {
		case code = <-exited:
			break wait
		case <-timer.C:
			if deadline > 0 && deadline < runCeiling {
				stopped, stopNote = "deadline", fmt.Sprintf("stopped after %ds, before the agent's time limit", int(limit.Round(time.Second).Seconds()))
			} else {
				stopped, stopNote = "ceiling", fmt.Sprintf("stopped after %s: commands stop before 10 minutes. Narrow it (one package or test file), or run it in the background with its output in a log", limit.Round(time.Second))
			}
			break wait
		case now := <-tick.C:
			state, cpu, io := look()
			// blocked threads that still use CPU or move data are working quietly
			if state == "waiting" && (cpu-lastCPU > hungCPU*every.Seconds() || io-lastIO > 0.0625) {
				state = "quiet"
			}
			ticks[state]++
			lastCPU, lastIO = cpu, io
			if state != "waiting" {
				waitingSince = time.Time{}
				continue
			}
			if waitingSince.IsZero() {
				waitingSince = now
			}
			// Hung: every thread blocked, no CPU, no I/O and no output for hungAfter. A run that is
			// only starved of CPU, or frozen by fairshare, is never hung.
			if since := now.Sub(maxTime(waitingSince, out.lastWrite())); since >= hungAfter {
				stopped = "hung"
				stopNote = fmt.Sprintf("stopped after %s: for %s it printed nothing and every thread was blocked, with no CPU or I/O, so it was waiting on something that was not coming (a prompt, a lock, a server, a dead connection)", now.Sub(start).Round(time.Second), since.Round(time.Second))
				break wait
			}
		}
	}
	if stopped != "" {
		code = 124
		pulledMu.Lock()
		killTree(jobs, jobErr == nil)
		pulledMu.Unlock()
		select {
		case <-exited:
		case <-time.After(2 * time.Second):
		}
	}
	// A grandchild that outlived the command can hold the pipe open; don't wait on it for long.
	select {
	case <-readDone:
	case <-time.After(2 * time.Second):
	}
	wall := time.Since(start)
	out.mu.Lock()
	text := out.buf.String()
	if out.file != nil {
		out.file.Close()
	}
	out.mu.Unlock()
	summary := runSummary(text, code, stopNote, logPath)
	os.Stdout.WriteString(summary)

	ev := map[string]any{
		"label": label, "shell": shell, "cmd": clip(cmdText, 300), "exit": code, "wall_ms": wall.Milliseconds(),
		"out_bytes": len(text), "out_lines": strings.Count(text, "\n"), "summary_bytes": len(summary), "log": logPath,
	}
	if cwd, err := os.Getwd(); err == nil {
		ev["cwd"] = cwd
	}
	if stopped != "" {
		ev["stopped"] = stopped
	}
	if slot != nil && slot.Waited > 0 {
		ev["waited_ms"] = slot.Waited.Milliseconds()
	}
	for state, n := range ticks {
		ev[state+"_s"] = n * int(every.Seconds())
	}
	pulledMu.Lock()
	defer pulledMu.Unlock()
	if pulled > 0 {
		ev["pulled_in"] = pulled
	}
	if sided > 0 {
		ev["side_jobs"] = sided
	}
	if len(tried) > 0 {
		ev["outside_job"], ev["outside_why"] = len(tried), refusal
	}
	if jobErr == nil {
		s := jobs.stats()
		ev["cpu_ms"] = int64(max(s.CPUSeconds-(selfCPU()-cpu0), seenTotal) * 1000)
		ev["procs"], ev["peak_mb"], ev["read_mb"], ev["write_mb"] = s.Processes-1, round1(s.PeakMB), round1(s.ReadMB), round1(s.WriteMB)
	} else {
		ev["job_error"] = jobErr.Error()
	}
	logEvent("run", ev)
	return code
}

// killTree ends every process the run started: everything in its jobs but the runner, and
// anything below the runner that escaped them.
func killTree(jobs *jobSet, haveJob bool) {
	self := windows.GetCurrentProcessId()
	var pids []uint32
	if haveJob {
		pids = jobs.pids()
	}
	pids = append(pids, descendants(snapshot(), self)...)
	for _, pid := range pids {
		if pid == self {
			continue
		}
		if h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, pid); err == nil {
			windows.TerminateProcess(h, 124)
			windows.CloseHandle(h)
		}
	}
}

func round1(f float64) float64 { return float64(int64(f*10+0.5)) / 10 }

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
