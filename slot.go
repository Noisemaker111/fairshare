package main

// Heavy-job slots: at most a few whole test suites, builds or checks run on this PC at once,
// across every chat and repo. Without them, six chats each start a full suite, the CPU sits at
// 100%, and tests fail on their own time limits instead of on bugs (2026-11-03: every session was
// cut off at 02:01 that way, after an evening of agents rerunning "flaky" tests). Idle priority
// alone does not help: heavy jobs at idle priority still share the CPU with each other.
//
// A slot is a lock file held open for writing. Windows closes it when the holder exits, however
// it exits, so a crashed or killed run never keeps a slot. The file holds who has it, so a run that
// waits can say what it waits for. A single test file or package never waits (see heavyRun).

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Noisemaker111/fairshare/internal/windows"
)

// heldEnv marks work that already holds a slot, so what it starts never waits for a second one.
const heldEnv = "FAIRSHARE_HEAVY_HELD"

var (
	installRun = regexp.MustCompile(`^(?:npm|pnpm|yarn|bun)\s+(?:i|install|ci)\b`)
	goRun      = regexp.MustCompile(`^go\s`)
	goTargeted = regexp.MustCompile(`\s-run[\s=]`)
	// heavyExtra: whole checks whose names say nothing longRun knows.
	heavyExtra = regexp.MustCompile(`^(?:bun|npm|pnpm)\s+run\s+(?:lane:close|smoke\S*|perf\S*|bench\S*)(?:\s|$)|^node\s+\S*check\.mjs\b`)
	leadAssign = regexp.MustCompile(`^\w+=`)
)

// heavyRun is a command that loads the whole PC: a whole suite, build or check. One test file,
// one Go package or one -run filter is light, and installs are short; those never wait.
func heavyRun(s string) bool {
	s = strings.TrimSpace(s)
	if server.MatchString(s) || installRun.MatchString(s) || singleTest.MatchString(s) {
		return false
	}
	if goRun.MatchString(s) && (!strings.Contains(s, "./...") || goTargeted.MatchString(s)) {
		return false
	}
	return longRun.MatchString(s) || heavyExtra.MatchString(s)
}

func slotsDir() string { return filepath.Join(eventsDir(), "slots") }

func slotCount() int {
	if n, err := strconv.Atoi(os.Getenv("FAIRSHARE_HEAVY_SLOTS")); err == nil && n > 0 {
		return n
	}
	return 2 // 24 threads: two whole suites keep the PC busy without either starving
}

type slotHolder struct {
	PID   int    `json:"pid"`
	Cmd   string `json:"cmd"`
	Cwd   string `json:"cwd"`
	Label string `json:"label,omitempty"`
	Since string `json:"since"`
}

// heavySlot is a held slot; release it with Close (exiting does the same).
type heavySlot struct {
	f      *os.File
	Waited time.Duration
}

func (s *heavySlot) Close() {
	if s != nil && s.f != nil {
		s.f.Close()
		s.f = nil
	}
}

var errBusy = errors.New("no heavy-job slot came free")

// trySlot takes slot i if nobody holds it. Others may still read it to see who does.
func trySlot(i int) *os.File {
	path := filepath.Join(slotsDir(), fmt.Sprintf("heavy-%d.lock", i))
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil
	}
	h, err := windows.CreateFile(p, windows.GENERIC_WRITE, windows.FILE_SHARE_READ, nil, windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil
	}
	return os.NewFile(uintptr(h), path)
}

// slotHolders reads who holds each slot that is taken now.
func slotHolders() []slotHolder {
	var out []slotHolder
	for i := range slotCount() {
		path := filepath.Join(slotsDir(), fmt.Sprintf("heavy-%d.lock", i))
		if f := trySlot(i); f != nil { // free: it was only left over
			f.Close()
			continue
		}
		p, err := windows.UTF16PtrFromString(path)
		if err != nil {
			continue
		}
		h, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
		if err != nil {
			continue
		}
		f := os.NewFile(uintptr(h), path)
		var who slotHolder
		buf := make([]byte, 4096)
		n, _ := f.Read(buf)
		f.Close()
		if json.Unmarshal(buf[:n], &who) == nil {
			out = append(out, who)
		}
	}
	return out
}

func describeHolders(hs []slotHolder) string {
	var parts []string
	for _, h := range hs {
		age := ""
		if t, err := time.Parse(time.RFC3339, h.Since); err == nil {
			age = ", " + time.Since(t).Round(time.Second).String()
		}
		parts = append(parts, fmt.Sprintf("`%s` in %s%s", clip(h.Cmd, 60), filepath.Base(h.Cwd), age))
	}
	if len(parts) == 0 {
		return "none readable"
	}
	return strings.Join(parts, "; ")
}

// acquireSlot waits for a free slot, up to maxWait (0: no limit). note is told once, when it has
// to wait. Work that already holds a slot gets an empty one at once.
func acquireSlot(cmd, cwd, label string, maxWait time.Duration, note func(string)) (*heavySlot, error) {
	if os.Getenv(heldEnv) != "" {
		return &heavySlot{}, nil
	}
	os.MkdirAll(slotsDir(), 0o755)
	start := time.Now()
	told := false
	for {
		for i := range slotCount() {
			f := trySlot(i)
			if f == nil {
				continue
			}
			f.Truncate(0)
			b, _ := json.Marshal(slotHolder{PID: os.Getpid(), Cmd: cmd, Cwd: cwd, Label: label, Since: time.Now().Format(time.RFC3339)})
			f.WriteAt(b, 0)
			return &heavySlot{f: f, Waited: time.Since(start)}, nil
		}
		if !told && note != nil {
			note(fmt.Sprintf("[fairshare] waiting for a heavy-job slot: the PC already runs %d whole suites or checks (%s). One test file or package never waits.", slotCount(), describeHolders(slotHolders())))
			told = true
		}
		if maxWait > 0 && time.Since(start) >= maxWait {
			return nil, errBusy
		}
		time.Sleep(time.Second)
	}
}

// `fairshare slot <command> [args...]` runs a command once a heavy-job slot is free, with the
// agent's own output, redirects and exit code untouched. The hook puts it in front of whole
// suites and checks that run in the background or write to the agent's own files.
func cmdSlot(args []string) int {
	var maxWait time.Duration // --wait <ms>: give up then; none waits as long as it takes
	if len(args) >= 2 && args[0] == "--wait" {
		ms, err := strconv.Atoi(args[1])
		if err != nil || ms < 0 {
			fmt.Fprintln(os.Stderr, "fairshare slot: --wait needs milliseconds")
			return 2
		}
		maxWait, args = time.Duration(ms)*time.Millisecond, args[2:]
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: fairshare slot [--wait <ms>] <command> [args...]")
		return 2
	}
	signal.Ignore(os.Interrupt) // the command gets Ctrl+C itself; we wait for it and pass on its code
	endWithCaller("slot")
	// Everything the command starts is in a job Windows kills when this process ends.
	if job, err := killOnExitJob(); err == nil {
		defer windows.CloseHandle(job)
	}
	text := strings.Join(args, " ")
	cwd, _ := os.Getwd()
	s, err := acquireSlot(text, cwd, "", maxWait, func(m string) { fmt.Fprintln(os.Stderr, m) })
	if err != nil {
		fmt.Fprintf(os.Stderr, "[fairshare] not started: no heavy-job slot came free in %s. Run it with run_in_background (it then waits as long as it takes), or narrow it to the files you changed.\n", maxWait.Round(time.Second))
		logEvent("slot", map[string]any{"cmd": clip(text, 300), "cwd": cwd, "stopped": "no-slot", "waited_ms": maxWait.Milliseconds()})
		return 124
	}
	defer s.Close()
	if s.Waited > time.Second {
		fmt.Fprintf(os.Stderr, "[fairshare] got a slot after %s\n", s.Waited.Round(time.Second))
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), heldEnv+"=1")
	started := time.Now()
	code := 0
	if err := cmd.Run(); err != nil {
		code = 127
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			fmt.Fprintln(os.Stderr, "fairshare slot:", err)
		}
	}
	logEvent("slot", map[string]any{"cmd": clip(text, 300), "cwd": cwd, "exit": code, "waited_ms": s.Waited.Milliseconds(), "wall_ms": time.Since(started).Milliseconds()})
	return code
}
