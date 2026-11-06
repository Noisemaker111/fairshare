package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"unsafe"

	"github.com/Noisemaker111/fairshare/internal/windows"
)

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	normal := fs.Bool("normal", false, "run the command at normal priority, outside this chat's lane, for measurements that need it (still ends the whole tree on exit)")
	memLimit := fs.Uint64("mem-limit", 0, "fail this run if its whole tree commits more than this many MB (0 = none)")
	freeze := fs.Bool("freeze", false, "suspend this run while memory is short (off: it is only lowered in priority)")
	freezeBelow := fs.Uint64("freeze-below", freezeBelowMB, "with --freeze: suspend when free RAM or free commit falls below this many MB")
	resumeAbove := fs.Uint64("resume-above", resumeAboveMB, "with --freeze: resume when both are back above this many MB")
	ioPri := fs.Int("io", 0, "disk priority: 0 very low, 1 low, 2 normal")
	memPri := fs.Int("mempri", 1, "memory priority: 1 very low .. 5 normal")
	useCache := fs.Bool("cache", false, "reuse the result of the same command on the same code, and wait for an identical run already in progress instead of repeating it")
	cacheFailures := fs.Bool("cache-failures", false, "also keep failures for later runs (runs already waiting always share them)")
	keyEnv := fs.String("key-env", "", "comma-separated environment variables that change the result")
	stats := fs.Bool("stats", false, "print what fairshare did to stderr at the end")
	fs.Parse(args)
	argv := fs.Args()
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "usage: fairshare run [flags] -- command args...")
		return 2
	}
	endWithCaller("run")

	p := DefaultPolicy()
	p.IOPriority, p.MemPriority, p.JobMemoryMB = *ioPri, *memPri, *memLimit
	if *freeze {
		p = p.withFreezing(*freezeBelow, *resumeAbove)
	}
	var gov *Governor
	var unit *Unit
	var err error
	if *normal {
		// fairshare itself stays where it is; only the command leaves the chat's lane (startNormal)
		p = Policy{IOPriority: -1, MemPriority: -1, JobMemoryMB: *memLimit}
		if gov, err = NewGovernor(p, nil); err != nil {
			fmt.Fprintln(os.Stderr, "fairshare:", err)
			return 1
		}
	} else if gov, unit, err = enterUnit(p); err != nil {
		fmt.Fprintln(os.Stderr, "fairshare:", err, "(running without it)")
	}
	signal.Ignore(os.Interrupt) // the child gets Ctrl+C itself; we wait for it and pass on its code

	execute := func(stdout, stderr io.Writer) int {
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, stdout, stderr
		if *normal {
			u, err := startNormal(gov, cmd)
			if err != nil {
				fmt.Fprintln(os.Stderr, "fairshare run --normal:", err)
				return 127
			}
			unit = u
		} else if err := cmd.Start(); err != nil {
			fmt.Fprintln(os.Stderr, "fairshare:", err)
			return 127
		}
		err := cmd.Wait()
		var exit *exec.ExitError
		switch {
		case err == nil:
			return 0
		case errors.As(err, &exit):
			return exit.ExitCode()
		default:
			fmt.Fprintln(os.Stderr, "fairshare:", err)
			return 127
		}
	}

	code := 0
	if *useCache {
		var envKeys []string
		if *keyEnv != "" {
			envKeys = strings.Split(*keyEnv, ",")
		}
		key, kerr := CacheKey(".", argv, envKeys)
		if kerr != nil {
			fmt.Fprintln(os.Stderr, "fairshare: not caching:", kerr)
			code = execute(os.Stdout, os.Stderr)
		} else {
			code = WithCache(key, *cacheFailures, execute)
		}
	} else {
		code = execute(os.Stdout, os.Stderr)
	}
	if *stats && unit != nil {
		fmt.Fprintf(os.Stderr, "fairshare: tuned=%d tune_errors=%d freezes=%d cpu_s=%.1f\n", gov.Tuned.Load(), gov.TuneErrs.Load(), gov.Freezes.Load(), jobCPUSeconds(unit.Job))
	}
	return code
}

// enterUnit puts fairshare itself in a fresh unit, so the command and everything it starts are
// in it; Windows kills whatever is left in it when fairshare exits, however it exits.
func enterUnit(p Policy) (*Governor, *Unit, error) {
	gov, err := NewGovernor(p, nil) // run is itself in the idle job, so it can't sense the foreground
	if err != nil {
		return nil, nil, err
	}
	u, err := gov.NewUnit(true, "")
	if err != nil {
		return nil, nil, err
	}
	u.Skip = windows.GetCurrentProcessId()
	if err := windows.AssignProcessToJobObject(u.Job, windows.CurrentProcess()); err != nil {
		return nil, nil, fmt.Errorf("join job: %w", err)
	}
	return gov, u, nil
}

// startNormal starts cmd at normal priority, the way a person starting it would get it. Inside a
// chat's lane that takes leaving the lane: the lane's job holds every process in it at idle
// priority, and a job's priority limit also binds every job nested in it, so no job fairshare makes
// there can raise anything. So the command starts as a child of the user's shell (explorer.exe),
// which is in no lane and no chat, so adopt never takes it or what it starts. It starts suspended,
// goes into a job of this run's that Windows kills when this process ends, gets normal CPU, disk and
// memory priority, and only then runs. A command that still is not at normal priority is ended and
// reported, never run at a priority it did not ask for.
//
// It also starts on a private desktop (startOffScreen): what a measurement opens, such as Strobe's
// browser windows, never appears on Jon's screen or takes his game's focus.
func startNormal(gov *Governor, cmd *exec.Cmd) (*Unit, error) {
	attr := &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED | windows.CREATE_NO_WINDOW}
	if shell := openUserShell(); shell != 0 {
		defer windows.CloseHandle(shell)
		attr.ParentProcess = syscall.Handle(shell)
	}
	cmd.SysProcAttr = attr
	if err := startOffScreen(cmd.Start); err != nil {
		return nil, err
	}
	pid := uint32(cmd.Process.Pid)
	fail := func(err error) (*Unit, error) {
		cmd.Process.Kill()
		cmd.Wait()
		return nil, err
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_SET_INFORMATION|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return fail(fmt.Errorf("open %d: %w", pid, err))
	}
	defer windows.CloseHandle(h)
	u, err := gov.NewUnit(true, "")
	if err != nil {
		return fail(err)
	}
	if err := windows.AssignProcessToJobObject(u.Job, h); err != nil {
		return fail(fmt.Errorf("join job: %w", err))
	}
	if err := windows.SetPriorityClass(h, windows.NORMAL_PRIORITY_CLASS); err != nil {
		return fail(fmt.Errorf("set priority: %w", err))
	}
	ioNormal, memNormal := uint32(2), uint32(5)
	ntSetInformationProcess.Call(uintptr(h), processIoPriority, uintptr(unsafe.Pointer(&ioNormal)), 4)
	setProcessInformation.Call(uintptr(h), processMemoryPriority, uintptr(unsafe.Pointer(&memNormal)), 4)
	if got := priorityName(pid); got != "normal" {
		return fail(fmt.Errorf("%s still runs at %s priority (is this shell in a job that forbids leaving it?)", cmd.Path, got))
	}
	if !setSuspended(pid, ntResumeProcess) {
		return fail(fmt.Errorf("resume %d failed", pid))
	}
	return u, nil
}

// startOffScreen calls start so the process it starts begins on a private desktop when this one's
// children would begin on the screen's ("WinSta0\Default"). A process takes its creator's desktop
// (explorer as its parent or not), and everything it starts takes that one in turn, so the whole
// tree's windows are drawn where no one sees them and none can take the foreground. Started from
// any other desktop (an Owl drive desktop already) it runs as is. The desktop stays open until this
// fairshare exits, past the end of the command, which may still be starting up on it.
func startOffScreen(start func() error) error {
	child, err := windows.ChildDesktop()
	if err != nil {
		return fmt.Errorf("read this process's desktop: %w", err)
	}
	station, desk, found := strings.Cut(child, `\`)
	if !found {
		station, desk = "WinSta0", child
	}
	if desk != "" && !strings.EqualFold(desk, "Default") {
		return start()
	}
	name := fmt.Sprintf("fairshare-%d", os.Getpid())
	if _, err := windows.CreateDesktop(name); err != nil {
		return fmt.Errorf("make a private desktop: %w", err)
	}
	return windows.WithChildDesktop(station+`\`+name, start)
}

// openUserShell opens this session's explorer.exe with the rights to start a process under it, or
// returns 0 when there is none (then the command starts under fairshare, and still must reach normal
// priority or fail).
func openUserShell() windows.Handle {
	var session uint32
	windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &session)
	for pid, p := range snapshot() {
		if p.name != "explorer.exe" {
			continue
		}
		var s uint32
		if windows.ProcessIdToSessionId(pid, &s) != nil || s != session {
			continue
		}
		if h, err := windows.OpenProcess(windows.PROCESS_CREATE_PROCESS|windows.PROCESS_DUP_HANDLE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid); err == nil {
			return h
		}
	}
	return 0
}

// priorityName is used by the self-check to show what Windows actually applied.
func priorityName(pid uint32) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "?"
	}
	defer windows.CloseHandle(h)
	c, err := windows.GetPriorityClass(h)
	if err != nil {
		return "?"
	}
	switch c {
	case windows.IDLE_PRIORITY_CLASS:
		return "idle"
	case windows.BELOW_NORMAL_PRIORITY_CLASS:
		return "below-normal"
	case windows.NORMAL_PRIORITY_CLASS:
		return "normal"
	case windows.ABOVE_NORMAL_PRIORITY_CLASS:
		return "above-normal"
	case windows.HIGH_PRIORITY_CLASS:
		return "high"
	}
	return fmt.Sprint(c)
}
