package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"github.com/Noisemaker111/fairshare/internal/windows"
)

const createBreakawayFromJob = 0x01000000

// The test binary doubles as a program that, told to over stdin, starts a child the way the
// MSYS2 runtime (Git Bash) does when its job allows breakaway: with CREATE_BREAKAWAY_FROM_JOB.
func TestMain(m *testing.M) {
	switch os.Getenv("FAIRSHARE_TEST_HELPER") {
	case "spawn-breakaway":
		bufio.NewReader(os.Stdin).ReadString('\n')
		c := exec.Command(os.Args[0])
		c.Env = append(os.Environ(), "FAIRSHARE_TEST_HELPER=exit")
		c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createBreakawayFromJob}
		if err := c.Run(); err != nil {
			fmt.Println("spawn failed:", err)
		} else {
			fmt.Println("spawned")
		}
		os.Exit(0)
	case "exit":
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// Agent harnesses start every shell inside a job of their own that allows breakaway (Claude Code
// and Codex both do), and Git Bash then asks for breakaway on each program it starts, deciding
// from its job's limits just before starting it. adopt finds the shell a moment later and wraps it
// in a side job. If that side job forbids breakaway, a start already decided fails with "Access is
// denied" (bash: exit 126, "Permission denied"). So a side job must allow what its process's job
// allowed.
func TestSideJobKeepsBreakawayOfTheJobAroundIt(t *testing.T) {
	harness, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(harness)
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK | windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(harness, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		t.Fatal(err)
	}

	shell := exec.Command(os.Args[0])
	shell.Env = append(os.Environ(), "FAIRSHARE_TEST_HELPER=spawn-breakaway")
	stdin, _ := shell.StdinPipe()
	stdout, _ := shell.StdoutPipe()
	if err := shell.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { shell.Process.Kill(); shell.Wait() }()
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_SET_INFORMATION|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(shell.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(harness, h); err != nil {
		t.Fatal(err)
	}

	// what adopt does with it: the chat's lane, where a process already in a job gets a side job
	p := DefaultPolicy()
	p.FreezeBelowMB, p.IOPriority, p.MemPriority = 0, -1, -1
	gov, err := NewGovernor(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	u, err := gov.NewUnit(true, "")
	if err != nil {
		t.Fatal(err)
	}
	l := newLane(gov, 0, 0, "test", u)
	defer l.close(gov, "test over")
	if how := l.place(gov, uint32(shell.Process.Pid), h); how != "side job" {
		t.Fatalf("placed as %q, want a side job", how)
	}

	stdin.Write([]byte("go\n"))
	line, _ := bufio.NewReader(stdout).ReadString('\n')
	if strings.TrimSpace(line) != "spawned" {
		t.Fatalf("a program in a side job could not start a child its own job let it start: %q", strings.TrimSpace(line))
	}
}
