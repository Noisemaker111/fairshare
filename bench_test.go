package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// If adopt dies holding work frozen, the guard must thaw it.
func TestThawGuardThawsAfterOwnerDies(t *testing.T) {
	cmd := exec.Command("node", "-e", "setTimeout(()=>{},300)")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := uint32(cmd.Process.Pid)
	time.Sleep(100 * time.Millisecond)
	if !setSuspended(pid, ntSuspendProcess) {
		t.Fatal("could not suspend")
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	select {
	case <-done:
		t.Fatal("process ran while suspended")
	case <-time.After(1500 * time.Millisecond):
	}
	owner := exec.Command("cmd", "/c", "exit 0")
	owner.Run() // an owner that has already ended
	file := filepath.Join(t.TempDir(), "frozen")
	os.WriteFile(file, []byte(fmt.Sprintf("%d %d\n", pid, creationTime(pid))), 0o644)
	thawGuard(uint32(owner.Process.Pid), file)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		setSuspended(pid, ntResumeProcess)
		t.Fatal("the guard did not thaw the process")
	}
}

// The bench freezes and measures its jobs through the job it put itself in; it must see them.
func TestKillOnExitJobListsChildren(t *testing.T) {
	job, err := killOnExitJob()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("node", "-e", "setTimeout(()=>{},2000)")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait()
	time.Sleep(300 * time.Millisecond)
	pids := jobPIDs(job)
	found := false
	for _, p := range pids {
		found = found || p == uint32(cmd.Process.Pid)
	}
	if !found {
		t.Fatalf("job lists %v, not the child %d", pids, cmd.Process.Pid)
	}
}

// The bench's "these jobs" share comes from treeCPU; it must see a busy child's CPU.
func TestTreeCPUSeesBusyChild(t *testing.T) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(job)
	// a fixed amount of work, not a fixed time, so a busy PC only makes it slower; then it waits
	// on stdin so it is still alive (and listed) when measured
	cmd := exec.Command("node", "-e", "let x=0; for (let i=0; i<6e8; i++) x+=i%7; console.log('done'); process.stdin.once('data',()=>process.exit(0))")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { stdin.Write([]byte("x\n")); stdin.Close(); cmd.Wait() }()
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		t.Fatal(err)
	}
	windows.CloseHandle(h)
	if pids := jobPIDs(job); len(pids) != 1 {
		t.Fatalf("jobPIDs = %v, want the one child", pids)
	}
	if _, err := io.ReadFull(out, make([]byte, 4)); err != nil { // "done": the work is finished
		t.Fatal(err)
	}
	used := cpuDelta(nil, treeCPU(job, 0))
	if used < 0.3 {
		t.Fatalf("a child that did about a second of work showed %.2f CPU seconds", used)
	}
}
