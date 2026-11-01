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

	"golang.org/x/sys/windows"
)

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	normal := fs.Bool("normal", false, "keep normal priority (still cleans up the whole tree on exit)")
	memLimit := fs.Uint64("mem-limit", 0, "fail this run if its whole tree commits more than this many MB (0 = none)")
	noFreeze := fs.Bool("no-freeze", false, "never freeze this run when memory is short")
	freezeBelow := fs.Uint64("freeze-below", DefaultPolicy().FreezeBelowMB, "freeze when free RAM or free commit falls below this many MB")
	resumeAbove := fs.Uint64("resume-above", DefaultPolicy().ResumeAboveMB, "thaw when both are back above this many MB")
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

	p := DefaultPolicy()
	p.IOPriority, p.MemPriority, p.JobMemoryMB = *ioPri, *memPri, *memLimit
	p.FreezeBelowMB, p.ResumeAboveMB = *freezeBelow, *resumeAbove
	if *normal {
		p.PriorityClass, p.IOPriority, p.MemPriority = 0, -1, -1
	}
	if *noFreeze || *normal {
		p.FreezeBelowMB = 0
	}
	gov, unit, err := enterUnit(p)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fairshare:", err, "(running without it)")
	}
	signal.Ignore(os.Interrupt) // the child gets Ctrl+C itself; we wait for it and pass on its code

	execute := func(stdout, stderr io.Writer) int {
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, stdout, stderr
		err := cmd.Run()
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
