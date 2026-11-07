package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
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
	case "orphan-parent":
		// Like convex-local-backend starting its node executors: start a long-lived child with no
		// job of its own, write its ID down, then die, leaving it orphaned in whatever job it is in.
		time.Sleep(1500 * time.Millisecond) // adopt has placed this process by now
		c := exec.Command(os.Args[0])
		c.Env = append(os.Environ(), "FAIRSHARE_TEST_HELPER=linger")
		if err := c.Start(); err != nil {
			os.Exit(1)
		}
		os.WriteFile(os.Getenv("FAIRSHARE_TEST_PIDFILE"), []byte(fmt.Sprint(c.Process.Pid)), 0o644)
		time.Sleep(2500 * time.Millisecond) // and adopt has placed the child
		os.Exit(0)
	case "linger":
		time.Sleep(time.Minute)
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
	defer l.close(gov, "test over", nil)
	if how := l.place(gov, uint32(shell.Process.Pid), h); how != "side job" {
		t.Fatalf("placed as %q, want a side job", how)
	}

	stdin.Write([]byte("go\n"))
	line, _ := bufio.NewReader(stdout).ReadString('\n')
	if strings.TrimSpace(line) != "spawned" {
		t.Fatalf("a program in a side job could not start a child its own job let it start: %q", strings.TrimSpace(line))
	}
}

// A process that just appeared may still be starting: a Store app (the Store PowerShell) fails to
// start with "Access is denied" if it is put in a job before Windows puts it in its package's job.
// So exec's sweep (and adopt) leave a new process alone for settleTime, then take it.
func TestSweepLeavesAStartingProcessUntilItSettles(t *testing.T) {
	job, err := killOnCloseJob()
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(job)
	s := &jobSet{main: job, newSide: func() (windows.Handle, func(), error) {
		j, err := killOnCloseJob()
		return j, func() { windows.CloseHandle(j) }, err
	}}
	defer s.release()
	child := exec.Command(os.Args[0])
	child.Env = append(os.Environ(), "FAIRSHARE_TEST_HELPER=spawn-breakaway") // waits on stdin
	stdin, _ := child.StdinPipe()
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	defer func() { stdin.Close(); child.Process.Kill(); child.Wait() }()
	self, tried := uint32(os.Getpid()), map[uint32]bool{}
	if pulled, sided, _, _ := pullIntoJobs(s, self, tried); pulled+sided != 0 {
		t.Fatalf("a process %s old was moved into a job while it may still be starting", time.Since(started))
	}
	time.Sleep(time.Until(started.Add(settleTime + 200*time.Millisecond)))
	if pulled, sided, out, why := pullIntoJobs(s, self, tried); pulled+sided != 1 {
		t.Fatalf("once settled it should join a job: pulled %d, side %d, outside %v, %v", pulled, sided, out, why)
	}
}
