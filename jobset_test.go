package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/Noisemaker111/fairshare/internal/windows"
)

const (
	createBreakawayFromJob = 0x01000000
	createSuspended        = 0x00000004
)

// The test binary doubles as a program that, told to over stdin, starts a child the way the
// MSYS2 runtime (Git Bash) does when its job allows breakaway: with CREATE_BREAKAWAY_FROM_JOB.
func TestMain(m *testing.M) {
	// Run as `owl storage reclaim --apply --json` (FAIRSHARE_OWL is this binary): write the arguments
	// to the file named by FAIRSHARE_TEST_FAKE_OWL.
	if out := os.Getenv("FAIRSHARE_TEST_FAKE_OWL"); out != "" && len(os.Args) > 2 && os.Args[1] == "storage" {
		os.WriteFile(out, []byte(strings.Join(os.Args[1:], " ")), 0o644)
		os.Exit(0)
	}
	// Run as `boat list --json` (BOAT_EXE is this binary): list the machine ids in the file named by
	// FAIRSHARE_TEST_BOAT_LIST, separated by spaces; "id=name" also gives the machine a name.
	if len(os.Args) > 1 && os.Args[1] == "list" && os.Getenv("FAIRSHARE_TEST_BOAT_LIST") != "" {
		b, _ := os.ReadFile(os.Getenv("FAIRSHARE_TEST_BOAT_LIST"))
		ids := []string{}
		for _, field := range strings.Fields(string(b)) {
			id, name, _ := strings.Cut(field, "=")
			ids = append(ids, fmt.Sprintf("{\"id\":%q,\"name\":%q}", id, name))
		}
		fmt.Printf("{\"sandboxes\":[%s]}\n", strings.Join(ids, ","))
		os.Exit(0)
	}
	// Tests never start the PC's real reclaim: the low-disk threshold is 1 GB and owl.exe is a path
	// that does not exist, unless a test sets its own (lowdisk_test.go).
	if os.Getenv("FAIRSHARE_OWL_SETTINGS") == "" {
		quiet := filepath.Join(os.TempDir(), "fairshare-test-owl-settings.json")
		os.WriteFile(quiet, []byte(`{"storage_clean":{"reclaim_free_gb":1}}`), 0o644)
		os.Setenv("FAIRSHARE_OWL_SETTINGS", quiet)
		os.Setenv("FAIRSHARE_OWL", filepath.Join(os.TempDir(), "no-owl-in-tests.exe"))
	}
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

// A process Windows is still starting has one thread, held suspended: a Store app (the Store
// PowerShell) fails to start with "Access is denied" if it is put in a job before Windows puts it
// in its package's job, which happens in that window. So exec's sweep (and adopt) leave such a
// process alone however long it stays that way, and take it as soon as its thread runs.
func TestSweepLeavesAProcessUntilItsThreadRuns(t *testing.T) {
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
	child.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createSuspended}
	stdin, _ := child.StdinPipe()
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { stdin.Close(); child.Process.Kill(); child.Wait() }()
	pid := uint32(child.Process.Pid)
	if !snapshot()[pid].unstarted {
		t.Fatal("a process whose one thread was never let run is not seen as starting")
	}
	self, tried := uint32(os.Getpid()), map[uint32]bool{}
	time.Sleep(1200 * time.Millisecond) // longer than the clock-based wait this replaced
	if pulled, sided, _, _ := pullIntoJobs(s, self, tried); pulled+sided != 0 {
		t.Fatal("a process still being started was moved into a job")
	}
	if !setSuspended(pid, ntResumeProcess) {
		t.Fatal("could not resume the child")
	}
	deadline := time.Now().Add(5 * time.Second)
	for snapshot()[pid].unstarted && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if pulled, sided, out, why := pullIntoJobs(s, self, tried); pulled+sided != 1 {
		t.Fatalf("once its thread runs it should join a job: pulled %d, side %d, outside %v, %v", pulled, sided, out, why)
	}
}
