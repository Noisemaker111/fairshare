package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// An agent harness stops a background command by killing the tree under its shell, by Windows
// parent links. When that shell runs a script that execs `fairshare run`, Git Bash leaves fairshare
// with a parent that has already exited, so the tree kill misses it. fairshare must notice its
// caller ended and take the command down with it (a headed browser once stayed open that way).
func TestRunEndsWhenTheShellThatStartedItIsKilled(t *testing.T) {
	bin := filepath.ToSlash(fairshareBin(t))
	for _, flags := range []string{"", "--normal"} {
		t.Run("run"+flags, func(t *testing.T) {
			script := filepath.Join(t.TempDir(), "wrapper.sh")
			body := fmt.Sprintf("exec %q run %s -- bash -c \"ping -n 120 127.0.0.1 >/dev/null\"\n", bin, flags)
			if err := os.WriteFile(script, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			start := nowTicks()
			// the shape of a harness command: the script is not the last command, so bash forks for it
			shell := exec.Command(bashPath(), "-c", "true && bash '"+filepath.ToSlash(script)+"' && true")
			shell.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
			if err := shell.Start(); err != nil {
				t.Fatal(err)
			}
			defer shell.Process.Kill()

			var fs, ping uint32
			for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline) && ping == 0; time.Sleep(200 * time.Millisecond) {
				fs, ping = findRunAndPing(start)
			}
			if ping == 0 {
				t.Fatal("the wrapped command never started")
			}
			defer killPID(ping)
			defer killPID(fs) // only if the check fails: the test leaves nothing running

			// what the harness does: kill the shell's tree by parent links
			exec.Command("taskkill", "/T", "/F", "/PID", fmt.Sprint(shell.Process.Pid)).Run()

			for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
				if !alive(fs) && !alive(ping) {
					return
				}
			}
			t.Fatalf("after the shell was killed, fairshare alive=%v, its command alive=%v", alive(fs), alive(ping))
		})
	}
}

// findRunAndPing finds the fairshare started after start under bash, and the ping it runs.
func findRunAndPing(start uint64) (fs, ping uint32) {
	procs := snapshot()
	for pid, p := range procs {
		if strings.EqualFold(p.name, "fairshare.exe") && p.created >= start && strings.EqualFold(procs[p.ppid].name, "bash.exe") {
			fs = pid
		}
	}
	if fs == 0 {
		return 0, 0
	}
	for pid, p := range procs {
		if strings.EqualFold(p.name, "PING.EXE") && p.created >= start && p.created >= procs[fs].created {
			ping = pid
		}
	}
	return fs, ping
}

func killPID(pid uint32) {
	if h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, pid); err == nil {
		windows.TerminateProcess(h, 1)
		windows.CloseHandle(h)
	}
}

func alive(pid uint32) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	ev, _ := windows.WaitForSingleObject(h, 0)
	return ev == uint32(windows.WAIT_TIMEOUT)
}

func nowTicks() uint64 {
	var ft windows.Filetime
	windows.GetSystemTimeAsFileTime(&ft)
	return ftTicks(ft)
}

func TestParseMsysPS(t *testing.T) {
	raw := []byte(`      PID    PPID    PGID     WINPID   TTY         UID    STIME COMMAND
     7925    7924    7924      30752  ?         197609 14:46:57 /c/Users/x/fairshare
     7924       1    7924      25916  ?         197609 14:46:57 /usr/bin/bash
S    7930    7924    7930      11111  pty0      197609 14:46:59 /usr/bin/sleep
`)
	parentOf, winOf := parseMsysPS(raw)
	if parentOf[30752] != 7924 || winOf[7924] != 25916 || parentOf[11111] != 7924 {
		t.Fatalf("parentOf=%v winOf=%v", parentOf, winOf)
	}
}
